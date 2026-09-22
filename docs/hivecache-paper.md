# HiveCache

## Dual-Embedding Semantic Reuse for Multi-Turn LLM Applications

**by Ubiquum**

---

## Abstract

HiveCache is a lightweight dual-scoring semantic cache for multi-turn LLM workloads. It intercepts requests at the HTTP middleware layer, scores candidates using a weighted combination of query and context embeddings, and returns cached responses when similarity exceeds configurable thresholds. We evaluate it across three operational scenarios — *cold start*, *warm generalization*, and *incremental learning* — on Banking77 (single-turn, 1,000 queries/run) and MultiWOZ 2.2 (multi-turn, 100 dialogues/run). Single-turn caches learn intent clusters and saturate after one pass (42% cold, ~77% after three passes). Multi-turn caches learn reusable conversational patterns: hit rate rises from 16% cold to 58% on entirely unseen dialogues after a single warm pass. Retrieval precision ranges from 90.5% to 98.2% depending on threshold. All mismatches occur between semantically adjacent intent pairs.

**Keywords:** semantic cache · LLM cost reduction · multi-turn dialogue · dual-embedding scoring · retrieval precision

---

### Key Findings

- Single-turn caches learn intent clusters and saturate after one pass.
- Multi-turn caches benefit from warm-start pattern accumulation — 16% → 58% on unseen dialogues.
- Retrieval precision exceeds 90% across all configurations; reaches 98.2% at threshold ≥ 0.95.
- FAQ workloads exceed 75% steady-state hit rate after three passes of representative traffic.

---

## 1  System Architecture

HiveCache is positioned as a lightweight dual-scoring semantic cache for multi-turn LLM workloads. It embeds both the query and conversation context via a **configurable embedding provider** and returns cached responses when similarity exceeds configurable thresholds. The scoring function is:

```
DualScore = α × cos(q_a, q_b) + β × cos(ctx_a, ctx_b)
```

with α = 0.85 and β = 0.15, selected empirically during early internal testing and kept fixed across all experiments reported here. The decomposition addresses the *polysemy problem* in dialogue: "Book it for 2 people" resolves to different targets depending on whether prior context concerns a restaurant or a hotel. Context is capped at the last 2,000 characters — earlier turns have diminishing relevance and introducing them degrades embedding quality [4].

### 1.1  Response Bands

| Band | Score | Behavior |
|------|:-----:|----------|
| DIRECT | ≥ 0.95 | Return cached response verbatim |
| REUSE | ≥ 0.90 | Return cached response as-is |
| TWEAK | ≥ 0.85 | Adapt via configurable adaptation model |
| MISS | < 0.85 | Full LLM call; cache result |

*Table 1: Tiered response bands.*

---

## 2  Experimental Design

Three phases isolate distinct cache properties. **P1** (seed 42, cache flushed) measures cold-start baseline. **P2** (seed 92, cache retained from P1) tests generalization: the cache is populated from P1, then evaluated on 100 entirely unseen dialogues drawn with a different random seed, ensuring no dialogue or query overlap between phases. **P3** (seed 142, no flush, 3 consecutive runs) evaluates incremental growth.

All runs use **Gemma 4 12B** (completion) and **phi4-mini 3.8B** (TWEAK adaptation), both served locally via Ollama to eliminate cloud-latency variability and ensure reproducible measurements. Datasets: **Banking77** [5] — 77 fine-grained banking intents (CC-BY-4.0); **MultiWOZ 2.2** [6] — 5 task-oriented domains (Apache 2.0).

---

## 3  Results

### 3.1  Overview

| Phase | Banking77 Hit Rate | MultiWOZ Hit Rate | Retrieval Precision (B77) |
|-------|:------------------:|:-----------------:|:-------------------------:|
| P1 — Cold Start | 42.3% | 16.2% | 90.5% |
| P2 — Warm Generaliz. | 42.4% | 57.9% | 90.6% |
| P3 Run 1 | 65.7% | 31.5% | — |
| P3 Run 2 | 74.1% | 45.1% | — |
| P3 Run 3 | 76.7% | 51.2% | 93.1% |

*Table 2: Hit rates and retrieval precision across all phases.*

Figure 1 illustrates a key asymmetry: single-turn caches learn intent clusters and saturate quickly — one cold pass already covers Banking77's 77 clusters, so P2 adds only +0.1 pp. Multi-turn caches learn reusable conversational patterns; the 714 turns seen in P1 are sufficient for the cache to match 58% of entirely new P2 dialogues, a +41.7 pp gain over cold start.

*Figure 1: Single-turn hit rate plateaus after the first pass; multi-turn hit rate nearly quadruples once the cache is warm.*

### 3.2  Domain Analysis (Multi-Turn)

Entity specificity is the primary predictor of cache difficulty (Figure 2). Taxi and attraction domains reference unique place names and origin/destination pairs that recur rarely across dialogues, limiting generalization. Closing sequences, by contrast, draw from a finite, domain-independent vocabulary and reach 87.5% in warm mode.

*Figure 2: Warm cache (P2) improves all domains; entity-specific domains (taxi, attraction) remain hardest.*

