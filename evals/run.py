"""Run the eval suite against the live stack and write a results file + markdown summary.

  uv run --project evals python evals/run.py --provider deepseek --model deepseek-flash
  (normally via `make eval PROVIDER=... MODEL=...`)

Each question gets a fresh session (no history leaking between questions) and goes
through the real Go API with the eval token, so the whole pipeline is measured.
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import httpx
import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
from judge import judge  # noqa: E402
from scoring import score_exact, score_list, score_number  # noqa: E402

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent


def load_env() -> None:
    env = ROOT / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            if "=" in line and not line.lstrip().startswith("#"):
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip())


def ask(client: httpx.Client, api: str, token: str, dataset: str, question: str, provider: str, model: str) -> dict:
    """One question in a fresh session. Returns what the agent did."""
    sid = client.post(f"{api}/sessions").json()["id"]
    csv = HERE / "datasets" / f"{dataset}.csv"
    up = client.post(f"{api}/sessions/{sid}/datasets", files={"file": (csv.name, csv.read_bytes(), "text/csv")})
    up.raise_for_status()
    out = {"answer": "", "status": "error", "cost_usd": 0.0, "steps": 0, "tool_errors": 0, "charts": 0, "error": ""}
    start = time.monotonic()
    headers = {"X-Eval-Token": token}
    body = {"question": question, "provider": provider, "model": model}
    with client.stream("POST", f"{api}/sessions/{sid}/messages", json=body, headers=headers, timeout=300) as r:
        if r.status_code != 200:
            out["error"] = f"HTTP {r.status_code}: {r.read().decode()[:200]}"
            return out | {"latency_s": time.monotonic() - start}
        event = None
        for line in r.iter_lines():
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: ") and event:
                d = json.loads(line[6:])
                if event == "tool_result" and d.get("error"):
                    out["tool_errors"] += 1
                elif event == "chart":
                    out["charts"] += 1
                elif event == "done":
                    out.update(answer=d["answer"], status=d["status"], cost_usd=d["cost_usd"], steps=d["steps"])
                elif event == "error":
                    out["error"] = d["error"]
                event = None
    return out | {"latency_s": time.monotonic() - start}


def grade(q: dict, truth: dict, res: dict, judge_cfg: tuple[str, str]) -> tuple[bool, str]:
    if res["status"] == "error" or not res["answer"]:
        return False, res["error"] or "no answer"
    kind, answer = q["kind"], res["answer"]
    if kind == "number":
        ok = score_number(answer, truth["value"], q.get("tol", 0.01), q.get("abs", False))
        return ok, f"expected {truth['value']:.4g}"
    if kind == "exact":
        return score_exact(answer, truth["value"]), f"expected {truth['value']}"
    if kind == "list":
        return score_list(answer, truth["values"], q.get("ordered", False)), f"expected {truth['values']}"
    if q.get("expects_chart") and res["charts"] == 0:
        return False, "no chart produced"
    ok, reason = judge(*judge_cfg, q["question"], answer, truth["facts"], q["rubric"])
    return ok, reason


def pct(values: list[float], p: float) -> float:
    s = sorted(values)
    return s[min(len(s) - 1, round(p * (len(s) - 1)))] if s else 0.0


def summarise(meta: dict, rows: list[dict]) -> str:
    n = len(rows)
    passed = sum(r["passed"] for r in rows)
    lat = [r["latency_s"] for r in rows]
    lines = [f"# Eval: {meta['provider']} / {meta['model']}", "",
             f"Run {meta['timestamp']} · {n} questions · judge {meta['judge']}", "",
             f"**Accuracy: {passed}/{n} ({100 * passed / n:.0f}%)**", "",
             "| Kind | Passed | Accuracy |", "|---|---|---|"]
    for kind in ("number", "exact", "list", "narrative"):
        k = [r for r in rows if r["kind"] == kind]
        if k:
            p = sum(r["passed"] for r in k)
            lines.append(f"| {kind} | {p}/{len(k)} | {100 * p / len(k):.0f}% |")
    total = sum(r["cost_usd"] for r in rows)
    lines += ["", "| Metric | Value |", "|---|---|",
              f"| Total cost | ${total:.4f} |", f"| Cost / question | ${total / n:.4f} |",
              f"| Avg tool steps | {statistics.mean(r['steps'] for r in rows):.1f} |",
              f"| Tool errors (self-corrections) | {sum(r['tool_errors'] for r in rows)} across {sum(r['tool_errors'] > 0 for r in rows)} questions |",
              f"| Hit step budget | {sum(r['status'] == 'budget_exhausted' for r in rows)} |",
              f"| Latency p50 / p95 | {pct(lat, 0.5):.1f}s / {pct(lat, 0.95):.1f}s |"]
    failed = [r for r in rows if not r["passed"]]
    if failed:
        lines += ["", "## Failures", ""]
        for r in failed:
            lines.append(f"- **{r['id']}** ({r['kind']}) {r['question']} — {r['note']}")
    return "\n".join(lines) + "\n"


def rescore(path: Path) -> int:
    """Regrade saved answers after a scorer fix. Narrative verdicts keep their stored judge result."""
    saved = json.loads(path.read_text())
    questions = {q["id"]: q for q in yaml.safe_load((HERE / "questions.yaml").read_text())["questions"]}
    truth = json.loads((HERE / "ground_truth.json").read_text())
    for row in saved["rows"]:
        q = questions[row["id"]]
        if q["kind"] != "narrative":
            row["passed"], row["note"] = grade(q, truth[row["id"]], row, ("", ""))
    path.write_text(json.dumps(saved, indent=1))
    path.with_suffix(".md").write_text(summarise(saved["meta"], saved["rows"]))
    print(path.with_suffix(".md").read_text())
    return 0


def estimate_per_question(provider: str, model: str, pricing: dict) -> float:
    """Average of earlier runs of this model if there are any, otherwise a rough guess from prices."""
    past = []
    for f in (HERE / "results").glob("*.json"):
        d = json.loads(f.read_text())
        if d["meta"]["model"] == model and d["rows"]:
            past.append(sum(r["cost_usd"] for r in d["rows"]) / len(d["rows"]))
    if past:
        return statistics.mean(past)
    p = pricing[model]  # a guess: ~15k input and ~4k output tokens per question
    return (15_000 * p["input_per_mtok"] + 4_000 * p["output_per_mtok"]) / 1e6


def main() -> int:
    load_env()
    ap = argparse.ArgumentParser()
    ap.add_argument("--provider", choices=["anthropic", "deepseek"])
    ap.add_argument("--model")
    ap.add_argument("--api", default="http://localhost:8080/api")
    ap.add_argument("--only", help="comma-separated question ids")
    ap.add_argument("--judge-provider", default="deepseek")
    ap.add_argument("--judge-model", default="deepseek-flash")
    ap.add_argument("--budget", type=float, default=float(os.environ.get("EVAL_RUN_BUDGET_USD", "1.00")))
    ap.add_argument("--yes", action="store_true", help="skip the cost confirmation")
    ap.add_argument("--rescore", type=Path, help="regrade a saved results file with the current scorers (free)")
    args = ap.parse_args()
    if args.rescore:
        return rescore(args.rescore)
    if not (args.provider and args.model):
        ap.error("--provider and --model are required")

    questions = yaml.safe_load((HERE / "questions.yaml").read_text())["questions"]
    if args.only:
        wanted = set(args.only.split(","))
        questions = [q for q in questions if q["id"] in wanted]
    truth = json.loads((HERE / "ground_truth.json").read_text())
    pricing = yaml.safe_load((ROOT / "config" / "pricing.yaml").read_text())["models"]
    if args.model not in pricing:
        print(f"{args.model} is not in config/pricing.yaml")
        return 2
    token = os.environ.get("EVAL_TOKEN", "")
    if not token:
        print("EVAL_TOKEN is not set (see .env.example)")
        return 2

    try:
        httpx.post(f"{args.api}/sessions", timeout=10).raise_for_status()
    except httpx.HTTPError as e:
        print(f"cannot reach the API at {args.api} ({e}). Is the stack running? Try: make up")
        return 2

    est = estimate_per_question(args.provider, args.model, pricing) * len(questions)
    print(f"{len(questions)} questions on {args.provider}/{args.model}: estimated ${est:.3f}, budget ${args.budget:.2f}")
    if est > args.budget:
        print("estimate is over the budget; raise --budget or use --only")
        return 2
    if not args.yes and input("run it? [y/N] ").strip().lower() != "y":
        return 1

    rows, spent = [], 0.0
    judge_cfg = (args.judge_provider, args.judge_model)
    with httpx.Client(timeout=60) as client:
        for i, q in enumerate(questions, 1):
            if spent >= args.budget:
                print(f"budget ${args.budget:.2f} reached after {len(rows)} questions; stopping")
                break
            try:
                res = ask(client, args.api, token, q["dataset"], q["question"], args.provider, args.model)
                passed, note = grade(q, truth[q["id"]], res, judge_cfg)
            except Exception as e:  # one broken question must not lose the whole run
                res = {"answer": "", "status": "error", "cost_usd": 0.0, "steps": 0, "tool_errors": 0,
                       "charts": 0, "error": str(e), "latency_s": 0.0}
                passed, note = False, f"harness error: {e}"
            spent += res["cost_usd"]
            rows.append({"id": q["id"], "kind": q["kind"], "question": q["question"], "passed": passed,
                         "note": note, **res})
            print(f"[{i}/{len(questions)}] {q['id']} {'PASS' if passed else 'FAIL'} "
                  f"steps={res['steps']} ${res['cost_usd']:.4f} {res['latency_s']:.0f}s  (spent ${spent:.3f})")

    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    meta = {"provider": args.provider, "model": args.model, "timestamp": stamp,
            "judge": f"{args.judge_provider}/{args.judge_model}"}
    base = HERE / "results" / f"{stamp}_{args.model}"
    base.with_suffix(".json").write_text(json.dumps({"meta": meta, "rows": rows}, indent=1))
    summary = summarise(meta, rows)
    base.with_suffix(".md").write_text(summary)
    print("\n" + summary)
    return 0


if __name__ == "__main__":
    sys.exit(main())
