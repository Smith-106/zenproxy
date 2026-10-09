#!/usr/bin/env bash
# 侦察 new-api 的缓存策略与现有用户令牌，判断"改库能不能立刻生效、能不能端到端验证"。
# 只读，不打印任何令牌值。
set -u

echo "=== new-api 关键环境变量（值可能含密码，做掩码） ==="
docker inspect new-api --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -viE 'password|secret|token|key' | grep -iE 'cache|sql|redis|node|retry|stream|debug' | sed 's/^/  /'

echo
echo "=== 缓存相关（只看开关，不打印密钥） ==="
docker inspect new-api --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -iE '^(MEMORY_CACHE_ENABLED|SYNC_FREQUENCY|CHANNEL_UPDATE_FREQUENCY|BATCH_UPDATE_ENABLED)=' | sed 's/^/  /' || echo "  （未显式设置，用默认值）"

echo
echo "=== 用户令牌数量（tokens 表） ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' \
  -c "select count(*) as total, count(*) filter (where status=1) as enabled from tokens;"

echo
echo "=== 已启用令牌的归属（不打印 key） ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' \
  -c "select t.id, t.user_id, t.name, t.status, t.group, length(coalesce(t.key,'')) as key_len from tokens t where t.status=1 order by t.id limit 10;"

echo
echo "=== 现有渠道的完整字段样例（id=1，用来对照建新渠道要填什么） ==="
docker exec postgres psql -U root -d new-api -x \
  -c "select id, type, status, name, base_url, models, \"group\", weight, priority, auto_ban, coalesce(model_mapping,'') as model_mapping, coalesce(remark,'') as remark from channels where id=1;"
