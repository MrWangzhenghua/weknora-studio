package service

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// SelectKnowledgeForBridge 过滤出可导出给 Bridge（PPT / 闪卡等）的知识条目。
// 与原先 PPT 专用逻辑一致：跳过未解析 / 禁用 / FAQ / 仅图等。
func SelectKnowledgeForBridge(list []*types.Knowledge, includeIDs []string) (included, skipped []*types.Knowledge) {
	whitelist := make(map[string]struct{}, len(includeIDs))
	for _, id := range includeIDs {
		whitelist[id] = struct{}{}
	}
	for _, k := range list {
		if k == nil {
			continue
		}
		if len(whitelist) > 0 {
			if _, ok := whitelist[k.ID]; !ok {
				continue
			}
		}
		if !isKnowledgeUsableForBridge(k) {
			skipped = append(skipped, k)
			continue
		}
		included = append(included, k)
	}
	return
}

func isKnowledgeUsableForBridge(k *types.Knowledge) bool {
	if k == nil {
		return false
	}
	if k.Type == types.KnowledgeTypeFAQ {
		return false
	}
	if k.IsManual() {
		return true
	}
	if k.ParseStatus != "" && k.ParseStatus != types.ParseStatusCompleted {
		return false
	}
	if k.EnableStatus != "" && k.EnableStatus != "enabled" {
		return false
	}
	return true
}

// CollectKnowledgeBridgeFiles 将知识列表转为 Bridge multipart 用的文件流（优先 chunk 拼 Markdown）。
func CollectKnowledgeBridgeFiles(
	ctx context.Context,
	knowSvc interfaces.KnowledgeService,
	chunkSvc interfaces.ChunkService,
	list []*types.Knowledge,
) ([]BridgeFile, int64, error) {
	files := make([]BridgeFile, 0, len(list))
	var total int64
	for _, k := range list {
		md, ok := buildKnowledgeMarkdownForBridge(ctx, chunkSvc, k)
		if ok {
			data := []byte(md)
			name := mdFileNameForBridge(k)
			files = append(files, BridgeFile{
				Name:        name,
				ContentType: "text/markdown; charset=utf-8",
				Reader:      io.NopCloser(bytes.NewReader(data)),
			})
			total += int64(len(data))
			logger.Infof(ctx, "[kb-bridge] knowledge %s -> markdown %d bytes (chunk-based)", k.ID, len(data))
			continue
		}

		reader, filename, err := knowSvc.GetKnowledgeFile(ctx, k.ID)
		if err != nil {
			logger.Warnf(ctx, "[kb-bridge] skip knowledge %s: no chunk and file unavailable: %v", k.ID, err)
			continue
		}
		if filename == "" {
			filename = k.FileName
			if filename == "" {
				filename = k.ID + ".bin"
			}
		}
		files = append(files, BridgeFile{
			Name:        filename,
			ContentType: guessContentType(filename),
			Reader:      reader,
		})
		total += k.FileSize
		logger.Warnf(ctx, "[kb-bridge] knowledge %s has no chunk, fall back to raw file %s", k.ID, filename)
	}
	return files, total, nil
}

func pickChunksForBridge(chunks []*types.Chunk, summaryOnly bool) []*types.Chunk {
	picked := make([]*types.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if c == nil {
			continue
		}
		if strings.TrimSpace(c.Content) == "" {
			continue
		}
		if summaryOnly {
			switch c.ChunkType {
			case types.ChunkTypeSummary, types.ChunkTypeTableSummary:
				picked = append(picked, c)
			}
			continue
		}
		switch c.ChunkType {
		case types.ChunkTypeText,
			types.ChunkTypeParentText,
			types.ChunkTypeSummary,
			types.ChunkTypeWikiPage,
			types.ChunkTypeImageCaption,
			types.ChunkTypeImageOCR,
			types.ChunkTypeTableSummary,
			types.ChunkTypeTableColumn:
			picked = append(picked, c)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		if picked[i].ChunkIndex != picked[j].ChunkIndex {
			return picked[i].ChunkIndex < picked[j].ChunkIndex
		}
		return picked[i].CreatedAt.Before(picked[j].CreatedAt)
	})
	return picked
}

