#!/usr/bin/env python3
"""从 CLIProxyAPI 的配置里取出「入站 api-key」（客户端访问 CPA 时要带的那把）。

注意区分两个同名字段（实测踩过）：
  - access.api-keys      <- 这个才是入站 key，客户端拿它来访问 CPA
  - 顶层 api-keys        <- 这是 **provider 凭据**段（codex/claude/xai/openai-compatibility），
                            里面塞的是各家上游的 key，不是入站 key
只打印第一条入站 key，其余一律不输出。

用法：
    python3 extract-cpa-inbound-key.py /opt/cli-proxy-api/config.yaml
    python3 extract-cpa-inbound-key.py --list /opt/cli-proxy-api/config.yaml   # 只看条数
"""
import re
import sys

args = [a for a in sys.argv[1:] if not a.startswith("--")]
show_count = "--list" in sys.argv
if not args:
    sys.exit("usage: extract-cpa-inbound-key.py [--list] <config.yaml>")

text = open(args[0], encoding="utf-8", errors="replace").read()

# 只认 access: 段下面的 api-keys 列表
m = re.search(r"(?m)^access:\n(?:.*\n)*?^    api-keys:\n((?:^        - .+\n)+)", text)
if not m:
    sys.exit("no access.api-keys found in " + args[0])

keys = []
for line in m.group(1).splitlines():
    v = line.strip().lstrip("- ").strip().strip("\"'")
    if v:
        keys.append(v)

if not keys:
    sys.exit("access.api-keys is empty")

if show_count:
    print("access.api-keys count=%d" % len(keys))
else:
    print(keys[0])
