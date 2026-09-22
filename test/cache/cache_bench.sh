#!/bin/bash
# Cache Benchmark — Banking77 Real Traffic Simulation
#
# Simulates a real customer support chatbot: N random queries from Banking77
# (13k queries, 77 intents, ~170 paraphrases each). No seeding — the cache
# builds up organically as traffic flows.
#
# Usage: ./test/cache_bench.sh [options]
#   -n, --queries N     number of queries per run (default: 100)
#   -r, --runs N        number of runs (default: 1). Run 1 starts cold, next runs are incremental.
#   -u, --url URL       gateway URL (default: http://localhost:4000)
#   --no-flush          skip cache flush before first run (start warm)
#   --mock              use echo provider (instant responses, no LLM calls)
#   --model MODEL       model to use (default: $BENCH_MODEL or gemma4)
#   --delay SECONDS     delay between queries (default: 0.1 with mock, 0.5 otherwise)
#   --seed N            random seed (default: 42)
#
# Examples:
#   ./test/cache_bench.sh --mock -n 200 -r 3          # 3 incremental runs, no LLM
#   ./test/cache_bench.sh --mock --no-flush -n 100    # warm start, no flush
#   ./test/cache_bench.sh -n 50                       # real LLM, 50 queries

set -o pipefail

# ─── Parse arguments ─────────────────────────────────────────────────────────
QUERIES=100
RUNS=1
BASE_URL="http://localhost:4000"
USE_MOCK=false
NO_FLUSH=false
MODEL="${BENCH_MODEL:-gemma4}"
DELAY=""
SEED="${BENCH_SEED:-42}"
MAX_TOKENS="${BENCH_MAX_TOKENS:-30}"
OUTPUT_PREFIX="/tmp/cache_bench"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--queries) QUERIES="$2"; shift 2 ;;
        -r|--runs)    RUNS="$2"; shift 2 ;;
        -u|--url)     BASE_URL="$2"; shift 2 ;;
        --mock)       USE_MOCK=true; shift ;;
        --no-flush)   NO_FLUSH=true; shift ;;
        --model)      MODEL="$2"; shift 2 ;;
        --delay)      DELAY="$2"; shift 2 ;;
        --seed)       SEED="$2"; shift 2 ;;
        --output)     OUTPUT_PREFIX="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Default delay: fast for mock, slower for real LLM
if [ -z "$DELAY" ]; then
    if [ "$USE_MOCK" = true ]; then DELAY="0.05"; else DELAY="0.5"; fi
fi

# If mock mode, override model to use echo provider
if [ "$USE_MOCK" = true ]; then
    MODEL="echo-bench"
fi

API_KEY="${UBIQUUM_API_KEY:-$(grep master_key ~/.ubiquum/gateway.yaml 2>/dev/null | awk '{print $2}' | tr -d '"')}"

if [ -z "$API_KEY" ] || [[ "$API_KEY" == *'${'* ]]; then
    echo "Error: no valid API key. Set UBIQUUM_API_KEY or master_key in ~/.ubiquum/gateway.yaml"
    exit 1
fi

# Colors
G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; D='\033[0;90m'; B='\033[1m'; N='\033[0m'
CYAN='\033[0;36m'; MAG='\033[0;35m'

# ─── Download dataset ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DATASET="$SCRIPT_DIR/fixtures/banking77.csv"

if [ ! -f "$DATASET" ]; then
    echo -e "${B}Downloading Banking77 dataset...${N}"
    mkdir -p "$SCRIPT_DIR/fixtures"
    curl -sL "https://raw.githubusercontent.com/PolyAI-LDN/task-specific-datasets/master/banking_data/train.csv" > /tmp/banking77_train.csv
    curl -sL "https://raw.githubusercontent.com/PolyAI-LDN/task-specific-datasets/master/banking_data/test.csv" > /tmp/banking77_test.csv
    # Merge (skip headers)
    head -1 /tmp/banking77_train.csv > "$DATASET"
    tail -n +2 /tmp/banking77_train.csv >> "$DATASET"
    tail -n +2 /tmp/banking77_test.csv >> "$DATASET"
    rm /tmp/banking77_train.csv /tmp/banking77_test.csv
    echo -e "  ${G}✓${N} $(wc -l < "$DATASET") queries saved to test/fixtures/"
fi

