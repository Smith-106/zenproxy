#!/usr/bin/env bash
# 读 cli-proxy-api 配置的关键段落。任何 sk-/token 值一律打码。
set -u
CFG=/opt/cli-proxy-api/config.yaml

mask() { sed -E 's/(sk-[A-Za-z0-9_-]{4})[A-Za-z0-9_-]+/\1…<masked>/g; s/(("|\s)(api[-_]?key|token|secret|password)"?\s*:\s*"?)([A-Za-z0-9_-]{4})[A-Za-z0-9_-]+/\1\4…<masked>/g'; }

echo "=== access: 段（94-110） ==="
sudo sed -n '94,110p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== server: 段（344-360） ==="
sudo sed -n '344,360p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== credentials: 段（359-382） ==="
sudo sed -n '359,382p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== management / oauth / observability（380-400） ==="
sudo sed -n '380,400p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== api-keys: 段（400 行起 15 行） ==="
sudo sed -n '400,415p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== routing: / requests: 段概览 ==="
sudo sed -n '4,14p' "$CFG" | mask | sed 's/^/  /'
echo "  --- requests 段前 25 行 ---"
sudo sed -n '100,125p' "$CFG" | mask | sed 's/^/  /'

echo
echo "=== 配置里 auths/凭据相关的实际文件 ==="
sudo find /opt/cli-proxy-api/data /opt/cli-proxy-api/auths -type f 2>/dev/null | head -20 | sed 's/^/  /'
echo "  (auths 为空 = 没有任何 provider 账号登录过)"
