"""Seam 1: runner.py as a process. JSON request on stdin, JSON result on stdout."""

import json
import subprocess
import sys
from pathlib import Path

import duckdb
import pytest

RUNNER = Path(__file__).resolve().parent.parent / "runner.py"


def run(request: dict) -> dict:
    proc = subprocess.run(
        [sys.executable, str(RUNNER)],
        input=json.dumps(request),
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert proc.returncode == 0, proc.stderr
    return json.loads(proc.stdout)


@pytest.fixture
def dirs(tmp_path):
    data, out, scratch = tmp_path / "data", tmp_path / "out", tmp_path / "scratch"
    for d in (data, out, scratch):
        d.mkdir()
    db = data / "session.duckdb"
    con = duckdb.connect(str(db))
    con.execute(
        "CREATE TABLE sales AS SELECT * FROM (VALUES "
        "('north', 10), ('south', 20), ('north', 5)) t(region, amount)"
    )
    con.close()
    return {"db_path": str(db), "out_dir": str(out), "scratch_dir": str(scratch)}


def test_python_code_can_query_the_session_database(dirs):
    result = run({**dirs, "mode": "python",
                  "code": "print(con.execute('select sum(amount) from sales').fetchone()[0])"})
    assert result["error"] is None
    assert result["stdout"].strip() == "35"


def test_python_error_comes_back_as_a_traceback_of_agent_code_only(dirs):
    result = run({**dirs, "mode": "python", "code": "x = 1\ny = x / 0\n"})
    assert "ZeroDivisionError" in result["error"]
    assert 'File "<agent>", line 2' in result["error"]
    assert "runner.py" not in result["error"]


def test_open_matplotlib_figures_are_saved_as_charts(dirs):
    code = (
        "import matplotlib.pyplot as plt\n"
        "df = con.execute('select region, sum(amount) s from sales group by 1').df()\n"
        "plt.bar(df.region, df.s)\n"
    )
    result = run({**dirs, "mode": "python", "code": code})
    assert result["error"] is None
    assert len(result["files"]) == 1
    assert result["files"][0].endswith(".png")
    assert (Path(dirs["out_dir"]) / result["files"][0]).stat().st_size > 0


def test_explicitly_saved_charts_are_reported_once(dirs):
    code = (
        "import matplotlib.pyplot as plt\n"
        "plt.plot([1, 2, 3])\n"
        "plt.savefig(OUT + '/trend.png')\n"
    )
    result = run({**dirs, "mode": "python", "code": code})
    assert result["files"] == ["trend.png"]


def test_sql_returns_first_rows_and_the_total_count(dirs):
    result = run({**dirs, "mode": "sql", "max_rows": 2,
                  "code": "select * from range(5) t(n) order by n"})
    assert result["error"] is None
    assert result["columns"] == ["n"]
    assert result["rows"] == [[0], [1]]
    assert result["total_rows"] == 5
    assert result["truncated"] is True


def test_sql_values_are_json_safe(dirs):
    result = run({**dirs, "mode": "sql",
                  "code": "select date '2024-03-01' d, 1.50::decimal(5,2) p, null x"})
    assert result["rows"] == [["2024-03-01", 1.5, None]]


def test_sql_error_comes_back_as_text(dirs):
    result = run({**dirs, "mode": "sql", "code": "select nope from sales"})
    assert "nope" in result["error"]
    assert result["rows"] == []


def test_long_python_output_is_truncated_with_a_note(dirs):
    result = run({**dirs, "mode": "python", "max_output_chars": 100,
                  "code": "print('a' * 1000)"})
    assert result["truncated"] is True
    assert "[truncated 901 more characters]" in result["stdout"]


def test_scratch_files_carry_results_between_calls(dirs):
    first = run({**dirs, "mode": "python", "code":
                 "con.execute('select * from sales').df().to_parquet(SCRATCH + '/s.parquet')"})
    assert first["error"] is None
    second = run({**dirs, "mode": "sql",
                  "code": f"select count(*) from '{dirs['scratch_dir']}/s.parquet'"})
    assert second["rows"] == [[3]]


def test_session_database_is_read_only(dirs):
    result = run({**dirs, "mode": "sql", "code": "drop table sales"})
    assert "read-only" in result["error"].lower()


def test_non_finite_floats_become_null_so_the_output_stays_valid_json(dirs):
    proc = subprocess.run([sys.executable, str(RUNNER)], capture_output=True, text=True, input=json.dumps(
        {**dirs, "mode": "sql", "code": "select 'NaN'::double a, 'inf'::double b, 1.5::double c"}))
    assert "NaN" not in proc.stdout and "Infinity" not in proc.stdout
    assert json.loads(proc.stdout)["rows"] == [[None, None, 1.5]]


def test_a_locked_database_is_reported_not_replaced_by_an_empty_one(dirs):
    writer = duckdb.connect(dirs["db_path"])  # as Go holds it during an upload
    try:
        result = run({**dirs, "mode": "sql", "code": "select count(*) from sales"})
    finally:
        writer.close()
    assert "busy" in result["error"]
    assert "does not exist" not in result["error"]


def test_chart_saved_outside_out_and_closed_is_still_delivered(dirs):
    # Models often savefig() into /scratch or /tmp and close the figure.
    code = (
        "import matplotlib.pyplot as plt\n"
        "plt.plot([1, 2, 3])\n"
        "plt.savefig(SCRATCH + '/salary_by_dept.png')\n"
        "plt.close('all')\n"
    )
    result = run({**dirs, "mode": "python", "code": code})
    assert result["files"] == ["salary_by_dept.png"]
    assert (Path(dirs["out_dir"]) / "salary_by_dept.png").stat().st_size > 0


def test_pngs_that_already_existed_in_scratch_are_not_redelivered(dirs):
    (Path(dirs["scratch_dir"]) / "old.png").write_bytes(b"x")
    result = run({**dirs, "mode": "python", "code": "print(1)"})
    assert result["files"] == []
