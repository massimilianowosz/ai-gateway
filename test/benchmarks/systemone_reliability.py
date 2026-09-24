#!/usr/bin/env python3
"""Is systemone reliable enough to label hivetrace sessions?

Accuracy on a handful of cases answers the wrong question. A classifier that is
right 90% of the time but changes its mind when you add "please" to the prompt
cannot be shown to a user. So this measures the properties that decide whether
the output can be trusted, most of which need no ground truth at all:

  determinism   same input twice, same answer? anything less is unusable
  invariance    does a semantics-preserving rewrite flip the label?
  order bias    does permuting the option list flip the label?
  calibration   does `confidence` mean anything (ECE, reliability diagram)?
  abstention    is there a threshold that buys precision at acceptable coverage?
  scale usage   does the score question ever leave the middle of its range?

The abstention curve is the practical result: if no threshold reaches useful
precision, the classifier cannot be wired into hivetrace whatever its accuracy.
"""
import argparse
import json
import os
import random
import statistics
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from task_classifier_bench import CASES, TYPES, DIFF_Q, TYPE_Q  # noqa: E402

ENDPOINT = os.environ.get("HIVEDECIDE_URL", "http://127.0.0.1:8000/v1/systemone")


def api_key():
    key = os.environ.get("HIVEDECIDE_API_KEY", "").strip()
    if key:
        return key
    path = os.path.expanduser("~/code/hivedecide/.api-key")
    if os.path.exists(path):
        return open(path).read().strip()
    sys.exit("set HIVEDECIDE_API_KEY")


KEY = api_key()


def ask(text, model, criteria_order=None):
    """One call, both questions. Returns (kind, kind_conf, level, diff_conf, ms)."""
    type_q = dict(TYPE_Q)
    if criteria_order:
        type_q["criteria"] = {k: TYPES[k] for k in criteria_order}
    payload = {
        "state": text,
        "model": model,
        "questions": {"kind": type_q, "difficulty": DIFF_Q},
    }
    req = urllib.request.Request(
        ENDPOINT,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY},
    )
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=180) as r:
        res = json.load(r)
    ms = (time.time() - t0) * 1000
    k, d = res["answers"]["kind"], res["answers"]["difficulty"]
    return (
        k["choice"],
        k.get("confidence", 0.0),
        round(d["score"]) + 1,  # wire is 0-based, labels are 1-5
        d.get("confidence", 0.0),
        ms,
    )


# Rewrites a human would consider identical in meaning.
PERTURBATIONS = [
    ("padding", lambda t: "  " + t + "  "),
    ("lowercase", lambda t: t.lower()),
    ("polite", lambda t: "Please " + t[0].lower() + t[1:]),
    ("greeting", lambda t: "Hi! " + t + " Thanks!"),
    ("fenced", lambda t: "```\n" + t + "\n```"),
    ("typo", lambda t: t.replace("the ", "teh ", 1) if "the " in t else t + " ,"),
]


def pct(n, d):
    return 100.0 * n / d if d else 0.0


def ece(points, bins=5):
    """Expected calibration error: |confidence - accuracy| weighted by bin size."""
    total, err = len(points), 0.0
    for i in range(bins):
        lo, hi = i / bins, (i + 1) / bins
        b = [p for p in points if (lo <= p[0] < hi) or (i == bins - 1 and p[0] == 1.0)]
        if not b:
            continue
        acc = sum(1 for _, ok in b if ok) / len(b)
        conf = sum(c for c, _ in b) / len(b)
        err += (len(b) / total) * abs(conf - acc)
    return err


def diagram(points, bins=5):
    rows = []
    for i in range(bins):
        lo, hi = i / bins, (i + 1) / bins
        b = [p for p in points if (lo <= p[0] < hi) or (i == bins - 1 and p[0] == 1.0)]
        if not b:
            rows.append((f"{lo:.1f}-{hi:.1f}", 0, None, None))
            continue
        acc = sum(1 for _, ok in b if ok) / len(b)
        conf = sum(c for c, _ in b) / len(b)
        rows.append((f"{lo:.1f}-{hi:.1f}", len(b), conf, acc))
    return rows


def abstention(points):
    """Precision and coverage on the kept subset as the threshold rises."""
    out = []
    for thr in [0.0, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95]:
        kept = [p for p in points if p[0] >= thr]
        if not kept:
            out.append((thr, 0.0, None))
            continue
        prec = sum(1 for _, ok in kept if ok) / len(kept)
        out.append((thr, pct(len(kept), len(points)), prec))
    return out


