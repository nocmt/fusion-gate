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

Or, you can run `build.sh` with one click to package binary files for multiple platforms.

# Fill in config.json with real API keys

# Start (auto health-checks every provider; results encrypted & cached to /tmp)
./fusiongate

# In Codex: set API Base URL to http://localhost:8086/v1
```

## Cache Strategy

Three-layer cache optimization (inspired by OpenClacky's 90.6% hit-rate practice):

| Layer             | Mechanism                                                   | Impact                            |
| ----------------- | ----------------------------------------------------------- | --------------------------------- |
| **Request-level** | SHA256(messages + tools + group) dedup, 10min TTL           | Zero API calls on repeats         |
| **Worker-level**  | Identical sub-tasks share one provider call                 | Less parallel API usage           |
| **Prompt-level**  | English prompts + stable prefix first, dynamic context last | Boosts upstream prompt cache hits |

```
Request 1: "Write quicksort in Go" → 3 workers → reviewer synthesis → cached (4 API calls)
Request 2: "Write quicksort in Go" → cache hit → instant return (0 API calls)
```

## Configuration (config.json)

See `config.json.example` for a full annotated example with all three provider API types.

### Provider fields

| Field                | Required | Default  | Description                                                                                     |
| -------------------- | -------- | -------- | ----------------------------------------------------------------------------------------------- |
| `name`               | ✅        | —        | Unique provider identifier, referenced by groups                                                |
| `api_key`            | ✅        | —        | API key for authentication                                                                      |
| `model_name`         | ✅        | —        | Model name sent to the provider                                                                 |
| `base_url`           | ✅¹       | —        | Base URL (e.g. `https://api.deepseek.com/v1`)                                                   |
| `full_url`           | —        | —        | **Highest priority**. Overrides `base_url` + `type`. Use this to point directly to any endpoint |
| `type`               | —        | `"chat"` | API format: `"chat"` (Chat Completions) or `"responses"` (Responses API)                        |
| `context_length`     | ⓟ       | `0`      | Max context window (tokens). Auto-filled from pricing DB                      |
| `output_length`      | ⓟ       | `0`      | Max output tokens. Auto-filled from pricing DB                               |
| `input_token_price`  | ⓟ       | `0`      | Price per input token (USD). Auto-filled from pricing DB                     |
| `cached_token_price` | ⓟ       | `0`      | Price per cached token (USD). Auto-filled from pricing DB                    |
| `output_token_price` | ⓟ       | `0`      | Price per output token (USD). Auto-filled from pricing DB                    |

> ⓟ = auto-filled from [LiteLLM pricing DB](https://github.com/BerriAI/litellm) (2,400+ models). Omit these to let FusionGate fill them.
> ¹ `base_url` is required unless `full_url` is set.

### API type scenarios

```jsonc
// A: Chat Completions (default — most providers)
{ "name": "deepseek", "base_url": "https://api.deepseek.com/v1", "model_name": "deepseek-chat", "api_key": "sk-xxx" }

// B: Responses API (e.g. third-party proxies)
{ "name": "openai", "base_url": "https://api.openai.com/v1", "type": "responses", "model_name": "gpt-5.4", "api_key": "sk-xxx" }

// C: Full URL override (highest priority)
{ "name": "proxy", "full_url": "https://my-proxy.example.com/v1/chat/completions", "model_name": "proxy-model", "api_key": "sk-xxx" }
```

### Group fields

| Field       | Required | Default | Description                                                                                                                          |
| ----------- | -------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `name`      | ✅        | —       | Group name (used as model name by clients)                                                                                           |
| `reviewer`  | ✅        | —       | Reviewer model — collects & synthesizes worker answers, holds tool authority                                                         |
| `providers` | ✅        | `[]`    | Worker models — provide analysis only. *Reviewer does NOT need to be listed here unless you want it to also contribute as a worker.* |

### Top-level fields

| Field               | Required | Default     | Description                                 |
| ------------------- | -------- | ----------- | ------------------------------------------- |
| `providers`         | ✅        | —           | Provider definitions                        |
| `groups`            | ✅        | —           | Model groups                                |
| `cli.port`          | —        | `8086`      | Listen port                                 |
| `cli.host`          | —        | `"0.0.0.0"` | Listen host                                 |
| `cli.language`      | —        | `"zh-CN"`   | UI language                                 |
| `session.enabled`   | —        | `false`     | Enable `previous_response_id` tracking      |
| `session.ttl`       | —        | `"1h"`      | Session expiry                              |
| `pricing_cache_ttl` | —        | `"72h"`     | Pricing DB cache TTL (`0`=disable)          |
| `log_level`         | —        | `"info"`    | `"debug"` / `"info"` / `"warn"` / `"error"` |

## Pricing Auto-fill

FusionGate ships with the [LiteLLM pricing database](https://github.com/BerriAI/litellm) (2,400+ models).
When you add a provider, missing `context_length`, `output_length`, and pricing fields are
automatically filled by matching `model_name` against the database. User-configured values always win.

```json
// This is enough — context_length & pricing auto-fill from the pricing DB:
{"name":"ds","base_url":"https://api.deepseek.com/v1","model_name":"deepseek-chat","api_key":"sk-xxx"}
```

The database is cached locally (default 72h, configurable via `pricing_cache_ttl`).

### Minimal config

```json
{
  "providers": [{ "name": "ds", "base_url": "https://api.deepseek.com/v1", "model_name": "deepseek-chat", "api_key": "sk-xxx" }],
  "groups": [{ "name": "main", "reviewer": "ds", "providers": [] }]
}
```

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

| Mode                      | Score       |
| ------------------------- | ----------- |
| Single (DeepSeek direct)  | 3.90 / 5.00 |
| Fusion (3-model ensemble) | 4.05 / 5.00 |
| **Delta**                 | **+0.15**   |

Fusion shows the strongest gains on architecture design questions (+2.8) and algorithm tasks (+1.0).

## Endpoints

| Endpoint                    | Purpose                   |
| --------------------------- | ------------------------- |
| `POST /v1/chat/completions` | OpenAI-compatible         |
| `POST /v1/responses`        | Responses API (for Codex) |
| `GET /v1/models`            | Model list                |
| `GET /health`               | Health check              |

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
