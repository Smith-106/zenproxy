#!/usr/bin/env bash
# Resin 出口到底以什么形式暴露？决定 CPA 能不能直接接 Resin（比绕 zenproxy 更简单）。
#   - zenproxy 配置里 proxy 条目的 URL 形状
#   - resin 容器是什么、监听什么端口、什么协议
set -u

echo "=== 1) zenproxy 配置里的出口定义（脱敏后看形状） ==="
sudo python3 - <<'PY'
import re
t = open('/opt/zenproxy/config.yaml', encoding='utf-8', errors='replace').read()
m = re.search(r'(?ms)^proxies:\n(.*?)(?=^\S|\Z)', t)
if not m:
    print('  没找到 proxies: 段'); raise SystemExit
body = m.group(1)
# 脱敏：把 token/密码替换成 ***
def mask(s):
    s = re.sub(r'(://[^:@/]+):[^@/]+@', r'\1:***@', s)
    s = re.sub(r'(token=)[^&\s"\']+', r'\1***', s)
    return s
lines = body.rstrip().splitlines()
print('  共 %d 行，前 6 行：' % len(lines))
for l in lines[:6]:
    print('    %s' % mask(l))
print('  ...')
print('  末 2 行：')
for l in lines[-2:]:
    print('    %s' % mask(l))
PY

echo
echo "=== 2) resin 容器 ==="
docker ps --format '  {{.Names}}\t{{.Image}}\t{{.Ports}}' | grep -i resin
echo
NETS=$(docker inspect resin --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>/dev/null)
echo "  网络：$NETS"

echo
echo "=== 3) resin 监听端口 ==="
docker exec resin sh -c 'netstat -tlnp 2>/dev/null || ss -tlnp 2>/dev/null || cat /proc/net/tcp | head -5' 2>/dev/null | head -12 | sed 's/^/  /'

echo
echo "=== 4) resin 容器的入口/命令（看它是什么服务） ==="
docker inspect resin --format '  cmd={{.Config.Cmd}} entrypoint={{.Config.Entrypoint}}' 2>/dev/null
docker inspect resin --format '  env={{range .Config.Env}}{{if or (eq (printf "%.3s" .) "RES") (eq (printf "%.3s" .) "PRO") (eq (printf "%.3s" .) "PO")}}{{.}} {{end}}{{end}}' 2>/dev/null | sed 's/\(:\/\/[^:]*:\)[^@]*@/\1***@/g; s/\(TOKEN=\).*/\1***/g'

echo
echo "=== 5) zenproxy 连 resin 用的是哪种 URL（从 zenproxy 环境变量看） ==="
sudo grep -oE '[a-z0-9]+://[^"'"'"' ]*resin[^"'"'"' ]*' /opt/zenproxy/docker-compose.yml /opt/zenproxy/config.yaml 2>/dev/null \
  | sed 's/\(:\/\/[^:@/]*\):[^@]*@/\1:***@/' | sort -u | head -5 | sed 's/^/  /'
