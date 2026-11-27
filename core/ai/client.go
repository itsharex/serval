// Package ai 提供面向终端的 AI 助手核心能力，基于 OpenAI 兼容的 Chat Completions API。
// 纯 Go 标准库实现，支持自定义 BaseURL、APIKey、Model。
// 不依赖 UI。
package ai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrNoAPIKey 表示未配置 API Key。
	ErrNoAPIKey = errors.New("未配置 AI API Key")
	// ErrEmptyResponse 表示 AI 服务返回空内容。
	ErrEmptyResponse = errors.New("AI 服务返回内容为空")
	// ErrUnauthorized 表示 API Key 无效或无权访问。
	ErrUnauthorized = errors.New("API Key 无效或无访问权限")
	// ErrRateLimited 表示请求过于频繁或额度不足。
	ErrRateLimited = errors.New("AI 服务请求过于频繁或额度已用尽")
	// ErrBadGateway 表示服务端暂时不可用。
	ErrBadGateway = errors.New("AI 服务暂时不可用")
	// ErrTimeout 表示请求超时。
	ErrTimeout = errors.New("请求 AI 服务超时")
	// ErrInvalidEndpoint 表示 API 端点地址无效。
	ErrInvalidEndpoint = errors.New("无效的 API 端点地址")
	// ErrStreamInterrupted 表示流式响应连接被中断或重置。
	ErrStreamInterrupted = errors.New("AI 响应流被中断")
	// ErrInvalidResponse 表示服务端返回了无法解析的响应格式。
	ErrInvalidResponse = errors.New("AI 服务返回了无法解析的响应格式")
)

// DefaultBaseURL 是默认的 OpenAI 兼容端点。
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultModel 是默认的模型名称。
const DefaultModel = "gpt-5.4"

// Config 是 AI 客户端配置。
type Config struct {
	BaseURL           string // 如 https://api.openai.com/v1 或本地 http://localhost:11434/v1
	APIKey            string // API 访问密钥
	Model             string // 模型名称，如 gpt-5.4, deepseek-chat, qwen3-max
	Timeout           time.Duration
	StreamIdleTimeout time.Duration // 流式读取空闲超时，默认 120s
}

// Client 是 OpenAI 兼容客户端。
type Client struct {
	baseURL           string
	apiKey            string
	model             string
	httpClient        *http.Client
	streamClient      *http.Client
	streamIdleTimeout time.Duration
}

func newAITransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       25 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// NewClient 创建 AI 客户端。
func NewClient(cfg Config) *Client {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultModel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	streamIdle := cfg.StreamIdleTimeout
	if streamIdle <= 0 {
		streamIdle = 120 * time.Second
	}
	headerTimeout := 90 * time.Second
	if isLocalEndpoint(baseURL) {
		headerTimeout = 5 * time.Minute
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   model,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: newAITransport(0),
		},
		streamClient: &http.Client{
			Transport: newAITransport(headerTimeout),
		},
		streamIdleTimeout: streamIdle,
	}
}

