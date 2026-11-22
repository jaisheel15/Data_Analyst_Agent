import json

import pytest
from conftest import FakeLLM, ai, run_request, stream_run, tool
from openinference.instrumentation.langchain import LangChainInstrumentor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from app.tracing import setup_tracing


@pytest.fixture
def spans():
    exporter = InMemorySpanExporter()
    setup_tracing(endpoint=None, exporter=exporter)
    yield exporter
    LangChainInstrumentor().uninstrument()


async def test_a_run_produces_spans_tagged_with_its_run_and_session(make_client, spans):
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select 1")]), ai("One.")])
    async with make_client(llm) as client:
        await stream_run(client, run_request())
    finished = spans.get_finished_spans()
    assert finished, "no spans were exported"
    kinds = {s.attributes.get("openinference.span.kind") for s in finished}
    assert {"LLM", "CHAIN"} <= kinds
    tagged = [s for s in finished if s.attributes.get("session.id") == "sess-1"]
    assert tagged, "spans do not carry the session id"
    meta = json.loads(next(s for s in tagged if "metadata" in s.attributes).attributes["metadata"])
    assert meta["run_id"] == "run-1"


def test_tracing_is_off_without_an_endpoint():
    assert setup_tracing(endpoint=None) is None
