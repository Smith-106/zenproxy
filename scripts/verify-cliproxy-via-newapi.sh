#!/usr/bin/env bash
# 经 new-api 验证 CLIProxyAPI 渠道（跨多个 provider 挑模型）。
set -u

API=http://127.0.0.1:3000
CHID=${CHID:-17}
MODELS=${*:-"nvidia/nemotron-3.5-lightning-30b-a3b gpt-oss:20b swe-2 GLM-5.2 meta/muse-glimmer-30b"}

psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
# ★ deleted_at is null 不能漏（GORM 软删除，挑到死令牌会 401）
KEY=$(psql -c "select key from tokens where status=1 and deleted_at is null order by id limit 1;")
case "$KEY" in sk-*) ;; *) KEY="sk-$KEY" ;; esac

for m in $MODELS; do
    out=$(curl -sS -m 180 -w '|%{http_code}|%{time_total}' -X POST "$API/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":300}" 2>&1)
    secs=${out##*|}; rest=${out%|*}; code=${rest##*|}; body=${rest%|*}
    printf '  %-42s ' "$m"
    printf '%s' "$body" | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('HTTP=$code 非JSON: '+raw[:90]); sys.exit()
if 'error' in d:
    e=d['error']; print('HTTP=$code %s: %s' % (e.get('type'), str(e.get('message'))[:80])); sys.exit()
ch=(d.get('choices') or [{}])[0]; msg=ch.get('message') or {}
txt=(msg.get('content') or '').strip().replace(chr(10),' ')
print('HTTP=$code content=%r finish=%s' % (txt[:36], ch.get('finish_reason')))
" 2>/dev/null
    echo "      耗时 ${secs}s"
done

echo
echo "=== 这些调用落在哪个渠道 ==="
psql -F' | ' -c "select model_name, channel_id, count(*) from logs where channel_id is not null and created_at > extract(epoch from now()) - 600 group by model_name, channel_id order by channel_id, model_name;" | sed 's/^/  /'
