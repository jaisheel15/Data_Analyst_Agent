# Data Analyst Agent

Upload a CSV or Parquet file, ask a question in plain English, and watch an AI agent work it out. It inspects the data, writes and runs SQL and Python in a locked-down sandbox, fixes its own errors, draws charts, and explains the answer.

![Demo: asking why West revenue dropped, then for a chart](docs/demo.gif)

[Full-quality demo video](docs/demo.mp4) (40 s, two questions on the sample retail data, played at 2x).

> **Eval score:** 29/30 on `deepseek-flash` at about $0.002 per question (see [Evals](#evals)).

## Run it

You need Docker.

```bash
git clone <this repo> && cd data-analyst-agent
cp .env.example .env          # then set DEEPSEEK_API_KEY (or ANTHROPIC_API_KEY + LLM_PROVIDER=anthropic)
docker compose up --build     # or: make up
```

Open <http://localhost:3000>, click **Retail sales**, and ask *"Why did West revenue drop in Q3 2025?"*

The defaults keep spending low: a cheap model (`deepseek-flash`), and a **$0.50/day** chat cap that the Go API enforces from real token counts. Change both in `.env`.

## Architecture

```mermaid
flowchart LR
    B[Browser<br/>Nuxt SPA] -- "HTTP + SSE" --> C[Caddy]
    C -- "/api" --> G[Go API]
    G -- "POST /runs<br/>(NDJSON stream)" --> P[Python agent<br/>FastAPI + LangGraph]
    P -- "POST /internal/tools/*" --> G
    P -- "LLM calls" --> L[(Anthropic /<br/>DeepSeek)]
    G --> PG[(Postgres)]
    G --> D[(DuckDB file<br/>per session)]
    G -- "socket proxy" --> S[Sandbox container<br/>per session]
    S -. "read-only" .-> D
```

**The split:** Go owns everything stateful or dangerous: uploads, DuckDB, Postgres, the sandbox, limits and costs. Python owns only the reasoning: the prompt, the LangGraph loop, and the LLM calls. The Python service is stateless, never touches the database or Docker, and runs tools by calling back into Go. Swapping LangGraph for a hand-written loop (planned for v2) only replaces the Python service.

**One question, end to end:**

1. The browser posts the question. Go checks the daily budget, whether a sandbox is free, and the model's price. Postgres's partial unique index allows one running run per session.
2. Go sends Python the question, the table schemas with sample rows, the last few turns of history (with compact tool steps), the model and the limits.
3. LangGraph loops: the model calls `describe_dataset`, `run_sql` or `run_python`; Python forwards each call to Go; Go runs it in the session's sandbox and returns text the model can read. Errors come back as results, so the agent can fix its code and retry.
4. Every step streams back as NDJSON from Python to Go, then as SSE from Go to the browser, and the answer arrives token by token as the model writes it. If a model writes a sentence like "Let me check" before calling a tool, that text is withdrawn (`answer_reset`) so it never shows up as the answer. Go saves each tool call, keeps the charts, and computes cost from the token counts.
5. A tool-call budget (10 by default) stops runaway loops. When it runs out, the agent must answer with what it has and say so.

The Go⇄Python API is defined once in [`contracts/internal-api.schema.json`](contracts/internal-api.schema.json), and both services' tests validate their payloads against it.

## Sandbox

Agent-written code runs in a container per session, created by Go through a Docker socket proxy:

- no network, 1 CPU, 1 GB memory (no swap), a PID limit, and a 30-second timeout per call that kills the whole process group
- read-only root filesystem, non-root user, all capabilities dropped, `no-new-privileges`
- the session's DuckDB file mounted read-only; writable `/out` (charts) and `/scratch` (intermediate results). Go accepts only regular PNG files from `/out`, so agent code can't plant a symlink to a secret file and have it served as a "chart"
- an explicit environment allowlist, so no secrets reach agent code
- plain Docker (`runc`) by default. `SANDBOX_RUNTIME=runsc` switches to gVisor if you install it; nothing else changes

Each call is a fresh process, so a crash, timeout, or fork bomb can't leave state behind: leftover processes are killed after every call, and a tiny PID 1 reaper stops zombies from piling up. If code hangs, the container is thrown away and recreated. At most 3 sandboxes run at once; when all slots are taken, the least recently used idle one is evicted, and a new session is only turned away when all 3 are running code at that moment. These behaviours are covered by integration tests against real Docker (`api/internal/sandbox/manager_test.go`).

**Honest limits:** the socket proxy restricts which Docker endpoints Go can reach, but "create container" is still a powerful permission, so a compromised API process remains serious. The container limits above make a breakout hard but not impossible: a Linux kernel bug could still let agent code escape plain Docker. gVisor (optional, see above) is the extra layer for that case, so don't run this against untrusted users on a machine you care about without it. On Linux, the host can reach containers on a bridge network by IP. The agent service publishes no port, and every internal call needs a shared token.

## Evals

`evals/questions.yaml` holds 30 questions over 3 datasets (retail sales, employees, support tickets): 21 with a number, name or list as the answer, and 9 open-ended ones graded by an LLM judge. **No expected answer is typed by hand.** Every question carries the SQL that computes its truth from the data, and `make eval-truth` writes those to `evals/ground_truth.json` (CI checks the file is up to date).

```bash
make eval PROVIDER=deepseek MODEL=deepseek-flash      # needs the stack running
```

Each question runs in a fresh session through the real API, so the whole pipeline is measured. The run prints an estimated cost first and stops if it reaches `EVAL_RUN_BUDGET_USD`. | Model | Accuracy | Numbers / names / lists | Open-ended | Cost per question | Latency p50 / p95 |
|---|---|---|---|---|---|
| `deepseek-flash` | 29/30 (97%) | 21/21 | 8/9 | $0.002 | 11 s / 34 s |

The open-ended answers are graded by the same model, which is a limit of this score. Results land in `evals/results/` as JSON plus a markdown summary: accuracy by question type, steps, self-corrections, cost, and p50/p95 latency.

## Tracing

Off by default. `make up-tracing` also starts [Arize Phoenix](https://github.com/Arize-ai/phoenix) at <http://localhost:6006>, and every run shows up as a trace: each model call with its prompt, tokens (including cached ones) and latency, and each graph step. Spans carry the same `run_id` and `session_id` as the Postgres rows, so a slow or wrong answer can be matched to its trace. It uses OpenTelemetry (OpenInference's LangChain instrumentation), so any OTLP backend works: set `TRACING_OTLP_ENDPOINT` instead.

Traces contain prompts and sample rows. Phoenix runs locally with its data in a Docker volume, bound to 127.0.0.1.

## Development

```bash
make test          # sandbox runner + agent + Go (Go tests need Docker: Postgres via testcontainers, real sandboxes)
make lint
make typecheck     # Nuxt
cd web && npm run dev   # UI on :3000 against the Go API on :8080 (from `docker compose up`)
```

| Path | What |
|---|---|
| `api/` | Go service: public API, internal tool API, sandbox manager, DuckDB loading, costs |
| `agent/` | Python agent service: LangGraph graph, prompts, provider factory |
| `sandbox/` | Sandbox image and `runner.py` (executes one SQL or Python call) |
| `web/` | Nuxt UI |
| `contracts/` | Internal API schema |
| `config/pricing.yaml` | Per-model prices used for the spend cap |
| `deploy/` | Production compose override and Caddyfile (deploy-ready; v1 runs locally) |
| `docs/specs/` | Design doc |
| `scripts/record-demo.mjs` | Re-records the demo GIF and video against a running stack |

## Data and privacy

Column names, sample rows, and query results are sent to the model provider you configure (DeepSeek's API is hosted in China). Don't upload sensitive data. Sessions, their files, and their sandboxes are deleted after 24 hours.
