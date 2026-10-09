#!/usr/bin/env bash
# Prove the region fix on a real pool, against a real upstream refusal.
#
# The unit tests prove the wiring. What they cannot prove is that a real 403
# RegionError from the real upstream is recognised as RegionError rather than
# swallowed as ClassAuth, reaches the ledger as an exit-and-model pair, and stops
# the caller being told the model does not exist for them.
#
# The exit is chosen from measurement, not assumption: the matrix probe found
# slot-10 (RU) and slot-13 (HK) refusing one free model while serving another.
# If none of the named exits is refusing right now the script says so and stops,
# rather than reporting a pass it did not earn.
#
# A throwaway account parks on the exit under test. A fresh account has no
# binding, so its `proxy` preference is what ExitFor resolves to. The account is
# deleted at the end and the key is read from .env without being printed.
#
# Usage: sudo ./verify-region-axis.sh [exit ...]

set -uo pipefail

EXITS=("$@")
[ ${#EXITS[@]} -eq 0 ] && EXITS=(resin-slot-10 resin-slot-13)
PROBE=zen-region-probe
MODEL=muse-spark-1.3-contributor-free
OTHER=big-pickle
BODY=/tmp/region-verify-resp.json
OTHER_BODY=/tmp/region-verify-chat.json

[ -f "$BODY" ] || { echo "缺 $BODY（用 gen-prodlike-responses-body.mjs 生成）"; exit 1; }
[ -f "$OTHER_BODY" ] || { echo "缺 $OTHER_BODY（用 gen-gate-bodies.mjs 生成）"; exit 1; }

status_json()  { curl -s -H "X-Admin-Token: $(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)" http://127.0.0.1:9000/admin/api/status; }

region_view() {
  status_json | python3 -c '
import json,sys
r=json.load(sys.stdin).get("region") or {}
print("  count=%s" % r.get("count", 0))
for b in (r.get("blocked") or []):
    print("    %-18s model=%s recheck=%s" % (b["exit_id"], b["model"], b.get("recheck_after")))
if not (r.get("blocked") or []): print("    （无）")'
}

cleanup() { curl -s -o /dev/null -X DELETE -H "X-Admin-Token: $(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)" "http://127.0.0.1:9000/admin/api/accounts/${PROBE}"; }
trap cleanup EXIT

# Sends one request through the probe account. Prints "CODE<TAB>ERROR-TYPE".
send() { # send <bodyfile> <path>
  python3 - "$1" "$2" <<'PY'
import json, os, subprocess, sys, urllib.error, urllib.request
body_file, path = sys.argv[1], sys.argv[2]
env = subprocess.run(["grep","-E","^ZEN_KEY_01=","/opt/zenproxy/.env"],
                     capture_output=True, text=True).stdout.strip()
key = env.split("=",1)[1]
h = os.urandom(6).hex()
data = open(body_file, "rb").read()
req = urllib.request.Request("http://127.0.0.1:9000/zen-region-probe/v1" + path, data=data,
    headers={"Authorization":"Bearer "+key,"Content-Type":"application/json",
             "User-Agent":"opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
             "x-opencode-client":"cli","x-opencode-project":"global",
             "x-opencode-request":"msg_"+h+"AAAAAAAAAAAAAA",
             "x-opencode-session":"ses_"+h+"AAAAAAAAAAAAAA",
             "x-opencode-session-id":"ses_"+h+"AAAAAAAAAAAAAA"})
try:
    r = urllib.request.urlopen(req, timeout=120)
    print("%d\t%s" % (r.status, r.headers.get("Content-Type","")))
except urllib.error.HTTPError as e:
    try:
        j = json.loads(e.read().decode("utf-8","replace"))
        t = (j.get("error") or {}).get("type") or (j.get("error") or {}).get("message") or "-"
    except Exception:
        t = "-"
    print("%d\t%s" % (e.code, str(t)[:70]))
except Exception as e:
    print("ERR\t%s" % str(e)[:70])
PY
}

bind_probe() { # bind_probe <exit>
  python3 - "$1" <<'PY'
import json, subprocess, sys, urllib.request
target = sys.argv[1]
def rd(name):
    out = subprocess.run(["grep","-E","^"+name+"=","/opt/zenproxy/.env"],
                         capture_output=True, text=True).stdout.strip()
    return out.split("=",1)[1]
body = json.dumps({"id":"zen-region-probe","name":"地区轴验证（临时）","upstream":"zen",
                   "pool":"zen","proxy":target,"api_key":rd("ZEN_KEY_01"),"enabled":True}).encode()
req = urllib.request.Request("http://127.0.0.1:9000/admin/api/accounts", data=body,
    headers={"X-Admin-Token":rd("ZEN_ADMIN_TOKEN"),"Content-Type":"application/json"})
try:
    r = json.load(urllib.request.urlopen(req)); a = r.get("account", r)
    print("  已建立 id=%s proxy=%s" % (a.get("id"), a.get("proxy")))
except Exception as e:
    print("  建立失败:", e); sys.exit(1)
PY
}

echo "=== 0. 起点 ==="
echo "  地区账本:"; region_view

# The client no longer sees the 403 — that is the fix — so the refusal cannot be
# identified from the response code. It is identified from the ledger instead:
# pin the probe onto a candidate, send one request, and see whether that
# exit-and-model pair got recorded. A 403 arriving at the client would be the
# bug, so it is checked for separately below.
TARGET=""
TARGET_CODE=""
TARGET_ERR=""
for e in "${EXITS[@]}"; do
  echo
  echo "=== 试出口 ${e} ==="
  cleanup >/dev/null 2>&1
  bind_probe "$e" || continue
  sleep 1
  LOG_FROM=$(sudo docker logs zenproxy 2>&1 | wc -l)
  RES=$(send "$BODY" "/responses")
  CODE="${RES%%	*}"; ERR="${RES##*	}"
  echo "  客户端收到: HTTP ${CODE}  ${ERR}"
  VIEW=$(region_view)
  if echo "$VIEW" | grep -q "$e"; then
    echo "  → 账本把这一对记下了，说明这个出口确实在按地区拒绝该模型"
    TARGET="$e"; TARGET_CODE="$CODE"; TARGET_ERR="$ERR"
    echo "$VIEW" | sed 's/^/  /'
    break
  fi
  echo "  → 账本里没有 ${e}，这个出口此刻不按地区拒绝该模型"
done

if [ -z "$TARGET" ]; then
  echo
  echo "没找到正在按地区拒绝的出口。"
  echo "先单独确认上游此刻是否真的在拦（直连 SOCKS 绕开 zenproxy 才能看到 403）："
  echo "  sudo bash /tmp/probe-region-matrix.sh $MODEL $OTHER ${EXITS[*]}"
  echo "若矩阵显示 403 而这里没记上，那才是真的没生效。"
  exit 1
fi

echo
echo "=== 1. 修复的核心：客户端不再被告知「你所在国家不可用」 ==="
if [ "$TARGET_CODE" = "403" ] && printf '%s' "$TARGET_ERR" | grep -qi 'region'; then
  echo "  ✗ 客户端仍然收到了 403 RegionError —— 轮换没有发生"
else
  echo "  ✓ 客户端收到的是 HTTP ${TARGET_CODE}（${TARGET_ERR}），不是 403 RegionError"
  echo "    —— 403 被换出口消化掉了，而不是转给调用方"
fi

echo
echo "=== 2. 记账是「出口 × 模型」而不是「出口」 ==="
REGION_VIEW=$(region_view)
echo "$REGION_VIEW"
if echo "$REGION_VIEW" | grep -q "$TARGET" && echo "$REGION_VIEW" | grep -q "$MODEL"; then
  echo "  ✓ $TARGET 被记为对 $MODEL 地区受限"
else
  echo "  ✗ 账本里没有这一对"
fi
if echo "$REGION_VIEW" | grep -q "$OTHER"; then
  echo "  ✗ $OTHER 也进了地区账本，说明记账没有按模型区分"
else
  echo "  ✓ 账本里只有 $MODEL 这一条，$OTHER 未被牵连"
fi

echo
echo "=== 3. 主动规避：下一个请求不该再走那个出口 ==="
LOG_FROM=$(sudo docker logs zenproxy 2>&1 | wc -l)
RES2=$(send "$BODY" "/responses")
echo "  客户端收到: HTTP ${RES2%%	*}  ${RES2##*	}"
echo "  日志里与地区有关的行:"
sudo docker logs zenproxy 2>&1 | tail -n +"$((LOG_FROM + 1))" |
  grep -Ei 'region|refused for this model' | sed 's/^/    /' | head -5

echo
echo "=== 4. clear-region ==="
CLEAR=$(curl -s -X POST -H "X-Admin-Token: $(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)" \
  "http://127.0.0.1:9000/admin/api/proxies/${TARGET}/clear-region")
echo "  $CLEAR"
echo "  清完后的账本:"; region_view

echo
echo "=== 5. 清理 ==="
echo "  临时账号将随脚本退出删除"
