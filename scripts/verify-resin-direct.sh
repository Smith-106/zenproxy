#!/usr/bin/env bash
# 验证：CPA 能不能直接接 Resin，并且用「身份名」控制出口。
# 关键点：curl 的代理凭据不能进 argv（会出现在 ps 里），所以写进 0600 的 -K 配置文件。
set -u

TOKEN=$(sed -n 's/^RESIN_PROXY_TOKEN=//p' /opt/zenproxy/.env)
[ -n "$TOKEN" ] || { echo "取不到 RESIN_PROXY_TOKEN"; exit 1; }

SRV=$(curl -s -m 10 https://api.ipify.org)
echo "=== 服务器自身出口 IP：$SRV ==="
echo

probe() {
    local slot="$1" host="$2" port="$3" label="$4"
    local cfg
    cfg=$(mktemp)
    chmod 600 "$cfg"
    # 身份格式来自 zenproxy 配置注释：<Platform>.<Account>:<token>@<host>
    printf 'proxy = "socks5h://Default.%s:%s@%s:%s"\n' "$slot" "$TOKEN" "$host" "$port" > "$cfg"
    printf '  %-14s %-22s %-8s ' "$slot" "$host:$port" "$label"
    out=$(curl -s -m 25 -K "$cfg" https://api.ipify.org 2>&1 | head -c 40)
    rm -f "$cfg"
    if [ -z "$out" ]; then
        echo "失败/超时"
    else
        echo "出口 IP = $out"
    fi
}

echo "=== A) 宿主机 -> Resin（127.0.0.1:2260），逐个 slot 看出口 IP 是否不同 ==="
for s in slot-01 slot-02 slot-03 slot-05 slot-07 slot-13 slot-15 slot-21; do
    probe "$s" 127.0.0.1 2260 "host"
done

echo
echo "=== B) CPA 容器 -> resin（当前未接同一网络，预期解析失败） ==="
docker exec cli-proxy-api sh -c 'getent hosts resin || echo "  解析不了 resin（CPA 不在 resin_default 上）"' 2>&1 | sed 's/^/  /'
echo
echo "  CPA 现有网络：$(docker inspect cli-proxy-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"
echo "  resin 网络：  $(docker inspect resin --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"

echo
echo "=== C) 对照：同一 slot 打两次，确认是粘性的（同 IP） ==="
for i in 1 2; do
    probe slot-13 127.0.0.1 2260 "host#$i"
done
