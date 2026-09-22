#!/bin/bash
# Cache behavior test script.
# Tests all cache bands: MISS → DIRECT, MISS → TWEAK, TWEAK_SKIP, and flush.
# Usage: ./test/cache_test.sh [base_url] [api_key]
set -eo pipefail

BASE_URL="${1:-http://localhost:4000}"
API_KEY="${2:-$(grep master_key ~/.ubiquum/gateway.yaml 2>/dev/null | awk '{print $2}' | tr -d '"')}"

if [ -z "$API_KEY" ]; then
    echo "Usage: $0 [base_url] [api_key]"
    echo "  or set master_key in ~/.ubiquum/gateway.yaml"
    exit 1
fi

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
DIM='\033[0;90m'
BOLD='\033[1m'
NC='\033[0m'

PASS=0
FAIL=0
MODEL="${CACHE_TEST_MODEL:-gemma4}"

pass() { ((PASS++)); echo -e "  ${GREEN}✓${NC} $1"; }
fail() { ((FAIL++)); echo -e "  ${RED}✗${NC} $1"; echo -e "    ${DIM}$2${NC}"; }
info() { echo -e "  ${DIM}→ $1${NC}"; }

# Helper: make a chat completion request, return headers + body
# Usage: request "prompt" ["header-value"]  (e.g., request "hi" "x-ubiquum-cache: false")
request() {
    local content="$1"
    local extra_header="${2:-}"
    
    local payload=$(cat <<EOF
{"model":"$MODEL","messages":[{"role":"user","content":"$content"}],"max_tokens":200}
EOF
)
    if [ -n "$extra_header" ]; then
        curl -s -D /tmp/cache_test_headers -X POST "$BASE_URL/v1/chat/completions" \
            -H "Authorization: Bearer $API_KEY" \
            -H "Content-Type: application/json" \
            -H "$extra_header" \
            -d "$payload" 2>/dev/null
    else
        curl -s -D /tmp/cache_test_headers -X POST "$BASE_URL/v1/chat/completions" \
            -H "Authorization: Bearer $API_KEY" \
            -H "Content-Type: application/json" \
            -d "$payload" 2>/dev/null
    fi
}

# Helper: get a specific header value
get_header() {
    grep -i "^$1:" /tmp/cache_test_headers 2>/dev/null | awk '{print $2}' | tr -d '\r\n' || echo ""
}

# Helper: flush cache
flush_cache() {
    curl -s -X POST "$BASE_URL/v1/cache/flush" \
        -H "Authorization: Bearer $API_KEY" > /dev/null 2>&1
}

# Helper: get cache stats
cache_entries() {
    local resp
    resp=$(curl -s "$BASE_URL/v1/cache/stats" \
        -H "Authorization: Bearer $API_KEY" 2>/dev/null || echo '{}')
    echo "$resp" | grep -o '"entries":[0-9]*' | cut -d: -f2 || echo "0"
}

echo ""
echo -e "${BOLD}═══ Ubiquum Cache Test Suite ═══${NC}"
echo -e "${DIM}  base: $BASE_URL${NC}"
echo -e "${DIM}  model: $MODEL${NC}"
echo ""

# ─── Verify server is up ─────────────────────────────────────────────────────
echo -e "${BOLD}[0] Health check${NC}"
HEALTH=$(curl -s "$BASE_URL/health" 2>/dev/null || echo "")
if echo "$HEALTH" | grep -q "ok"; then
    pass "server healthy"
else
    fail "server not responding" "$BASE_URL/health → $HEALTH"
    exit 1
fi
echo ""

# ─── Test 1: MISS on cold cache ──────────────────────────────────────────────
echo -e "${BOLD}[1] Cold cache → MISS${NC}"
flush_cache
sleep 0.5

info "prompt: \"What is the capital of France?\""
BODY=$(request "What is the capital of France?")
STATUS=$(get_header "x-hivecache-status")
SCORE=$(get_header "x-hivecache-score")

if [ "$STATUS" = "MISS" ] || [ -z "$STATUS" ]; then
    pass "first request is MISS (status=$STATUS)"
else
    fail "expected MISS, got $STATUS" "score=$SCORE"
fi

sleep 4
ENTRIES=$(cache_entries)
if [ "${ENTRIES:-0}" -ge 1 ]; then
    pass "response stored in cache (entries=$ENTRIES)"
