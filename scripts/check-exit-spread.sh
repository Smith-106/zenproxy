#!/usr/bin/env bash
# Report whether the enabled accounts are actually spread across the exit pool.
#
# Two accounts sharing an exit are not two chances at the upstream: they share
# that IP's allowance and its region refusals, so a failure rotation was meant to
# absorb instead lands on both at once. The redundancy the pool exists to provide
# is gone, and nothing says so -- /admin/api/status reports healthy accounts and a
# healthy pool either way, because both statements are true.
#
# This mattered enough to fix twice over: the replacement exit used to be chosen
# without regard for who else was on it, and an account only re-resolves its exit
# exactly when its old one goes bad, which is when a collision is most likely.
#
# Sampling matters. Bindings drift on their own as nodes die, so one snapshot can
# show a clean pool that collides a minute later, and a single snapshot showing a
# collision may be gone by the time anyone looks. Default is several samples
# spread over time; report a collision if any sample has one.
#
# Read-only: this never rotates, binds, or revives anything.
#
# Usage: sudo ./check-exit-spread.sh [-n samples] [-i seconds]

set -uo pipefail

SAMPLES=5
INTERVAL=6

while [ $# -gt 0 ]; do
  case "$1" in
    -n) SAMPLES="$2"; shift 2 ;;
    -i) INTERVAL="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

TOKEN=$(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)
ADMIN=http://127.0.0.1:9000/admin/api

if [ -z "${TOKEN}" ]; then
  echo "could not read ZEN_ADMIN_TOKEN from /opt/zenproxy/.env (run as root)" >&2
  exit 2
fi

python3 - "$SAMPLES" "$INTERVAL" "$TOKEN" "$ADMIN" <<'PY'
import json
import sys
import time
import urllib.request

samples, interval, token, admin = int(sys.argv[1]), float(sys.argv[2]), sys.argv[3], sys.argv[4]


def get(path):
    req = urllib.request.Request(admin + path, headers={"X-Admin-Token": token})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r)


def bindings():
    return {a["id"]: (a.get("bound_exit") or "") for a in get("/accounts")["accounts"]}


def exits():
    return get("/proxies")["exits"]


def clashes(m):
    seen = {}
    for acct, exit in m.items():
        seen.setdefault(exit, []).append(acct)
    return [(e, a) for e, a in seen.items() if len(a) > 1]


print("== 采样 %d 次，间隔 %.0f 秒 ==" % (samples, interval))
print()

worst = {}
for i in range(samples):
    m = bindings()
    hit = clashes(m)
    shown = {k: v.replace("resin-", "") for k, v in m.items()}
    print("  %2d  %-46s %s" % (i + 1, shown, "冲突: %s" % hit if hit else "无冲突"))
    for exit_id, accts in hit:
        worst[exit_id] = accts
    if i + 1 < samples:
        time.sleep(interval)

print()

# The pool's own view of who occupies what. Bind only ever added, so an account
# that moved used to leave a stale occupant behind, and a stale entry is
# indistinguishable from a live one.
pool = exits()
recorded = {}
for p in pool:
    for a in p.get("bound_accounts") or []:
        recorded[a] = p["id"]
m = bindings()
stale = [(a, recorded[a], m.get(a)) for a in recorded if m.get(a) != recorded[a]]

print("== 池子 ==")
healthy = sum(1 for p in pool if p.get("healthy"))
retired = sum(1 for p in pool if p.get("retired"))
cooled = sum(1 for p in pool if p.get("quota_until"))
blocked = sum(1 for p in pool if p.get("region_blocked_for"))
print("  共 %d 个出口：健康 %d，退休 %d，额度冷却 %d，地区封锁 %d" % (
    len(pool), healthy, retired, cooled, blocked))
print("  占用记录 %d 条 / 账号 %d 个" % (len(recorded), len(m)))

if stale:
    print()
    print("== ⚠ 幽灵占用（记录的占用 != 权威绑定）==")
    for acct, claims, actual in stale:
        print("  %s 仍声称占用 %s，实际绑定 %s" % (acct, claims, actual or "（无）"))

print()
if worst:
    print("结论：🔴 有账号共用出口，冗余是假的")
    for exit_id, accts in worst.items():
        print("  %s 被 %s 共用" % (exit_id, " 和 ".join(accts)))
    print()
    print("  轮换其中一个即可分开（新逻辑会避开被占用的出口）：")
    for exit_id, accts in worst.items():
        print("    curl -X POST -H \"X-Admin-Token: $ZEN_ADMIN_TOKEN\" %s/accounts/%s/rotate" % (admin, accts[-1]))
    sys.exit(1)

if stale:
    print("结论：🟡 无冲突，但有幽灵占用记录（重启即清，或等它被覆盖）")
    sys.exit(1)

print("结论：🟢 所有账号落在不同出口，且无幽灵占用记录")
PY

exit $?
