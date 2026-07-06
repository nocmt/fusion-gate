package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/orchestrator"
	"fusiongate/internal/session"
	"fusiongate/internal/types"
)

// ---- /v1/chat/completions ----

func HandleChatCompletions(
	cfg *config.Config,
	orch *orchestrator.Orchestrator,
	sess *session.Store,
	log *logger.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bodyRaw, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req types.ChatCompletionRequest
		if err := json.Unmarshal(bodyRaw, &req); err != nil {
			writeErr(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}

		groupName := resolveGroup(req.XGroup, req.Model, cfg)
		log.Info("ChatCompletions model=%s group=%s stream=%v tools=%d msgs=%d",
			req.Model, groupName, req.Stream, len(req.Tools), len(req.Messages))
		log.Raw("REQ-CHAT", "model=%s messages=%d tools=%d body=%s",
			req.Model, len(req.Messages), len(req.Tools), truncForLog(bodyRaw))

		ictx := types.InternalContext{
			GroupName: groupName, Tools: req.Tools,
			Stream: req.Stream, Temperature: req.Temperature,
			MaxTokens: req.MaxTokens, TopP: req.TopP,
		}

		if req.Stream {
			handleChatStream(w, r, req, ictx, orch, log)
		} else {
			handleChatNonStream(w, r, req, ictx, orch, log)
		}
	}
}

