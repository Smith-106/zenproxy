#!/usr/bin/env bash
# 逐个测试上游当前公布的免费模型，经 zenproxy 走一次真实调用。
# 判断标准：HTTP 200 = 可用；403 FreeTierError = 套壳没生效；
#           400 Model is unavailable = 模型名已过期（上游没有这个模型了）。
set -u

BASE=${BASE:-http://127.0.0.1:9000}
ACCT=${ACCT:-zen-01}
MT=${MT:-64}
MODELS=${*:-"big-pickle ling-3.1-flash-free mimo-v2.6-flash-free muse-spark-1.3-contributor-free nemotron-3.5-lightning-free jev-1.13-free step-5-preview-free longcat-2.5-preview-free space-bunny-free"}

cat > /tmp/judge.py <<'PY'
import json, sys
code = sys.argv[1]
raw = sys.stdin.read().strip()
try:
    d = json.loads(raw)
except Exception:
    print("HTTP=%s 非JSON: %s" % (code, raw[:90])); sys.exit()
if "error" in d:
    e = d["error"]
    print("HTTP=%s %s: %s" % (code, e.get("type"), str(e.get("message"))[:88])); sys.exit()
ch = (d.get("choices") or [{}])[0]
msg = ch.get("message") or {}
txt = (msg.get("content") or "").strip().replace("\n", " ")
print("HTTP=%s content=%r finish=%s" % (code, txt[:44], ch.get("finish_reason")))
PY

for m in $MODELS; do
    out=$(curl -sS -m 240 -w '|%{http_code}' -X POST "$BASE/$ACCT/v1/chat/completions" \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}],\"max_tokens\":$MT}" 2>&1)
    code=${out##*|}
    body=${out%|*}
    printf '  %-34s ' "$m"
    printf '%s' "$body" | python3 /tmp/judge.py "$code"
done
