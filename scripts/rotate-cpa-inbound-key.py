#!/usr/bin/env python3
"""Rotate CLIProxyAPI's inbound access.api-keys[0] in place.

Deliberately a targeted line edit rather than a YAML load/dump round trip: the
config is hand-written and ~25 KB, and safe_dump would discard every comment in
it. The edit asserts its way in and its way out, because "replace the list item
under api-keys" is exactly the kind of splice that silently takes the wrong line.

Verifies: the old key is gone, exactly one key remains, and the YAML still parses
to the same structure apart from that one value.
"""
import io
import secrets
import subprocess
import sys

PATH = '/opt/cli-proxy-api/config.yaml'


def run(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True, check=False)


before_raw = subprocess.run(['sudo', 'cat', PATH], capture_output=True, text=True, check=True).stdout
lines = before_raw.split('\n')

# --- locate the access: block, then its api-keys: list -----------------------
acc = [i for i, l in enumerate(lines) if l.rstrip() == 'access:' or l.startswith('access:')]
assert len(acc) == 1, 'access: 出现 %d 次' % len(acc)
start = acc[0]

# The block ends at the next top-level key.
end = len(lines)
for i in range(start + 1, len(lines)):
    l = lines[i]
    if l and not l[0].isspace() and not l.startswith('#'):
        end = i
        break
block = lines[start:end]

keyline = [i for i, l in enumerate(block) if l.strip().startswith('- ') and 'sk-' in l]
assert len(keyline) == 1, 'api-keys 列表项有 %d 条（期望 1）' % len(keyline)
bi = keyline[0]
old_line = block[bi]
old_key = old_line.strip().lstrip('- ').strip().strip('"').strip("'")
assert old_key, '取不到旧 key'

# --- generate the replacement, keeping the visible prefix convention --------
new_key = 'sk-cpa' + secrets.token_hex(21)  # 6 + 42 = 48; trim to the original length
new_key = new_key[:len(old_key)]
assert new_key != old_key and len(new_key) == len(old_key), '长度不一致'

indent = old_line[:len(old_line) - len(old_line.lstrip())]
block[bi] = '%s- "%s"' % (indent, new_key)
lines[start:end] = block
after_raw = '\n'.join(lines)

# --- structural verification ------------------------------------------------
import yaml
b = yaml.safe_load(before_raw)
a = yaml.safe_load(after_raw)
b_keys = b['access']['api-keys']
a_keys = a['access']['api-keys']
assert a_keys == [new_key], '写回后 api-keys 不等于新 key'
assert len(a_keys) == len(b_keys) == 1, '条数变了'

# Everything except that one value must be byte-identical in structure.
b['access']['api-keys'] = ['<x>']
a['access']['api-keys'] = ['<x>']
assert a == b, '除该 key 外结构发生了变化 —— 拒绝写入'

# The old key must not survive anywhere in the file.
assert old_key not in after_raw, '旧 key 仍在文件中'

proc = subprocess.run(['sudo', 'tee', PATH], input=after_raw, capture_output=True, text=True)
assert proc.returncode == 0, '写入失败: %s' % proc.stderr
run('sudo chmod 600 %s' % PATH)

print('  旧 key: len=%d prefix=%s' % (len(old_key), old_key[:6] + '***'))
print('  新 key: len=%d prefix=%s（未回显）' % (len(new_key), new_key[:6] + '***'))
print('  结构比对: 除该 key 外完全一致')
print('  旧 key 残留: 无')
print('  已写回 %s（0600）' % PATH)