// Message 代表一条聊天消息。
type Message struct {
	Role             string          `json:"role"` // "system", "user", "assistant", "tool"
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// IsLocalEndpoint 判断 baseURL 是否指向本机回环地址（如本地 Ollama，无需强制 API Key）。
func IsLocalEndpoint(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func isLocalEndpoint(baseURL string) bool {
	return IsLocalEndpoint(baseURL)
}

// APIError 表示 AI 服务端返回的 HTTP 错误响应（保留状态码与脱敏后的服务端提示）。
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// Unwrap 使已有哨兵错误判断（errors.Is(err, ErrUnauthorized) 等）保持兼容。
func (e *APIError) Unwrap() error {
	switch {
	case e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case e.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case e.StatusCode >= 500:
		return ErrBadGateway
	default:
		return nil
	}
}

func mapHTTPError(status int) error {
	return &APIError{StatusCode: status}
}

func extractErrorMessageFromEnvelope(raw json.RawMessage) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
		Msg     string          `json:"msg"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ""
	}
	if len(envelope.Error) > 0 {
		var errObj struct {
			Message string `json:"message"`
			Msg     string `json:"msg"`
		}
		if json.Unmarshal(envelope.Error, &errObj) == nil {
			if m := strings.TrimSpace(errObj.Message); m != "" {
				return m
			}
			if m := strings.TrimSpace(errObj.Msg); m != "" {
				return m
			}
		}
		var errStr string
		if json.Unmarshal(envelope.Error, &errStr) == nil && strings.TrimSpace(errStr) != "" {
			return strings.TrimSpace(errStr)
		}
	}
	for _, cand := range []string{envelope.Message, envelope.Detail, envelope.Msg} {
		if m := strings.TrimSpace(cand); m != "" {
			return m
		}
	}
	return ""
}

func parseAPIError(status int, body []byte, secret string) error {
	var msg string
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		msg = extractErrorMessageFromEnvelope(trimmed)
		if msg == "" && trimmed[0] == '[' {
			var arr []json.RawMessage
			if json.Unmarshal(trimmed, &arr) == nil {
				for _, item := range arr {
					if m := extractErrorMessageFromEnvelope(item); m != "" {
						msg = m
						break
					}
				}
			}
		}
		if msg == "" && trimmed[0] != '<' {
			msg = strings.Join(strings.Fields(string(trimmed)), " ")
		}
	}
	if secret != "" && msg != "" {
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	if runes := []rune(msg); len(runes) > 240 {
		msg = string(runes[:240]) + "…"
	}
	return &APIError{
		StatusCode: status,
		Message:    msg,
	}
}

func isStreamResetError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, kw := range []string{
		"connection reset",
		"broken pipe",
		"unexpected eof",
		"http2: response body closed",
		"stream error:",
		"goaway",
		"client connection force closed",
	} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func mapTransportError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if isStreamResetError(err) {
		return fmt.Errorf("%w: %v", ErrStreamInterrupted, err)
	}
	return err
}

// Probe 检测 AI 服务连通性。优先请求 GET /models（不消耗 token），
// 若服务端不支持再回退到最短对话（max_tokens=1）。
func (c *Client) Probe(ctx context.Context) error {
	if c.apiKey == "" && !isLocalEndpoint(c.baseURL) {
		return ErrNoAPIKey
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidEndpoint
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/models", nil)
	if err != nil {
		return ErrInvalidEndpoint
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mapTransportError(err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return mapHTTPError(resp.StatusCode)
	}

	// 部分兼容端点未实现 GET /models（返回 404/405），回退到最短对话。
	_, err = c.chatWithOptions(ctx, []Message{{Role: "user", Content: "hi"}}, 1)
	return err
}

// ListModels 从 OpenAI 兼容接口的 GET /models（本地 Ollama 可回退到 /api/tags）获取可用对话模型列表。
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	if c.apiKey == "" && !isLocalEndpoint(c.baseURL) {
		return nil, ErrNoAPIKey
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ErrInvalidEndpoint
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/models", nil)
	if err != nil {
		return nil, ErrInvalidEndpoint
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, mapTransportError(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, parseAPIError(resp.StatusCode, body, c.apiKey)
	}

	var models []string
	if resp.StatusCode == http.StatusOK {
		if readErr != nil {
			return nil, mapTransportError(readErr)
		}
		models = parseModelsResponse(body)
	}

	if len(models) == 0 && isLocalEndpoint(c.baseURL) {
		if tags, tagErr := c.listOllamaTags(ctx); tagErr == nil && len(tags) > 0 {
			models = tags
		}
	}

	if len(models) == 0 {
		if resp.StatusCode != http.StatusOK {
			return nil, parseAPIError(resp.StatusCode, body, c.apiKey)
		}
		return nil, ErrEmptyResponse
	}

	return models, nil
}

func (c *Client) listOllamaTags(ctx context.Context) ([]string, error) {
	base := strings.TrimRight(c.baseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/tags", nil)
	if err != nil {
		return nil, ErrInvalidEndpoint
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, mapTransportError(err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return nil, mapTransportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, mapHTTPError(resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var raw []string
	for _, m := range payload.Models {
		id := strings.TrimSpace(m.Name)
		if id == "" {
			id = strings.TrimSpace(m.Model)
		}
		if id != "" {
			raw = append(raw, id)
		}
	}
	return filterChatModelIDs(raw), nil
}

func parseModelsResponse(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var raw []string
	for _, item := range payload.Data {
		if strings.TrimSpace(item.ID) != "" {
			raw = append(raw, item.ID)
		}
	}
	for _, item := range payload.Models {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(item.Name)
		}
		if id != "" {
			raw = append(raw, id)
		}
	}
	return filterChatModelIDs(raw)
}

func filterChatModelIDs(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		id := normalizeModelID(item)
		if id == "" || !isChatModelID(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	slices.SortStableFunc(out, func(a, b string) int {
		return cmp.Compare(modelPreferenceRank(a), modelPreferenceRank(b))
	})
	return out
}

var preferredModelRanks = map[string]int{
	"gpt-5.4":            10,
	"gpt-5.4-mini":       11,
	"gpt-5":              12,
	"gpt-5-mini":         13,
	"gpt-4.1":            14,
	"gpt-4.1-mini":       15,
	"gpt-4o":             16,
	"gpt-4o-mini":        17,
	"o3":                 18,
	"o4-mini":            19,
	"deepseek-chat":      20,
	"deepseek-reasoner":  21,
	"qwen3-max":          30,
	"qwen-max":           31,
	"qwen-plus":          32,
	"kimi-k2":            40,
	"moonshot-v1-auto":   41,
	"glm-4.5":            50,
	"glm-4-plus":         51,
	"gemini-2.5-pro":     55,
	"gemini-2.5-flash":   56,
	"gemini-2.0-flash":   57,
	"minimax-m3.1-flash": 60,
	"minimax-m3":         61,
	"minimax-m2.7":       62,
	"minimax-m2.5":       63,
	"minimax-m2.1":       64,
	"minimax-text-01":    65,
}

func modelPreferenceRank(id string) int {
	low := strings.ToLower(strings.TrimSpace(id))
	if r, ok := preferredModelRanks[low]; ok {
		return r
	}
	// 将实验版、预览版、内测后缀或带日期快照的模型放到标准模型之后，避免优先选中冷门/不兼容型号
	suffixes := []string{
		"-terra", "-preview", "-exp", "-alpha", "-beta", "-test", "-dev",
		"-2023", "-2024", "-2025", "-2026", "-2027", "-2028",
	}
	for _, s := range suffixes {
		if strings.Contains(low, s) {
			return 300
		}
	}
	return 100
}

// ChoosePreferredModel 从可用模型列表中挑选优先级最高的主流对话模型。
func ChoosePreferredModel(models []string) string {
	best := ""
	bestRank := 1 << 30
	for _, m := range models {
		id := strings.TrimSpace(m)
		if id == "" {
			continue
		}
		rank := modelPreferenceRank(id)
		if best == "" || rank < bestRank {
			best = id
			bestRank = rank
		}
	}
	return best
}

func normalizeModelID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "models/")
	return strings.TrimSpace(id)
}

func isChatModelID(id string) bool {
	if id == "" {
		return false
	}
	low := strings.ToLower(id)
	excluded := []string{
		"embedding",
		"tts",
		"whisper",
		"dall-e",
		"moderation",
		"babbage",
		"davinci",
		"transcribe",
		"realtime",
		"audio",
		"sora",
		"omni-moderation",
		"gpt-image",
		"canary",
		"search-preview",
		"computer-use",
		"codex-mini",
		"imagen",
		"veo",
		"lyria",
	}
	for _, kw := range excluded {
		if strings.Contains(low, kw) {
			return false
		}
	}
	return true
}

// Chat 发送一条对话请求并返回响应文本。
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	return c.chatWithOptions(ctx, messages, 0)
}

func (c *Client) chatWithOptions(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	if c.apiKey == "" && !isLocalEndpoint(c.baseURL) {
		return "", ErrNoAPIKey
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidEndpoint
	}

	endpoint := c.baseURL + "/chat/completions"
	prepared := prepareMessagesForProvider(c.baseURL, c.model, messages)
	// 不强行传入 temperature，避免 gpt-5*/o3/o4 等仅接受默认采样参数的模型返回 HTTP 400。
	reqBody, err := json.Marshal(chatRequest{
		Model:     c.model,
		Messages:  prepared,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", ErrInvalidEndpoint
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", mapTransportError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", mapTransportError(err)
	}

	if resp.StatusCode != http.StatusOK {
		chatErr := parseAPIError(resp.StatusCode, body, c.apiKey)
		if shouldFallbackToResponses(resp.StatusCode, chatErr) {
			if rRes, rStatus, rErr := c.doResponses(ctx, prepared, nil, false, nil); rErr == nil && strings.TrimSpace(rRes.Content) != "" {
				return strings.TrimSpace(rRes.Content), nil
			} else if rErr != nil && rStatus != 0 && rStatus != http.StatusNotFound && rStatus != http.StatusMethodNotAllowed {
				return "", rErr
			}
		}
		return "", chatErr
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}

	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content == "" {
		return "", ErrEmptyResponse
	}

	return strings.TrimSpace(chatResp.Choices[0].Message.Content), nil
}

// Result 是生成命令或错误分析的解析结果。
type Result struct {
	Explanation string   // 简明原理解释
	Command     string   // 主推荐命令（第一个代码块）
	Commands    []string // 提取出的全部代码块命令
	Raw         string   // 完整原始文本
}

// GenerateCommand 根据用户的自然语言需求，生成对应操作系统的 Shell 命令。
func (c *Client) GenerateCommand(ctx context.Context, osInfo, query string) (*Result, error) {
	systemPrompt := "你是一名资深 Linux/Unix 运维专家。根据用户的需求，生成安全、准确的 Shell 命令。\n" +
		"规则：\n" +
		"1. 输出第一部分简要解释原理（中文，1-2 句话）。\n" +
		"2. 输出第二部分将命令放在 ```bash 代码块中，仅包含可执行命令，不要添加多余文字。\n" +
		"3. 若涉及高危危险操作（如 rm -rf、格式化磁盘），必须在解释中做醒目警告。"

	var userPrompt string
	if osInfo != "" {
		userPrompt = fmt.Sprintf("目标环境：%s\n需求：%s", osInfo, query)
	} else {
		userPrompt = query
	}

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	raw, err := c.Chat(ctx, messages)
	if err != nil {
		return nil, err
	}

	return parseMarkdownCommand(raw), nil
}

// ExplainError 分析终端报错并给出修复建议和命令。
func (c *Client) ExplainError(ctx context.Context, osInfo, errorText string) (*Result, error) {
	systemPrompt := "你是一名资深系统运维与故障排查专家。根据终端报错信息，分析可能的原因并提供修复方案。\n" +
		"规则：\n" +
		"1. 第一部分简要解释报错根因（中文，2-3 句话）。\n" +
		"2. 若有排查或修复命令，放在 ```bash 代码块中。\n" +
		"3. 不要包含虚构或不可靠的猜想。"

	var userPrompt string
	if osInfo != "" {
		userPrompt = fmt.Sprintf("运行环境：%s\n终端报错信息：\n%s", osInfo, errorText)
	} else {
		userPrompt = fmt.Sprintf("终端报错信息：\n%s", errorText)
	}

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	raw, err := c.Chat(ctx, messages)
	if err != nil {
		return nil, err
	}

	return parseMarkdownCommand(raw), nil
}

// FollowUp 基于本次会话历史继续追问。
func (c *Client) FollowUp(ctx context.Context, osInfo string, history []Message, query string) (*Result, error) {
	systemPrompt := "你是一名资深 Linux/Unix 运维专家。结合前面的对话上下文回答用户的追问。\n" +
		"规则：\n" +
		"1. 简明解释（中文，1-3 句话）。\n" +
		"2. 若给出可执行命令，放在 ```bash 代码块中。"
	if osInfo != "" {
		systemPrompt += "\n目标环境：" + osInfo
	}

	messages := make([]Message, 0, len(history)+2)
	messages = append(messages, Message{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, Message{Role: "user", Content: query})

	raw, err := c.Chat(ctx, messages)
	if err != nil {
		return nil, err
	}
	return parseMarkdownCommand(raw), nil
}

// ParseCommandText 从包含 Markdown 代码块或 JSON 的文本中提取命令与解释说明。
func ParseCommandText(text string) *Result {
	return parseMarkdownCommand(text)
}

// parseMarkdownCommand 从包含 Markdown 代码块或 JSON 的文本中提取命令与解释说明。
func parseMarkdownCommand(text string) *Result {
	res := &Result{Raw: text}
	trimmed := strings.TrimSpace(text)

	// 1. 优先尝试解析 JSON 格式
	var jsonObj struct {
		Explanation string `json:"explanation"`
		Command     string `json:"command"`
	}
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &jsonObj) == nil {
		if jsonObj.Command != "" || jsonObj.Explanation != "" {
			res.Explanation = jsonObj.Explanation
			res.Command = strings.TrimSpace(jsonObj.Command)
			if res.Command != "" {
				res.Commands = []string{res.Command}
			}
			return res
		}
	}

	// 2. Markdown 多代码块解析
	rest := text
	var explainParts []string
	for {
		idx := strings.Index(rest, "```")
		if idx == -1 {
			if tail := strings.TrimSpace(rest); tail != "" {
				explainParts = append(explainParts, tail)
			}
			break
		}
		if head := strings.TrimSpace(rest[:idx]); head != "" {
			explainParts = append(explainParts, head)
		}
		after := rest[idx+3:]
		if nl := strings.Index(after, "\n"); nl != -1 {
			after = after[nl+1:]
		}
		endIdx := strings.Index(after, "```")
		var block string
		if endIdx != -1 {
			block = strings.TrimSpace(after[:endIdx])
			rest = after[endIdx+3:]
		} else {
			block = strings.TrimSpace(after)
			rest = ""
		}
		if block != "" {
			res.Commands = append(res.Commands, block)
		}
		if endIdx == -1 {
			break
		}
	}

	res.Explanation = strings.Join(explainParts, "\n")
	if len(res.Commands) > 0 {
		res.Command = res.Commands[0]
	}
	return res
}

// IsDangerousCommand 通过词法切分识别可能造成系统破坏、数据丢失、意外关机或含隐蔽换行的危险命令。
// 相比简单子串匹配，能区分 `grep reboot log` 等安全查询与真正的高危调用。
func IsDangerousCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return false
	}

	// 含有换行符的多行指令，粘贴到终端存在首行自动触发执行的风险
	if strings.ContainsAny(trimmed, "\r\n") {
		return true
	}

	// Fork 炸弹
	if strings.Contains(trimmed, ":(){") && strings.Contains(trimmed, ":|:&") {
		return true
	}

	// 递归检查 $(...) 与反引号中的内嵌子命令，防止通过命令替换绕过检测
	for _, sub := range extractCommandSubstitutions(trimmed) {
		if IsDangerousCommand(sub) {
			return true
		}
	}

	// 按 shell 分隔符切分子命令
	segments := splitShellSegments(trimmed)
	for i, seg := range segments {
		tokens := shellFields(seg)
		if len(tokens) == 0 {
			continue
		}

		// 检查管道到 shell（如 curl ... | sh, wget ... | bash）
		if i > 0 {
			first := normalizeProgName(tokens[0])
			if first == "sudo" && len(tokens) > 1 {
				first = normalizeProgName(tokens[1])
			}
			if first == "sh" || first == "bash" || first == "zsh" || first == "dash" {
				return true
			}
		}

		// 检查重定向到设备节点或关键系统文件
		if hasDangerousRedirect(tokens) {
			return true
		}

		// 剥离前置 sudo / doas / env 等包装命令
		idx := skipCommandWrappers(tokens)
		if idx >= len(tokens) {
			continue
		}
		prog := normalizeProgName(tokens[idx])
		args := tokens[idx+1:]

		// 递归检查 sh -c "..." / bash -c "..." / eval "..."
		if (prog == "sh" || prog == "bash" || prog == "zsh" || prog == "dash") && len(args) >= 2 {
			for ai := 0; ai < len(args)-1; ai++ {
				if args[ai] == "-c" || args[ai] == "-lc" {
					if IsDangerousCommand(args[ai+1]) {
						return true
					}
				}
			}
		}
		if prog == "eval" && len(args) > 0 {
			if IsDangerousCommand(strings.Join(args, " ")) {
				return true
			}
		}
		if prog == "xargs" && len(args) > 0 {
			xIdx := 0
			for xIdx < len(args) && strings.HasPrefix(args[xIdx], "-") {
				// 跳过带参数的常见 xargs 选项
				if (args[xIdx] == "-I" || args[xIdx] == "-n" || args[xIdx] == "-P" || args[xIdx] == "-d" || args[xIdx] == "-L") && xIdx+1 < len(args) {
					xIdx += 2
					continue
				}
				xIdx++
			}
			if xIdx < len(args) && isDangerousInvocation(normalizeProgName(args[xIdx]), args[xIdx+1:]) {
				return true
			}
		}

		if isDangerousInvocation(prog, args) {
			return true
		}
	}

	return false
}

