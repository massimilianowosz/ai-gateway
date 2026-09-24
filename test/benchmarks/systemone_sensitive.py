#!/usr/bin/env python3
"""Can systemone make the sensitive-data findings list trustworthy?

The gateway already has regex detectors, so the question is never "can a model
find a secret". It is whether a model can fix the two ways regex is wrong:

  triage    regex fired on a placeholder, a git SHA, a revoked key. Can the
            model drop those without dropping real leaks? This buys precision.
  backstop  regex stayed silent on a password in prose, a DSN, a kubeconfig.
            Can the model raise those without flooding the list? This buys
            recall.

Both are measured against the production detectors, run by the Go program in
./scanbaseline over the same corpus, so the comparison is against the thing
that would have to be replaced rather than against nothing.

Reliability is measured the same way as the task classifier: a detector that
changes its mind when the text is re-wrapped is not deployable at any accuracy.

    go run ./test/benchmarks/scanbaseline < test/benchmarks/sensitive_corpus.json > /tmp/baseline.json
    python3 test/benchmarks/systemone_sensitive.py spark-4b laya-multilingual
"""
import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
CORPUS = os.path.join(HERE, "sensitive_corpus.json")
ENDPOINT = os.environ.get("JEV_ENDPOINT_URL", "http://127.0.0.1:8000/v1/systemone")


def api_key():
    key = os.environ.get("JEV_API_KEY", "").strip()
    if not key:
        sys.exit("set JEV_API_KEY")
    return key


KEY = api_key()

# Asked the way an operator would ask it. "Looks like a secret" is what regex
# already answers; the only question worth spending a model on is whether the
# thing would actually give somebody access.
CRED_Q = {
    "type": "choice",
    "instructions": (
        "Does this text contain a credential that would give someone access to "
        "a real system if they read it?"
    ),
    "criteria": {
        "yes": "a live password, API key, token, private key or connection string that still works",
        "no": "no credential, or only a placeholder, example, revoked key, public identifier, hash or commit id",
    },
}
PII_Q = {
    "type": "choice",
    "instructions": "Does this text contain personal data about an identifiable person?",
    "criteria": {
        "yes": "names, contact details, identity numbers, financial or health data about a real person",
        "no": "no personal data, only technical content or fictional examples",
    },
}

# Rewrites that must not change the answer.
PERTURBATIONS = [
    ("padding", lambda t: "  " + t + "  "),
    ("fenced", lambda t: "```\n" + t + "\n```"),
    ("prefixed", lambda t: "Here is the output you asked for:\n\n" + t),
    ("suffixed", lambda t: t + "\n\nLet me know if you need anything else."),
]


