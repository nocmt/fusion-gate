# FusionGate

> [English](README.md) | 中文

> 多模型融合 AI 编程网关。一个审查模型做组长，多个子模型集思广益，输出一个最优答案。
> 对外暴露 OpenAI 兼容接口（Chat Completions + Responses API）。完全本地化，单二进制，零依赖，开箱即用。

## 工作原理

```
Codex ──POST /v1/responses──▶ FusionGate
                                │
                ┌───────────────┼───────────────┐
                ▼               ▼               ▼
           子模型 A         子模型 B        子模型 C
         (出解法建议)    (出解法建议)     (出解法建议)
                │               │               │
                └───────────────┼───────────────┘
                                ▼
                       审查模型（组长）
                  审核所有子模型答案
                 综合输出最优方案
                  决定并执行工具调用
                                │
                                ▼
                   返回给 Codex 执行
```

- **子模型**只提供解法分析，不直接调用工具
- **审查模型**收集所有子模型答案，审核综合，独占工具调用权
- Codex 发来的工具定义原样透传给审查模型；子模型只被告知有这些工具（但不能直接调用）

## 快速开始

```bash
# 编译（零外部依赖，纯标准库）
go build -o fusiongate ./cmd/fusiongate/
go build -o fusiongate-bench ./cmd/fusiongate-bench/

或者一键执行`build.sh`，即可打包多个平台的二进制文件了。

# 填写 config.json，填入真实 API Key

# 启动（自动健康检查，结果 AES 加密缓存到 /tmp）
./fusiongate

# 在 Codex 中将 API Base URL 设为 http://localhost:8086/v1
```

## 缓存策略

三层缓存优化（参考 OpenClacky 90.6% 命中率实践）：

| 层            | 机制                                   | 效果                         |
| ------------- | -------------------------------------- | ---------------------------- |
| **请求级**    | SHA256(消息+工具+分组) 去重，10min TTL | 重复请求零 API 调用          |
| **Worker 级** | 相同子任务的 provider 共享结果         | 减少并行调用                 |
| **Prompt 级** | 英文 prompt + 稳定前缀前置             | 提升上游 prompt cache 命中率 |

```
请求 1: "用 Go 写快速排序" → 3 workers → 审查模型合成 → 缓存 (4 次 API)
请求 2: "用 Go 写快速排序" → 缓存命中 → 直接返回 (0 次 API)
```

## 配置说明（config.json）

详见 `config.json.example`，内含三种 provider API 类型的完整注释示例。

### Provider 字段

| 字段                 | 必填 | 默认值   | 说明                                                                    |
| -------------------- | ---- | -------- | ----------------------------------------------------------------------- |
| `name`               | ✅    | —        | 供应商唯一标识，分组中引用                                              |
| `api_key`            | ✅    | —        | API 密钥                                                                |
| `model_name`         | ✅    | —        | 发送给供应商的模型名称                                                  |
| `base_url`           | ✅¹   | —        | 基础 URL（如 `https://api.deepseek.com/v1`）                            |
| `full_url`           | —    | —        | **最高优先级**。覆盖 `base_url` + `type`，直接指向任意端点              |
| `type`               | —    | `"chat"` | API 格式：`"chat"`（Chat Completions）或 `"responses"`（Responses API） |
| `context_length`     | ⓟ    | `0`      | 最大上下文窗口。由定价库自动填充                                        |
| `output_length`      | ⓟ    | `0`      | 最大输出 tokens。由定价库自动填充                                       |
| `context_length`     | ⓟ    | `0`      | 最大上下文窗口。由定价库自动填充                                        |
| `output_length`      | ⓟ    | `0`      | 最大输出 tokens。由定价库自动填充                                       |
| `input_token_price`  | ⓟ    | `0`      | 输入 token 单价（USD）。由定价库自动填充                                |
| `cached_token_price` | ⓟ    | `0`      | 缓存 token 单价（USD）。由定价库自动填充                                |
| `output_token_price` | ⓟ    | `0`      | 输出 token 单价（USD）。由定价库自动填充                                |