func composeMarkdownFromPickedKnowledge(k *types.Knowledge, picked []*types.Chunk, banner string) string {
	if len(picked) == 0 {
		return ""
	}
	title := firstNonEmpty(k.Title, k.FileName, k.ID)
	var buf strings.Builder
	buf.Grow(2048)
	buf.WriteString("# ")
	buf.WriteString(title)
	buf.WriteString("\n\n")
	if banner != "" {
		buf.WriteString("> ")
		buf.WriteString(strings.ReplaceAll(strings.TrimSpace(banner), "\n", " "))
		buf.WriteString("\n\n")
	}
	if k.Description != "" {
		buf.WriteString("> ")
		buf.WriteString(strings.ReplaceAll(strings.TrimSpace(k.Description), "\n", " "))
		buf.WriteString("\n\n")
	}

	seen := make(map[string]struct{}, len(picked))
	for _, c := range picked {
		body := strings.TrimSpace(c.Content)
		if body == "" {
			continue
		}
		if _, ok := seen[body]; ok {
			continue
		}
		seen[body] = struct{}{}

		switch c.ChunkType {
		case types.ChunkTypeImageCaption:
			buf.WriteString("**[图片说明]** ")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		case types.ChunkTypeImageOCR:
			buf.WriteString("**[图片文字]** ")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		case types.ChunkTypeTableSummary:
			buf.WriteString("**[表格摘要]** ")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		case types.ChunkTypeTableColumn:
			buf.WriteString("**[表格列]** ")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		case types.ChunkTypeSummary:
			buf.WriteString("**[摘要]** ")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		default:
			buf.WriteString(body)
			buf.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(buf.String())
}

func truncateMarkdownRunesBridge(s string, maxRunes int) string {
	if maxRunes < 64 {
		maxRunes = 64
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
	}
	return strings.TrimSpace(string(r)) + "\n\n> **[WeKnora]** 内容仍超出长度上限，已截断尾部。可调大环境变量 `PPTGEN_MARKDOWN_MAX_RUNES` 或精简知识库。\n"
}

func buildKnowledgeMarkdownForBridge(ctx context.Context, chunkSvc interfaces.ChunkService, k *types.Knowledge) (string, bool) {
	if chunkSvc == nil || k == nil {
		return "", false
	}
	chunks, err := chunkSvc.ListChunksByKnowledgeID(ctx, k.ID)
	if err != nil {
		logger.Warnf(ctx, "[kb-bridge] list chunks failed for %s: %v", k.ID, err)
		return "", false
	}
	if len(chunks) == 0 {
		return "", false
	}

	limit := pptgenMarkdownMaxRunes()
	picked := pickChunksForBridge(chunks, false)
	if len(picked) == 0 {
		return "", false
	}

	md := composeMarkdownFromPickedKnowledge(k, picked, "")
	if md == "" {
		return "", false
	}

	if utf8.RuneCountInString(md) <= limit {
		return md, true
	}

	logger.Warnf(ctx, "[kb-bridge] knowledge %s markdown runes=%d > limit=%d, fallback to summary-only chunks",
		k.ID, utf8.RuneCountInString(md), limit)

	summaryPicked := pickChunksForBridge(chunks, true)
	banner := "【全文过长，已自动改为仅使用系统生成的摘要与表格摘要作为素材】"
	if len(summaryPicked) > 0 {
		md = composeMarkdownFromPickedKnowledge(k, summaryPicked, banner)
	} else {
		logger.Warnf(ctx, "[kb-bridge] knowledge %s has no summary chunks, hard-truncate full markdown", k.ID)
		md = composeMarkdownFromPickedKnowledge(k, picked, "【全文过长且无摘要类 chunk，将截断正文】")
		md = truncateMarkdownRunesBridge(md, limit)
	}

	if utf8.RuneCountInString(md) > limit {
		md = truncateMarkdownRunesBridge(md, limit)
	}
	if strings.TrimSpace(md) == "" {
		return "", false
	}
	return md, true
}

func mdFileNameForBridge(k *types.Knowledge) string {
	base := firstNonEmpty(strings.TrimSuffix(k.FileName, filepath.Ext(k.FileName)), k.Title, k.ID)
	base = sanitizePPTFilename(base)
	if base == "" {
		base = "knowledge"
	}
	return base + ".md"
}

// FlashcardBridgeMarkdownMaxRunes 闪卡单次合并上下文的码点上限（独立于 PPT 单文件上限时可扩展环境变量）。
func FlashcardBridgeMarkdownMaxRunes() int {
	v := strings.TrimSpace(os.Getenv("FLASHCARD_CONTEXT_MAX_RUNES"))
	if v == "" {
		return pptgenMarkdownMaxRunes() * 2
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 4096 {
		return pptgenMarkdownMaxRunes() * 2
	}
	if n > 800000 {
		return 800000
	}
	return n
}
