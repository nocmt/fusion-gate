package handler

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"fusiongate/internal/types"
)

func TestSendSSEInjectsResponsesEventType(t *testing.T) {
	rec := httptest.NewRecorder()

	sendSSE(rec, rec, "response.output_text.delta", types.EventTextDelta{
		OutputIndex:  0,
		ItemID:       "msg_1",
		ContentIndex: 0,
		Delta:        "hello",
	}, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta\n") {
		t.Fatalf("missing SSE event name in %q", body)
	}

	data := extractSSEData(t, body)
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["type"] != "response.output_text.delta" {
		t.Fatalf("payload type = %v, want response.output_text.delta", payload["type"])
	}
	if payload["delta"] != "hello" {
		t.Fatalf("payload delta = %v, want hello", payload["delta"])
	}
}

func TestInternalProgressChunksBecomeSSEComments(t *testing.T) {
	rec := httptest.NewRecorder()

	sendSSEComment(rec, rec, "FusionGate still working")

	body := rec.Body.String()
	if !strings.HasPrefix(body, ": FusionGate still working\n\n") {
		t.Fatalf("comment body = %q", body)
	}
}

func TestSendResponseInProgressUsesOfficialEvent(t *testing.T) {
	rec := httptest.NewRecorder()

	sendResponseInProgress(rec, rec, "resp_1", 123, "coding", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.in_progress\n") {
		t.Fatalf("missing in_progress event in %q", body)
	}
	data := extractSSEData(t, body)
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["type"] != "response.in_progress" {
		t.Fatalf("payload type = %v, want response.in_progress", payload["type"])
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response missing: %#v", payload)
	}
	if response["id"] != "resp_1" || response["status"] != "in_progress" {
		t.Fatalf("bad response object: %#v", response)
	}
}

func TestSendResponseCreatedUsesOfficialEventWrapper(t *testing.T) {
	rec := httptest.NewRecorder()

	sendResponseCreated(rec, rec, "resp_1", 123, "coding", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created\n") {
		t.Fatalf("missing created event in %q", body)
	}
	response := extractWrappedResponse(t, body, "response.created")
	if response["id"] != "resp_1" || response["status"] != "in_progress" {
		t.Fatalf("bad response object: %#v", response)
	}
	if response["object"] != "response" || response["model"] != "coding" {
		t.Fatalf("bad response metadata: %#v", response)
	}
}

func TestSendResponseCompletedUsesOfficialEventWrapper(t *testing.T) {
	rec := httptest.NewRecorder()

	sendResponseCompleted(rec, rec, "resp_1", 123, "coding", []types.OutputItem{{
		ID: "msg_1", Type: "message", Role: "assistant", Status: "completed",
		Content: []types.OutputContent{{Type: "output_text", Text: "hello"}},
	}}, &types.UsageDetail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.completed\n") {
		t.Fatalf("missing completed event in %q", body)
	}
	response := extractWrappedResponse(t, body, "response.completed")
	if response["id"] != "resp_1" || response["status"] != "completed" {
		t.Fatalf("bad response object: %#v", response)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("bad response output: %#v", response["output"])
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(5) {
		t.Fatalf("bad usage: %#v", response["usage"])
	}
}

func TestSendResponseFailedUsesOfficialEventWrapper(t *testing.T) {
	rec := httptest.NewRecorder()

	sendResponseFailed(rec, rec, "resp_1", 123, "coding", "upstream failed", nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed\n") {
		t.Fatalf("missing failed event in %q", body)
	}
	response := extractWrappedResponse(t, body, "response.failed")
	if response["id"] != "resp_1" || response["status"] != "failed" {
		t.Fatalf("bad response object: %#v", response)
	}
	errObj, ok := response["error"].(map[string]any)
	if !ok || errObj["message"] != "upstream failed" || errObj["type"] != "api_error" {
		t.Fatalf("bad error object: %#v", response["error"])
	}
}

func TestResponsesFunctionCallOutputConvertsToUserMessage(t *testing.T) {
	msgs, err := responsesInputToMessages([]any{
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_123",
			"output":  "command output",
		},
	})
	if err != nil {
		t.Fatalf("responsesInputToMessages failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("role = %q, want user", msgs[0].Role)
	}
	if strings.Contains(msgs[0].Content, `"role":"tool"`) || msgs[0].ToolCallID != "" {
		t.Fatalf("tool output should not become a Chat tool message: %#v", msgs[0])
	}
	if !strings.Contains(msgs[0].Content, "call_123") || !strings.Contains(msgs[0].Content, "command output") {
		t.Fatalf("content does not preserve call id and output: %q", msgs[0].Content)
	}
}

func TestInternalStreamErrorMessage(t *testing.T) {
	msg, ok := internalStreamError(types.StreamChunk{
		Object: "fusiongate.error",
		Choices: []types.ChunkChoice{{Delta: types.Delta{
			Content: "reviewer failed",
		}}},
	})
	if !ok {
		t.Fatal("expected fusiongate.error to be recognized")
	}
	if msg != "reviewer failed" {
		t.Fatalf("error message = %q, want reviewer failed", msg)
	}
}

func extractSSEData(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("no data line in %q", body)
	return ""
}

func extractWrappedResponse(t *testing.T, body, eventType string) map[string]any {
	t.Helper()
	data := extractSSEData(t, body)
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["type"] != eventType {
		t.Fatalf("payload type = %v, want %s", payload["type"], eventType)
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response missing: %#v", payload)
	}
	return response
}
