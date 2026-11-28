"""Tool schemas the model sees, and the client that runs them on the Go service.

The tools themselves run in Go (and its sandbox). This service only forwards
calls, so swapping LangGraph for a hand-written loop later changes nothing here.
"""

from __future__ import annotations

from dataclasses import dataclass

import httpx
from pydantic import BaseModel, Field

from app.tracing import inject_trace


class describe_dataset(BaseModel):
    """List the uploaded tables with their columns, types, row counts and a few sample rows.
    Pass a table name to describe just that table."""

    table: str | None = Field(default=None, description="Optional table name")


class run_sql(BaseModel):
    """Run one DuckDB SQL query against the uploaded tables (read-only).
    Returns up to 50 rows plus the total row count. Prefer this for aggregations and filtering."""

    sql: str = Field(description="A single DuckDB SQL statement")


class run_python(BaseModel):
    """Run Python 3.12 with pandas, numpy, scipy, matplotlib and seaborn. No network access.
    A read-only DuckDB connection is available as `con`. Use it for statistics, reshaping and charts.
    Print what you need to see."""

    code: str = Field(description="Python source code")


TOOLS = [describe_dataset, run_sql, run_python]


@dataclass
class ToolResult:
    output: str
    error: str
    chart_ids: list[str]
    # True when Go could not be reached or refused the call: a Tool failure,
    # not a Tool error in the agent's code. (Failures inside Go, such as a
    # sandbox timeout, are marked on Go's own spans.)
    failed: bool = False

    def for_model(self) -> str:
        """The text the model reads back as the tool result."""
        parts = [self.output] if self.output else []
        if self.error:
            parts.append("ERROR:\n" + self.error)
        return "\n".join(parts) or "(no output)"

    def summary(self, limit: int = 200) -> str:
        text = self.error or self.output
        return text if len(text) <= limit else text[:limit] + "…"


class ToolClient:
    """Calls POST {base_url}/internal/tools/{name} on the Go service."""

    def __init__(self, base_url: str, token: str, run_id: str, session_id: str,
                 transport: httpx.AsyncBaseTransport | None = None) -> None:
        self._http = httpx.AsyncClient(base_url=base_url, transport=transport,
                                       headers={"X-Internal-Token": token},
                                       timeout=httpx.Timeout(60.0, connect=5.0))
        self._run_id = run_id
        self._session_id = session_id

    async def call(self, name: str, args: dict) -> ToolResult:
        headers: dict[str, str] = {}
        inject_trace(headers)  # Go's tool span joins the current trace
        try:
            resp = await self._http.post(f"/internal/tools/{name}", headers=headers, json={
                "run_id": self._run_id, "session_id": self._session_id, "input": args})
        except httpx.HTTPError as exc:
            return ToolResult("", f"tool service unreachable: {exc}", [], failed=True)
        if resp.status_code != 200:
            return ToolResult("", f"tool service returned {resp.status_code}: {resp.text[:300]}", [],
                              failed=True)
        body = resp.json()
        return ToolResult(body.get("output", ""), body.get("error", "") or "", body.get("chart_ids") or [])

    async def aclose(self) -> None:
        await self._http.aclose()
