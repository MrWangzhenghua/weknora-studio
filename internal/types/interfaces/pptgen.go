package interfaces

import (
	"context"
	"io"

	"github.com/Tencent/WeKnora/internal/types"
)

// PPTGenService 提供"从知识库生成 PPT"的业务能力。
// 实现负责：
//  1. 收集知识库内可用的文件内容；
//  2. 调用 PPT Bridge 创建生成任务；
//  3. 缓存本地任务状态供前端查询；
//  4. 当任务完成后从 Bridge 下载结果文件，再回传给前端。
type PPTGenService interface {
	// CreateTask 基于知识库内全部文件创建一个 PPT 生成任务。
	CreateTask(ctx context.Context, kbID string, req *types.PPTGenCreateRequest) (*types.PPTGenTask, error)

	// GetTask 查询任务状态。会主动向 Bridge 拉取最新进度。
	GetTask(ctx context.Context, taskID string) (*types.PPTGenTask, error)

	// CancelTask 取消任务（若尚未完成）。
	CancelTask(ctx context.Context, taskID string) (*types.PPTGenTask, error)

	// DownloadResult 下载已完成任务的 PPT 文件，返回流与文件名。
	DownloadResult(ctx context.Context, taskID string) (io.ReadCloser, string, error)

	// ListTasksForKB 列出某个知识库的最近任务（按时间倒序）。
	ListTasksForKB(ctx context.Context, kbID string, limit int) ([]*types.PPTGenTask, error)

	// HealthCheck 探测 Bridge 是否就绪。
	HealthCheck(ctx context.Context) error

	// TestModelConnection 使用 Bridge 探测所选全局模型对应的 OpenAI 兼容接口是否可用。
	TestModelConnection(ctx context.Context, req *types.PPTGenModelTestRequest) (*types.PPTGenModelTestResponse, error)

	// PurgeTask 永久删除本地任务记录并请求 Bridge 删除结果文件（仅终态任务可删）。
	PurgeTask(ctx context.Context, taskID string) error

	// PreviewResult 返回已完成任务的 PDF 预览流（由 Bridge 将 pptx 转为 pdf）。
	PreviewResult(ctx context.Context, taskID string) (io.ReadCloser, error)
}