// IsDangerousExecCommand 判断 ssh_exec 传入的命令是否高危。
// 与 IsDangerousCommand 不同，ssh_exec 是非交互执行，不会因粘贴换行自动回车，
// 因此折叠续行后逐行做高危词法检查，避免安全的多行只读脚本被误拦。
func IsDangerousExecCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, ":(){") && strings.Contains(trimmed, ":|:&") {
		return true
	}
	normalized := strings.ReplaceAll(trimmed, "\\\r\n", " ")
	normalized = strings.ReplaceAll(normalized, "\\\n", " ")
	for _, line := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '\r' || r == '\n' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if IsDangerousCommand(line) {
			return true
		}
	}
	return false
}

// IsReadOnlyProbeCommand 判断命令是否属于纯只读系统探测命令（无重定向、无命令替换且每段均为只读白名单）。
// 用于在生产环境主机的「自动执行」模式下放行常规只读诊断，同时对写操作/服务变更强制弹窗确认。
func IsReadOnlyProbeCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" || strings.ContainsAny(trimmed, "\r\n") {
		return false
	}
	if len(extractCommandSubstitutions(trimmed)) > 0 {
		return false
	}
	segments := splitShellSegments(trimmed)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		tokens := shellFields(seg)
		if len(tokens) == 0 {
			return false
		}
		// 任何输出重定向（> / >>）均视为非只读
		for _, tok := range tokens {
			if strings.HasPrefix(tok, ">") || strings.Contains(tok, ">>") {
				return false
			}
		}
		idx := skipCommandWrappers(tokens)
		if idx >= len(tokens) {
			return false
		}
		prog := normalizeProgName(tokens[idx])
		args := tokens[idx+1:]
		if !isReadOnlyInvocation(prog, args) {
			return false
		}
	}
	return true
}

