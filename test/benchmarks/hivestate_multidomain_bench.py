#!/usr/bin/env python3
"""
HiveState Multi-Domain Benchmark (Real Data Only)
==================================================
Evaluates adaptive state compression across 4 AI workflow categories
using ONLY real-world datasets — no synthetic generation.

Datasets:
  1. ABCD v1.1          — multi-turn customer service (Chen et al. 2021)
  2. Tau-Bench          — tool-augmented agents (Sierra Research 2024)
  3. SWE-smith          — coding agents (SWE-bench/SWE-smith-trajectories)
  4. AgentInstruct WebShop — web shopping agents (THUDM, ICLR 2024)

Usage:
    ./test/hivestate_multidomain_bench.py --domain all -n 100
    ./test/hivestate_multidomain_bench.py --domain swe-smith -n 100
    ./test/hivestate_multidomain_bench.py --domain tau-bench -n 100 --tau-variant retail
    ./test/hivestate_multidomain_bench.py --domain webshop -n 100
    ./test/hivestate_multidomain_bench.py --domain abcd -n 100
    ./test/hivestate_multidomain_bench.py --dry-run  # show stats without calling gateway
"""

import argparse
import json
import os
import random
import re
import statistics
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# ─── Config ──────────────────────────────────────────────────────────────────

SCRIPT_DIR = Path(__file__).parent
FIXTURES = SCRIPT_DIR.parent / "fixtures"

DEFAULT_URL = "http://localhost:4000/v1/chat/completions"
DEFAULT_MODEL = "echo-bench"
DEFAULT_DELAY = 0.5

# Token threshold — same as gateway default
THRESHOLD = 400


# ─── Message Preparation ─────────────────────────────────────────────────────

def prepare_messages_for_gateway(messages):
    """Flatten messages into standard role/content format for the gateway."""
    prepared = []
    for m in messages:
        role = m.get("role", "user")
        content = m.get("content", "") or ""

        # Handle content that's a list (e.g. [{type: text, text: ...}])
        if isinstance(content, list):
            parts = []
            for part in content:
                if isinstance(part, dict):
                    parts.append(part.get("text", "") or str(part))
                else:
                    parts.append(str(part))
            content = "\n".join(parts)

        # Flatten tool_calls into content
        if "tool_calls" in m and m["tool_calls"]:
            tc_summary = json.dumps([
                {"function": tc.get("function", {})}
                for tc in m["tool_calls"]
            ], ensure_ascii=False)
            content = (content + "\n[tool_calls: " + tc_summary + "]").strip()

        # Normalize roles
        if role == "tool":
            role = "user"
            tool_id = m.get("tool_call_id", "")
            content = f"[tool_response:{tool_id}] {content}" if tool_id else f"[tool_response] {content}"

        if role not in ("system", "user", "assistant"):
            role = "user"

        prepared.append({"role": role, "content": content})

    # Cap context — keep system + last messages that fit
    MAX_CHARS = 32000
    total_chars = sum(len(m["content"]) for m in prepared)
    if total_chars > MAX_CHARS:
        # Keep system message (if any) + trim from the front
        system_msgs = [m for m in prepared if m["role"] == "system"]
        non_system = [m for m in prepared if m["role"] != "system"]
        # Keep last N messages that fit
        kept = []
        chars_used = sum(len(m["content"]) for m in system_msgs)
        for m in reversed(non_system):
            if chars_used + len(m["content"]) > MAX_CHARS:
                break
            kept.insert(0, m)
            chars_used += len(m["content"])
        prepared = system_msgs + kept

    return prepared


def count_tokens_approx(messages):
    """Approximate token count (chars / 4)."""
    total = 0
    for m in messages:
        content = m.get("content", "") or ""
        if isinstance(content, list):
            total += sum(len(str(p)) for p in content) // 4
        else:
            total += len(str(content)) // 4
        if "tool_calls" in m:
            total += len(json.dumps(m["tool_calls"])) // 4
    return total


# ─── Domain Loaders ──────────────────────────────────────────────────────────

