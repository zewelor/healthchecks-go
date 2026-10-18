#!/usr/bin/env python3
"""Probe Healthchecks v4.4 and optionally compare a local candidate service."""

import argparse
import http.client
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parent
FIXTURES = json.loads((ROOT / "api_cases.json").read_text())
CHECK = FIXTURES["check_uuid"]
UNKNOWN = FIXTURES["unknown_uuid"]
RID_A = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
RID_B = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
RID_C = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
STATE_CHECK = "22222222-2222-4222-8222-222222222222"


def parse_local_http_url(value: str, label: str) -> tuple[str, int, str]:
    parsed = urlsplit(value)
    if parsed.scheme != "http" or parsed.hostname not in {"localhost", "127.0.0.1", "::1"}:
        raise ValueError(f"{label} must be an http URL on loopback")
    if parsed.path not in {"", "/"} or parsed.query or parsed.fragment:
        raise ValueError(f"{label} must be a bare base URL without a path or query")
    return parsed.hostname, parsed.port or 80, parsed.netloc


def request(base_url: str, method: str, path: str) -> dict[str, object]:
    host, port, _ = parse_local_http_url(base_url, "service URL")
    connection = http.client.HTTPConnection(host, port, timeout=5)
    body = b"compatibility probe\n" if method == "POST" else None
    headers = {"Content-Type": "text/plain"} if body is not None else {}
    connection.request(method, path, body=body, headers=headers)
    response = connection.getresponse()
    response_body = response.read().decode("utf-8", errors="replace")
    result = {
        "status": response.status,
        "body": response_body,
        "headers": {key.lower(): value for key, value in response.getheaders()},
    }
    connection.close()
    return result


def expanded_cases() -> list[dict[str, object]]:
    result = []
    for case in FIXTURES["cases"]:
        path = case["path"].format(check=CHECK, unknown=UNKNOWN)
        for method in case["methods"]:
            expected_body = "" if method == "HEAD" else case.get("body")
            result.append(
                {
                    "name": f"{case['name']}:{method}",
                    "path": path,
                    "method": method,
                    "status": case["status"],
                    "body": expected_body,
                }
            )
    return result


def check_case(base_url: str, target: str, case: dict[str, object], results: list) -> list[str]:
    actual = request(base_url, str(case["method"]), str(case["path"]))
    errors = []
    if actual["status"] != case["status"]:
        errors.append(f"expected HTTP {case['status']}, got {actual['status']}")
    if case["body"] is not None and actual["body"] != case["body"]:
        errors.append(f"expected body {case['body']!r}, got {actual['body']!r}")
    results.append({"target": target, "case": case, "actual": actual, "errors": errors})
    status = "PASS" if not errors else "FAIL"
    print(f"{status} {target} {case['name']} -> HTTP {actual['status']}")
    if errors:
        print(f"  {case['method']} {case['path']}: {'; '.join(errors)}")
    return errors


STATE_QUERY = r'''
import json
from hc.api.models import Check
c = Check.objects.get(code="22222222-2222-4222-8222-222222222222")
p = c.ping_set.order_by("-n").first()
print(json.dumps({
    "status": c.status,
    "last_ping": c.last_ping.isoformat() if c.last_ping else None,
    "last_start": c.last_start.isoformat() if c.last_start else None,
    "last_start_rid": str(c.last_start_rid) if c.last_start_rid else None,
    "last_duration": c.last_duration.total_seconds() if c.last_duration is not None else None,
    "n_pings": c.n_pings,
    "latest_ping_kind": p.kind if p else None,
    "latest_ping_rid": str(p.rid) if p and p.rid else None,
}))
'''


def oracle_state(container: str) -> dict[str, object]:
    output = subprocess.check_output(
        ["docker", "exec", container, "./manage.py", "shell", "-c", STATE_QUERY],
        text=True,
    )
    for line in reversed(output.splitlines()):
        if line.startswith("{"):
            return json.loads(line)
    raise RuntimeError(f"could not find state JSON in manage.py output: {output!r}")


