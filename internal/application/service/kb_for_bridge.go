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

// Bridge 輸入策略：控制 PPT 等流程從知識條目組 multipart 時優先使用 chunk 拼 Markdown 還是原始源檔。
const (
	BridgeInputModeChunks = "chunks"
	BridgeInputModeSource = "source"
	BridgeInputModeAuto   = "auto"
)

// bridgeAutoOversizedStrategyChunks：auto 判定「超大」後走與 chunks 模式相同的 chunk→摘要→截斷邏輯。
const bridgeAutoOversizedStrategyChunks = "chunks"

// bridgeAutoOversizedStrategyOutlineExcerpt：摘要/outline 類 chunk + 可讀源文節選（手動 Markdown 或純文字檔前綴）。
const bridgeAutoOversizedStrategyOutlineExcerpt = "outline_excerpt"

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

// ResolvePPTGenBridgeInputMode 解析 PPT 喂给 Bridge 的输入策略：优先知识库 chunking_config.pptgen_bridge_input_mode，
// 否则读环境变量 PPTGEN_BRIDGE_INPUT_MODE（chunks|source|auto），默认 chunks（与历史「chunk 优先」一致）。
func ResolvePPTGenBridgeInputMode(kbFieldOverride string) string {
	o := strings.TrimSpace(strings.ToLower(kbFieldOverride))
	switch o {
	case BridgeInputModeChunks, BridgeInputModeSource, BridgeInputModeAuto:
		return o
	}
	v := strings.TrimSpace(strings.ToLower(os.Getenv("PPTGEN_BRIDGE_INPUT_MODE")))
	switch v {
	case BridgeInputModeChunks, BridgeInputModeSource, BridgeInputModeAuto:
		return v
	}
	return BridgeInputModeChunks
}

