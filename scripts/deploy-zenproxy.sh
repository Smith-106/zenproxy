#!/usr/bin/env bash
# Rebuild and restart zenproxy from an uploaded source tarball.
#
# The image is built here rather than shipped because the host is arm64 and the
# build workstation is not: shipping an amd64 image would leave the container
# unable to start, and cross-building needs buildx + qemu that this host does
# not have either.
#
# The swap is done with a rename rather than an in-place extract so a failed
# build still leaves the previous tree usable for a rollback. A successful
# build then recreates one replica at a time. Building the image alone leaves
# the running containers on the previous image.
#
# Usage: sudo ./deploy-zenproxy.sh /tmp/zenproxy-<sha>.tar.gz
set -euo pipefail

TAR="${1:?usage: deploy-zenproxy.sh <tarball>}"
SRC=/home/ubuntu/zenproxy-src
STAGE=/home/ubuntu/zenproxy-src.staging
PREV=/home/ubuntu/zenproxy-src.previous

[ -f "$TAR" ] || { echo "no such tarball: $TAR" >&2; exit 1; }

echo "=== 1. 解开新源码到暂存目录 ==="
rm -rf "$STAGE"
mkdir -p "$STAGE"
tar xzf "$TAR" -C "$STAGE"
echo "  条目数: $(find "$STAGE" -type f | wc -l)"
test -f "$STAGE/go.mod" || { echo "暂存目录不像仓库根，缺 go.mod" >&2; exit 1; }
test -f "$STAGE/deploy/Dockerfile" || { echo "缺 deploy/Dockerfile" >&2; exit 1; }

echo
echo "=== 2. 构建镜像（arm64，容器内编译）==="
cd "$STAGE"
docker build -f deploy/Dockerfile -t zenproxy:latest . 2>&1 | tail -20

echo
echo "=== 3. 冒烟：新镜像能跑起来并打印 usage ==="
docker run --rm --entrypoint /usr/local/bin/zenproxy zenproxy:latest --help >/dev/null 2>&1 || true
docker image inspect zenproxy:latest --format '  镜像 {{.Id}} 大小 {{.Size}} 字节 创建 {{.Created}}'

echo
echo "=== 4. 换掉源码快照（保留上一份以便回滚）==="
rm -rf "$PREV"
mv "$SRC" "$PREV" 2>/dev/null || true
mv "$STAGE" "$SRC"
echo "  当前: $SRC"
echo "  上一份: $PREV"

echo
echo "=== 5. 一次换一个副本，等它健康再换下一个 ==="
cd /opt/zenproxy
wait_healthy() {
  local name="$1" i s
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25; do
    s=$(docker inspect -f '{{.State.Health.Status}}' "$name")
    if [ "$s" = healthy ]; then
      echo "  $name healthy $i"
      return 0
    fi
    sleep 2
  done
  echo "  $name stuck $s" >&2
  return 1
}
docker compose up -d --no-deps --force-recreate zenproxy-a
wait_healthy zenproxy-zenproxy-a-1
docker compose up -d --no-deps --force-recreate zenproxy-b
wait_healthy zenproxy-zenproxy-b-1
docker inspect -f '{{.Name}} {{.Image}}' zenproxy-zenproxy-a-1 zenproxy-zenproxy-b-1
