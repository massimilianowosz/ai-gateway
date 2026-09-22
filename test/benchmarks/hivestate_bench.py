#!/usr/bin/env python3
"""
HiveState Benchmark — ABCD (Action-Based Conversations Dataset)
================================================================
Measures HiveState's ability to extract conversational state from multi-turn
customer service dialogues. Uses ABCD's Action State Tracking ground truth.

Ground truth from ABCD:
  - Intent: scenario.subflow (55 distinct intents)
  - Constraints: cumulative slot values from action turns (Value Filling)
  - Scenario: personal + order info (name, email, phone, order_id, etc.)

Metrics:
  - Constraint preservation: slot recall/precision/F1 against cumulative GT values
  - Intent accuracy: extracted intent vs ground-truth subflow (LLM judge)
  - Token reduction: original tokens vs state-compressed tokens
  - Extraction latency: p50/p95/avg of state extraction time

Usage:
    ./test/hivestate_bench.py [--dialogues N] [--url URL] [--model MODEL]
    ./test/hivestate_bench.py --mock   # uses echo provider (tests flow only)
"""

import argparse
import gzip
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request
from collections import Counter, defaultdict
from pathlib import Path

# ─── Config ──────────────────────────────────────────────────────────────────

SCRIPT_DIR = Path(__file__).parent
DATASET_FILE = SCRIPT_DIR / "fixtures" / "abcd_v1.1.json"

SYSTEM_PROMPT = (
    "You are a customer service agent for an online retail company. "
    "Help customers with order issues, returns, refunds, account changes, shipping, "
    "and product inquiries. You have access to the customer's account and order history. "
    "Before making any changes, verify the customer's identity (name, email, or account ID). "
    "Follow company policies: refunds within 90 days, exchanges within 30 days. "
    "For membership upgrades, verify eligibility based on current level and purchase history. "
    "Always be professional and empathetic. Escalate to a manager when policies cannot be bent."
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

# ─── Dataset ─────────────────────────────────────────────────────────────────


def download_dataset():
    """Download ABCD v1.1 dataset if not present."""
    if DATASET_FILE.exists():
        return

    print(f"{B}Downloading ABCD v1.1 dataset...{N}")
    DATASET_FILE.parent.mkdir(parents=True, exist_ok=True)

    url = "https://github.com/asappresearch/abcd/raw/master/data/abcd_v1.1.json.gz"
    gz_file = DATASET_FILE.with_suffix(".json.gz")

    urllib.request.urlretrieve(url, gz_file)

    # Decompress
    with gzip.open(gz_file, "rb") as f_in:
        data = json.loads(f_in.read())

    # Save test split
    test_dialogues = data.get("test", [])
    with open(DATASET_FILE, "w") as f:
        json.dump(test_dialogues, f)

    gz_file.unlink(missing_ok=True)
    print(f"  {G}✓{N} {len(test_dialogues)} test dialogues saved")


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


def get_ground_truth_intent(scenario):
    """Get the ground-truth intent from ABCD scenario (flow/subflow)."""
    flow = scenario.get("flow", "")
    subflow = scenario.get("subflow", "")
    return f"{flow}/{subflow}" if flow else subflow


def get_scenario_slots(scenario):
    """Extract all ground-truth constraint values from ABCD scenario.

    These are the values a customer would provide during the conversation
    (name, email, phone, order_id, etc). We track which ones appear in
    the extracted state to measure constraint preservation.
    """
    slots = {}
    personal = scenario.get("personal", {})
    for key in ("customer_name", "email", "phone", "username", "member_level"):
        if personal.get(key):
            slots[key] = str(personal[key]).lower().strip()

    order = scenario.get("order", {})
    for key in ("order_id", "street_address", "city", "zip_code", "payment_method"):
        if order.get(key):
            slots[key] = str(order[key]).lower().strip()

    product = scenario.get("product", {})
    if product.get("names"):
        slots["product"] = " ".join(product["names"]).lower().strip()
    if product.get("amounts"):
        slots["amount"] = str(product["amounts"][0])

    return slots


def get_cumulative_slots(delexed_turns, up_to_turn):
    """Get cumulative slot values from action turns up to a given turn number.

    ABCD action turns have targets[3] = list of slot values filled at that step.
    This accumulates all values provided so far.
    """
    values = set()
    for turn in delexed_turns:
        if turn["turn_count"] > up_to_turn:
            break
        targets = turn.get("targets", [])
        if len(targets) >= 4 and targets[3]:
            for v in targets[3]:
                if v:
                    values.add(str(v).lower().strip())
    return values


# ─── API Client ──────────────────────────────────────────────────────────────


def send_chat(base_url, api_key, model, messages, max_tokens=30, retries=3):
    """Send chat completion request with retry on 429. Returns (response, headers, latency_ms)."""
    payload = json.dumps({
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
    }).encode()

    hdrs = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
        "x-ubiquum-cache": "false",
    }

    for attempt in range(retries):
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
        except urllib.error.HTTPError as e:
            latency_ms = int((time.time() - start) * 1000)
            if e.code == 429 and attempt < retries - 1:
                wait = 2 ** attempt + 1  # 2s, 3s, 5s
                print(f"    {Y}429 rate limited, waiting {wait}s...{N}")
                time.sleep(wait)
                continue
            return {"error": f"HTTP {e.code}: {e.reason}"}, {}, latency_ms
        except Exception as e:
            latency_ms = int((time.time() - start) * 1000)
            return {"error": str(e)}, {}, latency_ms


