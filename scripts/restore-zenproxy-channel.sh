#!/usr/bin/env bash
# 恢复 zenproxy 渠道被我试探性 PUT 改坏的字段（models 被压成只剩 big-pickle）。
# 走 PUT 但**不带 status** —— 实测带 status 会被拒（Invalid parameters）。
set -euo pipefail

API=http://127.0.0.1:3000
psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
TOKEN=$(psql -c "select access_token from users where id=1;")
CID=$(psql -c "select id from channels where name='zenproxy (opencode free)' limit 1;")

MODELS="big-pickle,ling-3.1-flash-free,mimo-v2.6-flash-free,space-bunny-free,step-5-preview-free,longcat-2.5-preview-free,nemotron-3.5-lightning-free"

echo "=== 恢复前 ==="
psql -F' | ' -c "select id, name, models, weight, auto_ban, status from channels where id=$CID;" | sed 's/^/  /'

payload=$(python3 - "$CID" "$MODELS" <<'PY'
import json, sys
cid, models = sys.argv[1], sys.argv[2]
print(json.dumps({
    "id": int(cid),
    "type": 1,
    "name": "zenproxy (opencode free)",
    "key": "zenproxy-injects-real-key",
    "base_url": "http://zenproxy:9000/zen-01",
    "models": models,
    "group": "default",
    "model_mapping": "",
    "priority": 0,
    "weight": 1,
    "auto_ban": 0,
    "remark": "经 zenproxy 走 Resin 出口池的 opencode 免费额度；密钥由 zenproxy 按账号注入，这里的 key 是占位值",
}, ensure_ascii=False))
PY
)

echo
echo "=== PUT（故意不带 status） ==="
resp=$(printf '%s' "$payload" | curl -s -m 20 -X PUT \
    -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
    -H 'Content-Type: application/json' --data-binary @- "${API}/api/channel/")
printf '%s' "$resp" | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('  非JSON: '+raw[:200]); sys.exit()
print('  success=%s message=%s' % (d.get('success'), d.get('message') or '(空)'))
if d.get('data'): print('  模型数=%d status=%s' % (len((d['data'].get('models') or '').split(',')), d['data'].get('status')))
"

echo
echo "=== 恢复后 ==="
psql -F' | ' -c "select id, name, models, weight, auto_ban, status from channels where id=$CID;" | sed 's/^/  /'
echo "  路由条数：$(psql -c "select count(*) from abilities where channel_id=$CID;")"
psql -F' | ' -c "select model, enabled from abilities where channel_id=$CID order by model;" | sed 's/^/    /'
