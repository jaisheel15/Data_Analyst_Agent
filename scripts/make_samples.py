"""Generate the sample datasets offered in the UI. Deterministic: same output every run.

Built-in story for demos: West revenue drops sharply in Q3 2025 because the
"Trail Pack" product stalls there; Engineering attrition is highest.

Run: uv run --project sandbox python scripts/make_samples.py
"""

from pathlib import Path

import numpy as np
import pandas as pd

OUT = Path(__file__).resolve().parent.parent / "web" / "public" / "samples"
rng = np.random.default_rng(42)


def retail_sales() -> pd.DataFrame:
    days = pd.date_range("2024-01-01", "2025-12-31", freq="D")
    products = {"Trail Pack": 89.0, "City Tote": 49.0, "Rain Shell": 129.0, "Wool Socks": 14.0, "Day Bag": 64.0}
    regions = ["North", "South", "East", "West"]
    rows = []
    order_id = 10_000
    for day in days:
        for _ in range(rng.poisson(3)):
            region = regions[rng.integers(len(regions))]
            product = list(products)[rng.integers(len(products))]
            units = int(rng.integers(1, 6))
            if region == "West" and product == "Trail Pack" and day.year == 2025 and day.quarter == 3:
                if rng.random() < 0.85:  # the Q3 2025 stall
                    continue
            discount = float(rng.choice([0, 0, 0, 0.1, 0.2]))
            price = products[product]
            order_id += 1
            rows.append({
                "order_id": order_id, "order_date": day.date(), "region": region, "product": product,
                "units": units, "unit_price": price, "discount": discount,
                "revenue": round(units * price * (1 - discount), 2),
            })
    return pd.DataFrame(rows)


def employees() -> pd.DataFrame:
    depts = {"Engineering": 120_000, "Sales": 80_000, "Support": 55_000, "Marketing": 75_000, "Finance": 90_000}
    rows = []
    for i in range(300):
        dept = list(depts)[rng.integers(len(depts))]
        hired = pd.Timestamp("2016-01-01") + pd.Timedelta(days=int(rng.integers(0, 3300)))
        perf = int(rng.choice([1, 2, 3, 3, 4, 4, 5]))
        quit_p = {"Engineering": 0.30, "Sales": 0.22, "Support": 0.18, "Marketing": 0.15, "Finance": 0.10}[dept]
        rows.append({
            "employee_id": 1000 + i, "department": dept, "hire_date": hired.date(),
            "salary": int(depts[dept] * rng.normal(1, 0.15)), "performance_rating": perf,
            "remote": bool(rng.random() < 0.4), "left_company": bool(rng.random() < quit_p),
        })
    return pd.DataFrame(rows)


if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    for name, df in {"retail_sales": retail_sales(), "employees": employees()}.items():
        df.to_csv(OUT / f"{name}.csv", index=False)
        print(f"{name}.csv: {len(df)} rows")