func handleChatNonStream(w http.ResponseWriter, r *http.Request,
	req types.ChatCompletionRequest, ictx types.InternalContext,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	resp, err := orch.Run(ctx, req, ictx)
	if err != nil {
		log.Error("fusion failed: %v", err)
		writeErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if resp.ServiceTier == "" {
		resp.ServiceTier = "default"
	}
	raw, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func handleChatStream(w http.ResponseWriter, r *http.Request,
	req types.ChatCompletionRequest, ictx types.InternalContext,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, err := orch.RunStream(ctx, req, ictx)
	if err != nil {
		writeErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var totalUsage types.Usage
	chunkCount := 0
	for chunk := range ch {
		if msg, ok := internalProgressMessage(chunk); ok {
			sendSSEComment(w, flusher, msg)
			continue
		}
		chunkCount++
		// 累积 usage
		if chunk.Usage.TotalTokens > 0 {
			totalUsage = chunk.Usage
		}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(raw))
		flusher.Flush()
	}

	// 发送末尾 chunk（含 usage + finish_reason）
	if chunkCount == 0 {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	final := types.StreamChunk{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		Object: "chat.completion.chunk", Created: time.Now().Unix(),
		Model: req.Model,
		Choices: []types.ChunkChoice{{
			Index: 0, Delta: types.Delta{},
			FinishReason: strPtr("stop"),
		}},
		Usage: totalUsage,
	}
	raw, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", string(raw))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---- /v1/responses ----

func HandleResponses(
	cfg *config.Config,
	orch *orchestrator.Orchestrator,
	sess *session.Store,
	log *logger.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeResponsesErr(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req types.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeResponsesErr(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}

		groupName := resolveGroup(req.XGroup, req.Model, cfg)
		modelName := cfg.ResolveModelName(req.Model)

		// previous_response_id 会话续接
		sessionID := ""
		if req.PreviousResponseID != "" {
			if e := sess.FindByPrevResponse(req.PreviousResponseID); e != nil {
				sessionID = e.ID
				log.Info("session resume prev=%s sid=%s", req.PreviousResponseID, sessionID)
			}
		}
		if sessionID == "" {
			convID := extractConvID(req)
			if e := sess.FindByConv(convID); e != nil {
				sessionID = e.ID
			}
		}
		if sessionID == "" {
			sessionID = sess.Register(extractConvID(req), groupName)
		}

		log.Info("Responses model=%s group=%s stream=%v tools=%d sid=%s",
			req.Model, groupName, req.Stream, len(req.Tools), sessionID)
		log.Raw("REQ-RESPONSES", "model=%s tools=%d prev=%s input=%s",
			req.Model, len(req.Tools), req.PreviousResponseID, truncForLog(body))

		chatReq, err := responsesReqToChat(req)
		if err != nil {
			writeResponsesErr(w, err.Error(), http.StatusBadRequest)
			return
		}

		ictx := types.InternalContext{
			GroupName:   groupName,
			Tools:       chatToolsFromResponses(req.Tools),
			Stream:      req.Stream,
			Temperature: req.Temperature, MaxTokens: req.MaxOutputTokens, TopP: req.TopP,
		}

		if req.Stream {
			handleResponsesStream(w, r, chatReq, ictx, modelName, sessionID, sess, orch, log)
		} else {
			handleResponsesNonStream(w, r, chatReq, ictx, modelName, sessionID, sess, orch, log)
		}
	}
}

func handleResponsesNonStream(
	w http.ResponseWriter, r *http.Request,
	chatReq types.ChatCompletionRequest, ictx types.InternalContext,
	modelName, sessionID string, sess *session.Store,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	result, err := orch.Run(ctx, chatReq, ictx)
	if err != nil {
		log.Error("fusion failed: %v", err)
		writeResponsesErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respID := config.NextID()
	resp := chatToResponses(*result, modelName)
	resp.ID = respID
	resp.ResponseID = respID

	// 记录映射：下次带 previous_response_id 可找回
	sess.UpdateState(sessionID, "main", result.ID, "")
	if result.ServiceTier == "" {
		result.ServiceTier = "default"
	}

	raw, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

// toolCallState aggregates a single function_call across ChatCompletion stream chunks.
type toolCallState struct {
	id          string
	name        string
	args        strings.Builder
	itemID      string
	outputIndex int
	added       bool
}

func handleResponsesStream(
	w http.ResponseWriter, r *http.Request,
	chatReq types.ChatCompletionRequest, ictx types.InternalContext,
	modelName, sessionID string, sess *session.Store,
	orch *orchestrator.Orchestrator, log *logger.Logger,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeResponsesErr(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)

	respID := config.NextID()
	respCreated := time.Now().Unix()

	start := time.Now()
	sendCount := 0
	send := func(event string, data any) {
		sendSSE(w, flusher, event, data, log)
		sendCount++
	}

	sendResponseCreated(w, flusher, respID, respCreated, modelName, log)
	sendCount++

	ch, err := orch.RunStream(ctx, chatReq, ictx)
	if err != nil {
		sendResponseFailed(w, flusher, respID, respCreated, modelName, err.Error(), log)
		sendCount++
		log.Info("Responses stream failed: %v (elapsed %.2fs, events=%d)", err, time.Since(start).Seconds(), sendCount)
		return
	}

	msgID := "msg_" + respID
	textStarted := false
	var fullText strings.Builder
	var outputItems []types.OutputItem
	var totalInput, totalOutput int

	// Tool-call aggregation state. Key is the tool call index inside the assistant response.
	toolCalls := make(map[int]*toolCallState)
	nextOutputIndex := 0
	getToolBaseIndex := func() int {
		if textStarted {
			return 1
		}
		return 0
	}

	for chunk := range ch {
		select {
		case <-ctx.Done():
			log.Info("Responses stream client disconnected: sid=%s elapsed=%.2fs events=%d", sessionID, time.Since(start).Seconds(), sendCount)
			return
		default:
		}
		if msg, ok := internalStreamError(chunk); ok {
			sendResponseFailed(w, flusher, respID, respCreated, modelName, msg, log)
			sendCount++
			log.Error("Responses stream upstream failed: sid=%s err=%s elapsed=%.2fs events=%d", sessionID, msg, time.Since(start).Seconds(), sendCount)
			return
		}
		if msg, ok := internalProgressMessage(chunk); ok {
			sendResponseInProgress(w, flusher, respID, respCreated, modelName, log)
			sendCount++
			log.Debug("Responses progress: sid=%s %s", sessionID, msg)
			continue
		}

		if chunk.Usage.TotalTokens > 0 {
			totalInput = chunk.Usage.PromptTokens
			totalOutput = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				if !textStarted {
					send("response.output_item.added", types.EventOutputItemAdded{
						OutputIndex: nextOutputIndex,
						Item: types.OutputItem{
							ID: msgID, Type: "message", Role: "assistant", Status: "in_progress",
							Content: []types.OutputContent{{Type: "output_text", Text: "", Annotations: []types.Annotation{}}},
						},
					})
					send("response.content_part.added", types.EventContentPartAdded{
						OutputIndex: nextOutputIndex, ItemID: msgID, ContentIndex: 0,
						Part: types.OutputContent{Type: "output_text", Text: "", Annotations: []types.Annotation{}},
					})
					textStarted = true
					nextOutputIndex++
				}
				fullText.WriteString(c.Delta.Content)
				send("response.output_text.delta", types.EventTextDelta{
					OutputIndex: 0, ItemID: msgID, ContentIndex: 0, Delta: c.Delta.Content,
				})
			}

			for _, tc := range c.Delta.ToolCalls {
				state, ok := toolCalls[tc.Index]
				if !ok {
					base := getToolBaseIndex()
					state = &toolCallState{
						id:          tc.ID,
						outputIndex: base + tc.Index,
					}
					toolCalls[tc.Index] = state
				}
				if tc.ID != "" {
					state.id = tc.ID
				}
				if state.id == "" {
					state.id = fmt.Sprintf("call_%d", tc.Index)
				}
				if state.itemID == "" {
					state.itemID = "fc_" + state.id
				}
				if tc.Function.Name != "" {
					state.name = tc.Function.Name
				}
				if !state.added && state.name != "" {
					send("response.output_item.added", types.EventOutputItemAdded{
						OutputIndex: state.outputIndex,
						Item: types.OutputItem{
							ID: state.itemID, Type: "function_call", Status: "in_progress",
							CallID: state.id, Name: state.name, Arguments: "",
						},
					})
					state.added = true
				}
				if tc.Function.Arguments != "" {
					state.args.WriteString(tc.Function.Arguments)
					send("response.function_call_arguments.delta", types.EventFunctionCallArgsDelta{
						OutputIndex: state.outputIndex, ItemID: state.itemID,
						Delta: tc.Function.Arguments,
					})
				}
			}
		}
	}

	if textStarted {
		cleanText := strings.TrimSpace(fullText.String())
		send("response.output_text.done", types.EventTextDone{
			OutputIndex: 0, ItemID: msgID, ContentIndex: 0, Text: cleanText,
		})
		send("response.content_part.done", types.EventContentPartDone{
			OutputIndex: 0, ItemID: msgID, ContentIndex: 0,
			Part: types.OutputContent{Type: "output_text", Text: cleanText},
		})
		send("response.output_item.done", types.EventOutputItemDone{
			OutputIndex: 0,
			Item: types.OutputItem{
				ID: msgID, Type: "message", Role: "assistant", Status: "completed",
				Content: []types.OutputContent{{Type: "output_text", Text: cleanText}},
			},
		})
		outputItems = append(outputItems, types.OutputItem{
			ID: msgID, Type: "message", Role: "assistant", Status: "completed",
			Content: []types.OutputContent{{Type: "output_text", Text: cleanText}},
		})
	}

	// Finalize any tool calls in output_index order.
	var toolStates []*toolCallState
	for _, state := range toolCalls {
		toolStates = append(toolStates, state)
	}
	sort.Slice(toolStates, func(i, j int) bool { return toolStates[i].outputIndex < toolStates[j].outputIndex })
	for _, state := range toolStates {
		args := state.args.String()
		if !state.added {
			send("response.output_item.added", types.EventOutputItemAdded{
				OutputIndex: state.outputIndex,
				Item: types.OutputItem{
					ID: state.itemID, Type: "function_call", Status: "in_progress",
					CallID: state.id, Name: state.name, Arguments: "",
				},
			})
		}
		send("response.function_call_arguments.done", types.EventFunctionCallArgsDone{
			OutputIndex: state.outputIndex, ItemID: state.itemID, Arguments: args,
		})
		send("response.output_item.done", types.EventOutputItemDone{
			OutputIndex: state.outputIndex,
			Item: types.OutputItem{
				ID: state.itemID, Type: "function_call", Status: "completed",
				CallID: state.id, Name: state.name, Arguments: args,
			},
		})
		outputItems = append(outputItems, types.OutputItem{
			ID: state.itemID, Type: "function_call", Status: "completed",
			CallID: state.id, Name: state.name, Arguments: args,
		})
	}

	if !textStarted && len(toolStates) == 0 {
		msg := "upstream stream ended without output"
		sendResponseFailed(w, flusher, respID, respCreated, modelName, msg, log)
		sendCount++
		log.Error("Responses stream empty: sid=%s elapsed=%.2fs events=%d", sessionID, time.Since(start).Seconds(), sendCount)
		return
	}

	usage := &types.UsageDetail{}
	if totalInput+totalOutput > 0 {
		usage = &types.UsageDetail{
			InputTokens: totalInput, OutputTokens: totalOutput,
			TotalTokens: totalInput + totalOutput,
		}
	}

	sendResponseCompleted(w, flusher, respID, respCreated, modelName, outputItems, usage, log)
	sendCount++

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// 记录映射
	sess.UpdateState(sessionID, "main", respID, "")
	log.Info("Responses stream completed: sid=%s elapsed=%.2fs events=%d output_items=%d", sessionID, time.Since(start).Seconds(), sendCount, len(outputItems))
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
		if _, ok := cfg.Group(xGroup); ok {
			return xGroup
		}
	}
	for _, g := range cfg.Groups {
		if g.Name == model {
			return g.Name
		}
	}
	if len(cfg.Groups) > 0 {
		return cfg.Groups[0].Name
	}
	return ""
}

func writeErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": errorTypeForCode(code), "code": code},
	})
}

func writeResponsesErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": errorTypeForCode(code), "code": errorCodeForCode(code)},
	})
}

