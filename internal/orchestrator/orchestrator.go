package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"fusiongate/internal/cache"
	"fusiongate/internal/client"
	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

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

// ---- non-streaming ----

func (o *Orchestrator) Run(
	ctx context.Context, req types.ChatCompletionRequest, ictx types.InternalContext,
) (*types.ChatCompletionResponse, error) {
	req.Messages = normalizeMessages(req.Messages)
	o.log.Raw("RUN", "group=%s %s tools=%d", ictx.GroupName, msgSummary(req.Messages), len(ictx.Tools))

	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("group %q reviewer not found", ictx.GroupName)
	}

	ck := cache.Key(req.Messages, ictx.Tools, ictx.GroupName, req.Model)
	if cached, ok := o.cache.Get(ck); ok {
		o.log.Info("缓存命中")
		return cached, nil
	}

	if len(group.Providers) == 0 {
		resp, err := reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
		if err == nil {
			o.cache.Set(ck, resp)
		}
		return resp, err
	}

	workerRc := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	var workerResults []workerResult
	for r := range workerRc {
		workerResults = append(workerResults, r)
	}
	if len(workerResults) == 0 {
		resp, err := reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
		if err == nil {
			o.cache.Set(ck, resp)
		}
		return resp, err
	}

	synthMsgs := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	resp, err := reviewerCli.Chat(ctx, synthMsgs, req.Temperature, req.MaxTokens, ictx.Tools)
	if err != nil {
		return nil, fmt.Errorf("reviewer synthesis failed: %w", err)
	}
	o.cache.Set(ck, resp)
	o.log.Info("fusion complete: %d workers", len(workerResults))
	return resp, nil
}

// ---- streaming (with heartbeats during worker phase) ----

func (o *Orchestrator) RunStream(
	ctx context.Context, req types.ChatCompletionRequest, ictx types.InternalContext,
) (<-chan types.StreamChunk, error) {
	req.Messages = normalizeMessages(req.Messages)

	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("group %q reviewer not found", ictx.GroupName)
	}

	if len(group.Providers) == 0 {
		o.log.Info("stream reviewer only: no workers configured")
		return reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	ch := make(chan types.StreamChunk, 64)
	startTime := time.Now()

	go func() {
		defer close(ch)

		workerTimeout := o.cfg.WorkerTimeoutDuration()
		gracePeriod := 2 * time.Second
		hardTimeout := workerTimeout

		// Phase 1: consult experts
		ch <- statusChunk("FusionGate", fmt.Sprintf("Consulting %d experts (timeout %s)...", len(group.Providers), workerTimeout))

		workerRc := o.callWorkersParallel(ctx, req, group, ictx.Tools)
		var workerResults []workerResult

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		hardTimer := time.NewTimer(hardTimeout)
		defer hardTimer.Stop()
		var graceTimer *time.Timer
		var graceCh <-chan time.Time
		gotFirst := false

	loop:
		for {
			select {
			case <-ticker.C:
				ch <- heartbeatChunk()
			case r, ok := <-workerRc:
				if !ok {
					break loop
				}
				if r.err != nil {
					ch <- statusChunk("FusionGate", fmt.Sprintf("Expert %s did not respond in time.", r.name))
					continue
				}
				workerResults = append(workerResults, r)
				ch <- statusChunk("FusionGate", fmt.Sprintf("Expert %s returned (%d chars).", r.name, len(extractWorkerContent(r.resp))))
				if !gotFirst {
					gotFirst = true
					graceTimer = time.NewTimer(gracePeriod)
					graceCh = graceTimer.C
					ch <- statusChunk("FusionGate", fmt.Sprintf("First expert ready. Waiting %v for more...", gracePeriod))
				}
				if len(workerResults) >= len(group.Providers) {
					if graceTimer != nil {
						graceTimer.Stop()
					}
					break loop
				}
			case <-graceCh:
				ch <- statusChunk("FusionGate", "Grace period ended. Moving to synthesis.")
				break loop
			case <-hardTimer.C:
				ch <- statusChunk("FusionGate", "Worker timeout reached. Moving to synthesis.")
				break loop
			case <-ctx.Done():
				if graceTimer != nil {
					graceTimer.Stop()
				}
				return
			}
		}
		if graceTimer != nil {
			graceTimer.Stop()
		}

		if len(workerResults) == 0 {
			ch <- statusChunk("FusionGate", "No expert responded in time, falling back to reviewer.")
			revCh, err := reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
			if err != nil {
				ch <- errorChunk(err)
				return
			}
			for c := range revCh {
				ch <- c
			}
			return
		}

		// Phase 2: reviewer synthesis
		elapsed := time.Since(startTime).Seconds()
		ch <- statusChunk("FusionGate", fmt.Sprintf("Collected %d expert opinions (%.0fs). Synthesizing...", len(workerResults), elapsed))

		synthMsgs := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
		o.log.Info("reviewer synthesis started with %d workers", len(workerResults))
		revCh, err := reviewerCli.ChatStream(ctx, synthMsgs, req.Temperature, req.MaxTokens, ictx.Tools)
		if err != nil {
			ch <- errorChunk(err)
			return
		}

		var totalOutput int
		for c := range revCh {
			if c.Usage.CompletionTokens > 0 {
				totalOutput = c.Usage.CompletionTokens
			}
			ch <- c
		}

		totalTime := time.Since(startTime).Seconds()
		o.log.Info("fusion stream complete: %d workers, %.1fs, %d output tokens", len(workerResults), totalTime, totalOutput)
	}()

	return ch, nil
}