def run(model, repeats, order_shuffles, seed):
    rng = random.Random(seed)
    keys = list(TYPES)

    base = {}          # text -> (kind, kconf, diff, dconf)
    latencies = []
    nondeterministic = []
    kind_points = []   # (confidence, correct) for calibration
    diff_points = []
    kind_hits = diff_hits = 0
    flips = {name: 0 for name, _ in PERTURBATIONS}
    diff_flips = {name: 0 for name, _ in PERTURBATIONS}
    order_flips = 0
    order_trials = 0
    levels = []

    print(f"\n=== {model} ===")
    print(f"  {len(CASES)} cases x ({repeats} repeats + {len(PERTURBATIONS)} rewrites "
          f"+ {order_shuffles} option orders)")

    for idx, (text, exp_kind, exp_diff) in enumerate(CASES, 1):
        try:
            kind, kconf, diff, dconf, ms = ask(text, model)
        except (urllib.error.URLError, KeyError, TimeoutError) as exc:
            print(f"  [{idx}] ERROR {exc}")
            continue
        base[text] = (kind, kconf, diff, dconf)
        latencies.append(ms)
        levels.append(diff)

        k_ok, d_ok = kind == exp_kind, abs(diff - exp_diff) <= 1
        kind_hits += k_ok
        diff_hits += d_ok
        kind_points.append((kconf, k_ok))
        diff_points.append((dconf, d_ok))

        for _ in range(repeats - 1):
            k2, _, d2, _, ms2 = ask(text, model)
            latencies.append(ms2)
            if k2 != kind or d2 != diff:
                nondeterministic.append((text, (kind, diff), (k2, d2)))

        for name, fn in PERTURBATIONS:
            k3, _, d3, _, ms3 = ask(fn(text), model)
            latencies.append(ms3)
            if k3 != kind:
                flips[name] += 1
            if d3 != diff:
                diff_flips[name] += 1

        for _ in range(order_shuffles):
            shuffled = keys[:]
            rng.shuffle(shuffled)
            k4, _, _, _, ms4 = ask(text, model, criteria_order=shuffled)
            latencies.append(ms4)
            order_trials += 1
            if k4 != kind:
                order_flips += 1

        mark = f"{'K' if k_ok else '.'}{'D' if d_ok else '.'}"
        print(f"  [{idx:2}] {mark} {kind:<10} conf={kconf:.2f}  lvl={diff} "
              f"conf={dconf:.2f}  {text[:44]}")

    n = len(base)
    if not n:
        print("  no results")
        return

    print(f"\n  -- accuracy (for reference only) --")
    print(f"  task type   {kind_hits}/{n}  ({pct(kind_hits, n):.0f}%)")
    print(f"  difficulty  {diff_hits}/{n}  ({pct(diff_hits, n):.0f}%) within +/-1")

    print(f"\n  -- determinism --")
    if repeats < 2:
        print("  skipped (--repeats 1)")
    elif nondeterministic:
        print(f"  UNSTABLE: {len(nondeterministic)} disagreeing repeats")
        for text, a, b in nondeterministic[:5]:
            print(f"    {a} -> {b}  {text[:40]}")
    else:
        print(f"  stable across {repeats} repeats of every case")

    print(f"\n  -- invariance to semantics-preserving rewrites --")
    tk = sum(flips.values())
    td = sum(diff_flips.values())
    for name, _ in PERTURBATIONS:
        print(f"    {name:<10} type {flips[name]:>2}/{n} ({pct(flips[name], n):4.0f}%)"
              f"   level {diff_flips[name]:>2}/{n} ({pct(diff_flips[name], n):4.0f}%)")
    print(f"    {'TOTAL':<10} type {tk:>2}/{n * len(PERTURBATIONS)} "
          f"({pct(tk, n * len(PERTURBATIONS)):4.0f}%)   "
          f"level {td:>2}/{n * len(PERTURBATIONS)} ({pct(td, n * len(PERTURBATIONS)):4.0f}%)")

    print(f"\n  -- option-order bias --")
    if order_trials:
        print(f"  label changed in {order_flips}/{order_trials} permutations "
              f"({pct(order_flips, order_trials):.0f}%)")
    else:
        print("  skipped")

    print(f"\n  -- calibration of `confidence` --")
    print(f"  ECE  type {ece(kind_points):.3f}   difficulty {ece(diff_points):.3f}"
          "   (0 = perfect, >0.15 = do not surface as a percentage)")
    print(f"  {'bin':<10}{'n':>4}{'mean conf':>11}{'accuracy':>10}")
    for label, cnt, conf, acc in diagram(kind_points):
        if not cnt:
            print(f"  {label:<10}{0:>4}{'-':>11}{'-':>10}")
        else:
            print(f"  {label:<10}{cnt:>4}{conf:>11.2f}{acc:>10.0%}")

    print(f"\n  -- abstention (task type) --")
    print(f"  {'threshold':<11}{'coverage':>10}{'precision':>11}")
    for thr, cov, prec in abstention(kind_points):
        p = f"{prec:.0%}" if prec is not None else "-"
        print(f"  >= {thr:<8.2f}{cov:>9.0f}%{p:>11}")

    print(f"\n  -- difficulty scale usage --")
    hist = {lv: levels.count(lv) for lv in range(1, 6)}
    span = max(levels) - min(levels) if levels else 0
    for lv in range(1, 6):
        bar = "#" * hist[lv]
        print(f"    {lv}  {hist[lv]:>2}  {bar}")
    print(f"    distinct levels used: {len(set(levels))}/5, span {span}")

    latencies.sort()
    p = lambda q: latencies[min(len(latencies) - 1, int(q * len(latencies)))]
    print(f"\n  -- latency ({len(latencies)} calls) --")
    print(f"  p50 {p(0.50):.0f} ms   p95 {p(0.95):.0f} ms   max {latencies[-1]:.0f} ms")
    print(f"  mean {statistics.mean(latencies):.0f} ms")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("models", nargs="*", default=["spark-4b"])
    ap.add_argument("--repeats", type=int, default=2)
    ap.add_argument("--order-shuffles", type=int, default=2)
    ap.add_argument("--seed", type=int, default=7)
    args = ap.parse_args()
    for m in args.models or ["spark-4b"]:
        run(m, args.repeats, args.order_shuffles, args.seed)
