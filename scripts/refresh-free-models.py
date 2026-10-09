#!/usr/bin/env python3
"""把 /opt/zenproxy/config.yaml 的 free_tier.models 换成指定模型集。

逐行扫描定位，不用跨行正则（那玩意儿在后向回溯上会炸）。
只重写 models: 下面的 "- 名字" 若干行，其余字节原样保留 —— 注释和排版不动。

用法：
    sudo python3 refresh-free-models.py --show              # 只打印当前列表
    sudo python3 refresh-free-models.py model-a model-b ... # 替换成这些
"""
import os
import sys

PATH = os.environ.get("ZENPROXY_CONFIG", "/opt/zenproxy/config.yaml")

lines = open(PATH, encoding="utf-8").read().splitlines(keepends=True)

# 找到 "  models:" 行：必须在 free_tier: 段内，缩进 2 空格
start = None
for i, ln in enumerate(lines):
    # "  models:" 行尾可能挂着说明注释（本文件就是这样），只匹配前缀
    if ln.startswith("  models:") or ln.rstrip("\n") == "  models:":
        # 往上确认最近的顶层 key 是 free_tier:
        for j in range(i - 1, -1, -1):
            if lines[j] and not lines[j][0].isspace():
                if lines[j].startswith("free_tier:"):
                    start = i
                break
        if start is not None:
            break

if start is None:
    sys.exit("could not find free_tier.models in " + PATH)

# 收集紧跟着的 "    - xxx" 行
end = start + 1
while end < len(lines) and lines[end].startswith("    - "):
    end += 1

current = [ln.strip()[2:].strip() for ln in lines[start + 1:end]]

args = sys.argv[1:]
if not args or args[0] == "--show":
    print("current %d models:" % len(current))
    for c in current:
        print("  " + c)
    sys.exit(0)

# 第一个模型行上可能挂着行内注释，保留它
inline = ""
if current and "#" in lines[start + 1]:
    raw = lines[start + 1]
    inline = "   " + raw[raw.index("#"):].rstrip("\n")

body = []
for i, name in enumerate(args):
    body.append("    - %s%s\n" % (name, inline if i == 0 else ""))

open(PATH, "w", encoding="utf-8").write("".join(lines[: start + 1] + body + lines[end:]))
print("wrote %d models" % len(args))