func isReadOnlyInvocation(prog string, args []string) bool {
	switch prog {
	case "ls", "ll", "la", "dir", "pwd", "whoami", "id", "uname", "uptime", "hostname",
		"date", "df", "du", "free", "top", "htop", "ps", "pgrep", "lscpu", "lsmem",
		"lsblk", "lsmod", "lspci", "lsusb", "lsof", "cat", "head", "tail", "less", "more",
		"grep", "egrep", "fgrep", "rg", "awk", "sort", "uniq", "wc", "cut", "tr", "diff",
		"stat", "file", "readlink", "realpath", "which", "whereis", "type", "ss", "netstat",
		"ifconfig", "route", "traceroute", "tracepath", "dig", "nslookup", "host",
		"env", "printenv", "echo", "printf", "journalctl", "dmesg", "last", "who", "w",
		"vmstat", "iostat", "mpstat", "sar", "sysctl":
		if prog == "sysctl" {
			for _, a := range args {
				if strings.Contains(a, "=") || a == "-w" {
					return false
				}
			}
		}
		return true
	case "sed":
		for _, a := range args {
			if a == "-i" || strings.HasPrefix(a, "-i") || a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
				return false
			}
		}
		return true
	case "find":
		for _, a := range args {
			low := strings.ToLower(a)
			if low == "-delete" || low == "-exec" || low == "-execdir" || low == "-ok" {
				return false
			}
		}
		return true
	case "ip":
		if len(args) == 0 {
			return true
		}
		for _, a := range args {
			low := strings.ToLower(a)
			if low == "add" || low == "del" || low == "delete" || low == "set" || low == "change" || low == "replace" || low == "flush" {
				return false
			}
		}
		return true
	case "ping", "ping6":
		return slices.Contains(args, "-c")
	case "systemctl":
		if len(args) == 0 {
			return false
		}
		sub := strings.ToLower(args[0])
		return sub == "status" || sub == "list-units" || sub == "list-unit-files" ||
			sub == "is-active" || sub == "is-enabled" || sub == "show" || sub == "cat"
	case "service":
		return len(args) >= 2 && strings.ToLower(args[1]) == "status"
	case "docker", "podman":
		if len(args) == 0 {
			return false
		}
		sub := strings.ToLower(args[0])
		return sub == "ps" || sub == "images" || sub == "logs" || sub == "inspect" ||
			sub == "stats" || sub == "info" || sub == "version" || sub == "top"
	case "kubectl":
		if len(args) == 0 {
			return false
		}
		sub := strings.ToLower(args[0])
		return sub == "get" || sub == "describe" || sub == "logs" || sub == "top" || sub == "version"
	case "nginx", "apachectl", "httpd":
		return slices.Contains(args, "-t") || slices.Contains(args, "-T") || slices.Contains(args, "-v") || slices.Contains(args, "-V")
	case "crontab":
		return slices.Contains(args, "-l")
	}
	return false
}

