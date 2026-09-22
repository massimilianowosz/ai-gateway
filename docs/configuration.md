# Configuration Reference

## Overview

The gateway is configured via a YAML file. Environment variables can be referenced
using `${VAR_NAME}` syntax and will be expanded at load time.

## Server

```yaml
server:
  port: 4000                    # HTTP listen port (default: 4000)
  master_key: ${GATEWAY_MASTER_KEY}  # Required. Admin API authentication key.
  max_request_size_mb: 32       # Max request body size in MB (default: 32)
```

### Scoped upstream token forwarding

Local shims such as `ubiquum-cli` can authenticate to Ubiquum with a
virtual key while forwarding a separate upstream provider bearer token for the
selected provider.

```yaml
server:
  upstream_token_forwarding:
    enabled: true
    allowed_providers:
      - github_copilot
    header: X-Ubiquum-Upstream-Authorization
```

When enabled, the gateway expects `X-Ubiquum-Upstream-Provider` alongside the
forwarded bearer token. The forwarding header is stripped before downstream
middleware and redacted from hook payloads.

## Database

```yaml
database:
  url: ${DATABASE_URL}          # PostgreSQL connection string
  pool_size: 10                 # Connection pool size (default: 10)
  batch_write_interval: 60s     # How often to flush spend logs (default: 60s)
```

If `database.url` is omitted, the gateway runs without persistence (no keys, teams, or spend tracking — useful for testing).

## Providers

Provider entries hold credentials and endpoint settings once. Models reference
providers by name.

```yaml
providers:
  azure-prod:
    type: azure_openai
    api_base: ${AZURE_OPENAI_API_BASE}
    api_key: ${AZURE_OPENAI_API_KEY}
    api_version: "2024-10-21"
```

### Provider Types

| Provider | `type` value | Required fields |
|----------|--------------|-----------------|
| Azure OpenAI | `azure_openai` | `api_base`, `api_key`, `api_version` |
| Vertex AI | `vertex` | `project`, `location` |
| AWS Bedrock | `bedrock` | `region`, `api_key` |
| Anthropic | `anthropic` | `api_key` |
| Google AI Studio | `google_ai` | `api_key` |
| OpenAI | `openai` | `api_key` |
| OpenAI-compatible | `openai_compatible` | `api_base`, `api_key` |
| GitHub Copilot | `github_copilot` | `api_base` optional; Copilot bearer is usually forwarded by `ubiquum-cli` |

## Models

Each entry defines a model that clients can request by name.

```yaml
providers:
  azure-prod:
    type: azure_openai
    api_base: ${AZURE_OPENAI_API_BASE}
    api_key: ${AZURE_OPENAI_API_KEY}
    api_version: "2024-10-21"

models:
  - name: gpt-4o                     # Client-facing model name
    provider: azure-prod              # Provider name from providers
    provider_model: gpt-4o-2024-11-20 # Provider-specific model identifier
    drop_params:                      # Parameters to strip before sending
      - stream_options.include_usage
```

### Model aliases

Aliases add familiar client-facing IDs without renaming an established model
or duplicating its deployments:

```yaml
model_aliases:
  gpt-4o: azure-gpt-4o
```

`gpt-4o` replaces `azure-gpt-4o` in `/v1/models` and routes to the same
deployment pool. The established `azure-gpt-4o` ID remains callable for
backward compatibility, and model allowlists may contain either name. Alias
targets must be configured model names; collisions and alias chains are
rejected when the gateway starts.

### Multiple deployments (load balancing)

Define the same `name` with different providers or regions:

```yaml
providers:
  azure-east:
    type: azure_openai
    api_base: https://east.openai.azure.com
    api_key: ${AZURE_EAST_KEY}

  azure-west:
    type: azure_openai
    api_base: https://west.openai.azure.com
    api_key: ${AZURE_WEST_KEY}

models:
  - name: gpt-4o
    provider: azure-east
    provider_model: gpt-4o-2024-11-20

  - name: gpt-4o
    provider: azure-west
    provider_model: gpt-4o-2024-11-20
```

