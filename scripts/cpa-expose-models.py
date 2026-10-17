#!/usr/bin/env python3
"""让 CPA 的 openai-compatibility provider 按**客户端真正请求的名字**暴露模型。

## 这个脚本解决的那个 400

客户端拿到过：

    POST https://cpa.nikoniko.tech/v1/chat/completions
    {"model":"muse-spark-1.2-contributor-free", ...}

    400 {"error":{"message":"unknown provider for model muse-spark-1.2-contributor-free",
                  "type":"invalid_request_error","code":"model_not_found","param":"model"}}

关键反直觉点：**这个模型确实在 CPA 的 /v1/models 列表里**。列表里有、请求打不通，
因为 CPA v8.0.15 的 model 条目有两个名字：

    {"name": <转发给上游的名字>, "alias": <客户端可用名>}

`alias` 非空时，**只有 alias 是客户端可用名**；`name` 只是个上游侧的名字，
`/v1/models` 却仍然把 `name` 列出来。于是：

    alias: ""                                  -> 暴露 name，客户端按 name 请求 OK
    alias: "muse-spark-1.3-contributor-free"   -> 只暴露 1.3，请求 1.2 得到
                                                  400 unknown provider（列表里还看得见 1.2）

实测证据（同一 provider、同一 key）：

    glm-5.3-flash   (alias) -> 200
    glm-5.3-flash-free (name) -> 400 unknown provider

## 怎么修

免费模型一律以 `alias: ""` 挂载，并对**历史客户端名**单独留条目（而不是借用 alias，
因为 alias 会把那个名字从目录里挤掉）。历史名的上游改写交给 new-api 的
`model_mapping`，不要在 CPA 这层做双份映射。

## 为什么不能整体 yaml.safe_dump

`/opt/cli-proxy-api/config.yaml` 的 `api-keys.openai-compatibility` 是**单行 flow-style**，
整文件重写会抹掉注释与既有排版，还会把这一整行重新折行（这行的打印事故在本仓库
发生过多次，见 docs/SECRETS.md）。所以这里只做**精确子串替换**：
定位 provider 的 flow 映射 -> 在其内部找到 `"models": [...]`（按括号配对）-> 只替换那段。

## 用法

    # 只看现状（不写盘）
    sudo scripts/cpa-expose-models.py --provider new-api \\
        --models muse-spark-1.2-contributor-free,glm-5.3-flash-free,big-pickle

    # 应用（自动备份，改完提示重启）
    sudo scripts/cpa-expose-models.py --provider new-api --models ... --apply

    # 从 new-api 现有免费模型清单生成 --models（逗号分隔）
    sudo scripts/cpa-expose-models.py --provider new-api --from-newapi --apply

改完必须 `sudo docker restart cli-proxy-api`：模型索引只在启动时构建（实测热改文件
不生效）。
"""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import time

import yaml

DEFAULT_CFG = "/opt/cli-proxy-api/config.yaml"


# --------------------------------------------------------------- text surgery ---

def find_flow_entry(text: str, provider: str) -> tuple[int, int]:
    """Return (start, end) of the flow mapping `{...}` for `provider`.

    Only flow-style entries are handled; a block-style config is a different file
    shape and reformatting it wholesale is exactly what this script refuses to do.
    """
    needle = '"name": "%s"' % provider
    pos = text.find(needle)
    if pos < 0:
        raise SystemExit(
            "找不到 flow-style 的 provider 条目 %r。\n"
            "（本脚本只做精确子串替换，不会整体重排 YAML；块状写法的文件请手动改。）"
            % provider)

    start = text.rfind("{", 0, pos)
    if start < 0:
        raise SystemExit("provider 条目起点定位失败")
    depth, i, in_str, esc = 0, start, False, False
    while i < len(text):
        ch = text[i]
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
        else:
            if ch == '"':
                in_str = True
            elif ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
                if depth == 0:
                    return start, i + 1
        i += 1
    raise SystemExit("provider 条目结束括号定位失败")


