#!/usr/bin/env bash
# 摸清 cli-proxy-api 能提供什么、谁能连上它。配置里的凭据一律打码。
set -u

CFG=/opt/cli-proxy-api/config.yaml

echo "=== 顶层键（看开了哪些功能） ==="
grep -nE '^[a-z_-]+:' "$CFG" | sed 's/^/  /'

echo
echo "=== 入站鉴权 api-keys（只显示前缀与长度） ==="
grep -nE '^\s*-?\s*"?api-keys' "$CFG" | sed -E 's/(sk-[A-Za-z0-9]{6})[A-Za-z0-9]*/\1…<masked>/g' | sed 's/^/  /'
python3 - "$CFG" <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
m = re.search(r'(?m)^api-keys:\s*\n((?:\s*-\s*.+\n)+)', t)
if not m:
    m2 = re.search(r'(?m)^api-keys:\s*(.+)$', t)
    print("  api-keys 形式:", "单行" if m2 else "未找到")
    if m2:
        v = m2.group(1).strip().strip('"')
        print("    前缀 %s… 长度 %d" % (v[:8], len(v)))
else:
    items = [l.strip().lstrip('- ').strip().strip('"') for l in m.group(1).splitlines() if l.strip()]
    print("  api-keys 共 %d 条：" % len(items))
    for it in items:
        print("    %s… 长度 %d" % (it[:8], len(it)))
PY

echo
echo "=== 各 provider 的启用状态（只看 enabled 与 key 数量） ==="
python3 - "$CFG" <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
lines = t.splitlines()
cur = None
for i, ln in enumerate(lines):
    m = re.match(r'^([a-z][a-z0-9_-]*):\s*$', ln)
    if m and m.group(1) not in ('api-keys',):
        cur = m.group(1)
    if re.match(r'^\s+enabled:\s*(true|false)', ln) and cur:
        print("  %-22s %s" % (cur, ln.strip()))
PY

echo
echo "=== auths 目录里有哪些凭据文件（文件名 = provider/账号，不含内容） ==="
sudo ls -la /opt/cli-proxy-api/auths/ 2>/dev/null | sed 's/^/  /'

echo
echo "=== llm-internal 网络成员 ==="
docker network inspect llm-internal --format '{{range .Containers}}  {{.Name}}  {{.IPv4Address}}
{{end}}' 2>&1

echo "=== cli-proxy-api_default 网络成员 ==="
docker network inspect cli-proxy-api_default --format '{{range .Containers}}  {{.Name}}  {{.IPv4Address}}
{{end}}' 2>&1

echo
echo "=== 从宿主机探 CPA 的 /v1/models（用配置里的第一条 api-key） ==="
KEY=$(python3 - "$CFG" <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
m = re.search(r'(?m)^api-keys:\s*\n((?:\s*-\s*.+\n)+)', t)
if m:
    for l in m.group(1).splitlines():
        v = l.strip().lstrip('- ').strip().strip('"')
        if v: print(v); break
else:
    m2 = re.search(r'(?m)^api-keys:\s*(.+)$', t)
    if m2: print(m2.group(1).strip().strip('"'))
PY
)
if [ -n "$KEY" ]; then
    curl -s -m 15 -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models \
        | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('  非JSON: '+raw[:300]); sys.exit()
data=d.get('data') or []
print('  模型总数: %d' % len(data))
from collections import Counter
pref=Counter((m.get('id') or '').split('-')[0] for m in data)
for k,v in pref.most_common(15): print('    %-22s %d' % (k, v))
" 2>&1 | sed 's/^/  /'
else
    echo "  没取到 api-key，跳过"
fi
