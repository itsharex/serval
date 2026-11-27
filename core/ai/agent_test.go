package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunAgentStreamingToolLoop(t *testing.T) {
	var round atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		switch n {
		case 1:
			// 第一轮：流式推理 + 增量 tool_call 参数拼接 + usage
			chunks := []string{
				`{"choices":[{"delta":{"reasoning_content":"先查 CPU "}}]}`,
				`{"choices":[{"delta":{"reasoning_content":"和内存"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ssh_exec","arguments":"{\"command\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"lscpu && free -h\"}"}}]}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":40,"completion_tokens_details":{"reasoning_tokens":12}}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			// 检查第一轮工具结果已回传
			msgs, _ := req["messages"].([]any)
			hasToolMsg := false
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "tool" && mm["tool_call_id"] == "call_1" {
					hasToolMsg = true
				}
			}
			if !hasToolMsg {
				http.Error(w, "missing tool message", 400)
				return
			}
			chunks := []string{
				`{"choices":[{"delta":{"content":"### 配置概览\n\n| 项目 | 配置 |\n|---|---|\n| CPU | 4 vCPU |\n"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":220,"completion_tokens":35,"total_tokens":255,"prompt_tokens_details":{"cached_tokens":100}}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected round", 500)
		}
	}))
	defer ts.Close()

	client := NewClient(Config{BaseURL: ts.URL, APIKey: "sk-test", Model: "deepseek-chat"})
	var gotEvents []AgentEvent
	var executed []string

	res, err := RunAgent(context.Background(), AgentRunConfig{
		Client:   client,
		Messages: []Message{{Role: "user", Content: "这台服务配置如何"}},
		Tools: []ToolDefinition{{
			Type:     "function",
			Function: ToolFunction{Name: "ssh_exec", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ExecuteTool: func(_ context.Context, call ToolCall) (string, bool) {
			executed = append(executed, call.Function.Name+":"+call.Function.Arguments)
			return "CPU: 4\nMem: 16Gi", false
		},
		OnEvent: func(ev AgentEvent) {
			gotEvents = append(gotEvents, ev)
		},
	})
	if err != nil {
		t.Fatalf("RunAgent failed: %v", err)
	}
	if res.StepCount != 2 {
		t.Fatalf("expected 2 steps, got %d", res.StepCount)
	}
	if len(executed) != 1 || !strings.Contains(executed[0], "lscpu && free -h") {
		t.Fatalf("unexpected executed tools: %+v", executed)
	}
	if !strings.Contains(res.FinalText, "4 vCPU") {
		t.Fatalf("unexpected final text: %q", res.FinalText)
	}
	if res.Usage.PromptTokens != 320 || res.Usage.CompletionTokens != 55 || res.Usage.TotalTokens != 375 ||
		res.Usage.CachedTokens != 140 || res.Usage.ReasoningTokens != 12 {
		t.Fatalf("unexpected aggregated usage: %+v", res.Usage)
	}

	var sawThinking, sawToolStart, sawToolDone bool
	for _, ev := range gotEvents {
		if strings.Contains(ev.Thinking, "先查 CPU") {
			sawThinking = true
		}
		if ev.Kind == EventToolStart {
			sawToolStart = true
		}
		if ev.Kind == EventToolDone && strings.Contains(ev.ToolOutput, "Mem: 16Gi") {
			sawToolDone = true
		}
	}
	if !sawThinking || !sawToolStart || !sawToolDone {
		t.Fatalf("missing expected lifecycle events: think=%v start=%v done=%v", sawThinking, sawToolStart, sawToolDone)
	}
}

func TestStreamChatFallbackWithoutStreamOptionsAndJSONResponse(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n == 1 {
			if _, ok := req["stream_options"]; !ok {
				t.Fatal("first attempt should include stream_options")
			}
			http.Error(w, `{"error":"unsupported stream_options"}`, http.StatusBadRequest)
			return
		}
		if _, ok := req["stream_options"]; ok {
			t.Fatal("fallback attempt must omit stream_options")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{
					"content": "<think>思考一下</think>\n已检查完成。",
				}},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer ts.Close()

	client := NewClient(Config{BaseURL: ts.URL, APIKey: "sk-test", Model: "legacy-model"})
	res, err := client.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat fallback failed: %v", err)
	}
	if res.Thinking != "思考一下" || res.Content != "已检查完成。" || res.Usage.TotalTokens != 15 {
		t.Fatalf("unexpected fallback result: %+v", res)
	}
}

func TestStreamChatAndChatFallbackToResponsesAndOmitTemperature(t *testing.T) {
	var round atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, hasTemp := req["temperature"]; hasTemp {
			http.Error(w, `{"error":{"message":"temperature is not supported"}}`, http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"This model is only supported in v1/responses"}}`))
		case "/responses":
			n := round.Add(1)
			if n == 1 {
				// 第一步：流式推理 + tool_call
				w.Header().Set("Content-Type", "text/event-stream")
				events := []string{
					"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"先查系统\"}\n\n",
					"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_r1\",\"name\":\"host_info\",\"arguments\":\"\"}}\n\n",
					"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{}\"} \n\n",
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":50,\"output_tokens\":10,\"total_tokens\":60}}}\n\n",
				}
				for _, ev := range events {
					fmt.Fprint(w, ev)
				}
				return
			}
			// 第二步（RunAgent 工具结果回传）需要带 function_call_output；第三步（Chat）直接返回文本
			if n == 2 {
				inputs, _ := req["input"].([]any)
				sawOutput := false
				for _, it := range inputs {
					m, _ := it.(map[string]any)
					if m["type"] == "function_call_output" && m["call_id"] == "call_r1" {
						sawOutput = true
					}
				}
				if !sawOutput {
					http.Error(w, `{"error":{"message":"missing function_call_output"}}`, http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"output": []map[string]any{
					{
						"type": "message",
						"role": "assistant",
						"content": []map[string]string{
							{"type": "output_text", "text": "主机运行正常。"},
						},
					},
				},
				"usage": map[string]int{"input_tokens": 80, "output_tokens": 12, "total_tokens": 92},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewClient(Config{BaseURL: ts.URL, APIKey: "sk-resp", Model: "gpt-5.4"})
	res, err := RunAgent(context.Background(), AgentRunConfig{
		Client:   client,
		Messages: []Message{{Role: "user", Content: "检查主机"}},
		Tools: []ToolDefinition{{
			Type:     "function",
			Function: ToolFunction{Name: "host_info", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ExecuteTool: func(_ context.Context, call ToolCall) (string, bool) {
			return `{"os":"Linux"}`, false
		},
	})
	if err != nil {
		t.Fatalf("RunAgent Responses fallback failed: %v", err)
	}
	if res.FinalText != "主机运行正常。" || res.StepCount != 2 || res.Usage.TotalTokens != 152 {
		t.Fatalf("unexpected Responses fallback result: %+v", res)
	}

	// 验证非流式 Chat 也能回退到 /responses
	chatOut, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "ping"}})
	if err != nil {
		t.Fatalf("Chat Responses fallback failed: %v", err)
	}
	if chatOut != "主机运行正常。" {
		t.Fatalf("unexpected Chat Responses output: %q", chatOut)
	}
}

func TestRunAgentPreservesGoogleThoughtSignatureAndCrossProviderSwitch(t *testing.T) {
	var round atomic.Int32
	const sig = "sig_encrypted_gemini_v3_xyz"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n := round.Add(1)
		msgs, _ := req["messages"].([]any)

		switch n {
		case 1:
			// 验证从 DeepSeek 历史切换到 Google 时：
			// 1. 早期 DeepSeek 轮次的 reasoning_content 已被剥离
			// 2. 缺失 thought_signature 的历史 tool_call 自动补齐 skip_thought_signature_validator
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "assistant" {
					if rc, hasRC := mm["reasoning_content"]; hasRC && rc != "" {
						t.Fatalf("Google 请求不应残留旧轮 reasoning_content: %v", rc)
					}
					tcs, _ := mm["tool_calls"].([]any)
					for _, tc := range tcs {
						tcm, _ := tc.(map[string]any)
						extra, _ := tcm["extra_content"].(map[string]any)
						g, _ := extra["google"].(map[string]any)
						if g["thought_signature"] != googleSkipThoughtSignature {
							t.Fatalf("历史缺失签名的 tool_call 应补齐跳过签名，实际=%v", tcm["extra_content"])
						}
					}
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"我先补充检查一下。"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_g1","type":"function","function":{"name":"ssh_exec","arguments":"{\"command\":\"echo ok\"}"},"extra_content":{"google":{"thought_signature":"%s"}}}]},"finish_reason":"tool_calls"}]}`, sig))
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			// 验证第 2 步回传时必须原样保留第 1 步 Google 返回的 thought_signature
			foundSig := false
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] != "assistant" {
					continue
				}
				tcs, _ := mm["tool_calls"].([]any)
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					if tcm["id"] == "call_g1" {
						extra, _ := tcm["extra_content"].(map[string]any)
						g, _ := extra["google"].(map[string]any)
						if g["thought_signature"] == sig {
							foundSig = true
						}
					}
				}
			}
			if !foundSig {
				http.Error(w, `[{"error":{"code":400,"message":"Function call is missing a thought_signature"}}]`+"\n", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"Google 工具调用已完成。"},"finish_reason":"stop"}]}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected round", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	// 模拟由 DeepSeek 对话历史切到 Google Gemini 模型
	history := []Message{
		{Role: "user", Content: "先帮我看看状态"},
		{
			Role:             "assistant",
			ReasoningContent: "这是上一轮 DeepSeek 的推理过程",
			ToolCalls: []ToolCall{{
				ID:       "ds_call_1",
				Type:     "function",
				Function: ToolCallFunction{Name: "ssh_exec", Arguments: `{"command":"uname -a"}`},
			}},
		},
		{Role: "tool", ToolCallID: "ds_call_1", Name: "ssh_exec", Content: "Linux"},
		{Role: "assistant", Content: "已查看系统信息。"},
		{Role: "user", Content: "继续"},
	}

	gClient := NewClient(Config{BaseURL: ts.URL, APIKey: "sk-g", Model: "gemini-2.5-pro"})
	res, err := RunAgent(context.Background(), AgentRunConfig{
		Client:   gClient,
		Messages: history,
		Tools: []ToolDefinition{{
			Type:     "function",
			Function: ToolFunction{Name: "ssh_exec", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ExecuteTool: func(_ context.Context, call ToolCall) (string, bool) {
			return "ok", false
		},
	})
	if err != nil {
		t.Fatalf("Google RunAgent failed: %v", err)
	}
	if res.FinalText != "Google 工具调用已完成。" {
		t.Fatalf("unexpected FinalText: %q", res.FinalText)
	}

	// 验证切回非 Google 服务商时，会剥离 Google 的 extra_content
	cleaned := prepareMessagesForProvider("https://api.deepseek.com/v1", "deepseek-chat", res.Messages)
	for _, m := range cleaned {
		for _, tc := range m.ToolCalls {
			if len(tc.ExtraContent) > 0 {
				t.Fatalf("非 Google 服务商应剥离 extra_content: %s", string(tc.ExtraContent))
			}
		}
	}
}

func TestParseAPIErrorGoogleArrayAndVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "google_array",
			body: `[{"error":{"code":400,"message":"Function call is missing a thought_signature sk-secret-123","status":"INVALID_ARGUMENT"}}]`,
			want: "Function call is missing a thought_signature ***",
		},
		{
			name: "string_error",
			body: `{"error":"unsupported stream_options"}`,
			want: "unsupported stream_options",
		},
		{
			name: "detail_field",
			body: `{"detail":"invalid model parameter"}`,
			want: "invalid model parameter",
		},
		{
			name: "plain_text",
			body: "bad request:\nmissing field",
			want: "bad request: missing field",
		},
		{
			name: "html_ignored",
			body: "<html><body>502 Bad Gateway</body></html>",
			want: "",
		},
	}

	for _, tc := range cases {
		err := parseAPIError(http.StatusBadRequest, []byte(tc.body), "sk-secret-123")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: expected *APIError, got %T", tc.name, err)
		}
		if apiErr.Message != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, apiErr.Message, tc.want)
		}
	}
}