func pptgenBridgeSourceMaxBytes() int64 {
	const defaultBytes = 40 * 1024 * 1024
	v := strings.TrimSpace(os.Getenv("PPTGEN_BRIDGE_SOURCE_MAX_BYTES"))
	if v == "" {
		return defaultBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 65536 {
		return defaultBytes
	}
	if n > 500*1024*1024 {
		return 500 * 1024 * 1024
	}
	return n
}

// pptgenBridgeSourceMaxPages 元数据页数上限（0 表示不启用页数判定）。
func pptgenBridgeSourceMaxPages() int {
	const defaultPages = 400
	v := strings.TrimSpace(os.Getenv("PPTGEN_BRIDGE_SOURCE_MAX_PAGES"))
	if v == "" {
		return defaultPages
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultPages
	}
	if n > 10000 {
		return 10000
	}
	return n
}

// pptgenBridgeSourceMaxMDRunes 由 chunk 拼出的「全量」Markdown 码点上限：超过则 auto 模式视为不宜整份走源文件。
func pptgenBridgeSourceMaxMDRunes() int {
	defaultRunes := pptgenMarkdownMaxRunes() * 12
	if defaultRunes < 120000 {
		defaultRunes = 120000
	}
	v := strings.TrimSpace(os.Getenv("PPTGEN_BRIDGE_SOURCE_MAX_MD_RUNES"))
	if v == "" {
		return defaultRunes
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 4096 {
		return defaultRunes
	}
	if n > 2_000_000 {
		return 2_000_000
	}
	return n
}

func pptgenBridgeOutlineExcerptMaxRunes() int {
	const defaultExcerpt = 12000
	v := strings.TrimSpace(os.Getenv("PPTGEN_BRIDGE_OUTLINE_EXCERPT_MAX_RUNES"))
	if v == "" {
		return defaultExcerpt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 512 {
		return defaultExcerpt
	}
	if n > 200_000 {
		return 200_000
	}
	return n
}

func pptgenBridgeAutoOversizedStrategy() string {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("PPTGEN_BRIDGE_AUTO_OVERSIZED_STRATEGY")))
	if v == bridgeAutoOversizedStrategyOutlineExcerpt {
		return bridgeAutoOversizedStrategyOutlineExcerpt
	}
	return bridgeAutoOversizedStrategyChunks
}

func knowledgePageCountHint(k *types.Knowledge) int {
	if k == nil {
		return 0
	}
	meta := k.GetMetadata()
	for _, key := range []string{"page_count", "total_pages", "pages", "num_pages"} {
		s := strings.TrimSpace(meta[key])
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// knowledgeHasBridgeSourceCandidate 是否可能通过 GetKnowledgeFile 拿到可上传的源（手输 Markdown 或已落库的原始文件）。
func knowledgeHasBridgeSourceCandidate(k *types.Knowledge) bool {
	if k == nil {
		return false
	}
	if k.IsManual() {
		return true
	}
	return k.FileSize > 0 && strings.TrimSpace(k.FilePath) != ""
}

func knowledgeSourceByteEstimate(k *types.Knowledge) int64 {
	if k == nil {
		return 0
	}
	if k.IsManual() {
		meta, err := k.ManualMetadata()
		if err != nil || meta == nil {
			return 0
		}
		return int64(len([]byte(meta.Content)))
	}
	return k.FileSize
}

func knowledgeIsOversizedForAutoSource(ctx context.Context, chunkSvc interfaces.ChunkService, k *types.Knowledge) bool {
	if k == nil {
		return false
	}
	maxB := pptgenBridgeSourceMaxBytes()
	if knowledgeSourceByteEstimate(k) > maxB {
		return true
	}
	maxP := pptgenBridgeSourceMaxPages()
	if maxP > 0 {
		if p := knowledgePageCountHint(k); p > maxP {
			return true
		}
	}
	maxR := pptgenBridgeSourceMaxMDRunes()
	if maxR > 0 && chunkSvc != nil {
		if md, _, ok := rawMarkdownFromChunksForBridge(ctx, chunkSvc, k); ok {
			if utf8.RuneCountInString(md) > maxR {
				return true
			}
		}
	}
	return false
}

func isBridgePlainTextKnowledge(k *types.Knowledge) bool {
	if k == nil {
		return false
	}
	ext := strings.ToLower(filepath.Ext(k.FileName))
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".json", ".yaml", ".yml", ".xml", ".log", ".tsv", ".rst", ".adoc":
		return true
	default:
		return false
	}
}

// rawMarkdownFromChunksForBridge 列出 text 等 chunk 并拼出未做 PPTGEN_MARKDOWN_MAX_RUNES 降级的 Markdown。
func rawMarkdownFromChunksForBridge(ctx context.Context, chunkSvc interfaces.ChunkService, k *types.Knowledge) (md string, chunks []*types.Chunk, ok bool) {
	if chunkSvc == nil || k == nil {
		return "", nil, false
	}
	var err error
	chunks, err = chunkSvc.ListChunksByKnowledgeID(ctx, k.ID)
	if err != nil {
		logger.Warnf(ctx, "[kb-bridge] list chunks failed for %s: %v", k.ID, err)
		return "", nil, false
	}
	if len(chunks) == 0 {
		return "", chunks, false
	}
	picked := pickChunksForBridge(chunks, false)
	if len(picked) == 0 {
		return "", chunks, false
	}
	md = composeMarkdownFromPickedKnowledge(k, picked, "")
	if strings.TrimSpace(md) == "" {
		return "", chunks, false
	}
	return md, chunks, true
}

func markdownBridgeFile(k *types.Knowledge, md string) (BridgeFile, int64) {
	data := []byte(md)
	return BridgeFile{
		Name:        mdFileNameForBridge(k),
		ContentType: "text/markdown; charset=utf-8",
		Reader:      io.NopCloser(bytes.NewReader(data)),
	}, int64(len(data))
}

func tryAppendSourceBridgeFile(ctx context.Context, knowSvc interfaces.KnowledgeService, k *types.Knowledge) (BridgeFile, int64, bool) {
	if knowSvc == nil || k == nil {
		return BridgeFile{}, 0, false
	}
	reader, filename, err := knowSvc.GetKnowledgeFile(ctx, k.ID)
	if err != nil {
		return BridgeFile{}, 0, false
	}
	if filename == "" {
		filename = k.FileName
		if filename == "" {
			filename = k.ID + ".bin"
		}
	}
	n := k.FileSize
	if k.IsManual() {
		meta, err := k.ManualMetadata()
		if err == nil && meta != nil {
			n = int64(len([]byte(meta.Content)))
		}
	}
	return BridgeFile{
		Name:        filename,
		ContentType: guessContentType(filename),
		Reader:      reader,
	}, n, true
}

// buildOutlineExcerptMarkdownForBridge 超大源文件的折中：摘要/outline 类 chunk + 手输正文或纯文本源节选。
func buildOutlineExcerptMarkdownForBridge(
	ctx context.Context,
	knowSvc interfaces.KnowledgeService,
	chunkSvc interfaces.ChunkService,
	k *types.Knowledge,
) (string, bool) {
	if chunkSvc == nil || k == nil {
		return "", false
	}
	chunks, err := chunkSvc.ListChunksByKnowledgeID(ctx, k.ID)
	if err != nil {
		logger.Warnf(ctx, "[kb-bridge] outline excerpt: list chunks failed for %s: %v", k.ID, err)
		return "", false
	}
	summaryPicked := pickChunksForBridge(chunks, true)
	banner := "【文档体量较大：以下为摘要/outline 类 chunk，并附可读源文节选（若有）】"
	md := composeMarkdownFromPickedKnowledge(k, summaryPicked, banner)
	excerptMax := pptgenBridgeOutlineExcerptMaxRunes()
	var excerpt string
	if k.IsManual() {
		meta, err := k.ManualMetadata()
		if err == nil && meta != nil && strings.TrimSpace(meta.Content) != "" {
			excerpt = truncateMarkdownRunesBridge(strings.TrimSpace(meta.Content), excerptMax)
		}
	} else if isBridgePlainTextKnowledge(k) && knowSvc != nil {
		r, _, err := knowSvc.GetKnowledgeFile(ctx, k.ID)
		if err == nil && r != nil {
			func() {
				defer r.Close()
				buf, err := io.ReadAll(io.LimitReader(r, 512*1024))
				if err != nil {
					return
				}
				s := string(buf)
				if utf8.ValidString(s) && strings.TrimSpace(s) != "" {
					excerpt = truncateMarkdownRunesBridge(strings.TrimSpace(s), excerptMax)
				}
			}()
		}
	}
	if strings.TrimSpace(md) == "" && excerpt == "" {
		return "", false
	}
	if excerpt != "" {
		if strings.TrimSpace(md) == "" {
			md = "# " + firstNonEmpty(k.Title, k.FileName, k.ID) + "\n\n"
		}
		md += "\n\n## 源文节选\n\n" + excerpt
	}
	return strings.TrimSpace(md), true
}

func collectOneKnowledgeBridgeFile(
	ctx context.Context,
	knowSvc interfaces.KnowledgeService,
	chunkSvc interfaces.ChunkService,
	k *types.Knowledge,
	mode string,
) (BridgeFile, int64, bool) {
	var zero BridgeFile
	switch mode {
	case BridgeInputModeSource:
		if f, n, ok := tryAppendSourceBridgeFile(ctx, knowSvc, k); ok {
			logger.Infof(ctx, "[kb-bridge] knowledge %s -> raw source %s (%d bytes, mode=source)", k.ID, f.Name, n)
			return f, n, true
		}
		if md, ok := buildKnowledgeMarkdownForBridge(ctx, chunkSvc, k); ok {
			f, n := markdownBridgeFile(k, md)
			logger.Infof(ctx, "[kb-bridge] knowledge %s -> markdown %d bytes (mode=source, chunk fallback)", k.ID, n)
			return f, n, true
		}
		logger.Warnf(ctx, "[kb-bridge] skip knowledge %s: mode=source but no source/chunk payload", k.ID)
		return zero, 0, false

	case BridgeInputModeChunks:
		if md, ok := buildKnowledgeMarkdownForBridge(ctx, chunkSvc, k); ok {
			f, n := markdownBridgeFile(k, md)
			logger.Infof(ctx, "[kb-bridge] knowledge %s -> markdown %d bytes (chunk-based)", k.ID, n)
			return f, n, true
		}
		if f, n, ok := tryAppendSourceBridgeFile(ctx, knowSvc, k); ok {
			logger.Warnf(ctx, "[kb-bridge] knowledge %s has no chunk, fall back to raw file %s", k.ID, f.Name)
			return f, n, true
		}
		logger.Warnf(ctx, "[kb-bridge] skip knowledge %s: no chunk and file unavailable", k.ID)
		return zero, 0, false

	case BridgeInputModeAuto:
		hasSrc := knowledgeHasBridgeSourceCandidate(k)
		oversized := hasSrc && knowledgeIsOversizedForAutoSource(ctx, chunkSvc, k)
		if hasSrc && !oversized {
			if f, n, ok := tryAppendSourceBridgeFile(ctx, knowSvc, k); ok {
				logger.Infof(ctx, "[kb-bridge] knowledge %s -> raw source %s (%d bytes, mode=auto)", k.ID, f.Name, n)
				return f, n, true
			}
		}
		if oversized {
			logger.Infof(ctx, "[kb-bridge] knowledge %s mode=auto: treat as oversized (bytes/pages/md-runes vs env thresholds)", k.ID)
		}
		if oversized && pptgenBridgeAutoOversizedStrategy() == bridgeAutoOversizedStrategyOutlineExcerpt {
			if md, ok := buildOutlineExcerptMarkdownForBridge(ctx, knowSvc, chunkSvc, k); ok {
				limit := pptgenMarkdownMaxRunes()
				if utf8.RuneCountInString(md) > limit {
					md = truncateMarkdownRunesBridge(md, limit)
				}
				f, n := markdownBridgeFile(k, md)
				logger.Infof(ctx, "[kb-bridge] knowledge %s -> markdown %d bytes (mode=auto, outline_excerpt)", k.ID, n)
				return f, n, true
			}
		}
		if md, ok := buildKnowledgeMarkdownForBridge(ctx, chunkSvc, k); ok {
			f, n := markdownBridgeFile(k, md)
			logger.Infof(ctx, "[kb-bridge] knowledge %s -> markdown %d bytes (mode=auto, chunks)", k.ID, n)
			return f, n, true
		}
		if oversized {
			logger.Warnf(ctx, "[kb-bridge] skip knowledge %s: mode=auto oversized and no chunk/outline export", k.ID)
			return zero, 0, false
		}
		if f, n, ok := tryAppendSourceBridgeFile(ctx, knowSvc, k); ok {
			logger.Warnf(ctx, "[kb-bridge] knowledge %s fall back to raw file %s (mode=auto)", k.ID, f.Name)
			return f, n, true
		}
		logger.Warnf(ctx, "[kb-bridge] skip knowledge %s: mode=auto but no exportable payload", k.ID)
		return zero, 0, false

	default:
		return collectOneKnowledgeBridgeFile(ctx, knowSvc, chunkSvc, k, BridgeInputModeChunks)
	}
}

// CollectKnowledgeBridgeFiles 将知识列表转为 Bridge multipart 用的文件流。
// inputMode 通常由 ResolvePPTGenBridgeInputMode（全局 env + 可选知识库 chunking_config.pptgen_bridge_input_mode）得到；
// 闪卡等场景可固定传入 BridgeInputModeChunks 以保持历史行为。
func CollectKnowledgeBridgeFiles(
	ctx context.Context,
	knowSvc interfaces.KnowledgeService,
	chunkSvc interfaces.ChunkService,
	list []*types.Knowledge,
	inputMode string,
) ([]BridgeFile, int64, error) {
	mode := strings.TrimSpace(strings.ToLower(inputMode))
	if mode == "" {
		mode = BridgeInputModeChunks
	}
	files := make([]BridgeFile, 0, len(list))
	var total int64
	for _, k := range list {
		f, n, ok := collectOneKnowledgeBridgeFile(ctx, knowSvc, chunkSvc, k, mode)
		if !ok {
			continue
		}
		files = append(files, f)
		total += n
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
	md, chunks, ok := rawMarkdownFromChunksForBridge(ctx, chunkSvc, k)
	if !ok {
		return "", false
	}

	limit := pptgenMarkdownMaxRunes()
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
		picked := pickChunksForBridge(chunks, false)
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
