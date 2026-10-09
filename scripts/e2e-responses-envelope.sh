#!/usr/bin/env bash
# Full end-to-end check of the /responses envelope through the proxy itself.
#
# The direct probes (raw-one.sh, probe-responses-slots.sh) hand-build the
# envelope, so they prove what the upstream accepts but not what the proxy
# produces. This goes through the reverse ingress instead, which is the only way
# to exercise all three pieces at once:
#   1. the gate envelope the proxy builds for /responses
#   2. the forced stream (the free tier refuses non-streaming requests)
#   3. the Responses fold that undoes it for a non-streaming caller
#
# A non-streaming request is deliberate: a streaming one would skip step 3
# entirely, and step 3 is the part that silently returns an empty answer when it
# is wired to the wrong folder.
#
# Usage: sudo ./e2e-responses-envelope.sh [account] [model]

set -uo pipefail

ACCT="${1:-zen-01}"
MODEL="${2:-muse-spark-1.3-contributor-free}"
BASE="http://127.0.0.1:9000/${ACCT}/v1/responses"
TOKEN=$(grep -E '^ZEN_ADMIN_TOKEN=' /opt/zenproxy/.env | cut -d= -f2-)

echo "account=${ACCT}  model=${MODEL}  stream=false"

echo
echo "=== 1. 账号当前绑的出口 ==="
curl -s -H "X-Admin-Token: ${TOKEN}" http://127.0.0.1:9000/admin/api/accounts |
  python3 -c '
import json,sys
j=json.load(sys.stdin)
accts=j.get("accounts") or j.get("items") or []
for a in accts:
    if a.get("id")=="'"${ACCT}"'":
        print("  bound_exit:", a.get("bound_exit"), " ip:", a.get("bound_exit_ip"))
'

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
cat > "$WORK/req.json" <<JSON
{"model":"${MODEL}","stream":false,"input":"Say exactly: PONG","instructions":"You are a pirate."}
JSON

echo
echo "=== 2. 非流式请求（代理必须强制上游流式，再折回来）==="
CODE=$(curl -sS --max-time 90 -o "$WORK/out.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data-binary "@$WORK/req.json" "$BASE")
echo "  http_code=${CODE}  大小 $(wc -c < "$WORK/out.json") 字节"

echo
echo "=== 3. 判定 ==="
# A Responses object carries "error": null on success — the key is always
# present, so its presence proves nothing and only a truthy value is a failure.
cp "$WORK/out.json" /tmp/e2e-responses-last.json
python3 - "$WORK/out.json" <<'PY'
import json, sys
raw = open(sys.argv[1], encoding="utf-8", errors="replace").read()
try:
    j = json.loads(raw)
except Exception as e:
    print("  不是 JSON（折叠失败或上游报错）:", e)
    print("  开头:", raw[:300])
    sys.exit(0)
if j.get("error"):
    print("  错误信封:", json.dumps(j["error"], ensure_ascii=False)[:300])
    sys.exit(0)
if "choices" in j:
    print("  ✗ 折叠错了：这是 chat 形状，Responses 客户端读不了")
    print("  开头:", raw[:200])
    sys.exit(0)
print("  object :", j.get("object"))
print("  status :", j.get("status"))
print("  model  :", j.get("model"))
print("  id     :", j.get("id"))
out = j.get("output") or []
text = ""
for item in out:
    for c in (item.get("content") or []):
        if c.get("type") == "output_text":
            text += c.get("text", "")
print("  文本   :", repr(text[:200]))
print()
print("  => 折叠成了合法的 Responses 对象" if out else "  => 空 output：折叠没拿到答案")
PY
echo "  原文另存: /tmp/e2e-responses-last.json"
