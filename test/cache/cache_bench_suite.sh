#!/bin/bash
# Full Cache Benchmark Suite
# Runs the complete benchmark workflow:
#   1. Cold start (flush + 1000 single-turn + 100 multiturn dialogues)
#   2. Warm start (same workload, no flush — measures benefit of existing cache)
#   3. Incremental runs (N rounds to see how hit rate grows)
#
# Usage: ./test/cache_bench_suite.sh [options]
#   --mock              use echo provider (no LLM calls, fast)
#   --incremental N     number of incremental runs (default: 3)
#   --url URL           gateway URL (default: http://localhost:4000)
#   --seed N            random seed (default: 42)
#
# Examples:
#   ./test/cache_bench_suite.sh --mock                    # full suite, fast
#   ./test/cache_bench_suite.sh --mock --incremental 5    # 5 incremental rounds
#   ./test/cache_bench_suite.sh                           # real LLM (slow!)

set -o pipefail

# ─── Parse args ──────────────────────────────────────────────────────────────
MOCK_FLAG=""
INCREMENTAL=3
URL="http://localhost:4000"
SEED=42

while [[ $# -gt 0 ]]; do
    case "$1" in
        --mock)          MOCK_FLAG="--mock"; shift ;;
        --incremental)   INCREMENTAL="$2"; shift 2 ;;
        --url)           URL="$2"; shift 2 ;;
        --seed)          SEED="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RESULTS_DIR="$SCRIPT_DIR/results"
mkdir -p "$RESULTS_DIR"
B='\033[1m'; N='\033[0m'; G='\033[0;32m'; D='\033[0;90m'; Y='\033[0;33m'

echo ""
echo -e "${B}╔══════════════════════════════════════════════════════════════╗${N}"
echo -e "${B}║          Ubiquum Cache — Full Benchmark Suite             ║${N}"
echo -e "${B}╚══════════════════════════════════════════════════════════════╝${N}"
echo ""
echo -e "  ${D}mode:${N}         $([ -n "$MOCK_FLAG" ] && echo "mock (echo)" || echo "real LLM")"
echo -e "  ${D}incremental:${N}  $INCREMENTAL rounds"
echo -e "  ${D}url:${N}          $URL"
echo ""

# ─── Phase 1: Cold Start ────────────────────────────────────────────────────
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo -e "${B}  PHASE 1: Cold Start (flush → 1000 queries + 100 dialogues) ${N}"
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo ""

echo -e "${D}── Single-turn (Banking77, 1000 queries) ──${N}"
"$SCRIPT_DIR/cache_bench.sh" $MOCK_FLAG -n 1000 -r 1 -u "$URL" --seed "$SEED" --output "$RESULTS_DIR/p1_single"

echo ""
echo -e "${D}── Multi-turn (MultiWOZ, 100 dialogues) ──${N}"
python3 "$SCRIPT_DIR/multiturn_bench.py" $MOCK_FLAG -n 100 -r 1 --url "$URL" --seed "$SEED" --output "$RESULTS_DIR/p1_multi"

# ─── Phase 2: Warm Start ────────────────────────────────────────────────────
echo ""
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo -e "${B}  PHASE 2: Warm Start (no flush, same workload)              ${N}"
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo ""

echo -e "${D}── Single-turn (Banking77, 1000 queries) ──${N}"
"$SCRIPT_DIR/cache_bench.sh" $MOCK_FLAG -n 1000 -r 1 --no-flush -u "$URL" --seed $((SEED + 50)) --output "$RESULTS_DIR/p2_single"

echo ""
echo -e "${D}── Multi-turn (MultiWOZ, 100 dialogues) ──${N}"
python3 "$SCRIPT_DIR/multiturn_bench.py" $MOCK_FLAG -n 100 -r 1 --no-flush --url "$URL" --seed $((SEED + 50)) --output "$RESULTS_DIR/p2_multi"

# ─── Phase 3: Incremental Runs ──────────────────────────────────────────────
echo ""
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo -e "${B}  PHASE 3: Incremental Runs ($INCREMENTAL rounds, no flush)  ${N}"
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo ""

echo -e "${D}── Single-turn (Banking77, $INCREMENTAL × 1000 queries) ──${N}"
"$SCRIPT_DIR/cache_bench.sh" $MOCK_FLAG -n 1000 -r "$INCREMENTAL" --no-flush -u "$URL" --seed $((SEED + 100)) --output "$RESULTS_DIR/p3_single"

echo ""
echo -e "${D}── Multi-turn (MultiWOZ, $INCREMENTAL × 100 dialogues) ──${N}"
python3 "$SCRIPT_DIR/multiturn_bench.py" $MOCK_FLAG -n 100 -r "$INCREMENTAL" --no-flush --url "$URL" --seed $((SEED + 100)) --output "$RESULTS_DIR/p3_multi"

# ─── Done ────────────────────────────────────────────────────────────────────
echo ""
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo -e "${G}  ✓ Full benchmark suite complete${N}"
echo -e "${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
echo ""
echo -e "  ${D}Results in:${N} $RESULTS_DIR/"
echo -e "  ${D}  p1_single_*  (cold start, single-turn)${N}"
echo -e "  ${D}  p1_multi_*   (cold start, multi-turn)${N}"
echo -e "  ${D}  p2_single_*  (warm start, single-turn)${N}"
echo -e "  ${D}  p2_multi_*   (warm start, multi-turn)${N}"
echo -e "  ${D}  p3_single_*  (incremental, single-turn)${N}"
echo -e "  ${D}  p3_multi_*   (incremental, multi-turn)${N}"
echo ""
