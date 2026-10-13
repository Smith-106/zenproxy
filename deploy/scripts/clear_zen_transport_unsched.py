#!/usr/bin/env python3
"""Keep zen-01/02/03 permanently schedulable.

sub2api 0.2.x parks accounts for 10 minutes on transport faults it classifies
as "durable" (connection refused, etc.). For the Opencode free pool the
upstream (zenproxy → exit IPs) is often merely unstable, not dead — parking
the whole shared zen account set makes the pool unusable.

Policy: every tick, clear ANY active temp_unschedulable window on zen-01/02/03.
Request-level failover still happens; we only refuse to leave the accounts
out of the scheduler for minutes.
"""

from __future__ import annotations

import subprocess
import sys


NAMES = ("zen-01", "zen-02", "zen-03")


def log(msg: str) -> None:
    try:
        subprocess.run(
            ["logger", "-t", "clear-zen-transport-unsched", msg],
            check=False,
            capture_output=True,
        )
    except OSError:
        pass
    print(msg, flush=True)


def psql(sql: str) -> str:
    proc = subprocess.run(
        [
            "sudo",
            "docker",
            "exec",
            "-i",
            "sub2api-postgres",
            "psql",
            "-U",
            "sub2api",
            "-d",
            "sub2api",
            "-v",
            "ON_ERROR_STOP=1",
            "-At",
            "-c",
            sql,
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return proc.stdout.strip()


def main() -> int:
    names_sql = ",".join(f"'{n}'" for n in NAMES)
    where = f"""
      deleted_at IS NULL
      AND name IN ({names_sql})
      AND temp_unschedulable_until IS NOT NULL
      AND temp_unschedulable_until > NOW()
    """
    rows = psql(
        "SELECT name || '|' || coalesce(left(temp_unschedulable_reason, 160), '') "
        f"FROM accounts WHERE {where} ORDER BY name;"
    )
    if not rows:
        return 0

    count = psql(f"SELECT count(*) FROM accounts WHERE {where};")
    psql(
        "UPDATE accounts SET "
        "temp_unschedulable_until = NULL, "
        "temp_unschedulable_reason = NULL, "
        "updated_at = NOW() "
        f"WHERE {where};"
    )
    summary = " ; ".join(rows.splitlines())
    log(f"forced-clear temp-unschedulable on {count} zen account(s): {summary}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except subprocess.CalledProcessError as exc:
        err = (exc.stderr or "")[-300:]
        log(f"psql failed: {err or exc}")
        raise SystemExit(1)
