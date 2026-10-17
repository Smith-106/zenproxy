#!/usr/bin/env python3
"""把 vet 出来的存活池裁到注入器能接受的规模，并挑出每个国家里最快的那批。

为什么必须裁：zenproxy 的健康探针按 5 分钟一轮打满全池，出口数直接换算成带宽税。
1100 个出口 ≈ 0.6GB/天探活流量；不裁的话 4500 个出口 ≈ 2.6GB/天，配额扛不住。

选择策略：
  * 国家配额按存活量等比分配（最大余数法，小国不至于被抹掉）；
  * 同一国家内**按延迟升序取**，所以留下的是「最好的 1100 个」而不是随机 1100 个；
  * 输出直接是注入器要的格式：{代理URL: 国家码}。

用法：
    python3 trim-proxy-pool.py 存活.json 池.json [--target 1100]
"""
from __future__ import annotations

import argparse
import collections
import json
import random


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("src", help="vet-proxy-list.py 的输出 JSON")
    ap.add_argument("out", help="注入器读的 {url: cc} JSON")
    ap.add_argument("--target", type=int, default=1100)
    ap.add_argument("--floor", type=int, default=900)
    ap.add_argument("--ceiling", type=int, default=1200,
                    help="注入器的硬上限，超了它会 abort")
    ap.add_argument("--seed", type=int, default=20261017, help="只用于同延迟的稳定排序")
    args = ap.parse_args()

    random.seed(args.seed)
    payload = json.load(open(args.src, encoding="utf-8"))
    alive = payload["alive"]
    total = len(alive)
    print("source alive: %d (generated %s)" % (total, payload.get("generated_utc")))
    if total <= args.target:
        print("池子本来就小于目标值 %d，直接全量落地" % args.target)
        json.dump({r["url"]: r.get("cc") or "??" for r in alive},
                  open(args.out, "w", encoding="utf-8"))
        return 0

    by_cc = collections.defaultdict(list)
    for row in alive:
        by_cc[row.get("cc") or "??"].append(row)
    for cc in by_cc:
        by_cc[cc].sort(key=lambda r: (r.get("ms", 10 ** 6), r["url"]))

    # 最大余数法分配配额
    alloc = {cc: len(rows) * args.target // total for cc, rows in by_cc.items()}
    rest = args.target - sum(alloc.values())
    order = sorted(by_cc, key=lambda cc: -(len(by_cc[cc]) * args.target % total))
    for cc in order[:rest]:
        alloc[cc] += 1

    keep, lat_kept = {}, []
    for cc, rows in by_cc.items():
        for row in rows[:min(alloc[cc], len(rows))]:
            keep[row["url"]] = cc
            lat_kept.append(row.get("ms", 0))

    final = len(keep)
    if not args.floor < final < args.ceiling:
        print("裁剪结果 %d 落在注入器窗口 (%d, %d) 之外" % (final, args.floor, args.ceiling))
        return 2

    lat_kept.sort()
    src_lat = sorted(r.get("ms", 0) for r in alive)
    print("kept %d alive" % final)
    print("country mix:", dict(sorted(collections.Counter(keep.values()).items(),
                                      key=lambda x: -x[1])))
    print("kept latency p50=%dms p90=%dms (source p50=%dms)"
          % (lat_kept[len(lat_kept) // 2], lat_kept[int(len(lat_kept) * 0.9)],
             src_lat[len(src_lat) // 2]))

    json.dump(keep, open(args.out, "w", encoding="utf-8"))
    print("wrote", args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
