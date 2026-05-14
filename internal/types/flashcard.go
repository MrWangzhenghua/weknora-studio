package types

// FlashcardGenerateRequest 从知识库生成闪卡的请求体。
type FlashcardGenerateRequest struct {
	Topic            string   `json:"topic"`
	Count            int      `json:"count"`
	Language         string   `json:"language"`
	IncludeFileIDs   []string `json:"include_file_ids"`
	LLMModelID       string   `json:"llm_model_id"`
}

// FlashcardItem 单张闪卡。
type FlashcardItem struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

// FlashcardGenerateResult 生成结果（透传 Bridge 响应）。
type FlashcardGenerateResult struct {
	Topic       string                 `json:"topic"`
	Flashcards  []FlashcardItem        `json:"flashcards"`
	Citations   map[string]interface{} `json:"citations,omitempty"`
	Message     string                 `json:"message,omitempty"`
}