TOTAL_LINES=$(($(wc -l < "$DATASET") - 1))
echo ""
echo -e "${B}╔══════════════════════════════════════════════════════════╗${N}"
echo -e "${B}║     Ubiquum Cache Benchmark — Banking77 Traffic       ║${N}"
echo -e "${B}╚══════════════════════════════════════════════════════════╝${N}"
echo ""
echo -e "  ${D}gateway:${N}  $BASE_URL"
echo -e "  ${D}model:${N}    $MODEL"
echo -e "  ${D}mode:${N}     $([ "$USE_MOCK" = true ] && echo "${CYAN}mock (echo provider)${N}" || echo "real LLM")"
echo -e "  ${D}queries:${N}  $QUERIES per run × $RUNS run(s) (from $TOTAL_LINES total)"
echo -e "  ${D}flush:${N}    $([ "$NO_FLUSH" = true ] && echo "no (warm start)" || echo "yes (cold start)")"
echo -e "  ${D}delay:${N}    ${DELAY}s"
echo ""

# ─── Health check ────────────────────────────────────────────────────────────
HEALTH=$(curl -s "$BASE_URL/health" 2>/dev/null || echo "")
if ! echo "$HEALTH" | grep -q "ok"; then
    echo -e "  ${R}✗${N} gateway not responding"
    exit 1
fi

# ─── Flush cache ─────────────────────────────────────────────────────────────
if [ "$NO_FLUSH" = false ]; then
    echo -e "${D}Flushing cache...${N}"
    curl -s -X POST "$BASE_URL/v1/cache/flush" -H "Authorization: Bearer $API_KEY" > /dev/null 2>&1
    sleep 1
    echo -e "  ${G}✓${N} cache flushed"
    echo ""
else
    echo -e "  ${Y}⚡${N} skipping flush (warm start)"
    echo ""
fi

# ─── Pick random queries (enough for all runs) ──────────────────────────────
# Use awk with seed for reproducible shuffle
TOTAL_NEEDED=$((QUERIES * RUNS))
SAMPLE_FILE="/tmp/banking77_sample.txt"
awk -v seed="$SEED" -v n="$TOTAL_NEEDED" '
BEGIN { srand(seed) }
NR == 1 { next }
{ lines[NR] = $0; count++ }
END {
    # Fisher-Yates shuffle
    for (i = count; i > 1; i--) {
        j = int(rand() * i) + 1
        tmp = lines[j+1]; lines[j+1] = lines[i+1]; lines[i+1] = tmp
    }
    for (i = 2; i <= n+1 && i <= count+1; i++) print lines[i]
}
' "$DATASET" > "$SAMPLE_FILE"

ACTUAL=$(wc -l < "$SAMPLE_FILE" | tr -d ' ')

# ─── System prompt ───────────────────────────────────────────────────────────
SYSTEM_PROMPT="You are a helpful banking customer support assistant. Answer concisely based on standard banking policies."

# ─── Run benchmark (multi-run) ───────────────────────────────────────────────
RESULTS_FILE="${OUTPUT_PREFIX}_results.jsonl"
> "$RESULTS_FILE"

# Per-run summary arrays
declare -a RUN_HITS RUN_TOTALS RUN_RATES RUN_DIRECT RUN_REUSE RUN_TWEAK RUN_MISS

GLOBAL_LINE=0

