#!/usr/bin/env python3
"""免费模型实测：两个协议面都打一遍，只认 200。

为什么不能只看一面：上游的「模型在不在」和「这个端点收不收」是两个独立维度。
典型误判：
  * muse-spark-*-contributor-free 走 /chat 回 400 ModelProtocolUnsupported，
    只看 chat 就会当成废模型删掉 —— 它在 /responses 上是好的；
  * 反向地，多数 *-free 走 /chat 正常，/responses 不受理。

候选集默认从上游目录自动取（含 -free / -contributor / big-pickle / nano / preview 字样），
再并上显式指定的模型名，所以「上游是不是上了新免费模型」这一问也顺手回答了。

用法：
    python3 probe-free-models-both-faces.py                       # 自动发现 + 两个面
    python3 probe-free-models-both-faces.py --models a b c
    python3 probe-free-models-both-faces.py --base http://127.0.0.1:9000 --account zen-01

判定口径：
    200                        = 该面可用
    400 ModelProtocolUnsupported = 这个面本来就不收它（不是废模型）
    400 Model is unavailable / 410 ModelDeprecated = 上游已下线，应从名单剔除
    403 FreeTierError / RegionError = 套壳或出口问题，换出口重试
"""
from __future__ import annotations

import argparse
import concurrent.futures as cf
import json
import os
import urllib.error
import urllib.request

UA = "opencode/1.19.0 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
CATALOGS = ("https://opencode.ai/inference/v1/models",
            "https://opencode.ai/zen/go/v1/models")
HINTS = ("free", "contributor", "pickle", "nano", "preview", "test")

VERDICT = {
    400: "协议面不收",
    403: "门禁/出口",
    402: "付费无额度",
    410: "上游已下线",
    429: "额度耗尽",
}


def upstream_catalog() -> list[str]:
    ids = set()
    for url in CATALOGS:
        req = urllib.request.Request(url)
        req.add_header("Authorization", "Bearer public")   # 免费面凭据就是字面量 public
        req.add_header("User-Agent", UA)
        req.add_header("x-opencode-client", "cli")
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                ids |= {m["id"] for m in json.loads(resp.read().decode()).get("data", [])}
        except Exception as ex:
            print("catalog %s failed: %s" % (url, ex))
    return sorted(ids)


def probe(base, account, model, face, token, timeout):
    if face == "chat":
        path, body = "/v1/chat/completions", {
            "model": model, "messages": [{"role": "user", "content": "say OK"}],
            "max_tokens": 8}
    else:
        path, body = "/v1/responses", {
            "model": model, "max_output_tokens": 16,
            "input": [{"role": "user", "content": [{"type": "input_text", "text": "say OK"}]}]}
    req = urllib.request.Request("%s/%s%s" % (base.rstrip("/"), account, path),
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read(300).decode("utf-8", "replace")
            msg = "ok"
            if '"error"' in raw:
                try:
                    msg = (json.loads(raw).get("error") or {}).get("message", "err")
                except Exception:
                    msg = "err"
            return resp.status, msg
    except urllib.error.HTTPError as ex:
        raw = ex.read(300).decode("utf-8", "replace")
        try:
            msg = (json.loads(raw).get("error") or {}).get("message", raw)
        except Exception:
            msg = raw
        return ex.code, msg
    except Exception as ex:
        return None, type(ex).__name__


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=os.environ.get("ZEN_BASE", "http://127.0.0.1:9000"))
    ap.add_argument("--account", default=os.environ.get("ZEN_ACCOUNT", "zen-01"))
    ap.add_argument("--models", nargs="*", default=None, help="显式模型名（跳过自动发现）")
    ap.add_argument("--token", default=os.environ.get("ZEN_CLIENT_TOKEN", ""),
                    help="zenproxy 需鉴权时传；本地回环一般不必")
    ap.add_argument("--timeout", type=int, default=45)
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--out", default="/tmp/model-probe.json")
    args = ap.parse_args()

    if args.models:
        candidates = sorted(set(args.models))
        print("候选（显式）: %d" % len(candidates))
    else:
        catalog = upstream_catalog()
        # 既有的 *-free 家族 + 上游新冒头的同类命名，一起打
        candidates = sorted({m for m in catalog if any(h in m.lower() for h in HINTS)})
        print("上游目录 %d 个 ID，免费候选 %d 个" % (len(catalog), len(candidates)))
    if not candidates:
        print("没有候选模型")
        return 1

    jobs = [(m, f) for m in candidates for f in ("chat", "responses")]
    res: dict[str, dict] = {}
    with cf.ThreadPoolExecutor(args.workers) as pool:
        for (model, face), (code, msg) in zip(
                jobs, pool.map(lambda j: probe(args.base, args.account, j[0], j[1],
                                               args.token, args.timeout), jobs)):
            res.setdefault(model, {})[face] = {"http": code, "msg": str(msg)[:60]}

    print("\n%-44s %-24s %-24s" % ("model", "chat", "responses"))
    live, dead = [], []
    for m in candidates:
        row = res[m]
        cells = []
        for face in ("chat", "responses"):
            http, msg = row[face]["http"], row[face]["msg"]
            cells.append("%-4s %s" % (http if http else "---", msg[:16]))
        print("%-44s %-24s %-24s" % (m, cells[0], cells[1]))
        ok_faces = [f for f in ("chat", "responses") if row[f]["http"] == 200]
        deadish = all(row[f]["http"] in (400, 410, 402)
                      and any(k in row[f]["msg"] for k in
                              ("unavailable", "deprecated", "does not support"))
                      for f in ("chat", "responses"))
        if ok_faces:
            live.append({"model": m, "faces": ok_faces})
        elif deadish:
            dead.append(m)

    print("\n可用 %d: %s" % (len(live), [x["model"] for x in live]))
    for x in live:
        print("   %-44s faces=%s" % (x["model"], ",".join(x["faces"])))
    print("应剔除 %d: %s" % (len(dead), dead))

    json.dump({"base": args.base, "account": args.account,
               "live": live, "dead": dead, "detail": res},
              open(args.out, "w", encoding="utf-8"), indent=1)
    print("\nwrote", args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
