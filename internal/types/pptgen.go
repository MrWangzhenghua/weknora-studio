// Package types：PPT 生成功能相关的领域类型定义。
package types

import "time"

// PPTGenTask 表示一次"从知识库生成 PPT"的任务。
// 任务由 WeKnora 后端创建并跟踪，实际的 PPT 生成委托给 PPT Bridge（ppt-master 导出栈）完成。
type PPTGenTask struct {
	// TaskID 是 WeKnora 端的本地任务 ID，使用 UUID。
	TaskID string `json:"task_id"`
	// BridgeTaskID 是 Bridge 返回的远端任务 ID。
	BridgeTaskID string `json:"bridge_task_id"`
	// KnowledgeBaseID 关联的知识库。
	KnowledgeBaseID string `json:"knowledge_base_id"`
	// TenantID 租户。
	TenantID uint64 `json:"tenant_id"`
	// UserID 发起任务的用户。
	UserID string `json:"user_id"`
	// Title PPT 标题（默认为知识库名）。
	Title string `json:"title"`
	// Instruction 用户提示词（可为空）。
	Instruction string `json:"instruction"`
	// NumPages 期望幻灯片张数（0 表示由 LLM 自动决定）。
	NumPages int `json:"num_pages"`
	// Template 历史字段；当前 PPT Master Bridge 路径下可忽略。
	Template string `json:"template"`

	// Status 任务状态（与 Bridge 同步刷新）。
	Status string `json:"status"`
	// Progress 进度 0-100。
	Progress int `json:"progress"`
	// Message 当前状态描述。
	Message string `json:"message"`
	// Error 出错时的错误信息。
	Error string `json:"error"`

	// FilesIncluded 任务实际包含的文件数量。
	FilesIncluded int `json:"files_included"`
	// FilesSkipped 因不支持/过大被跳过的文件数。
	FilesSkipped int `json:"files_skipped"`

	// CreatedAt / UpdatedAt 时间戳。
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// CompletedAt 完成时间，仅成功或失败终态写入。
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// PPTGen 任务状态常量。与 Bridge 保持一致，以便直接透传。
const (
	PPTGenStatusPending    = "pending"
	PPTGenStatusExtracting = "extracting"
	PPTGenStatusDrafting   = "drafting"
	PPTGenStatusRendering  = "rendering"
	PPTGenStatusSucceeded  = "succeeded"
	PPTGenStatusFailed     = "failed"
	PPTGenStatusCancelled  = "cancelled"
)

// PPTGenCreateRequest 创建任务的请求体。
type PPTGenCreateRequest struct {
	// Instruction 自定义生成提示词（可选）。
	Instruction string `json:"instruction"`
	// NumPages 期望张数；<=0 时由模型自动决定。
	NumPages int `json:"num_pages"`
	// Language zh/en/auto。
	Language string `json:"language"`
	// Template 模板名（可选）。
	Template string `json:"template"`
	// Title 自定义标题（可选，默认知识库名）。
	Title string `json:"title"`
	// IncludeFileIDs 仅包含指定知识 ID（可选，空数组表示包含全部）。
	IncludeFileIDs []string `json:"include_file_ids"`
	// LLMModelID 全局设置中的对话模型 ID（可选）；若填写则本任务使用该模型的 base_url/api_key，不再使用容器环境变量中的 PPTMASTER_LLM_*。
	LLMModelID string `json:"llm_model_id,omitempty"`
	// VLMModelID 全局设置中的视觉模型 ID（可选）；不填则沿用 Bridge 默认 VLM 或未配置时回退到 LLM。
	VLMModelID string `json:"vlm_model_id,omitempty"`
}

// PPTGenModelTestRequest 测试 PPT 所用模型连接（转发至 Bridge 的 OpenAI 兼容探测）。
type PPTGenModelTestRequest struct {
	LLMModelID string `json:"llm_model_id"`
	VLMModelID string `json:"vlm_model_id"`
}

// PPTGenModelTestResponse 各模型探测结果（未测试的字段省略）。
type PPTGenModelTestResponse struct {
	LLMOk      *bool  `json:"llm_ok,omitempty"`
	LLMMessage string `json:"llm_message,omitempty"`
	VLMOk      *bool  `json:"vlm_ok,omitempty"`
	VLMMessage string `json:"vlm_message,omitempty"`
}
