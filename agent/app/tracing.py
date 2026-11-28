"""Tracing and logging: the Python half of one trace per Run.

The Go API starts each Run's trace (span `chat.run`) and sends its
`traceparent` with POST /runs. This service continues it: LangGraph/LangChain
calls become OpenInference spans, each tool call gets a `tool.<name>` span,
and the tool request to Go carries the trace onward.

Off unless an OTLP endpoint is configured, so the default stack needs nothing
extra. `make up-tracing` adds Arize Phoenix to view the traces.
"""

from __future__ import annotations

import contextvars
import json
import logging
import time
from collections.abc import Iterator, Mapping, MutableMapping
from contextlib import contextmanager

from openinference.instrumentation import TraceConfig
from openinference.instrumentation.langchain import LangChainInstrumentor, get_current_span
from opentelemetry import context as otel_context
from opentelemetry import propagate, trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SimpleSpanProcessor, SpanExporter
from opentelemetry.trace import Span, Status, StatusCode

PROJECT = "data-analyst-agent"
SERVICE = "analyst-agent"
MAX_PAYLOAD = 2048  # bytes of any code or output kept on a span

# Our own spans use this tracer. It is a no-op until setup_tracing replaces it.
# (We keep our own rather than setting the global provider, which OTel lets
# you set only once per process.)
_tracer: trace.Tracer = trace.NoOpTracer()
_hide_payloads = False

# The Run being worked on, so every log line can name it.
current_run_id: contextvars.ContextVar[str] = contextvars.ContextVar("run_id", default="")


def setup_tracing(endpoint: str | None, exporter: SpanExporter | None = None,
                  hide_payloads: bool = False) -> TracerProvider | None:
    """Start exporting spans. `exporter` is for tests; otherwise spans go to `endpoint` (OTLP/HTTP)."""
    global _tracer, _hide_payloads
    _hide_payloads = hide_payloads
    if exporter is None and not endpoint:
        _tracer = trace.NoOpTracer()
        return None
    provider = TracerProvider(resource=Resource.create({
        "service.name": SERVICE,
        "openinference.project.name": PROJECT,  # the project Phoenix files the traces under, shared with Go
    }))
    processor = SimpleSpanProcessor(exporter) if exporter else BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint))
    provider.add_span_processor(processor)
    hide = hide_payloads or None  # None leaves OpenInference's own env settings in charge
    LangChainInstrumentor().instrument(tracer_provider=provider, config=TraceConfig(
        hide_inputs=hide, hide_outputs=hide, hide_input_messages=hide, hide_output_messages=hide))
    _tracer = provider.get_tracer("app")
    return provider


def continue_trace(headers: Mapping[str, str]) -> otel_context.Context:
    """The trace context Go sent (traceparent), to attach while the Run streams."""
    return propagate.extract(headers)


def inject_trace(headers: MutableMapping[str, str]) -> None:
    """Add the current trace context (traceparent) to an outgoing request."""
    propagate.inject(headers)


def payload(span: Span, key: str, value: str | None) -> None:
    """Record code or output on span, cut to MAX_PAYLOAD bytes, unless payloads are hidden."""
    if _hide_payloads or not value:
        return
    raw = value.encode()
    if len(raw) > MAX_PAYLOAD:
        value = raw[:MAX_PAYLOAD].decode(errors="ignore") + "…[truncated]"
    span.set_attribute(key, value)


def node_span() -> Span:
    """The span of the LangGraph node we are in (the instrumentor never makes
    its spans current, so trace.get_current_span would miss it)."""
    return get_current_span() or trace.get_current_span()


@contextmanager
def tool_span(name: str, step: int, args: dict) -> Iterator[Span]:
    """A TOOL span for one Tool call, current while Go runs the tool, so the
    request to Go carries it as the parent."""
    parent = trace.set_span_in_context(node_span())
    with _tracer.start_as_current_span(f"tool.{name}", context=parent, attributes={
        "openinference.span.kind": "TOOL", "tool.name": name, "tool.step": step,
        "run.id": current_run_id.get(),
    }, record_exception=False, set_status_on_exception=False) as span:
        payload(span, "input.value", json.dumps(args, default=str))
        yield span


def mark_failure(span: Span, message: str) -> None:
    """A Tool failure: Go could not run the tool at all. (A Tool error, where
    the agent's own code was wrong, is not marked: the model just retries.)"""
    if _hide_payloads:  # the message can quote Go's reply, so it is a payload too
        message = "failed (details hidden: TRACING_HIDE_PAYLOADS)"
    span.set_status(Status(StatusCode.ERROR, message[:MAX_PAYLOAD]))


class FirstToken:
    """Times a model call's first streamed token onto the current node's span."""

    def __init__(self) -> None:
        self._start = time.monotonic()
        self._seen = False

    def saw_token(self) -> None:
        if not self._seen:
            self._seen = True
            node_span().set_attribute("llm.time_to_first_token_ms",
                                      round((time.monotonic() - self._start) * 1000))


class JSONFormatter(logging.Formatter):
    """One JSON object per line, with the Run and its trace for correlation."""

    def format(self, record: logging.LogRecord) -> str:
        line = {"time": self.formatTime(record), "level": record.levelname, "logger": record.name,
                "msg": record.getMessage()}
        if run_id := current_run_id.get():
            line["run_id"] = run_id
        ctx = trace.get_current_span().get_span_context()
        if ctx.is_valid:
            line["trace_id"] = format(ctx.trace_id, "032x")
            line["span_id"] = format(ctx.span_id, "016x")
        if record.exc_info:
            line["exc"] = self.formatException(record.exc_info)
        return json.dumps(line, default=str)


def setup_logging(level: int = logging.INFO) -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(JSONFormatter())
    logging.basicConfig(level=level, handlers=[handler], force=True)
    # Uvicorn set up its own plain-text handlers before calling our factory;
    # send its lines through the JSON handler instead.
    for name in ("uvicorn", "uvicorn.error", "uvicorn.access"):
        uv = logging.getLogger(name)
        uv.handlers.clear()
        uv.propagate = True