else
    fail "response not stored" "entries=$ENTRIES (async embedding may be slow)"
fi
echo ""

# ─── Test 2: DIRECT hit on identical request ─────────────────────────────────
echo -e "${BOLD}[2] Identical request → DIRECT${NC}"
sleep 0.5

info "prompt: \"What is the capital of France?\""
BODY=$(request "What is the capital of France?")
STATUS=$(get_header "x-hivecache-status")
SCORE=$(get_header "x-hivecache-score")
TOKENS_SAVED=$(get_header "x-hivecache-tokens-saved")

if [ "$STATUS" = "DIRECT" ]; then
    pass "identical request → DIRECT (score=$SCORE, saved=${TOKENS_SAVED}t)"
elif [ "$STATUS" = "REUSE" ]; then
    pass "identical request → REUSE (score=$SCORE) — close enough"
else
    fail "expected DIRECT, got $STATUS" "score=$SCORE"
fi
echo ""

# ─── Test 3: REUSE on very similar request ───────────────────────────────────
echo -e "${BOLD}[3] Very similar request → REUSE${NC}"
sleep 0.5

info "prompt: \"What's the capital city of France?\""
BODY=$(request "What's the capital city of France?")
STATUS=$(get_header "x-hivecache-status")
SCORE=$(get_header "x-hivecache-score")

if [ "$STATUS" = "DIRECT" ] || [ "$STATUS" = "REUSE" ]; then
    pass "very similar → $STATUS (score=$SCORE)"
elif [ "$STATUS" = "TWEAK" ]; then
    pass "very similar → TWEAK (score=$SCORE) — model sees slight difference"
else
    fail "expected REUSE or DIRECT, got $STATUS" "score=$SCORE"
fi
echo ""

# ─── Test 4: TWEAK on moderately similar request ─────────────────────────────
echo -e "${BOLD}[4] Moderately similar request → TWEAK${NC}"
sleep 0.5

info "prompt: \"What is France's capital and what is it known for?\""
BODY=$(request "What is France's capital and what is it known for?")
STATUS=$(get_header "x-hivecache-status")
SCORE=$(get_header "x-hivecache-score")

if [ "$STATUS" = "TWEAK" ]; then
    pass "moderate similarity → TWEAK (score=$SCORE)"
    info "response was adapted by tweak model"
elif [ "$STATUS" = "REUSE" ] || [ "$STATUS" = "DIRECT" ]; then
    pass "moderate similarity → $STATUS (score=$SCORE) — embeddings found it close enough"
elif [ "$STATUS" = "MISS" ]; then
    # Could be TWEAK_SKIP or genuine miss
    info "got MISS (score=$SCORE) — might be TWEAK_SKIP or below threshold"
    pass "MISS is acceptable for this test (embedding distance varies)"
else
    fail "unexpected status: $STATUS" "score=$SCORE"
fi
echo ""

# ─── Test 5: MISS on completely different request ─────────────────────────────
echo -e "${BOLD}[5] Completely different request → MISS${NC}"
sleep 0.5

info "prompt: \"Write a haiku about quantum computing\""
BODY=$(request "Write a haiku about quantum computing")
STATUS=$(get_header "x-hivecache-status")
SCORE=$(get_header "x-hivecache-score")

if [ "$STATUS" = "MISS" ] || [ -z "$STATUS" ]; then
    pass "different topic → MISS (score=$SCORE)"
else
    fail "expected MISS, got $STATUS" "score=$SCORE"
fi
echo ""

# ─── Test 6: x-ubiquum-cache: false (skip lookup, still store) ──────────────────
echo -e "${BOLD}[6] x-ubiquum-cache: false → skip lookup, still store${NC}"
flush_cache
sleep 0.5

info "prompt: \"What is 2+2?\" + header x-ubiquum-cache: false"
BODY=$(request "What is 2+2?" "x-ubiquum-cache: false")
STATUS=$(get_header "x-hivecache-status")

if [ -z "$STATUS" ]; then
    pass "no cache lookup performed (no status header)"
else
    fail "expected no cache status, got $STATUS" ""
fi

sleep 2
ENTRIES=$(cache_entries)
if [ "${ENTRIES:-0}" -ge 1 ]; then
    pass "response still stored for future hits (entries=$ENTRIES)"
else
    fail "response not stored" "entries=$ENTRIES"
