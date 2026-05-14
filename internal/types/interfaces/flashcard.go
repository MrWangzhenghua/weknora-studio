package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// FlashcardGenService 从知识库调用闪卡 Bridge 生成闪卡。
type FlashcardGenService interface {
	GenerateFromKnowledgeBase(ctx context.Context, kbID string, req *types.FlashcardGenerateRequest) (*types.FlashcardGenerateResult, error)
	HealthCheck(ctx context.Context) error
}
