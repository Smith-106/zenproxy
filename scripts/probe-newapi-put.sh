#!/usr/bin/env bash
# 实测 PUT /api/channel/ 到底哪个字段触发 "Invalid parameters"。
# 只做试探性请求；失败不会改动渠道（校验不过就不落库）。
set -u

API=http://127.0.0.1:3000
psql() { docker exec postgres psql -U root -d new-api -t -A "$@"; }
TOKEN=$(psql -c "select access_token from users where id=1;")

CID=$(psql -c "select id from channels where name='zenproxy (opencode free)' limit 1;")
echo "目标渠道 id=$CID"

probe() {
    local label="$1" body="$2"
    local resp
    resp=$(printf '%s' "$body" | curl -s -m 20 -X PUT \
        -H "Authorization: Bearer ${TOKEN}" -H "New-Api-User: 1" \
        -H 'Content-Type: application/json' --data-binary @- "${API}/api/channel/")
    printf '  %-34s -> %s\n' "$label" "$(printf '%s' "$resp" | head -c 130)"
}

probe "只有 id" "{\"id\":$CID}"
probe "id + name" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\"}"
probe "id + name + key" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\"}"
probe "id + name + key + type" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1}"
probe "加 base_url" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\"}"
probe "加 models" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\"}"
probe "加 group" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\"}"
probe "加 weight" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1}"
probe "加 status" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1,\"status\":1}"
probe "加 auto_ban" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1,\"status\":1,\"auto_ban\":0}"
probe "加 model_mapping" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1,\"status\":1,\"auto_ban\":0,\"model_mapping\":\"\"}"
probe "加 priority" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1,\"status\":1,\"auto_ban\":0,\"model_mapping\":\"\",\"priority\":0}"
probe "加 remark" "{\"id\":$CID,\"name\":\"zenproxy (opencode free)\",\"key\":\"zenproxy-injects-real-key\",\"type\":1,\"base_url\":\"http://zenproxy:9000/zen-01\",\"models\":\"big-pickle\",\"group\":\"default\",\"weight\":1,\"status\":1,\"auto_ban\":0,\"model_mapping\":\"\",\"priority\":0,\"remark\":\"测试\"}"

echo
echo "=== 每次试探后渠道的实际状态（看有没有被改动） ==="
psql -F' | ' -c "select id, name, base_url, models, weight, auto_ban, status from channels where id=$CID;" | sed 's/^/  /'
