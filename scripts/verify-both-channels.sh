#!/usr/bin/env bash
# 两个渠道（16=zenproxy，17=CLIProxyAPI）的最终核验。
set -u

API=http://127.0.0.1:3000
psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
KEY=$(psql -c "select key from tokens where status=1 and deleted_at is null order by id limit 1;")
case "$KEY" in sk-*) ;; *) KEY="sk-$KEY" ;; esac

probe() {
    local m="$1"
    out=$(curl -sS -m 180 -w '|%{http_code}|%{time_total}' -X POST "$API/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":400}" 2>&1)
    secs=${out##*|}; rest=${out%|*}; code=${rest##*|}; body=${rest%|*}
    printf '    %-42s ' "$m"
    printf '%s' "$body" | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('HTTP=$code 非JSON: '+raw[:80]); sys.exit()
if 'error' in d:
    e=d['error']; print('HTTP=$code %s' % str(e.get('message'))[:70]); sys.exit()
ch=(d.get('choices') or [{}])[0]; msg=ch.get('message') or {}
print('HTTP=$code content=%r' % ((msg.get('content') or '').strip().replace(chr(10),' ')[:30],))
" 2>/dev/null
    printf '      %ss\n' "$secs"
}

echo "=== 渠道总览 ==="
psql -F' | ' -c "select c.id, c.name, c.status, c.base_url, c.\"group\", (select count(*) from abilities a where a.channel_id=c.id) as routes from channels c where c.id in (16,17) order by c.id;" | sed 's/^/  /'

echo
echo "=== 渠道 16：zenproxy 免费额度（3 个抽样） ==="
probe big-pickle
probe ling-3.1-flash-free
probe space-bunny-free

echo
echo "=== 渠道 17：CLIProxyAPI（4 个不同 provider 抽样） ==="
probe gpt-oss:20b
probe swe-2
probe GLM-5.2
probe nvidia/nemotron-3.5-lightning-30b-a3b

echo
echo "=== 最近的调用落在哪个渠道 ==="
psql -F' | ' -c "select channel_id, count(*) as calls, count(distinct model_name) as models from logs where channel_id is not null and created_at > extract(epoch from now()) - 900 group by channel_id order by channel_id;" | sed 's/^/  /'
