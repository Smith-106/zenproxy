#!/usr/bin/env bash
# 把 new-api 接进 llm-internal 网络，让它够得到 cli-proxy-api。
#
# 两件事都做，缺一不可：
#   1) docker network connect  —— 立即生效，不动容器、零停机
#   2) 改 docker-compose.yml   —— 让它在容器**被重建**之后依然存在
#      （手工 connect 只是给当前容器加了个网卡，compose recreate 会丢掉）
set -euo pipefail

COMPOSE=/opt/new-api/docker-compose.yml
NET=llm-internal

echo "=== 0) 前置检查 ==="
docker network inspect "$NET" >/dev/null 2>&1 || { echo "  网络 $NET 不存在" >&2; exit 1; }
echo "  $NET 存在；成员：$(docker network inspect "$NET" --format '{{range .Containers}}{{.Name}} {{end}}')"
echo "  当前 new-api 网络：$(docker inspect new-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"

echo
echo "=== 1) 立即接入（零停机） ==="
if docker inspect new-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' | grep -qw "$NET"; then
    echo "  已在 $NET 上，跳过"
else
    docker network connect "$NET" new-api
    echo "  已接入"
fi
echo "  现在网络：$(docker inspect new-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"

echo
echo "=== 2) 写进 compose（保证重建后不丢） ==="
if grep -q "$NET" "$COMPOSE"; then
    echo "  compose 里已有 $NET，跳过"
else
    sudo cp -n "$COMPOSE" "${COMPOSE}.bak-$(date +%Y%m%d%H%M%S)"
    sudo python3 - "$COMPOSE" "$NET" <<'PY'
import sys
path, net = sys.argv[1], sys.argv[2]
lines = open(path, encoding='utf-8').read().splitlines(keepends=True)

# 在 new-api 服务的 healthcheck 块之后插入 networks:，并在文件末尾追加顶层 networks:
out = []
inserted = False
for i, ln in enumerate(lines):
    out.append(ln)
    # healthcheck 的最后一行是 "      retries: 3"，紧跟其后就是下一个服务
    if not inserted and ln.strip() == 'retries: 3':
        out.append("    networks:\n")
        out.append("      - default\n")
        out.append("      - %s\n" % net)
        inserted = True

if not inserted:
    sys.exit("could not find the healthcheck block to anchor the networks key")

text = "".join(out)
# 顶层 networks: 段（追加在 volumes 之后）
if "\nnetworks:\n" not in text:
    text += "\nnetworks:\n  %s:\n    external: true\n" % net

open(path, 'w', encoding='utf-8').write(text)
print("  compose 已更新")
PY
    echo "  --- 改动预览 ---"
    sudo diff "${COMPOSE}".bak-* "$COMPOSE" | sed 's/^/    /' || true
fi

echo
echo "=== 3) 验证连通性 ==="
echo "  --- DNS 解析 ---"
docker exec new-api sh -c 'getent hosts cli-proxy-api || echo "    解析失败"'
echo "  --- 试连 CPA ---"
docker exec new-api sh -c 'wget -q -T 8 -O - http://cli-proxy-api:8317/v1/models 2>&1 | head -c 100 || echo "    连不上"'
echo
echo "  --- 顺带确认 new-api 仍能出网（别把它的默认路由搞坏） ---"
docker exec new-api sh -c 'wget -q -T 8 -O /dev/null https://api.github.com 2>&1 && echo "    出网正常" || echo "    出网异常"'
