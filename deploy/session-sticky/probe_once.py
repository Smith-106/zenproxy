#!/usr/bin/env python3
"""Send two same-conversation and one different-conversation probes. No secrets printed."""

import json
import re
import urllib.request
from pathlib import Path

cfg = Path("/cfg").read_text(encoding="utf-8")
m = re.search(r"http://session-sticky:8091/v1", cfg)
if not m:
    raise SystemExit("sticky base url missing")
# pull the openai-compatibility JSON array and find the sub2api/session-sticky entry's first key
line = next(ln for ln in cfg.splitlines() if "session-sticky:8091" in ln)
raw = line.split(":", 1)[1].strip()
entries = json.loads(raw)
entry = next(e for e in entries if "session-sticky" in e.get("base-url", ""))
key = entry["keys"][0]["api-key"]

URL = "http://session-sticky:8091/v1/chat/completions"
MODEL = "muse-spark-1.3-contributor-free"


def send(tag: str, first: str, second: str) -> None:
    body = {
        "model": MODEL,
        "stream": False,
        "messages": [
            {"role": "system", "content": "reply with the single word pong"},
            {"role": "user", "content": first},
            {"role": "assistant", "content": "ack"},
            {"role": "user", "content": second},
        ],
    }
    req = urllib.request.Request(
        URL,
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            raw_body = resp.read(200)
            print(tag, "HTTP", resp.status, "bytes", len(raw_body))
    except Exception as exc:
        code = getattr(exc, "code", None)
        detail = ""
        if hasattr(exc, "read"):
            detail = exc.read()[:180].decode("utf-8", "replace").replace("\n", " ")
        print(tag, "FAIL", code or type(exc).__name__, detail)


same = "sticky-probe alpha conversation prefix 20261011 do not answer with secrets"
send("same-a", same, "turn A say pong")
send("same-b", same, "turn B say pong")
send("other", "sticky-probe beta conversation prefix 20261011 completely different chat", "turn C say pong")