def load_abcd(n=100):
    """Load MultiWoZ 2.2 multi-turn task-oriented dialogues.
    Includes dialogue state (frames/slots) as system context — realistic
    for production chatbots that carry DB results and user state.
    Falls back to ABCD v1.1 if MultiWoZ not available.
    """
    filepath = FIXTURES / "multiwoz22_test.json"
    if not filepath.exists():
        filepath = FIXTURES / "abcd_v1.1.json"
        if not filepath.exists():
            print(f"ERROR: No customer service dataset found")
            sys.exit(1)
        with open(filepath) as f:
            data = json.load(f)
        trajectories = []
        for item in data:
            messages = []
            for speaker, text in item["original"]:
                role = "assistant" if speaker == "agent" else "user"
                messages.append({"role": role, "content": text})
            tokens = count_tokens_approx(messages)
            trajectories.append({
                "id": f"abcd-{item['convo_id']}",
                "messages": messages,
                "domain": "chat",
                "approx_tokens": tokens,
            })
        if len(trajectories) > n:
            trajectories = random.sample(trajectories, n)
        return trajectories

    with open(filepath) as f:
        data = json.load(f)

    # Convert MultiWoZ turns to messages WITH state context
    # In production, chatbots carry DB state, slot values, search results in context
    trajectories = []
    for item in data:
        messages = []
        # System prompt with services
        messages.append({
            "role": "system",
            "content": f"You are a task-oriented dialogue assistant helping with: {', '.join(item['services'])}. "
                       f"Track user preferences and provide accurate information."
        })

        for turn in item["turns"]:
            role = "assistant" if turn["speaker"] == "SYSTEM" else "user"
            content = turn["utterance"]

            # Include dialogue state as context (realistic — chatbots carry this)
            if turn.get("frames") and role == "user":
                for frame in turn["frames"]:
                    state = frame.get("state", {})
                    if state.get("slot_values"):
                        slots_str = json.dumps(state["slot_values"])
                        content += f"\n[dialogue_state: intent={state.get('active_intent','')}, slots={slots_str}]"
                    if frame.get("actions"):
                        actions_str = ", ".join(a.get("act", "") for a in frame["actions"])
                        content += f"\n[actions: {actions_str}]"

            messages.append({"role": role, "content": content})

        tokens = count_tokens_approx(messages)
        trajectories.append({
            "id": f"mwoz-{item['dialogue_id']}",
            "messages": messages,
            "domain": "chat",
            "approx_tokens": tokens,
        })

    # Random sample
    if len(trajectories) > n:
        trajectories = random.sample(trajectories, n)

    return trajectories


def load_tau_bench(n=100, variant="retail"):
    """Load Tau-Bench tool-augmented agent trajectories.
    Format: list of {traj: [messages], task_id, reward, info: {task: ...}}
    """
    filepath = FIXTURES / f"tau_bench_{variant}.json"
    if not filepath.exists():
        print(f"ERROR: {filepath} not found")
        sys.exit(1)

    with open(filepath) as f:
        data = json.load(f)

    # Use all trajectories with reward > 0 (successful or partially successful)
    eligible = [t for t in data if t.get("reward", 0) > 0]
    if not eligible:
        eligible = data  # fallback to all

    if len(eligible) > n:
        selected = random.sample(eligible, n)
    else:
        selected = eligible

    trajectories = []
    for t in selected:
        messages = t["traj"]
        tokens = count_tokens_approx(messages)
        trajectories.append({
            "id": f"tau-{variant}-{t.get('task_id', len(trajectories))}",
            "messages": messages,
            "domain": "tool-agent",
            "approx_tokens": tokens,
            "reward": t.get("reward", 0),
        })

    return trajectories


def load_swe_smith(n=100):
    """Load SWE-smith coding agent trajectories.
    Format: list of {instance_id, messages: [{role, content, ...}], resolved, model, ...}
    """
    filepath = FIXTURES / "swe_smith_trajectories.json"
    if not filepath.exists():
        print(f"ERROR: {filepath} not found")
        sys.exit(1)

    with open(filepath) as f:
        data = json.load(f)

    # Random sample from full dataset
    if len(data) > n:
        selected = random.sample(data, n)
    else:
        selected = data

    trajectories = []
    for item in selected:
        messages = item["messages"]
        if isinstance(messages, str):
            messages = json.loads(messages)

        tokens = item.get("approx_tokens", count_tokens_approx(messages))
        trajectories.append({
            "id": f"swe-{item['instance_id'][:50]}",
            "messages": messages,
            "domain": "coding-agent",
            "approx_tokens": tokens,
            "model": item.get("model", "unknown"),
        })

    return trajectories


