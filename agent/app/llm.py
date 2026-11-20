"""The one place that knows how to build a chat model for a provider."""

from __future__ import annotations

from langchain.chat_models import init_chat_model
from langchain_core.language_models.chat_models import BaseChatModel

PROVIDERS = {"anthropic", "deepseek"}


def make_llm(provider: str, model: str, max_tokens: int) -> BaseChatModel:
    """API keys come from ANTHROPIC_API_KEY / DEEPSEEK_API_KEY in the environment."""
    if provider not in PROVIDERS:
        raise ValueError(f"unknown provider {provider!r}; expected one of {sorted(PROVIDERS)}")
    return init_chat_model(model, model_provider=provider, max_tokens=max_tokens, temperature=0)
