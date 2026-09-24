<p align="center">
  <img src="internal/console/assets/ai-gateway-logo.png" alt="Ubiquum AI Gateway" width="120">
</p>

# Ubiquum AI Gateway

Ubiquum AI Gateway is an appliance-oriented AI egress control point. A firewall
can deny direct access to external AI services and allow only this gateway to
reach approved providers. Agents and applications receive a local
OpenAI-compatible endpoint, while operators control credentials, model access,
budgets, rate limits, residency and audit data from an embedded web console.

The gateway is a single Go binary. The management console is compiled into the
binary and does not require Node.js or a separate control-plane service.

## What is included

- OpenAI-compatible Chat Completions, Responses, files, embeddings, images,
  audio and moderation endpoints
- native Anthropic Messages compatibility
- Azure OpenAI, OpenAI, Anthropic, Bedrock, Vertex AI, Google AI and generic
  OpenAI-compatible providers
- encrypted local BYOK vault with live provider registration
- virtual identities with hard budgets, model ACLs, rate limits and expiry
- short-lived agent JWT support through an optional JWKS authority
- per-request provider, token, latency and cost attribution
- PII, credential, prompt-injection and moderation guardrails
- routing, retry, circuit breaking and provider failover
- Prometheus metrics, signed webhooks and an audit trail

## Appliance flow

```text
agents / applications
        │  virtual key or agent JWT
        ▼
Ubiquum AI Gateway
  identity → policy → budget → guardrail → routing → accounting
        │
        ▼  only permitted AI egress path
approved cloud providers or local models
```

Network interception is intentionally outside this repository. The surrounding
firewall is expected to enforce the egress path.

## Quick start

```bash
cp .env.example .env
docker compose up --build
```

Open `http://localhost:4000/console/`. Add a BYOK connection and its models,
then create an identity with a budget and model allow-list. Provider secrets are
encrypted at rest in `~/.ubiquum/credentials.vault`; the separate 256-bit key is
created in `~/.ubiquum/credentials.key` with mode `0600`.

For production, mount the vault key from a TPM-backed or platform secret store
and enable secure cookies behind TLS.

## Client example

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://ai-gateway.internal/v1",
    api_key="sk-ubq-...",
)

response = client.chat.completions.create(
    model="approved-gpt",
    messages=[{"role": "user", "content": "Hello"}],
)
```

## Management console

The console provides:

- overview of spend, identities, models and failures;
- encrypted BYOK connections, added and removed without restarting;
- agent/application credentials shown only once;
- hard budgets, RPM limits and model access;
- request activity and cost attribution;
- readiness and integration endpoints.

Management sessions use signed HttpOnly `SameSite=Strict` cookies. Mutating
requests require a per-session CSRF token. The bearer master key remains
available for local CLI and automation compatibility.

## Important routes

| Route | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible chat |
| `POST /v1/responses` | Responses API |
| `POST /v1/messages` | Anthropic Messages API |
| `GET /v1/models` | Models visible to the identity |
| `GET /health` | Liveness |
| `GET /ready` | Data-plane and governance readiness |
| `GET /metrics` | Prometheus metrics |
| `GET /console/` | Embedded management console |

The complete contract is in [api/openapi.yaml](api/openapi.yaml).

## Development

```bash
go test ./...
go vet ./...
make build
```

The Go module is `github.com/ubiquum-ai/ubiquum-ai-gateway`. This repository has
its own Git history and contains no dependency on another gateway repository.

## License

GNU Affero General Public License v3.0. See [LICENSE](LICENSE).
