#!/usr/bin/env bash
# 查这 5 个模型现在到底能不能用、卡在哪。
#   1) 上游当前免费清单（经 zenproxy，走的是同一条出口）
#   2) 逐个经 zenproxy 多次实测（403 RegionError 是按出口所在国家判的，多打几次采样不同出口）
#   3) 看 CPA 侧有没有同名的
set -u

BASE=http://127.0.0.1:9000
ACCT=${ACCT:-zen-01}
TRIES=${TRIES:-4}
MODELS="muse-spark-1.3-contributor-free muse-spark-1.2-contributor-free exo-free jev-1.13-free space-bunny-free"

cat > /tmp/judge2.py <<'PY'
import json, sys
code = sys.argv[1]
raw = sys.stdin.read().strip()
try:
    d = json.loads(raw)
except Exception:
    print("HTTP=%s 非JSON:%s" % (code, raw[:70])); sys.exit()
if "error" in d:
    e = d["error"]
    print("HTTP=%s %-24s %s" % (code, e.get("type"), str(e.get("message"))[:64])); sys.exit()
ch = (d.get("choices") or [{}])[0]
m = ch.get("message") or {}
print("HTTP=%s OK content=%r" % (code, (m.get("content") or "").strip().replace("\n", " ")[:34]))
PY

echo "=== 1) 上游当前免费模型清单 ==="
curl -s -m 30 "$BASE/$ACCT/v1/models" | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('  拉不到'); sys.exit()
ids=[m.get('id') for m in (d.get('data') or [])]
free=[i for i in ids if i and 'free' in i]
print('  总数 %d，带 free 的 %d 个：' % (len(ids), len(free)))
for i in sorted(free): print('    %s' % i)
print('  --- 含 exo / muse / jev / spark 的（不限 free）---')
for i in sorted(ids):
    if any(k in (i or '').lower() for k in ('exo','muse','jev','spark')): print('    %s' % i)
"

echo
echo "=== 2) 逐个经 zenproxy 实测（每个 $TRIES 次，采样不同出口） ==="
for m in $MODELS; do
    echo "  --- $m ---"
    for i in $(seq 1 "$TRIES"); do
        out=$(curl -sS -m 180 -w '|%{http_code}' -X POST "$BASE/$ACCT/v1/chat/completions" \
            -H 'Content-Type: application/json' \
            -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":120}" 2>&1)
        code=${out##*|}; body=${out%|*}
        printf '    #%s ' "$i"
        printf '%s' "$body" | python3 /tmp/judge2.py "$code"
    done
done

echo
echo "=== 3) CPA 侧有没有同名的 ==="
docker exec new-api sh -c 'true' 2>/dev/null
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
curl -s -m 20 -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('  CPA 拉不到'); sys.exit()
ids=[m.get('id') for m in (d.get('data') or [])]
hit=[i for i in ids if any(k in (i or '').lower() for k in ('exo','muse','jev','spark','sensenova'))]
print('  CPA 共 %d 个模型，匹配到的：' % len(ids))
for i in sorted(hit): print('    %s' % i)
if not hit: print('    （无）')
"
rm -f /tmp/judge2.py
