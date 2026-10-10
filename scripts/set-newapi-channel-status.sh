#!/usr/bin/env bash
# 改 new-api 某个渠道的启用状态。
#
# 为什么需要单独的脚本：**改状态不能用 PUT /api/channel/**。
# 实测 PUT 的 body 里只要有 status，服务端就回
#   {"message":"Invalid parameters","success":false}
# （POST 建渠道时带 status 却完全正常）。
# 正确的端点是独立的一条：
#   POST /api/channel/{id}/status   body: {"status":1|2|3}
#
# status 取值：1 = 启用，2 = 手动禁用，3 = 自动禁用（上游故障后由 new-api 置位）。
#
# 用法：
#   scripts/set-newapi-channel-status.sh 17 2        # 禁用渠道 17
#   scripts/set-newapi-channel-status.sh 16 1        # 启用渠道 16
#   scripts/set-newapi-channel-status.sh --all      # 列出所有渠道与状态
#
# 环境变量：
#   API  默认 http://127.0.0.1:3000
set -uo pipefail

API=${API:-http://127.0.0.1:3000}
PSQL_CT=${PSQL_CT:-postgres}
PSQL_U=${PSQL_U:-root}
PSQL_D=${PSQL_D:-new-api}

psql() { sudo docker exec "$PSQL_CT" psql -U "$PSQL_U" -d "$PSQL_D" -t -A "$@"; }
sq() { printf '%s' "$1" | sed "s/'/''/g"; }

if [ "${1:-}" = "--all" ] || [ $# -eq 0 ]; then
    echo "=== 渠道状态（1 启用 / 2 手动禁用 / 3 自动禁用）==="
    psql -F'|' -c "select id, name, status from channels order by id;" 2>/dev/null | tr -d '\r' |
    while IFS='|' read -r i n s; do
        case "$s" in
            1) st=启用 ;;
            2) st=手动禁用 ;;
            3) st=自动禁用 ;;
            *) st="未知($s)" ;;
        esac
        printf "  %-4s %-42s %s\n" "$i" "$n" "$st"
    done
    [ $# -eq 0 ] && { echo; echo "用法: $0 <渠道ID> <1|2|3>"; exit 0; }
    exit 0
fi

CID=$1
WANT=${2:-}
case "$WANT" in
    1|2|3) ;;
    *) echo "错误: status 只能是 1(启用) / 2(禁用) / 3(自动禁用)" >&2; exit 2 ;;
esac

TOKEN=$(psql -c "select access_token from users where id=1;" 2>/dev/null | tr -d '\r')
[ -n "$TOKEN" ] || { echo "错误: users.access_token 为空，先用 create-newapi-channel.sh 生成" >&2; exit 1; }

BEFORE=$(psql -c "select status from channels where id = $(printf '%s' "$CID" | tr -cd '0-9');" 2>/dev/null | tr -d '\r')
[ -n "$BEFORE" ] || { echo "错误: 渠道 $CID 不存在" >&2; exit 1; }

echo "=== 渠道 $CID：$BEFORE -> $WANT ==="
code=$(curl -s -o /tmp/.setch -w '%{http_code}' -m 20 -X POST \
    -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" -H 'Content-Type: application/json' \
    --data-binary "{\"status\":$WANT}" "${API}/api/channel/${CID}/status")
echo "  POST /api/channel/${CID}/status -> HTTP $code"
head -c 200 /tmp/.setch | sed 's/^/    /'; echo
rm -f /tmp/.setch

AFTER=$(psql -c "select status from channels where id = $CID;" 2>/dev/null | tr -d '\r')
echo "  落库 status = $AFTER"
[ "$AFTER" = "$WANT" ] || { echo "  ⚠ 未生效（期望 $WANT，实际 $AFTER）" >&2; exit 1; }

echo
echo "=== 当前对外暴露的模型 ==="
psql -F'|' -c "
select a.model, string_agg(a.channel_id::text, ',' order by a.channel_id)
from abilities a join channels c on c.id = a.channel_id
where c.status = 1 and a.enabled
group by a.model order by a.model;" 2>/dev/null | tr -d '\r' | sed 's/^/  /'
echo "  （右列 = 能提供该模型的启用渠道，只有 1 个就是不冗余）"
