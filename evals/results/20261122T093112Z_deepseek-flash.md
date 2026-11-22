# Eval: deepseek / deepseek-flash

Run 20261122T093112Z · 30 questions · judge deepseek/deepseek-flash

**Accuracy: 29/30 (97%)**

| Kind | Passed | Accuracy |
|---|---|---|
| number | 15/15 | 100% |
| exact | 4/4 | 100% |
| list | 2/2 | 100% |
| narrative | 8/9 | 89% |

| Metric | Value |
|---|---|
| Total cost | $0.0606 |
| Cost / question | $0.0020 |
| Avg tool steps | 4.1 |
| Tool errors (self-corrections) | 4 across 3 questions |
| Hit step budget | 3 |
| Latency p50 / p95 | 11.1s / 33.9s |

## Failures

- **n01** (narrative) Why did West revenue drop in Q3 2025? — It does not clearly identify Trail Pack as the main cause and misstates it as the single biggest revenue-drop slice; in the provided facts Rain Shell's revenue decline is slightly larger, though pricing is correctly not blamed.
