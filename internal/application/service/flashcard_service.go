package service

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// FlashcardGenService 实现 interfaces.FlashcardGenService：聚合知识库 Markdown 并调用闪卡 Bridge。
type FlashcardGenService struct {
	client   *FlashcardBridgeClient
	knowSvc  interfaces.KnowledgeService
	kbSvc    interfaces.KnowledgeBaseService
	chunkSvc interfaces.ChunkService
	modelSvc interfaces.ModelService
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
	}
}

// GenerateFromKnowledgeBase 拉取知识库文档，导出为 Markdown 文件流后请求 Bridge 生成闪卡。
func (s *FlashcardGenService) GenerateFromKnowledgeBase(
	ctx context.Context,
	kbID string,
	req *types.FlashcardGenerateRequest,
) (*types.FlashcardGenerateResult, error) {
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

	list, err := s.knowSvc.ListKnowledgeByKnowledgeBaseID(ctx, kbID)
	if err != nil {
		return nil, errors.NewInternalServerError("列出知识失败").WithDetails(err.Error())
	}
	included, skipped := SelectKnowledgeForBridge(list, req.IncludeFileIDs)
	if len(included) == 0 {
		return nil, errors.NewBadRequestError("知识库中没有可用于生成闪卡的文档（或所选文件均不可用）")
	}
	_ = skipped // 仅统计；若需可记录日志

	files, _, err := CollectKnowledgeBridgeFiles(ctx, s.knowSvc, s.chunkSvc, included, BridgeInputModeChunks)
	if err != nil {
		closeBridgeFiles(files)
		return nil, errors.NewInternalServerError("导出知识内容失败").WithDetails(err.Error())
	}

	count := req.Count
	if count <= 0 {
		count = 10
	}
	if count > 50 {
		count = 50
	}
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
	if err := s.applyFlashModelToMeta(ctx, req, meta); err != nil {
		closeBridgeFiles(files)
		return nil, err
	}

	resp, err := s.client.Generate(ctx, meta, files)
	if err != nil {
		logger.Errorf(ctx, "[flashcard] bridge failed: %v", err)
		return nil, errors.NewInternalServerError("闪卡生成失败").WithDetails(err.Error())
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
	logger.Infof(ctx, "[flashcard] done kb=%s topic=%s cards=%d skipped_docs=%d", kbID, topic, len(out.Flashcards), len(skipped))
	return out, nil
}

// HealthCheck 探测闪卡 Bridge。
func (s *FlashcardGenService) HealthCheck(ctx context.Context) error {
	if s.client == nil {
		return errors.NewInternalServerError("闪卡客户端未初始化")
	}
	return s.client.Health(ctx)
}

// applyFlashModelToMeta 将租户所选对话模型写入 meta，供 Bridge 调用 OpenAI 兼容接口。
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

var _ interfaces.FlashcardGenService = (*FlashcardGenService)(nil)
