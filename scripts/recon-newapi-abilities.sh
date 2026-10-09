#!/usr/bin/env bash
# 确认 new-api 的模型路由表 abilities 存在且被使用——它决定"能不能直接改库建渠道"。
set -u

echo "=== abilities 表是否存在 ==="
docker exec postgres psql -U root -d new-api -t -A \
  -c "select to_regclass('public.abilities');" | sed 's/^/  /'

echo
echo "=== abilities 表结构 ==="
docker exec postgres psql -U root -d new-api -c "\d abilities" 2>&1 | head -20

echo
echo "=== 现有 abilities 记录数 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' \
  -c "select count(*) as rows, count(distinct channel_id) as channels, count(distinct model) as models from abilities;"

echo
echo "=== 现有 abilities 样例（前 5 条，看 group/model/channel 的对应） ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' \
  -c "select \"group\", model, channel_id, enabled, priority, weight from abilities order by channel_id, model limit 5;"
