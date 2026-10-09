#!/usr/bin/env bash
# muse-spark-*-contributor-free 是 403 RegionError（按出口所在国家判）。
# Resin 出口横跨十几个国家，所以逐个出口试一遍，找出到底哪些国家放行。
#
# 做法：建一个**临时账号**（不动线上的 zen-01），把它逐个指向不同出口再测，最后删掉。
set -u

API=http://127.0.0.1:9000
TOKEN=$(sed -n 's/^ZEN_ADMIN_TOKEN=//p' /opt/zenproxy/.env)
ZKEY=$(sed -n 's/^ZEN_KEY_01=//p' /opt/zenproxy/.env)
TMPID=ms-probe
MODEL=${MODEL:-muse-spark-1.3-contributor-free}

auth=(-H "X-Admin-Token: $TOKEN")
api() { curl -s -m 20 "${auth[@]}" "$@"; }

cat > /tmp/j4.py <<'PY'
import json, sys
code = sys.argv[1]
raw = sys.stdin.read().strip()
if not raw:
    print("%s (空响应)" % code); sys.exit()
try:
    d = json.loads(raw)
except Exception:
    print("%s %s" % (code, raw[:60])); sys.exit()
if "error" in d:
    e = d["error"]
    print("%s %s" % (code, e.get("type") or e.get("message", "")[:40])); sys.exit()
ch = (d.get("choices") or [{}])[0]
m = ch.get("message") or {}
print("%s OK %r" % (code, (m.get("content") or "").strip().replace("\n", " ")[:30]))
PY

echo "=== 取出有真实 IP 的出口（id + 国家） ==="
api "$API/admin/api/proxies" > /tmp/px.json
python3 - <<'PY' > /tmp/exits.txt
import json, urllib.request
ex = json.load(open('/tmp/px.json'))['exits']
live = [e for e in ex if e.get('exit_ip')]
for e in sorted(live, key=lambda x: x['id']):
    ip = e['exit_ip']
    try:
        j = json.load(urllib.request.urlopen('http://ip-api.com/json/%s?fields=countryCode' % ip, timeout=8))
        cc = j.get('countryCode', '?')
    except Exception:
        cc = '?'
    print('%s %s %s' % (e['id'], ip, cc))
PY
cat /tmp/exits.txt | sed 's/^/  /'
n_exits=$(wc -l < /tmp/exits.txt)

echo
echo "=== 建临时账号 $TMPID ==="
api -X POST -H 'Content-Type: application/json' \
    -d "{\"id\":\"$TMPID\",\"name\":\"temporary region probe\",\"upstream\":\"zen\",\"api_key\":\"$ZKEY\",\"proxy\":\"$(head -1 /tmp/exits.txt | cut -d' ' -f1)\",\"enabled\":true}" \
    "$API/admin/api/accounts" | head -c 160 | sed 's/^/  /'
echo

echo
echo "=== 逐个出口测 $MODEL ==="
found=""
while read -r exit ip cc; do
    # 把临时账号指向这个出口（upsert 是部分更新，只改 proxy）
    api -X POST -H 'Content-Type: application/json' \
        -d "{\"id\":\"$TMPID\",\"proxy\":\"$exit\"}" \
        "$API/admin/api/accounts" >/dev/null
    sleep 0.4
    out=$(curl -sS -m 90 -w '|%{http_code}' -X POST "$API/$TMPID/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":120}" 2>&1)
    code=${out##*|}; body=${out%|*}
    printf '  %-15s %-16s %-3s  ' "$exit" "$ip" "$cc"
    printf '%s' "$body" | python3 /tmp/j4.py "$code"
    case "$body" in *'"id"'*) found="$found $cc" ;; esac
done < /tmp/exits.txt

echo
echo "=== 清理临时账号 ==="
api -X DELETE "$API/admin/api/accounts/$TMPID" | head -c 100 | sed 's/^/  /'
echo
api "$API/admin/api/accounts" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('  剩余账号：%s' % ', '.join(a['id'] for a in d['accounts']))
"
echo "  成功的国家：${found:-（无）}"
rm -f /tmp/j4.py /tmp/px.json /tmp/exits.txt