// ---- worker prompts (English) ----

func buildWorkerSystemPrompt(providerName string) string {
	return fmt.Sprintf(`You are %s, a domain expert on a review panel.

Your job: review the user's request and provide a concise, high-value analysis that the lead reviewer will use to build the final answer.

Rules:
1. Be concise: 3–5 bullets or one short paragraph. Avoid long preambles.
2. Focus on: key insights, risks, edge cases, and concrete recommendations.
3. Do NOT call tools, emit function calls, or write code blocks unless the user explicitly asks for code.
4. Do NOT use any tool-call format such as <tool_call>, <function>, <｜｜DSML｜｜>, JSON function_call, or shell commands.
5. Do NOT repeat the user's question back.
6. Output only your analysis — plain text, nothing else.`, providerName)
}

func buildWorkerMessages(req types.ChatCompletionRequest, providerName string) []types.Message {
	msgs := stripCodexInstructions(req.Messages)
	wm := make([]types.Message, 0, len(msgs)+1)
	wm = append(wm, types.Message{Role: "system", Content: buildWorkerSystemPrompt(providerName)})
	wm = append(wm, msgs...)
	return wm
}

// stripCodexInstructions removes the Codex "You are a coding agent..." system prompt
// so that worker models do not mistake themselves for the Codex agent.
func stripCodexInstructions(msgs []types.Message) []types.Message {
	out := make([]types.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" && (strings.Contains(m.Content, "Codex CLI") || strings.Contains(m.Content, "You are a coding agent")) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func buildToolNotice(tools []types.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Available Tools - request via reviewer only]\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Function.Name, t.Function.Description))
	}
	return sb.String()
}

// ---- internal ----

