package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// FlashcardGenHandler 处理知识库闪卡生成 HTTP 请求。
type FlashcardGenHandler struct {
	svc interfaces.FlashcardGenService
}

// NewFlashcardGenHandler DI 构造函数。
func NewFlashcardGenHandler(svc interfaces.FlashcardGenService) *FlashcardGenHandler {
	return &FlashcardGenHandler{svc: svc}
}

type flashcardCreateTaskRequest struct {
	Topic          string   `json:"topic"`
	Count          int      `json:"count"`
	Language       string   `json:"language"`
	IncludeFileIDs []string `json:"include_file_ids"`
	LLMModelID     string   `json:"llm_model_id"`
}

func (h *FlashcardGenHandler) bindCreateRequest(c *gin.Context) (*types.FlashcardGenerateRequest, string, error) {
	kbID := secutils.SanitizeForLog(c.Param("id"))
	if kbID == "" {
		return nil, "", errors.NewBadRequestError("知识库 ID 不能为空")
	}
	var req flashcardCreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return nil, kbID, errors.NewBadRequestError("请求体 JSON 无效").WithDetails(err.Error())
	}
	if strings.TrimSpace(req.Topic) == "" {
		return nil, kbID, errors.NewBadRequestError("请填写主题 topic")
	}
	return &types.FlashcardGenerateRequest{
		Topic:          strings.TrimSpace(req.Topic),
		Count:          req.Count,
		Language:       req.Language,
		IncludeFileIDs: req.IncludeFileIDs,
		LLMModelID:     req.LLMModelID,
	}, kbID, nil
}

// GenerateFlashcards 从知识库内容生成闪卡（同步，兼容旧前端）。
func (h *FlashcardGenHandler) GenerateFlashcards(c *gin.Context) {
	ctx := c.Request.Context()
	req, kbID, err := h.bindCreateRequest(c)
	if err != nil {
		c.Error(err)
		return
	}
	logger.Infof(ctx, "[flashcard] HTTP POST generate (sync) kb=%s topic=%q", kbID, req.Topic)

	res, err := h.svc.GenerateFromKnowledgeBase(ctx, kbID, req)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
}

// CreateFlashcardGenTask 创建闪卡生成任务（异步，立即返回 202）。
func (h *FlashcardGenHandler) CreateFlashcardGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	req, kbID, err := h.bindCreateRequest(c)
	if err != nil {
		c.Error(err)
		return
	}
	logger.Infof(ctx, "[flashcard] HTTP POST task kb=%s topic=%q count=%d", kbID, req.Topic, req.Count)

	task, err := h.svc.CreateTask(ctx, kbID, req)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": task})
}

// GetFlashcardGenTask 查询单个闪卡任务（含闪卡内容）。
func (h *FlashcardGenHandler) GetFlashcardGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("任务 ID 不能为空"))
		return
	}
	task, err := h.svc.GetTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

// ListFlashcardGenTasks 列出某知识库的最近闪卡任务。
func (h *FlashcardGenHandler) ListFlashcardGenTasks(c *gin.Context) {
	ctx := c.Request.Context()
	kbID := secutils.SanitizeForLog(c.Param("id"))
	if kbID == "" {
		c.Error(errors.NewBadRequestError("知识库 ID 不能为空"))
		return
	}
	limitStr := c.DefaultQuery("limit", "20")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 20
	}
	tasks, err := h.svc.ListTasksForKB(ctx, kbID, limit)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": tasks})
}

// PurgeFlashcardGenTask 永久删除任务记录（仅终态）。
func (h *FlashcardGenHandler) PurgeFlashcardGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("任务 ID 不能为空"))
		return
	}
	if err := h.svc.PurgeTask(ctx, taskID); err != nil {
		c.Error(err)
		return
	}
	c.Status(http.StatusNoContent)
}

// FlashcardHealth 闪卡 Bridge 健康检查。
func (h *FlashcardGenHandler) FlashcardHealth(c *gin.Context) {
	ctx := c.Request.Context()
	if err := h.svc.HealthCheck(ctx); err != nil {
		c.Error(errors.NewInternalServerError("闪卡 Bridge 不可用").WithDetails(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"ok": true}})
}