The router will distribute requests across both deployments.

## Router

```yaml
router:
  strategy: shuffle             # shuffle | round-robin | latency (default: shuffle)
  retries: 3                    # Number of retries on failure (default: 3)
  retry_delay: 5s               # Delay between retries (default: 5s)
  circuit_breaker:
    threshold: 3                # Failures before marking unhealthy (default: 3)
    recovery: 60s               # Time before retrying unhealthy deployment (default: 60s)
```

## Webhooks

```yaml
webhooks:
  - url: http://backend:8000/api/webhooks/gateway
    secret: ${WEBHOOK_SECRET}
    events:
      - spend_tracked           # Fired after each request cost is recorded
      - budget_crossed          # Fired when a key or team crosses its budget
      - key_created
      - key_deleted
```

Webhook requests are signed with `X-Ubiquum-Signature: sha256=<hex>` when
`secret` is configured.

## Pricing

```yaml
pricing:
  remote_url: https://example.com/model_prices.json  # Optional override
  disable_remote_refresh: false                      # Set true for offline/CI
```

## Hooks

External services called during request processing. Used for guardrails, caching, etc.

```yaml
hooks:
  pre_request:
    url: http://guardrail-service:8000/check
    timeout: 3s
  post_request:
    url: http://analytics-service:8000/log
    timeout: 5s
```

Hooks are single HTTP endpoints. The pre-request hook rejects the request only
when it returns a non-2xx response; call failures fail open.

## Guardrails

Scans model-visible text in Chat Completions, Anthropic Messages, OpenAI
Responses and legacy Completions requests before it is proxied. PII and secret
policies run immediately after authentication, before workflow, cache, request
logging and HiveState. Scanners run inline in the gateway; `url` is the legacy
path to an external LLM Guard service and is ignored once inline scanners are
configured.

```yaml
guardrail:
  enabled: true
  fail_open: true          # a scanner error lets the request through
  timeout: 5s              # per-call timeout for ML-based scanners
  guardrails:              # policies offered to tenants
    - prompt-injection
    - toxicity
    - sensitive-data       # PII Redaction
    - sensitive-data-block # PII Blocking (opt-in)
  pii:
    enabled: true
  secrets:
    enabled: true
  injection:
    enabled: true
  moderation:
    enabled: true
    model: azure-gpt-5-nano
```

A tenant's `guardrails_config` decides which of these actually run for its
traffic. A policy the tenant has not configured stays on — except
`sensitive-data-block`, which is opt-in: listing it here does not turn it on for
anyone who has not chosen it, so no tenant is silently moved from redaction to
refusal.

### The two PII policies

`sensitive-data` and `sensitive-data-block` read the same PII, and differ only
in what they do with it:

| Policy | Name in the portal | Effect |
| --- | --- | --- |
| `sensitive-data` | PII Redaction | replaces each match with an `[ENTITY]` placeholder and forwards the request |
| `sensitive-data-block` | PII Blocking | refuses with `403` and forwards nothing |

`sensitive-data` also keeps blocking on the secrets scanner: an API key or a
password is not a PII entity, and redacting one is not what a caller means by
"protect it".

Enabling both is not a contradiction — blocking is evaluated first, so it wins
and nothing is redacted-and-sent behind it.

Which entities either policy looks for comes from the portal's
`anonymization_configs` row for the tenant ("PII Entity Settings"), read through
`tenants.litellm_team_id`: `EMAIL_ADDRESS`, `PHONE_NUMBER`, `PERSON`,
`CREDIT_CARD`, `CRYPTO`, `IBAN_CODE`, `IP_ADDRESS`, `US_SSN`, `US_BANK_NUMBER`,
`LOCATION`. No row at all means every entity applies; a row with an empty list
means the tenant unticked everything and neither policy acts on it. The Italian
national identifiers the scanner has always recognised — codice fiscale and
partita IVA — have no checkbox and stay on either way.

