# FusionGate

> [中文文档](README-zh.md) | English

> A multi-model fusion AI gateway. One reviewer leads, many models contribute — one best answer comes out.
> Exposes OpenAI-compatible APIs (Chat Completions + Responses API). Fully local, single binary, zero dependencies.

## How It Works

```
Codex ──POST /v1/responses──▶ FusionGate
                                │
                ┌───────────────┼───────────────┐
                ▼               ▼               ▼
           Worker A         Worker B         Worker C
         (analyzes)       (analyzes)        (analyzes)
                │               │               │
                └───────────────┼───────────────┘
                                ▼
                        Reviewer (lead)
                   Reviews all worker answers
                  Synthesizes the best output
                   Decides & executes tools
                                │
                                ▼
                      Returns to Codex
```

- **Worker models** provide analysis only — no direct tool access
- **Reviewer model** collects, reviews, synthesizes, and holds exclusive tool-calling authority
- Codex's tool definitions are passed through to the reviewer; workers are informed of available tools but cannot call them

## Quick Start

```bash
# Build (zero external dependencies, pure stdlib)
go build -o fusiongate ./cmd/fusiongate/
go build -o fusiongate-bench ./cmd/fusiongate-bench/

# Fill in config.json with real API keys

# Start (auto health-checks every provider; results encrypted & cached to /tmp)
./fusiongate

# In Codex: set API Base URL to http://localhost:8086/v1
```

## Codex Testing Guide → [docs/codex-guide.md](docs/codex-guide.md)

Includes: setup instructions, three difficulty levels of test prompts, benchmark suite overview, and GPT-5.4 reviewer analysis.

## Cache Strategy

Three-layer cache optimization (inspired by OpenClacky's 90.6% hit-rate practice):

| Layer | Mechanism | Impact |
|-------|-----------|--------|
| **Request-level** | SHA256(messages + tools + group) dedup, 10min TTL | Zero API calls on repeats |
| **Worker-level** | Identical sub-tasks share one provider call | Less parallel API usage |
| **Prompt-level** | English prompts + stable prefix first, dynamic context last | Boosts upstream prompt cache hits |

```
Request 1: "Write quicksort in Go" → 3 workers → reviewer synthesis → cached (4 API calls)
Request 2: "Write quicksort in Go" → cache hit → instant return (0 API calls)
```

## Configuration (config.json)

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
      "reviewer": "deepseek",                        // reviewer (lead)
      "providers": ["deepseek", "minimax", "glm"]    // workers (experts)
    }
  ],
  "session": { "enabled": true, "ttl": "1h" },
  "log_level": "info",
  "cli": { "port": 8086, "host": "0.0.0.0", "language": "zh-CN" }
}
```

**Recommended models** (benchmark-verified):
- Reviewer: DeepSeek-V4-Pro (long context + strong review ability)
- Workers: DeepSeek-V4-Pro / MiniMax-M3 / GLM-5.2

## A/B Benchmarking

```bash
# Add a single-model group for comparison:
# { "name": "single", "reviewer": "deepseek", "providers": [] }

# Run (supports resume — re-run picks up where it left off)
./fusiongate-bench --config eval/questions.json --gateway http://localhost:8086

# View report
cat eval/report.md
```

**Benchmark results** (deepseek-v4-pro + MiniMax-M3 + glm-5.2, 10 questions × 2 rounds):

| Mode | Score |
|------|-------|
| Single (DeepSeek direct) | 3.90 / 5.00 |
| Fusion (3-model ensemble) | 4.05 / 5.00 |
| **Delta** | **+0.15** |

Fusion shows the strongest gains on architecture design questions (+2.8) and algorithm tasks (+1.0).

## Endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /v1/chat/completions` | OpenAI-compatible |
| `POST /v1/responses` | Responses API (for Codex) |
| `GET /v1/models` | Model list |
| `GET /health` | Health check |

## Startup Health Check

On startup, each provider is probed with a minimal request:
- ✅ Pass → latency is shown
- ❌ Fail → specific error is logged (401 / timeout / etc.)
- Results are AES-GCM encrypted and cached to `/tmp/fusiongate_health_check`; skipped if config is unchanged

## Project Structure

```
fusiongate/
├── cmd/fusiongate/main.go          # entrypoint + health check
├── cmd/fusiongate-bench/main.go    # benchmark CLI
├── internal/
│   ├── cache/store.go             # request-level semantic cache
│   ├── config/config.go           # config loading & validation
│   ├── health/checker.go          # health check + AES cache
│   ├── session/store.go           # previous_response_id mapping
│   ├── logger/logger.go           # colored logger
│   ├── types/
│   │   ├── types.go               # Chat Completions types
│   │   └── responses.go          # Responses API types
│   ├── client/
│   │   ├── client.go             # upstream HTTP calls
│   │   └── stream.go             # upstream streaming
│   ├── orchestrator/orchestrator.go # fusion engine
│   └── handler/openai.go          # HTTP routing + format conversion
├── eval/questions.json             # benchmark question bank
├── docs/codex-guide.md             # Codex testing guide
├── config.json                     # config (contains API keys, gitignored)
├── LICENSE
└── .gitignore
```

## License

MIT
