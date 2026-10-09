#!/usr/bin/env bash
# 定位 new-api -> CPA 的 404：看 CPA 收到了什么路径。
set -u

echo "=== CPA 日志目录 ==="
sudo ls -lat /opt/cli-proxy-api/logs/ 2>/dev/null | head -8 | sed 's/^/  /'

echo
echo "=== CPA 最近日志里与 404 / 请求路径相关的行 ==="
for f in $(sudo ls -t /opt/cli-proxy-api/logs/*.log 2>/dev/null | head -2); do
    echo "  --- $f ---"
    sudo grep -aE '404|chat/completions|not found|path' "$f" 2>/dev/null | tail -15 | sed 's/^/    /'
done

echo
echo "=== 直接在新-api 容器里按渠道 base_url 打一次，看真实响应 ==="
KEY=$(sudo python3 - /opt/cli-proxy-api/config.yaml <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
m = re.search(r'(?m)^access:\n(?:.*\n)*?^    api-keys:\n((?:^        - .+\n)+)', t)
if m:
    for l in m.group(1).splitlines():
        v = l.strip().lstrip('- ').strip().strip('"\'')
        if v: print(v); break
PY
)
for path in "/v1/chat/completions" "/chat/completions" "/v1/v1/chat/completions"; do
    printf '  %-28s ' "$path"
    docker exec -e "K=$KEY" new-api sh -c \
      "wget -q -O /tmp/r.out -T 20 --header='Authorization: Bearer \$K' --header='Content-Type: application/json' \
       --post-data='{\"model\":\"gpt-oss:20b\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":64}' \
       http://cli-proxy-api:8317$path && head -c 150 /tmp/r.out || echo FAIL" 2>&1 | tr '\n' ' '
    echo
    docker exec new-api sh -c 'rm -f /tmp/r.out'
done
