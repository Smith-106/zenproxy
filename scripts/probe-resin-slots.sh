#!/usr/bin/env bash
# 实测 Resin 粘性身份的可用率：逐个 slot 经 socks5h 取出口 IP。
# 只读，不改任何状态。凭据从 resin 容器读取，不打印。
set -u

TOK=$(docker inspect resin --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^RESIN_PROXY_TOKEN=//p')
if [ -z "$TOK" ]; then echo "ERROR: no RESIN_PROXY_TOKEN in resin container" >&2; exit 1; fi

N=${1:-12}
START=$(date +%s)
ok=0
for i in $(seq -w 1 "$N"); do
  acct="slot-$i"
  out=$(curl -sS --max-time 25 -x "socks5h://Default.$acct:$TOK@127.0.0.1:2260" \
        -w '|%{http_code}|%{time_total}' https://api.ipify.org 2>&1 | tail -1)
  body=${out%%|*}
  rest=${out#*|}
  code=${rest%%|*}
  secs=${rest##*|}
  if [ "$code" = "200" ] && printf '%s' "$body" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
    printf '  %-9s OK   ip=%-16s %ss\n' "$acct" "$body" "$secs"
    ok=$((ok + 1))
  else
    printf '  %-9s FAIL %s %ss\n' "$acct" "${body:0:58}" "$secs"
  fi
done

echo
echo "可用 $ok / $N （耗时 $(( $(date +%s) - START ))s）"