func (o *Orchestrator) resolveGroup(groupName string) (config.Group, *client.Client) {
	group, ok := o.cfg.Group(groupName)
	if !ok && len(o.cfg.Groups) > 0 {
		group = o.cfg.Groups[0]
	}
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
) <-chan workerResult {
	providers := group.Providers
	rc := make(chan workerResult, len(providers))
	if len(providers) == 0 {
		close(rc)
		return rc
	}

	deduped := make([]struct {
		name string
		msg  []types.Message
	}, 0, len(providers))
	taskHashes := make(map[string]string)
	for _, pn := range providers {
		h := cache.Key(stripCodexInstructions(req.Messages), nil, "", pn)
		if first, exists := taskHashes[h]; exists {
			o.log.Debug("dedup: %s=%s", pn, first)
			continue
		}
		taskHashes[h] = pn
		wm := buildWorkerMessages(req, pn)
		deduped = append(deduped, struct {
			name string
			msg  []types.Message
		}{pn, wm})
	}
	if len(deduped) < len(providers) {
		o.log.Info("dedup: %d/%d unique", len(deduped), len(providers))
	}

	workerTimeout := o.cfg.WorkerTimeoutDuration()
	workerCtx, cancel := context.WithTimeout(ctx, workerTimeout)

	var wg sync.WaitGroup
	for _, d := range deduped {
		cli, ok := o.clients[d.name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, c *client.Client, msgs []types.Message) {
			defer wg.Done()
			start := time.Now()
			// workers only provide analysis; let each model use its own configured max_tokens
			resp, err := c.Chat(workerCtx, msgs, req.Temperature, req.MaxTokens, nil)
			if err != nil && workerCtx.Err() == context.DeadlineExceeded {
				o.log.Info("worker %s skipped after timeout %v", name, workerTimeout)
				err = fmt.Errorf("timeout after %v", workerTimeout)
			} else if err != nil {
				o.log.Warn("worker %s error after %v: %v", name, time.Since(start), err)
			}
			select {
			case rc <- workerResult{name: name, resp: resp, err: err}:
			case <-workerCtx.Done():
			}
		}(d.name, cli, d.msg)
	}

	go func() { wg.Wait(); close(rc); cancel() }()
	return rc
}

func (o *Orchestrator) buildReviewerPrompt(
	req types.ChatCompletionRequest, workers []workerResult, tools []types.Tool,
) []types.Message {
	var sb strings.Builder
	sb.WriteString(`You are the lead reviewer. Below are concise analyses from domain experts.

Instructions:
1. Read each expert's analysis and extract the most useful points.
2. Synthesize them into a single coherent, actionable final answer.
3. Resolve contradictions explicitly and state the final decision.
4. If a tool is needed to fulfill the user's request, use function_call.
5. Be concise and direct. Do not merely summarize; provide the actual answer.

--- Expert Analyses ---

`)
	for i, w := range workers {
		c := ""
		if len(w.resp.Choices) > 0 {
			c = w.resp.Choices[0].Message.Content
		}
		sb.WriteString(fmt.Sprintf("[Expert %d: %s]\n%s\n\n", i+1, w.name, c))
	}
	sb.WriteString("--- Original User Request ---\n")
	for _, m := range req.Messages {
		if m.Role == "user" {
			sb.WriteString(m.Content + "\n")
		}
	}
	sb.WriteString("\nProvide the final answer. Use function_call only if a tool is required.")
	out := []types.Message{{Role: "system", Content: "FusionGate lead reviewer. Synthesize expert analyses and produce the final answer. Full tool access."}}
	out = append(out, req.Messages...)
	out = append(out, types.Message{Role: "user", Content: sb.String()})
	return out
}

func normalizeMessages(msgs []types.Message) []types.Message {
	for i := range msgs {
		if msgs[i].Role == "developer" {
			msgs[i].Role = "system"
		}
	}
	return msgs
}

func msgSummary(msgs []types.Message) string {
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	return fmt.Sprintf("%d msgs roles=%v", len(msgs), roles)
}

// ---- stream helpers ----

// extractWorkerContent pulls the assistant text from a worker response.
func extractWorkerContent(resp *types.ChatCompletionResponse) string {
	if resp == nil || len(resp.Choices) == 0 {
		return "(no output)"
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return "(empty output)"
	}
	return content
}
func heartbeatChunk() types.StreamChunk {
	return types.StreamChunk{
		Object:  "fusiongate.heartbeat",
		Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{Content: "."}}},
	}
}

// statusChunk returns a chunk that carries a status message for client display.
func statusChunk(status, msg string) types.StreamChunk {
	return types.StreamChunk{
		Object: "fusiongate.status",
		Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{
			Content: fmt.Sprintf("\n[%s] %s\n", status, msg),
		}}},
	}
}

func errorChunk(err error) types.StreamChunk {
	return types.StreamChunk{
		Object: "fusiongate.error",
		Choices: []types.ChunkChoice{{Index: 0, Delta: types.Delta{
			Content: err.Error(),
		}}},
	}
}
