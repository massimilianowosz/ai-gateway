# Architecture

## Overview

ubiquum-ai-gateway is a high-performance LLM gateway proxy that routes requests to multiple AI providers through a unified OpenAI-compatible API.

## Design Principles

1. **Performance first** — Sub-millisecond proxy overhead. No unnecessary allocations in the hot path.
2. **Minimal dependencies** — ~10 direct Go dependencies vs 100+ in alternatives.
3. **Interface-driven** — Providers, routers, and hooks are defined as interfaces for testability and extensibility.
4. **Config-driven** — Adding a new model deployment requires only a YAML change, not code.
5. **Batch where possible** — Spend logs are buffered for unbudgeted keys; budgeted keys write synchronously so admission checks see recent spend.
6. **Explicit degradation** — Non-critical integrations such as hooks and webhooks degrade gracefully; authentication and budget checks fail closed.

## Package Structure

```
cmd/gateway/          Entry point. Loads config, wires dependencies, starts server.
internal/
  config/             YAML config parsing, env expansion, validation, defaults.
  server/             HTTP server lifecycle, route registration.
  middleware/         Composable middleware (auth, logging, recovery, etc).
  proxy/              HTTP handlers for /v1/* endpoints.
  provider/           Provider interface + cloud-specific implementations.
    azure/            Azure OpenAI.
    vertex/           Google Vertex AI (Gemini, Claude via Model Garden).
    bedrock/          AWS Bedrock with SigV4 signing.
    anthropic/        Direct Anthropic API.
    googleai/         Direct Google AI Studio API.
    openai/           Generic OpenAI-compatible endpoints.
  router/             Deployment selection: strategy, retry, circuit breaker.
  auth/               Authentication middleware (master key + virtual keys).
  admin/              Virtual keys, teams, users, spend query HTTP handlers.
  spend/              Cost tracking: batch writer + query APIs.
  webhook/            Async event dispatch to external URLs.
  store/              GORM store for SQLite/PostgreSQL and migrations.
```

## Request Lifecycle

1. **Receive** — HTTP request arrives at `/v1/chat/completions`
2. **Middleware** — Recovery → RequestID → Logging/Metrics → MaxBody → Auth/Budget → RateLimit → Hooks
3. **Route** — Router selects a healthy deployment for the requested model
4. **Transform** — Provider transforms the OpenAI-format request into provider-specific format
5. **Upstream** — Request sent to cloud provider
6. **Stream/Respond** — Response relayed to client (chunk-by-chunk for streaming)
7. **Track** — Cost record written synchronously for budgeted keys, otherwise buffered in spend writer
8. **Notify** — Webhooks and post-request hook fire asynchronously

## Streaming Architecture

For SSE streaming responses:
- The proxy reads chunks from the upstream provider as they arrive
- Each chunk is immediately flushed to the client (no buffering)
- Cost is calculated from the final usage chunk (sent by most providers)
- If the client disconnects mid-stream, the upstream connection is also closed
- The `StreamReader` interface abstracts provider-specific SSE formats

## Database Schema

```sql
-- Virtual API keys
CREATE TABLE keys (
    id UUID PRIMARY KEY,
    key_hash TEXT UNIQUE NOT NULL,   -- SHA-256 hash of the key value
    key_prefix TEXT NOT NULL,        -- prefix for identification
    team_id UUID REFERENCES teams(id),
    max_budget DECIMAL(12,6),
    budget_duration TEXT,            -- 'daily', 'weekly', 'monthly', NULL=lifetime
    spend DECIMAL(12,6) DEFAULT 0,
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Teams (multi-tenant isolation)
CREATE TABLE teams (
    id UUID PRIMARY KEY,
    alias TEXT UNIQUE,
    models TEXT[],                    -- allowed model names (NULL = all)
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Users
CREATE TABLE users (
    id UUID PRIMARY KEY,
    email TEXT UNIQUE,
    team_id UUID REFERENCES teams(id),
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Spend logs (append-only, batch-inserted)
CREATE TABLE spend_logs (
    id UUID PRIMARY KEY,
    key_id UUID REFERENCES keys(id),
    team_id UUID REFERENCES teams(id),
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    prompt_tokens INT,
    completion_tokens INT,
    total_cost DECIMAL(12,8),
    request_id TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
```

## Extensibility (Hooks)

The gateway supports pre-request and post-response hooks via HTTP callouts.
This enables proprietary features (semantic cache, guardrails) to integrate
without modifying OSS code:

```yaml
hooks:
  pre_request:
    url: http://hivecache:4100/hook/pre
    timeout: 5s
  post_request:
    url: http://hivecache:4100/hook/post
    timeout: 5s
```

The current OSS hook contract is intentionally small: one pre-request endpoint
and one post-request endpoint. Pre-request hooks can reject by returning a
non-2xx status. Provider response rewrite is not part of the v0.1.0 contract.

A hook that returns `{"action": "BLOCKED", "reason": "..."}` will reject the request.
A hook that returns `{"action": "REWRITE", "body": {...}}` will modify the request.
