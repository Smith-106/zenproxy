#!/usr/bin/env bash
# 在 new-api 里建/更新一条渠道。泛化版：zenproxy、CLIProxyAPI 等都用它。
#
# 为什么走 API 而不是直接改库：new-api 用 abilities 表做模型路由
# （主键 group+model+channel_id）。直接 insert channels 会漏掉路由记录，
# 渠道建了也永远不会被选中。管理 API 会事务性地一起写。
#
# 认证：new-api 的管理 API 要「系统访问令牌」= users.access_token
# （character(32)，必须正好 32 字符）。没有就生成一个写进去。
# 撤销：new-api 控制台 → 设置 → 系统访问令牌 → 清除。
#
# 用法（全部可用环境变量覆盖）：
#   CH_NAME      渠道名
#   CH_BASE_URL  上游 Base URL
#   CH_KEY       渠道密钥字面值
#   CH_KEY_CMD   取密钥的命令（stdout 即密钥）；优先于 CH_KEY，
#                好处是密钥不进命令行参数，不会出现在 ps 里
#   CH_MODELS    逗号分隔的模型名；留空则从 CH_BASE_URL/../v1/models 自动拉
#   CH_AUTO_BAN  1/0，默认 0（上游抖动时别让 new-api 把渠道静默停掉）
#   CH_GROUP     分组，默认 default
#
# 例：
#   sudo CH_NAME="zenproxy (opencode free)" \
#        CH_BASE_URL="http://zenproxy:9000/zen-01" \
#        CH_KEY="zenproxy-injects-real-key" \
#        CH_MODELS="big-pickle,ling-3.1-flash-free" \
#        scripts/create-newapi-channel.sh
set -euo pipefail

API=http://127.0.0.1:3000
CH_NAME=${CH_NAME:-"zenproxy (opencode free)"}
CH_BASE_URL=${CH_BASE_URL:-"http://zenproxy:9000/zen-01"}
CH_KEY=${CH_KEY:-"zenproxy-injects-real-key"}
CH_MODELS=${CH_MODELS:-""}
CH_AUTO_BAN=${CH_AUTO_BAN:-0}
CH_GROUP=${CH_GROUP:-default}
CH_REMARK=${CH_REMARK:-""}

psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
sq() { printf '%s' "$1" | sed "s/'/''/g"; }          # SQL 字面量转义
jq_esc() { python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))'; }  # JSON 字符串转义

echo "=== 0) 渠道：$CH_NAME ==="
echo "  base_url = $CH_BASE_URL"
existing=$(psql -c "select count(*) from channels where name = '$(sq "$CH_NAME")';")
echo "  同名渠道数：$existing"

echo
echo "=== 1) 准备管理令牌 ==="
TOKEN=$(psql -c "select access_token from users where id=1;")
WROTE=0
if [ -z "$TOKEN" ]; then
    TOKEN=$(openssl rand -hex 16)   # 必须正好 32 字符
    psql -c "update users set access_token = '${TOKEN}' where id = 1;" >/dev/null
    echo "  原无令牌，已生成并写入（${#TOKEN} 字符，值不回显）"
    WROTE=1
else
    echo "  已有令牌（${#TOKEN} 字符），复用"
fi

echo
echo "=== 2) 准备渠道密钥 ==="
if [ -n "${CH_KEY_CMD:-}" ]; then
    CH_KEY=$(eval "$CH_KEY_CMD")
    echo "  由 CH_KEY_CMD 提供，长度 ${#CH_KEY}，前缀 ${CH_KEY:0:7}…（值不回显）"
else
    echo "  字面值，长度 ${#CH_KEY}"
fi
[ -n "$CH_KEY" ] || { echo "  密钥为空，拒绝继续" >&2; exit 1; }

