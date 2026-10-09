# zenproxy 生产镜像 —— 多阶段构建，静态二进制，非 root 运行
#
# 构建（在仓库根目录执行，context 必须是仓库根，因为需要 go.mod / internal / cmd）：
#   docker build -f deploy/Dockerfile -t zenproxy:latest .
#
# 说明：
#   * 编译阶段用 golang:1.26-alpine，与 go.mod 里的 go 1.26.8 对齐。
#   * CGO_ENABLED=0 + -trimpath + -ldflags="-s -w" => 完全静态、可脱离 libc 运行。
#   * 运行阶段用 alpine:3.21 而不是 distroless，原因只有一个：
#     HEALTHCHECK 需要容器内有 shell 或 HTTP 客户端。distroless 里什么都没有，
#     要写 HEALTHCHECK 就必须额外塞一个静态探针二进制。若坚持 distroless，
#     把下面 FROM runtime 段换成 gcr.io/distroless/static-debian12:nonroot 并删掉
#     HEALTHCHECK 一段（改用宿主机侧或 compose 侧的健康检查）。

# ---------- 编译 ----------
FROM golang:1.26-alpine AS builder

# git 供 go mod 处理 VCS 依赖，ca-certificates 供后续可能的私有模块代理。
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 依赖层先落地，Go 源码变动时不会重复下载模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 静态、去路径、去符号表。netgo 强制纯 Go DNS 解析，避免容器内 NSS 配置差异。
# 结尾的 `--help` 只是一次「产物确实可执行」的冒烟：flag.Parse() 会打印 usage 并以 2 退出，
# 所以这里必须吞掉退出码，不能用它判断任何东西。
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-mod=mod
RUN GOARCH="$(go env GOARCH)" && \
    go build -trimpath \
      -tags netgo,osusergo \
      -ldflags="-s -w -extldflags '-static'" \
      -o /out/zenproxy ./cmd/zenproxy && \
    /out/zenproxy --help >/dev/null 2>&1 || true

# ---------- 运行 ----------
FROM alpine:3.21 AS runtime

# ca-certificates: 出站访问 https://opencode.ai 与探针 https://api.ipify.org 必需
# tzdata:          日志与 /admin 面板里的本地时间
# wget:            busybox 自带，供 HEALTHCHECK 使用
RUN apk add --no-cache ca-certificates tzdata

# 非 root 运行。固定 uid/gid，方便宿主侧 bind mount 时对齐权限。
RUN addgroup -g 10001 -S zenproxy && \
    adduser -u 10001 -S -G zenproxy -h /app -s /sbin/nologin zenproxy

WORKDIR /app

COPY --from=builder /out/zenproxy /usr/local/bin/zenproxy

# 配置与状态目录。state.json 必须落在可写位置（config.yaml 的 state_file 指向 /data/state.json）。
RUN mkdir -p /app /data && chown -R 10001:10001 /app /data

USER zenproxy

EXPOSE 9000

# 容器内必须监听 0.0.0.0:9000，127.0.0.1 只有容器自己能看到，litellm 连不上。
# 对宿主暴露由 compose 的 ports 段收窄为 127.0.0.1:9000。

# -config 是 zenproxy 真实的命令行参数（cmd/zenproxy/main.go: flag.String("config", "config.yaml", ...)），
# 默认值就是 "config.yaml"。这里显式写出绝对路径，避免依赖 WORKDIR。
ENTRYPOINT ["/usr/local/bin/zenproxy"]
CMD ["-config", "/app/config.yaml"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -T 4 -O /dev/null http://127.0.0.1:9000/healthz || exit 1