func normalizeProgName(tok string) string {
	tok = strings.TrimLeft(tok, "({")
	tok = strings.TrimRight(tok, ")}")
	if idx := strings.LastIndexByte(tok, '/'); idx >= 0 {
		tok = tok[idx+1:]
	}
	return strings.ToLower(tok)
}

func skipCommandWrappers(tokens []string) int {
	idx := 0
	for idx < len(tokens) {
		low := normalizeProgName(tokens[idx])
		if low == "sudo" || low == "doas" || low == "env" || low == "nohup" || low == "nice" ||
			low == "timeout" || strings.Contains(tokens[idx], "=") {
			if (low == "timeout" || low == "nice") && idx+1 < len(tokens) {
				idx += 2
				continue
			}
			idx++
			continue
		}
		break
	}
	return idx
}

func extractCommandSubstitutions(s string) []string {
	var out []string
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			}
			continue
		}
		if ch == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if ch == '"' {
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			continue
		}
		if ch == '\'' && quote == 0 {
			quote = '\''
			continue
		}
		if ch == '$' && i+1 < len(s) && s[i+1] == '(' {
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				if s[j] == '(' {
					depth++
				} else if s[j] == ')' {
					depth--
				}
				j++
			}
			if depth == 0 {
				out = append(out, s[i+2:j-1])
				i = j - 1
			}
			continue
		}
		if ch == '`' {
			j := i + 1
			for j < len(s) && s[j] != '`' {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				j++
			}
			if j < len(s) {
				out = append(out, s[i+1:j])
				i = j
			}
		}
	}
	return out
}

