package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// PPTAgentBridgeClient 是 WeKnora 调用 PPTAgent Bridge 的 HTTP 客户端。
//
// 选择 HTTP/JSON 而非 gRPC 的理由：
//  1. PPT 生成是低 QPS、高耗时的任务，HTTP 异步轮询模式开发成本最低；
//  2. Bridge 端使用 Python 实现，FastAPI 自带 OpenAPI/Swagger 文档，便于联调；
//  3. 上传文件场景多 part 数量不固定，REST + multipart 比 protobuf 更适合。
//
// 鉴权：通过 Bearer Token（共享密钥）实现服务间认证。Token 通过环境变量
// PPTAGENT_BRIDGE_TOKEN 注入；生产环境建议放在 secret 管理系统中。
type PPTAgentBridgeClient struct {
	baseURL     string
	token       string
	httpClient  *http.Client
	maxFileSize int64
}

// NewPPTAgentBridgeClient 构造客户端。配置来源：
//   - PPTAGENT_BRIDGE_URL    （默认 http://pptagent:8090）
//   - PPTAGENT_BRIDGE_TOKEN  （默认空，关闭鉴权，仅推荐内网部署）
//   - PPTAGENT_BRIDGE_TIMEOUT_SEC （默认 60，单次普通请求超时）
func NewPPTAgentBridgeClient() *PPTAgentBridgeClient {
	baseURL := strings.TrimRight(getenvDefault("PPTAGENT_BRIDGE_URL", "http://pptagent:8090"), "/")
	token := os.Getenv("PPTAGENT_BRIDGE_TOKEN")
	timeoutSec := getenvIntDefault("PPTAGENT_BRIDGE_TIMEOUT_SEC", 60)
	maxFile := int64(getenvIntDefault("PPTAGENT_BRIDGE_MAX_FILE_MB", 200)) * 1024 * 1024
	return &PPTAgentBridgeClient{
		baseURL:     baseURL,
		token:       token,
		httpClient:  &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
		maxFileSize: maxFile,
	}
}

// BridgeFile 描述上传给 Bridge 的单个文件。
// Reader 在请求构造期间被消费一次后关闭；调用方无需再 Close。
type BridgeFile struct {
	Name        string
	ContentType string
	Reader      io.ReadCloser
}

// BridgeCreateMeta 与 Bridge 的 GenerateRequestMetadata 对应。
type BridgeCreateMeta struct {
	Instruction       string                 `json:"instruction"`
	NumPages          *int                   `json:"num_pages,omitempty"`
	Language          string                 `json:"language,omitempty"`
	Template          string                 `json:"template,omitempty"`
	Title             string                 `json:"title,omitempty"`
	WeKnoraTenantID   uint64                 `json:"weknora_tenant_id,omitempty"`
	WeKnoraKBID       string                 `json:"weknora_kb_id,omitempty"`
	WeKnoraUserID     string                 `json:"weknora_user_id,omitempty"`
	Extra             map[string]interface{} `json:"extra,omitempty"`
	// 单次任务覆盖的 LLM / VLM（由 WeKnora 从租户模型表解析后下发，与 Bridge GenerateRequestMetadata 对齐）
	PptLLMBaseURL string `json:"ppt_llm_base_url,omitempty"`
	PptLLMModel   string `json:"ppt_llm_model,omitempty"`
	PptLLMAPIKey  string `json:"ppt_llm_api_key,omitempty"`
	PptLLMTimeout *int   `json:"ppt_llm_timeout,omitempty"`
	PptVLMBaseURL string `json:"ppt_vlm_base_url,omitempty"`
	PptVLMModel   string `json:"ppt_vlm_model,omitempty"`
	PptVLMAPIKey  string `json:"ppt_vlm_api_key,omitempty"`
	PptVLMTimeout *int   `json:"ppt_vlm_timeout,omitempty"`
}

// BridgeTaskInfo 与 Bridge 返回的 TaskInfoResponse 对应。
type BridgeTaskInfo struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	InputFiles int    `json:"input_files"`
	InputBytes int64  `json:"input_bytes"`
	HasResult  bool   `json:"has_result"`
}

// BridgeCreateResponse 来自 ``POST /v1/generate``。
type BridgeCreateResponse struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

// CreateTask 上传文件并触发 Bridge 生成任务。
//
// 上传过程中文件 Reader 会被流式读取，每个 file 在写完后即被关闭。
// 调用方在外层不需要再次关闭。
func (c *PPTAgentBridgeClient) CreateTask(
	ctx context.Context,
	meta *BridgeCreateMeta,
	files []BridgeFile,
) (*BridgeCreateResponse, error) {
	// 通过 io.Pipe + multipart.NewWriter 实现"边读边发"，避免大文件全部驻留内存。
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		var firstErr error
		setErr := func(err error) {
			if firstErr == nil {
				firstErr = err
			}
		}

		// 1) meta 字段：以 JSON 字符串写入 form-data。
		metaBytes, err := json.Marshal(meta)
		if err != nil {
			setErr(err)
		} else if err := writer.WriteField("meta", string(metaBytes)); err != nil {
			setErr(err)
		}

		// 2) 逐个写入文件。
		for _, f := range files {
			if firstErr != nil {
				break
			}
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, f.Name))
			if f.ContentType != "" {
				h.Set("Content-Type", f.ContentType)
			}
			fw, werr := writer.CreatePart(h)
			if werr != nil {
				setErr(werr)
				break
			}
			if _, cerr := io.Copy(fw, f.Reader); cerr != nil {
				setErr(cerr)
			}
			_ = f.Reader.Close()
		}

		// 3) close writer 写入结尾边界。
		if cerr := writer.Close(); cerr != nil {
			setErr(cerr)
		}
		_ = pw.CloseWithError(firstErr)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/generate", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call bridge create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bridge create returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	var out BridgeCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode bridge create response: %w", err)
	}
	logger.Infof(ctx, "[pptagent] bridge task created: %s", out.TaskID)
	return &out, nil
}