def load_webshop(n=100):
    """Load AgentInstruct WebShop trajectories.
    Format: list of {id, conversations: [{from: human/gpt, value: text}], ...}
    """
    filepath = FIXTURES / "webshop_trajectories.json"
    if not filepath.exists():
        print(f"ERROR: {filepath} not found")
        sys.exit(1)

    with open(filepath) as f:
        data = json.load(f)

    # Filter to those above threshold
    eligible = [t for t in data if t.get("approx_tokens", 0) >= THRESHOLD]
    if len(eligible) > n:
        selected = random.sample(eligible, n)
    else:
        selected = eligible

    trajectories = []
    for item in selected:
        # Convert from/value to role/content
        messages = []
        for turn in item["conversations"]:
            role = "user" if turn["from"] == "human" else "assistant"
            messages.append({"role": role, "content": turn.get("value", "")})

        tokens = item.get("approx_tokens", count_tokens_approx(messages))
        trajectories.append({
            "id": f"webshop-{item['id']}",
            "messages": messages,
            "domain": "web-agent",
            "approx_tokens": tokens,
        })

    return trajectories


# ─── Gateway Interaction ─────────────────────────────────────────────────────

def call_gateway(url, api_key, model, messages, delay=0):
    """Send a request to the gateway and return response + HiveState headers."""
    if delay > 0:
        time.sleep(delay)

    prepared = prepare_messages_for_gateway(messages)
    payload = json.dumps({"model": model, "messages": prepared}).encode()
    req = urllib.request.Request(
        url,
        data=payload,
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
            "x-ubiquum-cache": "false",
        },
    )

    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            body = json.loads(resp.read())
            latency_ms = (time.time() - start) * 1000
            hs_headers = {
                "mode": resp.headers.get("x-hivestate-mode", ""),
                "original_tokens": int(resp.headers.get("x-hivestate-original-tokens", "0")),
                "tokens": int(resp.headers.get("x-hivestate-tokens", "0")),
                "ratio": float(resp.headers.get("x-hivestate-ratio", "0")),
                "latency_ms": int(resp.headers.get("x-hivestate-latency-ms", "0")),
                "intent": resp.headers.get("x-hivestate-intent", ""),
                "constraints": resp.headers.get("x-hivestate-constraints", "{}"),
                "fallback": resp.headers.get("x-hivestate-fallback", ""),
            }
            return body, latency_ms, hs_headers, None
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        return None, (time.time() - start) * 1000, None, str(e)


# ─── Quality Metrics ──────────────────────────────────────────────────────────

def extract_key_facts(messages):
    """Extract key facts from the LAST FEW messages (what's needed to continue).
    Only looks at the last user message + last assistant message — these contain
    the active constraints needed for correct workflow continuation.
    Returns a set of normalized fact strings."""
    facts = set()

    # Only extract from last 4 messages (the active context)
    recent = messages[-4:] if len(messages) > 4 else messages

    for m in recent:
        content = m.get("content", "") or ""
        if isinstance(content, list):
            content = " ".join(str(p) for p in content)
        content = str(content)

        # Extract numbers (prices, IDs, counts, dates)
        for num in re.findall(r'\b\d[\d.,]+\b', content):
            if len(num) >= 2:  # skip single digits
                facts.add(num.replace(",", ""))

        # Extract emails
        for email in re.findall(r'[\w.+-]+@[\w-]+\.[\w.-]+', content):
            facts.add(email.lower())

        # Extract order/booking/ticket IDs (alphanumeric patterns)
        for oid in re.findall(r'\b[A-Z]{1,3}[-#]?\d{4,}\b', content):
            facts.add(oid)

        # Extract quoted strings (names, titles, specific values)
        for quoted in re.findall(r"'([^']{2,40})'|\"([^\"]{2,40})\"", content):
            val = quoted[0] or quoted[1]
            facts.add(val.lower().strip())

        # Extract tool/function names
        for func in re.findall(r'\b(get_\w+|create_\w+|update_\w+|delete_\w+|search_\w+|book_\w+|cancel_\w+)\b', content):
            facts.add(func)

    return facts


def compute_state_recall(state_json_str, key_facts):
    """Compute what fraction of key facts appear in the state JSON.
    Returns recall score 0.0-1.0."""
    if not key_facts or not state_json_str:
        return 0.0

    state_lower = state_json_str.lower()
    found = 0
    for fact in key_facts:
        if fact.lower() in state_lower:
            found += 1

    return found / len(key_facts)


