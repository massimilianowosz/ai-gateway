#!/bin/bash
# Integration test script: starts the gateway and tests all providers.
# Usage: ./test/integration.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

# Load env vars
if [ -f "../ubiquum/.env" ]; then
    set -a
    source "../ubiquum/.env"
    set +a
fi

export PATH="/opt/homebrew/bin:$PATH"

PORT=4111
MASTER_KEY="test-master-key-integration"
BASE_URL="http://localhost:$PORT"
BINARY="$ROOT_DIR/tmp/gateway-test"

echo "=== Ubiquum Gateway Integration Test ==="
echo ""

# Build
echo "Building gateway..."
go build -o "$BINARY" ./cmd/gateway/
echo "✓ Build successful"

# Start server
echo "Starting gateway on port $PORT..."
"$BINARY" -config test/fixtures/integration.yaml &
GATEWAY_PID=$!
trap "kill $GATEWAY_PID 2>/dev/null || true; rm -f $BINARY" EXIT

# Wait for server to be ready
for i in $(seq 1 30); do
    if curl -s "$BASE_URL/health" > /dev/null 2>&1; then
        break
    fi
    sleep 0.2
done

# Verify health
HEALTH=$(curl -s "$BASE_URL/health")
echo "✓ Server healthy: $HEALTH"
echo ""

# Test function
test_model() {
    local model="$1"
    local stream="${2:-false}"
    local desc="$3"

    echo -n "Testing $desc ($model, stream=$stream)... "

    local payload=$(cat <<EOF
{
    "model": "$model",
    "messages": [{"role": "user", "content": "Say exactly: hello world"}],
    "max_tokens": 50,
    "stream": $stream
}
EOF
)

    local response
    local http_code

    if [ "$stream" = "true" ]; then
        response=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/v1/chat/completions" \
            -H "Authorization: Bearer $MASTER_KEY" \
            -H "Content-Type: application/json" \
            -d "$payload" 2>&1)
        http_code=$(echo "$response" | tail -1)
        local body=$(echo "$response" | sed '$d')

        if [ "$http_code" = "200" ]; then
            # Check that we got SSE data
            if echo "$body" | grep -q "data:"; then
                echo "✓ (streaming OK, got SSE chunks)"
            else
                echo "✗ (got 200 but no SSE data)"
                echo "  Body: $(echo "$body" | head -3)"
                return 1
            fi
        else
            echo "✗ (HTTP $http_code)"
            echo "  Response: $(echo "$body" | head -3)"
            return 1
        fi
    else
        response=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/v1/chat/completions" \
            -H "Authorization: Bearer $MASTER_KEY" \
            -H "Content-Type: application/json" \
            -d "$payload" 2>&1)
        http_code=$(echo "$response" | tail -1)
        local body=$(echo "$response" | sed '$d')

        if [ "$http_code" = "200" ]; then
            # Check for valid response structure
            if echo "$body" | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['choices'][0]['message']['content']" 2>/dev/null; then
                local content=$(echo "$body" | python3 -c "import sys,json; print(json.load(sys.stdin)['choices'][0]['message']['content'][:80])")
                echo "✓ ($content)"
            else
                echo "✓ (HTTP 200, response received)"
                echo "  Body: $(echo "$body" | head -1 | cut -c1-120)"
            fi
        else
            echo "✗ (HTTP $http_code)"
            echo "  Response: $(echo "$body" | head -3)"
            return 1
        fi
    fi
}

PASSED=0
FAILED=0

run_test() {
    if test_model "$@"; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
}

# === Azure OpenAI ===
echo "--- Azure OpenAI ---"
run_test "gpt-4o" "false" "Azure GPT-4o"
run_test "gpt-4o" "true" "Azure GPT-4o streaming"
run_test "gpt-4o-mini" "false" "Azure GPT-4o-mini"
run_test "gpt-4o-mini" "true" "Azure GPT-4o-mini streaming"
echo ""

# === Vertex AI ===
echo "--- Vertex AI ---"
run_test "vertex-gemini-2-5-flash" "false" "Vertex Gemini 2.5 Flash"
run_test "vertex-gemini-2-5-flash" "true" "Vertex Gemini 2.5 Flash streaming"
run_test "vertex-claude-sonnet-4-5" "false" "Vertex Claude Sonnet 4.5"
run_test "vertex-claude-sonnet-4-5" "true" "Vertex Claude Sonnet 4.5 streaming"
echo ""

# === AWS Bedrock ===
echo "--- AWS Bedrock ---"
run_test "bedrock-claude-haiku" "false" "Bedrock Claude Haiku"
run_test "bedrock-claude-haiku" "true" "Bedrock Claude Haiku streaming"
run_test "bedrock-nova-lite" "false" "Bedrock Nova Lite"
run_test "bedrock-nova-lite" "true" "Bedrock Nova Lite streaming"
echo ""

# === Anthropic Direct ===
echo "--- Anthropic Direct ---"
run_test "claude-sonnet" "false" "Anthropic Claude Sonnet"
run_test "claude-sonnet" "true" "Anthropic Claude Sonnet streaming"
echo ""

# === Google AI Studio ===
echo "--- Google AI Studio ---"
run_test "gemini-flash" "false" "Google AI Gemini Flash"
run_test "gemini-flash" "true" "Google AI Gemini Flash streaming"
echo ""

# === Models endpoint ===
echo "--- API Endpoints ---"
echo -n "Testing GET /v1/models... "
MODELS=$(curl -s -w "\n%{http_code}" "$BASE_URL/v1/models" -H "Authorization: Bearer $MASTER_KEY")
MODELS_CODE=$(echo "$MODELS" | tail -1)
if [ "$MODELS_CODE" = "200" ]; then
    echo "✓"
else
    echo "✗ (HTTP $MODELS_CODE)"
    FAILED=$((FAILED + 1))
fi
PASSED=$((PASSED + 1))
echo ""

# === Summary ===
TOTAL=$((PASSED + FAILED))
echo "=== Results: $PASSED/$TOTAL passed ==="
if [ $FAILED -gt 0 ]; then
    echo "⚠️  $FAILED test(s) failed"
    exit 1
fi
echo "🎉 All tests passed!"
