#!/bin/bash
# zenproxy-rolling.sh — 双副本零空窗滚动重建
#
# 副本对（zenproxy-a/zenproxy-b，共享 DNS 名 zenproxy）交替重建：先重建一个，
# 等 healthcheck healthy，再重建另一个。任意时刻至少一个健康副本在 new-api 的
# DNS 名下服务，重建期的 connection refused 从物理上消失。
#
# 用法（在 /opt/zenproxy 下，或任意目录——脚本内部 cd /opt/zenproxy）：
#   /usr/local/bin/zenproxy-rolling.sh [image]
#
# image: 可选，默认 zenproxy:latest。换镜像时先用 compose 指到新 tag，或直接
# rebuild zenproxy:latest 后跑本脚本。
#
# 状态：.last-recreate 记录上次最后重建的是哪个副本，双次部署交替，避免只有
#   一个副本的镜像永远处于旧状态。

set -euo pipefail
cd /opt/zenproxy

STATE=/opt/zenproxy/.last-recreate
LAST=zenproxy-b
[ -f "$STATE" ] && LAST=$(cat "$STATE")
# 上次最后重建 A → 这次先重建 A（它还是旧镜像概率最大）
if [ "$LAST" = "zenproxy-a" ]; then
  ORDER=(zenproxy-a zenproxy-b)
  WRITEB=zenproxy-b
else
  ORDER=(zenproxy-b zenproxy-a)
  WRITEB=zenproxy-a
fi

cid() { sudo docker compose ps -q "$1" 2>/dev/null; }
healthy() {
  local c
  c=$(cid "$1")
  [ -n "$c" ] && [ "$(sudo docker inspect -f '{{.State.Health.Status}}' "$c" 2>/dev/null)" = "healthy" ]
}
up_and_wait() {
  local svc="$1" c i
  echo "[rolling] up -d --no-deps $svc"
  sudo docker compose up -d --no-deps "$svc" >/dev/null
  for i in $(seq 1 90); do
    if healthy "$svc"; then
      echo "[rolling] $svc healthy (waited ${i}x2s)"
      return 0
    fi
    sleep 2
  done
  echo "[rolling] $svc did not become healthy in 180s" >&2
  return 1
}

echo "[rolling] order: ${ORDER[*]}"
up_and_wait "${ORDER[0]}"
up_and_wait "${ORDER[1]}"
echo "$WRITEB" > "$STATE"
sudo docker compose ps
echo "[rolling] done — both replicas healthy under DNS name zenproxy"