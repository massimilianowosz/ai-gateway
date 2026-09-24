#!/usr/bin/env python3
"""Compare task classifiers on the job hivetrace would actually give them.

Three candidates, same cases, same label set:
  spark-4b / laya   SYSTEMONE, local, logit scoring, no generation
  jev-1.13          generative, reached through the gateway

Labels are the opening user turn of an agent session, which is what hivetrace
captures first and what a "what was this session doing" answer must rest on.
Difficulty is scored 1-5 and judged within +/-1, because the boundary between
adjacent levels is a matter of taste even between humans; the type label is
exact-match because it is not.
"""
import json
import os
import re
import sys
import urllib.request

SYSTEMONE = os.environ.get("JEV_ENDPOINT_URL", "http://127.0.0.1:8000/v1/systemone")
GATEWAY = "http://127.0.0.1:4000/v1/chat/completions"
JEV_KEY = os.environ.get("JEV_API_KEY", "")
GW_KEY = os.environ.get("GATEWAY_KEY", "")

TYPES = {
    "coding": "writing or modifying application code",
    "debugging": "diagnosing a failure, error or wrong behaviour",
    "ops": "deploying, configuring infrastructure, CI or releases",
    "research": "reading, comparing or understanding something without changing it",
    "data": "querying, transforming or analysing data",
    "writing": "producing prose, docs or messages",
}

# (opening turn, expected type, expected difficulty 1-5)
CASES = [
    ("Add a --verbose flag to the CLI parser", "coding", 1),
    ("Rename the variable `res` to `response` across the handlers package", "coding", 1),
    ("Implement OAuth2 PKCE login with token refresh and keychain storage", "coding", 5),
    ("Write a function that reverses a linked list in place", "coding", 2),
    ("Port the payment module from Python to Go, keeping the retry semantics", "coding", 5),

    ("The tests pass locally but fail in CI with a nil pointer dereference", "debugging", 4),
    ("Why does this return 404? curl http://localhost:4000/v1/messages", "debugging", 2),
    ("Users report the app freezes after 10 minutes, no errors in the logs", "debugging", 5),
    ("Fix the typo in the error message, it says 'recieved'", "debugging", 1),

    ("Deploy the gateway to the staging cluster and restart the pods", "ops", 3),
    ("Set up a GitHub Actions workflow that builds and pushes a Docker image", "ops", 3),
    ("Rotate the database credentials in the production secret store", "ops", 4),
    ("Restart the nginx container", "ops", 1),

    ("Compare Postgres and ClickHouse for storing 500M rows of event data", "research", 4),
    ("What does the `errgroup` package do?", "research", 1),
    ("Summarise the differences between HTTP/2 and HTTP/3 for our use case", "research", 3),

    ("Load this CSV and show the median order value per region", "data", 2),
    ("Build a pipeline that deduplicates 40M records and flags anomalies", "data", 5),

    ("Write release notes for version 2.4 from the changelog", "writing", 2),
    ("Draft an email to the customer explaining the outage", "writing", 2),
]

TYPE_Q = {
    "type": "choice",
    "instructions": "What kind of task is the user asking for?",
    "criteria": {k: v for k, v in TYPES.items()},
}
DIFF_Q = {
    "type": "score",
    "instructions": "How hard is this task for a competent engineer?",
    # A list, and scored 0-based: the wire format ranks levels by position.
    "criteria": [
        "trivial, a one-line change",
        "easy, a few minutes",
        "moderate, needs some thought",
        "hard, multiple parts and edge cases",
        "very hard, open-ended design work",
    ],
}


def post(url, payload, key):
    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    )
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)


def ask_systemone(text, model):
    res = post(SYSTEMONE, {
        "state": text, "model": model,
        "questions": {"kind": TYPE_Q, "difficulty": DIFF_Q},
    }, JEV_KEY)
    a = res["answers"]
    ms = res.get("x_systemone", {}).get("timing", {}).get("total_seconds", 0) * 1000
    # Levels are 0-indexed on the wire; the labels in CASES are 1-5.
    return a["kind"]["choice"], round(a["difficulty"]["score"]) + 1, ms


JEV_PROMPT = (
    "Classify the engineering task below.\n"
    "Answer with exactly one line of JSON, nothing else:\n"
    '{"kind":"<one of: ' + ", ".join(TYPES) + '>","difficulty":<1-5>}\n\n'
    "Task: "
)


def ask_jev(text, model):
    import time
    t0 = time.time()
    res = post(GATEWAY, {
        "model": model, "max_tokens": 60, "temperature": 0,
        "messages": [{"role": "user", "content": JEV_PROMPT + text}],
    }, GW_KEY)
    ms = (time.time() - t0) * 1000
    out = res["choices"][0]["message"]["content"]
    m = re.search(r'\{.*\}', out, re.S)
    if not m:
        return "?", 0, ms
    try:
        d = json.loads(m.group(0))
        return str(d.get("kind", "?")), int(d.get("difficulty", 0)), ms
    except Exception:
        return "?", 0, ms


def run(name, fn, model):
    kind_ok = diff_ok = 0
    total_ms = 0.0
    rows = []
    for text, exp_kind, exp_diff in CASES:
        try:
            kind, diff, ms = fn(text, model)
        except Exception as exc:  # noqa: BLE001
            rows.append((text, "ERROR", str(exc)[:40], False, False))
            continue
        k_ok = kind == exp_kind
        d_ok = abs(diff - exp_diff) <= 1
        kind_ok += k_ok
        diff_ok += d_ok
        total_ms += ms
        rows.append((text, kind, diff, k_ok, d_ok))

    n = len(CASES)
    print(f"\n=== {name} ===")
    for text, kind, diff, k_ok, d_ok in rows:
        print(f"  {'K' if k_ok else '.'}{'D' if d_ok else '.'}  {str(kind):<10} {str(diff):<3} {text[:52]}")
    print(f"\n  task type  {kind_ok}/{n}  ({100*kind_ok/n:.0f}%)")
    print(f"  difficulty {diff_ok}/{n}  ({100*diff_ok/n:.0f}%)  within +/-1")
    print(f"  avg {total_ms/max(1,n):.0f} ms/case")
    return kind_ok / n, diff_ok / n


if __name__ == "__main__":
    which = sys.argv[1:] or ["spark-4b", "laya-multilingual", "jev-1.13"]
    summary = []
    for model in which:
        if model.startswith("jev"):
            summary.append((model, *run(f"jev via gateway ({model})", ask_jev, model)))
        else:
            summary.append((model, *run(f"systemone ({model})", ask_systemone, model)))
    print("\n" + "=" * 52)
    print(f"  {'model':<22} {'type':>8} {'difficulty':>12}")
    for model, k, d in summary:
        print(f"  {model:<22} {100*k:7.0f}% {100*d:11.0f}%")
