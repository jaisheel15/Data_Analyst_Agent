"""Seam 9 (Python side): what this service sends and accepts matches contracts/."""

import json
from pathlib import Path

import pytest
from conftest import FakeLLM, ai, run_request, stream_run, tool
from jsonschema import Draft202012Validator
from referencing import Registry, Resource

SCHEMA = json.loads((Path(__file__).parents[2] / "contracts" / "internal-api.schema.json").read_text())
REGISTRY = Registry().with_resource(SCHEMA["$id"], Resource.from_contents(SCHEMA))


def validate(definition: str, payload) -> None:
    Draft202012Validator({"$ref": f"{SCHEMA['$id']}#/$defs/{definition}"}, registry=REGISTRY).validate(payload)


def test_the_test_kit_sends_contract_shaped_run_requests():
    validate("RunRequest", run_request())


async def test_streamed_events_and_tool_requests_match_the_contract(make_client, go):
    go.respond = lambda name, body: {"output": "ok", "chart_ids": ["c1"]}
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select 1")]), ai("done")])
    async with make_client(llm) as client:
        _, events = await stream_run(client, run_request())
    for event in events:
        validate("Event", event)
    for req in go.requests:
        validate("ToolRequest", {k: v for k, v in req.items() if k != "tool"})


@pytest.mark.parametrize("script", [
    [RuntimeError("boom")],
    [ai(tool_calls=[tool("run_python", code="1")]), ai("partial")],
])
async def test_error_and_budget_events_match_the_contract(make_client, script):
    async with make_client(FakeLLM(script=script)) as client:
        _, events = await stream_run(client, run_request(limits={"max_steps": 1, "max_tokens": 1024}))
    for event in events:
        validate("Event", event)
