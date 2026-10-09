#!/usr/bin/env bash
# Probe every Resin slot for one model, on two protocol surfaces at once.
#
# Why: a model can be refused for three different reasons, and they are not
# interchangeable. Only one of them is worth retrying elsewhere.
#
#   RegionError              this exit's country is blocked -> another exit may fix it
#   FreeTierError            the request was not enveloped   -> the envelope is wrong
#   ModelProtocolUnsupported this endpoint is wrong for this model -> try the other one
#
# The third is why this script sends the SAME enveloped body to both
# /chat/completions and /responses: the question "is this model only reachable
# through the other protocol" cannot be answered by testing one at a time.
#
# Runs on the deployment host. Talks to Resin directly (127.0.0.1:2260) so that
# nothing about the running zenproxy is touched, and reads the account key from
# the env file without echoing it.
#
# Usage: sudo ./probe-slots-by-protocol.sh <chat.json> <resp.json>

set -uo pipefail

CHAT_BODY="${1:?usage: probe-slots-by-protocol.sh <chat.json> <resp.json>}"
RESP_BODY="${2:?usage: probe-slots-by-protocol.sh <chat.json> <resp.json>}"

ENV_FILE=/opt/zenproxy/.env
RESIN_HOST=127.0.0.1:2260
UPSTREAM=https://opencode.ai/zen/v1
SLOTS="${SLOTS:-24}"
UA='opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14'

# Never echo these. The script only ever passes them to curl.
ZEN_KEY="$(grep -E '^ZEN_KEY_01=' "$ENV_FILE" | cut -d= -f2-)"
RESIN_TOKEN="$(grep -E '^RESIN_PROXY_TOKEN=' "$ENV_FILE" | cut -d= -f2-)"
if [ -z "$ZEN_KEY" ] || [ -z "$RESIN_TOKEN" ]; then
  echo "could not read ZEN_KEY_01 / RESIN_PROXY_TOKEN from $ENV_FILE" >&2
  exit 1
fi

MODEL="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["model"])' "$CHAT_BODY")"
echo "model: $MODEL"
printf '%-9s %-16s %-4s %-22s %s\n' SLOT EXIT_IP CC CHAT RESPONSES
printf '%s\n' "---------------------------------------------------------------------------"

rand_id() {
  # msg_/ses_ + 12 lowercase hex + 14 base62, the shape the gate requires.
  printf '%s%s%s' "$1" \
    "$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
    "$(head -c 14 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 14)"
}

probe() {
  # $1 slot, $2 body file, $3 path -> "status errorType"
  local slot="$1" body="$2" path="$3" sess req
  sess="$(rand_id ses_)"; req="$(rand_id msg_)"
  local out
  out="$(curl -sS --max-time 45 \
    --proxy "socks5h://${RESIN_HOST}" --proxy-user "Default.${slot}:${RESIN_TOKEN}" \
    -o /tmp/zenprobe-out.$$ -w '%{http_code}' \
    -H "Authorization: Bearer ${ZEN_KEY}" \
    -H "Content-Type: application/json" \
    -H "User-Agent: ${UA}" \
    -H 'x-opencode-client: cli' \
    -H 'x-opencode-project: global' \
    -H "x-opencode-request: ${req}" \
    -H "x-opencode-session: ${sess}" \
    -H "x-opencode-session-id: ${sess}" \
    --data-binary "@${body}" \
    "${UPSTREAM}${path}" 2>/dev/null)"
  local code="${out:-000}"
  local kind="-"
  if [ "$code" != "200" ]; then
    kind="$(python3 -c '
import json,sys
try:
    j=json.load(open("/tmp/zenprobe-out.'"$$"'"))
    e=j.get("error",j)
    print(e.get("type") or e.get("code") or "-")
except Exception:
    print("?")
' 2>/dev/null)"
  fi
  rm -f /tmp/zenprobe-out.$$
  printf '%s %s' "$code" "$kind"
}

for i in $(seq -w 1 "$SLOTS"); do
  slot="slot-${i}"
  # A slot whose exit is dead answers nothing; skip it cheaply rather than
  # spending 45s twice on it.
  ip="$(curl -sS --max-time 12 --proxy "socks5h://${RESIN_HOST}" \
        --proxy-user "Default.${slot}:${RESIN_TOKEN}" \
        https://api.ipify.org 2>/dev/null)"
  if [ -z "$ip" ]; then
    printf '%-9s %-16s %-4s %-22s %s\n' "$slot" "DEAD" "-" "-" "-"
    continue
  fi
  cc="$(curl -sS --max-time 10 "http://ip-api.com/json/${ip}?fields=countryCode" 2>/dev/null |
        python3 -c 'import json,sys;print(json.load(sys.stdin).get("countryCode","??"))' 2>/dev/null)"
  printf '%-9s %-16s %-4s %-22s %s\n' "$slot" "$ip" "${cc:-??}" "$(probe "$slot" "$CHAT_BODY" /chat/completions)" "$(probe "$slot" "$RESP_BODY" /responses)"
done

echo
echo "读法：CHAT 列出现 400 ModelProtocolUnsupported 说明这个出口已经过了地区与闸门，"
echo "      只是这条路不通 —— 这时才轮到看 RESPONSES 列。"
echo "      两列都是 403 RegionError = 这个出口的国家被限，跟模型能力无关。"
