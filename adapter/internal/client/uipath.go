package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"uipath-adapter/internal/debug"
)

// proxyTransports caches one *http.Transport per proxy URL so we don't rebuild
// dialer/TLS state on every request.
var proxyTransports sync.Map // string -> *http.Transport

// transportForProxy returns a cached transport routing through proxyURL.
// Empty/invalid proxyURL falls back to http.DefaultTransport.
func transportForProxy(proxyURL string) http.RoundTripper {
	if proxyURL == "" {
		return http.DefaultTransport
	}
	if v, ok := proxyTransports.Load(proxyURL); ok {
		return v.(*http.Transport)
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return http.DefaultTransport
	}
	tr := &http.Transport{
		Proxy:                http.ProxyURL(u),
		MaxIdleConns:         100,
		MaxIdleConnsPerHost:  10,
		IdleConnTimeout:      90 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	actual, _ := proxyTransports.LoadOrStore(proxyURL, tr)
	return actual.(*http.Transport)
}

// UiPathClient UiPath API 客户端
type UiPathClient struct {
	baseURL     string
	accessToken string
	httpClient  *http.Client
}

// NewUiPathClient 创建 UiPath 客户端
// baseURL 应该是完整的 URL，例如：
// https://cloud.uipath.com/{org}/{tenant}/autopilotstudio_/autopilot-everywhere
func NewUiPathClient(baseURL, accessToken string) *UiPathClient {
	return &UiPathClient{
		baseURL:     baseURL,
		accessToken: accessToken,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

// SetProxy routes outbound requests through proxyURL (scheme://[user:pass@]host:port).
// Empty proxyURL leaves the default transport (no proxy). Uses a cached transport.
func (c *UiPathClient) SetProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	c.httpClient.Transport = transportForProxy(proxyURL)
}

// UiPathRequest UiPath 请求格式
type UiPathRequest struct {
	Agent            string                 `json:"agent"`
	Content          string                 `json:"content"`
	Context          map[string]interface{} `json:"context"`
	Resources        map[string]interface{} `json:"resources"`
	Tools            []interface{}          `json:"tools"`
	UnavailableTools []interface{}          `json:"unavailable_tools"`
	UserConfig       map[string]interface{} `json:"user_config"`
	Attachments      []UiPathAttachment     `json:"-"`
}

// UiPathAttachment follows the desktop handler's multipart shape:
// request.attachments_metadata plus one "files" part per attachment.
type UiPathAttachment struct {
	Name             string
	Type             string
	Preamble         string
	AttachmentSource string
	LocalPath        string
	Content          []byte
}

// UiPathStreamEvent UiPath 流式事件
type UiPathStreamEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// UiPathMessageData 消息数据
type UiPathMessageData struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Text    string `json:"text,omitempty"`
}

// UiPathUsageData Token 使用数据
type UiPathUsageData struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// SendMessage 发送消息到 UiPath（流式）
func (c *UiPathClient) SendMessage(ctx context.Context, sessionID string, req *UiPathRequest) (<-chan *UiPathStreamEvent, error) {
	// 构建 URL - 使用正确的端点
	url := fmt.Sprintf("%s/v1/chat/sessions/%s/messages", c.baseURL, sessionID)

	body, contentType, debugPayload, err := encodeUiPathRequest(req)
	if err != nil {
		return nil, err
	}

	// DEBUG: 打印实际发送的 JSON
	debug.Printf("   DEBUG request body: %s\n", debugPayload)

	// 创建 HTTP 请求
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// 设置 headers（对齐 Rust apply_uipath_headers）
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.accessToken))
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-uipath-consumingproductid", "9c87d968-918e-4162-b585-def44e8c69b1")
	httpReq.Header.Set("x-uipath-autopilotframework-version", "1.10.11559482")
	httpReq.Header.Set("cache-control", "no-cache")
	httpReq.Header.Set("pragma", "no-cache")
	httpReq.Header.Set("sec-fetch-dest", "empty")
	httpReq.Header.Set("sec-fetch-mode", "cors")
	httpReq.Header.Set("sec-fetch-site", "same-origin")
	httpReq.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	httpReq.Header.Set("accept-language", "en-US,en;q=0.9")
	httpReq.Header.Set("referer", c.baseURL+"/")

	// 发送请求
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("UiPath API error: %d - %s", resp.StatusCode, string(body))
	}

	// 创建事件通道
	eventChan := make(chan *UiPathStreamEvent, 10)

	// 启动 goroutine 处理流式响应
	go func() {
		defer close(eventChan)
		defer resp.Body.Close()

		// AutoPilot 单行 SSE data 可能很长（大 tool 结果/图片），默认 64KB 会静默截断
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		var currentEvent string
		var currentData []byte

		for scanner.Scan() {
			line := scanner.Text()

			if line == "" {
				// 空行表示事件结束
				if currentEvent != "" && len(currentData) > 0 {
					event := &UiPathStreamEvent{
						Event: currentEvent,
						Data:  currentData,
					}
					select {
					case eventChan <- event:
					case <-ctx.Done():
						return
					}
				}
				currentEvent = ""
				currentData = nil
				continue
			}

			if len(line) > 6 && line[:6] == "event:" {
				currentEvent = line[7:]
			} else if len(line) > 5 && line[:5] == "data:" {
				currentData = []byte(line[6:])
			}
		}

		if err := scanner.Err(); err != nil {
			// 读取流出错（截断/断连）时，向下游发一个 error 事件，避免静默中断
			select {
			case eventChan <- &UiPathStreamEvent{
				Event: "error",
				Data:  []byte(fmt.Sprintf("stream read error: %v", err)),
			}:
			case <-ctx.Done():
			}
		}
	}()

	return eventChan, nil
}