func TestStreamChatNoBlindResponsesFallbackOn400(t *testing.T) {
	var responsesCalled atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"invalid tool parameter"}}]`))
		case "/responses":
			responsesCalled.Store(true)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"responses error"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewClient(Config{BaseURL: ts.URL, APIKey: "sk-test", Model: "gemini-2.5-pro"})
	_, err := client.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if responsesCalled.Load() {
		t.Fatal("generic HTTP 400 must not blindly fallback to /responses")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "invalid tool parameter" {
		t.Fatalf("expected original 400 APIError, got %v", err)
	}
}

func TestParseSSEStreamToolCallEdgeCases(t *testing.T) {
	// 1. 重复下发 function.name 不应拼接成 ssh_execssh_exec；
	// 2. index 相同或缺失但 id 不同时，多个并行工具调用不应合并；
	// 3. 空 arguments 规范为 "{}" 且序列化 Message 保留 "content" 键。
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"ssh_exec","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"ssh_exec","arguments":"\"uname\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"host_info","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	res, err := parseSSEStream(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("parseSSEStream failed: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("expected 2 distinct tool calls, got %d: %+v", len(res.ToolCalls), res.ToolCalls)
	}
	if res.ToolCalls[0].Function.Name != "ssh_exec" || res.ToolCalls[0].Function.Arguments != `{"command":"uname"}` {
		t.Fatalf("unexpected first tool call: %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].ID != "call_b" || res.ToolCalls[1].Function.Name != "host_info" {
		t.Fatalf("unexpected second tool call: %+v", res.ToolCalls[1])
	}

	prepared := prepareMessagesForProvider("https://api.openai.com/v1", "gpt-5.4", []Message{
		{Role: "assistant", Content: "", ToolCalls: res.ToolCalls},
	})
	raw, err := json.Marshal(prepared[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":""`) {
		t.Fatalf("serialized assistant message must include content key: %s", string(raw))
	}
	if prepared[0].ToolCalls[1].Function.Arguments != "{}" {
		t.Fatalf("empty arguments should normalize to {}: %q", prepared[0].ToolCalls[1].Function.Arguments)
	}
}

