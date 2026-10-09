#!/usr/bin/env bash
# 决定性实验：muse-spark-* / jev-1.13-free 在 chat/completions 上即使被套壳
# 也仍然 ModelProtocolUnsupported（它们就在 learned 里，说明套壳确实生效了）。
# 那它们是不是只认 Anthropic 协议？给 /v1/messages 带上完整官方标识试一次。
#
# 官方标识的 5 个条件（见 internal/forward/freegate.go 注释）：
#   1 User-Agent  2 x-opencode-* 头  3 stream:true  4 system 里 ≥1200 字节的原文提示词
#   5 带 tools 时必须凑够 5 个客户端工具名（本实验不带 tools，走 title 模板）
set -u

API=http://127.0.0.1:9000
ACCT=${ACCT:-zen-01}
UA="opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

cat > /tmp/replay.py <<'PY'
import json, os, random, string, subprocess, sys

chunk = open('/tmp/chunk_title.txt', encoding='utf-8').read()
hexs = '0123456789abcdef'
b62 = string.ascii_letters + string.digits

def cid(prefix):
    return prefix + ''.join(random.choice(hexs) for _ in range(12)) + ''.join(random.choice(b62) for _ in range(14))

ua = os.environ['UA']
api, acct = os.environ['API'], os.environ['ACCT']

def call(model, path, with_identity, stream=True, system=None):
    if path.endswith('/v1/messages'):
        body = {"model": model, "max_tokens": 120,
                "messages": [{"role": "user", "content": "Say OK"}]}
        if system: body["system"] = system
    else:
        msgs = []
        if system: msgs.append({"role": "system", "content": system})
        msgs.append({"role": "user", "content": "Say OK"})
        body = {"model": model, "messages": msgs, "max_tokens": 120}
    if stream: body["stream"] = True

    cmd = ['curl', '-sS', '-m', '60', '-o', '/tmp/out.bin', '-w', '%{http_code}', '-X', 'POST',
           f'{api}/{acct}{path}', '-H', 'Content-Type: application/json']
    if with_identity:
        s = cid('ses_')
        cmd += ['-H', f'User-Agent: {ua}', '-H', 'x-opencode-client: cli',
                '-H', 'x-opencode-project: global',
                '-H', f'x-opencode-request: {cid("msg_")}',
                '-H', f'x-opencode-session: {s}', '-H', f'x-opencode-session-id: {s}']
    cmd += ['--data-binary', json.dumps(body)]
    code = subprocess.run(cmd, capture_output=True, text=True).stdout.strip()
    raw = open('/tmp/out.bin', 'rb').read().decode('utf-8', 'replace')
    first = raw.strip().splitlines()[0][:110] if raw.strip() else '(空)'
    return code, first, body

MODELS = ["muse-spark-1.3-contributor-free", "muse-spark-1.2-contributor-free",
          "jev-1.13-free", "space-bunny-free", "big-pickle"]

print("=== A) /v1/messages + 完整官方标识 + 注入提示词 ===")
for m in MODELS:
    code, first, _ = call(m, '/v1/messages', True, system=chunk)
    print('  %-34s HTTP=%-4s %s' % (m, code, first))

print()
print("=== B) /v1/messages + 官方标识但不注入提示词（看提示词是不是必需） ===")
for m in MODELS:
    code, first, _ = call(m, '/v1/messages', True, system=None)
    print('  %-34s HTTP=%-4s %s' % (m, code, first))

print()
print("=== C) 对照：/v1/chat/completions + 官方标识 + 注入提示词 ===")
for m in MODELS:
    code, first, _ = call(m, '/v1/chat/completions', True, system=chunk)
    print('  %-34s HTTP=%-4s %s' % (m, code, first))
PY

UA="$UA" API="$API" ACCT="$ACCT" python3 /tmp/replay.py
rm -f /tmp/replay.py /tmp/out.bin
