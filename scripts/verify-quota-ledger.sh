#!/usr/bin/env bash
# Prove the quota ledger changes what the NEXT request does, on a real upstream.
#
# The unit tests prove the pieces. What they cannot prove is that a real
# Retry-After reaches the ledger and then alters behaviour — which is the whole
# claim.
#
# Why a throwaway account: to demonstrate "move off the cooled exit before
# sending", the account has to actually be sitting on a cooled exit. Rotating
# onto one cannot work, and that is the design working rather than a bug — Pick
# demotes cooled exits below healthy ones, so rotation will never choose one
# while a healthy exit exists (measured: 30 rotations, no hit). The only way an
# account ends up parked on a cooled exit in production is that the exit was
# cooled after it was bound, usually by another account's request.
#
# A fresh account has no binding, so its `proxy` preference is what ExitFor
# resolves to. That parks it on the cooled exit deterministically, without
# touching the accounts that carry real traffic. The account is deleted at the
# end, and the API key is read from .env and never printed.
#
# Usage: sudo ./verify-quota-ledger.sh [cooled-exit]

set -uo pipefail

TOKEN=$(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)
ADMIN=http://127.0.0.1:9000/admin/api
PROBE=zen-quota-probe

quota_json() { curl -s -H "X-Admin-Token: ${TOKEN}" "$ADMIN/status"; }

cooled_ids() {
  quota_json | python3 -c '
import json,sys
print(" ".join(c["exit_id"] for c in json.load(sys.stdin)["quota"]["cooled"]))'
}

quota() {
  quota_json | python3 -c '
import json,sys
q=json.load(sys.stdin)["quota"]
print("  count=%s / total=%s" % (q["count"], q["total"]))
for c in q["cooled"]:
    print("    %-18s until=%s after=%s" % (c["exit_id"], c["until"], c.get("after")))
if not q["cooled"]: print("    （无）")'
}

probe_exit() {
  curl -s -H "X-Admin-Token: ${TOKEN}" "$ADMIN/accounts" | python3 -c '
import json,sys
j=json.load(sys.stdin)
for a in (j.get("accounts") or j.get("items") or []):
    if a.get("id")=="'"${PROBE}"'": print(a.get("bound_exit") or "-"); break
else: print("-")'
}

cleanup() { curl -s -o /dev/null -X DELETE -H "X-Admin-Token: ${TOKEN}" "$ADMIN/accounts/${PROBE}"; }
trap cleanup EXIT

echo "=== 0. 起点 ==="
echo "  账本:"; quota

COOLED="$(cooled_ids)"
TARGET="${1:-$(echo "$COOLED" | awk '{print $1}')}"
if [ -z "$TARGET" ]; then
  echo "  账本是空的：先发几个请求让它记上（上游可能已重置额度，稍后重跑）"
  exit 1
fi
echo "  选定冷却出口: ${TARGET}"

echo
echo "=== 1. 建一个临时账号，直接停在冷却出口 ${TARGET} 上 ==="
# 密钥只在这个 python 进程里从 .env 读出来写进请求体，不落到任何输出。
sudo python3 - "$TARGET" <<'PY'
import json, subprocess, sys, urllib.request
target = sys.argv[1]
env = subprocess.run(["grep","-E","^ZEN_KEY_01=","/opt/zenproxy/.env"],
                     capture_output=True, text=True).stdout.strip()
key = env.split("=",1)[1]
tok = subprocess.run(["grep","-E","^ZEN_ADMIN_TOKEN=","/opt/zenproxy/.env"],
                     capture_output=True, text=True).stdout.strip().split("=",1)[1]
body = json.dumps({"id":"zen-quota-probe","name":"账本验证用（临时）","upstream":"zen",
                   "pool":"zen","proxy":target,"api_key":key,"enabled":True}).encode()
req = urllib.request.Request("http://127.0.0.1:9000/admin/api/accounts", data=body,
                             headers={"X-Admin-Token":tok,"Content-Type":"application/json"})
try:
    r = json.load(urllib.request.urlopen(req))
    a = r.get("account", r)
    print("  已建立: id=%s proxy=%s bound_exit=%s has_key=%s"
          % (a.get("id"), a.get("proxy"), a.get("bound_exit"), a.get("has_key")))
except Exception as e:
    print("  建立失败:", e); sys.exit(1)
PY
sleep 1
echo "  解析到的出口: $(probe_exit)"

echo
echo "=== 2. 发一个请求：账本应当在发包前就把它挪走 ==="
LOG_FROM=$(sudo docker logs zenproxy 2>&1 | wc -l)
CODE=$(sudo python3 - <<'PY'
import json, subprocess, urllib.request, os
env = subprocess.run(["grep","-E","^ZEN_KEY_01=","/opt/zenproxy/.env"],
                     capture_output=True, text=True).stdout.strip()
key = env.split("=",1)[1]
h = os.urandom(6).hex()
body = json.dumps({"model":"muse-spark-1.3-contributor-free","stream":True,
                   "input":"hi","instructions":"You are a pirate."}).encode()
req = urllib.request.Request("http://127.0.0.1:9000/zen-quota-probe/v1/responses", data=body,
    headers={"Authorization":"Bearer "+key,"Content-Type":"application/json",
             "User-Agent":"opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
             "x-opencode-client":"cli","x-opencode-project":"global",
             "x-opencode-request":"msg_"+h+"AAAAAAAAAAAAAA",
             "x-opencode-session":"ses_"+h+"AAAAAAAAAAAAAA",
             "x-opencode-session-id":"ses_"+h+"AAAAAAAAAAAAAA"})
try:
    r = urllib.request.urlopen(req, timeout=90)
    print(r.status)
except urllib.error.HTTPError as e:
    print(e.code)
except Exception as e:
    print("ERR", e)
PY
)
echo "  HTTP ${CODE}"
sudo docker logs zenproxy 2>&1 | tail -n +"$((LOG_FROM + 1))" |
  grep -E 'moving off an exit|every candidate exit' | sed 's/^/  /' | head -4
echo "  请求后出口: $(probe_exit)"

echo
echo "=== 3. 结论 ==="
if sudo docker logs zenproxy 2>&1 | grep -q 'moving off an exit whose quota is spent'; then
  echo "  ✓ 日志里有「发包前挪走」——账本改变了下一次请求的行为，而不只是记了一笔"
else
  echo "  ✗ 没观察到主动规避"
fi
echo "  最终账本:"; quota
