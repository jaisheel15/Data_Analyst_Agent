"""Optional tracing: LangChain/LangGraph calls become OpenTelemetry spans.

Off unless an OTLP endpoint is configured, so the default stack needs nothing
extra. `docker compose --profile tracing up` adds Arize Phoenix to view them.
"""

from __future__ import annotations

from openinference.instrumentation.langchain import LangChainInstrumentor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SimpleSpanProcessor, SpanExporter

PROJECT = "data-analyst-agent"


def setup_tracing(endpoint: str | None, exporter: SpanExporter | None = None) -> TracerProvider | None:
    """Start exporting spans. `exporter` is for tests; otherwise spans go to `endpoint` (OTLP/HTTP)."""
    if exporter is None and not endpoint:
        return None
    provider = TracerProvider(resource=Resource.create({
        "service.name": PROJECT,
        "openinference.project.name": PROJECT,  # the project Phoenix files the traces under
    }))
    processor = SimpleSpanProcessor(exporter) if exporter else BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint))
    provider.add_span_processor(processor)
    LangChainInstrumentor().instrument(tracer_provider=provider)
    return provider
