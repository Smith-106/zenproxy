#!/usr/bin/env bash
# 只读侦察 new-api 的渠道现状，判断 zenproxy 该接在哪。
# 不做任何写入：只 select。
set -u

PSQL="docker exec postgres psql -U root -d new-api -t -A -F| -c"

echo "=== 渠道总览（id|名称|类型|状态|base_url） ==="
$PSQL "select id, name, type, status, coalesce(base_url,'') from channels order by id;" 2>&1 | sed 's/^/  /'

echo
echo "=== 与 opencode/zen 相关的渠道 ==="
$PSQL "select id, name, status, coalesce(base_url,''), coalesce(models,'') from channels where name ilike '%opencode%' or name ilike '%zen%' or base_url ilike '%opencode%' or models ilike '%pickle%' or models ilike '%free%';" 2>&1 | sed 's/^/  /'

echo
echo "=== 渠道数量与启停统计 ==="
$PSQL "select count(*) as total, count(*) filter (where status=1) as enabled from channels;" 2>&1 | sed 's/^/  /'