def call_llm_judge(url, api_key, judge_model, state_json_str, last_user_msg, expected_response, delay=0.5):
    """Use LLM judge (gpt-oss) to score workflow continuation.
    Given compressed state + last user message, scores if the state
    contains enough info to produce a correct continuation.
    Returns score 1-5 and explanation."""
    if delay > 0:
        time.sleep(delay)

    judge_prompt = f"""You are evaluating a lossy state compression system for multi-turn conversations.
The system extracts a compact JSON state from conversation history to reduce token usage by 50-80%.

Given the compressed state and last user message, evaluate: could an LLM use this state 
to produce a REASONABLE continuation? (Not identical to original — just functionally correct.)

Score 1-5:
- 5: State has all key info — correct continuation is straightforward
- 4: State has enough for a correct response, may need minor re-phrasing
- 3: State captures intent and main constraints but misses secondary details
- 2: State misses critical info — response would be wrong or confused
- 1: State is irrelevant or contradicts the actual conversation

Key principle: The state should preserve INTENT + KEY IDENTIFIERS + ACTIVE CONSTRAINTS.
Exact wording, historical context, and re-fetchable data (tool outputs, search results) are OK to lose.

COMPRESSED STATE:
{state_json_str}

LAST USER MESSAGE:
{last_user_msg}

EXPECTED NEXT ACTION:
{expected_response[:300]}

Respond with ONLY a JSON object: {{"score": <1-5>, "reason": "<one sentence>"}}"""

    payload = json.dumps({
        "model": judge_model,
        "messages": [{"role": "user", "content": judge_prompt}],
        "temperature": 0.1,
        "max_tokens": 2048,
    }).encode()

    req = urllib.request.Request(
        url,
        data=payload,
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
            "x-ubiquum-cache": "false",
        },
    )

    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            body = json.loads(resp.read())
            content = body.get("choices", [{}])[0].get("message", {}).get("content", "")
            # Parse score from response
            try:
                # Strip markdown fences if present
                cleaned = content.strip()
                if cleaned.startswith("```"):
                    cleaned = re.sub(r'^```(?:json)?\s*', '', cleaned)
                    cleaned = re.sub(r'\s*```$', '', cleaned)
                result = json.loads(cleaned)
                return result.get("score", 0), result.get("reason", "")
            except json.JSONDecodeError:
                # Try to extract score from text with various patterns
                match = re.search(r'"score"\s*:\s*(\d)', content)
                if match:
                    score = int(match.group(1))
                    reason_match = re.search(r'"reason"\s*:\s*"([^"]*)"', content)
                    reason = reason_match.group(1) if reason_match else content[:80]
                    return score, reason
                # Try plain number
                match = re.search(r'\b([1-5])\s*/\s*5\b', content)
                if match:
                    return int(match.group(1)), content[:80]
                return 0, f"parse_error: {content[:200]}"
    except Exception as e:
        return 0, f"error: {str(e)[:80]}"


# ─── Evaluation ──────────────────────────────────────────────────────────────

def evaluate_trajectory(traj, url, api_key, model, delay):
    """Run a single trajectory through HiveState and measure results."""
    messages = traj["messages"]
    original_tokens = traj.get("approx_tokens", count_tokens_approx(messages))

    # Skip if below threshold
    if original_tokens < THRESHOLD:
        return {
            "id": traj["id"],
            "domain": traj["domain"],
            "mode": "NO_OP",
            "original_tokens": original_tokens,
            "compressed_tokens": original_tokens,
            "reduction": 0.0,
            "latency_ms": 0,
            "intent": "",
            "state_recall": 0.0,
            "state_json": "",
            "error": None,
        }

    body, latency_ms, hs_headers, error = call_gateway(url, api_key, model, messages, delay)

    if error:
        return {
            "id": traj["id"],
            "domain": traj["domain"],
            "mode": "ERROR",
            "original_tokens": original_tokens,
            "compressed_tokens": original_tokens,
            "reduction": 0.0,
            "latency_ms": latency_ms,
            "intent": "",
            "state_recall": 0.0,
            "state_json": "",
            "error": error,
        }

    if hs_headers and hs_headers["mode"] == "state":
        mode = "STATE"
        compressed_tokens = hs_headers["tokens"]
        reduction = hs_headers["ratio"]
        extraction_latency = hs_headers["latency_ms"]
        intent = hs_headers["intent"]

        # Reconstruct state JSON from headers for quality metrics
        constraints = hs_headers.get("constraints", "{}")
        state_json = json.dumps({"intent": intent, "active_constraints": constraints})

        # Compute State Recall (objective)
        key_facts = extract_key_facts(messages)
        state_recall = compute_state_recall(state_json, key_facts)
    else:
        mode = "PASS"
        compressed_tokens = original_tokens
        reduction = 0.0
        extraction_latency = 0
        intent = ""
        state_json = ""
        state_recall = 0.0

    return {
        "id": traj["id"],
        "domain": traj["domain"],
        "mode": mode,
        "original_tokens": hs_headers["original_tokens"] if hs_headers else original_tokens,
        "compressed_tokens": compressed_tokens,
        "reduction": reduction,
        "latency_ms": extraction_latency,
        "intent": intent,
        "state_recall": state_recall,
        "state_json": state_json,
        "fallback": hs_headers.get("fallback", "") if hs_headers else "",
        "error": None,
    }


