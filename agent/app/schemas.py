"""Pydantic models for the internal contract (contracts/internal-api.schema.json)."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field


class Model(BaseModel):
    provider: Literal["anthropic", "deepseek"]
    name: str


class Column(BaseModel):
    name: str
    type: str


class Table(BaseModel):
    name: str
    rows: int
    columns: list[Column]
    sample_rows: list[list[Any]] = []


class Step(BaseModel):
    tool: str
    input: Any
    output: str


class Turn(BaseModel):
    question: str
    answer: str
    steps: list[Step] = []


class Limits(BaseModel):
    max_steps: int = Field(default=10, ge=1, le=50)
    max_tokens: int = Field(default=4096, ge=256)


class RunRequest(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    run_id: str
    session_id: str
    question: str
    model: Model
    schema_: list[Table] = Field(default=[], alias="schema")
    history: list[Turn] = []
    limits: Limits = Limits()


class Usage(BaseModel):
    provider: str
    model: str
    input_tokens: int = 0  # uncached input only
    output_tokens: int = 0
    cached_tokens: int = 0  # cache reads
    cache_write_tokens: int = 0
    steps: int = 0

    def add(self, usage_metadata: dict | None) -> None:
        if not usage_metadata:
            return
        details = usage_metadata.get("input_token_details") or {}
        read = details.get("cache_read", 0) or 0
        write = details.get("cache_creation", 0) or 0
        self.input_tokens += max(usage_metadata.get("input_tokens", 0) - read - write, 0)
        self.output_tokens += usage_metadata.get("output_tokens", 0)
        self.cached_tokens += read
        self.cache_write_tokens += write
