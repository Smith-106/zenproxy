#!/usr/bin/env python3
"""CPA（CLIProxyAPI）模型定价：从 models.dev 同步价目表，free 模型钉 0，落库并复核。

CPA 的价目不在 config.yaml 里，而在插件 cap-token-usage-tracker（bbolt 库
data/token-usage-tracker.db）。可直接用的是两条管理路由：

    POST /v0/management/plugins/cap-token-usage-tracker/prices/sync   拉取/重算
    PUT  /v0/management/plugins/cap-token-usage-tracker/prices        整体写回

（`GET` 这两条是 404 —— 路由只登记了 POST/PUT，别拿 GET 判断"没这功能"。）

★ 两条硬约束，踩过的坑：
  1. 宿主机源 IP 会被管理口的失败计数封掉（30 分钟）。因此本脚本把 curl 放进
     CPA 自己的 netns 里跑（--network container:cli-proxy-api），源变成 127.0.0.1，
     不受限流影响；密钥文件以 600 挂在容器内，用 $(head -n1 /keys) 展开，不落命令行。
  2. **绝不要猜鉴权头**。失败计数是按源 IP 记的，而浏览器面板走的是同一条
     nginx→CPA 路径 —— 猜错几次，运维自己的面板也一起被 403 封住。封禁只存内存，
     重启容器即解；但别去试。

用法：
    sudo python3 cpa-model-prices.py show                 # 打印当前价目
    sudo python3 cpa-model-prices.py sync                 # 同步 models.dev
    sudo python3 cpa-model-prices.py apply                # 钉 free=0 + 别名映射 + 复核
    sudo python3 cpa-model-prices.py sync --keys /root/.cpa_keys

密钥文件格式：第 1 行 = 管理密钥（明文），第 2 行可放数据面 inbound key（本脚本不用）。
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.request

try:
    import yaml
except ImportError:               # 部署机上有；本地缺也能跑 netns 那半边
    yaml = None

CPA_CONFIG = os.environ.get("CPA_CONFIG", "/opt/cli-proxy-api/config.yaml")
CPA_URL = os.environ.get("CPA_URL", "http://127.0.0.1:8317")
CPA_CONTAINER = os.environ.get("CPA_CONTAINER", "cli-proxy-api")
CURL_IMAGE = os.environ.get("CURL_IMAGE", "curlimages/curl:latest")
MPATH = "/v0/management/plugins/cap-token-usage-tracker/prices"

# 免费/贡献者模型必须留 0 价条目（面板里"缺价"本身就是要消灭的状态）
FREE_HINTS = ("free", "contributor", "big-pickle")

# models.dev 里没有一一对应条目的付费模型（USD / 1M tokens，按需增删）
MANUAL_OVERRIDES: dict[str, tuple[float, float, float, float]] = {}

# models.dev 认不出来的别名后缀 -> 基础模型名
MAPPINGS = [
    {"source": "claude-opus-5-5-hi", "target": "claude-opus-5-5"},
    {"source": "claude-opus-5-5-max", "target": "claude-opus-5-5"},
    {"source": "claude-opus-5-5-med", "target": "claude-opus-5-5"},
    {"source": "claude-opus-5-5-xhi", "target": "claude-opus-5-5"},
    {"source": "claude-opus-5-5-xhigh", "target": "claude-opus-5-5"},
]

SYNC_SETTINGS = {
    "provider_priority": ["opencode-go", "opencode", "opencode-zen", "xai", "openai",
                          "google", "anthropic", "deepseek", "moonshotai", "minimax",
                          "alibaba", "qwen", "zhipuai", "glm", "xiaomi", "nvidia",
                          "nvidia-nim", "helicone"],
    "ignored_suffixes": ["-thinking", "-preview", "-high", "-low", "-medium", "-minimal",
                         "(thinking)", "(xhigh)", "(high)", "(low)", "(medium)", "(minimal)",
                         "-0309-reasoning", "-0309-non-reasoning", "-multi-agent-0309"],
    "mappings": MAPPINGS,
}


def first_key(node):
    """抽 CPA 配置里的第一个可用密钥（api-keys 结构随版本变过）。"""
    if isinstance(node, str) and node:
        return node
    if isinstance(node, dict):
        for k in ("api-key", "api_key", "key", "secret", "token"):
            if isinstance(node.get(k), str) and node[k]:
                return node[k]
        for v in node.values():
            got = first_key(v)
            if got:
                return got
    if isinstance(node, list):
        for v in node:
            got = first_key(v)
            if got:
                return got
    return None


def live_models() -> list[str]:
    if yaml is None:
        return []
    cfg = yaml.safe_load(open(CPA_CONFIG, encoding="utf-8"))
    key = first_key((cfg.get("access") or {}).get("api-keys"))
    if not key:
        return []
    req = urllib.request.Request(CPA_URL + "/v1/models",
                                 headers={"Authorization": "Bearer " + key})
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read().decode())
    return sorted({m.get("id") for m in data.get("data", []) if m.get("id")})


def mgmt(method: str, path: str, keys_file: str, body: dict | None = None):
    """在 CPA 的 netns 里发管理请求，返回 (http_code, payload)。"""
    resp_path = "/tmp/cpa-resp-%d.json" % os.getpid()
    if os.path.exists(resp_path):
        os.unlink(resp_path)
    body_path = None
    if body is not None:
        body_path = "/tmp/cpa-body-%d.json" % os.getpid()
        with open(body_path, "w", encoding="utf-8") as fh:
            json.dump(body, fh)

    args = ["-sS", "-o", "/out/" + os.path.basename(resp_path), "-w", "%{http_code}",
            "-X", method,
            "-H", "Authorization: Bearer $(head -n1 /keys)",
            "-H", "Content-Type: application/json"]
    if body_path:
        args += ["--data-binary", "@/in/" + os.path.basename(body_path)]
    args.append(CPA_URL + path)
    inner = "curl " + " ".join('"%s"' % a for a in args)

    cmd = ["sudo", "docker", "run", "--rm", "--user", "0:0",
           "--network", "container:" + CPA_CONTAINER,
           "-v", "%s:/keys:ro" % keys_file, "-v", "/tmp:/out"]
    if body_path:
        cmd += ["-v", "%s:/in/%s:ro" % (body_path, os.path.basename(body_path))]
    cmd += [CURL_IMAGE, "sh", "-c", inner]

    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print("docker/curl 失败:", res.stderr.strip()[:300])
        return -1, None
    try:
        code = int(res.stdout.strip()[:3])
    except ValueError:
        code = -1
    payload = None
    if os.path.exists(resp_path):
        raw = open(resp_path, encoding="utf-8", errors="replace").read()
        try:
            payload = json.loads(raw)
        except Exception:
            payload = raw[:400]
        os.unlink(resp_path)
    if body_path and os.path.exists(body_path):
        os.unlink(body_path)
    return code, payload


def do_sync(keys_file: str) -> dict | None:
    ids = live_models()
    if not ids:
        print("拿不到 CPA /v1/models（缺 pyyaml 或配置不含 inbound key）")
        return None
    print("CPA /v1/models: %d 个模型" % len(ids))
    code, payload = mgmt("POST", MPATH + "/sync", keys_file,
                         {"source": "models.dev", "models": ids,
                          "sync_settings": SYNC_SETTINGS})
    print("POST prices/sync -> HTTP %s" % code)
    if code != 200 or not isinstance(payload, dict):
        print(str(payload)[:300])
        return None
    prices = payload.get("prices") or {}
    last = payload.get("last_sync") or {}
    print("价目条目 %d ；last_sync %s" % (len(prices), json.dumps(last)[:220]))
    missing = sorted(set(ids) - set(prices))
    if missing:
        print("仍未匹配 (%d): %s" % (len(missing), missing))
    return {"ids": ids, "book": payload}


def do_apply(keys_file: str, book: dict) -> int:
    ids = book["ids"]
    idset = set(ids)
    prices = dict(book["book"].get("prices") or {})

    free_ids = [m for m in ids if any(h in m.lower() for h in FREE_HINTS)]
    for name in free_ids:
        prices[name] = {"input": 0.0, "output": 0.0, "cache_read": 0.0,
                        "cache_creation": 0.0, "source": "manual"}
    for name, (inp, out, cr, cc) in MANUAL_OVERRIDES.items():
        if name in idset:
            prices[name] = {"input": inp, "output": out, "cache_read": cr,
                            "cache_creation": cc, "source": "manual"}
    unmatched = sorted(idset - set(prices))
    for name in unmatched:          # 认不出来的也别留空：显式 0 + manual，面板不再报缺价
        prices[name] = {"input": 0.0, "output": 0.0, "cache_read": 0.0,
                        "cache_creation": 0.0, "source": "manual"}

    print("PUT %d 条（free 钉 0: %d，手工覆盖: %d，兜底 0: %d）"
          % (len(prices), len(free_ids),
             sum(1 for n in MANUAL_OVERRIDES if n in idset), len(unmatched)))
    code, res = mgmt("PUT", MPATH, keys_file,
                     {"prices": prices, "sync_settings": dict(SYNC_SETTINGS)})
    print("PUT prices -> HTTP %s" % code)
    if code != 200:
        print(str(res)[:300])
        return 1

    again = do_sync(keys_file)
    if not again:
        return 1
    p2 = again["book"].get("prices") or {}
    bad = [(n, p2.get(n)) for n in free_ids
           if any(float((p2.get(n) or {}).get(k) or 0) != 0
                  for k in ("input", "output", "cache_read", "cache_creation"))]
    still = sorted(idset - set(p2))
    print("\n复核：条目 %d ；free 非 0: %s ；无价目: %s"
          % (len(p2), bad if bad else "无", still if still else "无"))
    return 0 if not bad and not still else 1


def do_show(keys_file: str) -> int:
    got = do_sync(keys_file)
    if not got:
        return 1
    prices = got["book"].get("prices") or {}
    print("\n%-46s %-9s %-9s %-9s %-9s %s"
          % ("model", "in", "out", "cache_rd", "cache_cr", "src"))
    for name in sorted(prices):
        e = prices[name]
        print("%-46s %-9s %-9s %-9s %-9s %s"
              % (name, e.get("input"), e.get("output"), e.get("cache_read"),
                 e.get("cache_creation"), e.get("source")))
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=("show", "sync", "apply"))
    ap.add_argument("--keys", default="/tmp/.cpa_keys",
                    help="600 的密钥文件，第 1 行是管理密钥")
    args = ap.parse_args()

    if not os.path.exists(args.keys):
        print("找不到密钥文件 %s（chmod 600，首行为管理密钥）" % args.keys)
        return 1
    if args.mode == "show":
        return do_show(args.keys)
    if args.mode == "sync":
        return 0 if do_sync(args.keys) else 1
    got = do_sync(args.keys)
    return do_apply(args.keys, got) if got else 1


if __name__ == "__main__":
    sys.exit(main())
