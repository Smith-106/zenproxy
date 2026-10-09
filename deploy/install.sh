#!/usr/bin/env bash
#
# zenproxy 容器部署安装脚本 —— 默认路线（挂在「已经跑着的 new-api」旁边）
#
# 幂等，可重复执行。已有 config.yaml / .env 绝不会被覆盖（要覆盖显式加 --force）。
# 检测到占位值（REPLACE_WITH_*）或环境变量没配齐时，脚本会拒绝启动容器，
# 并明确告诉你缺哪一个 —— 不会把半成品配置推上线。
#
#   用法（在 deploy/ 目录下执行）：
#     sudo ./install.sh                     # 装文件 + 起容器
#     sudo ./install.sh --no-start          # 只装文件，不起容器
#     sudo ./install.sh --force             # 连 config.yaml / .env 一起重置成模板
#     sudo ./install.sh --build             # 先从源码构建 zenproxy:latest 再装
#     sudo ./install.sh --prefix /opt/zenproxy --image zenproxy:latest
#
#   产物（PREFIX 默认 /opt/zenproxy）：
#     docker-compose.yml    0644 root:root     每次刷新成仓库里的版本（非密钥）
#     config.yaml           0640 root:10001    已存在则保留原文件
#     .env                  0600 root:root     已存在则保留原文件
#     data/                 0750 10001:10001   state.json 落在这里
#
#   不做的事：不改 new-api / resin 的 compose 文件，不创建 docker 网络，
#             不碰 nginx，不覆盖任何已存在的密钥文件。
#
#   前置：new-api_default 与 resin_default 两个网络必须已存在
#         （分别由 new-api 与 resin 的 compose 创建），zenproxy:latest 镜像必须已有。

set -euo pipefail

# ---------------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------------
PREFIX="/opt/zenproxy"
IMAGE="zenproxy:latest"
COMPOSE_NAME="docker-compose.yml"
CONTAINER="zenproxy"
# 容器内运行用户（见 deploy/Dockerfile: adduser -u 10001）。config.yaml 与 data/ 的
# 宿主侧属主必须能对上，否则容器读不到配置、写不了状态。
CONTAINER_UID="10001"
CONTAINER_GID="10001"
HOST_PORT="9000"
NET_A="new-api_default"
NET_B="resin_default"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

FORCE=0
BUILD_LOCAL=0
START=1

# ---------------------------------------------------------------------------
# 输出 helper
# ---------------------------------------------------------------------------
log()  { printf '\033[1;34m[zenproxy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[zenproxy] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[zenproxy] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '3,26p' "${BASH_SOURCE[0]}" | sed -e 's/^# \{0,1\}//'
    exit 0
}

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --prefix)   PREFIX="${2:-}"; [[ -n "${PREFIX}" ]] || die "--prefix 后面要跟目录"; shift 2 ;;
        --image)    IMAGE="${2:-}";  [[ -n "${IMAGE}"  ]] || die "--image 后面要跟镜像名"; shift 2 ;;
        --build)    BUILD_LOCAL=1; shift ;;
        --no-start) START=0; shift ;;
        --force)    FORCE=1; shift ;;
        -h|--help)  usage ;;
        *)          die "未知参数：$1（用 --help 看用法）" ;;
    esac
done

DST_COMPOSE="${PREFIX}/${COMPOSE_NAME}"
DST_CFG="${PREFIX}/config.yaml"
DST_ENV="${PREFIX}/.env"
DST_DATA="${PREFIX}/data"

# ---------------------------------------------------------------------------
# 1. 前置检查
# ---------------------------------------------------------------------------
[[ "${EUID}" -eq 0 ]] || die "需要 root：sudo ./install.sh"
command -v docker >/dev/null 2>&1 || die "找不到 docker 命令"
docker compose version >/dev/null 2>&1 || die "找不到 docker compose 子命令（需要 Docker Compose v2+）"
[[ -f "${SCRIPT_DIR}/${COMPOSE_NAME}" ]] || die "找不到 ${SCRIPT_DIR}/${COMPOSE_NAME}"
[[ -f "${SCRIPT_DIR}/config.example.yaml" ]] || die "找不到 ${SCRIPT_DIR}/config.example.yaml"
[[ -f "${SCRIPT_DIR}/.env.example" ]] || die "找不到 ${SCRIPT_DIR}/.env.example"

