#!/usr/bin/env bash
# 部署自检：一次性打印 zenproxy 在服务器上的关键状态（不回显任何密钥）。
set -u

TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
API=http://127.0.0.1:9000

echo "=== 容器 ==="
docker inspect zenproxy --format '  status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}} restarts={{.RestartCount}} image={{.Config.Image}}'

echo
echo "=== /healthz ==="
printf '  '; curl -s -m 5 "$API/healthz" | tr -d '\n'; echo

echo
echo "=== 概览 ==="
curl -s -m 10 -H "X-Admin-Token: $TOKEN" "$API/admin/api/status" > /tmp/st.json
python3 - <<'PY'
import json
d = json.load(open('/tmp/st.json'))
print("  监听 %s | 账号 %s(启用 %s) | 出口 %s(健康 %s, 退休 %s)" % (
    d['listen'], d['accounts'], d['accounts_enabled'],
    d['exits'], d['exits_healthy'], d['exits_retired']))
print("  上游 %s | 白名单 %s" % (','.join(d['upstreams']), ','.join(d['allowed_hosts'])))
print("  状态文件 %s" % d['state_file'])
ft = d['free_tier']
print("  免费额度套壳: enabled=%s  models=%d  learned=%s" % (
    ft['enabled'], len(ft['models']), ft['learned'] or '无'))
for m in ft['models']:
    print("      - %s" % m)
PY

echo
echo "=== 出口池（有真实出口 IP 的） ==="
curl -s -m 10 -H "X-Admin-Token: $TOKEN" "$API/admin/api/proxies" > /tmp/px.json
python3 - <<'PY'
import json
ex = json.load(open('/tmp/px.json'))['exits']
live = sorted([e for e in ex if e.get('exit_ip')], key=lambda x: x['id'])
print("  %d/%d 个成员当前有可用出口：" % (len(live), len(ex)))
for e in live:
    print("    %-15s ip=%-16s latency=%sms" % (e['id'], e['exit_ip'], e['latency_ms']))
quar = [e for e in ex if e.get('quarantined_until')]
ret = [e for e in ex if e.get('retired')]
print("  隔离中 %d，已退休 %d" % (len(quar), len(ret)))
PY

echo
echo "=== 账号出口绑定 ==="
curl -s -m 10 -H "X-Admin-Token: $TOKEN" "$API/admin/api/accounts" > /tmp/ac.json
python3 - <<'PY'
import json
for a in json.load(open('/tmp/ac.json'))['accounts']:
    st = a.get('stats') or {}
    print("  %-8s up=%-6s proxy=%-16s bound=%-16s ip=%-16s req=%s fail=%s rot=%s last=%s" % (
        a['id'], a['upstream'], a['proxy'], a.get('bound_exit') or '-',
        a.get('bound_exit_ip') or '-', st.get('requests', 0), st.get('failures', 0),
        st.get('rotations', 0), st.get('last_class') or '-'))
PY
