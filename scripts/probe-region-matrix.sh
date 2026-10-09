#!/usr/bin/env bash
# Measure whether a region block is a property of the exit, or of the exit and
# the model together.
#
# This decides the shape of the fix. If a region block belongs to the exit alone,
# the right answer is a per-exit cooldown exactly like the quota ledger. If it
# belongs to the exit-and-model pair, a per-exit cooldown would be wrong in the
# expensive direction: it would take an exit out of rotation for every model
# because one model is unavailable there.
#
# It also has to separate the region wall from the tier gate, because on
# /responses the region check runs FIRST - a bare request there answers
# RegionError and never reaches the gate, so a region-blocked exit hides every
# other fact about the request shape.
#
# Usage: sudo ./probe-region-matrix.sh <model-a> <model-b> [exit ...]

set -uo pipefail

MODEL_A="${1:?usage: probe-region-matrix.sh <model-a> <model-b> [exit ...]}"
MODEL_B="${2:?}"
shift 2
EXITS=("$@")
[ ${#EXITS[@]} -eq 0 ] && EXITS=(slot-09 slot-10 slot-13)

K=$(grep -E '^ZEN_KEY_01=' /opt/zenproxy/.env | cut -d= -f2-)
T=$(grep -E '^RESIN_PROXY_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)

# One request, raw, with the full envelope. Prints "CODE<TAB>ERROR".
once() {
  local slot="$1" body="$2" path="$3" model="$4"
  local work h s
  work="$(mktemp -d)"
  h=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
  s=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
  local code
  code=$(curl -sS --max-time 40 -o "$work/b.txt" -w '%{http_code}' \
    --proxy socks5h://127.0.0.1:2260 --proxy-user "Default.${slot}:${T}" \
    -H "Authorization: Bearer ${K}" -H 'Content-Type: application/json' \
    -H 'User-Agent: opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14' \
    -H 'x-opencode-client: cli' -H 'x-opencode-project: global' \
    -H "x-opencode-request: msg_${h}AAAAAAAAAAAAAA" \
    -H "x-opencode-session: ses_${s}AAAAAAAAAAAAAA" \
    -H "x-opencode-session-id: ses_${s}AAAAAAAAAAAAAA" \
    --data-binary "@${body}" \
    "https://opencode.ai/zen/v1${path}" 2>/dev/null)
  local err
  err=$(python3 - "$work/b.txt" "$model" <<'PY'
import json, sys
try:
    j = json.load(open(sys.argv[1], encoding="utf-8", errors="replace"))
except Exception:
    print("-"); raise SystemExit
def dig(o):
    for k in ("error", "type"):
        if isinstance(o, dict) and k in o:
            return dig(o[k])
    if isinstance(o, str):
        return o
    return None
e = dig(j.get("error") or j)
print(e or "-")
PY
)
  rm -rf "$work"
  printf '%s\t%s' "$code" "$err"
}

printf '%-10s %-16s %-16s %s\n' "EXIT" "MODEL" "ENDPOINT" "CODE / ERROR"
echo "--------------------------------------------------------------------------------"
for slot in "${EXITS[@]}"; do
  for m in "$MODEL_A" "$MODEL_B"; do
    for spec in "/responses:resp" "/chat/completions:chat"; do
      path="${spec%%:*}"; kind="${spec##*:}"
      body="/tmp/rm-${m}-${kind}.json"
      [ -f "$body" ] || { echo "  缺 $body，先跑 gen-gate-bodies.mjs"; continue; }
      res=$(once "$slot" "$body" "$path" "$m")
      printf '%-10s %-16s %-16s %s\n' "$slot" "${m:0:15}" "$path" "${res/	/  }"
    done
  done
  echo
done

echo "RegionError    = 出口所在国家被限（换个出口可能就好）"
echo "FreeTierError  = 套壳没被接受（请求形状问题，跟出口无关）"
echo "FreeUsageLimit = 门禁过了，额度用尽"
echo "ModelProtocolUnsupported = 这个端点确实不接该模型"
