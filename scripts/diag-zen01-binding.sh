#!/usr/bin/env bash
# 解释 zen-01 与临时账号结果不一致：看 zen-01/zen-02 各自绑在哪个出口。
set -u
API=http://127.0.0.1:9000
TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)

echo "=== 账号绑定情况 ==="
curl -s -m 15 -H "X-Admin-Token: $TOKEN" "$API/admin/api/accounts" | python3 -c "
import json,sys
for a in json.load(sys.stdin)['accounts']:
    print('  %-10s proxy=%-16s upstream=%s weight=%s' % (a['id'], a.get('proxy') or '(未绑定/直连)', a.get('upstream'), a.get('weight')))
"

echo
echo "=== 各出口当前标记为可用/退出中 ==="
curl -s -m 15 -H "X-Admin-Token: $TOKEN" "$API/admin/api/proxies" | python3 -c "
import json,sys
ex=json.load(sys.stdin)['exits']
no_ip=[e['id'] for e in ex if not e.get('exit_ip')]
print('  没有出口 IP 的成员（%d）：%s' % (len(no_ip), ', '.join(sorted(no_ip))))
ret=[e['id'] for e in ex if e.get('retired')]
if ret: print('  已退役：%s' % ', '.join(ret))
"

echo
echo "=== zen-01 现在打 muse-spark-1.3（复现之前的 RegionError） ==="
for i in 1 2 3; do
  code=$(curl -s -o /tmp/z.json -w '%{http_code}' -m 60 -X POST "$API/zen-01/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"muse-spark-1.3-contributor-free","messages":[{"role":"user","content":"Say OK"}],"max_tokens":80}')
  printf '  #%s HTTP=%s %s\n' "$i" "$code" "$(head -c 105 /tmp/z.json | tr -d '\n')"
done
rm -f /tmp/z.json
