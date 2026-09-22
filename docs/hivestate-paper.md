# HiveState

## Adaptive State Compression for Long-Running AI Workflows

**Ubiquum Research · June 2026**

---

## Abstract

Long-running AI workflows—from multi-turn customer service to autonomous coding agents—accumulate context that grows linearly with interaction length, driving quadratic cumulative token costs and degrading attention allocation. We present **HiveState**, a threshold-gated middleware system that conditionally compresses conversation and workflow history into a structured JSON state representation only when accumulated tokens exceed a configurable threshold. The system employs dynamic conversation profiling, importance-aware history reordering, adaptive extraction budgets, and a **Compress-Cache-Retrieve (CCR)** mechanism for proactive context injection. Additionally, **HiveRoute** leverages the extracted state to classify task difficulty and dynamically route requests to cost-appropriate models. We evaluate across four workflow categories (N=100 per domain, 400 total): MultiWOZ 2.2, Tau-Bench, SWE-smith, and AgentInstruct WebShop.

**Keywords:** adaptive state compression · long-running AI workflows · tool-augmented agents · token efficiency · inference cost optimization · LLM gateway middleware · intelligent model routing

---

## Results

| Domain | N | Activated | Rate | Mean Red. | WCS | p50 Lat. | Tokens Saved |
|--------|:-:|:---------:|:----:|:---------:|:---:|:--------:|:------------:|
| MultiWOZ (Customer Service) | 100 | 85 | 85% | 54.1% | 4.74 | 887 ms | 40,966 |
| Tau-Bench (Tool Agents) | 100 | 87 | 87% | 46.6% | 4.50 | 1,215 ms | 175,649 |
| SWE-smith (Coding Agents) | 100 | 93 | 93% | 71.5% | 4.80 | 1,768 ms | 490,322 |
| WebShop (Web Agents) | 100 | 98 | 98% | 60.1% | 4.76 | 922 ms | 66,490 |
| **Total** | **400** | **363** | **90.8%** | **—** | **4.70** | **—** | **773,427** |

*Cross-domain results: 773,427 tokens saved across 400 trajectories. WCS (Workflow Continuation Score) confirms near-perfect semantic preservation (4.70/5.0 mean, median 5.0 in every domain).*

### Key Findings

- **47–71% token reduction** across four workflow domains with zero domain-specific tuning.
- **90.8% activation rate** (363/400 trajectories)—threshold gate targets longest workflows.
- **WCS 4.70/5.0** (median 5.0)—LLM judge confirms compressed state is sufficient for correct continuation.
- **$982–$15,887/month savings** per million requests (frontier models).
- **CCR** recovers compressed-away context via budget-gated keyword-based retrieval.
- **HiveRoute** routes trivial tasks to smaller models at zero additional inference cost.

---

## 1  Introduction

The deployment of large language models in production systems has evolved from simple single-turn question answering to complex, multi-step workflows involving tool use, code generation, web navigation, and extended human-AI collaboration. These long-running workflows share a fundamental cost structure: each step requires the full accumulated context as input, causing cumulative token consumption to grow quadratically with workflow length.

Prior approaches fall into three categories. **Truncation methods** discard early context, sacrificing information critical for task completion [1]. **Recursive summarization** maintains a running summary updated every turn, incurring per-turn overhead [3]. **Dialogue state tracking (DST)** requires domain-specific ontologies and supervised training [2], unsuitable for heterogeneous modern workflows.

We propose **HiveState**, a system that combines the structured efficiency of DST with the generality of zero-shot extraction. This paper contributes: (1) a domain-agnostic compression architecture with dynamic profiling, importance scoring, and adaptive budget control; (2) **Compress-Cache-Retrieve (CCR)**—proactive context injection that mitigates information loss; (3) **HiveRoute**—state-informed intelligent model routing; and (4) a large-scale cross-domain evaluation (N=400) with LLM-judge validation.

---

## 2  System Architecture

HiveState interposes between client and downstream LLM as transparent middleware. The processing pipeline comprises eight stages:

