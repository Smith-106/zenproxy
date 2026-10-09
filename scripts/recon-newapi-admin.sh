#!/usr/bin/env bash
# 侦察 new-api 的管理凭据与版本，为建渠道做准备。
# 只读取，不打印任何令牌值。
set -u

echo "=== new-api 版本 ==="
docker exec new-api cat /VERSION 2>/dev/null || docker inspect new-api --format '  image={{.Config.Image}}'
docker inspect new-api --format '  created={{.Created}}'

echo
echo "=== 用户表：谁能当管理员（不打印令牌） ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' \
  -c "select id, username, role,
             case when access_token is null or access_token = '' then 'no' else 'yes' end as has_token,
             length(coalesce(access_token,'')) as token_len
      from users order by id;"

echo
echo "=== new-api 容器监听与端口映射 ==="
docker inspect new-api --format '  {{range $p, $c := .NetworkSettings.Ports}}{{$p}} -> {{$c}}{{"\n"}}{{end}}'
docker inspect new-api --format '  networks={{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'
