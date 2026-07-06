package orchestrator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fusiongate/internal/cache"
	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

func TestRunStreamFailsWhenNoWorkerResponds(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"worker","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"late worker"},"finish_reason":"stop"}]}`)
	}))
	defer worker.Close()

	var reviewerHits atomic.Int32
	reviewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reviewerHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"reviewer","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"reviewer only"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer reviewer.Close()

	cfgPath := writeTempConfig(t, fmt.Sprintf(`{
		"providers": [
			{"name":"reviewer","base_url":%q,"model_name":"reviewer-model","api_key":"key"},
			{"name":"worker","base_url":%q,"model_name":"worker-model","api_key":"key"}
		],
		"groups": [
			{"name":"coding","reviewer":"reviewer","providers":["worker"]}
		],
		"worker_timeout": "10ms"
	}`, reviewer.URL+"/v1", worker.URL+"/v1"))

	t.Setenv("FUSIONGATE_CONFIG", cfgPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}
	store := cache.New(time.Minute, logger.New("error"))
	defer store.Shutdown()

	orch := New(cfg, store, logger.New("error"))
	ch, err := orch.RunStream(t.Context(), types.ChatCompletionRequest{
		Model:    "coding",
		Messages: []types.Message{{Role: "user", Content: "write code"}},
	}, types.InternalContext{GroupName: "coding"})
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}

	var sawError bool
	var text strings.Builder
	for chunk := range ch {
		if chunk.Object == "fusiongate.error" {
			sawError = true
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}

	if !sawError {
		t.Fatalf("expected fusiongate.error when no workers respond, got text %q", text.String())
	}
	if strings.Contains(text.String(), "reviewer only") {
		t.Fatalf("must not fall back to reviewer-only output: %q", text.String())
	}
	if reviewerHits.Load() != 0 {
		t.Fatalf("reviewer was called %d times; strict fusion should not synthesize without workers", reviewerHits.Load())
	}
}

func TestRunFailsWhenNoWorkerResponds(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"worker","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"late worker"},"finish_reason":"stop"}]}`)
	}))
	defer worker.Close()

	var reviewerHits atomic.Int32
	reviewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reviewerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"reviewer","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"reviewer only"},"finish_reason":"stop"}]}`)
	}))
	defer reviewer.Close()

	cfgPath := writeTempConfig(t, fmt.Sprintf(`{
		"providers": [
			{"name":"reviewer","base_url":%q,"model_name":"reviewer-model","api_key":"key"},
			{"name":"worker","base_url":%q,"model_name":"worker-model","api_key":"key"}
		],
		"groups": [
			{"name":"coding","reviewer":"reviewer","providers":["worker"]}
		],
		"worker_timeout": "10ms"
	}`, reviewer.URL+"/v1", worker.URL+"/v1"))

	t.Setenv("FUSIONGATE_CONFIG", cfgPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}
	store := cache.New(time.Minute, logger.New("error"))
	defer store.Shutdown()

	orch := New(cfg, store, logger.New("error"))
	resp, err := orch.Run(t.Context(), types.ChatCompletionRequest{
		Model:    "coding",
		Messages: []types.Message{{Role: "user", Content: "write code"}},
	}, types.InternalContext{GroupName: "coding"})
	if err == nil {
		t.Fatalf("expected error when no workers respond, got response %#v", resp)
	}
	if !strings.Contains(err.Error(), "no expert") {
		t.Fatalf("error = %v, want no expert message", err)
	}
	if reviewerHits.Load() != 0 {
		t.Fatalf("reviewer was called %d times; strict fusion should not synthesize without workers", reviewerHits.Load())
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fusiongate-*.json")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}
	return f.Name()
}
