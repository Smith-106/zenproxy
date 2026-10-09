#!/usr/bin/env bash
# One raw, fully-visible request: the point is to read the upstream's own words
# (and its rate-limit headers), not to summarise them.
#
# Usage: sudo ./raw-one.sh <slot> <body.json> <path> [model]
set -u

SLOT="${1:?slot}"; BODY="${2:?body}"; APATH="${3:?path}"
K=$(grep -E '^ZEN_KEY_01=' /opt/zenproxy/.env | cut -d= -f2-)
T=$(grep -E '^RESIN_PROXY_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)
H=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
S=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')

echo "slot=$SLOT path=$APATH"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
CODE=$(curl -sS --max-time 40 -D "$WORK/hdr.txt" -o "$WORK/body.txt" -w '%{http_code}' \
  --proxy socks5h://127.0.0.1:2260 --proxy-user "Default.${SLOT}:${T}" \
  -H "Authorization: Bearer ${K}" -H 'Content-Type: application/json' \
  -H 'User-Agent: opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14' \
  -H 'x-opencode-client: cli' -H 'x-opencode-project: global' \
  -H "x-opencode-request: msg_${H}AAAAAAAAAAAAAA" \
  -H "x-opencode-session: ses_${S}AAAAAAAAAAAAAA" \
  -H "x-opencode-session-id: ses_${S}AAAAAAAAAAAAAA" \
  --data-binary "@${BODY}" \
  "https://opencode.ai/zen/v1${APATH}")
echo "http_code=${CODE} exit=$?"
echo "--- body (first 900 bytes) ---"
head -c 900 "$WORK/body.txt"
# A streamed 200 is long and its answer is at the end: the head only proves the
# envelope landed, the tail is where the model actually says something.
SIZE=$(wc -c < "$WORK/body.txt")
if [ "$SIZE" -gt 900 ]; then
  echo
  echo "--- body (last 1400 of ${SIZE} bytes) ---"
  tail -c 1400 "$WORK/body.txt"
fi
echo
echo "--- headers of interest ---"
grep -iE 'retry-after|ratelimit|^HTTP|x-request' "$WORK/hdr.txt" | head -20
