#!/usr/bin/env bash
# 打印 zenproxy 配置的 proxies 段全文（凭据一律替换成 ***），
# 目的是拿到 Resin 的「出口定义形状」和 slot 命名规则。
set -u
sudo python3 - <<'PY'
import re
t = open('/opt/zenproxy/config.yaml', encoding='utf-8', errors='replace').read()
m = re.search(r'(?ms)^proxies:\n(.*?)(?=^\S|\Z)', t)
body = m.group(1) if m else ''
def mask(s):
    s = re.sub(r'(://[^:@/\s]+):[^@/\s]+@', r'\1:***@', s)
    s = re.sub(r'(token=)[^&\s"\']+', r'\1***', s)
    s = re.sub(r'([Pp]assword["\']?\s*[:=]\s*["\']?)[^"\'\s]+', r'\1***', s)
    return s
for i, l in enumerate(body.rstrip().splitlines(), 1):
    print('  %3d| %s' % (i, mask(l)))
print()
print('  总行数：%d' % len(body.rstrip().splitlines()))
PY

echo
echo "=== 顶层 token / 身份相关变量 ==="
sudo grep -nE 'token|identity|platform' /opt/zenproxy/config.yaml | sed 's/\(token[^:]*:\).*/\1 ***/I' | head -10 | sed 's/^/  /'

echo
echo "=== .env 里与 resin 有关的变量名（只列名字，不列值） ==="
sudo sed -n 's/^\([A-Z_]*RESIN[A-Z_]*\)=.*/  \1/p' /opt/zenproxy/.env
sudo sed -n 's/^\(RESIN[A-Z_]*\)=.*/  \1/p' /opt/zenproxy/.env
echo "  （以上仅变量名）"