def parse_hivestate_headers(headers):
    """Extract HiveState headers from response."""
    h = {k.lower(): v for k, v in headers.items()}
    constraints_raw = h.get("x-hivestate-constraints", "").strip()
    constraints = {}
    if constraints_raw:
        try:
            constraints = json.loads(constraints_raw)
            if not isinstance(constraints, dict):
                constraints = {}
        except (json.JSONDecodeError, TypeError):
            constraints = {}
    return {
        "mode": h.get("x-hivestate-mode", "").strip(),
        "original_tokens": int(h.get("x-hivestate-original-tokens", "0").strip() or 0),
        "result_tokens": int(h.get("x-hivestate-tokens", "0").strip() or 0),
        "ratio": h.get("x-hivestate-ratio", "").strip(),
        "latency_ms": int(h.get("x-hivestate-latency-ms", "0").strip() or 0),
        "extraction_prompt_tokens": int(h.get("x-hivestate-extraction-prompt-tokens", "0").strip() or 0),
        "extraction_completion_tokens": int(h.get("x-hivestate-extraction-completion-tokens", "0").strip() or 0),
        "intent": h.get("x-hivestate-intent", "").strip(),
        "constraints": constraints,
        "fallback": h.get("x-hivestate-fallback", "").strip(),
    }


def compute_slot_metrics(extracted_constraints, ground_truth_values):
    """Compare extracted constraints against ground truth slot values.

    Uses value-based matching — checks if GT values appear in extracted state.
    Returns (recall, precision, f1, matched_slots, total_gt, total_extracted).
    """
    if not ground_truth_values:
        return 1.0, 1.0, 1.0, 0, 0, len(extracted_constraints) if extracted_constraints else 0

    if not extracted_constraints:
        return 0.0, 0.0, 0.0, 0, len(ground_truth_values), 0

    def normalize(s):
        """Normalize for comparison: lowercase, strip punctuation/extra spaces."""
        s = str(s).lower().strip()
        # Normalize commas, multiple spaces, common formatting differences
        s = s.replace(",", " ").replace("  ", " ").strip()
        return s

    # Normalize GT values
    if isinstance(ground_truth_values, dict):
        gt_vals = set(normalize(v) for v in ground_truth_values.values())
    elif isinstance(ground_truth_values, set):
        gt_vals = set(normalize(v) for v in ground_truth_values)
    else:
        gt_vals = set(normalize(v) for v in ground_truth_values)

    # Normalize extracted values
    ext_vals = set()
    if isinstance(extracted_constraints, dict):
        for val in extracted_constraints.values():
            ext_vals.add(normalize(val))
    elif isinstance(extracted_constraints, (list, set)):
        for val in extracted_constraints:
            ext_vals.add(normalize(val))

    # Value-based matching (lenient — substring check)
    matched = 0
    for gt_val in gt_vals:
        if not gt_val:
            continue
        for ext_val in ext_vals:
            if gt_val in ext_val or ext_val in gt_val:
                matched += 1
                break

    total_gt = len([v for v in gt_vals if v])
    recall = matched / total_gt if total_gt > 0 else 1.0
    precision = matched / len(ext_vals) if ext_vals else 0.0
    f1 = 2 * recall * precision / (recall + precision) if (recall + precision) > 0 else 0.0

    return recall, precision, f1, matched, total_gt, len(ext_vals)


