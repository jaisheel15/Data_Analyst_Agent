"""Scoring for the non-narrative question kinds. Pure functions, unit-tested."""

from __future__ import annotations

import re

_NUM = re.compile(r"(?<![\w.])(-?\$?-?\d[\d,]*(?:\.\d+)?)\s*([kKmM](?![a-zA-Z]))?")


def numbers_in(text: str) -> list[float]:
    """Every number in the text. Understands $, thousands commas, %, and k/m suffixes."""
    text = text.replace("*", "").replace("−", "-")  # markdown bold, unicode minus
    found = []
    for raw, suffix in _NUM.findall(text):
        cleaned = raw.replace("$", "").replace(",", "")
        try:
            value = float(cleaned)
        except ValueError:
            continue
        if suffix:
            value *= 1_000 if suffix in "kK" else 1_000_000
        found.append(value)
    return found


def score_number(answer: str, expected: float, tol: float = 0.01, ignore_sign: bool = False) -> bool:
    for n in numbers_in(answer):
        got, want = (abs(n), abs(expected)) if ignore_sign else (n, expected)
        if abs(got - want) <= tol * abs(want) + 1e-9:
            return True
    return False


def _norm(text: str) -> str:
    return re.sub(r"[\s*_`]+", " ", text.lower())


def score_exact(answer: str, expected: str) -> bool:
    return _norm(expected).strip() in _norm(answer)


def score_list(answer: str, expected: list[str], ordered: bool = False) -> bool:
    text = _norm(answer)
    positions = [text.find(_norm(v).strip()) for v in expected]
    if any(p < 0 for p in positions):
        return False
    return positions == sorted(positions) if ordered else True
