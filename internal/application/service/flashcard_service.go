package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// FlashcardGenService 实现 interfaces.FlashcardGenService：聚合知识库 Markdown 并调用闪卡 Bridge。
// 任务记录保存在内存中（与 PPT 生成一致），重启后历史丢失。
type FlashcardGenService struct {
	client   *FlashcardBridgeClient
	knowSvc  interfaces.KnowledgeService
	kbSvc    interfaces.KnowledgeBaseService
	chunkSvc interfaces.ChunkService
	modelSvc interfaces.ModelService

	mu      sync.RWMutex
	tasks   map[string]*types.FlashcardGenTask
	kbIndex map[string][]string
}

// NewFlashcardGenService DI 构造函数。
func NewFlashcardGenService(
	client *FlashcardBridgeClient,
	knowSvc interfaces.KnowledgeService,
	kbSvc interfaces.KnowledgeBaseService,
	chunkSvc interfaces.ChunkService,
	modelSvc interfaces.ModelService,
) interfaces.FlashcardGenService {
	return &FlashcardGenService{
		client:   client,
		knowSvc:  knowSvc,
		kbSvc:    kbSvc,
		chunkSvc: chunkSvc,
		modelSvc: modelSvc,
		tasks:    make(map[string]*types.FlashcardGenTask),
		kbIndex:  make(map[string][]string),
	}
}

// GenerateFromKnowledgeBase 同步生成（兼容 POST .../flashcards/generate）。
func (s *FlashcardGenService) GenerateFromKnowledgeBase(
	ctx context.Context,
	kbID string,
	req *types.FlashcardGenerateRequest,
) (*types.FlashcardGenerateResult, error) {
	result, _, err := s.runGeneration(ctx, kbID, req, nil)
	return result, err
}

// CreateTask 创建闪卡任务并在后台执行生成，立即返回任务元数据。
func (s *FlashcardGenService) CreateTask(
	ctx context.Context,
	kbID string,
	req *types.FlashcardGenerateRequest,
) (*types.FlashcardGenTask, error) {
	if req == nil {
		req = &types.FlashcardGenerateRequest{}
	}
	topic := strings.TrimSpace(req.Topic)
	if topic == "" {
		return nil, errors.NewBadRequestError("请填写闪卡主题（topic）")
	}
	if _, err := s.kbSvc.GetKnowledgeBaseByID(ctx, kbID); err != nil {
		return nil, errors.NewNotFoundError("知识库不存在").WithDetails(err.Error())
	}

	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	userID, _ := ctx.Value(types.UserIDContextKey).(string)
	count := normalizeFlashcardCount(req.Count)
	lang := strings.TrimSpace(req.Language)
	if lang == "" {
		lang = "zh"
	}

	now := time.Now()
	task := &types.FlashcardGenTask{
		TaskID:          uuid.New().String(),
		KnowledgeBaseID: kbID,
		TenantID:        tenantID,
		UserID:          userID,
		Topic:           topic,
		Count:           count,
		Language:        lang,
		LLMModelID:      strings.TrimSpace(req.LLMModelID),
		Status:          types.FlashcardStatusPending,
		Progress:        0,
		Message:         "任务已创建，等待处理",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.storeTask(task)

	reqCopy := *req
	go s.executeTaskBackground(task.TaskID, kbID, &reqCopy, tenantID, userID)

	logger.Infof(ctx, "[flashcard] task created: id=%s kb=%s topic=%q", task.TaskID, kbID, topic)
	return s.loadTask(task.TaskID), nil
}

func (s *FlashcardGenService) executeTaskBackground(
	taskID, kbID string,
	req *types.FlashcardGenerateRequest,
	tenantID uint64,
	userID string,
) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, types.TenantIDContextKey, tenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, userID)

	s.patchTask(taskID, func(t *types.FlashcardGenTask) {
		t.Status = types.FlashcardStatusGenerating
		t.Progress = 5
		t.Message = "正在导出知识库内容"
	})

	result, stats, err := s.runGeneration(ctx, kbID, req, func(progress int, message string) {
		s.patchTask(taskID, func(t *types.FlashcardGenTask) {
			if progress > t.Progress {
				t.Progress = progress
			}
			if message != "" {
				t.Message = message
			}
		})
	})

	now := time.Now()
	if err != nil {
		s.patchTask(taskID, func(t *types.FlashcardGenTask) {
			t.Status = types.FlashcardStatusFailed
			t.Progress = 100
			t.Error = err.Error()
			t.Message = "闪卡生成失败"
			t.UpdatedAt = now
			t.CompletedAt = &now
		})
		logger.Errorf(ctx, "[flashcard] task %s failed: %v", taskID, err)
		return
	}

	s.patchTask(taskID, func(t *types.FlashcardGenTask) {
		t.Status = types.FlashcardStatusSucceeded
		t.Progress = 100
		t.Message = firstNonEmpty(result.Message, "闪卡生成成功")
		t.Error = ""
		t.Topic = result.Topic
		t.Flashcards = result.Flashcards
		t.Citations = result.Citations
		if stats != nil {
			t.FilesIncluded = stats.filesIncluded
			t.FilesSkipped = stats.filesSkipped
		}
		t.UpdatedAt = now
		t.CompletedAt = &now
	})
	logger.Infof(ctx, "[flashcard] task %s succeeded: cards=%d", taskID, len(result.Flashcards))
}

