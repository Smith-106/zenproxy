#!/usr/bin/env bash
# Add the two /responses-only free models to free_tier.models and restart.
#
# They are in free_tier.learned already — the proxy learned them the hard way,
# by being refused — so the envelope would now apply to them anyway. Listing
# them explicitly is what stops every cold start from paying one wasted 403 per
# model before that happens.
#
# Usage: sudo ./add-responses-only-models.sh
set -euo pipefail

CFG=/opt/zenproxy/config.yaml
BACKUP="/opt/zenproxy/config.yaml.bak.$(date +%Y%m%d-%H%M%S)"

ANCHOR='    - nemotron-3.5-lightning-free'
ADD='    - muse-spark-1.3-contributor-free  # 只在 /responses 上接单，chat 会回 ModelProtocolUnsupported
    - muse-spark-1.2-contributor-free  # 同上'

echo "=== 1. 备份 ==="
cp -p "$CFG" "$BACKUP"
echo "  $BACKUP"

echo
echo "=== 2. 检查锚点唯一性 ==="
n=$(grep -cF "$ANCHOR" "$CFG" || true)
if [ "$n" -ne 1 ]; then
  echo "锚点出现 $n 次（应为 1），中止以免改错地方" >&2
  exit 1
fi
echo "  锚点唯一"

if grep -qF 'muse-spark-1.3-contributor-free' "$CFG"; then
  echo "  已经存在，跳过插入"
else
  echo
  echo "=== 3. 插入两行 ==="
  python3 - "$CFG" "$ANCHOR" "$ADD" <<'PY'
import sys
cfg, anchor, add = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(cfg, encoding="utf-8").read().split("\n")
i = lines.index(anchor)
lines[i+1:i+1] = add.split("\n")
mode = None
import os, stat
st = os.stat(cfg)
open(cfg, "w", encoding="utf-8").write("\n".join(lines))
os.chmod(cfg, stat.S_IMODE(st.st_mode))
os.chown(cfg, st.st_uid, st.st_gid)
print(f"  在行 {i+1} 之后插入 2 行")
PY
fi

echo
echo "=== 4. free_tier 段 ==="
sed -n '/^free_tier:/,/^[a-z_]*:/p' "$CFG" | head -14

echo
echo "=== 5. 重启 ==="
cd /opt/zenproxy
docker compose up -d 2>&1 | tail -5
sleep 6
docker inspect zenproxy --format '  status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}n/a{{end}} restarts={{.RestartCount}}'
printf '  healthz: '; curl -s -m 5 http://127.0.0.1:9000/healthz; echo
