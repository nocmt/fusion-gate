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

## Codex 测试指南 → [docs/codex-guide.md](docs/codex-guide.md)

包含：配置方法、三个难度等级的实测题目、主流评测基准清单、GPT-5.4 主模型分析。

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
      "reviewer": "deepseek",                        // 审查模型（组长）
      "providers": ["deepseek", "minimax", "glm"]    // 子模型（组员）
    }
  ],
  "session": { "enabled": true, "ttl": "1h" },
  "log_level": "info",
  "cli": { "port": 8086, "host": "0.0.0.0", "language": "zh-CN" }
}
```

**推荐模型组合**（经实测验证）：
- 审查模型：DeepSeek-V4-Pro（长上下文 + 强审核能力）
- 子模型：DeepSeek-V4-Pro / MiniMax-M3 / GLM-5.2

## A/B 基准测试

```bash
# 需先在 config.json 添加 single 分组：
# { "name": "single", "reviewer": "deepseek", "providers": [] }

# 运行测试（支持断点续跑）
./fusiongate-bench --config eval/questions.json --gateway http://localhost:8086

# 查看报告
cat eval/report.md
```

**实测效果**（deepseek-v4-pro + MiniMax-M3 + glm-5.2，10 题 × 2 轮）：

| 模式                     | 得分        |
| ------------------------ | ----------- |
| 单模型 (DeepSeek direct) | 3.90 / 5.00 |
| Fusion (3 模型融合)      | 4.05 / 5.00 |
| **差值**                 | **+0.15**   |

Fusion 在架构设计题上提升最显著 (+2.8)，算法题也有明显优势 (+1.0)。

## 接口

| 端点                        | 用途                      |
| --------------------------- | ------------------------- |
| `POST /v1/chat/completions` | OpenAI 兼容               |
| `POST /v1/responses`        | Responses API（Codex 用） |
| `GET /v1/models`            | 模型列表                  |
| `GET /health`               | 健康检查                  |

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