# ─── Main ────────────────────────────────────────────────────────────────────


def main():
    parser = argparse.ArgumentParser(description="HiveState Benchmark — ABCD")
    parser.add_argument("--dialogues", "-n", type=int, default=50,
                        help="Number of dialogues (default: 50)")
    parser.add_argument("--min-turns", type=int, default=10,
                        help="Min turns per dialogue to include (default: 10)")
    parser.add_argument("--url", type=str, default="http://localhost:4000",
                        help="Gateway URL (default: http://localhost:4000)")
    parser.add_argument("--model", type=str, default=os.environ.get("BENCH_MODEL", "gemma4"),
                        help="Model to use for main completions")
    parser.add_argument("--mock", action="store_true",
                        help="Use echo provider (tests flow only, no real extraction)")
    parser.add_argument("--max-tokens", type=int, default=30)
    parser.add_argument("--delay", type=float, default=None,
                        help="Delay between requests")
    parser.add_argument("--main-input-cost", type=float, default=None,
                        help="Main model input cost per 1M tokens (for net savings; optional)")
    parser.add_argument("--extraction-input-cost", type=float, default=None,
                        help="Override extraction model input cost/M (auto-detected from config)")
    parser.add_argument("--extraction-output-cost", type=float, default=None,
                        help="Override extraction model output cost/M (auto-detected from config)")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--output", type=str, default="/tmp/hivestate_bench",
                        help="Output prefix for results")
    parser.add_argument("--verbose", "-v", action="store_true")
    args = parser.parse_args()

    if args.delay is None:
        args.delay = 0.05 if args.mock else 0.5
    if args.mock:
        args.model = "echo-bench"

    RESULTS_FILE = Path(f"{args.output}_results.jsonl")
    SUMMARY_FILE = Path(f"{args.output}_summary.json")

    # Parse gateway config
    config_path = Path.home() / ".ubiquum" / "gateway.yaml"
    api_key = ""
    extraction_model_name = ""
    extraction_input_cost = args.extraction_input_cost
    extraction_output_cost = args.extraction_output_cost
    main_input_cost = args.main_input_cost
    gateway_cfg = {}

    if config_path.exists():
        try:
            import yaml
            gateway_cfg = yaml.safe_load(config_path.read_text()) or {}
        except ImportError:
            # Fallback: line-by-line for essentials
            for line in config_path.read_text().splitlines():
                if "master_key" in line:
                    api_key = line.split(":", 1)[1].strip().strip('"')

    if gateway_cfg:
        api_key = gateway_cfg.get("server", {}).get("master_key", "")
        extraction_model_name = gateway_cfg.get("hivestate", {}).get("model", "")
        extraction_is_local = False

        # Auto-detect extraction model pricing and provider from config
        if extraction_model_name:
            for m in gateway_cfg.get("models", []):
                if m.get("name") == extraction_model_name:
                    provider_name = m.get("provider", "")
                    # Check if provider is local (ollama, local, etc.)
                    provider_cfg = gateway_cfg.get("providers", {}).get(provider_name, {})
                    api_base = provider_cfg.get("api_base", "")
                    if "localhost" in api_base or "127.0.0.1" in api_base or provider_cfg.get("type") == "ollama":
                        extraction_is_local = True
                    if extraction_input_cost is None:
                        if m.get("input_cost_per_million"):
                            extraction_input_cost = m["input_cost_per_million"]
                        if m.get("output_cost_per_million"):
                            extraction_output_cost = m["output_cost_per_million"]
                    break

        # Auto-detect main model pricing from config
        if main_input_cost is None and args.model:
            for m in gateway_cfg.get("models", []):
                if m.get("name") == args.model:
                    if m.get("input_cost_per_million"):
                        main_input_cost = m["input_cost_per_million"]
                    break

    if not api_key:
        print(f"{R}Error: no master_key in ~/.ubiquum/gateway.yaml{N}")
        sys.exit(1)

    # Download dataset
    download_dataset()

    # Load dialogues (filter by min turns)
    all_dialogues = load_dialogues(args.dialogues * 3, args.seed)
    dialogues = []
    for d in all_dialogues:
        # ABCD uses "original" field: list of [speaker, text] tuples
        n_turns = len(d.get("original", []))
        if n_turns >= args.min_turns:
            dialogues.append(d)
        if len(dialogues) >= args.dialogues:
            break

    total_customer_turns = sum(
        sum(1 for sp, _ in d["original"] if sp == "customer")
        for d in dialogues
    )

    # Header
    print()
    print(f"{B}╔══════════════════════════════════════════════════════════╗{N}")
    print(f"{B}║   HiveState Benchmark — ABCD Action State Tracking     ║{N}")
    print(f"{B}╚══════════════════════════════════════════════════════════╝{N}")
    print()
    print(f"  {D}gateway:{N}    {args.url}")
    print(f"  {D}model:{N}      {args.model}")
    mode_str = f"{CYAN}mock (echo provider){N}" if args.mock else "real LLM"
    print(f"  {D}mode:{N}       {mode_str}")
    print(f"  {D}dialogues:{N}  {len(dialogues)} (min {args.min_turns} turns)")
    print(f"  {D}turns:{N}      {total_customer_turns} customer turns")
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

    print(f"  {G}✓{N} gateway healthy")
    print()

    # ─── Run benchmark ─────────────────────────────────────────────────────────
    RESULTS_FILE.unlink(missing_ok=True)

    # Counters
    total_requests = 0
    state_extractions = 0
    noop_count = 0
    fallback_count = 0
    errors = 0

    intent_pairs = []  # (gt_intent, extracted_intent) for LLM judge — first per dialogue only
    intent_seen_dialogues = set()  # track which dialogues already contributed an intent

    slot_recalls = []
    slot_precisions = []
    slot_f1s = []

    token_original_total = 0
    token_result_total = 0
    extraction_prompt_tokens_total = 0
    extraction_completion_tokens_total = 0
    extraction_latencies = []

    domain_stats = defaultdict(lambda: {"correct": 0, "total": 0, "recall_sum": 0.0})

    query_num = 0

    for dlg_idx, dialogue in enumerate(dialogues):
        original = dialogue["original"]  # list of [speaker, text]
        delexed = dialogue.get("delexed", [])
        scenario = dialogue.get("scenario", {})
        gt_intent = get_ground_truth_intent(scenario)
        scenario_slots = get_scenario_slots(scenario)

        messages = [{"role": "system", "content": SYSTEM_PROMPT}]
        customer_turn_num = 0
        turn_count = 0  # tracks position in delexed for cumulative slots

        for speaker, text in original:
            turn_count += 1

            if speaker == "customer":
                customer_turn_num += 1
                query_num += 1
                messages.append({"role": "user", "content": text})

                # Build GT: only scenario values actually mentioned in conversation so far
                conversation_text = " ".join(
                    m["content"].lower() for m in messages if m["role"] in ("user", "assistant")
                )
                mentioned_slots = {
                    k: v for k, v in scenario_slots.items()
                    if v and v in conversation_text
                }

                # Send to gateway
                resp_data, headers, latency = send_chat(
                    args.url, api_key, args.model, messages, args.max_tokens
                )

                if "error" in resp_data:
                    errors += 1
                    time.sleep(args.delay)
                    continue

                total_requests += 1

                # Parse HiveState headers
                hs = parse_hivestate_headers(headers)

                if hs["mode"] == "state":
                    state_extractions += 1
                    extraction_latencies.append(hs["latency_ms"])

                    # Token reduction
                    token_original_total += hs["original_tokens"]
                    token_result_total += hs["result_tokens"]

                    # Extraction cost tracking
                    extraction_prompt_tokens_total += hs["extraction_prompt_tokens"]
                    extraction_completion_tokens_total += hs["extraction_completion_tokens"]

                    # Constraint preservation: check extracted constraints vs mentioned GT
                    if mentioned_slots and hs["constraints"]:
                        recall, precision, f1, matched, total_gt, total_ext = compute_slot_metrics(
                            hs["constraints"], mentioned_slots
                        )
                        slot_recalls.append(recall)
                        slot_precisions.append(precision)
                        slot_f1s.append(f1)

                    # Collect intent pair for LLM judge — only FIRST STATE turn per dialogue
                    # (first extraction best captures the customer's initial stated need)
                    if gt_intent and hs["intent"] and dlg_idx not in intent_seen_dialogues:
                        intent_seen_dialogues.add(dlg_idx)
                        intent_pairs.append((gt_intent, hs["intent"]))
                        flow = scenario.get("flow", "other")
                        domain_stats[flow]["total"] += 1

                    # Print progress
                    reduction = f"{float(hs['ratio'])*100:.0f}%" if hs["ratio"] else "0%"
                    print(
                        f"  [{query_num:>4}] {G}STATE{N} "
                        f"{hs['latency_ms']:>4}ms "
                        f"↓{reduction:>4} "
                        f"│ {D}D{dlg_idx+1}T{customer_turn_num}{N} "
                        f"[{scenario.get('subflow', '')}] "
                        f"{text[:45]}"
                    )

                elif hs["mode"] == "none":
                    noop_count += 1
                    if hs["fallback"]:
                        fallback_count += 1
                    print(
                        f"  [{query_num:>4}] {D}NOOP{N}  "
                        f"│ {D}D{dlg_idx+1}T{customer_turn_num}{N} "
                        f"{hs.get('fallback', '')}"
                    )

                # Record
                record = {
                    "query_num": query_num,
                    "dialogue_idx": dlg_idx,
                    "convo_id": dialogue.get("convo_id"),
                    "turn": customer_turn_num,
                    "gt_intent": gt_intent,
                    "gt_slots": mentioned_slots,
                    "extracted_constraints": hs["constraints"],
                    "hs_mode": hs["mode"],
                    "hs_intent": hs["intent"],
                    "hs_original_tokens": hs["original_tokens"],
                    "hs_result_tokens": hs["result_tokens"],
                    "hs_latency_ms": hs["latency_ms"],
                    "hs_fallback": hs["fallback"],
                    "latency_ms": latency,
                }
                with open(RESULTS_FILE, "a") as f:
                    f.write(json.dumps(record) + "\n")

                time.sleep(args.delay)

            elif speaker == "agent":
                # Use ABCD ground-truth agent response as assistant message
                messages.append({"role": "assistant", "content": text})

            # Skip "action" turns (system actions, not part of visible conversation)

    # ─── Summary ───────────────────────────────────────────────────────────────
    print()
    print(f"{B}╔══════════════════════════════════════════════════════════╗{N}")
    print(f"{B}║              HIVESTATE BENCHMARK RESULTS                 ║{N}")
    print(f"{B}╚══════════════════════════════════════════════════════════╝{N}")
    print()

    # Mode distribution
    print(f"  {B}Requests:{N}")
    print(f"    Total:        {total_requests}")
    print(f"    STATE mode:   {G}{state_extractions}{N}")
    print(f"    NO_OP mode:   {D}{noop_count}{N}")
    if fallback_count:
        print(f"    Fallbacks:    {Y}{fallback_count}{N}")
    if errors:
        print(f"    Errors:       {R}{errors}{N}")
    print()

    # Token reduction
    if token_original_total > 0:
        overall_reduction = 1.0 - (token_result_total / token_original_total)
        print(f"  {B}Token Reduction:{N}")
        print(f"    Original:     {token_original_total:,} tokens")
        print(f"    After state:  {token_result_total:,} tokens")
        print(f"    Reduction:    {G}{overall_reduction*100:.1f}%{N}")
        print(f"    Tokens saved: {CYAN}{token_original_total - token_result_total:,}{N}")
        print()

    # Constraint Preservation (slot recall) — PRIMARY QUALITY METRIC
    if slot_recalls:
        avg_recall = sum(slot_recalls) / len(slot_recalls)
        avg_precision = sum(slot_precisions) / len(slot_precisions)
        avg_f1 = sum(slot_f1s) / len(slot_f1s)
        color = G if avg_recall >= 0.8 else Y if avg_recall >= 0.6 else R
        print(f"  {B}★ Constraint Preservation (primary quality metric):{N}")
        print(f"    Slot Recall:    {color}{avg_recall*100:.1f}%{N}  (ground-truth values found in extracted state)")
        print(f"    Slot Precision: {avg_precision*100:.1f}%  (extracted values that match ground-truth)")
        print(f"    F1 Score:       {avg_f1*100:.1f}%")
        print(f"    Evaluated:      {len(slot_recalls)} turns with ground-truth slots")
        print()

    # Intent accuracy via LLM judge
    intent_correct = 0
    intent_total = len(intent_pairs)
    if intent_pairs and not args.mock:
        print(f"  {B}Intent Evaluation (LLM judge: gpt-oss){N}")
        print(f"    Judging {len(intent_pairs)} intent pairs...")

        judge_model = "gpt-oss"
        batch_size = 5
        for i in range(0, len(intent_pairs), batch_size):
            batch = intent_pairs[i:i+batch_size]
            # Build a single prompt with all pairs in this batch
            lines = []
            for idx, (gt, ext) in enumerate(batch):
                lines.append(f"{idx+1}. Ground truth: \"{gt}\" | Extracted: \"{ext}\"")

            prompt = (
                "You are evaluating intent extraction quality. The ground truth uses ABCD dataset format "
                "'flow/subflow' which describes the SCENARIO CATEGORY (e.g. purchase_dispute/bad_price_yesterday "
                "means the customer has a price dispute). The extracted intent is a concise action label.\n\n"
                "Mark Y if the extracted intent relates to the same customer problem:\n"
                "- return_item ↔ product_defect/return_size → Y (both about returning)\n"
                "- request_price_match ↔ purchase_dispute/bad_price_yesterday → Y (both about price issue)\n"
                "- cancel_subscription ↔ manage_account/status_service_added → Y (managing account service)\n"
                "- report_issue ↔ purchase_dispute/out_of_stock → Y (reporting purchase problem)\n\n"
                "Mark N ONLY if completely different domain (e.g. shipping vs account, return vs billing).\n\n"
                + "\n".join(lines) + "\n\n"
                "Reply with ONLY a comma-separated list of Y or N. Example: Y,Y,N"
            )

            payload = json.dumps({
                "model": judge_model,
                "messages": [{"role": "user", "content": prompt}],
                "max_tokens": 50,
            }).encode()

            req_obj = urllib.request.Request(
                f"{args.url}/v1/chat/completions",
                data=payload,
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "Content-Type": "application/json",
                    "x-ubiquum-cache": "false",
                    "x-ubiquum-state": "false",
                },
            )

            try:
                resp = urllib.request.urlopen(req_obj, timeout=60)
                body = json.loads(resp.read().decode())
                answer = body["choices"][0]["message"]["content"].strip()
                # Parse Y/N answers
                verdicts = [v.strip().upper() for v in answer.replace(" ", "").split(",")]
                for j, (gt, ext) in enumerate(batch):
                    if j < len(verdicts) and verdicts[j] == "Y":
                        intent_correct += 1
                        domain = gt.split("/")[0] if "/" in gt else "other"
                        domain_stats[domain]["correct"] += 1
            except Exception as e:
                print(f"    {Y}judge error: {e}{N}")

            time.sleep(0.3)

        intent_acc = 100 * intent_correct / intent_total if intent_total > 0 else 0
        color = G if intent_acc >= 80 else Y if intent_acc >= 60 else R
        print(f"    Correct:      {intent_correct}/{intent_total} ({color}{intent_acc:.1f}%{N})")
        print()

        # Per-domain
        if domain_stats:
            print(f"  {B}Per-Domain Intent Accuracy:{N}")
            for domain, stats in sorted(domain_stats.items(), key=lambda x: -x[1]["total"]):
                if stats["total"] > 0:
                    acc = 100 * stats["correct"] / stats["total"]
                    bar = "█" * int(acc / 5) + "░" * (20 - int(acc / 5))
                    print(f"    {domain:<12} {bar} {acc:.0f}% ({stats['correct']}/{stats['total']})")
            print()
    elif intent_pairs and args.mock:
        print(f"  {D}Intent evaluation skipped (mock mode){N}")
        print()

    # Latency
    if extraction_latencies:
        extraction_latencies.sort()
        p50 = extraction_latencies[len(extraction_latencies) // 2]
        p95_idx = int(len(extraction_latencies) * 0.95)
        p95 = extraction_latencies[min(p95_idx, len(extraction_latencies) - 1)]
        avg = sum(extraction_latencies) / len(extraction_latencies)
        print(f"  {B}Extraction Latency:{N}")
        print(f"    p50:          {p50}ms")
        print(f"    p95:          {p95}ms")
        print(f"    avg:          {avg:.0f}ms")
        print()

    # Cost analysis
    if token_original_total > 0 and extraction_prompt_tokens_total > 0:
        tokens_saved = token_original_total - token_result_total
        extraction_total = extraction_prompt_tokens_total + extraction_completion_tokens_total
        print(f"  {B}Cost of Compression:{N}")
        print(f"    Tokens saved (main model):   {CYAN}{tokens_saved:,}{N}")
        print(f"    Extraction tokens used:      {extraction_total:,} ({extraction_prompt_tokens_total:,} in / {extraction_completion_tokens_total:,} out)")

        if extraction_is_local:
            print(f"    Extraction cost:             {G}FREE{N} (local model: {extraction_model_name})")
            print(f"    Net savings:                 {G}{tokens_saved:,} tokens{N} on main model")
            if main_input_cost:
                saved_cost = tokens_saved * (main_input_cost / 1_000_000)
                print(f"    Dollar savings:              {G}${saved_cost:.6f}{N} (main @ ${main_input_cost}/M)")
        else:
            # Remote extraction — show net balance and overhead
            net_tokens = tokens_saved - extraction_total
            color = G if net_tokens > 0 else R
            print(f"    Net token balance:           {color}{net_tokens:+,}{N}")
            if tokens_saved > 0:
                overhead_pct = extraction_total / tokens_saved * 100
                print(f"    Compression overhead:        {overhead_pct:.1f}% of saved tokens")

            # Dollar cost analysis
            ext_in = extraction_input_cost
            ext_out = extraction_output_cost

            if ext_in is not None:
                extraction_cost_dollars = (
                    extraction_prompt_tokens_total * (ext_in / 1_000_000)
                    + extraction_completion_tokens_total * ((ext_out or ext_in * 4) / 1_000_000)
                )
                print()
                ext_model_label = extraction_model_name or "extraction model"
                print(f"    {D}── Dollar Analysis ({ext_model_label}: ${ext_in}/M in, ${ext_out or '?'}/M out) ──{N}")
                print(f"    Extraction cost:             ${extraction_cost_dollars:.6f}")

                if main_input_cost is not None:
                    saved_cost = tokens_saved * (main_input_cost / 1_000_000)
                    net_cost = saved_cost - extraction_cost_dollars
                    color = G if net_cost > 0 else R
                    print(f"    Saved (main @ ${main_input_cost}/M):     ${saved_cost:.6f}")
                    print(f"    Net savings:                 {color}${net_cost:+.6f}{N}")
                    if extraction_cost_dollars > 0:
                        roi = net_cost / extraction_cost_dollars * 100
                        print(f"    ROI:                         {color}{roi:+.0f}%{N}")
                else:
                    if tokens_saved > 0:
                        breakeven = extraction_cost_dollars / tokens_saved * 1_000_000
                        print(f"    Break-even main input cost:  ${breakeven:.4f}/M")
                        print(f"    {D}(extraction is profitable if main model costs ≥ ${breakeven:.4f}/M input){N}")
        print()

    # Save summary
    summary = {
        "total_requests": total_requests,
        "state_extractions": state_extractions,
        "noop_count": noop_count,
        "fallback_count": fallback_count,
        "errors": errors,
        "token_original_total": token_original_total,
        "token_result_total": token_result_total,
        "token_reduction_pct": round((1.0 - token_result_total / token_original_total) * 100, 1) if token_original_total > 0 else 0,
        "extraction_prompt_tokens": extraction_prompt_tokens_total,
        "extraction_completion_tokens": extraction_completion_tokens_total,
        "extraction_total_tokens": extraction_prompt_tokens_total + extraction_completion_tokens_total,
        "intent_accuracy_pct": round(100 * intent_correct / intent_total, 1) if intent_total > 0 else None,
        "intent_correct": intent_correct,
        "intent_total": intent_total,
        "latency_p50_ms": extraction_latencies[len(extraction_latencies) // 2] if extraction_latencies else None,
        "latency_p95_ms": extraction_latencies[int(len(extraction_latencies) * 0.95)] if extraction_latencies else None,
        "domain_stats": {k: v for k, v in domain_stats.items()},
    }

    with open(SUMMARY_FILE, "w") as f:
        json.dump(summary, f, indent=2)
    print(f"  {D}Results: {RESULTS_FILE}{N}")
    print(f"  {D}Summary: {SUMMARY_FILE}{N}")
    print()

    # Pass/fail
    if state_extractions > 0 and token_original_total > 0:
        reduction = 1.0 - (token_result_total / token_original_total)
        if reduction >= 0.5:
            print(f"  {G}✓ PASS{N} — {reduction*100:.0f}% token reduction (target: ≥50%)")
        elif reduction >= 0.3:
            print(f"  {Y}△ MARGINAL{N} — {reduction*100:.0f}% token reduction (target: ≥50%)")
        else:
            print(f"  {R}✗ FAIL{N} — {reduction*100:.0f}% token reduction (target: ≥50%)")

        if slot_recalls:
            avg_recall = sum(slot_recalls) / len(slot_recalls)
            if avg_recall >= 0.8:
                print(f"  {G}✓ PASS{N} — {avg_recall*100:.0f}% constraint preservation (target: ≥80%)")
            elif avg_recall >= 0.6:
                print(f"  {Y}△ MARGINAL{N} — {avg_recall*100:.0f}% constraint preservation (target: ≥80%)")
            else:
                print(f"  {R}✗ FAIL{N} — {avg_recall*100:.0f}% constraint preservation (target: ≥80%)")

        if intent_total > 0:
            intent_acc = 100 * intent_correct / intent_total
            if intent_acc >= 70:
                print(f"  {G}✓ PASS{N} — {intent_acc:.0f}% intent accuracy (target: ≥70%)")
            elif intent_acc >= 50:
                print(f"  {Y}△ MARGINAL{N} — {intent_acc:.0f}% intent accuracy (target: ≥70%)")
            else:
                print(f"  {R}✗ FAIL{N} — {intent_acc:.0f}% intent accuracy (target: ≥70%)")
    else:
        print(f"  {Y}△{N} No state extractions occurred (threshold too high or model not configured)")

    print()


if __name__ == "__main__":
    main()