// BridgeLLMTestRequest 与 Bridge ``POST /v1/test-llm`` 请求体一致。
type BridgeLLMTestRequest struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	Timeout int    `json:"timeout"`
}

// BridgeLLMTestResponse 与 Bridge 测试接口响应一致。
type BridgeLLMTestResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TestLLM 调用 Bridge 探测 OpenAI 兼容 Chat 接口。
func (c *PPTAgentBridgeClient) TestLLM(ctx context.Context, body *BridgeLLMTestRequest) (*BridgeLLMTestResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/test-llm", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call bridge test-llm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bridge test-llm returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	var out BridgeLLMTestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PurgeTask 永久删除 Bridge 端任务及磁盘文件（仅终态）。
func (c *PPTAgentBridgeClient) PurgeTask(ctx context.Context, bridgeTaskID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/v1/tasks/"+url.PathEscape(bridgeTaskID)+"/permanent", nil)
	if err != nil {
		return err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call bridge purge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrPPTAgentTaskNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("bridge purge conflict: task still running")
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge purge returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	return nil
}

// PreviewResult 拉取 Bridge 生成的 PDF 预览流。
func (c *PPTAgentBridgeClient) PreviewResult(ctx context.Context, bridgeTaskID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/tasks/"+url.PathEscape(bridgeTaskID)+"/preview", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	// 转换可能较慢，使用较长超时
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call bridge preview: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrPPTAgentTaskNotFound
	}
	if resp.StatusCode >= 400 {
		body := readBodySnippet(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("bridge preview returned %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// GetTask 查询单个任务状态。
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

// CancelTask 取消任务。
func (c *PPTAgentBridgeClient) CancelTask(ctx context.Context, taskID string) (*BridgeTaskInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/tasks/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call bridge cancel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPPTAgentTaskNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bridge cancel returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	var out BridgeTaskInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DownloadResult 下载任务结果，返回 PPT 二进制流与建议文件名。
// 调用方在使用完毕后必须关闭返回的 ReadCloser。
//
// 注意：此方法使用独立的 http.Client（无超时）以支持大文件下载；调用方应通过
// ctx 控制取消。
func (c *PPTAgentBridgeClient) DownloadResult(ctx context.Context, taskID string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/tasks/"+url.PathEscape(taskID)+"/file", nil)
	if err != nil {
		return nil, "", err
	}
	c.applyAuth(req)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("call bridge download: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, "", ErrPPTAgentTaskNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		return nil, "", ErrPPTAgentResultNotReady
	}
	if resp.StatusCode >= 400 {
		body := readBodySnippet(resp.Body)
		resp.Body.Close()
		return nil, "", fmt.Errorf("bridge download returned %d: %s", resp.StatusCode, body)
	}
	filename := parseAttachmentFilename(resp.Header.Get("Content-Disposition"))
	if filename == "" {
		filename = taskID + ".pptx"
	}
	return resp.Body, filename, nil
}

// Health 探测 Bridge 是否就绪。
func (c *PPTAgentBridgeClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bridge health %d", resp.StatusCode)
	}
	return nil
}

// MaxFileSize 单文件上限（字节）。0 表示不限制。
func (c *PPTAgentBridgeClient) MaxFileSize() int64 {
	return c.maxFileSize
}

// BaseURL 返回当前指向的 Bridge 地址，便于诊断输出。
func (c *PPTAgentBridgeClient) BaseURL() string {
	return c.baseURL
}

// ============== helpers ==============

// ErrPPTAgentTaskNotFound 表示 Bridge 中找不到任务（一般是过期被 GC）。
var ErrPPTAgentTaskNotFound = errors.New("pptagent bridge task not found")

// ErrPPTAgentResultNotReady 表示任务尚未完成，结果文件不可下载。
var ErrPPTAgentResultNotReady = errors.New("pptagent bridge result not ready")

func (c *PPTAgentBridgeClient) applyAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func readBodySnippet(r io.Reader) string {
	buf := &bytes.Buffer{}
	_, _ = io.CopyN(buf, r, 2048)
	return strings.TrimSpace(buf.String())
}

func parseAttachmentFilename(header string) string {
	if header == "" {
		return ""
	}
	// 简化解析：寻找 filename= 或 filename*=
	parts := strings.Split(header, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "filename=") {
			val := strings.TrimPrefix(p, "filename=")
			val = strings.Trim(val, `"`)
			if val != "" {
				return val
			}
		}
		if strings.HasPrefix(p, "filename*=") {
			val := strings.TrimPrefix(p, "filename*=")
			if idx := strings.Index(val, "''"); idx >= 0 {
				val = val[idx+2:]
			}
			if decoded, err := url.QueryUnescape(val); err == nil && decoded != "" {
				return decoded
			}
			if val != "" {
				return val
			}
		}
	}
	return ""
}

func getenvDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func getenvIntDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}