def find_models_array(entry: str) -> tuple[int, int]:
    """Return (start, end) of the `[ ... ]` following `"models":` inside entry."""
    key = entry.find('"models":')
    if key < 0:
        raise SystemExit("该 provider 没有 models 字段")
    start = entry.find("[", key)
    depth, i, in_str, esc = 0, start, False, False
    while i < len(entry):
        ch = entry[i]
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
        else:
            if ch == '"':
                in_str = True
            elif ch == "[":
                depth += 1
            elif ch == "]":
                depth -= 1
                if depth == 0:
                    return start, i + 1
        i += 1
    raise SystemExit("models 数组结束括号定位失败")


def dump_model(item: dict) -> str:
    """Serialize one model entry in the same flow style as the surrounding file."""
    parts = []
    for k, v in item.items():
        parts.append('"%s": %s' % (k, yaml.safe_dump(
            v, allow_unicode=True, default_flow_style=True).strip().rstrip("...")))
    return "{" + ", ".join(parts) + "}"


# ---------------------------------------------------------------------- issues ---

def audit(entry: dict, wanted: list[str]) -> tuple[list[str], list[str]]:
    """Return (missing, mis-exposed) model names.

    missing      : wanted but no entry exposes it (absent, or only behind an alias)
    mis-exposed  : entry whose `name` is a wanted model but whose alias hides it
    """
    by_exposed: dict[str, dict] = {}
    hidden: dict[str, str] = {}
    for item in entry.get("models") or []:
        name = item.get("name") if isinstance(item, dict) else item
        alias = item.get("alias") if isinstance(item, dict) else None
        if alias:
            hidden[name] = alias          # name is reachable only as `alias`
            by_exposed[alias] = item
        else:
            by_exposed[name] = item
    missing = [m for m in wanted if m not in by_exposed]
    mis = [m for m in wanted if m in hidden]
    return missing, mis


def extras_for(entry: dict, model: str) -> dict:
    """Reuse thinking/vision flags recorded for a model under either of its names."""
    for item in entry.get("models") or []:
        if not isinstance(item, dict):
            continue
        if item.get("name") == model or item.get("alias") == model:
            return {k: v for k, v in item.items() if k not in ("name", "alias")}
    return {}


# ----------------------------------------------------------------------- main ---

