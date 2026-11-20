"""Seam 8 test kit: POST /runs over HTTP with a fake LLM and a fake Go tool endpoint."""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import Any

import httpx
import pytest
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage
from langchain_core.outputs import ChatGeneration, ChatResult
from pydantic import Field

from app.main import Settings, create_app

TOKEN = "test-token"


class FakeLLM(BaseChatModel):
    """Replies from a script. Records every call's messages and whether tools were bound."""

    script: list[Any]  # AIMessage replies, or exceptions to raise
    calls: list[dict[str, Any]] = Field(default_factory=list)
    tools_bound: bool = False
    tool_choice: str | None = None

    @property
    def _llm_type(self) -> str:
        return "fake"

    def bind_tools(self, tools, **kwargs):
        bound = self.model_copy(update={"tools_bound": True, "tool_choice": kwargs.get("tool_choice")})
        # Share script and calls with the parent so tests can inspect them.
        object.__setattr__(bound, "script", self.script)
        object.__setattr__(bound, "calls", self.calls)
        return bound

    def _generate(self, messages: list[BaseMessage], stop=None, run_manager=None, **kwargs) -> ChatResult:
        self.calls.append({"messages": list(messages), "tools_bound": self.tools_bound,
                           "tool_choice": self.tool_choice})
        if not self.script:
            raise AssertionError("FakeLLM script exhausted")
        reply = self.script.pop(0)
        if isinstance(reply, Exception):
            raise reply
        return ChatResult(generations=[ChatGeneration(message=reply)])


def ai(text: str = "", tool_calls: list[dict] | None = None, inp: int = 100, out: int = 10,
       cache_read: int = 0, cache_write: int = 0) -> AIMessage:
    calls = [{"id": f"call_{i}_{c['name']}", "name": c["name"], "args": c["args"]}
             for i, c in enumerate(tool_calls or [])]
    return AIMessage(content=text, tool_calls=calls, usage_metadata={
        "input_tokens": inp, "output_tokens": out, "total_tokens": inp + out,
        "input_token_details": {"cache_read": cache_read, "cache_creation": cache_write},
    })


def tool(name: str, **args) -> dict:
    return {"name": name, "args": args}


class FakeGo:
    """The Go internal tool API, as an httpx transport."""

    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.respond: Callable[[str, dict], dict] = lambda name, body: {
            "output": f"{name} ok", "chart_ids": []}

    def handler(self, request: httpx.Request) -> httpx.Response:
        assert request.headers.get("X-Internal-Token") == TOKEN
        name = request.url.path.rsplit("/", 1)[-1]
        body = json.loads(request.content)
        self.requests.append({"tool": name, **body})
        return httpx.Response(200, json=self.respond(name, body))


@pytest.fixture
def go() -> FakeGo:
    return FakeGo()


@pytest.fixture
def make_client(go):
    """make_client(llm) -> an httpx.AsyncClient bound to the app."""

    def factory(llm: FakeLLM, **settings):
        app = create_app(
            Settings(internal_token=TOKEN, go_internal_url="http://go", **settings),
            llm_factory=lambda provider, model, max_tokens: llm,
            tool_transport=httpx.MockTransport(go.handler),
        )
        return httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://agent")

    return factory


def run_request(**overrides) -> dict:
    req = {
        "run_id": "run-1",
        "session_id": "sess-1",
        "question": "What is the total amount?",
        "model": {"provider": "anthropic", "name": "claude-haiku-5-5"},
        "schema": [{"name": "sales", "rows": 3,
                    "columns": [{"name": "region", "type": "VARCHAR"}, {"name": "amount", "type": "BIGINT"}],
                    "sample_rows": [["north", 10]]}],
        "history": [],
        "limits": {"max_steps": 10, "max_tokens": 1024},
    }
    req.update(overrides)
    return req


async def stream_run(client: httpx.AsyncClient, body: dict, token: str = TOKEN) -> tuple[int, list[dict]]:
    async with client.stream("POST", "/runs", json=body, headers={"X-Internal-Token": token}) as resp:
        if resp.status_code != 200:
            await resp.aread()
            return resp.status_code, []
        events = [json.loads(line) async for line in resp.aiter_lines() if line.strip()]
    return 200, events


def types(events: list[dict]) -> list[str]:
    return [e["type"] for e in events]
