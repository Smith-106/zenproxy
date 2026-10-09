#!/usr/bin/env bash
# 收尾：清理侦察/诊断遗留的临时脚本，并打印最终交付状态。
set -u

echo "=== 清理临时文件 ==="
sudo rm -f /tmp/recon.sh /tmp/recon2.sh /tmp/recon3.sh /tmp/recon4.sh /tmp/diag-tok.sh \
  /tmp/diag-sd.sh /tmp/chk.sh /tmp/create-ch.sh /tmp/e2e-napi.sh /tmp/verify-all.sh \
  /tmp/soak.sh /tmp/ch_list.json /tmp/ch_new.json
echo "  已清理 /tmp 下的脚本与中间产物"
echo "  /tmp 残留（应为空）:"
sudo find /tmp -maxdepth 1 -name '*zenproxy*' -o -maxdepth 1 -name '*recon*' -o -maxdepth 1 -name '*diag*' 2>/dev/null | sed 's/^/    /'

echo
echo "=== zenproxy ==="
docker inspect zenproxy --format '  容器 status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}} restarts={{.RestartCount}}'
printf '  healthz: '; curl -s -m 5 http://127.0.0.1:9000/healthz | tr -d '\n '; echo
sudo ls -la /opt/zenproxy/ | tail -4

echo
echo "=== new-api 渠道 16 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c \
  "select id, name, status, base_url, \"group\" from channels where name like 'zenproxy%';" | sed 's/^/  /'
echo "  路由条数: $(docker exec postgres psql -U root -d new-api -t -A -c "select count(*) from abilities a join channels c on c.id=a.channel_id where c.name like 'zenproxy%';")"

echo
echo "=== 最近经渠道 16 的调用 ==="
docker exec postgres psql -U root -d new-api -t -A -F' | ' -c \
  "select id, model_name, channel_id, prompt_tokens, completion_tokens, use_time from logs where channel_id=16 order by id desc limit 5;" | sed 's/^/  /'

echo
echo "=== 仓库状态（远端 main 已同步） ==="
echo "  /home/ubuntu/zenproxy-src 是部署时用的源码快照，仅 install.sh 重跑时需要"
ls -d /home/ubuntu/zenproxy-src 2>/dev/null | sed 's/^/  /'
