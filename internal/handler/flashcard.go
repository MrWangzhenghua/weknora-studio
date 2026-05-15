package handler

import (
	"net/http"
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

// GenerateFlashcards 从知识库内容生成闪卡（同步返回 JSON）。
func (h *FlashcardGenHandler) GenerateFlashcards(c *gin.Context) {
	ctx := c.Request.Context()
	kbID := secutils.SanitizeForLog(c.Param("id"))
	if kbID == "" {
		c.Error(errors.NewBadRequestError("知识库 ID 不能为空"))
		return
	}
	var req types.FlashcardGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewBadRequestError("请求体 JSON 无效").WithDetails(err.Error()))
		return
	}
	if strings.TrimSpace(req.Topic) == "" {
		c.Error(errors.NewBadRequestError("请填写主题 topic"))
		return
	}
	logger.Infof(ctx, "[flashcard] HTTP POST generate kb=%s topic=%q count=%d path=%s",
		kbID, req.Topic, req.Count, c.FullPath())

	res, err := h.svc.GenerateFromKnowledgeBase(ctx, kbID, &req)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
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
