#!/usr/bin/env bash
# 收尾：清理临时文件 + 打印两个渠道的最终状态。
set -u

echo "=== 清理临时脚本 ==="
sudo rm -f /tmp/recon-cpa.sh /tmp/recon-cpa2.sh /tmp/recon-cpa3.sh /tmp/verify-cpa.sh \
  /tmp/verify-cpa-napi.sh /tmp/chk-cpa.sh /tmp/connect-net.sh /tmp/create-newapi-channel.sh \
  /tmp/extract-cpa-inbound-key.py /tmp/diag-404.sh /tmp/probe-put.sh /tmp/restore-ch.sh \
  /tmp/verify-both.sh /tmp/r.out /tmp/probe.out /tmp/m.json /tmp/zenproxy-status.sh
echo "  /tmp 残留（应为空）："
sudo find /tmp -maxdepth 1 \( -name '*cpa*' -o -name '*zenproxy*' -o -name '*recon*' -o -name '*diag*' -o -name '*verify*' -o -name '*probe*' \) 2>/dev/null | sed 's/^/    /'

echo
echo "=== 容器 ==="
docker inspect zenproxy --format '  zenproxy   status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}}'
docker inspect new-api  --format '  new-api    status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}}'
docker inspect cli-proxy-api --format '  cli-proxy  status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}}'
echo "  new-api 网络：$(docker inspect new-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"

echo
echo "=== new-api 渠道 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c \
  "select c.id, c.name, c.status, c.base_url, (select count(*) from abilities a where a.channel_id=c.id) as routes from channels c order by c.id;" | sed 's/^/  /'

echo
echo "=== new-api compose 里的网络声明（保证重建后不丢） ==="
sudo grep -A 3 '^networks:' /opt/new-api/docker-compose.yml | sed 's/^/  /'