def check_state_oracle(base_url: str, container: str) -> list[str]:
    errors = []

    def expect(label: str, condition: bool, state: dict[str, object], detail: str) -> None:
        status = "PASS" if condition else "FAIL"
        print(f"{status} upstream-state {label}: {detail}")
        if not condition:
            errors.append(f"{label}: {state!r}")

    initial = oracle_state(container)
    expect(
        "new-check",
        initial["status"] == "new" and initial["n_pings"] == 0,
        initial,
        f"status={initial['status']}, pings={initial['n_pings']}",
    )

    response = request(base_url, "GET", f"/ping/{STATE_CHECK}/start?rid={RID_A}")
    started = oracle_state(container)
    expect(
        "start-keeps-new-status-and-no-last-ping",
        response["status"] == 200
        and started["status"] == "new"
        and started["last_ping"] is None
        and started["last_start"] is not None
        and started["last_start_rid"] == RID_A
        and started["latest_ping_kind"] == "start",
        started,
        f"status={started['status']}, last_ping={started['last_ping']}, start_rid={started['last_start_rid']}",
    )

    request(base_url, "POST", f"/ping/{STATE_CHECK}?rid={RID_B}")
    mismatch = oracle_state(container)
    expect(
        "success-with-mismatched-rid-leaves-start-open",
        mismatch["status"] == "up"
        and mismatch["last_start"] is not None
        and mismatch["last_start_rid"] == RID_A
        and mismatch["last_duration"] is None,
        mismatch,
        f"status={mismatch['status']}, start_rid={mismatch['last_start_rid']}, duration={mismatch['last_duration']}",
    )

    request(base_url, "GET", f"/ping/{STATE_CHECK}/fail?rid={RID_B}")
    failed = oracle_state(container)
    expect(
        "failure-clears-mismatched-start",
        failed["status"] == "down" and failed["last_start"] is None,
        failed,
        f"status={failed['status']}, last_start={failed['last_start']}",
    )

    request(base_url, "GET", f"/ping/{STATE_CHECK}/start?rid={RID_C}")
    request(base_url, "GET", f"/ping/{STATE_CHECK}?rid={RID_C}")
    matched = oracle_state(container)
    expect(
        "matching-rid-closes-start-and-records-duration",
        matched["status"] == "up"
        and matched["last_start"] is None
        and matched["last_duration"] is not None,
        matched,
        f"status={matched['status']}, last_start={matched['last_start']}, duration={matched['last_duration']}",
    )

    before_log = matched
    log_response = request(base_url, "POST", f"/ping/{STATE_CHECK}/log")
    after_log = oracle_state(container)
    expect(
        "log-does-not-change-check-state",
        log_response["status"] == 200
        and after_log["status"] == before_log["status"]
        and after_log["last_ping"] == before_log["last_ping"]
        and after_log["last_start"] == before_log["last_start"]
        and after_log["n_pings"] == before_log["n_pings"] + 1
        and after_log["latest_ping_kind"] == "log",
        after_log,
        f"status={after_log['status']}, last_ping unchanged={after_log['last_ping'] == before_log['last_ping']}, count={after_log['n_pings']}",
    )

    invalid_rid = request(base_url, "GET", f"/ping/{STATE_CHECK}/log?rid=not-a-uuid")
    after_invalid_rid = oracle_state(container)
    expect(
        "invalid-rid-is-rejected-before-recording-ping",
        invalid_rid["status"] == 400
        and after_invalid_rid["n_pings"] == after_log["n_pings"],
        after_invalid_rid,
        f"HTTP {invalid_rid['status']}, count={after_invalid_rid['n_pings']}",
    )

    return errors


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True, help="isolated upstream loopback base URL")
    parser.add_argument("--candidate-url", help="optional loopback candidate base URL")
    parser.add_argument("--container", required=True, help="temporary upstream Docker container name")
    parser.add_argument("--report", type=Path, help="optional JSON result artifact path")
    args = parser.parse_args()
    parse_local_http_url(args.base_url, "upstream URL")
    if args.candidate_url:
        parse_local_http_url(args.candidate_url, "candidate URL")

    errors = []
    results = []
    cases = expanded_cases()
    for case in cases:
        errors.extend(check_case(args.base_url, "upstream", case, results))
        if args.candidate_url:
            errors.extend(check_case(args.candidate_url, "candidate", case, results))

    state_errors = check_state_oracle(args.base_url, args.container)
    errors.extend(state_errors)
    if args.report:
        args.report.write_text(json.dumps({
            "generated_at": datetime.now(timezone.utc).isoformat(),
            "upstream_version": "4.4",
            "http_cases": len(cases),
            "results": results,
            "upstream_state_assertions": 7,
            "upstream_state_errors": state_errors,
            "passed": not errors,
        }, indent=2) + "\n")
        print(f"JSON report: {args.report}")
    print(f"Checked {len(cases)} HTTP cases and 7 upstream state assertions")
    if errors:
        print("Compatibility failures:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1
    print("Compatibility oracle passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
