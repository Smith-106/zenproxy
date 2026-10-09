#!/usr/bin/env bash
# 收尾核对：
#  1) 探测过程中写过含 RESIN_PROXY_TOKEN 的 0600 临时文件，确认一个都没残留
#  2) 确认线上配置确实没被改动（只看 mtime 与摘要，不打印内容）
#  3) 服务与渠道仍正常
set -u

TOKEN=$(sed -n 's/^RESIN_PROXY_TOKEN=//p' /opt/zenproxy/.env)
echo "=== 1) 凭据有没有残留在临时文件里 ==="
hits=0
for d in /tmp /var/tmp /root; do
    for f in $(find "$d" -maxdepth 2 -type f \( -name 'tmp.*' -o -name '*.cfg' -o -name 'curl*' \) 2>/dev/null); do
        if grep -qF "$TOKEN" "$f" 2>/dev/null; then
            echo "  ★ 命中：$f"
            hits=$((hits+1))
        fi
    done
done
echo "  含 token 的残留文件数：$hits（0 = 干净）"
echo "  /tmp 下的 tmp.* 一览："
find /tmp -maxdepth 1 -name 'tmp.*' 2>/dev/null | sed 's/^/    /'
echo "  （以上为空即无残留）"

echo
echo "=== 2) 线上配置未改动的证据 ==="
for f in /opt/cli-proxy-api/config.yaml /opt/zenproxy/config.yaml /opt/zenproxy/.env /opt/new-api/docker-compose.yml; do
    printf '  %-42s mtime=%s size=%s\n' "$f" "$(stat -c %y "$f" | cut -d. -f1)" "$(stat -c %s "$f")"
done
echo "  今天是：$(date '+%Y-%m-%d %H:%M:%S')"

echo
echo "=== 3) 容器与网络（确认没多接网络） ==="
for c in zenproxy new-api cli-proxy-api resin; do
    docker inspect "$c" --format "  {{.Name}}  {{.State.Status}}  nets={{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}" 2>/dev/null
done

echo
echo "=== 4) 两个渠道仍正常 ==="
KEY=$(docker exec postgres psql -U root -d new-api -t -A -c "select key from tokens where status=1 and deleted_at is null order by id limit 1;")
case "$KEY" in sk-*) ;; *) KEY="sk-$KEY" ;; esac
for m in big-pickle gpt-oss:20b; do
    code=$(curl -s -o /tmp/vc.json -w '%{http_code}' -m 120 -X POST http://127.0.0.1:3000/v1/chat/completions \
        -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":300}")
    printf '  %-16s HTTP=%s\n' "$m" "$code"
done
rm -f /tmp/vc.json
