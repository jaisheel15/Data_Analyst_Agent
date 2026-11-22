"""Compute the expected answers for every question from the data. Writes ground_truth.json.

Run after changing a dataset or questions.yaml:  make eval-truth
"""

import json
from pathlib import Path

import duckdb
import yaml

HERE = Path(__file__).resolve().parent


def connect() -> duckdb.DuckDBPyConnection:
    con = duckdb.connect()
    for csv in sorted((HERE / "datasets").glob("*.csv")):
        con.execute(f'CREATE TABLE "{csv.stem}" AS SELECT * FROM read_csv_auto(?)', [str(csv)])
    return con


def rows(con, sql: str) -> list[list]:
    return [[_plain(v) for v in r] for r in con.execute(sql).fetchall()]


def _plain(v):
    return v if isinstance(v, (int, float, str, bool)) or v is None else str(v)


def build() -> dict:
    con = connect()
    truth = {}
    for q in yaml.safe_load((HERE / "questions.yaml").read_text())["questions"]:
        if q["kind"] == "narrative":
            truth[q["id"]] = {"facts": rows(con, q["facts_sql"])}
            continue
        result = rows(con, q["truth_sql"])
        if q["kind"] == "list":
            truth[q["id"]] = {"values": [str(r[0]) for r in result]}
        elif q["kind"] == "number":
            assert len(result) == 1 and result[0][0] is not None, q["id"]
            truth[q["id"]] = {"value": float(result[0][0])}
        else:
            assert len(result) == 1, q["id"]
            truth[q["id"]] = {"value": str(result[0][0])}
    return truth


if __name__ == "__main__":
    truth = build()
    (HERE / "ground_truth.json").write_text(json.dumps(truth, indent=1) + "\n")
    print(f"wrote {len(truth)} answers")
    for qid, t in truth.items():
        print(qid, str(t)[:110])
