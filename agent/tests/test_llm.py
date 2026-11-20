import pytest
from langchain_anthropic import ChatAnthropic
from langchain_deepseek import ChatDeepSeek

from app.llm import make_llm


def test_anthropic_and_deepseek_build_their_own_clients(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")
    monkeypatch.setenv("DEEPSEEK_API_KEY", "test")
    claude = make_llm("anthropic", "claude-haiku-5-5", 1024)
    deepseek = make_llm("deepseek", "deepseek-flash", 2048)
    assert isinstance(claude, ChatAnthropic) and claude.model == "claude-haiku-5-5"
    assert claude.max_tokens == 1024 and claude.temperature == 0
    assert isinstance(deepseek, ChatDeepSeek) and deepseek.model_name == "deepseek-flash"
    assert deepseek.max_tokens == 2048


def test_unknown_provider_is_a_value_error():
    with pytest.raises(ValueError, match="unknown provider"):
        make_llm("openai", "gpt", 100)
