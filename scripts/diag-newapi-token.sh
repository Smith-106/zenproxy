#!/usr/bin/env bash
# 定位 new-api 401 "Invalid token" 的确切来源，并检查令牌缓存。
set -u

echo "=== Invalid token 字样出处 ==="
grep -rn 'Invalid token' /opt/new-api --include=*.go | head -10

echo
echo "=== tokens 表结构 ==="
docker exec postgres psql -U root -d new-api -c '\d tokens' 2>&1 | head -32

echo
echo "=== Redis 里的令牌缓存键 ==="
docker exec redis redis-cli --scan --pattern '*token*' 2>&1 | head -10
echo "--- 缓存条目总数 ---"
docker exec redis redis-cli dbsize 2>&1

echo
echo "=== new-api 最近 30 行日志里的鉴权失败 ==="
docker logs --tail 40 new-api 2>&1 | grep -iE 'token|401|unauthor|invalid' | tail -12
