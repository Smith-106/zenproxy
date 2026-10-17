#!/usr/bin/env python3
"""调 CPA 的重试/冷却，避免一次上游抖动被放大成十分钟的模型级硬锁。

## 现象

CPA 对一个模型持续返回（上游早已恢复也一样）：

    503 {"error":{"message":"auth_unavailable: no auth available
        (providers=openai-compatible-new-api, model=glm-5.3-flash;
         last upstream error: model_not_found: No available channel for model
         glm-5.3-flash ... (request id: 202610171130307451868248268d9d6SnDfeohr))",
        "type":"server_error","code":"internal_server_error"}}

注意里面那个 **request id 的时间戳**：它是 11:30 那次失败的，而当前已经是 11:40。
也就是说 CPA 在复用 10 分钟前的缓存错误，压根没再打上游。判据就靠这个时间戳：
**请求失败后，把错误里的 request id 时间和 `date -u` 对一下** —— 不相等即冷却。

## 为什么

    routing:
      cooldown:
        save-cooldown-status: true            # 冷却状态落盘 -> docker restart 也不清
        transient-error-cooldown-seconds: 15
      retry:
        request-retry: 0                      # 一次失败就结束，不重试
        max-retry-credentials: 0

免费面单发成功率本来就只有 ~50%（上游按出口 IP 信誉放行，`403 FreeTierError`）。
`request-retry: 0` 意味着**一次掷骰定生死**，失败即进入更长冷却，于是偶发抖动
被放大成「这个模型十分钟不可用」。

## 改法（本脚本）

* `request-retry: 2` + `max-retry-credentials: 1` —— 每次请求多两次独立掷骰。
* 对指定 provider 加 `disable-cooling: true` —— 只对它关冷却。这里的上游是 new-api，
  它自己会跨 zen-01/02/03/05 四条渠道重试；在 CPA 这层再冷却纯属把它的重试成果作废。

**不要**全局关冷却：对真正不可用的上游（欠费、鉴权错）冷却是有价值的限流。

## 用法

    sudo scripts/cpa-routing-tuning.py --check
    sudo scripts/cpa-routing-tuning.py --provider new-api --apply
    sudo docker restart cli-proxy-api        # 冷却状态是落盘的，改完重启才开始用新设置

改完的验收方式不是「看配置」，是**连打 10 次**：`200=10/10` 才算过。实测改前同一模型
3 次全 503，改后 10/10 全 200。
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import sys
import time

import yaml

DEFAULT_CFG = "/opt/cli-proxy-api/config.yaml"


def show(doc: dict) -> None:
    r = doc.get("routing") or {}
    print("routing.strategy            :", r.get("strategy"))
    print("routing.cooldown            :", r.get("cooldown"))
    print("routing.retry               :", r.get("retry"))
    print()
    print("%-22s %-14s %s" % ("provider", "disabled", "disable-cooling"))
    for p in (doc.get("api-keys") or {}).get("openai-compatibility") or []:
        print("%-22s %-14s %s" % (p.get("name"), p.get("disabled"),
                                  p.get("disable-cooling")))


def want_retry(doc: dict) -> list[tuple[str, int, int]]:
    """(label, current, wanted) rows that need changing."""
    r = doc.get("routing") or {}
    retry = r.get("retry") or {}
    rows = []
    for key, target in (("request-retry", 2), ("max-retry-credentials", 1)):
        cur = retry.get(key)
        if cur != target:
            rows.append((key, -1 if cur is None else int(cur), target))
    return rows


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default=DEFAULT_CFG)
    ap.add_argument("--provider", default="new-api",
                    help="要关冷却的 openai-compatibility 条目名")
    ap.add_argument("--apply", action="store_true")
    args = ap.parse_args()

    text = open(args.config, encoding="utf-8").read()
    doc = yaml.safe_load(text)
    show(doc)

    rows = want_retry(doc)
    prov = next((p for p in (doc.get("api-keys") or {}).get("openai-compatibility") or []
                 if p.get("name") == args.provider), None)
    need_provider = prov is not None and prov.get("disable-cooling") is not True

    print()
    if not rows and not need_provider:
        print("🟢 已是目标状态（retry=2/1，%s 已关冷却）" % args.provider)
        return 0
    for key, cur, target in rows:
        print("🔧 routing.retry.%s: %s -> %s" % (key, cur, target))
    if need_provider:
        print('🔧 provider %r: disable-cooling -> true' % args.provider)

    if not args.apply:
        print("\n（干跑）加 --apply 写入，然后 sudo docker restart cli-proxy-api")
        return 0

    edited = text
    for key, _cur, target in rows:
        pat = re.compile(r"^(\s*%s:\s*)\d+\s*$" % re.escape(key), re.M)
        hits = pat.findall(edited)
        if len(hits) != 1:
            raise SystemExit("%s 命中 %d 次，拒绝改写" % (key, len(hits)))
        edited = pat.sub(lambda m: "%s%d" % (m.group(1), target), edited)

    if need_provider:
        base = prov.get("base-url") or ""
        old = '"base-url": "%s", ' % base
        if edited.count(old) != 1:
            raise SystemExit("provider base-url 子串命中 %d 次，拒绝改写"
                             % edited.count(old))
        edited = edited.replace(old, old + '"disable-cooling": true, ')

    check = yaml.safe_load(edited)
    r = check["routing"]["retry"]
    assert r["request-retry"] == 2 and r["max-retry-credentials"] == 1
    p2 = next(p for p in check["api-keys"]["openai-compatibility"]
              if p.get("name") == args.provider)
    assert p2.get("disable-cooling") is True
    assert set(check.keys()) == set(doc.keys()), "顶层键被改动了"
    assert len(check["api-keys"]["openai-compatibility"]) == \
        len(doc["api-keys"]["openai-compatibility"]), "provider 数量变了"

    ts = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    shutil.copy2(args.config, args.config + ".bak-retry-" + ts)
    tmp = args.config + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(edited)
    st = os.stat(args.config)
    os.chmod(tmp, st.st_mode & 0o777)
    os.chown(tmp, st.st_uid, st.st_gid)
    os.rename(tmp, args.config)

    print("\n已写入。备份: %s" % (args.config + ".bak-retry-" + ts))
    print("生效: sudo docker restart cli-proxy-api")
    print("验收: 同一模型连打 10 次，要求 200=10/10（不是看配置）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
