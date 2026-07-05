package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"fusiongate/internal/cache"
	"fusiongate/internal/client"
	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

// Orchestrator is the multi-model fusion engine.
//
// Flow:
//   1. Classify task complexity (simple → direct, complex → multi-worker)
//   2. Complex: workers provide analysis, reviewer synthesizes + executes tools
//   3. Response cache avoids redundant API calls for repeated queries
type Orchestrator struct {
	cfg     *config.Config
	clients map[string]*client.Client
	cache   *cache.Store
	log     *logger.Logger
}

func New(cfg *config.Config, c *cache.Store, log *logger.Logger) *Orchestrator {
	clients := make(map[string]*client.Client, len(cfg.Providers))
	for _, p := range cfg.Providers {
		clients[p.Name] = client.New(p, log)
	}
	return &Orchestrator{cfg: cfg, clients: clients, cache: c, log: log}
}

func (o *Orchestrator) Run(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (*types.ChatCompletionResponse, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("group %q reviewer not found", ictx.GroupName)
	}

	// 缓存去重
	ck := cache.Key(req.Messages, ictx.Tools, ictx.GroupName, req.Model)
	if cached, ok := o.cache.Get(ck); ok {
		o.log.Info("缓存命中 (hit=%d)", 1)
		return cached, nil
	}

	if len(group.Providers) == 0 {
		resp, err := reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
		if err == nil { o.cache.Set(ck, resp) }
		return resp, err
	}

	// 自适应复杂度
	complexity := o.classifyComplexity(ctx, req, reviewerCli)
	o.log.Info("complexity=%s", complexity)

	if complexity == "simple" {
		resp, err := reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
		if err == nil { o.cache.Set(ck, resp) }
		return resp, err
	}

	// 复杂：多子模型融合
	workerResults := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	if len(workerResults) == 0 {
		resp, err := reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
		if err == nil { o.cache.Set(ck, resp) }
		return resp, err
	}

	synthMsgs := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	resp, err := reviewerCli.Chat(ctx, synthMsgs, req.Temperature, req.MaxTokens, ictx.Tools)
	if err != nil { return nil, fmt.Errorf("reviewer synthesis failed: %w", err) }
	o.cache.Set(ck, resp)
	o.log.Info("fusion complete: %d workers", len(workerResults))
	return resp, nil
}

