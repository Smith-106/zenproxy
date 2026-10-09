#!/usr/bin/env bash
# 全量扫描 24 个 slot：出口 IP、国家、建连延迟。为「该选哪些 slot」提供依据。
set -u
TOKEN=$(sed -n 's/^RESIN_PROXY_TOKEN=//p' /opt/zenproxy/.env)

echo "=== 24 个 slot 全扫（延迟取 api.ipify.org 的 total） ==="
printf '  %-10s %-8s %-18s %-6s %s\n' "slot" "状态" "出口IP" "延迟" "国家"
for i in $(seq -w 1 24); do
    s="slot-$i"
    cfg=$(mktemp); chmod 600 "$cfg"
    printf 'proxy = "socks5h://Default.%s:%s@127.0.0.1:2260"\n' "$s" "$TOKEN" > "$cfg"
    out=$(curl -s -m 20 -K "$cfg" -o /tmp/ip.out -w '%{time_total}' https://api.ipify.org 2>/dev/null)
    ip=$(cat /tmp/ip.out 2>/dev/null | tr -d '\n')
    rm -f "$cfg"
    if [ -z "$ip" ]; then
        printf '  %-10s %-8s %-18s %-6s %s\n' "$s" "超时" "-" "-" "-"
        continue
    fi
    cc="?"
    case "$ip" in
        34.132.*|65.49.*|72.167.*|23.129.*|129.153.*|162.214.*) cc="US";;
        144.124.*|79.137.*|84.17.*) cc="NL";;
        80.15.*) cc="FR";;
        87.242.*) cc="RU";;
        134.209.*) cc="GB";;
        112.78.*) cc="ID";;
        154.37.*) cc="HK";;
        146.70.*|68.178.*) cc="JP";;
        210.211.*) cc="VN";;
        193.176.*) cc="RO";;
        139.59.*) cc="IN";;
        185.243.*) cc="NO";;
    esac
    # 用 ip-api 兜底确认
    real=$(curl -s -m 8 "http://ip-api.com/json/$ip?fields=countryCode" 2>/dev/null | sed -n 's/.*"countryCode":"\([A-Z]*\)".*/\1/p')
    [ -n "$real" ] && cc="$real"
    mark="ok"
    awk -v t="$out" 'BEGIN{ if (t+0 > 6) exit 1 }' || mark="慢"
    printf '  %-10s %-8s %-18s %-6s %s\n' "$s" "$mark" "$ip" "${out}s" "$cc"
done
rm -f /tmp/ip.out
