#!/usr/bin/env bash
# 免费面经 CPA 的端到端验收。
#
# 这不是"配置检查"，是**真实流量**验收：把每个模型按客户端名字打一遍 CPA 的
# /v1/chat/completions，要求 200。2026-10-17 那次 `400 unknown provider` 的修复就是
# 靠它收口的（12/12），只改配置不看流量等于没验。
#
# 为什么每个模型重试 3 次：免费面单发成功率约 50%（上游按出口 IP 信誉放行，回
# 403 FreeTierError）。单次失败不代表模型不可用；`--strict` 才要求首次即 200，
# 用来验证重试/冷却调优是否生效（改前 3 次全 503，改后 10/10）。
#
# 用法：
#   scripts/e2e-cpa-free-lane.sh                 # 默认模型清单，每个最多试 3 次
#   scripts/e2e-cpa-free-lane.sh --strict        # 每个只打 1 次，用于验重试调优
#   scripts/e2e-cpa-free-lane.sh --stability 10  # **单个**模型连打 10 次，要求 200=10/10
#                                                # （默认 muse-spark-1.2-…，可用 MODEL= 指定）
#   MODELS="a,b" scripts/e2e-cpa-free-lane.sh    # 指定模型
#
# 退出码：0 全通过；1 有模型未通过；2 环境问题（CPA 不通 / 取不到 key）。
#
# 密钥纪律：key 从 config.yaml 里读进变量，**不回显**；本脚本不打印任何凭据。
set -uo pipefail

CFG=${CFG:-/opt/cli-proxy-api/config.yaml}
BASE=${BASE:-http://127.0.0.1:8317}
STRICT=0
STABILITY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --strict) STRICT=1 ;;
    --stability) STABILITY=${2:-10}; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
  shift
done

MODELS=${MODELS:-"muse-spark-1.2-contributor-free,muse-spark-1.3-contributor-free,glm-5.3-flash-free,glm-5.3-flash,big-pickle,deepseek-v4.1-flash-free,mimo-v2.6-flash-free,space-bunny-free,beam-free,longcat-2.5-preview-free,nemotron-3.5-lightning-free,nemotron-3.5-super-vl-free"}

# --stability 只量一个模型的抖动（12 个模型各打 10 次 = 120 个请求，免费面要跑很久）。
if [ "$STABILITY" -gt 0 ]; then
  MODELS=${MODEL:-muse-spark-1.2-contributor-free}
  echo "稳定性模式: model=$MODELS 次数=$STABILITY"
fi

CPAK=$(sudo python3 - "$CFG" <<'PY' 2>/dev/null || true
import sys, yaml
d = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
v = (d.get("access") or {}).get("api-keys") or []
print(v[0] if v and isinstance(v[0], str) else "")
PY
)
if [ -z "${CPAK}" ]; then
  echo "取不到 CPA 入站 key（$CFG 的 access.api-keys[0]）" >&2
  exit 2
fi

if ! curl -s -o /dev/null --max-time 5 "$BASE/v1/models"; then
  echo "CPA 不可达: $BASE" >&2
  exit 2
fi

attempts=$([ "$STRICT" = "1" ] && echo 1 || echo 3)
ok=0
total=0
failed=""

for m in ${MODELS//,/ }; do
  total=$((total + 1))
  best=""
  body=""
  tries=$([ "$STABILITY" -gt 0 ] && echo "$STABILITY" || echo "$attempts")
  hit=0
  for _ in $(seq 1 "$tries"); do
    code=$(curl -s -o /tmp/e2e-cpa-free.txt --max-time 90 \
      -X POST "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer $CPAK" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"say OK\"}],\"max_tokens\":8}" \
      -w "%{http_code}")
    best=$code
    body=$(head -c 120 /tmp/e2e-cpa-free.txt)
    if [ "$code" = "200" ]; then
      hit=$((hit + 1))
      [ "$STABILITY" -gt 0 ] || break
    fi
  done

  if [ "$STABILITY" -gt 0 ]; then
    if [ "$hit" = "$STABILITY" ]; then
      printf '🟢 %-38s 200=%d/%d\n' "$m" "$hit" "$STABILITY"
      ok=$((ok + 1))
    else
      printf '🔴 %-38s 200=%d/%d  最后: %s %s\n' "$m" "$hit" "$STABILITY" "$best" "$body"
      failed="$failed $m"
    fi
  elif [ "$best" = "200" ]; then
    printf '🟢 %-38s 200\n' "$m"
    ok=$((ok + 1))
  else
    printf '🔴 %-38s %s  %s\n' "$m" "$best" "$body"
    failed="$failed $m"
  fi
done

echo
if [ "$STABILITY" -gt 0 ]; then
  echo "稳定性: $ok/$total 模型达成 200=$STABILITY/$STABILITY"
else
  echo "通过 $ok/$total"
fi

if [ -n "$failed" ]; then
  echo "未通过:$failed"
  echo
  echo "排查顺序（详见 docs/CPA.md）："
  echo "  1) 400 unknown provider …        -> CPA 的 alias/name 暴露问题"
  echo "  2) 503 auth_unavailable …        -> 冷却；比对错误里 request id 的时间戳与 date -u"
  echo "  3) 503 No available channel …    -> new-api 的 channels.models / abilities"
  echo "  4) 403 FreeTierError …           -> 出口 IP 未放行，重试即可，不是模型坏"
  exit 1
fi
exit 0
