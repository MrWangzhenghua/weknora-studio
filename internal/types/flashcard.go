package types

import "time"

// FlashcardGenerateRequest 从知识库生成闪卡的请求体。
type FlashcardGenerateRequest struct {
	Topic          string   `json:"topic"`
	Count          int      `json:"count"`
	Language       string   `json:"language"`
	IncludeFileIDs []string `json:"include_file_ids"`
	LLMModelID     string   `json:"llm_model_id"`
}

// FlashcardItem 单张闪卡。
type FlashcardItem struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

// FlashcardGenerateResult 生成结果（透传 Bridge 响应）。
type FlashcardGenerateResult struct {
	Topic      string                 `json:"topic"`
	Flashcards []FlashcardItem        `json:"flashcards"`
	Citations  map[string]interface{} `json:"citations,omitempty"`
	Message    string                 `json:"message,omitempty"`
}

// FlashcardGenTask 表示一次「从知识库生成闪卡」的任务（WeKnora 侧记录，含生成结果）。
type FlashcardGenTask struct {
	TaskID          string `json:"task_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	TenantID        uint64 `json:"tenant_id"`
	UserID          string `json:"user_id"`
	Topic           string `json:"topic"`
	Count           int    `json:"count"`
	Language        string `json:"language"`
	LLMModelID      string `json:"llm_model_id,omitempty"`

	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Error    string `json:"error"`

	Flashcards []FlashcardItem        `json:"flashcards,omitempty"`
	Citations  map[string]interface{} `json:"citations,omitempty"`

	FilesIncluded int `json:"files_included"`
	FilesSkipped  int `json:"files_skipped"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// 闪卡任务状态（与前端 TERMINAL 集合对齐）。
const (
	FlashcardStatusPending    = "pending"
	FlashcardStatusGenerating = "generating"
	FlashcardStatusSucceeded  = "succeeded"
	FlashcardStatusFailed     = "failed"
)
