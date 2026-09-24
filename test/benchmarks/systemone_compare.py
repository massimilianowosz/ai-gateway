#!/usr/bin/env python3
"""Which SYSTEMONE model should label the gateway's sessions?

Sends every task_classifier_bench case to /v1/compare, which answers the same
request with each loaded model, and scores them the way the gateway uses them:
an answer only counts above min_confidence, so accuracy there, and how many
answers clear it, matter more than raw accuracy.

    set -a; . ./.env; set +a
    python3 test/benchmarks/systemone_compare.py [--min-confidence 0.5]
"""
import argparse
import json
import os
import statistics
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from task_classifier_bench import CASES, DIFF_Q, TYPE_Q  # noqa: E402

BASE = os.environ.get("JEV_ENDPOINT_URL", "http://127.0.0.1:8000/v1/systemone").rsplit("/v1/", 1)[0]
KEY = os.environ.get("JEV_API_KEY", "").strip()
# Optional hosted JEV, asked the same questions next to the local models.
REMOTE_URL = os.environ.get("JEV_ENDPOINT_URL_REMOTE", "").strip()
REMOTE_KEY = os.environ.get("JEV_API_KEY_REMOTE", "").strip()
REMOTE_MODEL = os.environ.get("JEV_MODEL_REMOTE", "jev-latest")


def post(url, key, payload):
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    )
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.load(r)


def compare(text):
    questions = {"kind": TYPE_Q, "difficulty": DIFF_Q}
    results = post(BASE + "/v1/compare", KEY,
                   {"state": text, "model": "systemone-latest", "questions": questions})["results"]
    if REMOTE_URL and REMOTE_KEY:
        name = f"{REMOTE_MODEL} (remote)"
        try:
            t0 = time.time()
            results[name] = post(REMOTE_URL, REMOTE_KEY, {"state": text, "model": REMOTE_MODEL, "questions": questions})
            results[name]["_elapsed_ms"] = (time.time() - t0) * 1000
        except Exception as error:  # noqa: BLE001
            results[name] = {"error": str(error)}
    return results


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--min-confidence", type=float, default=0.5)
    args = ap.parse_args()
    if not KEY:
        sys.exit("set JEV_API_KEY")

    stats = {}
    for text, want_kind, want_diff in CASES:
        for name, res in compare(text).items():
            s = stats.setdefault(name, {"n": 0, "ok": 0, "kept": 0, "kept_ok": 0, "diff_ok": 0,
                                        "ms": [], "errors": 0, "misses": []})
            if "error" in res:
                s["errors"] += 1
                continue
            a = res["answers"]
            kind = a["kind"]["choice"]
            p = a["kind"].get("probabilities", {}).get(kind, 0.0)
            diff = round(a["difficulty"]["score"]) + 1
            s["n"] += 1
            s["ok"] += kind == want_kind
            s["diff_ok"] += abs(diff - want_diff) <= 1
            if p >= args.min_confidence:
                s["kept"] += 1
                s["kept_ok"] += kind == want_kind
            if kind != want_kind:
                s["misses"].append(f"{want_kind}->{kind} ({p:.2f}): {text[:60]}")
            s["ms"].append(res.get("x_systemone", {}).get("timing", {}).get("total_seconds", 0) * 1000
                           or res.get("_elapsed_ms", 0))

    print(f"{len(CASES)} cases, min_confidence {args.min_confidence}\n")
    print(f"{'model':<22}{'type':>8}{'kept':>8}{'kept ok':>10}{'diff ±1':>10}{'p50 ms':>9}{'errors':>8}")
    for name, s in sorted(stats.items(), key=lambda kv: -kv[1]["kept_ok"]):
        n = s["n"] or 1
        kept = s["kept"] or 1
        p50 = statistics.median(s["ms"]) if s["ms"] else 0
        print(f"{name:<22}{s['ok'] / n:>8.0%}{s['kept']:>5}/{s['n']:<2}{s['kept_ok'] / kept:>10.0%}"
              f"{s['diff_ok'] / n:>10.0%}{p50:>9.0f}{s['errors']:>8}")
    for name, s in stats.items():
        if s["misses"]:
            print(f"\n{name} misses:")
            for m in s["misses"]:
                print("  " + m)


if __name__ == "__main__":
    main()
