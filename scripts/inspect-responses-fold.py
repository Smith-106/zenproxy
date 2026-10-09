#!/usr/bin/env python3
"""Print the shape of a folded Responses object, without printing it whole.

The fold passes `response.completed`'s `response` field through byte-exact, so
the output is the upstream's own object. This checks that the parts a client
actually reads survived: the output items, their text, and the usage block.

Usage: python3 inspect-responses-fold.py /tmp/e2e-responses-last.json
"""
import json
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/e2e-responses-last.json"
j = json.load(open(path, encoding="utf-8"))

print("顶层键:", sorted(j.keys()))
print("object :", j.get("object"), " status :", j.get("status"))
print("model  :", j.get("model"))
print("usage  :", json.dumps(j.get("usage"), ensure_ascii=False))
print("tools  :", len(j.get("tools") or []), "条")
print("instructions 前缀:", repr((j.get("instructions") or "")[:60]))
print()

for i, item in enumerate(j.get("output") or []):
    print(f"output[{i}] type={item.get('type')} status={item.get('status')} role={item.get('role')}")
    for c in item.get("content") or []:
        text = c.get("text") or ""
        print(f"   content type={c.get('type')} len={len(text)}")
        print("     文本:", repr(text[:300]))
    for s in item.get("summary") or []:
        print("   summary:", repr((s.get("text") or "")[:200]))
