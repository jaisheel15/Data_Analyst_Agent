"""LLM judge for narrative answers. Calls the provider API directly (not through the app)."""

from __future__ import annotations

import json
import os
import re

import httpx

PROMPT = """You are grading an AI data analyst's answer.

Question: {question}

Facts computed directly from the data (these are correct):
{facts}

Grading rubric: {rubric}

Answer to grade:
{answer}

Check the answer against the rubric and the facts only. Numbers may be rounded. Reply with JSON only:
{{"pass": true or false, "reason": "one sentence"}}"""

ENDPOINTS = {
    "anthropic": "https://api.anthropic.com/v1/messages",
    "deepseek": "https://api.deepseek.com/chat/completions",
}


def judge(provider: str, model: str, question: str, answer: str, facts: list, rubric: str) -> tuple[bool, str]:
    for _ in range(3):  # reasoning models sometimes return an empty reply; ask again
        verdict = _judge_once(provider, model, question, answer, facts, rubric)
        if not verdict[1].startswith("judge reply was not"):
            return verdict
    return verdict


def _judge_once(provider: str, model: str, question: str, answer: str, facts: list, rubric: str) -> tuple[bool, str]:
    prompt = PROMPT.format(question=question, facts=json.dumps(facts[:60]), rubric=rubric, answer=answer[:6000])
    if provider == "anthropic":
        key = os.environ["ANTHROPIC_API_KEY"].strip()
        r = httpx.post(ENDPOINTS[provider], timeout=120,
                       headers={"x-api-key": key, "anthropic-version": "2023-06-01"},
                       json={"model": model, "max_tokens": 300, "messages": [{"role": "user", "content": prompt}]})
        r.raise_for_status()
        text = r.json()["content"][0]["text"]
    else:
        key = os.environ["DEEPSEEK_API_KEY"].strip()
        r = httpx.post(ENDPOINTS[provider], timeout=120, headers={"Authorization": f"Bearer {key}"},
                       json={"model": model, "max_tokens": 3000, "temperature": 0,
                             "messages": [{"role": "user", "content": prompt}]})
        r.raise_for_status()
        text = r.json()["choices"][0]["message"]["content"]
    return parse_verdict(text)


def parse_verdict(text: str) -> tuple[bool, str]:
    match = re.search(r"\{.*\}", text, re.S)
    if not match:
        return False, f"judge reply was not JSON: {text[:100]}"
    try:
        data = json.loads(match.group(0))
        return bool(data["pass"]), str(data.get("reason", ""))
    except (ValueError, KeyError):
        return False, f"judge reply was not valid: {text[:100]}"
