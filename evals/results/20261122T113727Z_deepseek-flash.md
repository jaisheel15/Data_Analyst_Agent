# Eval: deepseek / deepseek-flash

Run 20261122T113727Z · 30 questions · judge deepseek/deepseek-flash

**Accuracy: 28/30 (93%)**

| Kind | Passed | Accuracy |
|---|---|---|
| number | 15/15 | 100% |
| exact | 4/4 | 100% |
| list | 2/2 | 100% |
| narrative | 7/9 | 78% |

| Metric | Value |
|---|---|
| Total cost | $0.0634 |
| Cost / question | $0.0021 |
| Avg tool steps | 4.2 |
| Tool errors (self-corrections) | 7 across 7 questions |
| Hit step budget | 2 |
| Latency p50 / p95 | 10.0s / 27.4s |

## Failures

- **n02** (narrative) Compare the four regions and tell me which are strongest and weakest. — The revenue table is ordered and rounded correctly, but the answer identifies South (or East/South) as weakest rather than West, which is lowest in the provided facts.
- **n04** (narrative) What seems to be related to employees leaving the company? — The answer correctly identifies Engineering as highest and Marketing as lowest attrition, but it overclaims by calling department a clear 'driver' and salary a proxy/not independent despite lacking multivariate or causal evidence.
