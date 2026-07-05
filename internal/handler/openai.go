package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/orchestrator"
	"fusiongate/internal/types"
)

// HandleChatCompletions 处理 POST /v1/chat/completions（OpenAI 格式）
func HandleChatCompletions(
	cfg *config.Config,
	orch *orchestrator.Orchestrator,
	log *logger.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req types.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, "解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body.Close()

		groupName := resolveGroup(req.XGroup, req.Model, cfg)
		log.Info("ChatCompletions  model=%s  group=%s  stream=%v  tools=%d",
			req.Model, groupName, req.Stream, len(req.Tools))

		ictx := types.InternalContext{
			GroupName:   groupName,
			Tools:       req.Tools,
			Stream:      req.Stream,
			Temperature: req.Temperature,
			MaxTokens:   req.MaxTokens,
			TopP:        req.TopP,
		}

		if req.Stream {
			handleChatStream(w, r, req, ictx, orch, log)
		} else {
			handleChatNonStream(w, r, req, ictx, orch, log)
		}
	}
}

func handleChatNonStream(
	w http.ResponseWriter, r *http.Request,
	req types.ChatCompletionRequest, ictx types.InternalContext,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	resp, err := orch.Run(ctx, req, ictx)
	if err != nil {
		log.Error("融合失败: %v", err)
		writeErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func handleChatStream(
	w http.ResponseWriter, r *http.Request,
	req types.ChatCompletionRequest, ictx types.InternalContext,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok { writeErr(w, "不支持流式", http.StatusInternalServerError); return }
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, err := orch.RunStream(ctx, req, ictx)
	if err != nil { writeErr(w, err.Error(), http.StatusInternalServerError); return }
	for chunk := range ch {
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(raw))
		flusher.Flush()
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// HandleResponses 处理 POST /v1/responses（Codex 格式）
func HandleResponses(
	cfg *config.Config,
	orch *orchestrator.Orchestrator,
	log *logger.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeResponsesErr(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body); r.Body.Close()

		var req types.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeResponsesErr(w, "解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}

		groupName := resolveGroup(req.XGroup, req.Model, cfg)
		modelName := cfg.ResolveModelName(req.Model)

		log.Info("Responses  model=%s  group=%s  stream=%v  tools=%d",
			req.Model, groupName, req.Stream, len(req.Tools))

		// 转成 ChatCompletions 格式
		chatReq, err := responsesReqToChat(req)
		if err != nil {
			writeResponsesErr(w, err.Error(), http.StatusBadRequest)
			return
		}

		ictx := types.InternalContext{
			GroupName:   groupName,
			Tools:       chatToolsFromResponses(req.Tools),
			Stream:      req.Stream,
			Temperature: req.Temperature,
			MaxTokens:   req.MaxOutputTokens,
			TopP:        req.TopP,
		}

		if req.Stream {
			handleResponsesStream(w, r, chatReq, ictx, modelName, orch, log)
		} else {
			handleResponsesNonStream(w, r, chatReq, ictx, modelName, orch, log)
		}
	}
}

func handleResponsesNonStream(
	w http.ResponseWriter, r *http.Request,
	chatReq types.ChatCompletionRequest, ictx types.InternalContext,
	modelName string, orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	result, err := orch.Run(ctx, chatReq, ictx)
	if err != nil {
		log.Error("融合失败: %v", err)
		writeResponsesErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := chatToResponses(*result, modelName)
	resp.ID = config.NextID()
	raw, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func handleResponsesStream(
	w http.ResponseWriter, r *http.Request,
	chatReq types.ChatCompletionRequest, ictx types.InternalContext,
	modelName string, orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok { writeResponsesErr(w, "SSE 不支持", http.StatusInternalServerError); return }
	setSSEHeaders(w)

	respID := config.NextID()
	respCreated := time.Now().Unix()

	sendSSE(w, flusher, "response.created", types.EventResponseCreated{
		ID: respID, Object: "response", CreatedAt: respCreated,
		Model: modelName, Status: "in_progress",
	})

	ch, err := orch.RunStream(ctx, chatReq, ictx)
	if err != nil {
		sendSSE(w, flusher, "response.failed", types.EventResponseFailed{
			ID: respID, Status: "failed",
			Error: &types.ErrorDetail{Message: err.Error(), Type: "api_error"},
		})
		return
	}

	msgID := "msg_" + respID
	textStarted := false
	var fullText strings.Builder
	var outputItems []types.OutputItem

	for chunk := range ch {
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				if !textStarted {
					// message output_item.added
					sendSSE(w, flusher, "response.output_item.added", types.EventOutputItemAdded{
						OutputIndex: 0,
						Item: types.OutputItem{
							ID: msgID, Type: "message", Role: "assistant", Status: "in_progress",
							Content: []types.OutputContent{{Type: "output_text", Text: "", Annotations: []types.Annotation{}}},
						},
					})
					sendSSE(w, flusher, "response.content_part.added", types.EventContentPartAdded{
						OutputIndex: 0, ItemID: msgID, ContentIndex: 0,
						Part: types.OutputContent{Type: "output_text", Text: "", Annotations: []types.Annotation{}},
					})
					textStarted = true
				}
				fullText.WriteString(c.Delta.Content)
				sendSSE(w, flusher, "response.text.delta", types.EventTextDelta{
					OutputIndex: 0, ItemID: msgID, ContentIndex: 0, Delta: c.Delta.Content,
				})
			}
		}
	}

	// 完成
	if textStarted {
		sendSSE(w, flusher, "response.text.done", types.EventTextDone{
			OutputIndex: 0, ItemID: msgID, ContentIndex: 0, Text: fullText.String(),
		})
		sendSSE(w, flusher, "response.output_item.done", types.EventOutputItemDone{
			OutputIndex: 0,
			Item: types.OutputItem{
				ID: msgID, Type: "message", Role: "assistant", Status: "completed",
				Content: []types.OutputContent{{Type: "output_text", Text: fullText.String()}},
			},
		})
		outputItems = append(outputItems, types.OutputItem{
			ID: msgID, Type: "message", Role: "assistant", Status: "completed",
			Content: []types.OutputContent{{Type: "output_text", Text: fullText.String()}},
		})
	}

	sendSSE(w, flusher, "response.completed", types.EventResponseCompleted{
		ID: respID, Object: "response", CreatedAt: respCreated,
		Model: modelName, Status: "completed", Output: outputItems,
		Usage: &types.UsageDetail{},
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---- /v1/models ----

func HandleModels(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var data []types.ModelCard
		for _, g := range cfg.Groups {
			data = append(data, types.ModelCard{ID: g.Name, Object: "model", OwnedBy: "fusiongate"})
		}
		for _, p := range cfg.Providers {
			data = append(data, types.ModelCard{ID: p.Name, Object: "model", OwnedBy: p.Name})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

func HandleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}
}

// ---- 辅助 ----

func resolveGroup(xGroup, model string, cfg *config.Config) string {
	if xGroup != "" {
		if _, ok := cfg.Group(xGroup); ok { return xGroup }
	}
	for _, g := range cfg.Groups {
		if g.Name == model { return g.Name }
	}
	if len(cfg.Groups) > 0 { return cfg.Groups[0].Name }
	return ""
}

func writeErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errorTypeForCode(code),
			"code":    code,
		},
	})
}

func writeResponsesErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errorTypeForCode(code),
			"code":    errorCodeForCode(code),
		},
	})
}

