"""Generate the third eval dataset (support tickets). Deterministic.

retail_sales.csv and employees.csv are frozen copies of the UI samples, so the
eval set does not change if the samples are regenerated.

Planted story: Billing tickets triple in March 2025 (a pricing change), and chat
tickets take the longest to resolve. Some resolution/CSAT values are NULL on purpose.
"""

from pathlib import Path

import numpy as np
import pandas as pd

OUT = Path(__file__).resolve().parent / "datasets"
rng = np.random.default_rng(7)


def support_tickets() -> pd.DataFrame:
    channels = {"email": 1.0, "chat": 1.6, "phone": 0.8}  # resolution-time multiplier
    categories = ["Billing", "Login", "Shipping", "Bug report", "Feature request"]
    priorities = ["low", "medium", "high", "urgent"]
    rows, tid = [], 5000
    for day in pd.date_range("2025-01-01", "2025-06-30", freq="D"):
        for _ in range(rng.poisson(8)):
            category = categories[rng.integers(len(categories))]
            copies = 3 if category == "Billing" and day.month == 3 else 1
            for _ in range(copies):
                tid += 1
                channel = list(channels)[rng.integers(3)]
                priority = priorities[rng.choice(4, p=[0.35, 0.4, 0.17, 0.08])]
                first = round(float(rng.gamma(2.0, 1.5 if priority != "urgent" else 0.5)), 2)
                is_open = bool(rng.random() < 0.08)
                resolution = None if is_open else round(float(rng.gamma(3.0, 6.0) * channels[channel]), 2)
                csat = None if (is_open or rng.random() < 0.3) else int(rng.choice([1, 2, 3, 4, 5], p=[.08, .1, .17, .35, .3]))
                rows.append({
                    "ticket_id": tid, "created_at": day.date(), "channel": channel, "priority": priority,
                    "category": category, "first_response_hours": first, "resolution_hours": resolution,
                    "status": "open" if is_open else "closed", "csat": csat,
                })
    return pd.DataFrame(rows)


if __name__ == "__main__":
    df = support_tickets()
    df.to_csv(OUT / "support_tickets.csv", index=False)
    print(f"support_tickets.csv: {len(df)} rows")
