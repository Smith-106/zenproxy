#!/usr/bin/env bash
# 可行性关键测试：真实 LLM 调用经 Resin 住宅出口，延迟/吞吐能不能用？
# 对比「服务器直连」与「经 slot-13(HK) / slot-15(JP) / slot-21(JP)」。
# 用 ollama.com 的免费 key（配置里那 11 把），请求很小。
# 凭据一律走 0600 的 curl -K 配置，不进 argv。
set -u

TOKEN=$(sed -n 's/^RESIN_PROXY_TOKEN=//p' /opt/zenproxy/.env)
# 取第一把 ollama free 的 key（不回显）
OKEY=$(sudo python3 - <<'PY'
import json, re
t = open('/opt/cli-proxy-api/config.yaml', encoding='utf-8', errors='replace').read()
m = re.search(r'"name": "ollama free".*?\]', t, re.S)
if not m: raise SystemExit
seg = t[max(0, m.start()-4000):m.start()]
ks = re.findall(r'"api-key": "([^"]+)"', seg)
if ks: print(ks[0])
PY
)
[ -n "$OKEY" ] || { echo "取不到 ollama free 的 key"; exit 1; }
echo "  ollama free key 已取到（长度 ${#OKEY}，不回显）"

BODY='{"model":"gpt-oss:20b","messages":[{"role":"user","content":"Count from 1 to 20."}],"max_tokens":200,"stream":false}'

run() {
    local label="$1" proxyslot="$2"
    local cfg
    cfg=$(mktemp); chmod 600 "$cfg"
    echo "url = \"https://ollama.com/v1/chat/completions\"" >> "$cfg"
    echo "header = \"Authorization: Bearer $OKEY\"" >> "$cfg"
    echo "header = \"Content-Type: application/json\"" >> "$cfg"
    if [ -n "$proxyslot" ]; then
        printf 'proxy = "socks5h://Default.%s:%s@127.0.0.1:2260"\n' "$proxyslot" "$TOKEN" >> "$cfg"
    fi
    local t0 t1 out
    t0=$(date +%s.%N)
    out=$(curl -s -m 90 -K "$cfg" \
        -w '\n__META__%{http_code}|%{time_connect}|%{time_starttransfer}|%{time_total}|%{size_download}' \
        -d "$BODY" 2>&1)
    t1=$(date +%s.%N)
    rm -f "$cfg"
    local meta code tc tst tt sz
    meta=$(printf '%s' "$out" | grep -o '__META__.*$' | sed 's/__META__//')
    IFS='|' read -r code tc tst tt sz <<< "$meta"
    printf '  %-16s HTTP=%-4s connect=%-7s TTFB=%-7s total=%-7s bytes=%s\n' \
        "$label" "${code:-?}" "${tc:-?}" "${tst:-?}" "${tt:-?}" "${sz:-?}"
    printf '%s' "$out" | sed 's/__META__.*//' | head -c 150 | sed 's/^/      /'
    echo
}

echo
echo "=== 对照：服务器直连 ==="
run "直连(机房IP)" ""

echo
echo "=== 经 Resin 住宅出口 ==="
for s in slot-13 slot-15 slot-21 slot-07; do
    run "$s" "$s"
done

echo
echo "=== 参考：服务器 -> 各自出口的 TCP 建连延迟 ==="
for s in slot-13 slot-15 slot-21; do
    cfg=$(mktemp); chmod 600 "$cfg"
    printf 'proxy = "socks5h://Default.%s:%s@127.0.0.1:2260"\n' "$s" "$TOKEN" > "$cfg"
    printf '  %-10s ' "$s"
    curl -s -o /dev/null -m 25 -K "$cfg" -w 'connect=%{time_connect}s total=%{time_total}s\n' https://api.ipify.org
    rm -f "$cfg"
done