# ---------------------------------------------------------------------------
# 2. 目录
# ---------------------------------------------------------------------------
install -d -m 0755 "${PREFIX}"
install -d -m 0750 "${DST_DATA}"
log "部署目录：${PREFIX}"

# ---------------------------------------------------------------------------
# 3. compose 文件（非密钥，每次刷新成仓库里的版本）
# ---------------------------------------------------------------------------
if [[ -f "${DST_COMPOSE}" ]] && cmp -s "${SCRIPT_DIR}/${COMPOSE_NAME}" "${DST_COMPOSE}"; then
    log "compose 文件无变化：${DST_COMPOSE}"
else
    install -m 0644 -o root -g root "${SCRIPT_DIR}/${COMPOSE_NAME}" "${DST_COMPOSE}"
    log "已安装/更新 compose 文件：${DST_COMPOSE}"
fi

# ---------------------------------------------------------------------------
# 4. 配置与密钥（★ 已有文件绝不覆盖）
# ---------------------------------------------------------------------------
CFG_FRESH=0
if [[ -f "${DST_CFG}" && "${FORCE}" -ne 1 ]]; then
    log "配置已存在，内容保留不动：${DST_CFG}"
else
    install -m 0640 -o root -g root "${SCRIPT_DIR}/config.example.yaml" "${DST_CFG}"
    CFG_FRESH=1
    log "已从模板生成配置：${DST_CFG}"
fi
# 无论新建还是沿用，都把权限收敛到「root 可写、容器 uid/gid 10001 可读」。
# 容器以非 root 运行，root:root 0600 会让它读不到配置，启动直接失败。
chown "root:${CONTAINER_GID}" "${DST_CFG}"
chmod 0640 "${DST_CFG}"

ENV_FRESH=0
if [[ -f "${DST_ENV}" ]]; then
    log "环境文件已存在，内容保留不动：${DST_ENV}"
else
    install -m 0600 -o root -g root "${SCRIPT_DIR}/.env.example" "${DST_ENV}"
    ENV_FRESH=1
    log "已从 .env.example 生成：${DST_ENV}（全是占位值，必须替换）"
fi
# .env 只被宿主上的 compose 读取（做变量插值），文件本身不进容器 —— 所以保持 0600 root:root。
chmod 0600 "${DST_ENV}"

# 数据目录交给容器运行用户，state.json 要写在这里。
chown -R "${CONTAINER_UID}:${CONTAINER_GID}" "${DST_DATA}"
chmod 0750 "${DST_DATA}"

# ---------------------------------------------------------------------------
# 5. 镜像
# ---------------------------------------------------------------------------
if [[ "${BUILD_LOCAL}" -eq 1 ]]; then
    REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
    [[ -f "${REPO_ROOT}/go.mod" ]] || die "--build 需要仓库源码，但在 ${REPO_ROOT} 找不到 go.mod"
    log "构建镜像 ${IMAGE}（context=${REPO_ROOT}）"
    docker build -f "${SCRIPT_DIR}/Dockerfile" -t "${IMAGE}" "${REPO_ROOT}"
fi

IMAGE_OK=1
if docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    log "镜像就绪：${IMAGE}"
else
    IMAGE_OK=0
    warn "本地没有镜像 ${IMAGE}，无法启动容器。先构建再传上来，二选一："
    warn "  同机： cd <仓库根> && docker build -f deploy/Dockerfile -t ${IMAGE} ."
    warn "  异机： docker save ${IMAGE} | gzip | ssh <宿主机> 'gunzip | docker load'"
    warn "  或直接重跑： sudo ./install.sh --build"
fi