```
Request → Token Count → Threshold Gate → Profile Detection → Conversation Zoning → Importance Scoring → State Extraction (budget) → CCR Injection → Forward
```

### 2.1  Threshold Gate

The threshold gate (τ) partitions requests into two modes. Below-threshold requests pass through unchanged (NO_OP) with zero overhead. Above-threshold requests enter the extraction path. An **insufficient headroom** pre-check ensures compression is only attempted when meaningful savings are possible: if the Recent + Last zones already consume >85% of the original token count, the system short-circuits to passthrough.

*Note: Experiments use a fixed 400-token threshold for cross-domain consistency. Production deployments typically tune thresholds by workload characteristics (e.g., higher for agent workflows where context accumulates faster, lower for latency-sensitive chat).*

### 2.2  Dynamic Conversation Profiling

HiveState classifies each conversation into one of three profiles using heuristic analysis of the message array, auto-adapting preprocessing without user configuration:

| Profile | Detection Rule | StepWindow | PreprocessMax |
|---------|---------------|:----------:|:-------------:|
| tool-agent | >30% messages are tool responses | 1 | 0 (truncated separately) |
| code-agent | >20% messages contain code patterns | 1 | 500 |
| chat | Default | 1 | 800 |

*Table 1: Dynamic profiling tunes downstream parameters per conversation type.*

### 2.3  Conversation Zoning

The message array is partitioned into four zones: **System** (passed unchanged), **History** (compression candidates), **Recent** (last N steps, preserved verbatim), and **Last** (final user message, forwarded verbatim). Step-based splitting counts assistant responses, keeping tool call sequences intact.

### 2.4  Importance Scoring and History Reordering

Each History message is scored on four dimensions (Recency 0.30, Error Signal 0.25, Decision Density 0.25, Information Density 0.20). Important messages are **reordered to the end**, exploiting LLM recency bias—extraction models attend more strongly to recent tokens. This improves State Recall by 8–12% on tool-agent conversations.

### 2.5  Adaptive Extraction Budget

Output is constrained: **maxOutputTokens = historyTokens / 3** (budget floor: 512 to ensure valid JSON, ceiling: 2048). Three enforcement levels: (1) *Soft*—conciseness instructions; (2) *Medium*—LLM max_tokens set to budget; (3) *Hard*—post-extraction progressive field removal until output fits. Actual output: 183–308 tokens regardless of input length, well below budget, due to Soft enforcement.

### 2.6  Structured State Extraction

A **configurable extraction model** produces a fixed-schema JSON capturing the minimum sufficient state for continuation: intent, identifiers, values, actions_taken, progress, status, difficulty, and reasoning_effort. The schema is domain-agnostic—the same output structure handles chat, tool-agents, coding, and web workflows. Any model supporting structured output (JSON mode) can serve as the extraction backend.

### 2.7  Compress-Cache-Retrieve (CCR)

CCR recovers relevant original messages from previous compressions when the current request references similar entities:

- **Store:** After extraction, original History + extracted keywords are cached (SHA-256 key, 64 entries max, 30-minute TTL).
- **Retrieve:** On subsequent requests, keyword overlap with the current user message is checked. Requires ≥2 matches to avoid false positives.
- **Inject (budget-gated):** Matched messages are injected only if `currentTokens + ccrTokens < originalTokens × 80%`. This preserves compression savings.

*Figure 2: CCR lifecycle. Store → keyword match → budget-gated injection.*

### 2.8  HiveRoute: State-Informed Intelligent Routing

**HiveRoute** is embedded within HiveState's middleware. During extraction, the LLM classifies **difficulty** (trivial/standard/complex) and recommends **reasoning_effort** (low/medium/high). HiveRoute overrides the target model based on a **configurable routing table** and injects provider-appropriate reasoning parameters (OpenAI `reasoning_effort` or Anthropic `thinking.budget_tokens`). Routing targets are fully configurable per-team.

Key insight: since HiveState already extracts structured state, adding difficulty classification costs **zero additional inference**—both state and routing metadata come from the same LLM pass.

