#!/usr/bin/env bash
# 这 5 个模型的真实归属：
#   - 哪些 zenproxy（经 Resin 出口）拿得到
#   - 哪些 CPA（经服务器 IP + 第三方聚合）拿得到
# 403 RegionError 是按**出口所在国家**判的，两条路径的国家不同，所以要分别测。
set -u

CPA=http://127.0.0.1:8317
MODELS="muse-spark-1.3-contributor-free muse-spark-1.2-contributor-free jev-1.13-free exo-free space-bunny-free"

cat > /tmp/j3.py <<'PY'
import json, sys
code = sys.argv[1]
raw = sys.stdin.read().strip()
try:
    d = json.loads(raw)
except Exception:
    print("HTTP=%s 非JSON:%s" % (code, raw[:80])); sys.exit()
if "error" in d:
    e = d["error"]
    print("%s %s: %s" % (code, e.get("type"), str(e.get("message"))[:70])); sys.exit()
ch = (d.get("choices") or [{}])[0]
m = ch.get("message") or {}
print("%s OK  %r" % (code, (m.get("content") or "").strip().replace("\n", " ")[:40]))
PY

KEY=$(sudo python3 - /opt/cli-proxy-api/config.yaml <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
m = re.search(r'(?m)^access:\n(?:.*\n)*?^    api-keys:\n((?:^        - .+\n)+)', t)
if m:
    for l in m.group(1).splitlines():
        v = l.strip().lstrip('- ').strip().strip('"\'')
        if v: print(v); break
PY
)

echo "=== 经 CPA（服务器 IP 所在国家 + CPA 自己的上游） ==="
for m in $MODELS; do
    out=$(curl -sS -m 90 -w '|%{http_code}' -X POST "$CPA/v1/chat/completions" \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":120}" 2>&1)
    code=${out##*|}; body=${out%|*}
    printf '  %-34s ' "$m"
    printf '%s' "$body" | python3 /tmp/j3.py "$code"
done

echo
echo "=== 服务器自身出口 IP 与所属国家（这决定 CPA 那条路的判定） ==="
ip=$(curl -s -m 10 https://api.ipify.org)
echo "  IP: $ip"
curl -s -m 10 "http://ip-api.com/json/$ip?fields=country,countryCode,isp" | sed 's/^/  /'
echo

echo "=== zenproxy 当前各出口的 IP 与国家（这决定 zenproxy 那条路的判定） ==="
TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
curl -s -m 15 -H "X-Admin-Token: $TOKEN" http://127.0.0.1:9000/admin/api/proxies \
  | python3 -c "
import json,sys,urllib.request
ex=json.load(sys.stdin)['exits']
live=[e for e in ex if e.get('exit_ip')]
print('  有出口 IP 的成员 %d 个：' % len(live))
for e in sorted(live, key=lambda x: x['id']):
    ip=e['exit_ip']
    try:
        u='http://ip-api.com/json/%s?fields=countryCode,country' % ip
        j=json.load(urllib.request.urlopen(u, timeout=8))
        cc,cn=j.get('countryCode','?'),j.get('country','?')
    except Exception:
        cc,cn='?','?'
    print('    %-15s %-16s %s %s' % (e['id'], ip, cc, cn))
"
rm -f /tmp/j3.py
