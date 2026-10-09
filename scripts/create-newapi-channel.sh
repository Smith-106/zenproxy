#!/usr/bin/env bash
# 在 new-api 里建一条指向 zenproxy 的渠道。
#
# 为什么走 API 而不是直接改库：new-api 用 abilities 表做模型路由，
# 建渠道时必须同步写 abilities（主键是 group+model+channel_id）。
# 直接 insert channels 会漏掉路由记录，渠道建了也用不了。
#
# 认证：new-api 的管理 API 要「系统访问令牌」。库里 root 用户原先没有，
# 这里生成一个写进去（值不回显）。要撤销：new-api 控制台 → 设置 → 系统访问令牌 → 清除。
set -euo pipefail

API=http://127.0.0.1:3000
CH_NAME=${CH_NAME:-"zenproxy (opencode free)"}
ZEN_ACCOUNT=${ZEN_ACCOUNT:-zen-01}
BASE_URL="http://zenproxy:9000/${ZEN_ACCOUNT}"
MODELS=${MODELS:-"big-pickle,ling-3.1-flash-free,mimo-v2.6-flash-free,space-bunny-free,step-5-preview-free,longcat-2.5-preview-free,nemotron-3.5-lightning-free"}

psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }

echo "=== 0) 是否已存在同名渠道 ==="
existing=$(psql -c "select count(*) from channels where name = '${CH_NAME//\'/\'\'}';")
echo "  同名渠道数：$existing"
if [ "$existing" != "0" ]; then
    echo "  已存在，改为更新而不是新建。"
fi

echo
echo "=== 1) 准备系统访问令牌 ==="
TOKEN=$(psql -c "select access_token from users where id=1;")
if [ -z "$TOKEN" ]; then
    # users.access_token 是 character(32)，必须正好 32 个字符
    TOKEN=$(openssl rand -hex 16)
    psql -c "update users set access_token = '${TOKEN}' where id = 1;" >/dev/null
    echo "  原无令牌，已生成并写入（${#TOKEN} 字符，值不回显）"
    WROTE=1
else
    echo "  已有令牌（${#TOKEN} 字符），直接复用"
    WROTE=0
fi

echo
echo "=== 2) 探测管理 API 是否认这个令牌 ==="
code=$(curl -s -o /tmp/ch_list.json -w '%{http_code}' -m 15 \
    -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
    "${API}/api/channel/?p=0&page_size=5")
echo "  GET /api/channel/ -> HTTP $code"
if [ "$code" != "200" ]; then
    if [ "$WROTE" = "1" ]; then
        echo "  令牌刚写入，缓存未刷新 —— 重启 new-api 后重试"
        docker restart new-api >/dev/null
        sleep 12
        code=$(curl -s -o /tmp/ch_list.json -w '%{http_code}' -m 15 \
            -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
            "${API}/api/channel/?p=0&page_size=5")
        echo "  重启后重试 -> HTTP $code"
    fi
fi
if [ "$code" != "200" ]; then
    echo "  管理 API 仍不可用，响应前 300 字：" >&2
    head -c 300 /tmp/ch_list.json >&2; echo >&2
    exit 1
fi
head -c 160 /tmp/ch_list.json | sed 's/^/    /'; echo

echo
echo "=== 3) 建渠道 ==="
# 请求体形状是从源码里核出来的，不是猜的：
#   POST /api/channel/ -> controller.AddChannel 绑定 AddChannelRequest，
#                         渠道必须包在 "channel" 字段里（顶层 mode="single"）
#   PUT  /api/channel/ -> controller.UpdateChannel 绑定 PatchChannel，
#                         是**裸**的 channel 对象，且必须带 id
CHANNEL_JSON=$(cat <<JSON
{
  "type": 1,
  "name": "${CH_NAME}",
  "key": "zenproxy-injects-real-key",
  "base_url": "${BASE_URL}",
  "models": "${MODELS}",
  "group": "default",
  "model_mapping": "",
  "priority": 0,
  "weight": 1,
  "status": 1,
  "auto_ban": 0,
  "remark": "经 zenproxy 走 Resin 出口池的 opencode 免费额度；密钥由 zenproxy 按账号注入，这里的 key 是占位值"
}
JSON
)
if [ "$existing" != "0" ]; then
    cid=$(psql -c "select id from channels where name = '${CH_NAME}' limit 1;")
    payload=$(printf '{"id":%s,%s' "$cid" "$(printf '%s' "$CHANNEL_JSON" | tail -n +2)")
    code=$(curl -s -o /tmp/ch_new.json -w '%{http_code}' -m 20 -X PUT \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" -H 'Content-Type: application/json' \
        -d "${payload}" "${API}/api/channel/")
    echo "  PUT /api/channel/ (id=$cid) -> HTTP $code"
else
    payload=$(printf '{"mode":"single","channel":%s}' "$CHANNEL_JSON")
    code=$(curl -s -o /tmp/ch_new.json -w '%{http_code}' -m 20 -X POST \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" -H 'Content-Type: application/json' \
        -d "${payload}" "${API}/api/channel/")
    echo "  POST /api/channel/ -> HTTP $code"
fi
head -c 300 /tmp/ch_new.json | sed 's/^/    /'; echo

# 兜底：万一 abilities 没跟上，用官方修复接口重建一次
if ! psql -c "select count(*) from abilities a join channels c on c.id=a.channel_id where c.name = '${CH_NAME}';" | grep -qv '^0$'; then
    echo "  abilities 未同步，调用官方修复接口 /api/channel/fix"
    curl -s -m 30 -X POST -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
        "${API}/api/channel/fix" | head -c 200 | sed 's/^/    /'; echo
fi

echo
echo "=== 4) 落库结果 ==="
psql -F' | ' -c "select id, name, status, base_url, \"group\", weight, auto_ban from channels where name = '${CH_NAME}';" | sed 's/^/  /'

echo
echo "=== 5) abilities 路由是否同步（这是渠道能不能被选中的关键） ==="
psql -F' | ' -c "select a.\"group\", a.model, a.channel_id, a.enabled from abilities a join channels c on c.id=a.channel_id where c.name = '${CH_NAME}' order by a.model;" | sed 's/^/  /'

echo
echo "=== 6) 渠道里的模型数 ==="
psql -c "select count(*) from abilities a join channels c on c.id=a.channel_id where c.name = '${CH_NAME}';" | sed 's/^/  /'

rm -f /tmp/ch_list.json /tmp/ch_new.json
