#!/bin/bash
# Funding-gate smoke test.
#
# Walks every endpoint that can reach a provider with two keys — one funded,
# one not — and asserts the funded key gets through while the unfunded one is
# refused for lack of budget.
#
# This exists because the funding check moved out of authentication, which saw
# every request, and into the handlers, where the model is known. That is the
# right place and it fails open: a route added without the check simply does
# not have it. Five shipped that way. Unit tests did not notice, because each
# handler passes its own tests either way.
#
# Usage: ./test/integration/funding_gate.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT_DIR"

if [ -f "../ubiquum/.env" ]; then
    set -a; source "../ubiquum/.env"; set +a
fi
export PATH="/opt/homebrew/bin:$PATH"

PORT=${PORT:-4112}
MASTER_KEY="test-master-key-integration"
BASE_URL="http://localhost:$PORT"
BINARY="$ROOT_DIR/tmp/gateway-funding-test"
DB_FILE="$ROOT_DIR/tmp/funding-gate.db"
CONFIG="$ROOT_DIR/tmp/funding-gate.yaml"

PASS=0; FAIL=0
ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

echo "=== Funding gate: every paid route, funded vs unfunded ==="

mkdir -p "$ROOT_DIR/tmp"
rm -f "$DB_FILE"

# A self-contained config: a store so keys can be created, and one echo-backed
# model that needs no upstream credentials.
cat > "$CONFIG" <<YAML
server:
  port: $PORT
  master_key: "$MASTER_KEY"
  max_request_size_mb: 32

database:
  driver: sqlite
  url: $DB_FILE

providers:
  echo-local:
    type: echo

models:
  - name: paid-model
    provider: echo-local
    provider_model: echo
YAML

go build -o "$BINARY" ./cmd/gateway/ || { echo "build failed"; exit 1; }
"$BINARY" serve -config "$CONFIG" >"$ROOT_DIR/tmp/funding-gate.log" 2>&1 &
GATEWAY_PID=$!
trap "kill $GATEWAY_PID 2>/dev/null || true; rm -f $BINARY $CONFIG $DB_FILE" EXIT

for _ in $(seq 1 40); do
    curl -sf "$BASE_URL/health" >/dev/null 2>&1 && break
    sleep 0.25
done
curl -sf "$BASE_URL/health" >/dev/null 2>&1 || {
    echo "gateway did not start; see tmp/funding-gate.log"; tail -20 "$ROOT_DIR/tmp/funding-gate.log"; exit 1;
}

mint_key() { # name budget -> raw key
    curl -s -X POST "$BASE_URL/v1/key/generate" \
        -H "Authorization: Bearer $MASTER_KEY" -H "Content-Type: application/json" \
        -d "{\"name\":\"$1\",\"budget\":$2,\"models\":[\"paid-model\"]}" |
        sed -n 's/.*"key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

FUNDED=$(mint_key funded 100)
[ -n "$FUNDED" ] || { echo "could not mint a funded key"; exit 1; }

# An unfunded key cannot be minted through the admin API by design, so it is
# written straight to the store — which is exactly the state an already-issued
# key is in after this change.
UNFUNDED_RAW="sk-ubq-unfundedtestkey000000000000"
UNFUNDED_HASH=$(printf '%s' "$UNFUNDED_RAW" | shasum -a 256 | cut -d' ' -f1)
sqlite3 "$DB_FILE" \
  "INSERT INTO api_keys_local (id, litellm_key_id, key_prefix, key_name, budget, spend, active, models, created_at, updated_at)
   VALUES ('unfunded-1','$UNFUNDED_HASH','sk-ubq-unfu','unfunded',0,0,1,'[\"paid-model\"]',datetime('now'),datetime('now'));" \
  2>/dev/null || { echo "could not seed the unfunded key (sqlite3 missing?)"; exit 1; }

# path | body | content-type
ROUTES=(
  "/v1/chat/completions|{\"model\":\"paid-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}|application/json"
  "/v1/completions|{\"model\":\"paid-model\",\"prompt\":\"hi\"}|application/json"
  "/v1/messages|{\"model\":\"paid-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":16}|application/json"
  "/v1/responses|{\"model\":\"paid-model\",\"input\":\"hi\"}|application/json"
  "/v1/embeddings|{\"model\":\"paid-model\",\"input\":\"hi\"}|application/json"
  "/v1/moderations|{\"model\":\"paid-model\",\"input\":\"hi\"}|application/json"
  "/v1/images/generations|{\"model\":\"paid-model\",\"prompt\":\"hi\"}|application/json"
  "/v1/audio/speech|{\"model\":\"paid-model\",\"input\":\"hi\",\"voice\":\"alloy\"}|application/json"
)

status_and_body() { # key path body ct -> "<status> <body>"
    curl -s -o /tmp/fg_body -w '%{http_code}' -X POST "$BASE_URL$2" \
        -H "Authorization: Bearer $1" -H "Content-Type: $4" -d "$3"
    echo " $(tr -d '\n' < /tmp/fg_body)"
}

echo ""
echo "unfunded key must be refused for lack of budget:"
for entry in "${ROUTES[@]}"; do
    IFS='|' read -r path body ct <<< "$entry"
    read -r code rest <<< "$(status_and_body "$UNFUNDED_RAW" "$path" "$body" "$ct")"
    if [ "$code" = "429" ] && echo "$rest" | grep -q "no budget"; then
        ok "$path -> 429 no budget"
    else
        bad "$path -> $code ${rest:0:120}"
    fi
done

echo ""
echo "funded key must not be refused for lack of budget:"
for entry in "${ROUTES[@]}"; do
    IFS='|' read -r path body ct <<< "$entry"
    read -r code rest <<< "$(status_and_body "$FUNDED" "$path" "$body" "$ct")"
    # Anything but a funding refusal is fine here: a route may legitimately
    # reject this fixture model for its own reasons. What must never happen is
    # a funded key being told it has no budget.
    if [ "$code" = "429" ] && echo "$rest" | grep -q "no budget"; then
        bad "$path -> refused a funded key"
    else
        ok "$path -> $code (not a funding refusal)"
    fi
done

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
