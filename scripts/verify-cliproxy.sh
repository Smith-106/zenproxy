#!/usr/bin/env bash
# 验证 cli-proxy-api 真能用，并确认 new-api 够不够得着它。凭据不回显。
set -u

CFG=/opt/cli-proxy-api/config.yaml

echo "=== 取入站 api-key（access.api-keys 下第一条，值不回显） ==="
KEY=$(sudo python3 - "$CFG" <<'PY'
import re, sys
t = open(sys.argv[1], encoding='utf-8', errors='replace').read()
# access: 段下的 api-keys 列表
m = re.search(r'(?m)^access:\n(?:.*\n)*?^    api-keys:\n((?:^        - .+\n)+)', t)
if m:
    for l in m.group(1).splitlines():
        v = l.strip().lstrip('- ').strip().strip('"\'')
        if v: print(v); break
PY
)
if [ -z "$KEY" ]; then echo "  没取到，放弃" >&2; exit 1; fi
echo "  取到一条，长度 ${#KEY}，前缀 ${KEY:0:7}…"

echo
echo "=== 宿主机直连 CPA /v1/models ==="
curl -s -m 20 -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models \
    | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('  非JSON: '+raw[:300]); sys.exit()
data=d.get('data') or []
print('  模型总数: %d' % len(data))
for m in data[:40]: print('    %s' % m.get('id'))
if len(data)>40: print('    … 还有 %d 个' % (len(data)-40))
" 2>&1 | sed 's/^/  /'

echo
echo "=== 宿主机直连 CPA 真跑一次推理 ==="
curl -sS -m 90 -w '\n  ---HTTP=%{http_code} time=%{time_total}s---\n' \
    -X POST http://127.0.0.1:8317/v1/chat/completions \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"model":"nvidia/nemotron-3.5-lightning-30b-a3b","messages":[{"role":"user","content":"Say OK"}],"max_tokens":64}' 2>&1 \
    | python3 -c "
import json,sys
raw=sys.stdin.read()
meta=''
if '---HTTP=' in raw:
    i=raw.index('---HTTP='); meta=raw[i:].strip(); raw=raw[:i]
raw=raw.strip()
try: d=json.loads(raw)
except Exception: print('  非JSON: '+raw[:300]); print('  '+meta); sys.exit()
print('  '+meta)
if 'error' in d:
    e=d['error']; print('  错误: %s' % str(e.get('message'))[:200])
else:
    ch=(d.get('choices') or [{}])[0]; m=ch.get('message') or {}
    print('  模型=%s 内容=%r' % (d.get('model'), (m.get('content') or '').strip()[:60]))
" 2>&1 | sed 's/^/  /'

echo
echo "=== new-api 能不能够到 CPA（网络连通性） ==="
echo "  new-api 的网络: $(docker inspect new-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"
echo "  CPA 的网络:     $(docker inspect cli-proxy-api --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')"
echo "  --- 从 new-api 容器内解析 cli-proxy-api ---"
docker exec new-api sh -c 'getent hosts cli-proxy-api || echo "    解析失败（不在同一个网络）"'
echo "  --- 从 new-api 容器内试连 8317 ---"
docker exec new-api sh -c 'wget -q -T 6 -O - http://cli-proxy-api:8317/v1/models 2>&1 | head -c 120 || echo "    连不上"'
echo
