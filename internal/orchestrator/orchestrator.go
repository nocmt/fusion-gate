package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"fusiongate/internal/client"
	"fusiongate/internal/config"
	"fusiongate/internal/logger"
	"fusiongate/internal/types"
)

// Orchestrator 是多模型融合引擎。
//
// 协作流程：
//   1. Codex 发来请求（含 tools 定义）
//   2. 审查模型（reviewer）拿到全部上下文 + 工具
//   3. 子模型（providers）拿到问题 + 工具说明（不能直接调用，需向审查模型申请）
//   4. 审查模型收集所有子模型答案，逐一审核，综合输出
//   5. 审查模型决定最终工具调用并返回给 Codex
type Orchestrator struct {
	cfg     *config.Config
	clients map[string]*client.Client
	log     *logger.Logger
}

func New(cfg *config.Config, log *logger.Logger) *Orchestrator {
	clients := make(map[string]*client.Client, len(cfg.Providers))
	for _, p := range cfg.Providers {
		clients[p.Name] = client.New(p, log)
	}
	return &Orchestrator{cfg: cfg, clients: clients, log: log}
}

// Run 执行一次非流式融合请求。
func (o *Orchestrator) Run(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (*types.ChatCompletionResponse, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil { return nil, fmt.Errorf("分组 %q 的审查模型未找到", ictx.GroupName) }

	// 1. 并行调用所有子模型（告知可用的工具但无权直接调用）
	workerResults := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	if len(workerResults) == 0 {
		// 没有子模型可用，直通审查模型
		return reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	// 2. 审查模型合成 + 决定工具调用
	synthMessages := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	resp, err := reviewerCli.Chat(ctx, synthMessages, req.Temperature, req.MaxTokens, ictx.Tools)
	if err != nil {
		return nil, fmt.Errorf("审查模型合成失败: %w", err)
	}
	o.log.Info("融合完成: %d 个子模型参与", len(workerResults))
	return resp, nil
}

// RunStream 执行流式融合。
func (o *Orchestrator) RunStream(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (<-chan types.StreamChunk, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil { return nil, fmt.Errorf("分组 %q 的审查模型未找到", ictx.GroupName) }

	workerResults := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	if len(workerResults) == 0 {
		return reviewerCli.ChatStream(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	synthMessages := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	return reviewerCli.ChatStream(ctx, synthMessages, req.Temperature, req.MaxTokens, ictx.Tools)
}

// ---- 内部 ----

func (o *Orchestrator) resolveGroup(groupName string) (config.Group, *client.Client) {
	group, ok := o.cfg.Group(groupName)
	if !ok {
		if len(o.cfg.Groups) > 0 { group = o.cfg.Groups[0] } else { return group, nil }
	}
	reviewerCli, ok := o.clients[group.Reviewer]
	if !ok { return group, nil }
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

	// 为子模型构建工具说明消息（只告知，不给 tools 参数）
	workerTools := clientTools
	toolNotice := buildToolNotice(workerTools)

	rc := make(chan workerResult, len(providers))
	var wg sync.WaitGroup

	for _, pn := range providers {
		cli, ok := o.clients[pn]
		if !ok { continue }
		wg.Add(1)
		go func(name string, c *client.Client) {
			defer wg.Done()

			// 子模型拿到：工具说明 + 原问题
			workerMessages := make([]types.Message, 0, len(req.Messages)+1)
			if toolNotice != "" {
				workerMessages = append(workerMessages, types.Message{
					Role: "system", Content: toolNotice,
				})
			}
			workerMessages = append(workerMessages, types.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"你是 %s 模型，作为专家组员。请针对用户问题提供你的最优解法。如果需要调用工具（如读取文件、执行命令等），在你的回答中说明你需要什么工具和参数，审查模型会评估后帮你调用。直接给出你的分析，不要使用 function_call。",
					name,
				),
			})
			workerMessages = append(workerMessages, req.Messages...)

			resp, err := c.Chat(ctx, workerMessages, req.Temperature, req.MaxTokens, nil)
			rc <- workerResult{name: name, resp: resp, err: err}
		}(pn, cli)
	}
	wg.Wait()
	close(rc)

	var out []workerResult
	for r := range rc {
		if r.err != nil { o.log.Warn("子模型 %s 调用失败: %v", r.name, r.err); continue }
		out = append(out, r)
	}
	return out
}

// buildReviewerPrompt 构造审查模型的合成提示。
func (o *Orchestrator) buildReviewerPrompt(
	req types.ChatCompletionRequest, workers []workerResult, tools []types.Tool,
) []types.Message {
	var sb strings.Builder
	sb.WriteString("你是审查模型（组长）。以下是各子模型针对用户问题的分析，请：\n")
	sb.WriteString("1. 逐一审查每个子模型的回答，指出优缺点\n")
	sb.WriteString("2. 综合所有子模型的优点，给出一个最优的最终答案\n")
	sb.WriteString("3. 如果用户要求执行具体操作（如读写文件、运行命令等），请使用 tool_call 来执行\n\n")
	sb.WriteString("--- 子模型分析 ---\n\n")

	for i, w := range workers {
		content := ""
		if len(w.resp.Choices) > 0 { content = w.resp.Choices[0].Message.Content }
		sb.WriteString(fmt.Sprintf("【子模型 %d: %s】\n%s\n\n", i+1, w.name, content))
	}

	sb.WriteString("--- 用户原始问题 ---\n")
	for _, m := range req.Messages {
		if m.Role == "user" { sb.WriteString(m.Content + "\n") }
	}
	sb.WriteString("\n请给出最终答案。如需调用工具，请使用 function_call。")

	// 审查模型拿到原始上下文 + 子模型分析
	out := []types.Message{
		{Role: "system", Content: "你是 FusionGate 审查模型，负责审核所有子模型的分析并输出最终答案。你可以调用工具来完成任务。"},
	}
	// 携带用户原始上下文
	out = append(out, req.Messages...)
	out = append(out, types.Message{Role: "user", Content: sb.String()})
	return out
}

// buildToolNotice 为子模型生成本次可用工具的说明。
func buildToolNotice(tools []types.Tool) string {
	if len(tools) == 0 { return "" }
	var sb strings.Builder
	sb.WriteString("【可用工具列表】（你无权直接调用，只能通过审查模型申请）\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Function.Name, t.Function.Description))
	}
	sb.WriteString("\n如果你的分析需要用到以上工具，请在回答中说明'建议调用 xxx(参数...)'，审查模型会评估后执行。")
	return sb.String()
}
