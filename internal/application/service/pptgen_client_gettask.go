package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// GetTask 查询单个任务状态（独立文件避免合并时弄丢 func 声明导致 Docker 编译失败）。
func (c *PPTAgentBridgeClient) GetTask(ctx context.Context, taskID string) (*BridgeTaskInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/tasks/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call bridge get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPPTAgentTaskNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bridge get returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	var out BridgeTaskInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
