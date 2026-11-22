import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from judge import parse_verdict
from scoring import numbers_in, score_exact, score_list, score_number


def test_numbers_understand_currency_commas_percent_and_suffixes():
    assert numbers_in("Revenue was $209,566.70 (up 12.5%) and **1,078** orders; about 209.6k") == [
        209566.70, 12.5, 1078.0, 209600.0]


def test_negative_and_unicode_minus():
    assert numbers_in("changed by -53.3% or −53") == [-53.3, -53.0]


def test_number_within_tolerance_and_sign_handling():
    assert score_number("About $209,600 in total", 209566.70, tol=0.01)
    assert not score_number("About $230,000", 209566.70, tol=0.01)
    assert score_number("a 53.3% drop", -53.25, tol=0.01, ignore_sign=True)
    assert not score_number("a 53.3% drop", -53.25, tol=0.01)


def test_zero_tolerance_needs_the_exact_integer():
    assert score_number("There were 1,078 orders", 1078, tol=0)
    assert not score_number("There were 1,079 orders", 1078, tol=0)


def test_exact_ignores_case_and_markdown():
    assert score_exact("The top region is **North**.", "North")
    assert not score_exact("South leads", "North")


def test_list_order():
    answer = "1. Rain Shell 2. Trail Pack 3. Day Bag"
    assert score_list(answer, ["Rain Shell", "Trail Pack", "Day Bag"], ordered=True)
    assert not score_list(answer, ["Trail Pack", "Rain Shell", "Day Bag"], ordered=True)
    assert score_list(answer, ["Trail Pack", "Rain Shell", "Day Bag"])
    assert not score_list("Rain Shell and Day Bag", ["Rain Shell", "Trail Pack"])


def test_judge_verdict_parsing():
    assert parse_verdict('```json\n{"pass": true, "reason": "ok"}\n```') == (True, "ok")
    assert parse_verdict("no json")[0] is False


def test_ordered_list_ignores_an_early_mention_of_a_later_item():
    # A headline like "Engineering is highest and Support lowest" must not break the ranking that follows.
    answer = "Engineering is highest, Support lowest.\n1. Engineering 2. Finance 3. Sales 4. Marketing 5. Support"
    assert score_list(answer, ["Engineering", "Finance", "Sales", "Marketing", "Support"], ordered=True)
    assert not score_list("1. Finance 2. Engineering", ["Engineering", "Finance"], ordered=True)
