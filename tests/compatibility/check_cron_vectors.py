#!/usr/bin/env python3
"""Verify checked-in DST vectors with the cronsim installed in the v4.4 image."""

import json
from datetime import datetime
from importlib.metadata import version
from pathlib import Path
from zoneinfo import ZoneInfo

from cronsim import CronSim


vectors = json.loads(Path(__file__).with_name("cron_vectors.json").read_text())
actual_version = version("cronsim")
expected_version = vectors["source"]["cronsim_version"]
if actual_version != expected_version:
    raise SystemExit(f"expected cronsim {expected_version}, found {actual_version}")

for case in vectors["cases"]:
    zone = ZoneInfo(case.get("timezone", vectors["timezone"]))
    start = datetime.fromisoformat(case["now"]).astimezone(zone)
    iterator = CronSim(case["expression"], start)
    actual = [next(iterator).isoformat() for _ in case["next"]]
    if actual != case["next"]:
        raise SystemExit(
            f"{case['name']}: expected {case['next']!r}, got {actual!r}"
        )
    print(f"PASS {case['name']}: {', '.join(actual)}")

print(f"Verified {len(vectors['cases'])} vectors with cronsim {actual_version}")
