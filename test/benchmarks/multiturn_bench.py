#!/usr/bin/env python3
"""
Multi-Turn Cache Benchmark — MultiWOZ 2.2
==========================================
Simulates real multi-turn task-oriented dialogues (hotel booking, train search,
restaurant reservations, etc.) to measure semantic cache effectiveness on
conversations with growing message history.

Usage:
    ./test/multiturn_bench.py [--dialogues N] [--url URL] [--model MODEL]

Dataset: MultiWOZ 2.2 (Zang et al., 2020) — 1,000 test dialogues, 5 domains,
         14,744 turns. Apache 2.0 license.
"""

import argparse
import json
import os
import random
import sys
import time
import urllib.request
from collections import Counter, defaultdict
from pathlib import Path

# ─── Config ──────────────────────────────────────────────────────────────────

SCRIPT_DIR = Path(__file__).parent
DATASET_FILE = SCRIPT_DIR / "fixtures" / "multiwoz22_test.json"
DEFAULT_OUTPUT_PREFIX = "/tmp/multiturn_bench"

SYSTEM_PROMPT = (
    "You are a helpful travel assistant for Cambridge, UK. "
    "Help users find and book hotels, restaurants, trains, taxis, and attractions. "
    "Be concise and helpful."
)

# ─── Colors ──────────────────────────────────────────────────────────────────

G = "\033[0;32m"
R = "\033[0;31m"
Y = "\033[0;33m"
D = "\033[0;90m"
B = "\033[1m"
N = "\033[0m"
CYAN = "\033[0;36m"
MAG = "\033[0;35m"

BAND_COLORS = {"DIRECT": G, "REUSE": CYAN, "TWEAK": MAG, "MISS": D}


# ─── Dataset ─────────────────────────────────────────────────────────────────

def download_dataset():
    """Download MultiWOZ 2.2 test set if not present."""
    if DATASET_FILE.exists():
        return

    print(f"{B}Downloading MultiWOZ 2.2 test set...{N}")
    DATASET_FILE.parent.mkdir(parents=True, exist_ok=True)

    base = "https://raw.githubusercontent.com/budzianowski/multiwoz/master/data/MultiWOZ_2.2/test"
    all_dialogues = []
    for i in range(1, 3):
        url = f"{base}/dialogues_{i:03d}.json"
        resp = urllib.request.urlopen(url)
        data = json.loads(resp.read())
        all_dialogues.extend(data)

    with open(DATASET_FILE, "w") as f:
        json.dump(all_dialogues, f)
    print(f"  {G}✓{N} {len(all_dialogues)} dialogues saved")


def load_dialogues(n_dialogues, seed):
    """Load and sample dialogues."""
    with open(DATASET_FILE) as f:
        all_dialogues = json.load(f)

    random.seed(seed)
    if n_dialogues >= len(all_dialogues):
        sample = all_dialogues
    else:
        sample = random.sample(all_dialogues, n_dialogues)

    return sample


def get_active_intent(turn):
    """Extract primary active intent from a USER turn."""
    if turn.get("frames"):
        for frame in turn["frames"]:
            intent = frame.get("state", {}).get("active_intent", "NONE")
            if intent != "NONE":
                return intent
    return "unknown"


def get_domain(turn):
    """Extract primary domain from a USER turn."""
    if turn.get("frames"):
        for frame in turn["frames"]:
            intent = frame.get("state", {}).get("active_intent", "NONE")
            if intent != "NONE":
                return frame.get("service", "unknown")
    return "unknown"


# ─── API Client ──────────────────────────────────────────────────────────────

def send_chat(base_url, api_key, model, messages, max_tokens=30, intent=None):
    """Send chat completion request and return (response, headers, latency_ms)."""
    payload = json.dumps({
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
    }).encode()

    hdrs = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }
    if intent:
        hdrs["x-ubiquum-cache-meta"] = intent

    req = urllib.request.Request(
        f"{base_url}/v1/chat/completions",
        data=payload,
        headers=hdrs,
    )

    start = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=90)
        body = resp.read().decode()
        headers = dict(resp.headers)
        latency_ms = int((time.time() - start) * 1000)
        return json.loads(body), headers, latency_ms
    except Exception as e:
        latency_ms = int((time.time() - start) * 1000)
        return {"error": str(e)}, {}, latency_ms


