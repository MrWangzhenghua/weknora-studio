package handler

import (
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// PPTGenHandler 处理 "从知识库生成 PPT" 相关的 HTTP 请求。
type PPTGenHandler struct {
	pptSvc interfaces.PPTGenService
	kbSvc  interfaces.KnowledgeBaseService
}

// NewPPTGenHandler DI 构造函数。
func NewPPTGenHandler(pptSvc interfaces.PPTGenService, kbSvc interfaces.KnowledgeBaseService) *PPTGenHandler {
	return &PPTGenHandler{pptSvc: pptSvc, kbSvc: kbSvc}
}

// createTaskRequest 是创建任务接口的请求体。
// 路径上的 kb_id 通过 URL 参数 :id 传入。
type createTaskRequest struct {
	Instruction    string   `json:"instruction"`
	NumPages       int      `json:"num_pages"`
	Language       string   `json:"language"`
	Template       string   `json:"template"`
	Title          string   `json:"title"`
	IncludeFileIDs []string `json:"include_file_ids"`
	LLMModelID     string   `json:"llm_model_id"`
	VLMModelID     string   `json:"vlm_model_id"`
}

// CreatePPTGenTask godoc
// @Summary      触发 PPT 生成任务
// @Description  使用知识库内的所有文件作为输入，调用 PPTAgent Bridge 生成 PPT。
// @Tags         PPT 生成
// @Accept       json
// @Produce      json
// @Param        id    path      string                true  "知识库 ID"
// @Param        body  body      createTaskRequest     false "生成参数"
// @Success      202   {object}  map[string]interface{} "任务已受理"
// @Failure      400   {object}  errors.AppError        "参数错误"
// @Failure      404   {object}  errors.AppError        "知识库不存在"
// @Failure      500   {object}  errors.AppError        "内部错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/ppt-tasks [post]
func (h *PPTGenHandler) CreatePPTGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	kbID := secutils.SanitizeForLog(c.Param("id"))
	if kbID == "" {
		c.Error(errors.NewBadRequestError("knowledge base id cannot be empty"))
		return
	}
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// JSON 解析失败时容忍空 body（生成参数全部可选）
		req = createTaskRequest{}
	}
	logger.Infof(ctx, "[pptgen] create task: kb=%s pages=%d include=%d", kbID, req.NumPages, len(req.IncludeFileIDs))

	task, err := h.pptSvc.CreateTask(ctx, kbID, &types.PPTGenCreateRequest{
		Instruction:    req.Instruction,
		NumPages:       req.NumPages,
		Language:       req.Language,
		Template:       req.Template,
		Title:          req.Title,
		IncludeFileIDs: req.IncludeFileIDs,
		LLMModelID:     req.LLMModelID,
		VLMModelID:     req.VLMModelID,
	})
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"data":    task,
	})
}

// GetPPTGenTask godoc
// @Summary      查询 PPT 生成任务状态
// @Tags         PPT 生成
// @Produce      json
// @Param        task_id  path      string  true  "任务 ID"
// @Success      200      {object}  map[string]interface{}  "任务详情"
// @Failure      404      {object}  errors.AppError         "任务不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /ppt-tasks/{task_id} [get]
func (h *PPTGenHandler) GetPPTGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("task id cannot be empty"))
		return
	}
	task, err := h.pptSvc.GetTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

// CancelPPTGenTask godoc
// @Summary      取消 PPT 生成任务
// @Tags         PPT 生成
// @Produce      json
// @Param        task_id  path      string  true  "任务 ID"
// @Success      200      {object}  map[string]interface{}  "任务详情"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /ppt-tasks/{task_id} [delete]
func (h *PPTGenHandler) CancelPPTGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("task id cannot be empty"))
		return
	}
	task, err := h.pptSvc.CancelTask(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

// DownloadPPTGenResult godoc
// @Summary      下载生成的 PPT 文件
// @Tags         PPT 生成
// @Produce      application/octet-stream
// @Param        task_id  path      string  true  "任务 ID"
// @Success      200      {file}    file    "PPT 文件"
// @Failure      400      {object}  errors.AppError  "任务未完成"
// @Failure      404      {object}  errors.AppError  "任务不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /ppt-tasks/{task_id}/download [get]
func (h *PPTGenHandler) DownloadPPTGenResult(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("task id cannot be empty"))
		return
	}
	stream, filename, err := h.pptSvc.DownloadResult(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	defer stream.Close()

	cd := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	c.Header("Content-Disposition", cd)
	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
	c.Header("Cache-Control", "no-store")
	c.Stream(func(w io.Writer) bool {
		if _, err := io.Copy(w, stream); err != nil {
			logger.Errorf(ctx, "[pptgen] download stream error: %v", err)
			return false
		}
		return false
	})
}