type flashcardGenStats struct {
	filesIncluded int
	filesSkipped  int
}

type flashcardProgressFn func(progress int, message string)

// runGeneration 执行一次闪卡生成；progress 可选，用于异步任务更新进度。
func (s *FlashcardGenService) runGeneration(
	ctx context.Context,
	kbID string,
	req *types.FlashcardGenerateRequest,
	progress flashcardProgressFn,
) (*types.FlashcardGenerateResult, *flashcardGenStats, error) {
	if req == nil {
		req = &types.FlashcardGenerateRequest{}
	}
	topic := strings.TrimSpace(req.Topic)
	if topic == "" {
		return nil, nil, errors.NewBadRequestError("请填写闪卡主题（topic）")
	}

	kb, err := s.kbSvc.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil {
		return nil, nil, errors.NewNotFoundError("知识库不存在").WithDetails(err.Error())
	}
	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	userID, _ := ctx.Value(types.UserIDContextKey).(string)

	if progress != nil {
		progress(10, "正在读取知识库文档")
	}
	list, err := s.knowSvc.ListKnowledgeByKnowledgeBaseID(ctx, kbID)
	if err != nil {
		return nil, nil, errors.NewInternalServerError("列出知识失败").WithDetails(err.Error())
	}
	included, skipped := SelectKnowledgeForBridge(list, req.IncludeFileIDs)
	if len(included) == 0 {
		return nil, nil, errors.NewBadRequestError("知识库中没有可用于生成闪卡的文档（或所选文件均不可用）")
	}
	stats := &flashcardGenStats{filesIncluded: len(included), filesSkipped: len(skipped)}

	if progress != nil {
		progress(25, "正在合并知识摘录")
	}
	files, _, err := CollectKnowledgeBridgeFiles(ctx, s.knowSvc, s.chunkSvc, included, BridgeInputModeChunks)
	if err != nil {
		closeBridgeFiles(files)
		return nil, stats, errors.NewInternalServerError("导出知识内容失败").WithDetails(err.Error())
	}

	count := normalizeFlashcardCount(req.Count)
	lang := strings.TrimSpace(req.Language)
	if lang == "" {
		lang = "zh"
	}

	meta := &FlashcardBridgeMeta{
		Topic:           topic,
		Count:           count,
		Language:        lang,
		WeKnoraTenantID: tenantID,
		WeKnoraKBID:     kbID,
		WeKnoraUserID:   userID,
	}
	// 未在前端选择对话模型时，不向 Bridge 下发 flash_llm_*，由容器内 FLASHCARD_LLM_*（.env）提供默认端点。
	// 避免误用知识库绑定的华为 MaaS 等与闪卡 .env 不一致的模型。
	fcReq := *req
	if err := s.applyFlashModelToMeta(ctx, &fcReq, meta); err != nil {
		closeBridgeFiles(files)
		return nil, stats, err
	}

	if progress != nil {
		progress(45, "正在调用大模型生成闪卡")
	}
	resp, err := s.client.Generate(ctx, meta, files)
	if err != nil {
		logger.Errorf(ctx, "[flashcard] bridge failed: %v", err)
		return nil, stats, errors.NewInternalServerError("闪卡生成失败").WithDetails(err.Error())
	}

	if progress != nil {
		progress(90, "正在整理闪卡结果")
	}
	out := &types.FlashcardGenerateResult{
		Topic:      resp.Topic,
		Citations:  resp.Citations,
		Message:    resp.Message,
		Flashcards: make([]types.FlashcardItem, 0, len(resp.Flashcards)),
	}
	for _, c := range resp.Flashcards {
		out.Flashcards = append(out.Flashcards, types.FlashcardItem{Front: c.Front, Back: c.Back})
	}
	return out, stats, nil
}