# ─── Main ────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Multi-Turn Cache Benchmark")
    parser.add_argument("--dialogues", "-n", type=int, default=100,
                        help="Number of dialogues per run (default: 100)")
    parser.add_argument("--runs", "-r", type=int, default=1,
                        help="Number of runs (default: 1). Run 1 starts cold, next runs are incremental.")
    parser.add_argument("--url", type=str, default="http://localhost:4000",
                        help="Gateway URL (default: http://localhost:4000)")
    parser.add_argument("--model", type=str, default=os.environ.get("BENCH_MODEL", "gemma4"),
                        help="Model to use")
    parser.add_argument("--mock", action="store_true",
                        help="Use echo provider (instant responses, no LLM calls)")
    parser.add_argument("--no-flush", action="store_true",
                        help="Skip cache flush (warm start)")
    parser.add_argument("--max-tokens", type=int, default=30)
    parser.add_argument("--delay", type=float, default=None,
                        help="Delay between requests (default: 0.05 with --mock, 0.3 otherwise)")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--output", type=str, default=DEFAULT_OUTPUT_PREFIX,
                        help="Output prefix for results/summary files")
    parser.add_argument("--api-key", type=str, default=None,
                        help="API key (or UBIQUUM_API_KEY env var)")
    args = parser.parse_args()

    # Defaults based on mock mode
    if args.delay is None:
        args.delay = 0.05 if args.mock else 0.3
    if args.mock:
        args.model = "echo-bench"

    RESULTS_FILE = Path(f"{args.output}_results.jsonl")
    SUMMARY_FILE = Path(f"{args.output}_summary.json")

    # Get API key: CLI > env var > ~/.ubiquum/gateway.yaml
    api_key = args.api_key or os.environ.get("UBIQUUM_API_KEY", "")
    if not api_key:
        config_path = Path.home() / ".ubiquum" / "gateway.yaml"
        if config_path.exists():
            for line in config_path.read_text().splitlines():
                if "master_key" in line:
                    val = line.split(":", 1)[1].strip().strip('"')
                    if val and not val.startswith("${"):
                        api_key = val
                    break
    if not api_key:
        print(f"{R}Error: no API key found. Use --api-key, UBIQUUM_API_KEY env, or set master_key in ~/.ubiquum/gateway.yaml{N}")
        sys.exit(1)

    # Download dataset
    download_dataset()

    # Load dialogues
    dialogues = load_dialogues(args.dialogues, args.seed)

    # Header
    total_turns = sum(
        sum(1 for t in d["turns"] if t["speaker"] == "USER")
        for d in dialogues
    )
    print()
    print(f"{B}╔══════════════════════════════════════════════════════════╗{N}")
    print(f"{B}║   Ubiquum Cache Benchmark — MultiWOZ 2.2 Multi-Turn  ║{N}")
    print(f"{B}╚══════════════════════════════════════════════════════════╝{N}")
    print()
    print(f"  {D}gateway:{N}    {args.url}")
    print(f"  {D}model:{N}      {args.model}")
    mode_str = f"{CYAN}mock (echo provider){N}" if args.mock else "real LLM"
    print(f"  {D}mode:{N}       {mode_str}")
    print(f"  {D}dialogues:{N}  {len(dialogues)} per run × {args.runs} run(s)")
    print(f"  {D}turns:{N}      ~{total_turns} user turns per run")
    print(f"  {D}flush:{N}      {'no (warm start)' if args.no_flush else 'yes (cold start)'}")
    print(f"  {D}delay:{N}      {args.delay}s")
    print()

    # Health check
    try:
        resp = urllib.request.urlopen(f"{args.url}/health", timeout=5)
        if b"ok" not in resp.read():
            raise Exception("not ok")
    except Exception:
        print(f"  {R}✗{N} gateway not responding at {args.url}")
        sys.exit(1)

    # Flush cache
    if not args.no_flush:
        print(f"{D}Flushing cache...{N}")
        try:
            req = urllib.request.Request(
                f"{args.url}/v1/cache/flush",
                method="POST",
                headers={"Authorization": f"Bearer {api_key}"},
            )
            urllib.request.urlopen(req, timeout=10)
        except Exception:
            pass
        time.sleep(1)
        print(f"  {G}✓{N} cache flushed")
    else:
        print(f"  {Y}⚡{N} skipping flush (warm start)")
    print()

    # ─── Run benchmark (multi-run) ─────────────────────────────────────────────
    RESULTS_FILE.unlink(missing_ok=True)

    # Per-run summaries
    run_summaries = []
    query_num = 0
    total_queries = total_turns * args.runs

    for run_idx in range(1, args.runs + 1):
        # Per-run counters
        bands = Counter()
        tokens_saved = 0
        errors = 0
        latencies_hit = []
        latencies_miss = []
        turn_stats = defaultdict(Counter)
        domain_stats = defaultdict(Counter)
        intent_stats = defaultdict(Counter)

        # Re-sample dialogues with different seed per run for variety
        run_dialogues = load_dialogues(args.dialogues, args.seed + run_idx - 1)
        run_turns = sum(
            sum(1 for t in d["turns"] if t["speaker"] == "USER")
            for d in run_dialogues
        )

        print(f"{B}═══ Run {run_idx}/{args.runs} — {len(run_dialogues)} dialogues, ~{run_turns} turns ═══{N}")
        print()

        for dlg_idx, dialogue in enumerate(run_dialogues):
            turns = dialogue["turns"]
            messages = [{"role": "system", "content": SYSTEM_PROMPT}]
            user_turn_num = 0

            for turn in turns:
                speaker = turn["speaker"]
                utterance = turn["utterance"]

                if speaker == "USER":
                    user_turn_num += 1
                    query_num += 1
                    messages.append({"role": "user", "content": utterance})

                    intent = get_active_intent(turn)
                    domain = get_domain(turn)

                    resp_data, headers, latency = send_chat(
                        args.url, api_key, args.model, messages, args.max_tokens,
                        intent=intent
                    )

                    if "error" in resp_data:
                        errors += 1
                        print(f"  [{query_num:>4}] {R}ERROR{N} {latency:>5}ms │ {utterance[:60]}")
                        messages.append({"role": "assistant", "content": "I apologize, let me help you with that."})
                        time.sleep(args.delay)
                        continue

                    h = {k.lower(): v for k, v in headers.items()}
                    status = h.get("x-hivecache-status", "").strip()
                    score = h.get("x-hivecache-score", "").strip()
                    tsaved = int(h.get("x-hivecache-tokens-saved", "0").strip() or 0)
                    matched_meta = h.get("x-hivecache-matched-meta", "").strip()

                    band = status if status else "MISS"
                    bands[band] += 1
                    tokens_saved += tsaved
                    turn_stats[user_turn_num][band] += 1
                    domain_stats[domain][band] += 1
                    intent_stats[intent][band] += 1

                    # Correctness: compare current intent with cached entry's intent
                    if band != "MISS" and intent and matched_meta:
                        correct = (intent == matched_meta)
                    else:
                        correct = None

                    if band == "MISS":
                        latencies_miss.append(latency)
                    else:
                        latencies_hit.append(latency)

                    color = BAND_COLORS.get(band, D)
                    dlg_label = f"D{dlg_idx+1:>3}T{user_turn_num}"
                    print(
                        f"  [{query_num:>4}] {color}{band:<6}{N} "
                        f"{latency:>5}ms │ {D}{dlg_label}{N} [{domain}] {utterance[:55]}"
                    )

                    try:
                        assistant_msg = resp_data["choices"][0]["message"]["content"]
                    except (KeyError, IndexError):
                        assistant_msg = "I can help you with that."
                    messages.append({"role": "assistant", "content": assistant_msg})

                    record = {
                        "run": run_idx,
                        "query_num": query_num,
                        "dialogue_idx": dlg_idx,
                        "turn": user_turn_num,
                        "domain": domain,
                        "intent": intent,
                        "matched_intent": matched_meta,
                        "correct": correct,
                        "band": band,
                        "score": score,
                        "tokens_saved": tsaved,
                        "latency_ms": latency,
                        "msg_count": len(messages),
                    }
                    with open(RESULTS_FILE, "a") as f:
                        f.write(json.dumps(record) + "\n")

                    time.sleep(args.delay)

                elif speaker == "SYSTEM":
                    if messages and messages[-1]["role"] == "user":
                        pass

        # Per-run summary
        run_total = sum(bands.values())
        run_hits = run_total - bands.get("MISS", 0)
        run_rate = 100 * run_hits / run_total if run_total > 0 else 0

        run_summaries.append({
            "run": run_idx,
            "total": run_total,
            "hits": run_hits,
            "hit_rate": round(run_rate, 1),
            "bands": dict(bands),
            "tokens_saved": tokens_saved,
            "errors": errors,
            "turn_stats": {str(k): dict(v) for k, v in turn_stats.items()},
            "domain_stats": {k: dict(v) for k, v in domain_stats.items()},
        })

        print()
        band_str = f"{G}D:{bands.get('DIRECT',0)}{N} {CYAN}R:{bands.get('REUSE',0)}{N} {MAG}T:{bands.get('TWEAK',0)}{N} {D}M:{bands.get('MISS',0)}{N}"
        print(f"  {B}Run {run_idx}:{N} hit rate {G}{run_rate:.1f}%{N} ({run_hits}/{run_total}) — {band_str} — tokens saved: {CYAN}{tokens_saved}{N}")
        if errors:
            print(f"  {R}Errors: {errors}{N}")
        print()

    # ─── Final Summary ───────────────────────────────────────────────────────
    print(f"{B}╔══════════════════════════════════════════════════════════╗{N}")
    print(f"{B}║               MULTI-RUN SUMMARY                         ║{N}")
    print(f"{B}╚══════════════════════════════════════════════════════════╝{N}")
    print()

    if args.runs > 1:
        print(f"  ┌──────┬──────────┬────────┬────────┬────────┬────────┐")
        print(f"  │ Run  │ Hit Rate │ DIRECT │ REUSE  │ TWEAK  │  MISS  │")
        print(f"  ├──────┼──────────┼────────┼────────┼────────┼────────┤")
        for s in run_summaries:
            print(
                f"  │ {s['run']:>4} │ {s['hit_rate']:>5.1f}%  │"
                f" {s['bands'].get('DIRECT',0):>6} │ {s['bands'].get('REUSE',0):>6} │"
                f" {s['bands'].get('TWEAK',0):>6} │ {s['bands'].get('MISS',0):>6} │"
            )
        print(f"  └──────┴──────────┴────────┴────────┴────────┴────────┘")
        print()

        first_rate = run_summaries[0]["hit_rate"]
        last_rate = run_summaries[-1]["hit_rate"]
        improvement = last_rate - first_rate
        print(f"  {B}Improvement:{N} {first_rate}% → {G}{last_rate}%{N} (+{improvement:.1f} pp)")
        print()

    # Aggregate
    agg_total = sum(s["total"] for s in run_summaries)
    agg_hits = sum(s["hits"] for s in run_summaries)
    agg_rate = 100 * agg_hits / agg_total if agg_total > 0 else 0
    agg_tokens = sum(s["tokens_saved"] for s in run_summaries)

    print(f"  {B}Overall:{N} {agg_hits}/{agg_total} ({G}{agg_rate:.1f}%{N}) — tokens saved: {CYAN}{agg_tokens}{N}")
    print()

    # Per-turn-position hit rate (last run)
    if run_summaries:
        last = run_summaries[-1]
        last_turn_stats = last.get("turn_stats", {})
        if last_turn_stats:
            print(f"  {B}Hit rate by turn position (last run):{N}")
            print(f"    {'Turn':<6} {'Hits':>5} {'Total':>6} {'Rate':>7}")
            for turn_pos in sorted(last_turn_stats.keys(), key=int)[:8]:
                bc = last_turn_stats[turn_pos]
                t_total = sum(bc.values())
                t_hits = t_total - bc.get("MISS", 0)
                t_rate = 100 * t_hits / t_total if t_total > 0 else 0
                print(f"    T{turn_pos:<5} {t_hits:>5} {t_total:>6} {t_rate:>6.1f}%")
            print()

    # Save summary
    summary = {
        "benchmark": "multiwoz22_multiturn",
        "mock": args.mock,
        "runs": args.runs,
        "dialogues_per_run": args.dialogues,
        "run_summaries": run_summaries,
        "overall": {
            "total": agg_total,
            "hits": agg_hits,
            "hit_rate_pct": round(agg_rate, 1),
            "tokens_saved": agg_tokens,
        },
        "model": args.model,
        "seed": args.seed,
    }
    with open(SUMMARY_FILE, "w") as f:
        json.dump(summary, f, indent=2)
    print(f"  {D}Results: {RESULTS_FILE}{N}")
    print(f"  {D}Summary: {SUMMARY_FILE}{N}")


if __name__ == "__main__":
    main()