> ⓟ = 由 [LiteLLM 定价库](https://github.com/BerriAI/litellm) 自动填充（2400+ 模型）。省略这些字段即可。
> ¹ 如果设置了 `full_url`，`base_url` 可以不填。

### API 类型三种场景

```jsonc
// A: Chat Completions（默认，最常见）
{ "name": "deepseek", "base_url": "https://api.deepseek.com/v1", "model_name": "deepseek-chat", "api_key": "sk-xxx" }

// B: Responses API（如第三方中转站）
{ "name": "openai", "base_url": "https://api.openai.com/v1", "type": "responses", "model_name": "gpt-5.4", "api_key": "sk-xxx" }

// C: 自定义完整 URL（最高优先级）
{ "name": "proxy", "full_url": "https://my-proxy.example.com/v1/chat/completions", "model_name": "proxy-model", "api_key": "sk-xxx" }
```

### Group 字段

| 字段        | 必填 | 默认值 | 说明                                                                                                                   |
| ----------- | ---- | ------ | ---------------------------------------------------------------------------------------------------------------------- |
| `name`      | ✅    | —      | 分组名（客户端用此名称作为 model）                                                                                     |
| `reviewer`  | ✅    | —      | 审查模型 — 收集、审核、合成子模型答案，独占工具调用权                                                                  |
| `providers` | ✅    | —      | 子模型列表 — 只提供分析。必须至少配置一个 provider。*审查模型不需要出现在此列表中，除非你希望它也作为子模型参与分析。* |

### 顶层字段

| 字段                | 必填 | 默认值      | 说明                                                            |
| ------------------- | ---- | ----------- | --------------------------------------------------------------- |
| `providers`         | ✅    | —           | 供应商定义列表                                                  |
| `groups`            | ✅    | —           | 模型分组                                                        |
| `cli.port`          | —    | `8086`      | 监听端口                                                        |
| `cli.host`          | —    | `"0.0.0.0"` | 监听地址                                                        |
| `cli.language`      | —    | `"zh-CN"`   | 界面语言                                                        |
| `session.enabled`   | —    | `false`     | 启用 `previous_response_id` 会话追踪                            |
| `session.ttl`       | —    | `"1h"`      | 会话过期时间                                                    |
| `pricing_cache_ttl` | —    | `"72h"`     | 定价库缓存过期时间（`0`=禁用）                                  |
| `worker_timeout`    | —    | `"40s"`     | 每个工人类调用的超时；慢工人类会被跳过，避免 Codex 长时间无响应 |
| `log_level`         | —    | `"info"`    | `"debug"` / `"info"` / `"warn"` / `"error"`                     |

## 定价自动填充

FusionGate 内置 [LiteLLM 定价数据库](https://github.com/BerriAI/litellm)（2400+ 模型）。新增 provider 时，未填的 `context_length`、`output_length`、`input_token_price`、`output_token_price` 会自动匹配 `model_name` 从数据库补全。用户配置的值始终优先。

```json
// 这样就够了 — context_length 和定价从数据库自动补全：
{"name":"ds","base_url":"https://api.deepseek.com/v1","model_name":"deepseek-chat","api_key":"sk-xxx"}
```

数据库本地缓存，默认 72 小时过期，可通过 `pricing_cache_ttl` 配置。

### 最小融合配置

```json
{
  "providers": [
    { "name": "reviewer", "base_url": "https://api.deepseek.com/v1", "model_name": "deepseek-chat", "api_key": "sk-xxx" },
    { "name": "worker", "base_url": "https://api.deepseek.com/v1", "model_name": "deepseek-chat", "api_key": "sk-xxx" }
  ],
  "groups": [{ "name": "main", "reviewer": "reviewer", "providers": ["worker"] }]
}
```

## 基准测试

```bash
# 运行测试（支持断点续跑）
./fusiongate-bench --config eval/questions.json --gateway http://localhost:8086

# 查看报告
cat eval/report.md
```

**实测效果**（deepseek-v4-pro + MiniMax-M3 + glm-5.2，10 题 × 2 轮）：

| 模式                | 得分        |
| ------------------- | ----------- |
| Fusion (3 模型融合) | 4.05 / 5.00 |

Fusion 在架构设计题上提升最显著 (+2.8)，算法题也有明显优势 (+1.0)。

## 接口

| 端点                        | 用途                      |
| --------------------------- | ------------------------- |
| `POST /v1/chat/completions` | OpenAI 兼容               |
| `POST /v1/responses`        | Responses API（Codex 用） |
| `GET /v1/models`            | 模型列表                  |
| `GET /health`               | 健康检查                  |

## Codex 兼容性说明

- FusionGate 发送官方 Responses API 流式事件，每个事件 payload 都带 `type` 字段。文本使用 `response.output_text.delta` / `.done`，审查模型工具调用使用 `response.function_call_arguments.delta` / `.done`。
- FusionGate 严格执行“子模型 + 审查模型”融合：分组必须配置至少一个子模型；如果一轮请求没有任何子模型按时返回，该轮会失败而不是退化成 reviewer-only 单模型输出。
- `worker_timeout` 默认 `40s`，未按时返回的工人类会被跳过；只要至少一个子模型返回，审查模型就基于已收集意见继续合成。
- SSE 响应头包含 `X-Accel-Buffering: no` 避免反向代理缓冲；内部进度通过 SSE comment 保活，避免 Codex 把 FusionGate 状态误当成模型输出。

## 启动时健康检查

启动时向每个供应商发送最小化请求验证连通性：
- ✅ 通过 → 显示延迟
- ❌ 失败 → 显示具体错误（401 / 超时等）
- 结果 AES-GCM 加密缓存到 `/tmp/fusiongate_health_check`，配置不变则跳过

## 项目结构

```
fusiongate/
├── cmd/fusiongate/main.go          # 入口 + 健康检查
├── cmd/fusiongate-bench/main.go    # benchmark CLI
├── internal/
│   ├── cache/store.go             # 请求级语义缓存
│   ├── config/config.go           # 配置加载
│   ├── health/checker.go          # 健康检查 + AES 加密缓存
│   ├── session/store.go           # previous_response_id 映射
│   ├── logger/logger.go           # 彩色日志
│   ├── types/
│   │   ├── types.go               # Chat Completions 类型
│   │   └── responses.go          # Responses API 类型
│   ├── client/
│   │   ├── client.go             # 上游 HTTP 调用
│   │   └── stream.go             # 上游流式调用
│   ├── orchestrator/orchestrator.go # 融合引擎
│   └── handler/openai.go          # HTTP 路由 + 格式转换
├── eval/questions.json             # 测试题库
├── docs/codex-guide.md             # Codex 测试指南
├── config.json                     # 配置（含 API Key，已 gitignore）
├── LICENSE
└── .gitignore
```

## 许可证

MIT