// pptModelTestRequest 与 types.PPTGenModelTestRequest 对齐。
type pptModelTestRequest struct {
	LLMModelID string `json:"llm_model_id"`
	VLMModelID string `json:"vlm_model_id"`
}

// TestPPTGenModels 测试所选全局模型与 OpenAI 兼容接口的连通性（经 Bridge 转发）。
func (h *PPTGenHandler) TestPPTGenModels(c *gin.Context) {
	ctx := c.Request.Context()
	var req pptModelTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewBadRequestError("invalid json body").WithDetails(err.Error()))
		return
	}
	res, err := h.pptSvc.TestModelConnection(ctx, &types.PPTGenModelTestRequest{
		LLMModelID: req.LLMModelID,
		VLMModelID: req.VLMModelID,
	})
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
}

// PurgePPTGenTask 永久删除任务及 Bridge 端生成的 PPT 文件。
func (h *PPTGenHandler) PurgePPTGenTask(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("task id cannot be empty"))
		return
	}
	if err := h.pptSvc.PurgeTask(ctx, taskID); err != nil {
		c.Error(err)
		return
	}
	c.Status(http.StatusNoContent)
}

// PreviewPPTGenResult 返回 PDF 预览流，供浏览器 iframe / object 展示。
func (h *PPTGenHandler) PreviewPPTGenResult(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := secutils.SanitizeForLog(c.Param("task_id"))
	if taskID == "" {
		c.Error(errors.NewBadRequestError("task id cannot be empty"))
		return
	}
	stream, err := h.pptSvc.PreviewResult(ctx, taskID)
	if err != nil {
		c.Error(err)
		return
	}
	defer stream.Close()
	c.Header("Content-Type", "application/pdf")
	c.Header("Cache-Control", "no-store")
	c.Stream(func(w io.Writer) bool {
		if _, err := io.Copy(w, stream); err != nil {
			logger.Errorf(ctx, "[pptgen] preview stream error: %v", err)
			return false
		}
		return false
	})
}

// ListPPTGenTasks godoc
// @Summary      列出某知识库的最近 PPT 任务
// @Tags         PPT 生成
// @Produce      json
// @Param        id     path      string  true  "知识库 ID"
// @Param        limit  query     int     false "返回条数，默认 20"
// @Success      200    {object}  map[string]interface{}  "任务列表"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/ppt-tasks [get]
func (h *PPTGenHandler) ListPPTGenTasks(c *gin.Context) {
	ctx := c.Request.Context()
	kbID := secutils.SanitizeForLog(c.Param("id"))
	if kbID == "" {
		c.Error(errors.NewBadRequestError("knowledge base id cannot be empty"))
		return
	}
	limitStr := c.DefaultQuery("limit", "20")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 20
	}
	tasks, err := h.pptSvc.ListTasksForKB(ctx, kbID, limit)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": tasks})
}

// PPTGenHealth godoc
// @Summary      PPTAgent Bridge 健康检查
// @Tags         PPT 生成
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      503  {object}  errors.AppError
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /ppt-tasks/health [get]
func (h *PPTGenHandler) PPTGenHealth(c *gin.Context) {
	ctx := c.Request.Context()
	if err := h.pptSvc.HealthCheck(ctx); err != nil {
		c.Error(errors.NewInternalServerError("pptagent bridge unhealthy").WithDetails(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"status": "ok"}})
}
