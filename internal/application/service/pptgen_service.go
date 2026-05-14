package service

import (
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PPTGenService 实现 interfaces.PPTGenService。
// 设计上保持轻量、单实例：
//   - 任务记录在内存里，重启即丢失，前端展示也仅作为"最近一次任务"提示；
//   - 实际生成进度从 Bridge 实时拉取，本地只缓存上次结果减少远端访问；
//   - 单文件超过 Bridge 限制时直接跳过并记录原因，避免整批失败。
type PPTGenService struct {
	client    *PPTAgentBridgeClient
	knowSvc   interfaces.KnowledgeService
	kbSvc     interfaces.KnowledgeBaseService
	chunkSvc  interfaces.ChunkService
	modelSvc  interfaces.ModelService

	mu    sync.RWMutex
	tasks map[string]*types.PPTGenTask // taskID -> task
	// kbIndex 记录每个 KB 最近创建的若干任务，便于前端进入页面时回显历史。
	kbIndex map[string][]string
}

// NewPPTGenService DI 构造函数。
func NewPPTGenService(
	client *PPTAgentBridgeClient,
	knowSvc interfaces.KnowledgeService,
	kbSvc interfaces.KnowledgeBaseService,
	chunkSvc interfaces.ChunkService,
	modelSvc interfaces.ModelService,
) interfaces.PPTGenService {
	return &PPTGenService{
		client:   client,
		knowSvc:  knowSvc,
		kbSvc:    kbSvc,
		chunkSvc: chunkSvc,
		modelSvc: modelSvc,
		tasks:    make(map[string]*types.PPTGenTask),
		kbIndex:  make(map[string][]string),
	}
}

// CreateTask 实现 interfaces.PPTGenService。
func (s *PPTGenService) CreateTask(
	ctx context.Context,
	kbID string,
	req *types.PPTGenCreateRequest,
) (*types.PPTGenTask, error) {
	if req == nil {
		req = &types.PPTGenCreateRequest{}
	}

	kb, err := s.kbSvc.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil {
		return nil, errors.NewNotFoundError("knowledge base not found").WithDetails(err.Error())
	}
	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	userID, _ := ctx.Value(types.UserIDContextKey).(string)

	// 1) 列出所有知识。
	list, err := s.knowSvc.ListKnowledgeByKnowledgeBaseID(ctx, kbID)
	if err != nil {
		return nil, errors.NewInternalServerError("list knowledge failed").WithDetails(err.Error())
	}
	included, skipped := SelectKnowledgeForBridge(list, req.IncludeFileIDs)
	if len(included) == 0 {
		return nil, errors.NewBadRequestError("knowledge base has no eligible documents for PPT generation")
	}

	// 2) 组装 bridge 文件流。
	files, totalBytes, err := CollectKnowledgeBridgeFiles(ctx, s.knowSvc, s.chunkSvc, included)
	if err != nil {
		// 已打开的流由 collectFiles 自己负责关闭出错的部分；剩余流随 client 关闭处理。
		closeBridgeFiles(files)
		return nil, errors.NewInternalServerError("collect knowledge files failed").WithDetails(err.Error())
	}

	// 3) 构造 meta 并调用 Bridge。
	meta := &BridgeCreateMeta{
		Instruction:     strings.TrimSpace(req.Instruction),
		Language:        req.Language,
		Template:        req.Template,
		Title:           firstNonEmpty(req.Title, kb.Name, "WeKnora-Presentation"),
		WeKnoraTenantID: tenantID,
		WeKnoraKBID:     kbID,
		WeKnoraUserID:   userID,
		Extra: map[string]interface{}{
			"weknora_kb_name":      kb.Name,
			"weknora_total_files":  len(included),
			"weknora_total_bytes":  totalBytes,
		},
	}
	if req.NumPages > 0 {
		n := req.NumPages
		meta.NumPages = &n
	}

	if err := s.applyModelOverridesToMeta(ctx, req, meta); err != nil {
		closeBridgeFiles(files)
		return nil, err
	}

	bridgeResp, err := s.client.CreateTask(ctx, meta, files)
	if err != nil {
		logger.Errorf(ctx, "[pptgen] bridge create failed: %v", err)
		return nil, errors.NewInternalServerError("pptagent bridge create failed").WithDetails(err.Error())
	}

	now := time.Now()
	task := &types.PPTGenTask{
		TaskID:          uuid.New().String(),
		BridgeTaskID:    bridgeResp.TaskID,
		KnowledgeBaseID: kbID,
		TenantID:        tenantID,
		UserID:          userID,
		Title:           meta.Title,
		Instruction:     meta.Instruction,
		NumPages:        req.NumPages,
		Template:        req.Template,
		Status:          bridgeResp.Status,
		Progress:        0,
		Message:         bridgeResp.Message,
		FilesIncluded:   len(included),
		FilesSkipped:    len(skipped),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	s.storeTask(task)
	logger.Infof(ctx, "[pptgen] task created: local=%s bridge=%s kb=%s files=%d skipped=%d",
		task.TaskID, task.BridgeTaskID, kbID, len(included), len(skipped))
	return task, nil
}

// GetTask 实现 interfaces.PPTGenService。
func (s *PPTGenService) GetTask(ctx context.Context, taskID string) (*types.PPTGenTask, error) {
	task := s.loadTask(taskID)
	if task == nil {
		return nil, errors.NewNotFoundError("ppt generation task not found")
	}
	if task.Status == types.PPTGenStatusSucceeded ||
		task.Status == types.PPTGenStatusFailed ||
		task.Status == types.PPTGenStatusCancelled {
		return task, nil
	}
	// 终态前，主动同步一次进度。
	info, err := s.client.GetTask(ctx, task.BridgeTaskID)
	if err != nil {
		// Bridge 端找不到任务时通常是已 GC 或 Bridge 重启，标记失败以便前端重试。
		if err == ErrPPTAgentTaskNotFound {
			task.Status = types.PPTGenStatusFailed
			task.Error = "PPTAgent Bridge 已丢失任务，请重试"
			task.Message = task.Error
			task.UpdatedAt = time.Now()
			s.storeTask(task)
		}
		return task, nil
	}
	s.mergeBridgeInfo(task, info)
	s.storeTask(task)
	return task, nil
}

// CancelTask 实现 interfaces.PPTGenService。
func (s *PPTGenService) CancelTask(ctx context.Context, taskID string) (*types.PPTGenTask, error) {
	task := s.loadTask(taskID)
	if task == nil {
		return nil, errors.NewNotFoundError("ppt generation task not found")
	}
	info, err := s.client.CancelTask(ctx, task.BridgeTaskID)
	if err != nil && err != ErrPPTAgentTaskNotFound {
		return nil, errors.NewInternalServerError("cancel bridge task failed").WithDetails(err.Error())
	}
	if info != nil {
		s.mergeBridgeInfo(task, info)
	} else {
		task.Status = types.PPTGenStatusCancelled
		task.Message = "已取消"
		task.UpdatedAt = time.Now()
	}
	s.storeTask(task)
	return task, nil
}

// DownloadResult 实现 interfaces.PPTGenService。
func (s *PPTGenService) DownloadResult(ctx context.Context, taskID string) (io.ReadCloser, string, error) {
	task := s.loadTask(taskID)
	if task == nil {
		return nil, "", errors.NewNotFoundError("ppt generation task not found")
	}
	if task.Status != types.PPTGenStatusSucceeded {
		return nil, "", errors.NewBadRequestError(fmt.Sprintf("task status %q is not ready for download", task.Status))
	}
	stream, name, err := s.client.DownloadResult(ctx, task.BridgeTaskID)
	if err != nil {
		if err == ErrPPTAgentResultNotReady {
			return nil, "", errors.NewBadRequestError("ppt result not ready")
		}
		return nil, "", errors.NewInternalServerError("download bridge result failed").WithDetails(err.Error())
	}
	// 优先使用我们自己拼出的标题作为文件名（用户更友好）。
	if task.Title != "" {
		safe := sanitizePPTFilename(task.Title)
		if safe != "" {
			name = safe + ".pptx"
		}
	}
	return stream, name, nil
}

// ListTasksForKB 实现 interfaces.PPTGenService。
func (s *PPTGenService) ListTasksForKB(ctx context.Context, kbID string, limit int) ([]*types.PPTGenTask, error) {
	if limit <= 0 {
		limit = 20
	}
	s.mu.RLock()
	ids := append([]string(nil), s.kbIndex[kbID]...)
	tasks := make([]*types.PPTGenTask, 0, len(ids))
	for _, id := range ids {
		if t, ok := s.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	s.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
	})
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, nil
}

// HealthCheck 实现 interfaces.PPTGenService。
func (s *PPTGenService) HealthCheck(ctx context.Context) error {
	return s.client.Health(ctx)
}

// ================ 内部 helpers ================

// pptgenMarkdownMaxRunes 单份知识导出 Markdown 的码点上限（与 PPTAgent 内部对超长输入的预警尺度同量级）。
// 超出后优先改为仅摘要类 chunk；仍超长则硬截断。可通过环境变量 PPTGEN_MARKDOWN_MAX_RUNES 调整。
func pptgenMarkdownMaxRunes() int {
	v := strings.TrimSpace(os.Getenv("PPTGEN_MARKDOWN_MAX_RUNES"))
	if v == "" {
		return 28000
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2048 {
		return 28000
	}
	if n > 500000 {
		return 500000
	}
	return n
}

func closeBridgeFiles(files []BridgeFile) {
	for _, f := range files {
		if f.Reader != nil {
			_ = f.Reader.Close()
		}
	}
}

// applyModelOverridesToMeta 将全局设置中选中的模型解析为 Bridge 可识别的 ppt_llm_* / ppt_vlm_* 字段。
func (s *PPTGenService) applyModelOverridesToMeta(
	ctx context.Context,
	req *types.PPTGenCreateRequest,
	meta *BridgeCreateMeta,
) error {
	if s.modelSvc == nil {
		if strings.TrimSpace(req.LLMModelID) != "" || strings.TrimSpace(req.VLMModelID) != "" {
			return errors.NewBadRequestError("模型服务未就绪，无法按全局配置覆盖 PPT 生成模型")
		}
		return nil
	}
	ttl := 600
	if strings.TrimSpace(req.LLMModelID) != "" {
		m, err := s.modelSvc.GetModelByID(ctx, req.LLMModelID)
		if err != nil || m == nil {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			return errors.NewNotFoundError("对话模型不存在或无权访问").WithDetails(detail)
		}
		if m.Type != types.ModelTypeKnowledgeQA {
			return errors.NewBadRequestError("PPT 主模型必须选择「对话」类（KnowledgeQA）模型")
		}
		base := strings.TrimSpace(m.Parameters.BaseURL)
		name := strings.TrimSpace(m.Name)
		if base == "" || name == "" {
			return errors.NewBadRequestError("所选对话模型缺少 base_url 或模型名称")
		}
		meta.PptLLMBaseURL = base
		meta.PptLLMModel = name
		meta.PptLLMAPIKey = m.Parameters.APIKey
		meta.PptLLMTimeout = &ttl
	}
	if strings.TrimSpace(req.VLMModelID) != "" {
		m, err := s.modelSvc.GetModelByID(ctx, req.VLMModelID)
		if err != nil || m == nil {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			return errors.NewNotFoundError("视觉模型不存在或无权访问").WithDetails(detail)
		}
		if m.Type != types.ModelTypeVLLM {
			return errors.NewBadRequestError("PPT 视觉模型必须选择「多模态」（VLLM）模型")
		}
		base := strings.TrimSpace(m.Parameters.BaseURL)
		name := strings.TrimSpace(m.Name)
		if base == "" || name == "" {
			return errors.NewBadRequestError("所选视觉模型缺少 base_url 或模型名称")
		}
		meta.PptVLMBaseURL = base
		meta.PptVLMModel = name
		meta.PptVLMAPIKey = m.Parameters.APIKey
		meta.PptVLMTimeout = &ttl
	}
	return nil
}

// TestModelConnection 将所选模型参数转发给 Bridge，由 Bridge 发起最小 Chat Completions 探测。
func (s *PPTGenService) TestModelConnection(
	ctx context.Context,
	req *types.PPTGenModelTestRequest,
) (*types.PPTGenModelTestResponse, error) {
	if req == nil {
		return nil, errors.NewBadRequestError("请求体不能为空")
	}
	if strings.TrimSpace(req.LLMModelID) == "" && strings.TrimSpace(req.VLMModelID) == "" {
		return nil, errors.NewBadRequestError("请至少选择对话模型或视觉模型之一进行测试")
	}
	if s.modelSvc == nil {
		return nil, errors.NewInternalServerError("模型服务未就绪")
	}
	out := &types.PPTGenModelTestResponse{}
	if id := strings.TrimSpace(req.LLMModelID); id != "" {
		m, err := s.modelSvc.GetModelByID(ctx, id)
		if err != nil || m == nil {
			return nil, errors.NewNotFoundError("对话模型不存在").WithDetails(errString(err))
		}
		if m.Type != types.ModelTypeKnowledgeQA {
			return nil, errors.NewBadRequestError("PPT 主模型测试须选择对话类（KnowledgeQA）模型")
		}
		br, err := s.client.TestLLM(ctx, &BridgeLLMTestRequest{
			BaseURL: strings.TrimSpace(m.Parameters.BaseURL),
			Model:   strings.TrimSpace(m.Name),
			APIKey:  m.Parameters.APIKey,
			Timeout: 30,
		})
		ok := false
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			ok = br.OK
			msg = br.Message
		}
		out.LLMOk = &ok
		out.LLMMessage = msg
	}
	if id := strings.TrimSpace(req.VLMModelID); id != "" {
		m, err := s.modelSvc.GetModelByID(ctx, id)
		if err != nil || m == nil {
			return nil, errors.NewNotFoundError("视觉模型不存在").WithDetails(errString(err))
		}
		if m.Type != types.ModelTypeVLLM {
			return nil, errors.NewBadRequestError("视觉模型测试须选择 VLLM 类型模型")
		}
		br, err := s.client.TestLLM(ctx, &BridgeLLMTestRequest{
			BaseURL: strings.TrimSpace(m.Parameters.BaseURL),
			Model:   strings.TrimSpace(m.Name),
			APIKey:  m.Parameters.APIKey,
			Timeout: 30,
		})
		ok := false
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			ok = br.OK
			msg = br.Message
		}
		out.VLMOk = &ok
		out.VLMMessage = msg
	}
	return out, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// PurgeTask 删除本地索引并请求 Bridge 物理删除任务目录（含 pptx）。
func (s *PPTGenService) PurgeTask(ctx context.Context, taskID string) error {
	t := s.loadTask(taskID)
	if t == nil {
		return errors.NewNotFoundError("ppt generation task not found")
	}
	err := s.client.PurgeTask(ctx, t.BridgeTaskID)
	if err != nil && err != ErrPPTAgentTaskNotFound {
		if strings.Contains(strings.ToLower(err.Error()), "conflict") ||
			strings.Contains(err.Error(), "尚未结束") {
			return errors.NewBadRequestError("任务尚未结束，请取消或等待完成后再删除")
		}
		return errors.NewInternalServerError("删除 Bridge 任务失败").WithDetails(err.Error())
	}
	s.removeLocalTask(taskID, t.KnowledgeBaseID)
	return nil
}

func (s *PPTGenService) removeLocalTask(taskID, kbID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	ids := s.kbIndex[kbID]
	dst := ids[:0]
	for _, id := range ids {
		if id != taskID {
			dst = append(dst, id)
		}
	}
	s.kbIndex[kbID] = dst
}

// PreviewResult 返回 PDF 预览流（Bridge 内通过 LibreOffice 将 pptx 转 pdf）。
func (s *PPTGenService) PreviewResult(ctx context.Context, taskID string) (io.ReadCloser, error) {
	t := s.loadTask(taskID)
	if t == nil {
		return nil, errors.NewNotFoundError("ppt generation task not found")
	}
	if t.Status != types.PPTGenStatusSucceeded {
		return nil, errors.NewBadRequestError("仅生成成功的任务可预览")
	}
	return s.client.PreviewResult(ctx, t.BridgeTaskID)
}

func (s *PPTGenService) storeTask(task *types.PPTGenTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, existed := s.tasks[task.TaskID]; !existed {
		s.kbIndex[task.KnowledgeBaseID] = append([]string{task.TaskID}, s.kbIndex[task.KnowledgeBaseID]...)
		if len(s.kbIndex[task.KnowledgeBaseID]) > 50 {
			s.kbIndex[task.KnowledgeBaseID] = s.kbIndex[task.KnowledgeBaseID][:50]
		}
	}
	s.tasks[task.TaskID] = task
}

func (s *PPTGenService) loadTask(taskID string) *types.PPTGenTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tasks[taskID]
	if t == nil {
		return nil
	}
	clone := *t
	return &clone
}

func (s *PPTGenService) mergeBridgeInfo(task *types.PPTGenTask, info *BridgeTaskInfo) {
	if info == nil {
		return
	}
	task.Status = info.Status
	task.Progress = info.Progress
	task.Message = info.Message
	task.Error = info.Error
	task.UpdatedAt = time.Now()
	if task.Status == types.PPTGenStatusSucceeded ||
		task.Status == types.PPTGenStatusFailed ||
		task.Status == types.PPTGenStatusCancelled {
		now := time.Now()
		task.CompletedAt = &now
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func guessContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// sanitizePPTFilename 把可能出现在标题中的非法路径字符替换为下划线，
// 避免下载到客户端时被浏览器/操作系统拒绝。
func sanitizePPTFilename(name string) string {
	const bad = "<>:\"/\\|?*\n\r\t"
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if strings.ContainsRune(bad, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	result := strings.TrimSpace(b.String())
	if result == "" {
		return "presentation"
	}
	return result
}