for RUN in $(seq 1 $RUNS); do
    # Calculate line range for this run
    RUN_START=$(( (RUN - 1) * QUERIES + 1 ))
    RUN_END=$(( RUN * QUERIES ))
    if [ $RUN_END -gt $ACTUAL ]; then RUN_END=$ACTUAL; fi

    echo -e "${B}=== Run $RUN/$RUNS - queries $RUN_START to $RUN_END ===${N}"
    echo ""

    # Per-run counters
    R_DIRECT=0; R_REUSE=0; R_TWEAK=0; R_MISS=0; R_ERRORS=0
    R_TOKENS_SAVED=0
    R_LATENCY_SUM_HIT=0; R_LATENCY_COUNT_HIT=0
    R_LATENCY_SUM_MISS=0; R_LATENCY_COUNT_MISS=0

    LINE_NUM=0
    while IFS= read -r line; do
        LINE_NUM=$((LINE_NUM + 1))

        # Skip lines not in this run's range
        if [ $LINE_NUM -lt $RUN_START ] || [ $LINE_NUM -gt $RUN_END ]; then
            continue
        fi

        GLOBAL_LINE=$((GLOBAL_LINE + 1))
        line="${line//$'\r'/}"

        # Parse CSV: Banking77 format is "query text",intent_label
        # Intent is always the last field (after last comma, never quoted)
        INTENT="${line##*,}"
        if [[ "$line" == \"* ]]; then
            # Quoted query: strip leading quote and trailing ",intent"
            QUERY="${line:1}"
            QUERY="${QUERY%\",*}"
        else
            QUERY="${line%,*}"
        fi

        # JSON-escape the query
        QUERY_ESC=$(printf '%s' "$QUERY" | sed 's/\\/\\\\/g;s/"/\\"/g;s/	/\\t/g')

        # Send request (with intent metadata for correctness tracking)
        RESP=$(curl -s -w '\n%{time_total}' -D /tmp/bench_headers -X POST "$BASE_URL/v1/chat/completions" \
            -H "Authorization: Bearer $API_KEY" \
            -H "Content-Type: application/json" \
            -H "x-ubiquum-cache-meta: $INTENT" \
            --max-time 60 \
            -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"system\",\"content\":\"$SYSTEM_PROMPT\"},{\"role\":\"user\",\"content\":\"$QUERY_ESC\"}],\"max_tokens\":$MAX_TOKENS}" \
            2>/dev/null)

        LATENCY_S=$(echo "$RESP" | tail -1)
        RESP=$(echo "$RESP" | sed '$d')
        LATENCY=$(awk "BEGIN{printf \"%d\", $LATENCY_S * 1000}" 2>/dev/null || echo "0")

        if echo "$RESP" | grep -q '"error"'; then
            R_ERRORS=$((R_ERRORS + 1))
            printf "  ${R}✗${N} [%d] ERROR: %s\n" "$GLOBAL_LINE" "$(echo "$RESP" | head -c 80)"
            sleep "$DELAY"
            continue
        fi

        # Parse cache headers
        STATUS=$(grep -i "^x-hivecache-status:" /tmp/bench_headers 2>/dev/null | awk '{print $2}' | tr -d '\r\n' || echo "")
        SCORE=$(grep -i "^x-hivecache-score:" /tmp/bench_headers 2>/dev/null | awk '{print $2}' | tr -d '\r\n' || echo "")
        TSAVED=$(grep -i "^x-hivecache-tokens-saved:" /tmp/bench_headers 2>/dev/null | awk '{print $2}' | tr -d '\r\n' || echo "0")
        MATCHED_META=$(grep -i "^x-hivecache-matched-meta:" /tmp/bench_headers 2>/dev/null | sed 's/^[^:]*: //' | tr -d '\r\n' || echo "")

        BAND="${STATUS:-MISS}"
        case "$BAND" in
            DIRECT) R_DIRECT=$((R_DIRECT + 1)); R_TOKENS_SAVED=$((R_TOKENS_SAVED + TSAVED))
                    R_LATENCY_SUM_HIT=$((R_LATENCY_SUM_HIT + LATENCY)); R_LATENCY_COUNT_HIT=$((R_LATENCY_COUNT_HIT + 1)) ;;
            REUSE)  R_REUSE=$((R_REUSE + 1)); R_TOKENS_SAVED=$((R_TOKENS_SAVED + TSAVED))
                    R_LATENCY_SUM_HIT=$((R_LATENCY_SUM_HIT + LATENCY)); R_LATENCY_COUNT_HIT=$((R_LATENCY_COUNT_HIT + 1)) ;;
            TWEAK)  R_TWEAK=$((R_TWEAK + 1)); R_TOKENS_SAVED=$((R_TOKENS_SAVED + TSAVED))
                    R_LATENCY_SUM_HIT=$((R_LATENCY_SUM_HIT + LATENCY)); R_LATENCY_COUNT_HIT=$((R_LATENCY_COUNT_HIT + 1)) ;;
            *)      R_MISS=$((R_MISS + 1)); BAND="MISS"
                    R_LATENCY_SUM_MISS=$((R_LATENCY_SUM_MISS + LATENCY)); R_LATENCY_COUNT_MISS=$((R_LATENCY_COUNT_MISS + 1)) ;;
        esac

        # Print
        case "$BAND" in
            DIRECT) COLOR="$G" ;; REUSE) COLOR="$CYAN" ;; TWEAK) COLOR="$MAG" ;; *) COLOR="$D" ;;
        esac
        printf "  [%3d] ${COLOR}%-6s${N} %4dms │ %s\n" "$GLOBAL_LINE" "$BAND" "$LATENCY" "$QUERY"

        # Correctness: compare true intent vs matched cache entry intent
        if [ -n "$MATCHED_META" ] && [ "$BAND" != "MISS" ]; then
            CORRECT=$([ "$INTENT" = "$MATCHED_META" ] && echo "true" || echo "false")
        else
            CORRECT="null"
        fi

        echo "{\"run\":$RUN,\"i\":$GLOBAL_LINE,\"true_intent\":\"$INTENT\",\"matched_intent\":\"$MATCHED_META\",\"correct\":$CORRECT,\"band\":\"$BAND\",\"score\":\"$SCORE\",\"tokens_saved\":$TSAVED,\"latency_ms\":$LATENCY}" >> "$RESULTS_FILE"

        sleep "$DELAY"
    done < "$SAMPLE_FILE"

    # Per-run results
    R_TOTAL=$((R_DIRECT + R_REUSE + R_TWEAK + R_MISS))
    R_HITS=$((R_DIRECT + R_REUSE + R_TWEAK))
    R_RATE=$(awk "BEGIN{printf \"%.1f\", 100*$R_HITS/($R_TOTAL+0.001)}")

    RUN_HITS[$RUN]=$R_HITS
    RUN_TOTALS[$RUN]=$R_TOTAL
    RUN_RATES[$RUN]=$R_RATE
    RUN_DIRECT[$RUN]=$R_DIRECT
    RUN_REUSE[$RUN]=$R_REUSE
    RUN_TWEAK[$RUN]=$R_TWEAK
    RUN_MISS[$RUN]=$R_MISS

    echo ""
    echo -e "  ${B}Run $RUN:${N} hit rate ${G}${R_RATE}%${N} (${R_HITS}/${R_TOTAL}) — ${G}D:${R_DIRECT}${N} ${CYAN}R:${R_REUSE}${N} ${MAG}T:${R_TWEAK}${N} ${D}M:${R_MISS}${N} — tokens saved: ${CYAN}${R_TOKENS_SAVED}${N}"
    [ $R_ERRORS -gt 0 ] && echo -e "  ${R}Errors: ${R_ERRORS}${N}"
    echo ""
