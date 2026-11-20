"""The one place that knows how to build a chat model for a provider."""

from __future__ import annotations

import os

from langchain.chat_models import init_chat_model
from langchain_core.language_models.chat_models import BaseChatModel

KEY_VARS = {"anthropic": "ANTHROPIC_API_KEY", "deepseek": "DEEPSEEK_API_KEY"}
PROVIDERS = set(KEY_VARS)


class MissingAPIKey(RuntimeError):
    """The chosen provider's API key is not configured."""


def make_llm(provider: str, model: str, max_tokens: int) -> BaseChatModel:
    """API keys come from ANTHROPIC_API_KEY / DEEPSEEK_API_KEY in the environment."""
    if provider not in PROVIDERS:
        raise ValueError(f"unknown provider {provider!r}; expected one of {sorted(PROVIDERS)}")
    var = KEY_VARS[provider]
    if not os.environ.get(var, "").strip():
        # An empty key otherwise surfaces later as a confusing "connection error".
        raise MissingAPIKey(f"{var} is not set: add it to .env and restart the stack")
    return init_chat_model(model, model_provider=provider, max_tokens=max_tokens, temperature=0)