func TestStreamChatRetryOnStaleEOFAndTrailingUnexpectedEOF(t *testing.T) {
	// 1. 零输出前遇到断流自动重试并成功
	var attempts atomic.Int32
	tsRetry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"重试成功\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer tsRetry.Close()

	c1 := NewClient(Config{BaseURL: tsRetry.URL, APIKey: "sk-test", Model: "gpt-4o"})
	res1, err := c1.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if res1.Content != "重试成功" || attempts.Load() != 2 {
		t.Fatalf("unexpected retry result: content=%q attempts=%d", res1.Content, attempts.Load())
	}

	// 2. 已收到 finish_reason 后连接被强制截断（ErrUnexpectedEOF）应容错返回已完成结果
	tsTrunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"完整回答\"},\"finish_reason\":\"stop\"}]}\n\n"
		_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(chunk), chunk)
		_ = buf.Flush()
		// 故意不写 chunked 结束符 0\r\n\r\n 直接关闭连接触发 ErrUnexpectedEOF
	}))
	defer tsTrunc.Close()

	c2 := NewClient(Config{BaseURL: tsTrunc.URL, APIKey: "sk-test", Model: "gpt-4o"})
	res2, err := c2.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("expected completed response despite trailing EOF, got: %v", err)
	}
	if res2.Content != "完整回答" {
		t.Fatalf("unexpected content: %q", res2.Content)
	}
}

func TestStreamChatIdleTimeoutAndRunAgentDeadlineMapsToErrTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer ts.Close()

	c := NewClient(Config{
		BaseURL:           ts.URL,
		APIKey:            "sk-test",
		Model:             "gpt-4o",
		StreamIdleTimeout: 120 * time.Millisecond,
	})
	_, err := c.StreamChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout on stalled stream, got: %v", err)
	}

	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, runErr := RunAgent(expiredCtx, AgentRunConfig{
		Client:   c,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(runErr, ErrTimeout) {
		t.Fatalf("expected RunAgent deadline to map to ErrTimeout, got: %v", runErr)
	}
}