done

# ─── Final Summary ───────────────────────────────────────────────────────────
echo -e "${B}╔══════════════════════════════════════════════════════════╗${N}"
echo -e "${B}║                 MULTI-RUN SUMMARY                       ║${N}"
echo -e "${B}╚══════════════════════════════════════════════════════════╝${N}"
echo ""

if [ $RUNS -gt 1 ]; then
    echo -e "  ┌──────┬──────────┬────────┬────────┬────────┬────────┐"
    echo -e "  │ Run  │ Hit Rate │ DIRECT │ REUSE  │ TWEAK  │  MISS  │"
    echo -e "  ├──────┼──────────┼────────┼────────┼────────┼────────┤"
    for R in $(seq 1 $RUNS); do
        printf "  │ %4d │ %6s%% │ %6d │ %6d │ %6d │ %6d │\n" \
            "$R" "${RUN_RATES[$R]}" "${RUN_DIRECT[$R]}" "${RUN_REUSE[$R]}" "${RUN_TWEAK[$R]}" "${RUN_MISS[$R]}"
    done
    echo -e "  └──────┴──────────┴────────┴────────┴────────┴────────┘"
    echo ""

    # Show improvement from run 1 to last
    FIRST_RATE="${RUN_RATES[1]}"
    LAST_RATE="${RUN_RATES[$RUNS]}"
    IMPROVEMENT=$(awk "BEGIN{printf \"%.1f\", $LAST_RATE - $FIRST_RATE}")
    echo -e "  ${B}Improvement:${N} ${FIRST_RATE}% → ${G}${LAST_RATE}%${N} (+${IMPROVEMENT} pp)"
    echo ""
fi

# Aggregate totals
TOTAL_ALL=0; HITS_ALL=0; TOKENS_ALL=0
for R in $(seq 1 $RUNS); do
    TOTAL_ALL=$((TOTAL_ALL + ${RUN_TOTALS[$R]}))
    HITS_ALL=$((HITS_ALL + ${RUN_HITS[$R]}))
done
RATE_ALL=$(awk "BEGIN{printf \"%.1f\", 100*$HITS_ALL/($TOTAL_ALL+0.001)}")

echo -e "  ${B}Overall:${N} ${HITS_ALL}/${TOTAL_ALL} (${G}${RATE_ALL}%${N})"
echo ""

# Top intents hit
echo -e "  ${B}Top cached intents:${N}"
sort "$RESULTS_FILE" | grep -v '"band":"MISS"' | grep -v '"band":""' \
    | sed 's/.*"intent":"//' | sed 's/".*//' \
    | sort | uniq -c | sort -rn | head -10 \
    | while read count intent; do
        printf "    %3d hits  %s\n" "$count" "$intent"
    done
echo ""

# Save summary
SUMMARY="${OUTPUT_PREFIX}_summary.json"
cat > "$SUMMARY" <<EOF
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "config": {"model":"$MODEL","queries_per_run":$QUERIES,"runs":$RUNS,"mock":$USE_MOCK,"delay":"$DELAY"},
  "runs": [$(for R in $(seq 1 $RUNS); do
    [ $R -gt 1 ] && printf ","
    printf '{"run":%d,"hit_rate":%s,"direct":%d,"reuse":%d,"tweak":%d,"miss":%d}' \
        "$R" "${RUN_RATES[$R]}" "${RUN_DIRECT[$R]}" "${RUN_REUSE[$R]}" "${RUN_TWEAK[$R]}" "${RUN_MISS[$R]}"
  done)],
  "overall": {"hit_rate":$RATE_ALL,"total":$TOTAL_ALL,"hits":$HITS_ALL}
}
EOF
echo -e "  ${D}Results: $RESULTS_FILE${N}"
echo -e "  ${D}Summary: $SUMMARY${N}"
echo ""
