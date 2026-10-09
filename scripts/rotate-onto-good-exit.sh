#!/usr/bin/env bash
# Rotate an account until its exit lands on an IP that is known to pass the
# region check for the model under test.
#
# Region and protocol are separate walls, and a region-blocked exit hides
# everything behind it: the model answers RegionError whatever the request looks
# like, so an end-to-end test run from there proves nothing about the envelope.
#
# Usage: sudo ./rotate-onto-good-exit.sh <account> <ip> [ip ...]

set -uo pipefail

ACCT="${1:?usage: rotate-onto-good-exit.sh <account> <ip> [ip ...]}"; shift
GOOD=" $* "
TOKEN=$(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)
ADMIN=http://127.0.0.1:9000/admin/api

exit_ip() {
  curl -s -H "X-Admin-Token: ${TOKEN}" "$ADMIN/accounts" |
    python3 -c '
import json,sys
want = "'"${ACCT}"'"
j = json.load(sys.stdin)
for a in (j.get("accounts") or j.get("items") or []):
    if a.get("id") == want:
        print(a.get("bound_exit_ip") or "-", a.get("bound_exit") or "-")
        break
else:
    print("- -")
'
}

echo "目标出口: ${GOOD# }"
# A freshly bound exit has no IP until something probes it, so a slot that just
# tested clean can still read as "-". Matching on the exit id as well as the IP
# is what keeps those slots reachable.
matches() { case "$GOOD" in *" $1 "*) return 0;; *" $2 "*) return 0;; *) return 1;; esac; }

BEFORE="$(exit_ip)"
echo "当前: $BEFORE"
if matches "${BEFORE%% *}" "${BEFORE##* }"; then echo "已经落在目标出口上"; exit 0; fi

for i in $(seq 1 24); do
  curl -s -o /dev/null -X POST -H "X-Admin-Token: ${TOKEN}" "$ADMIN/accounts/${ACCT}/rotate"
  sleep 1
  NOW="$(exit_ip)"
  echo "  第 $i 次 rotate -> $NOW"
  if matches "${NOW%% *}" "${NOW##* }"; then echo "命中"; exit 0; fi
done

echo "24 次都没命中目标出口；当前 $NOW" >&2
exit 1
