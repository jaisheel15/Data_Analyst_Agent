"""Tracing: the agent continues the Run's trace that Go started, and passes it on to Go's tools."""

import json
import logging

import httpx
import pytest
from conftest import TOKEN, FakeLLM, ai, run_request, stream_run, tool
from openinference.instrumentation.langchain import LangChainInstrumentor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode

from app.main import Settings, create_app
from app.tracing import JSONFormatter, setup_tracing

GO_TRACE = "4bf92f3577b34da6a3ce929d0e0e4736"
GO_SPAN = "00f067aa0ba902b7"
TRACEPARENT = {"traceparent": f"00-{GO_TRACE}-{GO_SPAN}-01"}


@pytest.fixture
def spans():
    exporter = InMemorySpanExporter()
    setup_tracing(endpoint=None, exporter=exporter)
    yield exporter
    LangChainInstrumentor().uninstrument()
    setup_tracing(endpoint=None)  # back to the no-op tracer


@pytest.fixture
def hidden_spans():
    exporter = InMemorySpanExporter()
    setup_tracing(endpoint=None, exporter=exporter, hide_payloads=True)
    yield exporter
    LangChainInstrumentor().uninstrument()
    setup_tracing(endpoint=None)


def named(exporter, name):
    found = [s for s in exporter.get_finished_spans() if s.name == name]
    assert len(found) == 1, f"want one {name!r} span, got {[s.name for s in exporter.get_finished_spans()]}"
    return found[0]


def hexid(n: int, width: int) -> str:
    return format(n, f"0{width}x")


async def test_a_run_produces_spans_tagged_with_its_run_and_session(make_client, spans):
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select 1")]), ai("One.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request())
    finished = spans.get_finished_spans()
    assert finished, "no spans were exported"
    kinds = {s.attributes.get("openinference.span.kind") for s in finished}
    assert {"LLM", "CHAIN", "TOOL"} <= kinds
    tagged = [s for s in finished if s.attributes.get("session.id") == "sess-1"]
    assert tagged, "spans do not carry the session id"
    meta = json.loads(next(s for s in tagged if "metadata" in s.attributes).attributes["metadata"])
    assert meta["run_id"] == "run-1"


async def test_the_run_joins_the_trace_go_started(make_client, spans):
    llm = FakeLLM(script=[ai("Hi.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request(), headers=TRACEPARENT)
    root = named(spans, "analyst-run")
    assert hexid(root.context.trace_id, 32) == GO_TRACE
    assert root.parent is not None and hexid(root.parent.span_id, 16) == GO_SPAN
    assert all(hexid(s.context.trace_id, 32) == GO_TRACE for s in spans.get_finished_spans())


async def test_each_tool_call_is_a_tool_span_that_go_continues(make_client, go, spans):
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select 1")]), ai("One.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request(), headers=TRACEPARENT)
    span = named(spans, "tool.run_sql")
    assert span.attributes["openinference.span.kind"] == "TOOL"
    assert span.attributes["tool.step"] == 1
    assert span.attributes["run.id"] == "run-1"
    assert "select 1" in span.attributes["input.value"]
    assert span.attributes["output.value"] == "run_sql ok"
    assert span.status.status_code != StatusCode.ERROR
    # It sits under the LangGraph tools node, in Go's trace...
    assert span.parent.span_id == named(spans, "tools").context.span_id
    assert hexid(span.context.trace_id, 32) == GO_TRACE
    # ...and the request to Go names it as the parent of Go's tool span.
    sent = go.headers[0]["traceparent"]
    assert sent == f"00-{GO_TRACE}-{hexid(span.context.span_id, 16)}-01"


async def test_a_tool_error_is_recorded_but_not_marked_as_a_failure(make_client, go, spans):
    go.respond = lambda name, body: {"output": "", "error": "Binder Error: column x not found"}
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select x")]), ai("Sorry.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request())
    span = named(spans, "tool.run_sql")
    assert "column x" in span.attributes["tool.error"]
    assert span.attributes["tool.failed"] is False
    assert span.status.status_code != StatusCode.ERROR


async def test_an_unreachable_tool_service_is_a_tool_failure(spans):
    def unreachable(request):
        raise httpx.ConnectError("connection refused")

    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select 1")]), ai("Sorry.")])
    app = create_app(Settings(internal_token=TOKEN, go_internal_url="http://go"),
                     llm_factory=lambda *args: llm, tool_transport=httpx.MockTransport(unreachable))
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://agent") as client:
        await stream_run(client, run_request())
    span = named(spans, "tool.run_sql")
    assert span.attributes["tool.failed"] is True
    assert span.status.status_code == StatusCode.ERROR


async def test_model_calls_record_time_to_first_token_and_withdrawn_preambles(make_client, spans):
    llm = FakeLLM(script=[ai("Let me check.", tool_calls=[tool("run_sql", sql="select 1")]), ai("One.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request())
    agent_nodes = [s for s in spans.get_finished_spans() if s.name == "agent"]
    assert len(agent_nodes) == 2
    assert all("llm.time_to_first_token_ms" in s.attributes for s in agent_nodes)
    assert [e.name for e in agent_nodes[0].events] == ["answer_reset"]
    assert not agent_nodes[1].events


async def test_payloads_can_be_kept_off_spans(make_client, hidden_spans):
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select secret")]), ai("Secret answer.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request(question="What is the secret?"))
    for span in hidden_spans.get_finished_spans():
        for key, value in span.attributes.items():
            assert "secret" not in str(value).lower(), f"{span.name}.{key} leaks a payload"


def test_tracing_is_off_without_an_endpoint():
    assert setup_tracing(endpoint=None) is None


async def test_log_lines_are_json_with_the_run_and_its_trace(make_client, spans):
    seen: list[dict] = []

    class Capture(logging.Handler):
        def emit(self, record):
            seen.append(json.loads(JSONFormatter().format(record)))

    handler = Capture()
    logging.getLogger("agent").addHandler(handler)
    try:
        llm = FakeLLM(script=[RuntimeError("model exploded")])
        async with make_client(llm) as client:
            await stream_run(client, run_request(), headers=TRACEPARENT)
    finally:
        logging.getLogger("agent").removeHandler(handler)
    line = next(line for line in seen if "failed" in line["msg"])
    assert line["run_id"] == "run-1"
    assert line["trace_id"] == GO_TRACE
    assert line["level"] == "ERROR"
