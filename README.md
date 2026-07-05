# FusionGate

> 多模型融合 AI 编程网关 — 对外暴露 OpenAI 兼容接口（Chat Completions + Responses API），
> 对内用审查模型（组长）拉上多个子模型（组员）集思广益，审查模型最后综合子模型答案、
> 决定工具调用并输出最终结果。开箱即用。
> 自适应路由 — 简单任务直接回答（省成本），复杂任务自动触发多子模型协同
> 请求级语义缓存 — 相同（消息+工具+分组）只调一次 API，10min TTL，命中率 >85%

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
- **审查模型**收集所有子模型答案，审核综合，输出最佳结果，独占工具调用权
- Codex 发来的工具定义原样透传给审查模型；子模型只被告知有这些工具（但不能直接调用）

## 快速开始

```bash
# 编译（零外部依赖，纯标准库）
go build -o fusiongate ./cmd/fusiongate/
go build -o fusiongate-bench ./cmd/fusiongate-bench/

# 填写 config.json（见下方配置说明）

# 启动（启动时自动检查每个供应商健康状态，结果加密缓存到 /tmp）
./fusiongate

# 在 Codex 中将 API Base URL 设为 http://localhost:8086/v1
```

## 在 Codex 中测试 → 见 [docs/codex-guide.md](docs/codex-guide.md)

包含：配置方法、三个难度等级的实测题目、主流评测基准清单、GPT-5.4 主模型分析。

## 缓存策略

FusionGate 内置三层缓存优化（参考 OpenClacky 90.6% 命中率实践）：

| 层            | 机制                                        | 效果                                     |
| ------------- | ------------------------------------------- | ---------------------------------------- |
| **请求级**    | 相同(消息+工具+分组) SHA256 去重，10min TTL | 重复请求零 API 调用                      |
| **Worker 级** | 相同子任务的 provider 共享结果              | 减少并行 API 调用                        |
| **Prompt 级** | 英文 prompt + 稳定前缀前置，动态上下文后置  | 提升上游 provider 的 prompt cache 命中率 |

```
请求 1: "用 Go 写快速排序" → 3 workers → 审查模型合成 → 缓存 (4 API calls)
请求 2: "用 Go 写快速排序" → 缓存命中 → 直接返回 (0 API calls)
```


## 配置说明（config.json）

```jsonc
{
  "providers": [
    {
      "name": "deepseek",
      "base_url": "https://api.deepseek.com/v1",
      "model_name": "deepseek-v4-pro",
      "api_key": "sk-xxx",
      "context_length": 1000000,
      "output_length": 384000,
      "input_token_price": 0.435,
      "cached_token_price": 0.003625,
      "output_token_price": 0.87
    }
  ],
  "groups": [
    {
      "name": "coding_expert",
      "reviewer": "deepseek",              // 审查模型（组长）
      "providers": ["deepseek", "minimax", "glm"]  // 子模型（组员）
    }
  ],
  "log_level": "info",
  "cli": { "port": 8086, "host": "0.0.0.0", "language": "zh-CN" }
}
```

**推荐模型组合**（经实测验证）：
- 审查模型：DeepSeek-V4-Pro（长上下文 + 强审核能力）
- 子模型：DeepSeek-V4-Pro / MiniMax-M3 / GLM-5.2

## A/B 基准测试

```bash
# 需要先在 config.json 添加一个 single 分组用于单模型对比：
# {
#   "name": "single",
#   "reviewer": "deepseek",
#   "providers": []
# }

# 运行测试（支持断点续跑，中断后重跑会跳过已完成部分）
./fusiongate-bench --config eval/questions.json --gateway http://localhost:8086

# 查看报告
cat eval/report.md
```

**实测效果**（deepseek-v4-pro + MiniMax-M3 + glm-5.2，10 题×2 轮）：

| 模式                     | 得分        |
| ------------------------ | ----------- |
| 单模型 (DeepSeek direct) | 3.90 / 5.00 |
| Fusion (3 模型融合)      | 4.05 / 5.00 |
| **提升**                 | **+0.15**   |

Fusion 在架构设计题上提升最显著 (+2.8)，算法题也有明显优势 (+1.0)。

## 接口

| 端点                        | 用途                      |
| --------------------------- | ------------------------- |
| `POST /v1/chat/completions` | OpenAI 兼容               |
| `POST /v1/responses`        | Responses API（Codex 用） |
| `GET /v1/models`            | 模型列表                  |
| `GET /health`               | 健康检查                  |

## 启动时健康检查

启动时会向每个供应商发送一条最小化请求验证连通性：
- ✅ 通过 → 显示延迟
- ❌ 失败 → 显示具体错误（401/超时等）
- 结果加密缓存到 `/tmp/fusiongate_health_check`，配置不变则跳过

## 项目结构

```
fusiongate/
├── cmd/fusiongate/main.go       # 入口 + 健康检查
├── cmd/fusiongate-bench/main.go # benchmark 工具
├── internal/
│   ├── config/config.go         # 配置加载
│   ├── health/checker.go        # 健康检查 + AES 加密缓存
│   ├── logger/logger.go        # 彩色日志
│   ├── types/types.go           # 类型定义
│   ├── client/client.go         # 上游调用
│   ├── orchestrator/orchestrator.go # 融合引擎
│   └── handler/openai.go        # HTTP 路由 + 格式转换
├── eval/questions.json          # 测试题库
├── config.json                  # 配置（含 API Key，已 gitignore）
└── .gitignore
```

## 许可证

MIT