func errorTypeForCode(code int) string {
	switch {
	case code == 400:
		return "invalid_request_error"
	case code == 401 || code == 403:
		return "authentication_error"
	case code == 404:
		return "not_found_error"
	case code == 429:
		return "rate_limit_error"
	case code >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

func errorCodeForCode(code int) string {
	switch {
	case code == 400:
		return "invalid_request"
	case code == 401:
		return "invalid_api_key"
	case code == 429:
		return "rate_limit_exceeded"
	default:
		return "server_error"
	}
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any, log *logger.Logger) {
	raw := responseEventPayload(event, data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(raw))
	flusher.Flush()
	if log != nil {
		log.Debug("SSE-OUT event=%s bytes=%d", event, len(raw))
	}
}

func responseEventPayload(event string, data any) []byte {
	raw, _ := json.Marshal(data)
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return raw
	}
	if _, ok := payload["type"]; !ok {
		payload["type"] = event
	}
	raw, _ = json.Marshal(payload)
	return raw
}

func sendSSEComment(w http.ResponseWriter, flusher http.Flusher, msg string) {
	msg = strings.ReplaceAll(msg, "\n", " ")
	fmt.Fprintf(w, ": %s\n\n", strings.TrimSpace(msg))
	flusher.Flush()
}

func sendResponseCreated(w http.ResponseWriter, flusher http.Flusher, id string, createdAt int64, model string, log *logger.Logger) {
	sendSSE(w, flusher, "response.created", responseLifecyclePayload(id, createdAt, model, "in_progress", []types.OutputItem{}, nil, nil), log)
}

func sendResponseInProgress(w http.ResponseWriter, flusher http.Flusher, id string, createdAt int64, model string, log *logger.Logger) {
	sendSSE(w, flusher, "response.in_progress", responseLifecyclePayload(id, createdAt, model, "in_progress", []types.OutputItem{}, nil, nil), log)
}

func sendResponseCompleted(w http.ResponseWriter, flusher http.Flusher, id string, createdAt int64, model string, output []types.OutputItem, usage *types.UsageDetail, log *logger.Logger) {
	sendSSE(w, flusher, "response.completed", responseLifecyclePayload(id, createdAt, model, "completed", output, usage, nil), log)
}

func sendResponseFailed(w http.ResponseWriter, flusher http.Flusher, id string, createdAt int64, model string, message string, log *logger.Logger) {
	sendSSE(w, flusher, "response.failed", responseLifecyclePayload(id, createdAt, model, "failed", []types.OutputItem{}, nil, &types.ErrorDetail{
		Message: message,
		Type:    "api_error",
	}), log)
}

func responseLifecyclePayload(id string, createdAt int64, model string, status string, output []types.OutputItem, usage *types.UsageDetail, err *types.ErrorDetail) map[string]any {
	response := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": createdAt,
		"model":      model,
		"status":     status,
		"output":     output,
	}
	if usage != nil {
		response["usage"] = usage
	}
	if err != nil {
		response["error"] = err
	}
	return map[string]any{"response": response}
}