echo
echo "=== 3) 准备模型清单 ==="
if [ -z "$CH_MODELS" ]; then
    echo "  未指定，从上游 /v1/models 拉取"
    # ★ CH_BASE_URL 是给 new-api **容器**用的地址（多为容器名），宿主机往往解析不了。
    #   拉清单是脚本在宿主机做的事，所以要能把地址分开指定。
    if [ -n "${CH_MODELS_URL:-}" ]; then
        models_url="$CH_MODELS_URL"
    else
        base_root=${CH_BASE_URL%/}
        case "$base_root" in
            */v1) models_url="${base_root}/models" ;;
            *)    models_url="${base_root}/v1/models" ;;
        esac
    fi
    echo "  GET $models_url"
    CH_MODELS=$(curl -s -m 20 -H "Authorization: Bearer $CH_KEY" "$models_url" \
        | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit('上游没返回 JSON（宿主机可能解析不了这个主机名，用 CH_MODELS_URL 指到 127.0.0.1）')
ids=[m.get('id') for m in (d.get('data') or []) if m.get('id')]
print(','.join(ids))
")
    [ -n "$CH_MODELS" ] || { echo "  拉不到模型，用 CH_MODELS 手工指定" >&2; exit 1; }
fi
n_models=$(printf '%s' "$CH_MODELS" | tr ',' '\n' | grep -c . || true)
echo "  模型数：$n_models"

echo
echo "=== 4) 探测管理 API ==="
code=$(curl -s -o /tmp/ch_list.json -w '%{http_code}' -m 15 \
    -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
    "${API}/api/channel/?p=0&page_size=5")
echo "  GET /api/channel/ -> HTTP $code"
if [ "$code" != "200" ] && [ "$WROTE" = "1" ]; then
    echo "  令牌刚写入，缓存未刷新 —— 重启 new-api 后重试"
    docker restart new-api >/dev/null; sleep 12
    code=$(curl -s -o /tmp/ch_list.json -w '%{http_code}' -m 15 \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
        "${API}/api/channel/?p=0&page_size=5")
    echo "  重启后 -> HTTP $code"
fi
[ "$code" = "200" ] || { echo "  管理 API 不可用：" >&2; head -c 300 /tmp/ch_list.json >&2; exit 1; }

echo
echo "=== 5) 建/更新渠道 ==="
# 请求体形状是核过源码的，不是猜的：
#   POST /api/channel/ -> AddChannel 绑定 AddChannelRequest，渠道包在 "channel" 里
#   PUT  /api/channel/ -> UpdateChannel 绑定 PatchChannel，是裸对象且必须带 id
CHANNEL_JSON=$(python3 - "$CH_NAME" "$CH_KEY" "$CH_BASE_URL" "$CH_MODELS" "$CH_GROUP" "$CH_AUTO_BAN" "$CH_REMARK" <<'PY'
import json, sys
name, key, base, models, group, autoban, remark = sys.argv[1:8]
print(json.dumps({
    "type": 1,
    "name": name,
    "key": key,
    "base_url": base,
    "models": models,
    "group": group,
    "model_mapping": "",
    "priority": 0,
    "weight": 1,
    "auto_ban": int(autoban),
    "remark": remark,
}, ensure_ascii=False))
PY
)
# ★ 用 --data-binary @- 从 stdin 传，密钥不进 argv（否则会短暂出现在 ps 里）
if [ "$existing" != "0" ]; then
    cid=$(psql -c "select id from channels where name = '$(sq "$CH_NAME")' limit 1;")
    # ★ 更新路径**不能带 status**：实测 PUT 的 body 里只要有 status，
    #   服务端就回 {"message":"Invalid parameters","success":false}（POST 带 status 却正常）。
    #   不带 status 时其余字段照常更新，而且已发布的分级/启用状态会被保留 —— 正好是我们想要的。
    # ★ 想**主动改**启用状态，用独立端点，既不是 PUT 也不是 POST /api/channel/：
    #     POST /api/channel/{id}/status   {"status":1|2|3}   （1 启用 / 2 手动禁用 / 3 自动禁用）
    #   封装在 scripts/set-newapi-channel-status.sh。⚠ 它**不是只读的**，探测即生效。
    payload=$(python3 - "$cid" "$CHANNEL_JSON" <<'PY'
import json, sys
cid, chan = sys.argv[1], json.loads(sys.argv[2])
chan.pop("status", None)
print(json.dumps({"id": int(cid), **chan}, ensure_ascii=False))
PY
)
    code=$(printf '%s' "$payload" | curl -s -o /tmp/ch_new.json -w '%{http_code}' -m 20 -X PUT \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" -H 'Content-Type: application/json' \
        --data-binary @- "${API}/api/channel/")
    echo "  PUT (id=$cid) -> HTTP $code"
else
    payload=$(printf '{"mode":"single","channel":%s}' "$(python3 - "$CHANNEL_JSON" <<'PY'
import json, sys
chan = json.loads(sys.argv[1])
chan["status"] = 1
print(json.dumps(chan, ensure_ascii=False))
PY
)")
    code=$(printf '%s' "$payload" | curl -s -o /tmp/ch_new.json -w '%{http_code}' -m 20 -X POST \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" -H 'Content-Type: application/json' \
        --data-binary @- "${API}/api/channel/")
    echo "  POST -> HTTP $code"
fi
head -c 300 /tmp/ch_new.json | sed 's/^/    /'; echo

echo
echo "=== 6) 落库结果 ==="
psql -F' | ' -c "select id, name, status, base_url, \"group\", weight, auto_ban from channels where name = '$(sq "$CH_NAME")';" | sed 's/^/  /'

echo
echo "=== 7) abilities 路由 ==="
cnt=$(psql -c "select count(*) from abilities a join channels c on c.id=a.channel_id where c.name = '$(sq "$CH_NAME")';")
echo "  路由条数：$cnt （0 = 渠道不会被选中）"
if [ "$cnt" = "0" ]; then
    echo "  abilities 未同步，调用官方修复接口 /api/channel/fix"
    curl -s -m 60 -X POST -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
        "${API}/api/channel/fix" | head -c 200 | sed 's/^/    /'; echo
    cnt=$(psql -c "select count(*) from abilities a join channels c on c.id=a.channel_id where c.name = '$(sq "$CH_NAME")';")
    echo "  修复后路由条数：$cnt"
fi
psql -F' | ' -c "select a.model, a.channel_id, a.enabled from abilities a join channels c on c.id=a.channel_id where c.name = '$(sq "$CH_NAME")' order by a.model limit 30;" | sed 's/^/  /'

rm -f /tmp/ch_list.json /tmp/ch_new.json
