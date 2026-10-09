#!/usr/bin/env bash
# The contract a gateway has to satisfy before any aggregator will keep it.
#
# CLIProxyAPI, new-api and sub2api all import an upstream the same way: they
# probe /v1/models to learn what it serves, then send /v1/chat/completions and
# read the answer. When one of them drops an upstream it is almost never because
# a model was missing - it is because a response did not carry the fields the
# aggregator parses, and that shows up as an opaque "no auth available", or as a
# model that quietly disappears from the list.
#
# So this checks the shape, not the models, and it is deliberately independent
# of zenproxy's own test suite: it is what a *foreign* client sees. It can be
# pointed at any OpenAI-compatible gateway to find out whether importing it will
# work before wiring it into anything.
#
# Usage: ./verify-openai-compat-contract.sh <base-url> <api-key>

set -uo pipefail

BASE="${1:?usage: verify-openai-compat-contract.sh <base-url> <api-key>}"
KEY="${2:?usage: verify-openai-compat-contract.sh <base-url> <api-key>}"
BASE="${BASE%/}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0

# assertion <name> <python-expression>  — reads /tmp files: code, ctype, body, raw
assert() {
  local name="$1" expr="$2"
  printf '%s' "$name" > "$WORK/name"
  if python3 - "$WORK" "$expr" <<'PY'
import json, sys, pathlib
w = pathlib.Path(sys.argv[1])
expr = sys.argv[2]
code = int((w / "code").read_text().strip() or 0)
ctype = (w / "ctype").read_text().strip()
body = (w / "body").read_text(errors="replace")
raw = body
try:
    j = json.loads(body) if body.lstrip().startswith(("{", "[")) else None
except Exception:
    j = None
ok = bool(eval(expr))
name = (w / "name").read_text().strip()
mark = "  PASS  " if ok else "  FAIL  "
detail = "" if ok else "   <- HTTP %s %s" % (code, body[:200].replace("\n", " "))
print(mark + name + detail)
sys.exit(0 if ok else 1)
PY
  then PASS=$((PASS + 1)); else FAIL=$((FAIL + 1)); fi
}

run() { # run <name> <method> <path> <auth: key|bad|none> [data]
  printf '%s' "$1" > "$WORK/name"
  local method="$2" path="$3" auth="$4" data="${5:-}"
  local args=(-s -m 120 -o "$WORK/body" -w '%{http_code}|%{content_type}')
  [ "$auth" = "key" ] && args+=(-H "Authorization: Bearer $KEY")
  [ "$auth" = "bad" ] && args+=(-H "Authorization: Bearer sk-not-a-real-key")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
  local out; out=$(curl "${args[@]}" -X "$method" "$BASE$path")
  printf '%s' "${out%%|*}" > "$WORK/code"
  printf '%s' "${out##*|}" > "$WORK/ctype"
}

echo "== 网关对外契约验证：$BASE =="
echo

echo "-- 1. 模型列表：聚合器靠它学习上游有什么 --"
run "GET /v1/models 返回 200" GET /v1/models key
assert "/v1/models 返回 200" "code == 200"
assert "/v1/models 的 object 是 list" "j and j.get('object') == 'list'"
assert "data 是数组且非空" "j and isinstance(j.get('data'), list) and j['data']"
assert "每项有 id / object=model / created" \
  "j and all(m.get('id') and m.get('object') == 'model' and 'created' in m for m in j['data'])"
assert "每项有 owned_by（聚合器面板会显示）" \
  "j and all('owned_by' in m for m in j['data'])"
MODEL=$(python3 -c "
import json
try: d = json.load(open('$WORK/body'))
except Exception: d = {}
print((d.get('data') or [{}])[0].get('id') or '')
")
echo "     （用第一个模型做后续测试：${MODEL:-无}）"

if [ -n "$MODEL" ]; then
  echo
  echo "-- 2. 非流式对话：聚合器解析的就是这些字段 --"
  # Retried, because a pool of rotating exits will occasionally have one that is
  # refused or spent. The point here is the response shape, and a run that skips
  # the shape assertions because it drew a bad exit has verified nothing.
  for try in 1 2 3 4; do
    run "POST /v1/chat/completions" POST /v1/chat/completions key \
      "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: PONG\"}],\"stream\":false}"
    [ "$(cat "$WORK/code")" = "200" ] && break
    echo "     第 $try 次未得 200（HTTP $(cat "$WORK/code")），重试"
    sleep 2
  done
  assert "有响应且不是 5xx" "code in (200,401,403,429)"
  if [ "$(cat "$WORK/code")" = "200" ]; then
    assert "object=chat.completion" "j and j.get('object') == 'chat.completion'"
    assert "有 id / created / model" "j and j.get('id') and 'created' in j and j.get('model')"
    assert "choices[0] 有 message 与 finish_reason" \
      "j and j.get('choices') and 'message' in j['choices'][0] and 'finish_reason' in j['choices'][0]"
    assert "有 usage（计费与配额统计靠它）" "j and isinstance(j.get('usage'), dict)"
  else
    echo "     （重试后上游仍未给 200，跳过字段断言——这不是契约失败）"
  fi

  echo
  echo "-- 3. 流式：聚合器多半用 stream=true --"
  # Same retry reasoning as above, and the assertion is written so that a failed
  # request cannot satisfy it: "code != 200 or ..." would pass on a 502, which
  # means the run proves nothing while looking green.
  for try in 1 2 3 4; do
    run "POST /v1/chat/completions stream" POST /v1/chat/completions key \
      "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"count 1 to 3\"}],\"stream\":true}"
    [ "$(cat "$WORK/code")" = "200" ] && break
    echo "     第 $try 次未得 200（HTTP $(cat "$WORK/code")），重试"
    sleep 2
  done
  assert "流式返回 200 且是 text/event-stream" "code == 200 and 'text/event-stream' in ctype"
  [ "$(cat "$WORK/code")" = "200" ] && assert "以 data: [DONE] 收尾" "raw.rstrip().endswith('[DONE]')"
fi

echo
echo "-- 4. 失败形状：聚合器靠它决定冷却还是换 key --"
run "错误 key" GET /v1/models bad
assert "错误 key 得到 401（而不是 500）" "code == 401"
assert "401 是 JSON 错误对象" "j and 'error' in j"
run "无 key" GET /v1/models none
assert "无 key 得到 401" "code == 401"
run "未知模型" POST /v1/chat/completions key \
  '{"model":"definitely-not-a-real-model-xyz","messages":[],"stream":false}'
assert "未知模型是 4xx（不是 200，也不是崩）" "400 <= code < 500"
assert "未知模型的错误也是 JSON 对象" "j and 'error' in j"

echo
echo "-- 5. Responses API：新版聚合器会探这个 --"
run "POST /v1/responses" POST /v1/responses key "{\"model\":\"${MODEL:-x}\",\"input\":\"say PONG\"}"
# 404 or 501 would mean the endpoint is not implemented, which is a real contract
# gap: a newer aggregator probes this and would conclude the upstream is broken.
# 502/503 means it is implemented and the pool had nothing to serve it with,
# which is a different thing entirely and not this script's business.
assert "/v1/responses 已实现（非 404/501）" "code not in (404, 501)"

echo
echo "== 通过 $PASS，失败 $FAIL =="
if [ "$FAIL" -eq 0 ]; then
  echo "这份网关满足聚合器导入所需的对外契约。"
  exit 0
fi
echo "上面失败项要修好，才能被聚合器稳定导入。"
exit 1