func encodeUiPathRequest(req *UiPathRequest) (*bytes.Buffer, string, string, error) {
	payload, err := uiPathRequestPayload(req)
	if err != nil {
		return nil, "", "", err
	}
	requestJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to marshal request: %w", err)
	}

	if len(req.Attachments) == 0 {
		return bytes.NewBuffer(requestJSON), "application/json", string(requestJSON), nil
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("request", string(requestJSON)); err != nil {
		return nil, "", "", fmt.Errorf("failed to write multipart request: %w", err)
	}
	for _, attachment := range req.Attachments {
		if len(attachment.Content) == 0 {
			continue
		}
		part, err := writer.CreatePart(attachmentPartHeader(attachment))
		if err != nil {
			return nil, "", "", fmt.Errorf("failed to create multipart file: %w", err)
		}
		if _, err := part.Write(attachment.Content); err != nil {
			return nil, "", "", fmt.Errorf("failed to write multipart file: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", "", fmt.Errorf("failed to close multipart request: %w", err)
	}
	return &body, writer.FormDataContentType(), string(requestJSON), nil
}

func uiPathRequestPayload(req *UiPathRequest) (map[string]interface{}, error) {
	payload := map[string]interface{}{
		"agent":             req.Agent,
		"content":           req.Content,
		"context":           req.Context,
		"resources":         req.Resources,
		"tools":             req.Tools,
		"unavailable_tools": req.UnavailableTools,
		"user_config":       req.UserConfig,
	}
	if payload["resources"] == nil {
		payload["resources"] = map[string]interface{}{}
	}
	if payload["tools"] == nil {
		payload["tools"] = []interface{}{}
	}
	if payload["unavailable_tools"] == nil {
		payload["unavailable_tools"] = []interface{}{}
	}
	if payload["user_config"] == nil {
		payload["user_config"] = map[string]interface{}{}
	}
	if len(req.Attachments) > 0 {
		metadata := make([]map[string]interface{}, 0, len(req.Attachments))
		for _, attachment := range req.Attachments {
			if len(attachment.Content) == 0 {
				continue
			}
			item := map[string]interface{}{
				"name":             attachment.Name,
				"type":             attachment.Type,
				"preamble":         attachment.Preamble,
				"attachmentSource": attachment.AttachmentSource,
			}
			if attachment.LocalPath != "" {
				item["localPath"] = attachment.LocalPath
			}
			metadata = append(metadata, item)
		}
		if len(metadata) > 0 {
			payload["attachments_metadata"] = metadata
		}
	}
	return payload, nil
}

func attachmentPartHeader(attachment UiPathAttachment) textproto.MIMEHeader {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, escapeMultipartFilename(attachment.Name)))
	if attachment.Type != "" {
		header.Set("Content-Type", attachment.Type)
	} else {
		header.Set("Content-Type", "application/octet-stream")
	}
	return header
}

func escapeMultipartFilename(name string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(name)
}

// CreateSession 创建新的会话
func (c *UiPathClient) CreateSession(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/v1/chat/sessions/", c.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.accessToken))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create session failed: %d - %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	debug.Printf("   CreateSession raw response: %s\n", string(body))

	var result struct {
		ID        string `json:"_id"`
		SessionID string `json:"session_id"`
		ChatID    string `json:"chat_id"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// 尝试所有可能的字段名
	sessionID := result.ID
	if sessionID == "" {
		sessionID = result.SessionID
	}
	if sessionID == "" {
		sessionID = result.ChatID
	}

	return sessionID, nil
}