### 3.3  Turn-Position Effect

Later turns are systematically more cacheable because task-oriented dialogues undergo *conversational convergence*: by turn 7+, exchanges concentrate on confirmations, closings and slot-filling recaps that share a far smaller vocabulary than early-turn specification exchanges (Figure 3). The warm-start gain is uniform across all positions (~40 pp), showing HiveCache accumulates useful patterns at every conversational depth.

*Figure 3: Hit rate grows steadily with turn depth as dialogue vocabulary converges; warm cache lifts every position by ~40 pp.*

---

## 4  Operational Cost Implications

To illustrate potential savings, we project cloud-equivalent monthly costs for 1M queries/month (80 input + 150 output tokens/request) at steady-state hit rates using Q2 2025 public API pricing. These are not costs measured in the local Ollama setup (zero marginal API cost) but illustrative projections. For a blended workload (70% FAQ + 30% multi-turn), the effective hit rate is 0.7×76.7% + 0.3×51.2% = **69.1%**.

| Model | No Cache ($/mo) | Cached ($/mo) | Savings — Single | Savings — Multi |
|-------|:---------------:|:-------------:|:----------------:|:---------------:|
| GPT-4o | $1,700 | $396 | $1,304 | $870 |
| Claude Sonnet 4 | $2,490 | $580 | $1,910 | $1,274 |
| Gemini 2.5 Pro | $1,225 | $286 | $939 | $627 |
| GPT-4o-mini | $100 | $23 | $77 | $51 |

*Table 3: Illustrative monthly cost projections at steady-state hit rates (76.7% single-turn; 51.2% multi-turn; 1M queries/month).*

---

## 5  Discussion

### 5.1  What HiveCache Learns

HiveCache does not learn individual dialogues. Instead, it accumulates reusable semantic and conversational patterns that recur across otherwise unrelated dialogues. This is why the warm-start effect is so pronounced in multi-turn workloads: structurally similar exchanges — a hotel booking negotiation, a train reservation confirmation — share embedding neighborhoods even when the specific entities (city names, dates, prices) differ. Single-turn caches exhibit no such warm-start gain because each intent cluster is already fully covered after one random pass.

### 5.2  Retrieval Precision and Threshold Trade-off

Retrieval precision improves monotonically with stricter thresholds (Figure 4). At threshold ≥ 0.95, precision reaches 98.2% with only 11 mismatches out of 600 hits. Notably, no mismatch spans semantically distant categories: all errors occur between adjacent intent pairs (e.g., *declined_transfer* ↔ *failed_transfer*), which would often warrant identical responses in production.

*Figure 4: Raising the threshold trades hit rate for retrieval precision — 98.2% precision at ≥ 0.95 with 20% hit rate.*

### 5.3  Limitations

- **DIRECT band inflation.** Finite dataset sampling causes ~38 verbatim collisions per 1,000 Banking77 queries (birthday problem). Production DIRECT share is expected at 5–10% of hits, not the 15–30% observed here.
- **Single-machine, sequential traffic.** Distributed or concurrent scenarios may exhibit different latency and race-condition behavior.
- **Unbounded memory backend.** No eviction policy applied; production deployments with capacity limits may show lower steady-state rates.
- **English only.** Embedding quality for other languages may require threshold re-tuning.

---

## 6  Conclusions

- **Pre-warm multi-turn caches.** Seeding with 500–1,000 representative conversations eliminates the 16% cold-start period and immediately delivers 50%+ hit rates.
- **Cache selectively from turn 5+.** Conversational convergence concentrates most caching value in later turns; early-turn caching risks false positives from insufficient context.
- **Monitor per-domain hit rates.** Entity-specific domains (taxi, attraction) may never achieve high rates and should be excluded from savings projections.
- **Tune threshold by precision requirement.** ≥ 0.94 → 96.2% precision at 28.4% hit rate; ≥ 0.95 → 98.2% precision at 20% hit rate.
- **Expect logarithmic saturation.** ~80% of benefit is captured within the first 3–4 passes of representative traffic.

---

## References

[1] Zhu, F., et al. "GPTCache: An Open-Source Semantic Cache for LLM Applications." *arXiv:2311.07629*, 2023.

[2] Karpukhin, V., et al. "Dense Passage Retrieval for Open-Domain Question Answering." *EMNLP*, 2020.

[3] Reimers, N. & Gurevych, I. "Sentence-BERT: Sentence Embeddings using Siamese BERT-Networks." *EMNLP*, 2019.

[4] Liu, N., et al. "Lost in the Middle: How Language Models Use Long Contexts." *TACL*, 2024.

[5] Casanueva, I., et al. "Efficient Intent Detection with Dual Sentence Encoders." *NLP4ConvAI Workshop, ACL*, 2020.

[6] Zang, X., et al. "MultiWOZ 2.2: A Dialogue Dataset with Additional Annotation Corrections." *NLP4ConvAI Workshop, ACL*, 2020.

[7] Muennighoff, N., et al. "MTEB: Massive Text Embedding Benchmark." *EACL*, 2023.