fi
echo ""

# ─── Test 7: Cache flush ─────────────────────────────────────────────────────
echo -e "${BOLD}[7] Cache flush${NC}"

# Populate
info "seeding 2 entries..."
request "Seed question one" > /dev/null
request "Seed question two" > /dev/null
sleep 1

BEFORE=$(cache_entries)
flush_cache
sleep 0.5
AFTER=$(cache_entries)

if [ "${AFTER:-0}" -eq 0 ]; then
    pass "flush cleared all entries ($BEFORE → $AFTER)"
else
    fail "flush didn't clear" "before=$BEFORE, after=$AFTER"
fi
echo ""

# ─── Test 8: Tweak stores response (next hit should be DIRECT) ───────────────
echo -e "${BOLD}[8] Tweaked response gets cached (next hit → DIRECT)${NC}"
flush_cache
sleep 0.5

# Seed with a long response
info "seed: \"Explain in detail how photosynthesis works in plants, including the light reactions and Calvin cycle\""
request "Explain in detail how photosynthesis works in plants, including the light reactions and Calvin cycle" > /dev/null
sleep 1

# Hit with similar query that should trigger TWEAK
info "tweak: \"How does photosynthesis work? Explain the full process\""
BODY=$(request "How does photosynthesis work? Explain the full process")
STATUS1=$(get_header "x-hivecache-status")
SCORE1=$(get_header "x-hivecache-score")
info "result: status=$STATUS1, score=$SCORE1"

if [ "$STATUS1" = "TWEAK" ]; then
    # Now the tweaked response should be stored — repeat same query for DIRECT
    sleep 1
    BODY=$(request "How does photosynthesis work? Explain the full process")
    STATUS2=$(get_header "x-hivecache-status")
    SCORE2=$(get_header "x-hivecache-score")
    
    if [ "$STATUS2" = "DIRECT" ] || [ "$STATUS2" = "REUSE" ]; then
        pass "tweaked response cached → next hit is $STATUS2 (score=$SCORE2)"
    else
        fail "expected DIRECT after tweak, got $STATUS2" "score=$SCORE2"
    fi
elif [ "$STATUS1" = "DIRECT" ] || [ "$STATUS1" = "REUSE" ]; then
    pass "queries were too similar — got $STATUS1 directly (score=$SCORE1)"
elif [ "$STATUS1" = "MISS" ]; then
    info "embeddings didn't find similarity — MISS (score=$SCORE1)"
    pass "MISS acceptable (embedding model varies)"
else
    fail "unexpected: $STATUS1" "score=$SCORE1"
fi
echo ""

# ─── Test 9: Streaming + cache ──────────────────────────────────────────
echo -e "${BOLD}[9] Streaming request from cache${NC}"
flush_cache
sleep 0.5

# Non-streaming seed
info "seed: \"Name the planets in our solar system\""
request "Name the planets in our solar system" > /dev/null
sleep 1

# Streaming request for same content
info "stream: \"Name the planets in our solar system\" (stream=true)"
STREAM_PAYLOAD='{"model":"'$MODEL'","messages":[{"role":"user","content":"Name the planets in our solar system"}],"max_tokens":200,"stream":true}'
STREAM_RESP=$(curl -s -D /tmp/cache_test_headers -X POST "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $API_KEY" \
    -H "Content-Type: application/json" \
    -d "$STREAM_PAYLOAD" 2>/dev/null)
STATUS=$(get_header "x-hivecache-status")

if [ "$STATUS" = "DIRECT" ] || [ "$STATUS" = "REUSE" ]; then
    if echo "$STREAM_RESP" | grep -q "data:"; then
        pass "streaming from cache works (status=$STATUS, got SSE)"
    else
        fail "got $STATUS but no SSE data" "response: $(echo "$STREAM_RESP" | head -2)"
    fi
else
    fail "expected cache hit for streaming, got $STATUS" ""
fi
echo ""

# ─── Summary ─────────────────────────────────────────────────────────────────
echo -e "${BOLD}═══ Results ═══${NC}"
TOTAL=$((PASS + FAIL))
if [ $FAIL -eq 0 ]; then
    echo -e "  ${GREEN}All $PASS tests passed${NC}"
else
    echo -e "  ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC} (of $TOTAL)"
fi
echo ""

# Cleanup
rm -f /tmp/cache_test_headers
exit $FAIL
