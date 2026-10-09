#!/usr/bin/env bash
# 在把 zenproxy 填进任何聚合器之前，先验一遍这个 base URL 填对了没有。
#
# 为什么要单独有这个检查：填错的症状**完全没有指向性**。
# 少写 /v1 的请求不会得到「找不到路径」，而是掉进 zenproxy 的反向入口，
# 于是第一个路径段被当成账号名，你收到的是
#
#     unknown or disabled account: chat
#
# 看到这句话的人会去查账号配置，而账号配置没有问题。同一个错误在三个系统里
# 有三副面孔：LiteLLM 包成 NotFoundError，CPA 包成 500 server_error，
# new-api 那边则是渠道测试一直失败。它们都不会提到 base URL。
#
# 反过来，多写一个 /v1（比如 new-api 那条规则）会得到 /v1/v1/chat/completions，
# 一样是 404。所以「带不带 /v1」在两个方向上都错得起，只能实测。
#
# 判据：拿 <base>/models 探一次。
#   * 网关挂在 /v1 下，所以正确的写法会 200；
#   * 错误写法会掉进反向入口，返回一个说「账号」的错误。
#
# 用法: ./check-api-base.sh <base-url> <gateway-key>

set -uo pipefail

BASE="${1:?用法: check-api-base.sh <base-url> <gateway-key>}"
KEY="${2:?用法: check-api-base.sh <base-url> <gateway-key>}"
BASE="${BASE%/}"

probe() { # probe <url> -> "HTTPCODE<TAB>BODY"
  local out
  out=$(curl -s -m 20 -w $'\n%{http_code}' -H "Authorization: Bearer $KEY" "$1/models" 2>&1)
  printf '%s\t%s' "$(printf '%s' "$out" | tail -1)" "$(printf '%s' "$out" | head -c 220 | tr -d '\n')"
}

CODE="${BASE##*/v1}"
WITH_V1="${BASE%/v1}/v1"
WITHOUT_V1="${BASE%/v1}"

echo "== base URL 自检：$BASE =="
echo

IFS=$'\t' read -r C B <<<"$(probe "$BASE")"

if [ "$C" = "200" ]; then
  echo "  ✓ 这个 base URL 是对的（<base>/models → 200）"
  case "$BASE" in
    */v1) echo "      它带 /v1，适用于 LiteLLM / CLIProxyAPI / 反代模式。" ;;
    *)    echo "      它不带 /v1，适用于 new-api。" ;;
  esac
  exit 0
fi

echo "  ✗ <base>/models → HTTP $C"
echo "      $B"
echo
echo "  两个候选写法都试一下，看哪个对："
for cand in "$WITH_V1" "$WITHOUT_V1"; do
  [ "$cand" = "$BASE" ] && continue
  IFS=$'\t' read -r C2 B2 <<<"$(probe "$cand")"
  if [ "$C2" = "200" ]; then
    echo "      $cand  → 200   ← 应该用这个"
  else
    echo "      $cand  → $C2"
  fi
done
echo
echo "  提醒：这两个写法的差别在**两个方向上都错得起**——"
echo "  LiteLLM / CLIProxyAPI 要带 /v1，new-api 不能带。"
exit 1