def ask(text, model):
    payload = {
        "state": text,
        "model": model,
        "questions": {"credential": CRED_Q, "personal": PII_Q},
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
    a = res["answers"]
    # The probability of "yes" is the score; the choice is just its threshold
    # at 0.5, and a threshold is exactly what has to be chosen deliberately.
    return (
        a["credential"]["probabilities"].get("yes", 0.0),
        a["personal"]["probabilities"].get("yes", 0.0),
        ms,
    )


def baseline():
    out = subprocess.run(
        ["go", "run", "./test/benchmarks/scanbaseline"],
        cwd=ROOT,
        stdin=open(CORPUS),
        capture_output=True,
        check=True,
    )
    rows = json.loads(out.stdout)
    for r in rows:
        r["secrets"] = r.get("secrets") or []
        r["pii_types"] = r.get("pii_types") or []
    return {r["id"]: r for r in rows}


def counts(pairs):
    """pairs: (predicted, actual)."""
    tp = sum(1 for p, a in pairs if p and a)
    fp = sum(1 for p, a in pairs if p and not a)
    fn = sum(1 for p, a in pairs if not p and a)
    tn = sum(1 for p, a in pairs if not p and not a)
    return tp, fp, fn, tn


def prf(pairs):
    tp, fp, fn, tn = counts(pairs)
    prec = tp / (tp + fp) if tp + fp else 0.0
    rec = tp / (tp + fn) if tp + fn else 0.0
    f1 = 2 * prec * rec / (prec + rec) if prec + rec else 0.0
    return prec, rec, f1, (tp, fp, fn, tn)


def auc(scored):
    """scored: (score, label). Probability a positive outranks a negative."""
    pos = [s for s, y in scored if y]
    neg = [s for s, y in scored if not y]
    if not pos or not neg:
        return float("nan")
    wins = sum((p > n) + 0.5 * (p == n) for p in pos for n in neg)
    return wins / (len(pos) * len(neg))


def line(label, prec, rec, f1, c):
    tp, fp, fn, tn = c
    print(f"  {label:<26} precision {prec:>4.0%}   recall {rec:>4.0%}   F1 {f1:>4.2f}"
          f"   (tp {tp} fp {fp} fn {fn} tn {tn})")


def run(model, items, base, repeats):
    print(f"\n{'=' * 78}\n=== {model} ===")

    scores = {}
    latencies = []
    flips = {name: 0 for name, _ in PERTURBATIONS}
    unstable = []

    for item in items:
        try:
            cred, pii, ms = ask(item["text"], model)
        except Exception as exc:  # noqa: BLE001
            print(f"  ERROR {item['id']}: {exc}")
            continue
        scores[item["id"]] = (cred, pii)
        latencies.append(ms)

        for _ in range(repeats - 1):
            c2, p2, ms2 = ask(item["text"], model)
            latencies.append(ms2)
            if abs(c2 - cred) > 1e-9 or abs(p2 - pii) > 1e-9:
                unstable.append(item["id"])

        for name, fn_ in PERTURBATIONS:
            c3, _, ms3 = ask(fn_(item["text"]), model)
            latencies.append(ms3)
            if (c3 >= 0.5) != (cred >= 0.5):
                flips[name] += 1

    scored = [(scores[i["id"]][0], i.get("leak", False)) for i in items if i["id"] in scores]
    scored_pii = [(scores[i["id"]][1], i.get("pii", False)) for i in items if i["id"] in scores]

    print(f"\n  -- separability --")
    print(f"  credential AUC {auc(scored):.2f}   personal-data AUC {auc(scored_pii):.2f}"
          "   (0.5 = coin flip, 1.0 = a threshold exists)")

    print(f"\n  -- per item (credential score, label, what regex said) --")
    for item in items:
        if item["id"] not in scores:
            continue
        cred, _ = scores[item["id"]]
        b = base[item["id"]]
        regex = ",".join(b["secrets"]) or "-"
        mark = "!" if (cred >= 0.5) != item.get("leak", False) else " "
        print(f"  {mark} {item['id']:<22} {cred:>5.2f}  leak={str(item.get('leak', False)):<5}"
              f" regex={regex[:26]}")

    print(f"\n  -- standalone, at the natural 0.5 threshold --")
    line("systemone alone", *prf([(s >= 0.5, y) for s, y in scored]))
    line("regex alone", *prf([(bool(base[i['id']]['secrets']), i.get('leak', False))
                             for i in items if i["id"] in scores]))

    # Triage: regex decides what to look at, the model decides what to keep.
    print(f"\n  -- triage: keep a regex hit only if the model agrees --")
    for thr in (0.2, 0.3, 0.5, 0.7):
        pairs = []
        for i in items:
            if i["id"] not in scores:
                continue
            fired = bool(base[i["id"]]["secrets"])
            pairs.append((fired and scores[i["id"]][0] >= thr, i.get("leak", False)))
        line(f"threshold {thr}", *prf(pairs))

    # Backstop: the model may raise a finding regex never saw.
    print(f"\n  -- backstop: regex hits, plus anything the model is sure about --")
    for thr in (0.7, 0.8, 0.9):
        pairs = []
        for i in items:
            if i["id"] not in scores:
                continue
            fired = bool(base[i["id"]]["secrets"])
            pairs.append((fired or scores[i["id"]][0] >= thr, i.get("leak", False)))
        line(f"threshold {thr}", *prf(pairs))

    print(f"\n  -- determinism --")
    if repeats < 2:
        print("  skipped")
    elif unstable:
        print(f"  UNSTABLE on {len(set(unstable))} items: {sorted(set(unstable))[:5]}")
    else:
        print(f"  identical scores across {repeats} repeats of every item")

    print(f"\n  -- invariance (credential verdict flipping on a rewrite) --")
    n = len(scores)
    total = 0
    for name, _ in PERTURBATIONS:
        total += flips[name]
        print(f"    {name:<10} {flips[name]:>2}/{n} ({100 * flips[name] / max(1, n):4.0f}%)")
    print(f"    {'TOTAL':<10} {total:>2}/{n * len(PERTURBATIONS)} "
          f"({100 * total / max(1, n * len(PERTURBATIONS)):4.0f}%)")

    latencies.sort()
    if latencies:
        p = lambda q: latencies[min(len(latencies) - 1, int(q * len(latencies)))]
        print(f"\n  -- latency ({len(latencies)} calls) --")
        print(f"  p50 {p(0.5):.0f} ms   p95 {p(0.95):.0f} ms")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("models", nargs="*", default=["spark-4b"])
    ap.add_argument("--repeats", type=int, default=2)
    args = ap.parse_args()

    items = json.load(open(CORPUS))["items"]
    base = baseline()

    print(f"corpus: {len(items)} items, "
          f"{sum(1 for i in items if i.get('leak'))} of them real credential leaks")

    for m in args.models or ["spark-4b"]:
        run(m, items, base, args.repeats)
