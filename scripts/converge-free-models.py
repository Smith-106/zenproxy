#!/usr/bin/env python3
"""把「实测可用」的免费模型清单同时收敛到 zenproxy 与 new-api。

只改一处不够 —— 两边各有自己的名单，缺一个就是「目录里看得见、请求打不通」：
  * zenproxy `free_tier.models`：不在名单里的模型第一次被上游拒后才现学，
    名单外硬打会白烧一次 403；
  * new-api `channels.models` + `abilities`：少了 abilities 行，`/v1/models`
    里根本不会出现这个模型（channels.models 只是字符串，不承担路由）。
  * Redis 里还有一份 abilities 缓存，改完得 FLUSHALL + 重启 new-api 才生效。

zenproxy 侧的改写直接复用同目录的 refresh-free-models.py（不重复造 YAML 扫描）。
别名（ALIASES）只进 new-api：老客户端还在按旧模型名打，靠 channels.model_mapping
把它改写到活着的名字上，所以别名不参与 zenproxy 名单。

用法：
    sudo python3 converge-free-models.py --models-file /tmp/model-probe.json
    sudo python3 converge-free-models.py --models a b c --alias old-name
    sudo python3 converge-free-models.py --models a b c --skip-zenproxy --dry-run
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PSQL = ["sudo", "docker", "exec", "postgres", "psql", "-U", "root", "-d", "new-api", "-c"]


def live_from_probe(path: str) -> list[str]:
    data = json.load(open(path, encoding="utf-8"))
    models = []
    for row in data.get("live") or []:
        if isinstance(row, dict) and row.get("model"):
            models.append(row["model"])
        elif isinstance(row, str):
            models.append(row)
    return sorted(set(models))


def apply_zenproxy(models: list[str], dry: bool) -> None:
    script = os.path.join(HERE, "refresh-free-models.py")
    if not os.path.exists(script):
        print("跳过 zenproxy：找不到", script)
        return
    cmd = ["sudo", "python3", script] + models
    print("$", " ".join(cmd[:4]), "... (%d 个模型)" % len(models))
    if dry:
        return
    res = subprocess.run(cmd, capture_output=True, text=True)
    print(res.stdout.strip()[:800])
    if res.returncode != 0:
        print(res.stderr.strip()[:400])


def apply_newapi(models: list[str], aliases: list[str], channels: list[int], dry: bool) -> None:
    all_models = models + aliases
    quoted = ",".join("'%s'" % m for m in all_models)
    chan_ids = ",".join(str(c) for c in channels)
    values = ",".join("('default','%s',%d,true,0,1)" % (m, ch)
                      for ch in channels for m in all_models)
    sql = f"""BEGIN;
UPDATE channels SET models = '{",".join(all_models)}' WHERE id IN ({chan_ids});
DELETE FROM abilities WHERE channel_id IN ({chan_ids}) AND model NOT IN ({quoted});
INSERT INTO abilities ("group", model, channel_id, enabled, priority, weight)
VALUES {values}
ON CONFLICT ("group", model, channel_id) DO UPDATE SET enabled = true, weight = 1;
COMMIT;"""
    print("new-api: channels %s 名单 -> %d 个（含别名 %d）" % (chan_ids, len(all_models), len(aliases)))
    if dry:
        print(sql[:400], "...")
        return
    res = subprocess.run(PSQL + [sql], capture_output=True, text=True)
    print(res.stdout.strip()[:600] or res.stderr.strip()[:400])
    subprocess.run(["sudo", "docker", "exec", "redis", "redis-cli", "FLUSHALL"],
                   capture_output=True, text=True)
    print("redis 已清；重启 new-api 后生效： sudo docker restart new-api")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", nargs="*", default=None)
    ap.add_argument("--models-file", default=None, help="probe-free-models-both-faces.py 的 --out")
    ap.add_argument("--alias", action="append", default=[],
                    help="只留在 new-api 的兼容别名，可重复")
    ap.add_argument("--channels", default="21,22,23,24", help="承接免费模型的 new-api 渠道 ID")
    ap.add_argument("--skip-zenproxy", action="store_true")
    ap.add_argument("--skip-newapi", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    models = list(args.models or [])
    if args.models_file:
        models += live_from_probe(args.models_file)
    models = sorted(set(models))
    if not models:
        print("没有模型可收敛：传 --models 或 --models-file")
        return 1
    channels = [int(x) for x in args.channels.split(",") if x.strip()]

    print("时间 %s" % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    print("目标清单 %d: %s" % (len(models), models))
    if args.alias:
        print("保留别名（仅 new-api）: %s" % args.alias)
    if not args.skip_zenproxy:
        apply_zenproxy(models, args.dry_run)
    if not args.skip_newapi:
        apply_newapi(models, args.alias, channels, args.dry_run)
    if not args.dry_run:
        print("\n收尾：两个副本都要重启（顺序见 inject-static-pool.py 的说明），"
              "然后跑 scripts/probe-free-models-both-faces.py 复测。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
