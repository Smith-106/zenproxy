#!/usr/bin/env bash
# 验证「令牌查不到」是不是因为被软删除（GORM 的 deleted_at IS NULL 过滤）。
set -u

echo "=== 令牌的软删除状态 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c "
select id,
       case when deleted_at is null then 'visible to app' else 'SOFT-DELETED at ' || deleted_at::text end as visibility,
       status,
       left(name, 20) as name
from tokens order by id;"

echo
echo "=== 模拟应用的查询（带 deleted_at is null） ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c "
select t.id, (select count(*) from tokens x where x.key = t.key and x.deleted_at is null) as visible_matches
from tokens t order by t.id;"

echo
echo "=== 顺带看用户是否也被软删除 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c "
select id, username, role, case when deleted_at is null then 'active' else 'SOFT-DELETED' end from users order by id;"
