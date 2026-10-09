#!/usr/bin/env bash
# 1) 看 free_tier.learned 里有没有 muse-spark / jev —— 解释 403 为何变成 400
# 2) 对照 zen-01（正式账号）与刚才的临时账号，确认行为是否一致
# 3) 试别的协议端点（responses / messages），看这两个模型到底要什么形状
set -u

API=http://127.0.0.1:9000
TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
auth=(-H "X-Admin-Token: $TOKEN")

echo "=== 1) free_tier 当前状态（含 learned） ==="
curl -s -m 15 "${auth[@]}" "$API/admin/api/status" | python3 -c "
import json,sys
ft=json.load(sys.stdin)['free_tier']
print('  enabled=%s' % ft['enabled'])
print('  配置的 models (%d)：' % len(ft['models']))
for m in ft['models']: print('    %s' % m)
learned = ft.get('learned') or []
print('  自动学会的 learned (%d)：' % len(learned))
for m in learned: print('    %s' % m)
if not learned: print('    （空）')
"

echo
echo "=== 2) 对照：zen-01 现在打这两个模型是什么反应 ==="
for m in muse-spark-1.3-contributor-free muse-spark-1.2-contributor-free jev-1.13-free; do
    for path in "/v1/chat/completions"; do
        code=$(curl -s -o /tmp/a.json -w '%{http_code}' -m 60 -X POST "$API/zen-01$path" \
            -H 'Content-Type: application/json' \
            -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":80}")
        printf '  %-34s %-22s HTTP=%s  %s\n' "$m" "$path" "$code" "$(head -c 110 /tmp/a.json | tr -d '\n')"
    done
done

echo
echo "=== 3) 换协议端点试（responses / messages） ==="
for m in muse-spark-1.3-contributor-free jev-1.13-free space-bunny-free; do
    echo "  --- $m ---"
    # OpenAI Responses API 形状
    code=$(curl -s -o /tmp/r1.json -w '%{http_code}' -m 60 -X POST "$API/zen-01/v1/responses" \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"input\":\"Say OK\"}")
    printf '    /v1/responses      HTTP=%s  %s\n' "$code" "$(head -c 100 /tmp/r1.json | tr -d '\n')"
    # Anthropic Messages 形状
    code=$(curl -s -o /tmp/r2.json -w '%{http_code}' -m 60 -X POST "$API/zen-01/v1/messages" \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"max_tokens\":80,\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}]}")
    printf '    /v1/messages       HTTP=%s  %s\n' "$code" "$(head -c 100 /tmp/r2.json | tr -d '\n')"
done
rm -f /tmp/a.json /tmp/r1.json /tmp/r2.json