# ---------------------------------------------------------------------------
# 6. 外部网络（只检查，不创建 —— 它们归 new-api / resin 的 compose 管）
# ---------------------------------------------------------------------------
NET_OK=1
for net in "${NET_A}" "${NET_B}"; do
    if docker network inspect "${net}" >/dev/null 2>&1; then
        log "外部网络就绪：${net}"
    else
        NET_OK=0
        warn "找不到 docker 网络 ${net}。它由对应服务的 compose 创建，本脚本不会替你建："
        warn "  确认 ${net%%_*} 的 compose 正在跑： docker network ls"
        warn "  名字对不上时改 deploy/docker-compose.yml 的 networks 段（必须与真实网络名逐字一致）"
    fi
done

# ---------------------------------------------------------------------------
# 7. ★ 占位值 / 变量齐备性闸门
# ---------------------------------------------------------------------------
READY=1

# 只看「真正的赋值行」：注释里出现 REPLACE_WITH 是很正常的（本文件顶部就在
# 讲这个占位约定，LiteLLM 那几行也是注释掉的），拿整文件 grep 会让默认路线
# 永远卡在「还有占位值没替换」，即使四个真实变量都填好了。
assignments() { grep -E '^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=' "$1" || true; }

if assignments "${DST_ENV}" | grep -q 'REPLACE_WITH'; then
    READY=0
    warn "${DST_ENV} 里还有 REPLACE_WITH_* 占位值没替换："
    assignments "${DST_ENV}" | grep -n 'REPLACE_WITH' | sed 's/^/    /' >&2
fi

if assignments "${DST_CFG}" | grep -q 'REPLACE_WITH'; then
    READY=0
    warn "${DST_CFG} 里有 REPLACE_WITH 占位值没替换"
fi

# 从 config.yaml 里抽出所有 ${VAR}，逐个确认 .env 里有非空值。
# 这一步与 zenproxy 自己的启动校验同源：配置里引用了没定义的变量，它一定起不来。
# ★ 必须排除注释行：本文件顶部就用 ${VAR} 讲通用规则，那是文档不是配置。
env_value() {
    local key="$1" line
    line="$(assignments "${DST_ENV}" | grep -E "^[[:space:]]*${key}=" | tail -n 1 || true)"
    [[ -n "${line}" ]] || return 0
    printf '%s' "${line#*=}"
}

MISSING=""
NOT_INJECTED=""
while read -r var; do
    [[ -n "${var}" ]] || continue
    if [[ -z "$(env_value "${var}")" ]]; then
        MISSING="${MISSING} ${var}"
    fi
    # 变量还必须被 compose 的 environment 段注入，否则它进不了容器。
    # ★ needle 只取 "${VAR" 前缀：compose 写的是 "${VAR:?未设置…}"，
    #   找完整的 "${VAR}" 永远匹配不上，会让每个变量都被误报成「没注入」。
    if ! grep -qF -- "\${${var}" "${DST_COMPOSE}"; then
        NOT_INJECTED="${NOT_INJECTED} ${var}"
    fi
done < <(grep -vE '^[[:space:]]*#' "${DST_CFG}" \
         | grep -oE '\$\{[A-Za-z_][A-Za-z0-9_]*\}' | tr -d '${}' | sort -u || true)

if [[ -n "${MISSING}" ]]; then
    READY=0
    warn "${DST_CFG} 引用了这些变量，但 ${DST_ENV} 里没有非空值：${MISSING}"
fi
if [[ -n "${NOT_INJECTED}" ]]; then
    READY=0
    warn "这些变量没有被 ${DST_COMPOSE} 的 environment 段注入，容器里会是空的：${NOT_INJECTED}"
    warn "  （zenproxy 只会加载 config.yaml，环境变量靠 compose 逐项传入，不是 env_file）"
fi

[[ "${IMAGE_OK}" -eq 1 ]] || READY=0
[[ "${NET_OK}" -eq 1 ]] || READY=0

# ---------------------------------------------------------------------------
# 8. 启动（只有全部就绪才启）
# ---------------------------------------------------------------------------
if [[ "${START}" -eq 0 ]]; then
    log "已指定 --no-start，跳过启动"
