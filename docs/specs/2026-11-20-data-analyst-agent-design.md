# Data Analyst Agent — Design (v1)

Date: 2026-11-20
Status: draft, awaiting final approval

## 1. Intent and why

A portfolio/resume project for someone applying to **both AI engineer and backend engineer** roles. A user uploads a data file and asks questions in plain English ("why did revenue drop in Q3?"). An agent inspects the data, writes and runs SQL and Python in a sandbox, recovers from its own errors, produces charts, and answers with a written explanation.

The project has to give an interviewer something to probe on both sides:
- **AI side:** a tool-using agent (LangGraph), self-correction, a measured eval score, and a **Claude vs DeepSeek comparison** on accuracy, cost, and latency.
- **Backend side:** a Go API, a two-service architecture with a defined internal contract, hardened execution of untrusted (LLM-generated) code, persistence, streaming, and CI.

**v1 runs locally only** (user's decision). Hosting may come later, so the code stays deploy-ready. Instead of a live link, the portfolio shows a **one-command local setup** and a **demo video**. The author has 15–20+ hours/week and is comfortable with Go, Python, and Nuxt. Target: about 3 weeks.

## 2. Goals (v1)

1. Upload CSV, XLSX, or Parquet files (one or more per session). Each is loaded into a per-session **DuckDB** database as a table.
2. Chat in natural language about the uploaded data, with follow-ups that use conversation history.
3. The agent uses three tools: `describe_dataset`, `run_sql` (DuckDB), and `run_python` (pandas/matplotlib, with the DuckDB file available).
4. **Self-correction:** when a tool call errors, the agent sees the error and retries, within a step budget.
5. Charts are produced by Python, returned as images, and shown inline in the chat.
6. Agent steps (tool calls, code, results) **stream live** to the UI.
7. Sessions, messages, datasets, and tool-call traces are **persisted** in Postgres.
8. **A swappable model provider:** Anthropic (Claude) or DeepSeek, chosen by config.
9. An **eval harness** produces a reproducible score, plus a Claude vs DeepSeek comparison table.
10. **Low cost:** spending caps, a cheap default model, and a fake model in tests.
11. **Portfolio packaging:** `git clone` → `cp .env.example .env` → `docker compose up` works first time; a 2–3 minute demo video and a GIF in the README; an architecture diagram and eval results in the README.
12. **CI** on GitHub Actions. The repo is **public on GitHub from day one**.

## 3. Non-goals (v1)

- Public hosting, and features that only matter for public visitors: per-IP rate limits, replayed example conversations, a "busy" screen.
- Connecting to external databases: planned for v2.
- User accounts or auth. Sessions are anonymous.
- A hand-written agent loop. v1 uses LangGraph; a custom loop is planned for v2.
- gRPC, job queues, background runs that survive a disconnect, or more than two backend services.
- Continuous deployment.
- Fine-tuning, or memory across sessions.

## 4. Anti-goals (failure even if it "works")

- **LLM-generated code running outside the sandbox.** Any path that executes model code in the Go or Python service process is a bug.
- **The agent service reachable from outside the internal Docker network.**
- **Surprise bills.** Spending must stay within the caps in §7.8.
- **Secrets in the public repo.** Only `.env.example` is committed.
- **A black box.** The tools, sandbox, and service contract are our own code, and the author can explain every part.
- **Never finishing.** Get the core path working end to end before polishing anything.

## 5. Constraints

- **Backend: Go. Agent: Python + LangGraph, as a separate service. Frontend: Nuxt** (all user's choices).
- Runs on the author's Linux machine (CachyOS) with Docker. gVisor is available there.
- Go's DuckDB driver (`github.com/duckdb/duckdb-go` / `marcboeker/go-duckdb`) needs cgo, which affects the Go Docker build.
- A very small LLM budget (see §7.8).

## 6. Approaches considered

### 6.1 Overall architecture

| # | Approach | Verdict |
|---|---|---|
| 1 | Streamlit app, agent in-process | Rejected: weak backend signal, unsafe execution |
| 2 | Real backend + hand-written agent loop | Deferred to v2 (the loop part) |
| 3 | LangGraph + hosted sandbox + Streamlit | Rejected: frameworks hide the engineering |
| **Chosen** | **Real backend, with LangGraph orchestrating the agent in v1** | **Picked by the user** |

### 6.2 Language split

| # | Approach | Trade-off | Verdict |
|---|---|---|---|
| 1 | All Go, Eino as the agent framework | One language, but a less-known framework | Not chosen |
| **2** | **Go API + Python agent service (LangGraph)** | Keeps LangGraph and gives a system-design story, but two services to build | **Picked by the user** |
| 3 | All Go, hand-written loop now | Drops the framework-first plan | Not chosen |

### 6.3 Other decisions (confirmed by the user in review)

| Decision | Chosen | Alternative rejected |
|---|---|---|
| Who runs tools | Go; Python calls back to Go | Python drives Docker and DuckDB itself |
| Internal transport | HTTP + streamed NDJSON | gRPC (later, v3) |
| Sandbox isolation | gVisor (`runsc`) + Docker socket proxy | Plain Docker only |
| Concurrency | 1 run per session, at most 3 sandboxes | Job queue |
| Browser disconnect | Cancel the run, mark it `cancelled` | Runs that keep going in the background |
| Hosting | Local only for v1, deploy-ready | Hetzner VM + domain |
| Model | Swappable Anthropic/DeepSeek; cheap model by default | A single fixed provider |

## 7. Design

### 7.1 Architecture

```
Nuxt (Vue 3 + TS)
      │  HTTP + SSE
      ▼
Go API service  ─────────────────────────────┐
  • sessions, uploads, persistence           │
  • DuckDB loading, describe_dataset         │
  • sandbox manager (via socket proxy)       │
  • concurrency limits, spend cap            │
  • relays agent events to the browser       │
      │  POST /runs  (internal, streams NDJSON events back)
      ▼                                      │
Python agent service (FastAPI + LangGraph)   │
  • stateless: gets history + schema per run │
  • LLM calls (Anthropic or DeepSeek)        │
  • tools are thin HTTP clients ─────────────┘
        POST /internal/tools/{name}  (back to Go)
                                             │
                                             ▼
                docker-socket-proxy ──▶ container per session
                                        (gVisor, no network, CPU/mem/time limits)

Postgres ◀── Go only        File storage (uploads, session.duckdb, charts) ◀── Go only
```

**The service boundary:** Go owns **everything stateful and everything dangerous**: data, files, Postgres, the sandbox, and limits. Python owns **only the reasoning**: the prompt, the LangGraph loop, and the LLM calls. Python never touches Postgres, files, or Docker. The v2 swap to a hand-written loop only replaces the Python service.

### 7.2 Internal contract (Go ⇄ Python)

Plain HTTP + JSON on an internal-only Docker network. A shared secret header (`X-Internal-Token`) on every call.

**Go → Python: `POST /runs`.** Body: `run_id`, `session_id`, `question`, `schema` (tables, columns, sample rows), `history` (§7.3), `limits` (step budget, max tokens). The response is **streamed NDJSON**, one event per line:
`step_start`, `tool_call` (tool, input), `tool_result` (summary, error, chart_ids), `answer_delta`, `done` (final answer, usage: provider, model, input/output/cached tokens, steps), `error`.

**Python → Go: `POST /internal/tools/{describe_dataset|run_sql|run_python}`.** Body: `run_id`, `session_id`, and the tool input. Response: the tool output (already truncated for the LLM), `error` text if it failed, and `chart_ids`. Go records each call in `tool_calls` as it happens.

**Cancellation:** if the browser disconnects, Go cancels the request to `/runs`, Python stops the graph, and Go marks the run `cancelled` after saving the steps that finished.

The contract is defined in one place (`contracts/`, OpenAPI or JSON schema). Both services test against it.

### 7.3 Agent (Python)

- LangGraph ReAct-style graph: `agent` node ⇄ `tools` node, looping until the model gives a final answer or the **step budget** is used up (default 10 tool calls). When the budget runs out, the agent gives a best-effort answer and says it hit the limit.
- System prompt: the table schemas plus sample rows, the tool descriptions, how sandbox state works (§7.5), and instructions to check results, retry on errors, and cite computed numbers.
- **Model provider:** set with `LLM_PROVIDER=anthropic|deepseek` and `LLM_MODEL=...`, using LangChain's `init_chat_model` (`langchain-anthropic`, plus `langchain-deepseek` or the OpenAI-compatible client). Prompt changes for one provider live in `prompts/<provider>/` only if needed. Test against both providers from week 1.
- **Default model is a cheap one** (Claude Haiku or DeepSeek). Sonnet is used only for the demo video and the published eval scores, switched in `.env`.
- **Prompt caching** is on for the system prompt and schema (Anthropic cache control), to cut costs within a run.
- **Conversation history:** Go stores it and sends it with each run, so Python keeps no state and LangGraph's checkpointer is not used. Go sends the last 3 turns. Each has the question, the final answer, and a compact tool history: every tool call's code plus about 20 rows or 1 KB of its result. Older turns carry only the question and answer.

### 7.4 Tools (executed by Go)

| Tool | Input | Output | Runs where |
|---|---|---|---|
| `describe_dataset` | optional table name | tables, columns + types, row counts, sample rows, null counts | Go, read-only DuckDB connection |
| `run_sql` | SQL string | first N rows (default 50) + total row count, or the error text | Sandbox |
| `run_python` | code string | stdout/stderr (truncated), produced chart files, or the traceback | Sandbox |

- Large outputs are truncated before going to the LLM, with a note saying how much was cut.
- Tool errors come back as normal results, not HTTP errors. This is how self-correction works. Only a transport failure between the services comes back as an HTTP error.

### 7.5 Sandbox (managed by Go)

- One container per active session, reused within the session, and killed after 15 minutes idle.
- **Go never mounts the raw Docker socket.** It talks to a **docker-socket-proxy** (for example `tecnativa/docker-socket-proxy`) that only allows the container create, start, exec, inspect, and kill/remove endpoints.
- **Runtime:** `SANDBOX_RUNTIME=runsc` (gVisor) by default. At startup, Go checks that the runtime is available. If gVisor isn't installed, it falls back to `runc` and logs a warning. The README documents how to install gVisor.
- Image: Python slim plus pandas, numpy, duckdb, matplotlib, seaborn, scipy, and openpyxl/pyarrow. No pip install at runtime.
- Limits: `--network none`, 1 CPU, 1 GB memory, pids limit, read-only root filesystem, non-root user, 30 s wall-clock timeout per call (enforced by killing the process).
- Mounts: session data directory **read-only** (including `session.duckdb`); writable `/out` (charts) and `/scratch` (intermediate results).
- Execution: Go runs `runner.py` through `docker exec`. It returns JSON (stdout, stderr, error, files in `/out`). `run_sql` uses the same runner, with the duckdb Python package inside the container.
- **State between calls:** each call is a **fresh process**, so Python variables don't survive between calls. Each call opens `session.duckdb` read-only and closes it, so Go can write tables for new uploads between calls. The agent saves intermediate results as Parquet files in `/scratch`.
- Charts: PNGs written to `/out` are copied to storage, recorded in the database, and served via `GET /charts/{id}`.

### 7.6 Public API (Go)

- Router: chi or the standard library's `net/http`. Postgres access through `pgx` + `sqlc`. Migrations with `golang-migrate` or `goose`.
- `POST /sessions` creates a session.
- `POST /sessions/{id}/datasets` uploads a file (≤ 25 MB; CSV/XLSX/Parquet). Go loads it into DuckDB with `read_csv_auto`, the excel extension, or `read_parquet`, and returns the schema.
- `GET /sessions/{id}` returns datasets and message history.
- `POST /sessions/{id}/messages` returns an **SSE stream**. Go builds the history, calls Python `/runs`, persists each event, and relays it to the browser.
  - It returns **409** if this session already has a run in progress.
  - It returns **503** if 3 sandboxes are already active.
  - It returns **429** with a clear message once the daily spend cap is reached.
- `GET /charts/{id}`, `GET /health` (which also pings the agent service).

### 7.7 Persistence (Postgres, Go only)

- `sessions` (id, created_at, last_active_at)
- `datasets` (id, session_id, table_name, original_filename, rows, columns_json)
- `messages` (id, session_id, role, content, created_at)
- `agent_runs` (id, message_id, provider, model, steps, input_tokens, output_tokens, cached_tokens, cost_usd, latency_ms, status: running|done|error|cancelled|budget_exhausted)
- `tool_calls` (id, run_id, seq, tool, input, output_truncated, error, duration_ms)
- `charts` (id, run_id, path)

A background goroutine deletes sessions, files, and containers after 24 h.

### 7.8 Cost limits, data notice, and running locally

- **Prices:** set per model in `config/pricing.yaml`, filled in from the providers' current published prices at build time. Go computes `cost_usd` from the token counts in the `done` event.
- **Chat cap:** `DAILY_CHAT_BUDGET_USD=0.50`, the sum of `agent_runs.cost_usd` for the current UTC day. Once it's reached, new questions get a 429 and the UI shows "daily budget reached."
- **Eval budget:** `EVAL_RUN_BUDGET_USD=1.00`. Evals don't count against the chat cap. `make eval` prints an estimated cost (questions × average cost per question for that model, from earlier runs) and asks for confirmation. It stops if a run goes over the budget.
- **Tests** use a fake LLM and cost $0.
- **Data notice:** a one-line notice next to the upload box says not to upload sensitive data, because column names and sample rows are sent to the model provider (DeepSeek's API is hosted in China). The README states this, and that data is deleted after 24 h.
- **Local run:** `docker compose up` starts Go API, Python agent, Postgres, Nuxt, the socket proxy, and the sandbox image build. The agent service is on the internal network only.
- **Deploy-ready, not deployed:** `docker-compose.prod.yml` and a `Caddyfile` live in the repo. CI validates them, but nothing is hosted in v1.
- **Tracing:** Langfuse or LangSmith in the Python service, with `run_id` attached. Structured JSON logs with `run_id` in both services.

### 7.9 Frontend (Nuxt + TypeScript)

- Runs entirely in the browser (SPA mode, no server-side rendering). It talks to the Go API.
- SSE through `fetch` + a stream reader, because native `EventSource` can't POST.
- Left: upload area with the data notice, dataset list, schema/preview table, sample-dataset buttons.
- Right: chat. Each turn shows collapsible **steps** (code, a results table, errors and retries), then charts inline, then the final answer.
- Clear messages for the 409, 503, and 429 responses.
- A plain look is fine. The UI library is the builder's call.

### 7.10 Evals

- **The builder drafts and the user reviews:** 3 public datasets and about 30 questions (about 20 with numeric or exact answers, about 10 narrative). **Ground truth is computed by a script from the data** and checked into `evals/`, never typed by hand. The user reviews the set once.
- Scoring: numbers within a tolerance, exact match for categories and lists, an LLM judge with a rubric for narrative answers.
- `make eval PROVIDER=... MODEL=...` runs against the full stack through the Go API. It writes the results to `evals/results/<timestamp>.json` plus a markdown summary (accuracy by question type, average steps, retries, tokens, cost, p50/p95 latency).
- **A provider comparison:** `make eval-compare` runs the suite on Claude and on DeepSeek, within the eval budget, and produces the comparison table that goes in the README.

### 7.11 Testing and CI

- **Go:** unit tests (upload loading, history building, truncation, cost calculation, cap logic, 409/503 limits); sandbox integration tests (timeout, network blocked, memory limit, read-only mounts, runtime fallback); SSE relay tests with a fake agent service.
- **Python:** graph tests with a fake LLM and a fake tool endpoint (event order, step budget, error-retry path, provider factory).
- **Contract tests:** both services validate against `contracts/`.
- **Security tests:** code that tries network access, writes outside `/out` and `/scratch`, fork-bombs, or reads environment variables or secrets must fail safely. The socket proxy rejects endpoints that aren't allowed. The agent service is unreachable from the host network.
- **GitHub Actions on every push:** Go test + `golangci-lint`, Python test + `ruff`, contract tests, Nuxt typecheck + build, sandbox security tests (with `runc` in CI), and validation of the prod compose file and Caddyfile. Evals are never in automatic CI. There's only an optional workflow you start manually.

### 7.12 Repo layout (public monorepo)

```
data-analyst-agent/
  api/        Go: cmd/api, internal/{http,sessions,datasets,tools,sandbox,agentclient,cost,db}
  agent/      Python: app/{main.py,graph.py,tools.py,llm.py,prompts/}  tests/
  sandbox/    Dockerfile  runner.py
  web/        Nuxt app
  contracts/  internal API schema
  config/     pricing.yaml
  evals/      datasets/ questions.yaml  ground_truth/  run.py  results/
  deploy/     docker-compose.prod.yml  Caddyfile
  docs/specs/
  .github/workflows/
  docker-compose.yml  .env.example  Makefile  README.md
```

### 7.13 Build order and cut list

1. **Core path:** CSV upload into DuckDB (Go), the internal contract, the Python agent with all three tools and the provider swap, the hardened sandbox, SSE relay, Postgres, spend cap, a basic Nuxt chat, and CI.
2. **Package:** one-command setup, sample datasets, the README with the architecture diagram, the demo video and GIF. **This is the first "done" milestone.**
3. **Evals:** the harness, about 30 questions, the Claude vs DeepSeek comparison, and the results in the README.
4. **Polish:** XLSX/Parquet, collapsible steps UI, tracing.

If time runs short, cut from the bottom up: tracing, then steps-UI polish, then XLSX/Parquet, then LLM-judged narrative questions. For v1 to count as done, it needs the one-command setup, the demo video, and at least a numeric eval score.

## 8. Left to the builder

Prompt wording; UI layout and UI library; chi vs `net/http`; migration tool; which cheap default model; eval datasets and draft questions (user reviews); Langfuse vs LangSmith; container warm starts; idle and cleanup timings.

## 9. Later versions (out of scope, but the design shouldn't block them)

- **Hosting:** a small VM (for example a Hetzner 4 GB) + domain + Caddy, adding back per-IP rate limits and the cached-example fallback.
- **v2:** a hand-written agent loop behind the same `/runs` contract, in Python or moved into Go, with eval scores compared before and after. Database connectors with read-only roles.
- **v3:** a job queue so runs survive disconnects; gRPC between services; auth and saved workspaces; a planner/critic multi-agent mode.
