#!/usr/bin/env bash
# 端到端：经 zenproxy 调一次免费模型，验证免费额度套壳是否真的生效。
# 只看结论与关键字段，不打印账号密钥。
set -u

BASE=http://127.0.0.1:9000
ACCT=${1:-zen-01}
MODEL=${2:-big-pickle}

echo "=== 1) 非流式：$MODEL 经 $ACCT ==="
resp=$(curl -sS -m 120 -w '\n---HTTP=%{http_code} time=%{time_total}s exit_via_proxy---' \
  -X POST "$BASE/$ACCT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}],\"max_tokens\":64}" 2>&1)

body=$(printf '%s' "$resp" | sed 's/---HTTP=.*//')
meta=$(printf '%s' "$resp" | sed -n 's/.*---\(HTTP=.*\)---/\1/p')
echo "  $meta"
printf '%s' "$body" | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try:
    d=json.loads(raw)
except Exception:
    print('  非 JSON 响应，前 400 字：'); print('  '+raw[:400]); sys.exit()
if 'error' in d:
    e=d['error']
    print('  上游报错: type=%s message=%s' % (e.get('type'), str(e.get('message'))[:200]))
else:
    ch=(d.get('choices') or [{}])[0]
    msg=ch.get('message') or {}
    print('  模型 %s' % d.get('model'))
    print('  内容: %r' % (msg.get('content') or '')[:200])
    print('  finish_reason=%s usage=%s' % (ch.get('finish_reason'), d.get('usage')))
"

echo
echo "=== 2) 流式：$MODEL（免费额度只回 SSE，验证是否真的逐块到达） ==="
curl -sS -m 120 -N -D /tmp/h.txt \
  -X POST "$BASE/$ACCT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: STREAMOK\"}],\"max_tokens\":64}" \
  > /tmp/sse.txt 2>&1
echo "  响应头关键字段："
grep -iE '^(HTTP/|content-type|transfer-encoding|content-encoding)' /tmp/h.txt | sed 's/^/    /'
echo "  SSE 数据行数：$(grep -c '^data:' /tmp/sse.txt || echo 0)"
echo "  前 3 行："
grep '^data:' /tmp/sse.txt | head -3 | cut -c1-160 | sed 's/^/    /'
echo "  拼接后的文本："
python3 - <<'PY'
import json
out=[]
for line in open('/tmp/sse.txt', encoding='utf-8', errors='replace'):
    line=line.strip()
    if not line.startswith('data:'): continue
    p=line[5:].strip()
    if p=='[DONE]': break
    try: d=json.loads(p)
    except Exception: continue
    for c in d.get('choices') or []:
        out.append((c.get('delta') or {}).get('content') or '')
print('    %r' % ''.join(out)[:200])
PY

echo
echo "=== 3) 经代理时的出口 IP（对照） ==="
docker exec zenproxy sh -c 'wget -q -T 8 -O - http://127.0.0.1:9000/healthz' >/dev/null 2>&1 && echo "  zenproxy 容器内 /healthz 可达"