func splitShellSegments(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			cur.WriteByte(ch)
			if ch == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			cur.WriteByte(ch)
			continue
		}
		if ch == ';' || ch == '|' {
			out = append(out, cur.String())
			cur.Reset()
			if ch == '|' && i+1 < len(s) && s[i+1] == '|' {
				i++
			}
			continue
		}
		if ch == '&' && i+1 < len(s) && s[i+1] == '&' {
			out = append(out, cur.String())
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			cur.WriteByte(ch)
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch == ' ' || ch == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func hasDangerousRedirect(tokens []string) bool {
	dangerousTargets := []string{
		"/dev/sd", "/dev/nvme", "/dev/vd", "/dev/hd", "/dev/disk",
		"/etc/passwd", "/etc/shadow", "/etc/sudoers",
	}
	for i, tok := range tokens {
		target := ""
		if strings.HasPrefix(tok, ">>") {
			target = strings.TrimPrefix(tok, ">>")
			if target == "" && i+1 < len(tokens) {
				target = tokens[i+1]
			}
		} else if strings.HasPrefix(tok, ">") {
			target = strings.TrimPrefix(tok, ">")
			if target == "" && i+1 < len(tokens) {
				target = tokens[i+1]
			}
		}
		if target == "" {
			continue
		}
		low := strings.ToLower(target)
		for _, dt := range dangerousTargets {
			if strings.HasPrefix(low, dt) {
				return true
			}
		}
	}
	return false
}

func isDangerousInvocation(prog string, args []string) bool {
	switch {
	case prog == "rm":
		for _, a := range args {
			low := strings.ToLower(a)
			if low == "--recursive" || (strings.HasPrefix(low, "-") && !strings.HasPrefix(low, "--") && strings.ContainsRune(low, 'r')) {
				return true
			}
		}
	case strings.HasPrefix(prog, "mkfs"):
		return true
	case prog == "dd":
		for _, a := range args {
			if strings.HasPrefix(strings.ToLower(a), "of=/dev/") {
				return true
			}
		}
	case prog == "shutdown" || prog == "reboot" || prog == "poweroff" || prog == "halt":
		return true
	case prog == "init" && len(args) > 0 && (args[0] == "0" || args[0] == "6"):
		return true
	case prog == "systemctl" && len(args) > 0:
		sub := strings.ToLower(args[0])
		if sub == "poweroff" || sub == "reboot" || sub == "halt" {
			return true
		}
		if (sub == "stop" || sub == "disable") && len(args) > 1 {
			svc := strings.ToLower(args[1])
			if svc == "ssh" || svc == "sshd" || svc == "ssh.service" || svc == "sshd.service" ||
				svc == "networking" || svc == "networkmanager" || svc == "systemd-networkd" {
				return true
			}
		}
	case prog == "kill":
		for _, a := range args {
			if a == "-1" {
				return true
			}
		}
	case prog == "killall" || prog == "pkill":
		for _, a := range args {
			if strings.Contains(strings.ToLower(a), "sshd") {
				return true
			}
		}
	case prog == "iptables" || prog == "ip6tables":
		for _, a := range args {
			if a == "-F" || a == "--flush" || a == "-X" {
				return true
			}
		}
	case prog == "ufw":
		return len(args) > 0 && strings.ToLower(args[0]) == "disable"
	case prog == "nft":
		return len(args) >= 2 && strings.ToLower(args[0]) == "flush"
	case prog == "find":
		for i, a := range args {
			low := strings.ToLower(a)
			if low == "-delete" {
				return true
			}
			if (low == "-exec" || low == "-execdir") && i+1 < len(args) {
				if isDangerousInvocation(normalizeProgName(args[i+1]), args[i+2:]) {
					return true
				}
			}
		}
	case prog == "chmod" || prog == "chown":
		hasRec := false
		hasWide := false
		hasRoot := false
		for _, a := range args {
			low := strings.ToLower(a)
			if low == "-r" || low == "--recursive" {
				hasRec = true
			}
			if prog == "chmod" && (low == "777" || low == "000") {
				hasWide = true
			}
			if low == "/" {
				hasRoot = true
			}
		}
		if hasRec && (hasWide || hasRoot) {
			return true
		}
	}
	return false
}
