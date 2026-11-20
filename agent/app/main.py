"""The agent service: POST /runs streams one run's events as NDJSON.

It is stateless. Go sends everything a run needs (question, schema, history,
model, limits) and runs every tool. Closing the stream stops the run.
"""

from __future__ import annotations

import hmac
import json
import logging
import os
from collections.abc import AsyncIterator, Callable
from dataclasses import dataclass, field

import httpx
from fastapi import FastAPI, Header, HTTPException
from fastapi.responses import StreamingResponse
from langchain_core.language_models.chat_models import BaseChatModel

from app.graph import build_graph
from app.llm import MissingAPIKey, make_llm
from app.prompts import build_messages
from app.schemas import RunRequest, Usage
from app.tools import ToolClient

log = logging.getLogger("agent")

LLMFactory = Callable[[str, str, int], BaseChatModel]


@dataclass
class Settings:
    internal_token: str = field(default_factory=lambda: os.environ.get("INTERNAL_TOKEN", ""))
    go_internal_url: str = field(default_factory=lambda: os.environ.get("GO_INTERNAL_URL", "http://api:8081"))


def create_app(settings: Settings | None = None, llm_factory: LLMFactory = make_llm,
               tool_transport: httpx.AsyncBaseTransport | None = None) -> FastAPI:
    settings = settings or Settings()
    if not settings.internal_token:
        raise RuntimeError("INTERNAL_TOKEN must be set")
    app = FastAPI(title="data-analyst-agent", docs_url=None, redoc_url=None)

    def check_token(token: str | None) -> None:
        if not token or not hmac.compare_digest(token, settings.internal_token):
            raise HTTPException(status_code=401, detail="invalid internal token")

    @app.get("/health")
    async def health() -> dict:
        return {"status": "ok"}

    @app.post("/runs")
    async def runs(req: RunRequest, x_internal_token: str | None = Header(default=None)) -> StreamingResponse:
        check_token(x_internal_token)
        try:
            llm = llm_factory(req.model.provider, req.model.name, req.limits.max_tokens)
        except MissingAPIKey as exc:
            # A setup problem the user can fix: report it in-stream so the UI shows it.
            return StreamingResponse(iter([_line({"type": "error", "error": str(exc)})]),
                                     media_type="application/x-ndjson")
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        tools = ToolClient(settings.go_internal_url, settings.internal_token, req.run_id, req.session_id,
                           transport=tool_transport)
        return StreamingResponse(_events(req, llm, tools), media_type="application/x-ndjson")

    return app


async def _events(req: RunRequest, llm: BaseChatModel, tools: ToolClient) -> AsyncIterator[str]:
    usage = Usage(provider=req.model.provider, model=req.model.name)
    graph = build_graph(llm, tools, usage, req.limits.max_steps)
    state = {"messages": build_messages(req), "steps": 0, "answer": "", "status": ""}
    final: dict = {}
    try:
        async for mode, chunk in graph.astream(state, stream_mode=["custom", "values"],
                                               config={"recursion_limit": 4 * req.limits.max_steps + 10}):
            if mode == "custom":
                yield _line(chunk)
            else:
                final = chunk
        yield _line({"type": "done", "answer": final.get("answer", ""),
                     "status": final.get("status") or "done", "usage": usage.model_dump()})
    except Exception as exc:
        log.exception("run %s failed", req.run_id)
        yield _line({"type": "error", "error": f"agent failed: {type(exc).__name__}: {exc}",
                     "usage": usage.model_dump()})
    finally:
        await tools.aclose()


def _line(event: dict) -> str:
    return json.dumps(event, default=str) + "\n"


def app_from_env() -> FastAPI:  # uvicorn --factory app.main:app_from_env
    logging.basicConfig(level=logging.INFO)
    return create_app()
