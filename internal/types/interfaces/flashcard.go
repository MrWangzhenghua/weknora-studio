package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// FlashcardGenService 从知识库调用闪卡 Bridge 生成闪卡，并维护任务历史。
type FlashcardGenService interface {
	// GenerateFromKnowledgeBase 同步生成（兼容旧接口）。
	GenerateFromKnowledgeBase(ctx context.Context, kbID string, req *types.FlashcardGenerateRequest) (*types.FlashcardGenerateResult, error)

	CreateTask(ctx context.Context, kbID string, req *types.FlashcardGenerateRequest) (*types.FlashcardGenTask, error)
	GetTask(ctx context.Context, taskID string) (*types.FlashcardGenTask, error)
	ListTasksForKB(ctx context.Context, kbID string, limit int) ([]*types.FlashcardGenTask, error)
	PurgeTask(ctx context.Context, taskID string) error

	HealthCheck(ctx context.Context) error
}
