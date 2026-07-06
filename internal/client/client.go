package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

type Client struct {
	provider config.Provider
	http     *http.Client
	log      *logger.Logger
}

func New(p config.Provider, log *logger.Logger) *Client {
	return &Client{
		provider: p,
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 5 * time.Minute,
		},
		log: log,
	}
}

func (c *Client) Provider() config.Provider { return c.provider }

// ---- Chat (always returns ChatCompletionResponse, handles both API formats internally) ----

func (c *Client) Chat(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (*types.ChatCompletionResponse, error) {
	if c.provider.APIType() == "responses" {
		return c.chatViaResponses(ctx, messages, temp, maxTokens, tools)
	}
	return c.chatViaChatCompletions(ctx, messages, temp, maxTokens, tools)
}

func (c *Client) ChatStream(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (<-chan types.StreamChunk, error) {
	if c.provider.APIType() == "responses" {
		return c.streamViaResponses(ctx, messages, temp, maxTokens, tools)
	}
	return c.streamViaChatCompletions(ctx, messages, temp, maxTokens, tools)
}

// ---- Chat Completions API ----

type chatPayload struct {
	Model       string          `json:"model"`
	Messages    []types.Message `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Tools       []types.Tool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
}

func (c *Client) chatViaChatCompletions(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (*types.ChatCompletionResponse, error) {
	body := chatPayload{Model: c.provider.ModelName, Messages: messages, Stream: false, Temperature: temp, MaxTokens: maxTokens, Tools: tools}
	raw, _ := json.Marshal(body)
	return c.doChatRequest(ctx, raw)
}

func (c *Client) streamViaChatCompletions(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (<-chan types.StreamChunk, error) {
	body := chatPayload{Model: c.provider.ModelName, Messages: messages, Stream: true, Temperature: temp, MaxTokens: maxTokens, Tools: tools}
	raw, _ := json.Marshal(body)
	return c.doChatStream(ctx, raw)
}

func (c *Client) doChatRequest(ctx context.Context, raw []byte) (*types.ChatCompletionResponse, error) {
	url := c.provider.ResolveEndpoint()
	c.log.Raw("UPSTREAM-REQ", "[%s] POST %s body=%s", c.provider.Name, url, truncForLog(raw))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s call failed: %w", c.provider.Name, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	c.log.Raw("UPSTREAM-RESP", "[%s] HTTP %d body=%s", c.provider.Name, resp.StatusCode, truncForLog(data))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s HTTP %d: %s", c.provider.Name, resp.StatusCode, truncate(string(data), 300))
	}
	var result types.ChatCompletionResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%s parse failed: %w", c.provider.Name, err)
	}
	return &result, nil
}

func (c *Client) doChatStream(ctx context.Context, raw []byte) (<-chan types.StreamChunk, error) {
	url := c.provider.ResolveEndpoint()
	c.log.Raw("UPSTREAM-STREAM", "[%s] POST %s body=%s", c.provider.Name, url, truncForLog(raw))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s stream failed: %w", c.provider.Name, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s HTTP %d: %s", c.provider.Name, resp.StatusCode, truncate(string(data), 300))
	}

	ch := make(chan types.StreamChunk, 32)
	go parseSSE(resp, ch)
	return ch, nil
}

// ---- Responses API (上游支持 /v1/responses 格式时使用) ----

func (c *Client) chatViaResponses(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (*types.ChatCompletionResponse, error) {
	input := messagesToInput(messages)
	body := map[string]any{
		"model":             c.provider.ModelName,
		"input":             input,
		"stream":            false,
		"max_output_tokens": maxTokens,
		"temperature":       temp,
	}
	if len(tools) > 0 {
		body["tools"] = toolsToResponseTools(tools)
	}
	raw, _ := json.Marshal(body)

	url := c.provider.ResolveEndpoint()
	c.log.Raw("UPSTREAM-REQ", "[%s] POST %s body=%s", c.provider.Name, url, truncForLog(raw))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s call failed: %w", c.provider.Name, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	c.log.Raw("UPSTREAM-RESP", "[%s] HTTP %d body=%s", c.provider.Name, resp.StatusCode, truncForLog(data))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s HTTP %d: %s", c.provider.Name, resp.StatusCode, truncate(string(data), 300))
	}

	return parseResponsesToChat(data, c.provider.ModelName), nil
}

func (c *Client) streamViaResponses(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (<-chan types.StreamChunk, error) {
	input := messagesToInput(messages)
	body := map[string]any{
		"model":             c.provider.ModelName,
		"input":             input,
		"stream":            true,
		"max_output_tokens": maxTokens,
		"temperature":       temp,
	}
	if len(tools) > 0 {
		body["tools"] = toolsToResponseTools(tools)
	}
	raw, _ := json.Marshal(body)

	url := c.provider.ResolveEndpoint()
	c.log.Raw("UPSTREAM-STREAM", "[%s] POST %s body=%s", c.provider.Name, url, truncForLog(raw))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s stream failed: %w", c.provider.Name, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s HTTP %d: %s", c.provider.Name, resp.StatusCode, truncate(string(data), 300))
	}

	ch := make(chan types.StreamChunk, 32)
	go parseResponsesSSE(resp, ch, c.provider.ModelName)
	return ch, nil
}

// ---- SSE 解析 ----

func parseSSE(resp *http.Response, ch chan<- types.StreamChunk) {
	defer close(ch)
	defer resp.Body.Close()
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		for {
			idx := indexOf(buf, []byte("\n\n"))
			if idx < 0 {
				break
			}
			line := string(buf[:idx])
			buf = buf[idx+2:]
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				return
			}
			var chunk types.StreamChunk
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			ch <- chunk
		}
		if err != nil {
			return
		}
	}
}

func parseResponsesSSE(resp *http.Response, ch chan<- types.StreamChunk, model string) {
	defer close(ch)
	defer resp.Body.Close()
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 4096)
	created := false
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		for {
			idx := indexOf(buf, []byte("\n\n"))
			if idx < 0 {
				break
			}
			pair := string(buf[:idx])
			buf = buf[idx+2:]
			event, data := parseEventPair(pair)
			if data == "[DONE]" {
				return
			}
			if event == "" {
				event = eventType(data)
			}
			switch event {
			case "response.output_text.delta", "response.text.delta":
				var d types.EventTextDelta
				if json.Unmarshal([]byte(data), &d) == nil {
					ch <- types.StreamChunk{
						ID: "resp_0", Object: "chat.completion.chunk", Model: model,
						Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{Content: d.Delta}}},
					}
				}
			case "response.output_item.added":
				var d types.EventOutputItemAdded
				if json.Unmarshal([]byte(data), &d) == nil && d.Item.Type == "function_call" {
					callID := d.Item.CallID
					if callID == "" {
						callID = d.Item.ID
					}
					ch <- types.StreamChunk{
						ID: d.Item.ID, Object: "chat.completion.chunk", Model: model,
						Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{ToolCalls: []types.ToolCall{{
							Index: d.OutputIndex, ID: callID, Type: "function",
							Function: types.FunctionCall{Name: d.Item.Name, Arguments: d.Item.Arguments},
						}}}}},
					}
				}
			case "response.function_call_arguments.delta":
				var d types.EventFunctionCallArgsDelta
				if json.Unmarshal([]byte(data), &d) == nil {
					ch <- types.StreamChunk{
						ID: d.ItemID, Object: "chat.completion.chunk", Model: model,
						Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{ToolCalls: []types.ToolCall{{
							Index: d.OutputIndex, Type: "function",
							Function: types.FunctionCall{Arguments: d.Delta},
						}}}}},
					}
				}
			case "response.completed":
				var d types.EventResponseCompleted
				if json.Unmarshal([]byte(data), &d) == nil && d.Usage != nil {
					ch <- types.StreamChunk{
						ID: d.ID, Object: "chat.completion.chunk", Model: model,
						Choices: []types.ChunkChoice{{Index: 0, FinishReason: strPtr("stop")}},
						Usage: types.Usage{
							PromptTokens: d.Usage.InputTokens, CompletionTokens: d.Usage.OutputTokens,
							TotalTokens: d.Usage.TotalTokens,
						},
					}
				}
			case "response.created":
				if !created {
					var d types.EventResponseCreated
					if json.Unmarshal([]byte(data), &d) == nil {
						ch <- types.StreamChunk{ID: d.ID, Object: "chat.completion.chunk", Model: d.Model,
							Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{Role: "assistant"}}},
						}
					}
					created = true
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func parseEventPair(s string) (event, data string) {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	return
}

func eventType(data string) string {
	var d struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
	}
	return d.Type
}

// ---- 格式转换 ----

func messagesToInput(msgs []types.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"type": "message", "role": m.Role, "content": m.Content})
	}
	return out
}

func toolsToResponseTools(tools []types.Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" {
			out = append(out, map[string]any{
				"type": "function", "name": t.Function.Name,
				"description": t.Function.Description, "parameters": t.Function.Parameters,
			})
		}
	}
	return out
}

func parseResponsesToChat(raw []byte, model string) *types.ChatCompletionResponse {
	var r struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		Output    []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *types.UsageDetail `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return &types.ChatCompletionResponse{ID: "error", Object: "chat.completion"}
	}

	var content string
	var toolCalls []types.ToolCall
	for _, o := range r.Output {
		if o.Type == "message" {
			for _, c := range o.Content {
				if c.Type == "output_text" {
					content += c.Text
				}
			}
		} else if o.Type == "function_call" {
			callID := o.CallID
			if callID == "" {
				callID = o.ID
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID: callID, Type: "function",
				Function: types.FunctionCall{Name: o.Name, Arguments: o.Arguments},
			})
		}
	}

	resp := &types.ChatCompletionResponse{
		ID: r.ID, Object: "chat.completion", Created: r.CreatedAt, Model: model,
		Choices: []types.Choice{{Index: 0, Message: types.Message{Role: "assistant", Content: content, ToolCalls: toolCalls}, FinishReason: "stop"}},
	}
	if r.Usage != nil {
		resp.Usage = types.Usage{
			PromptTokens: r.Usage.InputTokens, CompletionTokens: r.Usage.OutputTokens,
			TotalTokens: r.Usage.TotalTokens,
		}
	}
	return resp
}

// ---- 工具 ----

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncForLog(raw []byte) string {
	s := string(raw)
	if len(s) > 4000 {
		return s[:4000] + "...[truncated]"
	}
	return s
}

func indexOf(s, sub []byte) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if bytes.Equal(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func strPtr(s string) *string { return &s }