def run_domain(domain_name, trajectories, url, api_key, model, delay):
    """Run benchmark for a single domain."""
    print(f"\n{'='*60}")
    print(f"  Domain: {domain_name}")
    print(f"  Trajectories: {len(trajectories)}")
    print(f"{'='*60}\n")

    results = []
    errors = 0
    for i, traj in enumerate(trajectories):
        result = evaluate_trajectory(traj, url, api_key, model, delay)
        results.append(result)

        if result["error"]:
            errors += 1
            status = "✗"
        elif result["mode"] == "STATE":
            status = "✓"
        elif result["mode"] == "NO_OP":
            status = "○"
        else:
            status = "·"

        reduction_str = f"{result['reduction']*100:.0f}%" if result["reduction"] > 0 else "—"
        fallback_str = f" [{result.get('fallback','')}]" if result.get('fallback') else ""
        print(f"  {status} [{i+1}/{len(trajectories)}] {traj['id'][:40]}: "
              f"{result['original_tokens']}→{result['compressed_tokens']} tok "
              f"({reduction_str}) [{result['latency_ms']:.0f}ms]"
              f"{fallback_str}"
              f"{' ERR:'+result['error'][:30] if result['error'] else ''}")

        # Abort if too many consecutive errors
        if errors >= 5 and i < 10:
            print(f"\n  ABORT: Too many errors ({errors}/{i+1}). Is the gateway running?")
            break

    return results


def print_summary(domain_name, results):
    """Print summary statistics for a domain."""
    state_results = [r for r in results if r["mode"] == "STATE"]
    noop_results = [r for r in results if r["mode"] == "NO_OP"]
    pass_results = [r for r in results if r["mode"] == "PASS"]
    error_results = [r for r in results if r["mode"] == "ERROR"]

    total = len(results)
    print(f"\n{'─'*60}")
    print(f"  SUMMARY: {domain_name}")
    print(f"{'─'*60}")
    print(f"  Total trajectories:  {total}")
    print(f"  STATE (compressed):  {len(state_results)} ({len(state_results)/total*100:.1f}%)")
    print(f"  PASS (below τ/skip): {len(pass_results)} ({len(pass_results)/total*100:.1f}%)")
    print(f"  NO_OP (below τ):     {len(noop_results)} ({len(noop_results)/total*100:.1f}%)")
    print(f"  Errors:              {len(error_results)}")

    if state_results:
        reductions = [r["reduction"] for r in state_results]
        latencies = [r["latency_ms"] for r in state_results]
        orig_tokens = [r["original_tokens"] for r in state_results]
        comp_tokens = [r["compressed_tokens"] for r in state_results]

        total_orig = sum(orig_tokens)
        total_comp = sum(comp_tokens)

        print(f"\n  Token Reduction (STATE only):")
        print(f"    Mean reduction:    {statistics.mean(reductions)*100:.1f}%")
        print(f"    Median reduction:  {statistics.median(reductions)*100:.1f}%")
        if len(reductions) > 1:
            print(f"    Std deviation:     {statistics.stdev(reductions)*100:.1f}%")
        print(f"    Min/Max:           {min(reductions)*100:.1f}% / {max(reductions)*100:.1f}%")
        print(f"    Total original:    {total_orig:,} tokens")
        print(f"    Total compressed:  {total_comp:,} tokens")
        print(f"    Total saved:       {total_orig - total_comp:,} tokens")

        print(f"\n  Latency (STATE only):")
        print(f"    Mean:  {statistics.mean(latencies):.0f} ms")
        print(f"    p50:   {statistics.median(latencies):.0f} ms")
        if len(latencies) >= 20:
            sorted_lat = sorted(latencies)
            print(f"    p95:   {sorted_lat[int(len(sorted_lat)*0.95)]:.0f} ms")
            print(f"    p99:   {sorted_lat[int(len(sorted_lat)*0.99)]:.0f} ms")
        print(f"    Min:   {min(latencies):.0f} ms")
        print(f"    Max:   {max(latencies):.0f} ms")

        # State Recall (objective)
        recalls = [r["state_recall"] for r in state_results if r.get("state_recall", 0) > 0]
        if recalls:
            print(f"\n  State Recall (key fact preservation):")
            print(f"    Mean:   {statistics.mean(recalls)*100:.1f}%")
            print(f"    Median: {statistics.median(recalls)*100:.1f}%")
            print(f"    Min/Max: {min(recalls)*100:.1f}% / {max(recalls)*100:.1f}%")

    print()
    return {
        "domain": domain_name,
        "total": total,
        "state_count": len(state_results),
        "noop_count": len(noop_results),
        "pass_count": len(pass_results),
        "error_count": len(error_results),
        "activation_rate": len(state_results) / total if total > 0 else 0,
        "mean_reduction": statistics.mean([r["reduction"] for r in state_results]) if state_results else 0,
        "median_reduction": statistics.median([r["reduction"] for r in state_results]) if state_results else 0,
        "std_reduction": statistics.stdev([r["reduction"] for r in state_results]) if len(state_results) > 1 else 0,
        "mean_latency_ms": statistics.mean([r["latency_ms"] for r in state_results]) if state_results else 0,
        "p50_latency_ms": statistics.median([r["latency_ms"] for r in state_results]) if state_results else 0,
        "p95_latency_ms": sorted([r["latency_ms"] for r in state_results])[int(len(state_results)*0.95)] if len(state_results) >= 20 else 0,
        "mean_state_recall": statistics.mean([r["state_recall"] for r in state_results]) if state_results else 0,
        "total_original_tokens": sum(r["original_tokens"] for r in state_results),
        "total_compressed_tokens": sum(r["compressed_tokens"] for r in state_results),
        "total_saved_tokens": sum(r["original_tokens"] - r["compressed_tokens"] for r in state_results),
        "results": results,
    }


