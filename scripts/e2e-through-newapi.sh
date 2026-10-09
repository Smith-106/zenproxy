#!/usr/bin/env bash
# 最终端到端：经 new-api 调一次免费模型，验证
#   用户令牌 -> new-api 路由(abilities) -> 渠道 16 -> zenproxy:9000 -> Resin 出口 -> opencode
# 整条链路通。令牌从库里读，不回显。
set -u

API=http://127.0.0.1:3000
MODEL=${1:-big-pickle}

psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }

echo "=== 取一个启用的用户令牌（值不回显） ==="
# ★ deleted_at is null 不能漏：new-api 是 GORM 软删除，被删的令牌在库里还在，
#   但应用查不到（GORM 自动加 deleted_at IS NULL）。漏了这个条件会挑到死令牌，
#   然后拿到 401 Invalid token，白排查半天。
KEY=$(psql -c "select key from tokens where status=1 and deleted_at is null order by id limit 1;")
if [ -z "$KEY" ]; then echo "  没有可用的令牌，无法端到端验证" >&2; exit 1; fi
# 库里存的是裸键；new-api 收到请求会 TrimPrefix(token, "sk-") 再查库，
# 所以对外必须补回 sk- 前缀，否则查不到 -> 401 Invalid token。
case "$KEY" in sk-*) ;; *) KEY="sk-$KEY" ;; esac
echo "  已取到（${#KEY} 字符，含 sk- 前缀）"

echo
echo "=== 令牌额度设置（决定会不会被额度拦住） ==="
psql -F' | ' -c "select id, unlimited_quota, remain_quota, used_quota, status, coalesce(nullif(\"group\",''),'default') as grp from tokens where deleted_at is null order by id;" | sed 's/^/  /'

echo
echo "=== 该令牌所属分组 ==="
grp=$(psql -c "select coalesce(nullif(\"group\",''),'default') from tokens where status=1 order by id limit 1;")
echo "  group=$grp"

echo
echo "=== 经 new-api 调 $MODEL ==="
out=$(curl -sS -m 180 -w '|%{http_code}' -X POST "$API/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"What is 17*23? Answer with just the number.\"}],\"max_tokens\":600}" 2>&1)
code=${out##*|}
body=${out%|*}
echo "  HTTP=$code"
printf '%s' "$body" | python3 -c "
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: print('  非JSON: '+raw[:400]); sys.exit()
if 'error' in d:
    e=d['error']; print('  错误 type=%s message=%s' % (e.get('type'), str(e.get('message'))[:250])); sys.exit()
ch=(d.get('choices') or [{}])[0]; msg=ch.get('message') or {}
print('  模型: %s' % d.get('model'))
print('  内容: %r' % (msg.get('content') or '').strip()[:120])
print('  用量: %s' % d.get('usage'))
"

echo
echo "=== new-api 侧的调用日志（最近 3 条） ==="
psql -F' | ' -c "select id, to_timestamp(created_at) as t, model_name, channel_id, prompt_tokens, completion_tokens, use_time, coalesce(left(content,'40'),'') from logs order by id desc limit 3;" | sed 's/^/  /'

echo
echo "=== 渠道 16 的用量 ==="
psql -F' | ' -c "select id, name, status, used_quota from channels where id=16;" | sed 's/^/  /'