Redaction rewrites exactly the text the scanner reads: chat/message content,
Responses API `instructions`/`input`, and legacy Completions `prompt` values.
It needs the inline engine; a deployment still pointed at the legacy `url` can
only block.

## Live compression

Compresses the newest tool output entering a prompt — the result of the call
the agent just made — before it reaches the provider.

This is the opposite end of the prompt from HiveState. History compression
rewrites the head, which is why it needs the
[prefix-cache guard](#hivestate-prefix-cache-guard); live compression touches
only the tail, past the reach of any provider cache, so shrinking it is pure
gain. The two run together: live compression first, so history compression sees
an already-slimmer delta.

```yaml
live_compression:
    enabled: false           # default off
    allow_lossy: false       # collapse repeated log lines, truncate long JSON arrays
    canary_percent: 0        # 0 or 100 = everyone; otherwise a stable share by API key
    protected_tools: []      # extends the built-in list; "mcp__server__*" covers a server
    min_bytes: 512
    max_bytes: 4194304
    min_gain_percent: 5
    live_turns: 1            # trailing user turns treated as live
    json_min_items: 20       # arrays shorter than this are never truncated
    json_keep_head: 5
    json_keep_tail: 3
    diff_context: 3          # unchanged lines kept around each change
    csv_min_rows: 30         # tables shorter than this are never truncated
    csv_keep_head: 10
    csv_keep_tail: 5
```

### What it does

| Content | Transform | Lossy |
|---|---|---|
| JSON | Whitespace compaction. Numbers keep their written form — `1.0` stays `1.0`, integers above 2^53 keep their digits. | No |
| JSON | Long arrays keep the first 5, the last 3, and **every element reporting an error**; a marker records how many were dropped. | Yes |
| JSON | Arrays of plain numbers become `{count, min, max, mean, median, first, last}`, computed over every element. | Yes |
| JSON | Byte-identical elements collapse to one carrying a `_ubiquum_repeated` count. Only exact duplicates — deciding which differing field is unimportant is the agent's call. | Yes |
| Logs | Runs of identical lines collapse to one plus a count; terminal escape sequences are stripped. Order is never changed. | Yes |
| Diff | Context narrows to 3 lines around each change. Hunk headers are recomputed and hunks split where a window is cut, so the patch still applies. | Yes |
| CSV / TSV | Keeps the header row, the first 10 and last 5 data rows, and **every row reporting an error**. Re-encoded through a CSV writer, so quoting survives. | Yes |

A line naming a failure, or belonging to a stack trace, is never collapsed —
including when it repeats, because a check that failed five times failed five
times. The same rule governs JSON arrays and CSV rows: elements reporting an
error survive wherever they sit.

Detection order matters where formats overlap. A diff is line-oriented like a
log, and a table of timestamps reads log-shaped, so the more specific signal
wins: JSON, then diff, then CSV, then logs.

### What it never touches

`Read`, `Glob`, `Grep`, `Write`, `Edit`, `MultiEdit`, `WebSearch`, `WebFetch`
and the editor tools. An agent quotes those back by line number and offset, so
compressing them corrupts its next action rather than merely costing accuracy.
MCP wrappers unwrap to the real tool name, and a tool result whose name cannot
be resolved is treated as protected.

Shell output is deliberately compressible — build logs and test output are the
large repetitive payloads this exists for. Add `Bash` to `protected_tools` to
change that.

Only the live zone is rewritten: the newest user turn and everything after it,
which in an agentic loop is that turn plus the tool results it went on to
produce. `live_turns` widens the zone to the last N user turns. Everything
earlier, including the system prompt and the tools array, is forwarded
byte-for-byte.

A tool result is not itself a user turn, even where the wire format delivers it
inside a `user` message, as Anthropic's does. Treating one as a turn boundary
would slide the live zone forward on every request, so each tool result would
be compressed once and then re-sent at full size — changing the prompt at a
point the provider's prefix cache had already covered, which costs more than
the compression saves.

### Failure behaviour

Every failure path forwards the original body: an unparseable request, a
transform that produced something no smaller, a panic on hostile input. A
caller can opt out per request with `x-ubiquum-live-compression: false`, which
outranks the operator's setting.

### Headers and metrics

| | |
|---|---|
| `x-livezone-blocks` | Blocks compressed in this request |
| `x-livezone-bytes-saved` | Request bytes removed |
| `ubiquum_live_compression_blocks_total{transformer}` | Blocks compressed, by transformer |
| `ubiquum_live_compression_skips_total{reason}` | Requests declined, by cause |
| `ubiquum_live_compression_bytes_saved_total` | Cumulative bytes removed |

## HiveState prefix-cache guard

HiveState compresses old conversation history into a structured state summary
inserted after the system messages. That is a rewrite at the *head* of the
prompt, and provider prompt caches are positional: they match a byte prefix,
not a set of messages. Rewriting at the head therefore drops every later
message out of the cached prefix, so it gets re-billed at full input price even
though its bytes are unchanged.

The break-even is set by the provider's cache-read discount:

```
cost_passthrough = ratio x cached_tokens + uncached_tokens
cost_rewritten   = tokens_after_rewrite      # nothing is cached any more
```

On Anthropic (cache reads at 0.1x) a rewrite must shrink the prompt below
roughly 10% of its original size just to break even — a preserved recent window
alone usually exceeds that budget, so cutting tokens can *raise* the bill. On
OpenAI (0.5x) the bar is near 50% and a rewrite often still wins.

The guard runs this comparison before every extraction and skips HiveState when
passthrough is cheaper. It is enabled by default.

```yaml
hivestate:
    enabled: true
    model: llama-3-70b
    threshold: 4000
    step_window: 4
    prefix_cache_guard:
        enabled: true                    # default true; false restores legacy behaviour
        anthropic_cache_read_ratio: 0.1  # cache-read price multiplier for /v1/messages
        openai_cache_read_ratio: 0.5     # cache-read price multiplier for /v1/chat/completions
        assume_implicit_cache: false     # opt-in: guess a warm cache on providers with no markers
        min_cacheable_tokens: 1024       # below this, no implicit provider cache is assumed
        state_summary_chars: 1200        # assumed size of the injected state summary
        margin: 0.10                     # advantage a rewrite must show before busting the cache
                                         # clamped to [0, 0.95]: at 1.0 the guard would skip everything
```

The guard decides whether to **rewrite the body**. It does not decide which
model serves the request: HiveRoute still runs on a skipped request, so routing
and cache protection stay independent. A skip that would otherwise have avoided
a state extraction still pays for one when HiveRoute is enabled, because routing
needs the extracted difficulty.

The guard is only consulted once a conversation is over `threshold`. Below it
HiveState would pass the request through untouched, so there is no rewrite to
weigh and no `x-hivestate-prefix-cache` header is emitted.

### How the cached prefix is detected

| Surface | Signal | Behaviour |
|---|---|---|
| `/v1/messages` | `cache_control` breakpoints | The cached prefix runs through the last marked message, and includes the system prompt ahead of it. A breakpoint only in `system`/`tools` freezes the preamble, which HiveState never rewrites, so it does not block compression. |
| `/v1/chat/completions`, `/v1/responses` | none (automatic caching) | **Off by default.** With `assume_implicit_cache: true`, a conversation above `min_cacheable_tokens` with two or more user turns is assumed warm through the newest turn. On Responses, a request chaining on `previous_response_id` keeps its history server-side, so there is no client-side prefix to protect. |

Anthropic's markers are evidence; the implicit case is a guess. A wrong guess
silently disables HiveState on traffic that had no cache to protect, which is
why it must be enabled deliberately.

Detection is deliberately conservative: a false positive skips HiveState, which
costs a missed compression. A false negative would silently destroy a live
cache, which costs money on every subsequent turn. For the same reason the
predicted size of a rewrite includes room for any context CCR may re-inject —
the comparison has to weigh the body that is actually sent, not a smaller one.

### Metrics

`/metrics` exposes the aggregate view the per-request headers cannot give:

| Series | Meaning |
|---|---|
| `ubiquum_prefix_cache_decisions_total{reason,api}` | Guard outcomes by cause and surface |
| `ubiquum_ccr_injections_total` | Requests where recovered context was re-injected |
| `ubiquum_ccr_files_total`, `ubiquum_ccr_tokens_total` | Volume recovered |
| `ubiquum_ccr_budget_skips_total` | Retrievals declined to preserve the savings |
| `ubiquum_prompt_cache_read_tokens_total`, `..._write_tokens_total` | Provider-reported cache usage |

Labels are drawn from fixed vocabularies. Request, session and key identifiers
are never used as labels.

### Response headers

| Header | Meaning |
|---|---|
| `x-hivestate-prefix-cache` | Guard outcome: `prefix_cache_cheaper`, `rewrite_cheaper`, `no_cached_prefix`, `no_cache_discount`, `unknown_token_count` |
| `x-hivestate-cached-prefix-tokens` | Tokens the guard believed were cached |
| `x-hivestate-fallback` | Set to the guard reason when it skipped HiveState, or `rewrite_would_be_noop` when the rewrite would have changed nothing |

See [Cache-aware billing](#cache-aware-billing) for how the resulting cache
hits are measured and charged.

## CCR scoping (Compress-Cache-Retrieve)

When HiveState compresses old history away, CCR keeps the file contents the
conversation had already read, so the ones the agent is still working with can
be re-injected into a later prompt. Files enter a working set when read, are
refreshed whenever they are mentioned again, and decay out after 5 turns
without a mention. Re-reading the same file with unchanged content is not a new
read: the compressed history still carries the original, so the entry keeps its
original age rather than being renewed on every turn.

That content is verbatim user data — source files, tool output — so it is filed
under a scope and is only ever retrievable under the identical scope:

```
scope = (team_id, key_hash, session_id)
```

There is no shared bucket and no global fallback. A request whose scope is
under-specified gets no CCR at all, which is the safe failure; shared memory is
not.

### Session identity

| Source | Behaviour |
|---|---|
| `x-ubiquum-session` request header | Used verbatim when present. |
| Derived | Otherwise the session is hashed from the system messages plus the first user turn. That anchor stays byte-stable as a conversation grows but differs between conversations, so CCR works without client changes. |
| Neither | No session can be derived (no system or user message): CCR is disabled for the request. |

An API key with neither a team nor a key hash also yields an invalid scope.

### Bounds

Storage is bounded by entry count and by bytes, so one tenant cannot displace
another and no combination of large files can exhaust memory:

- each scope holds at most 32 files and 1 MiB of content, evicting its **own** oldest first;
- at most 512 scopes are live, evicting the least recently used;
- the store retains at most 128 MiB in total, shedding least-recently-used scopes.

Entries expire after 30 minutes. `Forget(scope)` drops a scope outright. The
decay counter is per-scope, so unrelated traffic cannot age a working set out
from under an idle conversation.

### Placement in the prompt

Retrieval runs identically on both of HiveState's paths — a fresh state
extraction and a state-cache hit. A session whose state keeps hitting the cache
takes only the second path, so if it did not also record the files it saw, the
working set would never fill and retrieval would always come back empty.

Recovered files are spliced in **as late in the request as they can go**, never
next to the state summary at the head, and the "don't re-read these files"
instruction rides with them rather than being appended to the system prompt. The
head sits inside the region a provider prompt cache covers, so injecting there
would invalidate the cached prefix on every retrieval — the same failure the
[prefix-cache guard](#hivestate-prefix-cache-guard) exists to prevent. The tail
is past the reach of any cache, so retrieval costs nothing beyond the tokens
themselves.

The one thing that outranks lateness is tool-call adjacency. A tool call must be
immediately followed by its response in every wire format — and Anthropic
carries tool results inside `user` messages, so "the final user turn" is not a
safe anchor in an agentic loop. The insertion point therefore walks back over a
trailing tool exchange rather than landing inside it, which costs at most one
assistant step of cache reach and keeps the request valid.

Injection also stays budget-gated: it happens only while the rewritten prompt
remains under 80% of the original token count. The prefix-cache guard assumes
that same ceiling when predicting what a rewrite will cost.

## Cache-aware billing

Providers report prompt-cache usage in incompatible shapes, and the gateway
normalizes them into one internal model where `prompt_tokens` is the inclusive
input total:

```
prompt_tokens = uncached + cached + cache_creation
```

| Provider | Wire fields | Normalization |
|---|---|---|
| OpenAI / Azure / OpenAI-compatible | `prompt_tokens_details.cached_tokens` | Already inclusive; only the breakdown is recovered. |
| Anthropic, Anthropic-on-Vertex | `cache_read_input_tokens`, `cache_creation_input_tokens` | Reported *alongside* `input_tokens`, so both are added into the prompt total. |
| Bedrock Converse | `cacheReadInputTokens`, `cacheWriteInputTokens` | Same as Anthropic; `totalTokens` is recomputed rather than trusted. |

Each category is billed at its own tariff, taken from
`cache_read_input_token_cost` and `cache_creation_input_token_cost` in the
price catalog. Typical multipliers are 0.1x read and 1.25x write on Anthropic,
and 0.5x read on OpenAI. Per-model overrides in `pricing:` can set every
tariff. When a catalog entry carries no cache tariff the input price is used —
never zero, so a missing tariff cannot make tokens free.

Spend records gain three columns:

| Column | Meaning |
|---|---|
| `cached_prompt_tokens` | Subset of `prompt_tokens` served from the provider cache |
| `cache_creation_tokens` | Subset of `prompt_tokens` written into the provider cache |
| `cost_estimated` | True when the provider reported no cache breakdown, so the charge assumes nothing was cached and is an upper bound |

Both cache columns are subsets of `prompt_tokens`, not additions to it —
summing them into the total would double count.

The inclusive model is internal accounting only. A `/v1/messages` response keeps
Anthropic's own shape, with the cache counters split back out beside
`input_tokens`:

```json
"usage": {"input_tokens": 200, "cache_read_input_tokens": 100000, "output_tokens": 300}
```

Reporting the inclusive number as `input_tokens` would make a client billing off
the response overcharge a cached request by roughly the cache discount. The
cache fields are omitted entirely when the provider reported no breakdown.

> **Upgrade note.** Anthropic-family traffic previously billed only
> `input_tokens`, which excludes cache reads and writes, so cached requests
> were undercharged. After this change the same traffic reports its full input
> volume and costs more per request than it did before, while cached reads are
> discounted to their real tariff. Historical spend rows are not rewritten:
> their `cached_prompt_tokens` stay zero and `cost_estimated` stays false, so
> any before/after comparison should be cut at the upgrade date.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `GATEWAY_MASTER_KEY` | Master API key for admin operations |
| `DATABASE_URL` | PostgreSQL connection string |
| `AZURE_OPENAI_API_BASE` | Azure OpenAI endpoint URL |
| `AZURE_OPENAI_API_KEY` | Azure OpenAI API key |
| `VERTEXAI_PROJECT` | Google Cloud project ID |
| `VERTEXAI_LOCATION` | Vertex AI region (e.g., europe-west1) |
| `AWS_REGION` | AWS region for Bedrock |
| `GROQ_API_KEY` | Groq API key (or any OpenAI-compatible) |
