#!/usr/bin/env bash
# 终态核对
set -u
echo "=== /tmp 残留 ==="
find /tmp -maxdepth 1 \( -name "*probe*" -o -name "*chunk*" -o -name "*matrix*" -o -name "*diag*" -o -name "*verify*" \) 2>/dev/null | sed 's/^/  /'
echo "  （以上为空即已清干净）"

echo
echo "=== zenproxy 账号（临时探测账号应已删除） ==="
TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
curl -s -m 15 -H "X-Admin-Token: $TOKEN" http://127.0.0.1:9000/admin/api/accounts \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print('  ' + ', '.join('%s(proxy=%s)' % (a['id'], a.get('proxy') or '直连') for a in d['accounts']))"

echo
echo "=== free_tier 现状 ==="
curl -s -m 15 -H "X-Admin-Token: $TOKEN" http://127.0.0.1:9000/admin/api/status \
  | python3 -c "
import json,sys
ft=json.load(sys.stdin)['free_tier']
print('  配置 models (%d)：%s' % (len(ft['models']), ', '.join(ft['models'])))
print('  learned (%d)：%s' % (len(ft.get('learned') or []), ', '.join(ft.get('learned') or []) or '（空）'))
"

echo
echo "=== 两个渠道仍正常 ==="
for a in zen-01; do
  code=$(curl -s -o /tmp/f.json -w '%{http_code}' -m 90 -X POST "http://127.0.0.1:9000/$a/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"space-bunny-free","messages":[{"role":"user","content":"Say OK"}],"max_tokens":80}')
  printf '  %-8s space-bunny-free  HTTP=%s %s\n' "$a" "$code" "$(head -c 60 /tmp/f.json | tr -d '\n')"
done
rm -f /tmp/f.json
