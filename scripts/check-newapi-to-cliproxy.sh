#!/usr/bin/env bash
# 从 new-api 容器内部确认能不能真的够到 cli-proxy-api:8317。
set -u

echo "=== 1) wget 直连（把 HTTP 状态也打出来） ==="
docker exec new-api sh -c 'wget -S -O /tmp/probe.out -T 10 http://cli-proxy-api:8317/v1/models 2>&1 | head -12; echo "  exit=$?"'
echo "  --- 响应体前 200 字节 ---"
docker exec new-api sh -c 'head -c 200 /tmp/probe.out 2>/dev/null; echo' | sed 's/^/    /'
docker exec new-api sh -c 'rm -f /tmp/probe.out'

echo
echo "=== 2) 带正确入站 key 的完整调用（key 从 CPA 配置读，不回显） ==="
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
if [ -n "$KEY" ]; then
    docker exec -e "K=$KEY" new-api sh -c \
      'wget -q -O /tmp/m.json -T 15 --header="Authorization: Bearer $K" http://cli-proxy-api:8317/v1/models && echo "  拉取成功" || echo "  拉取失败"' 2>&1
    docker exec new-api sh -c 'python3 -c "
import json
d=json.load(open(\"/tmp/m.json\"))
print(\"  模型数: %d\" % len(d.get(\"data\") or []))
" 2>/dev/null || head -c 200 /tmp/m.json' 2>&1 | sed 's/^/  /'
    docker exec new-api sh -c 'rm -f /tmp/m.json'
else
    echo "  没取到 key"
fi