def models_from_newapi() -> list[str]:
    """Free-tier models new-api currently serves (group default).

    Reads the DB read-only; prints nothing but model names.
    """
    sql = ("SELECT string_agg(DISTINCT model, ',' ORDER BY model) FROM abilities "
           "WHERE model LIKE '%free%' AND enabled = true;")
    out = subprocess.run(
        ["docker", "exec", "postgres", "psql", "-U", "root", "-d", "new-api",
         "-t", "-A", "-c", sql],
        capture_output=True, text=True, check=True).stdout.strip()
    return [m for m in out.split(",") if m]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default=DEFAULT_CFG)
    ap.add_argument("--provider", required=True,
                    help="openai-compatibility 条目名，如 new-api")
    ap.add_argument("--models", default="", help="逗号分隔的客户端模型名")
    ap.add_argument("--from-newapi", action="store_true",
                    help="从 new-api abilities 取启用中的 *free* 模型")
    ap.add_argument("--apply", action="store_true", help="写盘（默认只报告）")
    args = ap.parse_args()

    wanted = [m.strip() for m in args.models.split(",") if m.strip()]
    if args.from_newapi:
        wanted += [m for m in models_from_newapi() if m not in wanted]
    if not wanted:
        raise SystemExit("没有要暴露的模型：给 --models 或 --from-newapi")

    text = open(args.config, encoding="utf-8").read()
    doc = yaml.safe_load(text)
    entry = next((p for p in doc["api-keys"]["openai-compatibility"]
                  if p.get("name") == args.provider), None)
    if entry is None:
        raise SystemExit("找不到 provider: %s" % args.provider)

    visible = [m.get("name") if isinstance(m, dict) else m
               for m in (entry.get("models") or [])]
    print("provider      :", args.provider)
    print("当前 models   :", len(visible))
    for item in entry.get("models") or []:
        nm = item.get("name") if isinstance(item, dict) else item
        al = item.get("alias") if isinstance(item, dict) else None
        flag = ("  <- 只暴露 %r" % al) if al else ""
        print("   - %-42s%s" % (nm, flag))

    missing, mis = audit(entry, wanted)
    print()
    print("期望暴露      :", len(wanted))
    if missing:
        print("🔴 打不通      :", ", ".join(missing))
    if mis:
        print("🔴 被 alias 顶掉:", ", ".join(mis))
    if not missing and not mis:
        print("🟢 全部按本名暴露")

    if not args.apply:
        if missing or mis:
            print("\n（干跑）加 --apply 写入，然后 sudo docker restart cli-proxy-api")
        return 0

    # 重建 models 列表：wanted 全部 alias:""，其它既有条目原样保留（含历史名）
    keep = []
    for item in entry.get("models") or []:
        nm = item.get("name") if isinstance(item, dict) else item
        al = item.get("alias") if isinstance(item, dict) else None
        keep.append({"name": nm, "alias": al or ""}
                    if isinstance(item, dict) else {"name": nm, "alias": ""})
        if isinstance(item, dict):
            keep[-1].update({k: v for k, v in item.items()
                             if k not in ("name", "alias")})
    # 历史名（alias 非空）保留为新条目，只按 wanted 里没出现的上游名去重
    rebuilt: list[dict] = []
    seen: set[str] = set()
    for model in wanted:
        item = {"name": model, "alias": ""}
        item.update(extras_for(entry, model))
        rebuilt.append(item)
        seen.add(model)
    for item in keep:
        nm, al = item["name"], item.get("alias") or ""
        # 历史名：客户端名本身就是个独立名字时，作为单独条目保留
        if al and al not in seen:
            hist = {"name": al, "alias": ""}
            hist.update({k: v for k, v in item.items()
                         if k not in ("name", "alias")})
            rebuilt.append(hist)
            seen.add(al)
        elif not al and nm not in seen:
            rebuilt.append(item)
            seen.add(nm)

    start, end = find_flow_entry(text, args.provider)
    entry_text = text[start:end]
    ms, me = find_models_array(entry_text)
    new_array = "[" + ", ".join(dump_model(m) for m in rebuilt) + "]"
    new_entry = entry_text[:ms] + new_array + entry_text[me:]
    updated = text[:start] + new_entry + text[end:]

    # 校验：能解析、条目数对得上、没有残留的非空 alias
    check = yaml.safe_load(updated)
    e2 = next(p for p in check["api-keys"]["openai-compatibility"]
              if p.get("name") == args.provider)
    got = [m["name"] for m in e2["models"]]
    assert set(got) >= set(wanted), "改完仍缺: %s" % (set(wanted) - set(got))
    assert all((m.get("alias") or "") == "" for m in e2["models"]), \
        "还有非空 alias"
    assert set(check.keys()) == set(doc.keys()), "顶层键被改动了"

    ts = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    shutil.copy2(args.config, args.config + ".bak-expose-" + ts)
    tmp = args.config + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(updated)
    st = os.stat(args.config)
    os.chmod(tmp, st.st_mode & 0o777)
    os.chown(tmp, st.st_uid, st.st_gid)
    os.rename(tmp, args.config)

    print("\n已写入（%d 条）。备份: %s" % (len(got), args.config + ".bak-expose-" + ts))
    print("现在执行: sudo docker restart cli-proxy-api   # 模型索引只在启动时构建")
    return 0


if __name__ == "__main__":
    sys.exit(main())
