#!/usr/bin/env python3
"""Find configs still holding a retired credential, and optionally replace it.

Rotating a credential on the server is half a rotation. Every client config holds
its own copy, and a client that still has the old value does not see a rotation --
it sees a 401, and the natural reading of that is "the service is broken" rather
than "my copy is stale". That happened here with the CLIProxyAPI inbound key: the
server was rotated, three client configs on another machine were not, and the
symptom (401 Invalid API key, and a summarization step that silently stopped
working) pointed at the proxy instead of at the configs.

So: after any rotation, sweep for the old value. The test is not "I changed the
server", it is "nothing anywhere still holds the old value".

Reports paths and line numbers only. Never prints a credential.

Usage:
    sweep-stale-credential.py --old OLD_FILE [--new NEW_FILE | --apply] [ROOT ...]

    --old FILE     file containing the retired value (a file, not the value, so it
                   never lands in shell history or a process list)
    --new FILE     file containing the replacement
    --apply        rewrite matches that contain the old value; needs --new
    --quiet        only print files, no commentary
    ROOT ...       directories to walk (default: the usual per-user config roots)
"""
import io
import os
import sys

SKIP_DIRS = {'node_modules', 'tmp', 'teammate', 'usage-history', '.git', 'logs',
             'sessions', '__pycache__', 'dist', 'build'}
EXT = {'.json', '.yaml', '.yml', '.toml', '.env', '.ini', '.conf', '.cfg', '.ps1', '.sh',
       '.properties', '.txt'}
DEFAULT_ROOTS = ['.dsh', '.pi/agent', '.pi/backups', '.codex', '.claude', '.config']


def value_of(path):
    return io.open(path, encoding='utf-8').read().strip()


def main():
    args = sys.argv[1:]
    old_file = new_file = None
    apply_ = quiet = False
    rest = []
    while args:
        a = args.pop(0)
        if a == '--old':
            old_file = args.pop(0)
        elif a == '--new':
            new_file = args.pop(0)
        elif a == '--apply':
            apply_ = True
        elif a == '--quiet':
            quiet = True
        else:
            rest.append(a)

    if not old_file:
        print(__doc__.strip().split('Usage:')[1].strip(), file=sys.stderr)
        return 2
    old = value_of(old_file)
    new = value_of(new_file) if new_file else None

    if apply_ and not new:
        print('--apply needs --new', file=sys.stderr)
        return 2
    if new is not None and old == new:
        print('the replacement is identical to the retired value', file=sys.stderr)
        return 2

    home = os.path.expanduser('~')
    roots = [r if os.path.isabs(r) else os.path.join(home, r) for r in (rest or DEFAULT_ROOTS)]

    # The --old and --new files hold the values by definition, so if either sits
    # inside a scanned root it matches itself and the tool reports a stale copy
    # that is really just its own input. Measured: that produced a permanent
    # false positive, and a sweeper that always finds something gets ignored.
    ignore = {os.path.realpath(p) for p in (old_file, new_file) if p}

    hits = []
    scanned = 0
    for base in roots:
        if not os.path.isdir(base):
            continue
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for fn in filenames:
                if os.path.splitext(fn)[1] not in EXT:
                    continue
                path = os.path.join(dirpath, fn)
                if os.path.realpath(path) in ignore:
                    continue
                try:
                    if os.path.getsize(path) > 4 * 1024 * 1024:
                        continue
                    text = io.open(path, encoding='utf-8', errors='replace').read()
                except OSError:
                    continue
                scanned += 1
                if old in text:
                    hits.append((path, text))

    if not quiet:
        print('扫过 %d 个配置文件，%d 个仍在用旧凭据：' % (scanned, len(hits)))
    for path, _ in hits:
        print('  %s' % path.replace(home, '~'))

    if not hits:
        return 0

    if not apply_:
        if not quiet:
            print()
            print('（加 --apply --new <file> 才会改写）')
        return 1

    for path, text in hits:
        io.open(path, 'w', encoding='utf-8', newline='').write(
            text.replace(old, new))
        if not quiet:
            print('  已更新 %s' % path.replace(home, '~'))

    left = 0
    for path, _ in hits:
        if old in io.open(path, encoding='utf-8', errors='replace').read():
            left += 1
    if left:
        print('  复查失败，仍有 %d 处残留' % left, file=sys.stderr)
        return 1
    if not quiet:
        print('  复查：0 处残留')
    return 0


if __name__ == '__main__':
    sys.exit(main())
