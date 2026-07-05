package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"fusiongate/internal/types"
)

// ChatStream 发送流式请求（支持 tools）。
func (c *Client) ChatStream(ctx context.Context, messages []types.Message, temp *float64, maxTokens *int, tools []types.Tool) (<-chan types.StreamChunk, error) {
	body := payload{
		Model:       c.provider.ModelName,
		Messages:    messages,
		Stream:      true,
		Temperature: temp,
		MaxTokens:   maxTokens,
		Tools:       tools,
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.provider.BaseURL+"/chat/completions", bytes.NewReader(raw))
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
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") { continue }
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" { return }
			var chunk types.StreamChunk
			if json.Unmarshal([]byte(data), &chunk) != nil { continue }
			ch <- chunk
		}
	}()
	return ch, nil
}
