"""Executes one tool call inside the sandbox container.

Reads a JSON request on stdin, writes a JSON result on stdout. Every call is a
fresh process: nothing survives between calls except files in scratch_dir.
The wall-clock timeout is enforced from outside (the Go sandbox manager kills
the process), so this script never needs to police time itself.
"""

from __future__ import annotations

import contextlib
import io
import json
import linecache
import math
import os
import sys
import traceback

os.environ.setdefault("MPLBACKEND", "Agg")  # no display inside the sandbox

DEFAULTS = {
    "db_path": "/data/session.duckdb",
    "out_dir": "/out",
    "scratch_dir": "/scratch",
    "max_output_chars": 8000,
    "max_rows": 50,
}


class DatabaseBusy(RuntimeError):
    pass


def _connect(db_path: str):
    import duckdb

    if not os.path.exists(db_path):
        # No dataset uploaded yet: give the agent an empty in-memory database.
        return duckdb.connect(":memory:")
    try:
        return duckdb.connect(db_path, read_only=True)
    except duckdb.IOException as exc:
        # Most likely Go holds the write lock while loading an upload.
        raise DatabaseBusy(f"the database is busy (an upload is in progress); try again in a moment ({exc})") from exc


def _truncate(text: str, limit: int) -> tuple[str, bool]:
    if len(text) <= limit:
        return text, False
    cut = len(text) - limit
    return text[:limit] + f"\n... [truncated {cut} more characters]", True


def _compile_agent_code(code: str):
    # Register the source so tracebacks can show the failing line of agent code.
    linecache.cache["<agent>"] = (len(code), None, code.splitlines(True), "<agent>")
    return compile(code, "<agent>", "exec")


def _agent_traceback(exc: BaseException) -> str:
    # Drop the runner's own frame: the agent only needs to see its own code.
    tb = exc.__traceback__.tb_next if exc.__traceback__ else None
    return "".join(traceback.format_exception(type(exc), exc, tb))


def _pngs(out_dir: str) -> set[str]:
    return {f for f in os.listdir(out_dir) if f.lower().endswith(".png")}


def _save_open_figures(out_dir: str) -> None:
    plt = sys.modules.get("matplotlib.pyplot")
    if plt is None:
        return
    for i, num in enumerate(plt.get_fignums(), start=1):
        plt.figure(num).savefig(os.path.join(out_dir, f"chart_{i}.png"),
                                dpi=110, bbox_inches="tight")
    plt.close("all")


def run_python(req: dict) -> dict:
    before = _pngs(req["out_dir"])
    out = io.StringIO()
    err = io.StringIO()
    try:
        con = _connect(req["db_path"])
    except DatabaseBusy as exc:
        return {"stdout": "", "stderr": "", "error": str(exc), "truncated": False, "files": []}
    scope = {"con": con, "SCRATCH": req["scratch_dir"], "OUT": req["out_dir"],
             "__name__": "__main__"}
    error = None
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        try:
            exec(_compile_agent_code(req["code"]), scope)
        except BaseException as exc:  # noqa: BLE001 - report every failure to the agent
            error = _agent_traceback(exc)
    con.close()
    if not _pngs(req["out_dir"]) - before:
        # The agent drew but never called savefig: keep its figures anyway.
        _save_open_figures(req["out_dir"])
    files = sorted(_pngs(req["out_dir"]) - before)
    stdout, t1 = _truncate(out.getvalue(), req["max_output_chars"])
    stderr, t2 = _truncate(err.getvalue(), req["max_output_chars"])
    return {"stdout": stdout, "stderr": stderr, "error": error, "truncated": t1 or t2,
            "files": files}


def _json_safe(value):
    if isinstance(value, float) and not math.isfinite(value):
        return None  # NaN and infinity are not valid JSON
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    import decimal

    if isinstance(value, decimal.Decimal):
        return float(value)
    if hasattr(value, "isoformat"):  # date, datetime, time
        return value.isoformat()
    return str(value)


def run_sql(req: dict) -> dict:
    try:
        con = _connect(req["db_path"])
    except DatabaseBusy as exc:
        return {"columns": [], "rows": [], "total_rows": 0, "truncated": False, "error": str(exc)}
    try:
        cur = con.execute(req["code"])
        columns = [d[0] for d in cur.description] if cur.description else []
        rows = cur.fetchmany(req["max_rows"]) if columns else []
        total = len(rows)
        while columns and (batch := cur.fetchmany(10_000)):
            total += len(batch)
    except Exception as exc:  # noqa: BLE001 - SQL errors go back to the agent
        return {"columns": [], "rows": [], "total_rows": 0, "truncated": False,
                "error": f"{type(exc).__name__}: {exc}"}
    finally:
        con.close()
    return {
        "columns": columns,
        "rows": [[_json_safe(v) for v in row] for row in rows],
        "total_rows": total,
        "truncated": total > len(rows),
        "error": None,
    }


def main() -> None:
    req = {**DEFAULTS, **json.load(sys.stdin)}
    if req["mode"] == "python":
        result = run_python(req)
    elif req["mode"] == "sql":
        result = run_sql(req)
    else:
        result = {"error": f"unknown mode: {req['mode']!r}"}
    json.dump(result, sys.__stdout__, allow_nan=False)


if __name__ == "__main__":
    main()