func (o *Orchestrator) RunStream(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (<-chan types.StreamChunk, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("group %q reviewer not found", ictx.GroupName)
	}

	if len(group.Providers) == 0 {
		return reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	complexity := o.classifyComplexity(ctx, req, reviewerCli)
	if complexity == "simple" {
		return reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	workerResults := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	if len(workerResults) == 0 {
		return reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	synthMsgs := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	return reviewerCli.ChatStream(ctx, synthMsgs, req.Temperature, req.MaxTokens, ictx.Tools)
}

// ---- complexity classification ----

func (o *Orchestrator) classifyComplexity(
	ctx context.Context, req types.ChatCompletionRequest, cli *client.Client,
) string {
	userText := ""
	for _, m := range req.Messages {
		if m.Role == "user" { userText += m.Content }
	}
	// heuristic pre-check
	if len([]rune(userText)) < 40 {
		return o.finalClassify(ctx, userText, cli)
	}
	if strings.Contains(userText, "design") || strings.Contains(userText, "architect") ||
		strings.Contains(userText, "implement") || strings.Contains(userText, "设计") ||
		strings.Contains(userText, "架构") || len([]rune(userText)) > 500 {
		return "complex"
	}
	return o.finalClassify(ctx, userText, cli)
}

func (o *Orchestrator) finalClassify(ctx context.Context, userText string, cli *client.Client) string {
	msg := types.Message{
		Role: "system",
		Content: "Answer exactly one word: simple or complex.\n\n" +
			"simple: trivial question, single concept, short snippet, minor fix\n" +
			"complex: multi-step, architectural design, multi-file, system-level\n\n" +
			"Task: " + userText,
	}
	resp, err := cli.Chat(ctx, []types.Message{msg}, nil, nil, nil)
	if err != nil { return "complex" }
	if len(resp.Choices) == 0 { return "complex" }
	if strings.Contains(strings.ToLower(resp.Choices[0].Message.Content), "simple") { return "simple" }
	return "complex"
}

// ---- worker prompt (English for higher accuracy) ----

func buildWorkerSystemPrompt(providerName string) string {
	return fmt.Sprintf(
		"You are the %s model, serving as a domain expert on a review panel.\n\n"+
			"Instructions:\n"+
			"1. Provide your best solution or analysis for the user's problem.\n"+
			"2. Be thorough: cover edge cases, performance implications, and trade-offs.\n"+
			"3. If you need a tool (e.g. read_file, run_command), mention it explicitly:\n"+
			"   \"I recommend calling read_file(path='/x/y') to check the existing implementation.\"\n"+
			"   The reviewer will evaluate your request and execute the tool if approved.\n"+
			"4. Output your answer directly — do NOT use function_call or tool_calls.",
		providerName,
	)
}

func buildToolNotice(tools []types.Tool) string {
	if len(tools) == 0 { return "" }
	var sb strings.Builder
	sb.WriteString("[Available Tools]\n")
	sb.WriteString("You do NOT have direct access to these tools. To request a tool call,\n")
	sb.WriteString("describe what you need in your response (e.g. \"I recommend calling...\").\n")
	sb.WriteString("The reviewer model will evaluate and execute if approved.\n\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Function.Name, t.Function.Description))
	}
	return sb.String()
}

// ---- internal ----

func (o *Orchestrator) resolveGroup(groupName string) (config.Group, *client.Client) {
	group, ok := o.cfg.Group(groupName)
	if !ok && len(o.cfg.Groups) > 0 { group = o.cfg.Groups[0] }
	reviewerCli, ok := o.clients[group.Reviewer]
	if !ok && len(o.cfg.Groups) > 0 {
		group = o.cfg.Groups[0]
		reviewerCli, _ = o.clients[group.Reviewer]
	}
	return group, reviewerCli
}

type workerResult struct {
	name string
	resp *types.ChatCompletionResponse
	err  error
}

func (o *Orchestrator) callWorkersParallel(
	ctx context.Context, req types.ChatCompletionRequest,
	group config.Group, clientTools []types.Tool,
) []workerResult {
	providers := group.Providers
	if len(providers) == 0 { return nil }

	// 去重：相同子任务只发一次请求
	taskHashes := make(map[string]string) // hash → provider_name
	deduped := make([]struct{ name string; msg []types.Message }, 0)
	for _, pn := range providers {
		h := cache.Key(req.Messages, nil, "", pn)
		if first, exists := taskHashes[h]; exists {
			o.log.Debug("worker dedup: %s shares result with %s", pn, first)
			continue
		}
		taskHashes[h] = pn
		wm := make([]types.Message, 0, len(req.Messages)+2)
		if notice := buildToolNotice(clientTools); notice != "" {
			wm = append(wm, types.Message{Role: "system", Content: notice})
		}
		wm = append(wm, types.Message{Role: "system", Content: buildWorkerSystemPrompt(pn)})
		wm = append(wm, req.Messages...)
		deduped = append(deduped, struct {
			name string
			msg  []types.Message
		}{pn, wm})
	}

	if len(deduped) < len(providers) {
		o.log.Info("worker dedup: %d/%d unique tasks", len(deduped), len(providers))
	}

	rc := make(chan workerResult, len(deduped))
	var wg sync.WaitGroup

	for _, d := range deduped {
		cli, ok := o.clients[d.name]
		if !ok { continue }
		wg.Add(1)
		go func(name string, c *client.Client, msgs []types.Message) {
			defer wg.Done()
			resp, err := c.Chat(ctx, msgs, req.Temperature, req.MaxTokens, nil)
			rc <- workerResult{name: name, resp: resp, err: err}
		}(d.name, cli, d.msg)
	}
	wg.Wait()
	close(rc)

	var out []workerResult
	for r := range rc {
		if r.err != nil { o.log.Warn("worker %s failed: %v", r.name, r.err); continue }
		out = append(out, r)
	}
	return out
}

func (o *Orchestrator) buildReviewerPrompt(
	req types.ChatCompletionRequest, workers []workerResult, tools []types.Tool,
) []types.Message {
	var sb strings.Builder
	sb.WriteString("You are the reviewer (lead). Below are analyses from panel experts.\n\n")
	sb.WriteString("Instructions:\n")
	sb.WriteString("1. Review each expert's answer — note strengths and weaknesses.\n")
	sb.WriteString("2. Synthesize the best parts into one optimal final answer.\n")
	sb.WriteString("3. If file operations or commands are needed, use function_call.\n\n")
	sb.WriteString("--- Expert Analyses ---\n\n")

	for i, w := range workers {
		content := ""
		if len(w.resp.Choices) > 0 { content = w.resp.Choices[0].Message.Content }
		sb.WriteString(fmt.Sprintf("[Expert %d: %s]\n%s\n\n", i+1, w.name, content))
	}

	sb.WriteString("--- Original User Request ---\n")
	for _, m := range req.Messages {
		if m.Role == "user" { sb.WriteString(m.Content + "\n") }
	}
	sb.WriteString("\nProvide the final answer. Use function_call for tool operations if needed.")

	out := []types.Message{
		{Role: "system", Content: "You are the FusionGate reviewer. Synthesize expert analyses into a final answer. You have full tool access."},
	}
	out = append(out, req.Messages...)
	out = append(out, types.Message{Role: "user", Content: sb.String()})
	return out
}