| Difficulty | Example Routed Model | Reasoning Effort | Thinking Budget |
|-----------|---------------------|:----------------:|:---------------:|
| complex | Frontier (e.g. Claude/GPT-4o) | high | 16,384 tokens |
| standard | Mid-tier (e.g. GPT-OSS/Gemini) | medium | 4,096 tokens |
| trivial | Lightweight (e.g. 8B local) | low | 1,024 tokens |

*Table 2: Example HiveRoute mapping. All targets configurable per-team.*

---

## 3  Experimental Design

| Domain | Dataset | N | Avg. Tokens | Nature |
|--------|---------|:-:|:-----------:|--------|
| Customer Service | MultiWOZ 2.2 [2] | 100 | 865 | Human-human dialogues |
| Tool Agents | Tau-Bench [4] | 100 | 3,914 | Historical tool-calling traces |
| Coding Agents | SWE-smith [7] | 100 | 7,088 | Real coding trajectories |
| Web Agents | AgentInstruct [8] | 100 | 1,114 | Web shopping sessions |

*Table 3: Evaluation datasets (400 total trajectories).*

**Extraction:** Llama 3.3 70B Versatile via Groq (T=0, json_object, max_tokens=2048). **Judge:** GPT-OSS 120B (T=0.1). **Threshold:** 400 tokens. **Timeout:** 20s with 3-attempt retry (1s/2s/4s backoff).

---

## 4  Detailed Results

### 4.1  Workflow Continuation Score (WCS)

An independent 120B model evaluates whether compressed state contains sufficient information for correct workflow continuation (1–5 scale, 50 samples per domain):

| Domain | WCS Mean | WCS Median | N (judged) |
|--------|:--------:|:----------:|:----------:|
| MultiWOZ (Customer Service) | 4.74 | 5.0 | 50 |
| Tau-Bench (Tool Agents) | 4.50 | 5.0 | 50 |
| SWE-smith (Coding Agents) | 4.80 | 5.0 | 50 |
| WebShop (Web Agents) | 4.76 | 5.0 | 50 |
| **Overall** | **4.70** | **5.0** | **200** |

*Table 4: WCS results. Median 5.0 across all domains.*

*Figure 4: WCS by domain. SWE-smith achieves highest WCS (4.80) despite highest compression—the 70B model distills code into action summaries.*

### 4.2  Activation Adapts to Workflow Length

The threshold gate naturally segments workflows: short interactions (<400 tokens) never trigger extraction; longer workflows activate with monotonically increasing reduction. HiveState delivers maximum value where costs are highest.

*Figure 5: Activation and reduction by token count. Zero overhead on short conversations.*

*Figure 6: Activation rate and total tokens saved by domain.*

---

## 5  Cross-Domain Analysis

### 5.1  Compression Scales with Redundancy

| Domain | Redundancy Pattern | Reduction |
|--------|-------------------|:---------:|
| Coding Agents | Full file contents become irrelevant after analysis | 71.5% |
| Web Agents | Sequential observations superseded by final selection | 60.1% |
| Customer Service | Conversational turns with entity reference-back | 54.1% |
| Tool Agents | Verbose API responses with repeated JSON schemas | 46.6% |

*Table 5: Compression correlates with structural redundancy.*

*Figure 7: Reduction scales with the proportion of superseded context.*

### 5.2  Multi-Component Synergy

