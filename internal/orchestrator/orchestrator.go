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
//   1. 审查模型判断任务复杂度（简单→直接回答，复杂→多子模型协同）
//   2. 复杂任务：子模型提供解法分析，审查模型审核合成
//   3. 审查模型独占工具调用权，决定最终输出
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

// Run 执行一次非流式请求。自动判断简单/复杂任务。
func (o *Orchestrator) Run(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (*types.ChatCompletionResponse, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("分组 %q 的审查模型未找到", ictx.GroupName)
	}

	providers := group.Providers

	// 无子模型配置 → 直接回答
	if len(providers) == 0 {
		return reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	// 自适应判断：简单任务直接回答，复杂任务走融合
	complexity := o.classifyComplexity(ctx, req, reviewerCli)
	o.log.Info("任务复杂度: %s", complexity)

	if complexity == "simple" {
		return reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	// 复杂任务：完整融合流程
	workerResults := o.callWorkersParallel(ctx, req, group, ictx.Tools)
	if len(workerResults) == 0 {
		return reviewerCli.Chat(ctx, req.Messages, req.Temperature, req.MaxTokens, ictx.Tools)
	}

	synthMessages := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	resp, err := reviewerCli.Chat(ctx, synthMessages, req.Temperature, req.MaxTokens, ictx.Tools)
	if err != nil {
		return nil, fmt.Errorf("审查模型合成失败: %w", err)
	}
	o.log.Info("融合完成: %d 个子模型参与", len(workerResults))
	return resp, nil
}

// RunStream 执行流式请求。
func (o *Orchestrator) RunStream(
	ctx context.Context,
	req types.ChatCompletionRequest,
	ictx types.InternalContext,
) (<-chan types.StreamChunk, error) {
	group, reviewerCli := o.resolveGroup(ictx.GroupName)
	if reviewerCli == nil {
		return nil, fmt.Errorf("分组 %q 的审查模型未找到", ictx.GroupName)
	}

	providers := group.Providers
	if len(providers) == 0 {
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

	synthMessages := o.buildReviewerPrompt(req, workerResults, ictx.Tools)
	return reviewerCli.ChatStream(ctx, synthMessages, req.Temperature, req.MaxTokens, ictx.Tools)
}

// ---- 复杂度分类 ----

func (o *Orchestrator) classifyComplexity(
	ctx context.Context, req types.ChatCompletionRequest, cli *client.Client,
) string {
	// 启发式预判：极短的问题 → simple
	userText := ""
	for _, m := range req.Messages {
		if m.Role == "user" { userText += m.Content }
	}
	if len([]rune(userText)) < 40 {
		return o.finalClassify(ctx, userText, cli)
	}
	if strings.Contains(userText, "设计") || strings.Contains(userText, "架构") ||
		strings.Contains(userText, "design") || strings.Contains(userText, "implement") ||
		len([]rune(userText)) > 500 {
		return "complex" // 明显复杂，跳过分类调用
	}
	return o.finalClassify(ctx, userText, cli)
}

func (o *Orchestrator) finalClassify(ctx context.Context, userText string, cli *client.Client) string {
	msg := types.Message{
		Role: "system",
		Content: `只回答一个词:simple或complex。

simple: 简单问题、单一知识点、代码片段、小修小补
complex: 多步骤、需架构设计、跨文件开发、系统级方案

问题: ` + userText,
	}
	resp, err := cli.Chat(ctx, []types.Message{msg}, nil, nil, nil)
	if err != nil {
		o.log.Debug("复杂度分类失败，默认 complex: %v", err)
		return "complex"
	}
	if len(resp.Choices) == 0 { return "complex" }
	answer := strings.TrimSpace(strings.ToLower(resp.Choices[0].Message.Content))
	if strings.Contains(answer, "simple") { return "simple" }
	return "complex"
}

// ---- 内部 ----

func (o *Orchestrator) resolveGroup(groupName string) (config.Group, *client.Client) {
	group, ok := o.cfg.Group(groupName)
	if !ok {
		if len(o.cfg.Groups) > 0 {
			group = o.cfg.Groups[0]
		}
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
) []workerResult {
	providers := group.Providers
	if len(providers) == 0 { return nil }

	toolNotice := buildToolNotice(clientTools)

	rc := make(chan workerResult, len(providers))
	var wg sync.WaitGroup

	for _, pn := range providers {
		cli, ok := o.clients[pn]
		if !ok { continue }
		wg.Add(1)
		go func(name string, c *client.Client) {
			defer wg.Done()
			workerMessages := make([]types.Message, 0, len(req.Messages)+2)
			if toolNotice != "" {
				workerMessages = append(workerMessages, types.Message{
					Role: "system", Content: toolNotice,
				})
			}
			workerMessages = append(workerMessages, types.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"你是 %s 模型，作为专家组成员。请针对用户问题提供最优解法。如需工具，在回答中说明即可，审查模型会评估后调用。",
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
		if r.err != nil {
			o.log.Warn("子模型 %s 调用失败: %v", r.name, r.err)
			continue
		}
		out = append(out, r)
	}
	return out
}

func (o *Orchestrator) buildReviewerPrompt(
	req types.ChatCompletionRequest, workers []workerResult, tools []types.Tool,
) []types.Message {
	var sb strings.Builder
	sb.WriteString("你是审查模型（组长）。以下是各子模型针对用户问题的分析。请：\n")
	sb.WriteString("1. 逐一审核每个子模型的回答\n")
	sb.WriteString("2. 综合优点，给出最优最终答案\n")
	sb.WriteString("3. 如需文件操作、命令执行等，使用 function_call\n\n")
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
	sb.WriteString("\n请给出最终答案。如需调用工具，使用 function_call。")

	out := []types.Message{
		{Role: "system", Content: "你是 FusionGate 审查模型，审核子模型分析并输出最终答案。有全部工具调用权限。"},
	}
	out = append(out, req.Messages...)
	out = append(out, types.Message{Role: "user", Content: sb.String()})
	return out
}

func buildToolNotice(tools []types.Tool) string {
	if len(tools) == 0 { return "" }
	var sb strings.Builder
	sb.WriteString("【可用工具】（你无权直接调用，只能通过审查模型申请）\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Function.Name, t.Function.Description))
	}
	sb.WriteString("\n如需以上工具，在回答中说明'建议调用 xxx(参数)'，审查模型评估后执行。")
	return sb.String()
}
