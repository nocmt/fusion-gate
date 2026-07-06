package client

import (
	"fusiongate/internal/types"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseResponsesSSEConvertsOutputTextAndFunctionCall(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created\ndata: {\"type\":\"response.created\",\"id\":\"resp_1\",\"model\":\"gpt-test\"}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"run_command\",\"arguments\":\"\"}}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"cmd\\\":\"}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"\\\"pwd\\\"}\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"id\":\"resp_1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}\n\n",
			"data: [DONE]\n\n",
		}, ""))),
	}

	ch := make(chan types.StreamChunk, 8)
	parseResponsesSSE(resp, ch, "fallback-model")

	var text string
	var toolName string
	var toolArgs string
	var usageTotal int
	for chunk := range ch {
		for _, choice := range chunk.Choices {
			text += choice.Delta.Content
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Function.Name != "" {
					toolName = tc.Function.Name
				}
				toolArgs += tc.Function.Arguments
			}
		}
		if chunk.Usage.TotalTokens > 0 {
			usageTotal = chunk.Usage.TotalTokens
		}
	}

	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}
	if toolName != "run_command" {
		t.Fatalf("tool name = %q, want run_command", toolName)
	}
	if toolArgs != `{"cmd":"pwd"}` {
		t.Fatalf("tool args = %q, want pwd JSON", toolArgs)
	}
	if usageTotal != 7 {
		t.Fatalf("usage total = %d, want 7", usageTotal)
	}
}