func errorTypeForCode(code int) string {
	switch {
	case code == 400: return "invalid_request_error"
	case code == 401 || code == 403: return "authentication_error"
	case code == 404: return "not_found_error"
	case code == 429: return "rate_limit_error"
	case code >= 500: return "api_error"
	default: return "api_error"
	}
}

func errorCodeForCode(code int) string {
	switch {
	case code == 400: return "invalid_request"
	case code == 401: return "invalid_api_key"
	case code == 429: return "rate_limit_exceeded"
	default: return "server_error"
	}
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	raw, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(raw))
	flusher.Flush()
}

// ---- 格式转换（内联，避免依赖 converter 包）----

func responsesReqToChat(req types.ResponsesRequest) (types.ChatCompletionRequest, error) {
	cc := types.ChatCompletionRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxOutputTokens,
		TopP:        req.TopP,
	}
	msgs, err := responsesInputToMessages(req.Input)
	if err != nil { return cc, err }
	if req.Instructions != "" {
		msgs = append([]types.Message{{Role: "system", Content: req.Instructions}}, msgs...)
	}
	cc.Messages = msgs
	return cc, nil
}

func responsesInputToMessages(input any) ([]types.Message, error) {
	if input == nil { return nil, nil }
	switch v := input.(type) {
	case string:
		if v == "" { return nil, nil }
		return []types.Message{{Role: "user", Content: v}}, nil
	default:
		raw, _ := json.Marshal(input)
		return parseInputArray(raw)
	}
}

func parseInputArray(raw json.RawMessage) ([]types.Message, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil { return nil, fmt.Errorf("input 格式错误: %w", err) }
	var msgs []types.Message
	for _, item := range arr {
		var typ struct{ Type string }
		json.Unmarshal(item, &typ)
		switch typ.Type {
		case "message":
			var im types.InputMessage
			json.Unmarshal(item, &im)
			msgs = append(msgs, types.Message{Role: im.Role, Content: extractText(im.Content)})
		case "function_call_output":
			var fo types.FunctionCallOutput
			json.Unmarshal(item, &fo)
			msgs = append(msgs, types.Message{Role: "tool", ToolCallID: fo.CallID, Content: fo.Output})
		default:
			continue
		}
	}
	return msgs, nil
}

func extractText(content any) string {
	switch v := content.(type) {
	case string: return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" { sb.WriteString(t) }
			}
		}
		return sb.String()
	}
	return ""
}

func chatToResponses(cc types.ChatCompletionResponse, model string) types.ResponsesResponse {
	output := make([]types.OutputItem, 0, len(cc.Choices))
	for i, ch := range cc.Choices {
		if ch.Message.Content != "" {
			output = append(output, types.OutputItem{
				ID: fmt.Sprintf("msg_%d", i+1), Type: "message", Role: "assistant", Status: "completed",
				Content: []types.OutputContent{{Type: "output_text", Text: ch.Message.Content}},
			})
		}
		for _, tc := range ch.Message.ToolCalls {
			output = append(output, types.OutputItem{
				ID: tc.ID, Type: "function_call", CallID: tc.ID,
				Name: tc.Function.Name, Arguments: tc.Function.Arguments, Status: "completed",
			})
		}
	}
	resp := types.ResponsesResponse{
		ID: cc.ID, Object: "response", CreatedAt: cc.Created,
		Model: model, Status: "completed", Output: output,
	}
	if cc.Usage.TotalTokens > 0 {
		resp.Usage = &types.UsageDetail{
			InputTokens: cc.Usage.PromptTokens, OutputTokens: cc.Usage.CompletionTokens,
			TotalTokens: cc.Usage.TotalTokens,
		}
	}
	return resp
}

func chatToolsFromResponses(items []types.ToolItem) []types.Tool {
	out := make([]types.Tool, 0, len(items))
	for _, it := range items {
		if it.Type == "function" {
			out = append(out, types.Tool{
				Type: "function",
				Function: types.FunctionDef{
					Name: it.Name, Description: it.Description, Parameters: it.Parameters,
				},
			})
		}
	}
	return out
}
