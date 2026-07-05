package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		provider: p,
		http:     &http.Client{Transport: transport, Timeout: 5 * time.Minute},
		log:      log,
	}
}

func (c *Client) Provider() config.Provider { return c.provider }

type payload struct {
	Model       string          `json:"model"`
	Messages    []types.Message `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Tools       []types.Tool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
}

// Chat 发送非流式请求（支持 tools）。
func (c *Client) Chat(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (*types.ChatCompletionResponse, error) {
	body := payload{
		Model:       c.provider.ModelName,
		Messages:    messages,
		Stream:      false,
		Temperature: temp,
		MaxTokens:   maxTokens,
		Tools:       tools,
	}
	return c.doRequest(ctx, body)
}

func (c *Client) doRequest(ctx context.Context, body payload) (*types.ChatCompletionResponse, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.provider.BaseURL+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s call failed: %w", c.provider.Name, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s HTTP %d: %s", c.provider.Name, resp.StatusCode, truncate(string(data), 300))
	}
	var result types.ChatCompletionResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%s parse failed: %w", c.provider.Name, err)
	}
	return &result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n { return s }
	return s[:n] + "..."
}