// GetTask 返回任务详情（含已保存的闪卡 JSON）。
func (s *FlashcardGenService) GetTask(ctx context.Context, taskID string) (*types.FlashcardGenTask, error) {
	_ = ctx
	t := s.loadTask(taskID)
	if t == nil {
		return nil, errors.NewNotFoundError("闪卡生成任务不存在")
	}
	return t, nil
}

// ListTasksForKB 列出某知识库最近的闪卡任务。
func (s *FlashcardGenService) ListTasksForKB(ctx context.Context, kbID string, limit int) ([]*types.FlashcardGenTask, error) {
	_ = ctx
	if limit <= 0 {
		limit = 20
	}
	s.mu.RLock()
	ids := append([]string(nil), s.kbIndex[kbID]...)
	tasks := make([]*types.FlashcardGenTask, 0, len(ids))
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
	out := make([]*types.FlashcardGenTask, len(tasks))
	for i, t := range tasks {
		clone := *t
		out[i] = &clone
	}
	return out, nil
}

// PurgeTask 从内存删除任务记录（仅终态可删）。
func (s *FlashcardGenService) PurgeTask(ctx context.Context, taskID string) error {
	_ = ctx
	t := s.loadTask(taskID)
	if t == nil {
		return errors.NewNotFoundError("闪卡生成任务不存在")
	}
	if t.Status != types.FlashcardStatusSucceeded && t.Status != types.FlashcardStatusFailed {
		return errors.NewBadRequestError("任务尚未结束，请等待完成后再删除")
	}
	s.removeLocalTask(taskID, t.KnowledgeBaseID)
	return nil
}

// HealthCheck 探测闪卡 Bridge。
func (s *FlashcardGenService) HealthCheck(ctx context.Context) error {
	if s.client == nil {
		return errors.NewInternalServerError("闪卡客户端未初始化")
	}
	return s.client.Health(ctx)
}

func normalizeFlashcardCount(count int) int {
	if count <= 0 {
		return 10
	}
	if count > 50 {
		return 50
	}
	return count
}

func (s *FlashcardGenService) applyFlashModelToMeta(
	ctx context.Context,
	req *types.FlashcardGenerateRequest,
	meta *FlashcardBridgeMeta,
) error {
	if strings.TrimSpace(req.LLMModelID) == "" {
		return nil
	}
	if s.modelSvc == nil {
		return errors.NewBadRequestError("模型服务未就绪，无法使用所选 LLM")
	}
	m, err := s.modelSvc.GetModelByID(ctx, req.LLMModelID)
	if err != nil || m == nil {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		return errors.NewNotFoundError("对话模型不存在或无权访问").WithDetails(detail)
	}
	if m.Type != types.ModelTypeKnowledgeQA {
		return errors.NewBadRequestError("闪卡生成须选择「对话」类（KnowledgeQA）模型")
	}
	base := strings.TrimSpace(m.Parameters.BaseURL)
	name := strings.TrimSpace(m.Name)
	if base == "" || name == "" {
		return errors.NewBadRequestError("所选模型缺少 base_url 或模型名称")
	}
	ttl := 120
	meta.FlashLLMBaseURL = base
	meta.FlashLLMModel = name
	meta.FlashLLMAPIKey = m.Parameters.APIKey
	meta.FlashLLMTimeout = &ttl
	return nil
}

func (s *FlashcardGenService) storeTask(task *types.FlashcardGenTask) {
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

func (s *FlashcardGenService) patchTask(taskID string, fn func(*types.FlashcardGenTask)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	if t == nil {
		return
	}
	fn(t)
	t.UpdatedAt = time.Now()
}

func (s *FlashcardGenService) loadTask(taskID string) *types.FlashcardGenTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tasks[taskID]
	if t == nil {
		return nil
	}
	clone := *t
	if t.Flashcards != nil {
		clone.Flashcards = append([]types.FlashcardItem(nil), t.Flashcards...)
	}
	return &clone
}

func (s *FlashcardGenService) removeLocalTask(taskID, kbID string) {
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

var _ interfaces.FlashcardGenService = (*FlashcardGenService)(nil)