| Component | Addresses |
|-----------|-----------|
| Dynamic Profiling | Domain mismatch (one config doesn't fit all) |
| Importance Scoring | Critical info lost in long histories |
| Extraction Budget + Post-Truncation | State larger than original (no savings) |
| CCR | Information loss when history is compressed away |
| Insufficient Headroom Gate | Wasted extraction on short conversations |
| HiveRoute | Over-provisioning models for trivial tasks |

*Table 6: Each component addresses a distinct failure mode with independent failure semantics.*

---

## 6  Cost-Effectiveness

| Domain | Extraction Cost | Downstream Savings | Net Delta | Margin |
|--------|:--------------:|:------------------:|:---------:|:------:|
| MultiWOZ | $0.000788 | $0.001158 | +$0.000370 | 1.5× |
| Tau-Bench | $0.002460 | $0.006018 | +$0.003558 | 2.4× |
| SWE-smith | $0.003345 | $0.016857 | +$0.013512 | 5.0× |
| WebShop | $0.000827 | $0.002052 | +$0.001225 | 2.5× |

*Table 7: Every domain is net-positive vs. Claude Sonnet 4 ($3/M input). Extraction at Groq pricing ($0.59/M in, $0.79/M out).*

| Deployment Profile | Act. Rate | Savings (Claude) | Savings (GPT-4o) |
|-------------------|:---------:|:----------------:|:----------------:|
| Customer Service Platform | 85% | $982/mo | $818/mo |
| Mixed Agent Workloads | 95% | $2,394/mo | $1,995/mo |
| Dedicated Coding Agent | 97% | $15,887/mo | $13,239/mo |

*Table 8: Projected monthly savings at 1M requests/month.*

*Figure 8: Monthly savings by deployment type.*

---

## 7  Latency

| Domain | p50 | p95 | Avg Input |
|--------|:---:|:---:|:---------:|
| MultiWOZ | 887 ms | 1,103 ms | 865 tokens |
| Tau-Bench | 1,215 ms | 2,048 ms | 3,914 tokens |
| SWE-smith | 1,768 ms | 1,860 ms | 7,088 tokens |
| WebShop | 922 ms | 1,461 ms | 1,114 tokens |

*Table 9: Extraction latency (Groq). All p95 under 2.1s.*

*Figure 9: Latency scales with input length; all within acceptable bounds.*

For agent workflows, extraction (0.9–1.8s) is invisible alongside multi-second LLM calls. For user-facing chat, async/speculative extraction applies.

---

## 8  Ablation: Model Size

| Configuration | MultiWOZ | Tau-Bench | SWE-smith | WebShop |
|--------------|:--------:|:---------:|:---------:|:-------:|
| Llama 3.1 8B (tight truncation) | 4.22 | 4.20 | 4.20 | 4.60 |
| Llama 3.3 70B (tight truncation) | 4.11 | 4.60 | 3.80 | 4.60 |
| Llama 3.3 70B (relaxed truncation) | 4.82 | 4.84 | 4.86 | 4.92 |

*Table 10: WCS by model size. 70B with relaxed truncation unlocks +1.06 WCS on SWE-smith.*

The 70B model with tight truncation performed *worse* on SWE-smith (3.80) than the 8B—it needs more code context to leverage its superior reasoning. Extraction model capability and input preprocessing must be co-tuned.

---

## 9  Production Considerations

Deploying HiveState in production involves tuning several dimensions to workload characteristics:

- **Extraction model choice.** The extraction model is fully configurable. Larger models (70B+) deliver highest quality but require cloud inference; smaller models (8B) run locally with acceptable quality on chat workloads. Model choice should match the complexity of your dominant workflow type.

- **Threshold tuning.** The 400-token threshold used here provides cross-domain consistency for evaluation. Production deployments benefit from per-workload tuning: higher thresholds for agent workflows (where context accumulates in large steps), lower for latency-sensitive chat where even moderate context growth impacts cost.

- **CCR effectiveness depends on workload patterns.** CCR activates when the same user/project generates multiple requests referencing similar entities. High-traffic platforms with session continuity see frequent CCR hits; stateless batch workloads see minimal activation. Monitor CCR hit rate to validate value.

- **Routing policies are deployment-specific.** HiveRoute's difficulty→model mapping depends on which models are available and their cost/quality trade-offs. The default mapping serves as a starting point; teams should calibrate based on their model portfolio and quality requirements.

---

## 10  Conclusion

HiveState achieves **near-perfect workflow continuation** (WCS 4.70/5.0) while reducing context by **47–71%** across four workflow categories. The central insight: **HiveState preserves executable workflow state, not history.** By extracting intent, identifiers, action outcomes, and pending requirements, it eliminates redundant context while retaining what drives downstream decisions.

Combined with CCR for episodic retrieval and HiveRoute for intelligent routing, HiveState delivers $982–$15,887/month savings per million requests—with every domain net-positive after extraction costs. As AI systems increasingly rely on long-running workflows, domain-agnostic context compression becomes essential infrastructure.

---

## Reproducibility

```bash
cd ubiquum-ai-gateway && python3 test/hivestate_multidomain_bench.py \
  --domain all -n 100 --api-key $UBIQUUM_API_KEY \
  --delay 0.1 --judge gpt-oss:120b --judge-n 50 \
  --output test/results/multidomain_bench_results.json
```

Gateway: Ubiquum Gateway v1.x (Go) · Extraction: Llama 3.3 70B via Groq · Judge: GPT-OSS 120B · Seed: 42 · Timeout: 20s

---

## References

[1] Liu, N.F., et al. "Lost in the Middle: How Language Models Use Long Contexts." *TACL*, 12:157–173, 2024.

[2] Budzianowski, P., et al. "MultiWOZ — A Large-Scale Multi-Domain Wizard-of-Oz Dataset." *EMNLP*, 2018.

[3] Zhang, Y., et al. "DIALOGPT: Large-Scale Generative Pre-training for Conversational Response Generation." *ACL System Demos*, 2020.

[4] Yao, S., et al. "Tau-Bench: A Benchmark for Tool-Agent-User Interaction." *arXiv:2406.12045*, 2024.

[5] Packer, C., et al. "MemGPT: Towards LLMs as Operating Systems." *arXiv:2310.08560*, 2023.

[6] Shinn, N., et al. "Reflexion: Language Agents with Verbal Reinforcement Learning." *NeurIPS*, 2023.

[7] Yang, J., et al. "SWE-smith: Scaling Data for Software Engineering Agents." *arXiv:2504.21798*, 2025.

[8] Zeng, A., et al. "AgentTuning: Enabling Generalized Agent Abilities for LLMs." *arXiv:2310.12823*, 2023.

---

## Appendix A — How HiveState Works

We illustrate HiveState with a concrete example drawn from the Tau-Bench evaluation (retail agent handling an order exchange). After 20 messages the accumulated context reaches 7,000 tokens—system prompt, tool calls, API responses, and user messages. HiveState activates when token count exceeds the configured τ threshold.

The system detects the conversation profile (*tool-agent*), zones the 18 historical messages by importance, preserves the most recent step, and extracts a structured state of just 220 tokens:

```json
{ "intent": "exchange_items",
  "active_constraints": {
    "identifiers": { "order_id": "ORD-9842", "user": "sophia_h" },
    "values": { "item": "Blue Wool Sweater M", "exchange_for": "L" },
    "actions_taken": [
      {"action": "looked up order", "result": "found, delivered"},
      {"action": "verified exchange policy", "result": "eligible"}
    ],
    "progress": { "done": ["verify_order","check_policy"],
        "current": "process_exchange", "next": ["confirm"] }
  },
  "conversation_status": "in_progress",
  "difficulty": "standard", "reasoning_effort": "medium" }
```

*Listing A.1: Extracted state (220 tokens). The full 7,000-token history is replaced by this structured representation for all subsequent requests.*

On the next turn, the downstream model receives only: system prompt + state JSON + last user message—a **96.9% reduction** in context size. The agent completes the task with a perfect score (WCS: 5/5), demonstrating zero information loss.

| | Before HiveState | After HiveState |
|-|:----------------:|:---------------:|
| Context size | 7,000 tokens | 220 tokens |
| Includes | Full message history | Structured state + last message |
| Reduction | — | 96.9% |
| Task score (WCS) | 5/5 | 5/5 |

*Table A.1: Context before and after compression for the exchange task.*

The state preserves **what matters** for continuation: intent, identifiers, action outcomes, and next steps. Everything else—verbose API responses, repeated schema elements, superseded intermediate results—is eliminated.
