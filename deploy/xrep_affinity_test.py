#!/usr/bin/env python3
"""Cross-replica session-affinity cache test.

Turn 1 goes through replica A, turn 2 through replica B, turn 3 through A —
same session id, same account (zen-01), same growing prefix. If the shared
sessions.json sync works, B must adopt A's pin (log line on B: cache hit),
and the upstream must report cached_tokens > 0 on turn 2/3 despite the
replica hop. Also replays the billing story: what new-api will record.
"""
import json, subprocess, time, sys

SESS = "xrep-aff-%d" % (time.time() % 100000)
ACC = "ZEN_KEY_01"

def build(tag):
    filler = "Audit excerpt %d: " % tag + ("Ledger rows contain balance items, risk marks, and reconciliation offsets " "cross-checked for ticket-level consistency before scoring. " * 260)
    return json.dumps({
        "model": "muse-spark-1.3-contributor-free",
        "stream": False,
        "max_tokens": 32,
        "reasoning": {"effort": "low", "summary": "auto"},
        "messages": [{"role": "user", "content": filler + " Reply with exactly: ok tag=%d" % tag}],
    })

def turn(rep, tag):
    payload = build(tag)
    b64 = __import__("base64").b64encode(payload.encode()).decode()
    p = subprocess.run(["sudo", "docker", "exec", "zenproxy-zenproxy-%s-1" % rep, "sh", "-c",
        "echo %s | base64 -d > /tmp/req.json && "
        "wget -qO- -T 150 --header='Authorization: Bearer %s' "
        "--header='Content-Type: application/json' "
        "--header='x-session-id: %s' "
        "--post-data=/tmp/req.json http://127.0.0.1:9000/zen-01/v1/chat/completions && rm /tmp/req.json"
        % (b64, subprocess.run(["sudo","docker","exec","zenproxy-zenproxy-a-1","printenv",ACC],
                               capture_output=True, text=True).stdout.strip(), SESS)
    ], capture_output=True, text=True)
    dt0 = time.time()
    out = p.stdout.strip()
    if not out:
        print("turn(%s) FAILED: %s" % (rep, p.stderr[:200]))
        return
    try:
        j = json.loads(out)
    except Exception:
        print("turn(%s) bad json: %s" % (rep, out[:150]))
        return
    u = j.get("usage") or {}
    det = u.get("prompt_tokens_details") or {}
    print("turn(%s) tag=%d prompt=%s cached=%s (raw response detail)" % (rep, tag, u.get("prompt_tokens"), det.get("cached_tokens", 0)))

print("session:", SESS)
t1 = time.time()
turn("a", 1)
turn("b", 2)
turn("a", 3)

time.sleep(1)
for rep in ("a", "b"):
    p = subprocess.run(["sudo","docker","logs","zenproxy-zenproxy-%s-1" % rep,"--since","3m"], capture_output=True, text=True)
    lines = [l for l in (p.stdout+p.stderr).split("\n") if SESS[:12] in l]
    print("--- replica %s session lines:" % rep)
    for l in lines[-4:]:
        print("   ", l[:150])