func internalProgressMessage(chunk types.StreamChunk) (string, bool) {
	switch chunk.Object {
	case "fusiongate.heartbeat":
		return "fusiongate heartbeat", true
	case "fusiongate.status":
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				return c.Delta.Content, true
			}
		}
		return "fusiongate status", true
	default:
		return "", false
	}
}

func internalStreamError(chunk types.StreamChunk) (string, bool) {
	if chunk.Object != "fusiongate.error" {
		return "", false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			return c.Delta.Content, true
		}
	}
	return "upstream stream failed", true
}

// ---- 格式转换 ----

func responsesReqToChat(req types.ResponsesRequest) (types.ChatCompletionRequest, error) {
	cc := types.ChatCompletionRequest{Model: req.Model, Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxOutputTokens, TopP: req.TopP}
	msgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		return cc, err
	}
	if req.Instructions != "" {
		msgs = append([]types.Message{{Role: "system", Content: req.Instructions}}, msgs...)
	}
	cc.Messages = msgs
	return cc, nil
}

func responsesInputToMessages(input any) ([]types.Message, error) {
	if input == nil {
		return nil, nil
	}
	switch v := input.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		return []types.Message{{Role: "user", Content: v}}, nil
	default:
		raw, _ := json.Marshal(input)
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return nil, fmt.Errorf("input format error")
		}
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
				msgs = append(msgs, types.Message{Role: "user", Content: formatToolOutputForChat(fo)})
			}
		}
		return msgs, nil
	}
}

