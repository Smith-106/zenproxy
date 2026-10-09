#!/usr/bin/env bash
# 侦察 CPA 的出站代理能力与上游规模：
#   - 配置里有没有 proxy-url / 全局代理字段，能挂在哪一层
#   - 各 provider 有多少把 key（决定"按 key 配代理"要改多少处）
#   - 这些上游是 http 还是 https（决定走 zenproxy 后能不能看到状态码）
set -u

CFG=/opt/cli-proxy-api/config.yaml

echo "=== 1) 配置里与代理相关的字段 ==="
sudo grep -n -iE 'proxy|proxy-url|proxy_url|http-proxy|socks' "$CFG" | head -20 | sed 's/^/  /'
echo "  （以上为全部命中；空 = CPA 配置里目前没有任何代理设置）"

echo
echo "=== 2) 顶层 section ==="
sudo grep -n -E '^[a-z-]+:' "$CFG" | sed 's/^/  /'

echo
echo "=== 3) 各 provider 的 key 数量与协议 ==="
sudo python3 - "$CFG" <<'PY'
import re, sys, collections
txt = open(sys.argv[1], encoding='utf-8', errors='replace').read()
lines = txt.splitlines()

# 找出所有 base-url 与它们属于哪个 section
cur = None
sec = collections.Counter()
urls = []
keys = collections.Counter()
for i, ln in enumerate(lines, 1):
    m = re.match(r'^([a-z-]+):', ln)
    if m: cur = m.group(1)
    if re.search(r'base-url:', ln):
        u = ln.split('base-url:', 1)[1].strip().strip('"\'')
        urls.append((cur, u))
    if re.search(r'api-key', ln) or re.search(r'^\s*-\s*"?(nvapi-|sk-)', ln):
        keys[cur] += 1

print('  provider 段（顶层 section）与 base-url：')
seen = set()
for s, u in urls:
    if (s, u) not in seen:
        seen.add((s, u))
        proto = 'https' if u.startswith('https') else ('http' if u.startswith('http') else '?')
        print('    %-24s %-46s [%s]' % (s, u, proto))
print()
print('  顶层 section 名：%s' % ', '.join(sorted(set(s for s, _ in urls if s))))
PY

echo
echo "=== 4) 上游主机名一览（换 allowed_hosts 时要加的就是这些） ==="
sudo python3 - "$CFG" <<'PY'
import re, sys
from urllib.parse import urlparse
txt = open(sys.argv[1], encoding='utf-8', errors='replace').read()
hosts = set()
for m in re.finditer(r'base-url:\s*["\']?(https?://[^\s"\']+)', txt):
    h = urlparse(m.group(1)).hostname
    if h: hosts.add(h)
for h in sorted(hosts): print('    %s' % h)
print('  共 %d 个上游主机' % len(hosts))
PY

echo
echo "=== 5) CPA 容器版本与是否支持 proxy-url ==="
docker inspect cli-proxy-api --format '  image={{.Config.Image}}'
sudo grep -rn -i 'proxy-url' /opt/cli-proxy-api/ --include=*.md --include=*.yaml --include=*.yml 2>/dev/null | head -8 | sed 's/^/  /'
echo "  （若无文档，需查二进制里的字符串）"
docker exec cli-proxy-api sh -c 'strings /CLIProxyAPI 2>/dev/null | grep -i "proxy-url" | head -8' 2>/dev/null | sed 's/^/    /' || echo "    （容器里没有 strings，或路径不同）"
