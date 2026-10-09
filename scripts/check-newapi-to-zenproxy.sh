#!/usr/bin/env bash
# 验证 new-api 容器能否按容器名访问 zenproxy —— 渠道 Base URL 能不能写
# http://zenproxy:9000/<账号ID> 取决于这一步。
set -u

echo "=== zenproxy 接在哪些网络、别名是什么 ==="
docker inspect zenproxy --format '{{range $net, $cfg := .NetworkSettings.Networks}}  {{$net}}  aliases=[{{range $cfg.Aliases}}{{.}} {{end}}]  ip={{$cfg.IPAddress}}
{{end}}'

echo "=== new-api 接在哪些网络 ==="
docker inspect new-api --format '{{range $net, $cfg := .NetworkSettings.Networks}}  {{$net}}  ip={{$cfg.IPAddress}}
{{end}}'

echo "=== 从 new-api 容器内解析并访问 zenproxy:9000 ==="
docker exec new-api sh -c 'getent hosts zenproxy || echo "  DNS 解析失败"'
echo "  --- /healthz ---"
docker exec new-api sh -c 'wget -q -T 8 -O - http://zenproxy:9000/healthz 2>&1 || curl -s -m 8 http://zenproxy:9000/healthz 2>&1 || echo "  访问失败"'

echo
echo "=== 从 new-api 容器内访问渠道路径（账号前缀形式） ==="
docker exec new-api sh -c 'wget -q -T 8 -O - http://zenproxy:9000/zen-01/v1/models 2>&1 | head -c 200 || echo "  访问失败"'
echo
