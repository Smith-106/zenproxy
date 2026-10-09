#!/usr/bin/env bash
# Sweep Resin slots for a model on ONE protocol surface, reporting the
# upstream's own error type and its retry-after.
#
# This exists because "the model does not work" turned out to be three
# different statements, and only one of them is about the model's capability:
#
#   ModelProtocolUnsupported  this endpoint is the wrong one for this model
#   RegionError               this exit's country is blocked
#   FreeUsageLimitError       the model works here; the free allowance is spent
#
# The third is a success in disguise — it is the only one that says the request
# reached the model at all.
#
# Usage: sudo ./probe-responses-slots.sh <body.json> [path]

set -uo pipefail

BODY="${1:?usage: probe-responses-slots.sh <body.json> [path]}"
APATH="${2:-/responses}"
SLOTS="${SLOTS:-24}"
RESIN_HOST=127.0.0.1:2260
UA='opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14'

K=$(grep -E '^ZEN_KEY_01=' /opt/zenproxy/.env | cut -d= -f2-)
T=$(grep -E '^RESIN_PROXY_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)

MODEL="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["model"])' "$BODY")"
echo "model: $MODEL   path: $APATH"
printf '%-9s %-16s %-4s %-6s %-26s %s\n' SLOT EXIT_IP CC CODE ERR RETRY_AFTER
printf '%s\n' "--------------------------------------------------------------------------------"

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

for i in $(seq -w 1 "$SLOTS"); do
  slot="slot-${i}"
  ip="$(curl -sS --max-time 12 --proxy "socks5h://${RESIN_HOST}" \
        --proxy-user "Default.${slot}:${T}" https://api.ipify.org 2>/dev/null)"
  if [ -z "$ip" ]; then
    printf '%-9s %-16s %-4s %-6s %-26s %s\n' "$slot" DEAD - - - -
    continue
  fi
  cc="$(curl -sS --max-time 10 "http://ip-api.com/json/${ip}?fields=countryCode" 2>/dev/null |
        python3 -c 'import json,sys;print(json.load(sys.stdin).get("countryCode","??"))' 2>/dev/null)"
  h=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
  s=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
  code="$(curl -sS --max-time 45 -D "$WORK/h" -o "$WORK/b" -w '%{http_code}' \
    --proxy "socks5h://${RESIN_HOST}" --proxy-user "Default.${slot}:${T}" \
    -H "Authorization: Bearer ${K}" -H 'Content-Type: application/json' \
    -H "User-Agent: ${UA}" \
    -H 'x-opencode-client: cli' -H 'x-opencode-project: global' \
    -H "x-opencode-request: msg_${h}AAAAAAAAAAAAAA" \
    -H "x-opencode-session: ses_${s}AAAAAAAAAAAAAA" \
    -H "x-opencode-session-id: ses_${s}AAAAAAAAAAAAAA" \
    --data-binary "@${BODY}" \
    "https://opencode.ai/zen/v1${APATH}" 2>/dev/null)"
  [ -z "$code" ] && code=000
  err="-"
  if [ "$code" != "200" ]; then
    err="$(python3 -c '
import json,sys
try:
    j=json.load(open(sys.argv[1])); e=j.get("error",j)
    print(e.get("type") or e.get("code") or "-")
except Exception: print("?")' "$WORK/b" 2>/dev/null)"
  fi
  ra="$(grep -i '^retry-after:' "$WORK/h" | tr -d '\r' | awk '{print $2}')"
  printf '%-9s %-16s %-4s %-6s %-26s %s\n' "$slot" "$ip" "${cc:-??}" "$code" "$err" "${ra:--}"
done

echo
echo "FreeUsageLimitError = 这条路通了，只是额度用完了（看 RETRY_AFTER 秒数）。"
echo "ModelProtocolUnsupported = 这个端点确实不支持该模型。"
echo "RegionError = 该出口国家被限。"
