#!/usr/bin/env bash
# 连续打几次免费模型请求，观察出口轮换是否真的在工作。
# 只读 + 发正常推理请求，不改任何配置。
set -u

TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
API=http://127.0.0.1:9000
N=${1:-8}

echo "=== 连打 $N 次（zen-01 / big-pickle） ==="
ok=0
for i in $(seq 1 "$N"); do
    out=$(curl -sS -m 120 -w '|%{http_code}|%{time_total}' -X POST "$API/zen-01/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d '{"model":"big-pickle","messages":[{"role":"user","content":"Reply with exactly: OK"}],"max_tokens":300}' 2>&1)
    # -w 尾部追加了 "|http_code|time_total" 两个字段，要从后往前剥
    secs=${out##*|}
    rest=${out%|*}
    code=${rest##*|}
    body=${rest%|*}
    txt=$(printf '%s' "$body" | python3 -c "
import json,sys
try: d=json.loads(sys.stdin.read().strip())
except Exception: print('非JSON'); sys.exit()
if 'error' in d: print('ERR:'+str(d['error'].get('type'))); sys.exit()
m=(d.get('choices') or [{}])[0].get('message') or {}
print((m.get('content') or '').strip()[:24] or '(空)')
" 2>/dev/null)
    printf '  #%-2s HTTP=%s %6ss  %s\n' "$i" "$code" "$secs" "$txt"
    [ "$code" = "200" ] && ok=$((ok+1))
done
echo "  成功 $ok / $N"

echo
echo "=== 账号统计（看轮换次数与最近分类） ==="
curl -s -m 10 -H "X-Admin-Token: $TOKEN" "$API/admin/api/accounts" > /tmp/f_ac.json
python3 - <<'PY'
import json
for a in json.load(open('/tmp/f_ac.json'))['accounts']:
    st = a.get('stats') or {}
    by = st.get('by_class') or {}
    top = ', '.join('%s=%s' % (k, v) for k, v in sorted(by.items(), key=lambda x: -x[1])[:5])
    print("  %-8s req=%-4s fail=%-4s rot=%-3s bound=%-16s ip=%-16s last=%s" % (
        a['id'], st.get('requests', 0), st.get('failures', 0), st.get('rotations', 0),
        a.get('bound_exit') or '-', a.get('bound_exit_ip') or '-', st.get('last_class') or '-'))
    if top:
        print("           分类: %s" % top)
PY
rm -f /tmp/f_ac.json
