package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// FlashcardBridgeClient 调用闪卡 Bridge（multipart 与鉴权方式对齐 PPTAgentBridgeClient）。
type FlashcardBridgeClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewFlashcardBridgeClient 从环境变量构造客户端。
//   - FLASHCARD_BRIDGE_URL           默认 http://flashcard:8091
//   - FLASHCARD_BRIDGE_TOKEN         与 Bridge BRIDGE_API_TOKEN 一致
//   - FLASHCARD_BRIDGE_TIMEOUT_SEC   默认 180
func NewFlashcardBridgeClient() *FlashcardBridgeClient {
	baseURL := strings.TrimRight(getenvDefault("FLASHCARD_BRIDGE_URL", "http://flashcard:8091"), "/")
	token := os.Getenv("FLASHCARD_BRIDGE_TOKEN")
	timeoutSec := getenvIntDefault("FLASHCARD_BRIDGE_TIMEOUT_SEC", 180)
	return &FlashcardBridgeClient{
		baseURL:    baseURL,
		token:      token,
		httpClient: &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
}

func (c *FlashcardBridgeClient) applyFlashAuth(req *http.Request) {
	if strings.TrimSpace(c.token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.token))
	}
}

// FlashcardBridgeMeta 与 Python GenerateFlashcardMeta 对齐。
type FlashcardBridgeMeta struct {
	Topic           string `json:"topic"`
	Count           int    `json:"count"`
	Language        string `json:"language,omitempty"`
	WeKnoraTenantID uint64 `json:"weknora_tenant_id,omitempty"`
	WeKnoraKBID     string `json:"weknora_kb_id,omitempty"`
	WeKnoraUserID   string `json:"weknora_user_id,omitempty"`
	FlashLLMBaseURL string `json:"flash_llm_base_url,omitempty"`
	FlashLLMModel   string `json:"flash_llm_model,omitempty"`
	FlashLLMAPIKey  string `json:"flash_llm_api_key,omitempty"`
	FlashLLMTimeout *int   `json:"flash_llm_timeout,omitempty"`
}

// FlashcardBridgeResponse 与 Bridge JSON 对齐。
type FlashcardBridgeResponse struct {
	Topic       string                   `json:"topic"`
	Flashcards  []FlashcardBridgeItem    `json:"flashcards"`
	Citations   map[string]interface{}   `json:"citations"`
	Message     string                   `json:"message"`
}

// FlashcardBridgeItem 单张闪卡。
type FlashcardBridgeItem struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

// Generate 上传文件并触发闪卡生成（流式 multipart，与 PPT CreateTask 相同模式）。
func (c *FlashcardBridgeClient) Generate(ctx context.Context, meta *FlashcardBridgeMeta, files []BridgeFile) (*FlashcardBridgeResponse, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		var firstErr error
		setErr := func(err error) {
			if firstErr == nil {
				firstErr = err
			}
		}

		metaBytes, err := json.Marshal(meta)
		if err != nil {
			setErr(err)
		} else if err := writer.WriteField("meta", string(metaBytes)); err != nil {
			setErr(err)
		}

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

		if cerr := writer.Close(); cerr != nil {
			setErr(cerr)
		}
		_ = pw.CloseWithError(firstErr)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/generate", pr)
	if err != nil {
		closeBridgeFiles(files)
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.applyFlashAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("flashcard bridge generate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("flashcard bridge returned %d: %s", resp.StatusCode, readBodySnippet(resp.Body))
	}
	var out FlashcardBridgeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode flashcard response: %w", err)
	}
	logger.Infof(ctx, "[flashcard] bridge ok topic=%s cards=%d", out.Topic, len(out.Flashcards))
	return &out, nil
}

// Health 探测 Bridge /health。
func (c *FlashcardBridgeClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	c.applyFlashAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("flashcard health status %d", resp.StatusCode)
	}
	return nil
}
