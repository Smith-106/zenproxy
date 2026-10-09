#!/usr/bin/env python3
"""把部署配置里 proxies.resin.accounts 的槽位扩到（或缩到）N 个。

逐行扫描定位，不用跨行正则（那玩意儿在后向回溯上会炸）。
只重写 accounts: 下面的 "- slot-NN" 若干行，其余字节原样保留。

用法：
    sudo python3 set-resin-slots.py 24
    ZENPROXY_CONFIG=./config.local.yaml python3 set-resin-slots.py 8
"""
import os
import sys

PATH = os.environ.get("ZENPROXY_CONFIG", "/opt/zenproxy/config.yaml")
N = int(sys.argv[1]) if len(sys.argv) > 1 else 24

lines = open(PATH, encoding="utf-8").read().splitlines(keepends=True)

def indent_of(s: str) -> int:
    return len(s) - len(s.lstrip(" "))


# 找到 resin 段里的 "    accounts:"（4 空格缩进），并确认它属于 proxies.resin
start = None
for i, ln in enumerate(lines):
    if ln.startswith("    accounts:") and "resin" not in ln:
        # 往上找父级：第一个缩进比它**浅**的非注释行。
        # ★ 不能只往上找「第一个非注释行」——accounts: 上面紧挨着的是同级缩进的
        #   token:/platform:，那样会在第一行就 break，永远找不到段头。
        for j in range(i - 1, -1, -1):
            s = lines[j]
            if not s.strip() or s.lstrip().startswith("#"):
                continue
            if indent_of(s) < indent_of(ln):
                if s.startswith("  resin:"):
                    start = i
                break
        if start is not None:
            break

if start is None:
    sys.exit("could not find proxies.resin.accounts in " + PATH)

end = start + 1
while end < len(lines) and lines[end].startswith("      - "):
    end += 1

current = [ln.strip()[2:].strip().strip('"') for ln in lines[start + 1:end]]
want = ["slot-%02d" % i for i in range(1, N + 1)]

if current == want:
    print("already %d slots, nothing to do" % N)
    sys.exit(0)

body = ['      - "%s"\n' % name for name in want]
open(PATH, "w", encoding="utf-8").write("".join(lines[: start + 1] + body + lines[end:]))
print("wrote %d slots (%d -> %d)" % (N, len(current), N))
