"""Builds the model's input from a run request: system prompt, history, question."""

from __future__ import annotations

import json
import re
from pathlib import Path

from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage

from app.schemas import RunRequest, Table, Turn

_SYSTEM = (Path(__file__).parent / "system.md").read_text()
_PLAIN = re.compile(r"^[a-z_][a-z0-9_]*$")

WRAP_UP = ("You have used all {max_steps} tool calls allowed for this question. Do not call any more tools. "
           "Answer now using only what you have found so far, and say clearly that you hit the step limit, "
           "so the answer may be incomplete.")


def _ident(name: str) -> str:
    return name if _PLAIN.match(name) else '"' + name.replace('"', '""') + '"'


def format_schema(tables: list[Table]) -> str:
    if not tables:
        return "No tables have been uploaded yet. Tell the user to upload a CSV or Parquet file."
    blocks = []
    for t in tables:
        cols = "\n".join(f"  - {_ident(c.name)} {c.type}" for c in t.columns)
        sample = "\n".join("  " + " | ".join("NULL" if v is None else str(v) for v in row)
                           for row in t.sample_rows[:3])
        block = f"### table {_ident(t.name)} ({t.rows} rows)\ncolumns:\n{cols}"
        if sample:
            block += f"\nsample rows:\n{sample}"
        blocks.append(block)
    return "\n\n".join(blocks)


def system_prompt(req: RunRequest) -> str:
    return _SYSTEM.format(schema=format_schema(req.schema_), max_steps=req.limits.max_steps)


def _turn_messages(turn: Turn) -> list[BaseMessage]:
    """Earlier turns are replayed as plain text, so they work with any provider."""
    if not turn.steps:
        return [HumanMessage(turn.question), AIMessage(turn.answer)]
    work = "\n".join(f"- {s.tool} {json.dumps(s.input)}\n  result: {s.output}" for s in turn.steps)
    return [HumanMessage(turn.question), AIMessage(f"[My tool calls for this question]\n{work}\n\n{turn.answer}")]


def build_messages(req: RunRequest) -> list[BaseMessage]:
    text = system_prompt(req)
    if req.model.provider == "anthropic":
        # Cache the system prompt (instructions plus schema) across this run's calls.
        system = SystemMessage(content=[{"type": "text", "text": text, "cache_control": {"type": "ephemeral"}}])
    else:
        system = SystemMessage(content=text)  # DeepSeek caches repeated prefixes automatically
    messages: list[BaseMessage] = [system]
    for turn in req.history:
        messages.extend(_turn_messages(turn))
    messages.append(HumanMessage(req.question))
    return messages