# ─── Main ────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="HiveState Multi-Domain Benchmark (Real Data)")
    parser.add_argument("--domain", default="all",
                        choices=["all", "abcd", "tau-bench", "swe-smith", "webshop"],
                        help="Domain to benchmark")
    parser.add_argument("-n", type=int, default=100, help="Number of trajectories per domain")
    parser.add_argument("--url", default=DEFAULT_URL, help="Gateway URL")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Model name")
    parser.add_argument("--delay", type=float, default=DEFAULT_DELAY, help="Delay between requests (s)")
    parser.add_argument("--api-key", default=None, help="API key (or UBIQUUM_API_KEY env)")
    parser.add_argument("--output", default=None, help="Save results to JSON file")
    parser.add_argument("--tau-variant", default="retail", choices=["retail", "airline", "both"],
                        help="Tau-Bench variant")
    parser.add_argument("--seed", type=int, default=42, help="Random seed for reproducibility")
    parser.add_argument("--dry-run", action="store_true", help="Show stats without calling gateway")
    parser.add_argument("--judge", default=None,
                        help="LLM judge model for Workflow Continuation Score (e.g. gpt-oss)")
    parser.add_argument("--judge-n", type=int, default=25,
                        help="Number of STATE results to judge per domain (default 25)")
    args = parser.parse_args()

    random.seed(args.seed)
    api_key = args.api_key or os.environ.get("UBIQUUM_API_KEY", "sk-ubq-test")

    print("=" * 60)
    print("  HiveState Multi-Domain Benchmark")
    print("  Real-World Data Only — No Synthetic Generation")
    print("=" * 60)
    print(f"  Gateway:  {args.url}")
    print(f"  Model:    {args.model}")
    print(f"  Delay:    {args.delay}s")
    print(f"  Seed:     {args.seed}")
    print(f"  Domain:   {args.domain}")
    print(f"  N:        {args.n}")

    # Load domains
    domains_to_run = []

    if args.domain in ("all", "abcd"):
        trajs = load_abcd(args.n)
        domains_to_run.append(("MultiWoZ (Customer Service)", trajs))

    if args.domain in ("all", "tau-bench"):
        if args.tau_variant == "both":
            trajs_r = load_tau_bench(args.n // 2, "retail")
            trajs_a = load_tau_bench(args.n // 2, "airline")
            trajs = trajs_r + trajs_a
        else:
            trajs = load_tau_bench(args.n, args.tau_variant)
        domains_to_run.append(("Tau-Bench (Tool Agents)", trajs))

    if args.domain in ("all", "swe-smith"):
        trajs = load_swe_smith(args.n)
        domains_to_run.append(("SWE-smith (Coding Agents)", trajs))

    if args.domain in ("all", "webshop"):
        trajs = load_webshop(args.n)
        domains_to_run.append(("WebShop (Web Agents)", trajs))

    # Dry run — show trajectory statistics
    if args.dry_run:
        print("\n\n─── DRY RUN: Dataset Statistics ───\n")
        for domain_name, trajs in domains_to_run:
            tokens = [t.get("approx_tokens", count_tokens_approx(t["messages"])) for t in trajs]
            msgs = [len(t["messages"]) for t in trajs]
            above_threshold = sum(1 for t in tokens if t >= THRESHOLD)
            print(f"  {domain_name}:")
            print(f"    Loaded:          {len(trajs)} trajectories")
            print(f"    Token range:     {min(tokens):,}–{max(tokens):,} (mean: {statistics.mean(tokens):,.0f}, median: {statistics.median(tokens):,.0f})")
            print(f"    Messages:        {min(msgs)}–{max(msgs)} (mean: {statistics.mean(msgs):.1f})")
            print(f"    Above τ={THRESHOLD}:   {above_threshold}/{len(trajs)} ({above_threshold/len(trajs)*100:.0f}%)")
            print()
        return

    # Run benchmarks
    all_summaries = {}
    for domain_name, trajs in domains_to_run:
        results = run_domain(domain_name, trajs, args.url, api_key, args.model, args.delay)
        summary = print_summary(domain_name, results)
        all_summaries[domain_name] = summary

    # Cross-domain summary
    if len(all_summaries) > 1:
        print("\n" + "=" * 60)
        print("  CROSS-DOMAIN SUMMARY")
        print("=" * 60)
        print(f"  {'Domain':<30} {'Activation':>10} {'Mean Red.':>10} {'Recall':>8} {'p50 Lat':>8} {'Saved':>10}")
        print(f"  {'─'*30} {'─'*10} {'─'*10} {'─'*8} {'─'*8} {'─'*10}")
        for domain_name, s in all_summaries.items():
            act = f"{s['state_count']}/{s['total']}"
            mean_r = f"{s['mean_reduction']*100:.1f}%"
            recall = f"{s['mean_state_recall']*100:.0f}%"
            lat = f"{s['p50_latency_ms']:.0f}ms"
            saved = f"{s['total_saved_tokens']:,}"
            print(f"  {domain_name:<30} {act:>10} {mean_r:>10} {recall:>8} {lat:>8} {saved:>10}")
        print()

    # Save results BEFORE judge phase so we don't lose data on crash
    if args.output:
        output_data = {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "config": {
                "url": args.url,
                "model": args.model,
                "threshold": THRESHOLD,
                "delay": args.delay,
                "seed": args.seed,
                "n": args.n,
            },
            "domains": {},
        }
        for domain_name, s in all_summaries.items():
            summary_copy = {k: v for k, v in s.items() if k != "results"}
            summary_copy["results"] = s["results"]
            output_data["domains"][domain_name] = summary_copy

        outpath = Path(args.output)
        outpath.parent.mkdir(parents=True, exist_ok=True)
        with open(outpath, "w") as f:
            json.dump(output_data, f, indent=2, default=str)
        print(f"  [checkpoint] Results saved to {outpath}")

    # ─── LLM Judge Pass (Workflow Continuation Score) ─────────────────────────
    if args.judge:
        print("\n" + "=" * 60)
        print(f"  LLM JUDGE: Workflow Continuation Score")
        print(f"  Judge model: {args.judge}")
        print(f"  Samples per domain: {args.judge_n}")
        print("=" * 60)

        for domain_name, s in all_summaries.items():
            state_results = [r for r in s["results"]
                            if r["mode"] == "STATE" and r.get("state_json")
                            and r.get("intent", "").upper() != "NONE"]
            judge_sample = state_results[:args.judge_n]
            if not judge_sample:
                continue

            # Find original trajectories for these results
            domain_trajs = {}
            for _, trajs_list in domains_to_run:
                for t in trajs_list:
                    domain_trajs[t["id"]] = t

            scores = []
            print(f"\n  {domain_name}: judging {len(judge_sample)} samples...")
            for i, result in enumerate(judge_sample):
                traj = domain_trajs.get(result["id"])
                if not traj:
                    continue

                messages = traj["messages"]
                # Find the last SUBSTANTIVE user message (not goodbye/thanks)
                # and its expected response — this is where the state matters most
                last_user = ""
                expected_response = ""
                # Collect all user→assistant pairs, pick the last substantive one
                pairs = []
                for idx, m in enumerate(messages):
                    if m.get("role") == "user":
                        user_content = m.get("content", "") or ""
                        if isinstance(user_content, list):
                            user_content = " ".join(str(p) for p in user_content)
                        exp = ""
                        if idx + 1 < len(messages) and messages[idx + 1].get("role") == "assistant":
                            next_msg = messages[idx + 1]
                            exp = next_msg.get("content", "") or ""
                            if not exp and next_msg.get("tool_calls"):
                                tc_summary = json.dumps([
                                    {"function": tc.get("function", {}).get("name", ""),
                                     "args": tc.get("function", {}).get("arguments", "")[:200]}
                                    for tc in next_msg["tool_calls"]
                                ], ensure_ascii=False)
                                exp = f"[Expected action: {tc_summary}]"
                        pairs.append((user_content, exp))

                # Pick last non-farewell pair (farewell = short msg that's just goodbye/thanks)
                for user_content, exp in reversed(pairs):
                    user_lower = user_content.lower().strip()
                    # Only skip if it's a SHORT pure farewell (< 60 chars)
                    is_pure_farewell = (len(user_lower) < 60 and
                        any(p in user_lower for p in {'goodbye', 'bye', '###stop###', 'that\'s all'})) or \
                        (len(user_lower) < 30 and any(p in user_lower for p in {'thank', 'thanks'}))
                    if not is_pure_farewell and len(user_lower) > 5:
                        last_user = user_content
                        expected_response = exp
                        break
                # Fallback to last pair if all are farewells
                if not last_user and pairs:
                    last_user, expected_response = pairs[-1]

                if isinstance(expected_response, list):
                    expected_response = " ".join(str(p) for p in expected_response)

                score, reason = call_llm_judge(
                    args.url, api_key, args.judge,
                    result["state_json"], last_user, expected_response,
                    delay=1.0
                )
                scores.append(score)
                symbol = "★" * score + "☆" * (5 - score) if score > 0 else "ERR"
                print(f"    [{i+1}/{len(judge_sample)}] {symbol} ({score}/5) {reason[:60]}")

            valid_scores = [s for s in scores if s > 0]
            if valid_scores:
                s["judge_mean"] = statistics.mean(valid_scores)
                s["judge_median"] = statistics.median(valid_scores)
                s["judge_n"] = len(valid_scores)
                print(f"\n    → Mean: {s['judge_mean']:.2f}/5, Median: {s['judge_median']:.1f}/5 (n={len(valid_scores)})")
            else:
                s["judge_mean"] = 0
                s["judge_median"] = 0
                s["judge_n"] = 0

        # Judge summary table
        print(f"\n  {'Domain':<30} {'WCS Mean':>10} {'WCS Median':>12} {'N':>5}")
        print(f"  {'─'*30} {'─'*10} {'─'*12} {'─'*5}")
        for domain_name, s in all_summaries.items():
            if s.get("judge_n", 0) > 0:
                print(f"  {domain_name:<30} {s['judge_mean']:>10.2f} {s['judge_median']:>12.1f} {s['judge_n']:>5}")
        print()

    # Save final results (with judge scores)
    if args.output:
        output_data = {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "config": {
                "url": args.url,
                "model": args.model,
                "threshold": THRESHOLD,
                "delay": args.delay,
                "seed": args.seed,
                "n": args.n,
                "judge_model": args.judge,
                "judge_n": args.judge_n,
            },
            "domains": {},
        }
        for domain_name, s in all_summaries.items():
            summary_copy = {k: v for k, v in s.items() if k != "results"}
            summary_copy["results"] = s["results"]
            output_data["domains"][domain_name] = summary_copy

        outpath = Path(args.output)
        outpath.parent.mkdir(parents=True, exist_ok=True)
        with open(outpath, "w") as f:
            json.dump(output_data, f, indent=2, default=str)
        print(f"Results saved to: {args.output}")


if __name__ == "__main__":
    main()
