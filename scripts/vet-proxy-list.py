#!/usr/bin/env python3
"""静态代理批量验活：HTTPS CONNECT 真实探测 + 出口 IP 归属地补全。

阶段一：对每个唯一代理发一次真实 HTTPS 请求（CONNECT 隧道）——这正是 zenproxy
        访问 https://opencode.ai 需要走的路径，能通过才说明可用。
阶段二：把存活出口 IP 批量送 ip-api.com 取国家码（100 个/请求，4.2s 间隔避开限速）。

用法：
    python3 vet-proxy-list.py 导出.txt 结果.json [--workers 120] [--timeout 8]

输出 JSON 里带着带凭据的代理 URL，属敏感物，落盘即 600；终端只打印聚合统计。
导出行格式：http://user:pass@ip:port（每行一条，允许同 IP 重复）。
"""
from __future__ import annotations

import argparse
import base64
import collections
import concurrent.futures as cf
import json
import os
import time
import urllib.error
import urllib.request

PROBE_URL = "https://api.ipify.org?format=json"
GEO_BATCH = "http://ip-api.com/batch?fields=status,countryCode,query"


def load_candidates(path: str):
    items, seen, dupes, skipped = [], set(), 0, 0
    for raw in open(path, encoding="utf-8", errors="replace"):
        raw = raw.strip().strip('"')
        if not raw.startswith("http://") or "@" not in raw:
            skipped += 1
            continue
        at = raw.index("@")
        userpass = raw[len("http://"):at]
        ipport = raw[at + 1:]
        if ":" not in ipport:
            skipped += 1
            continue
        ip = ipport.rsplit(":", 1)[0]
        if ip in seen:
            dupes += 1
            continue
        seen.add(ip)
        items.append((raw, userpass, ipport))
    print("input uniq-by-ip: %d (dupes skipped: %d, malformed: %d)"
          % (len(items), dupes, skipped))
    return items


def classify(ex: Exception) -> str:
    if isinstance(ex, urllib.error.HTTPError):
        return "http%d" % ex.code
    low = str(ex).lower()
    for needle, label in (("timed out", "timeout"), ("timeout", "timeout"),
                          ("refused", "refused"), ("reset", "reset"),
                          ("tunnel", "no_connect"), ("connect", "no_connect")):
        if needle in low:
            return label
    return type(ex).__name__


def probe(item, timeout: int):
    raw, userpass, ipport = item
    t0 = time.time()
    try:
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": "http://" + ipport,
                                         "https": "http://" + ipport}))
        req = urllib.request.Request(PROBE_URL)
        # 凭据走 Proxy-Authorization：走代理认证，不进 URL，免得落进日志
        req.add_header("Proxy-Authorization",
                       "Basic " + base64.b64encode(userpass.encode()).decode())
        with opener.open(req, timeout=timeout) as resp:
            exit_ip = json.loads(resp.read(200).decode("utf-8", "replace")).get("ip")
        return (raw, True, exit_ip, int((time.time() - t0) * 1000), "")
    except Exception as ex:
        return (raw, False, None, 0, classify(ex))


def geo_batch(exit_ips):
    geo = {}
    for i in range(0, len(exit_ips), 100):
        chunk = exit_ips[i:i + 100]
        try:
            body = json.dumps([{"query": ip} for ip in chunk]).encode()
            req = urllib.request.Request(GEO_BATCH, data=body,
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=20) as resp:
                for row in json.loads(resp.read().decode()):
                    if row.get("status") == "success":
                        geo[row["query"]] = row.get("countryCode") or "??"
        except Exception as ex:
            print("geo batch %d failed: %s" % (i // 100, ex))
        if i + 100 < len(exit_ips):
            time.sleep(4.2)   # ip-api 免费档 45 次/分钟
    return geo


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("out")
    ap.add_argument("--workers", type=int, default=120)
    ap.add_argument("--timeout", type=int, default=8)
    ap.add_argument("--no-geo", action="store_true", help="跳过归属地补全（省时间）")
    args = ap.parse_args()

    items = load_candidates(args.src)
    if not items:
        return 1

    t0 = time.time()
    alive, dead = [], []
    with cf.ThreadPoolExecutor(args.workers) as pool:
        for res in pool.map(lambda it: probe(it, args.timeout), items):
            (alive if res[1] else dead).append(res)
    dur = time.time() - t0

    lat = sorted(r[3] for r in alive)
    reasons = collections.Counter(r[4] for r in dead)
    print("alive=%d dead=%d dur=%.0fs" % (len(alive), len(dead), dur))
    if lat:
        print("latency p50=%dms p90=%dms p99=%dms"
              % (lat[len(lat) // 2], lat[int(len(lat) * 0.9)], lat[int(len(lat) * 0.99)]))
    print("DEAD REASONS:", reasons.most_common(10))

    exit_dupes = collections.Counter(r[2] for r in alive)
    print("dup exit ips:", sum(v - 1 for v in exit_dupes.values() if v > 1),
          "max dup:", max(exit_dupes.values(), default=0))

    geo = {}
    if not args.no_geo:
        exit_ips = sorted({r[2] for r in alive if r[2]})
        geo = geo_batch(exit_ips)
        cc = collections.Counter(geo.get(r[2], "??") for r in alive)
        print("\nCOUNTRY DIST (alive):")
        for code, n in cc.most_common(25):
            print("  %-4s %4d" % (code, n))

    payload = {
        "generated_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source": args.src,
        "alive": [{"url": r[0], "exit": r[2], "cc": geo.get(r[2], "??"), "ms": r[3]}
                  for r in alive],
        "stats": {"input_uniq": len(items), "alive": len(alive), "dead": len(dead),
                  "dur_s": round(dur, 1),
                  "reasons": dict(reasons.most_common(10)),
                  "country": dict(collections.Counter(
                      geo.get(r[2], "??") for r in alive).most_common(40))},
    }
    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(payload, fh)
    os.chmod(args.out, 0o600)
    print("\nwrote %s (alive=%d, 600)" % (args.out, len(alive)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
