#!/usr/bin/env bash
# 经 new-api 逐个验证 7 个免费模型的路由与可用性。
set -u

API=http://127.0.0.1:3000
MODELS=${*:-"big-pickle ling-3.1-flash-free mimo-v2.6-flash-free space-bunny-free step-5-preview-free longcat-2.5-preview-free nemotron-3.5-lightning-free"}

psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
KEY=$(psql -c "select key from tokens where status=1 and deleted_at is null order by id limit 1;")
case "$KEY" in sk-*) ;; *) KEY="sk-$KEY" ;; esac

judge() {
    python3 -c "
import json,sys
code = sys.argv[1]
raw = sys.stdin.read().strip()
try: d = json.loads(raw)
except Exception: print('HTTP=%s 非JSON: %s' % (code, raw[:90])); sys.exit()
if 'error' in d:
    e = d['error']
    print('HTTP=%s %s: %s' % (code, e.get('type'), str(e.get('message'))[:80])); sys.exit()
ch = (d.get('choices') or [{}])[0]
m = ch.get('message') or {}
txt = (m.get('content') or '').strip().replace('\n', ' ')
print('HTTP=%s content=%r finish=%s' % (code, txt[:36], ch.get('finish_reason')))
" "$1"
}

for m in $MODELS; do
    out=$(curl -sS -m 240 -w '|%{http_code}' -X POST "$API/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":1500}" 2>&1)
    code=${out##*|}
    body=${out%|*}
    printf '  %-32s ' "$m"
    printf '%s' "$body" | judge "$code"
done

echo
echo "=== 这些调用落在哪个渠道（应全部是 16） ==="
psql -F' | ' -c "select model_name, channel_id, count(*) from logs where model_name <> '' and created_at > extract(epoch from now()) - 600 group by model_name, channel_id order by model_name;" | sed 's/^/  /'
