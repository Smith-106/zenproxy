#!/usr/bin/env python3
"""把裁剪好的静态池写进 /opt/zenproxy/config.yaml 的 static: 段。

★ 顺序是硬约束，写错了会静静污染整个池子：

    1. sudo docker stop zenproxy-zenproxy-a-1 zenproxy-zenproxy-b-1   （先停干净）
    2. python3 inject-static-pool.py 池.json                          （本脚本）
    3. sudo docker start zenproxy-zenproxy-a-1 && sleep 4 \\
       && sudo docker start zenproxy-zenproxy-b-1                    （一次起一个）

原因：出口 ID 是**位置式**的（static-0、static-1…）。运行中的进程会把旧池子的健康
状态按老编号写回 exits-state.json；池子换了而状态没换，新代理就顶着别人的
quarantine / free-tier 封锁标记上场 —— 表现是"注入完还是一堆红的"。
所以本脚本会先确认两个副本都停了，并顺手把 exits-state.json 备份后删掉。

用法：
    sudo python3 inject-static-pool.py 池.json
    sudo python3 inject-static-pool.py 池.json --config /opt/zenproxy/config.yaml --force
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time

CONTAINERS = ("zenproxy-zenproxy-a-1", "zenproxy-zenproxy-b-1")


def running_containers() -> list[str]:
    try:
        out = subprocess.run(["docker", "ps", "--format", "{{.Names}}"],
                             capture_output=True, text=True, timeout=20).stdout
    except Exception:
        return []      # 没装 docker 命令就别拦人
    names = set(out.split())
    return [c for c in CONTAINERS if c in names]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("pool", help="trim-proxy-pool.py 的输出 {url: cc}")
    ap.add_argument("--config", default="/opt/zenproxy/config.yaml")
    ap.add_argument("--state", default="/opt/zenproxy/data/exits-state.json")
    ap.add_argument("--floor", type=int, default=900)
    ap.add_argument("--ceiling", type=int, default=1200)
    ap.add_argument("--force", action="store_true",
                    help="允许在副本仍在跑时注入（不推荐，只为救急备着）")
    args = ap.parse_args()

    up = running_containers()
    if up and not args.force:
        print("★ 副本仍在运行：%s" % ", ".join(up))
        print("  先执行： sudo docker stop " + " ".join(CONTAINERS))
        print("  注入后： sudo docker start %s && sleep 4 && sudo docker start %s"
              % CONTAINERS)
        return 2

    pool = json.load(open(args.pool, encoding="utf-8"))
    alive = [u for u, cc in pool.items()
             if not str(cc).startswith(("http", "timeout", "refused", "err:"))]
    print("alive entries:", len(alive))
    if not args.floor < len(alive) < args.ceiling:
        print("出口数 %d 落在 (%d, %d) 之外，拒绝注入" % (len(alive), args.floor, args.ceiling))
        return 3

    ts = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    shutil.copy2(args.config, args.config + ".bak-" + ts)
    state_dir = os.path.dirname(args.state) or "."
    cleared = 0
    for name in sorted(os.listdir(state_dir)):
        if not (name == "exits-state.json" or (name.startswith("exits-state-") and name.endswith(".json"))):
            continue
        src = os.path.join(state_dir, name)
        if not os.path.isfile(src):
            continue
        shutil.copy2(src, src + ".bak-" + ts)
        os.remove(src)          # 从干净状态开跑：ID 是位置式的，旧标记不能留
        cleared += 1
    if cleared:
        print("cleared %d exit-state file(s) under %s" % (cleared, state_dir))

    lines = open(args.config, encoding="utf-8").read().split("\n")
    i0 = next(i for i, l in enumerate(lines) if l.rstrip() == "  static:")
    # 段尾 = 下一条非 "- " 的顶层键
    i1 = next(i for i in range(i0 + 1, len(lines))
              if not lines[i].startswith("  - ") and lines[i][:1].isalpha())
    block = ["  static:"] + ['  - "%s"' % u for u in alive]
    print("replacing lines %d..%d (old=%d) with %d entries"
          % (i0, i1 - 1, i1 - i0 - 1, len(alive)))

    tmp = args.config + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines[:i0] + block + lines[i1:]))
    st = os.stat(args.config)
    os.chmod(tmp, st.st_mode & 0o777)
    os.chown(tmp, st.st_uid, st.st_gid)   # 容器按属主读配置，别改成 root-only
    os.rename(tmp, args.config)           # 原子替换，容器不会读到半个文件

    print("config updated, backup: %s.bak-%s" % (args.config, ts))
    print("接下来： sudo docker start %s && sleep 4 && sudo docker start %s"
          % CONTAINERS)
    return 0


if __name__ == "__main__":
    sys.exit(main())