elif [[ "${READY}" -eq 1 ]]; then
    log "启动容器：docker compose -f ${DST_COMPOSE} up -d"
    docker compose --project-directory "${PREFIX}" -f "${DST_COMPOSE}" up -d

    HEALTHY=0
    if command -v curl >/dev/null 2>&1; then
        for _ in $(seq 1 15); do
            if curl -fsS -m 3 "http://127.0.0.1:${HOST_PORT}/healthz" >/dev/null 2>&1; then
                HEALTHY=1
                break
            fi
            sleep 1
        done
    fi
    if [[ "${HEALTHY}" -eq 1 ]]; then
        log "/healthz 已就绪（http://127.0.0.1:${HOST_PORT}/healthz）"
    else
        warn "还探不到 /healthz。看日志： docker compose -f ${DST_COMPOSE} logs -f ${CONTAINER}"
    fi
else
    warn "配置还没就绪（见上面的 WARN），暂不启动容器"
    warn "补齐之后执行："
    warn "  sudo docker compose --project-directory ${PREFIX} -f ${DST_COMPOSE} up -d"
fi

# ---------------------------------------------------------------------------
# 9. 收尾自检
# ---------------------------------------------------------------------------
echo
log "---- 自检 ----"
printf '  compose 文件   %s\n' "$(stat -c '%A %U:%G %n' "${DST_COMPOSE}" 2>/dev/null || echo '缺失')"
printf '  config.yaml    %s\n' "$(stat -c '%A %U:%G %n' "${DST_CFG}" 2>/dev/null || echo '缺失')"
printf '  .env           %s\n' "$(stat -c '%A %U:%G %n' "${DST_ENV}" 2>/dev/null || echo '缺失')"
printf '  data/          %s\n' "$(stat -c '%A %U:%G %n' "${DST_DATA}" 2>/dev/null || echo '缺失')"
printf '  镜像           %s\n' "$(docker image inspect "${IMAGE}" --format '{{.Id}}' 2>/dev/null | cut -c1-19 || echo '缺失')"
printf '  外部网络       %s\n' "$(docker network inspect "${NET_A}" "${NET_B}" --format '{{.Name}}' 2>/dev/null | tr '\n' ' ' || echo '缺失')"
printf '  容器状态       %s\n' "$(docker inspect "${CONTAINER}" --format '{{.State.Status}} (health: {{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}})' 2>/dev/null || echo '未运行')"

# ---------------------------------------------------------------------------
# 10. 下一步
# ---------------------------------------------------------------------------
cat <<EOF

$(log "下一步：")

  1) 填密钥（替换全部 REPLACE_WITH_*）
       sudo vi ${DST_ENV}
       # ZEN_ADMIN_TOKEN      openssl rand -hex 32
       # RESIN_PROXY_TOKEN    从 resin 容器读，不要自己编：
       #   docker inspect resin --format '{{range .Config.Env}}{{println .}}{{end}}' \\
       #     | grep '^RESIN_PROXY_TOKEN='
       # ZEN_KEY_01/02        上游账号后台复制

  2) 核对 ${DST_CFG}
       # listen 保持 "0.0.0.0:9000"；state_file 保持 "/data/state.json"；
       # proxies.resin.host 保持 "resin:2260"

  3) 起容器并验活
       sudo docker compose --project-directory ${PREFIX} -f ${DST_COMPOSE} up -d
       curl -s http://127.0.0.1:${HOST_PORT}/healthz          # 期望 status: ok
       curl -s -H "X-Admin-Token: \$ZEN_ADMIN_TOKEN" \\
         http://127.0.0.1:${HOST_PORT}/admin/api/status       # 看 exits_healthy 不为 0

  4) 接 new-api（完整步骤见仓库里的 deploy/README.md）
       渠道 → 高级配置 → Base URL = http://zenproxy:9000/<账号ID>
       ★ 填 Base URL，不要填「代理」字段 —— 原因见 README 第二节。

  5) 卸载
       sudo docker compose --project-directory ${PREFIX} -f ${DST_COMPOSE} down
       sudo rm -rf ${PREFIX}          # 会连 data/state.json 一起删掉
EOF

if [[ "${CFG_FRESH}" -eq 1 || "${ENV_FRESH}" -eq 1 ]]; then
    log "提示：本次有文件是从模板新生成的，务必先去第 1、2 步填好再起容器。"
fi