func formatToolOutputForChat(output types.FunctionCallOutput) string {
	return fmt.Sprintf("[Tool result for call_id=%s]\n%s", output.CallID, output.Output)
}

func extractText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
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
	resp := types.ResponsesResponse{ID: cc.ID, Object: "response", CreatedAt: cc.Created, Model: model, Status: "completed", Output: output}
	if cc.Usage.TotalTokens > 0 {
		resp.Usage = &types.UsageDetail{InputTokens: cc.Usage.PromptTokens, OutputTokens: cc.Usage.CompletionTokens, TotalTokens: cc.Usage.TotalTokens}
	}
	return resp
}

func chatToolsFromResponses(items []types.ToolItem) []types.Tool {
	out := make([]types.Tool, 0, len(items))
	for _, it := range items {
		if it.Type == "function" {
			out = append(out, types.Tool{Type: "function", Function: types.FunctionDef{Name: it.Name, Description: it.Description, Parameters: it.Parameters}})
		}
	}
	return out
}

func extractConvID(req types.ResponsesRequest) string {
	switch v := req.Conversation.(type) {
	case string:
		return v
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

func strPtr(s string) *string { return &s }

func truncForLog(raw []byte) string {
	s := string(raw)
	if len(s) > 4000 {
		return s[:4000] + "...[truncated]"
	}
	return s
}
