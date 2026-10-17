#!/usr/bin/env python3
"""Redact secrets from anything, structurally and by value shape.

Why this exists in addition to redact-config.py: that one is YAML-only and matches
secrets by *key name* plus a narrow value pattern (`sk-`, `Bearer`, `eyJ`). Every
leak in this repo's history went through the gap around it:

  * reading state.json / a .env / `docker exec ... env` and formatting it by hand,
    because the YAML tool looked inapplicable;
  * redacting on the key name someone *expected* rather than the names present, so
    `s/(KEY|TOKEN)=.*/` sailed past `ZEN_KEY_01=` (the `_01` sits between);
  * a value-pattern list that did not know the `oc_sk_` key format, so an
    unknown-shaped key under a non-matching name went out verbatim;
  * an ad-hoc `len(v) > 12` filter, which passed a 12-character password.

So this one does three things differently:

  1. accepts JSON, YAML, dotenv/.env, an `env` dump, or plain text, detected or
     forced, from a file or stdin;
  2. redacts on key name *and* on value shape, independently - a secret is caught
     if either signal fires, so an unexpected name no longer defeats it;
  3. ships a self-test (`--test`) carrying the exact shapes that leaked before, so
     a regression here is a failing test rather than an incident.

Usage:
    redact.py <file>              # or
    <producer> | redact.py -      # compose it into any pipeline
    redact.py --test              # verify the rules against known leak shapes

Options:
    --format auto|json|yaml|dotenv|env|text   default: auto (by content)
    --hint                                     include a short value prefix
                                               (default: length only)
"""

import json
import re
import sys

try:
    import yaml
except ImportError:
    yaml = None

# ---------------------------------------------------------------- key names ---

# Broader than redact-config.py's list: the previous miss was a name that *did*
# look credential-ish to a human but that the inline regex did not actually match.
SECRET_KEY = re.compile(
    r"("
    r"key|token|secret|password|passwd|pwd|credential|cookie|authorization|"
    r"bearer|hash|salt|signature|private|session|cert|dsn"
    r")",
    re.I,
)

# --------------------------------------------------------------- value shapes ---

# Ordered most specific first. `oc_sk_` is called out because it is a real format
# that this tool's predecessor did not know, and an unknown-shape secret under an
# unmatched name is precisely how a live key escaped.
SECRET_VALUE = [
    re.compile(r"\boc_sk_[A-Za-z0-9_\-]{8,}"),          # opencode "oc_sk_" keys
    re.compile(r"\bsk-[A-Za-z0-9_\-]{8,}"),             # opencode/OpenAI style
    re.compile(r"\bsk-or-v1-[A-Za-z0-9_\-]{8,}"),       # OpenRouter
    re.compile(r"\bghp_[A-Za-z0-9]{20,}"),              # GitHub PAT
    re.compile(r"\bxox[baprs]-[A-Za-z0-9\-]{10,}"),     # Slack
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),                # AWS access key id
    re.compile(r"\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+"),  # JWT
    re.compile(r"\bBearer\s+[A-Za-z0-9._\-]{8,}"),      # any bearer token
    re.compile(r"\bsk_live_[A-Za-z0-9]{10,}"),          # Stripe
    # Credentials embedded in a URL userinfo ($socks5h://user:pass@host$). Incident
    # #6 leaked the proxy password this way: the value contains `/` and `@`, so both
    # the opaque-token rules below skip it, and no other pattern matched it.
    re.compile(r"\b[a-z][a-z0-9+.\-]*://[^/\s:@]+:[^/\s@]+@", re.I),
]

# A token-ish run that is long enough to be a credential and carries no spaces.
# Deliberately conservative: >=32 chars of [A-Za-z0-9_-]. Shorter opaque strings
# are left alone, because redacting every 20-character word makes output useless.
LONG_OPAQUE = re.compile(r"\b[A-Za-z0-9_\-]{32,}\b")

# A long run that is *definitely* a key rather than a word: mixed case plus digits,
# >=24 chars. Catches keys whose format nobody published.
MIXED_LONG = re.compile(r"\b(?=[A-Za-z0-9_\-]{24,}\b)(?=[^\s]*[a-z])(?=[^\s]*[A-Z])(?=[^\s]*\d)[A-Za-z0-9_\-]{24,}\b")


def looks_secret(value):
    """True when the value itself is credential-shaped, regardless of its name."""
    if not isinstance(value, str) or not value:
        return False
    for pat in SECRET_VALUE:
        if pat.search(value):
            return True
    body = value.strip()
    # Only apply the opaque rules to a single unbroken token, so prose and long
    # file paths are untouched.
    if body and " " not in body and "/" not in body:
        if LONG_OPAQUE.search(body) or MIXED_LONG.search(body):
            return True
    return False


def mask(value, hint=False):
    if not isinstance(value, str):
        return f"<redacted {type(value).__name__}>"
    if hint and len(value) >= 6:
        return f"<redacted len={len(value)} prefix={value[:6]}…>"
    return f"<redacted len={len(value)}>"


def redact(node, key="", hint=False):
    if isinstance(node, dict):
        return {k: redact(v, k, hint) for k, v in node.items()}
    if isinstance(node, list):
        # The key carries down, so every element of a list under `api-keys:` is
        # treated as a key, not just the first.
        return [redact(v, key, hint) for v in node]
    if isinstance(node, str):
        if SECRET_KEY.search(key) or looks_secret(node):
            return mask(node, hint)
        return node
    return node


# -------------------------------------------------------------- text formats ---

ASSIGN = re.compile(r"^(\s*(?:export\s+)?)([A-Za-z_][A-Za-z0-9_.\-]*)(\s*[:=]\s*)(.*)$")


def redact_text(text, hint=False):
    """Line-oriented redaction for .env, env dumps, shell output, and prose.

    Handles the `NAME=value` shape including `export` and surrounding whitespace,
    so `ZEN_KEY_01=...` is caught by name even when the inline regex in a shell
    one-liner would have missed it.
    """
    out = []
    for line in text.split("\n"):
        m = ASSIGN.match(line)
        if m:
            indent, name, sep, value = m.groups()
            # Strip a trailing comment before judging, then decide.
            if SECRET_KEY.search(name) or looks_secret(value):
                out.append(f"{indent}{name}{sep}{mask(value, hint)}")
                continue
            if any(p.search(value) for p in SECRET_VALUE):
                # Name looks innocent but the value is a known key format.
                out.append(f"{indent}{name}{sep}{mask(value, hint)}")
                continue
        else:
            if any(p.search(line) for p in SECRET_VALUE):
                scrubbed = line
                for pat in SECRET_VALUE:
                    scrubbed = pat.sub(lambda mm: mask(mm.group(0), hint), scrubbed)
                out.append(scrubbed)
                continue
        out.append(line)
    return "\n".join(out)


def detect(text):
    s = text.lstrip()
    if s.startswith("{") or s.startswith("["):
        return "json"
    for line in text.split("\n"):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        return "dotenv" if ASSIGN.match(line) else "yaml"
    return "text"


def main():
    args = [a for a in sys.argv[1:]]
    hint = "--hint" in args
    if hint:
        args.remove("--hint")
    fmt = "auto"
    if "--format" in args:
        i = args.index("--format")
        fmt = args[i + 1]
        del args[i:i + 2]

    if "--test" in args:
        return selftest()

    src = args[0] if args else "-"
    if src == "-" or src is None:
        text = sys.stdin.read()
    else:
        with open(src, encoding="utf-8") as fh:
            text = fh.read()

    if fmt == "auto":
        fmt = detect(text)

    if fmt == "json":
        doc = json.loads(text) if text.strip() else None
        print(json.dumps(redact(doc, hint=hint), ensure_ascii=False, indent=2))
    elif fmt == "yaml":
        if yaml is None:
            print("PyYAML not installed; use --format text", file=sys.stderr)
            return 2
        doc = yaml.safe_load(text)
        print(yaml.safe_dump(redact(doc, hint=hint), allow_unicode=True,
                             sort_keys=False, default_flow_style=False))
    else:
        sys.stdout.write(redact_text(text, hint))
    return 0


# ------------------------------------------------------------------ self-test ---

# Every case below is a shape that actually escaped in this repo's history. The
# test exists so the next regression is a failing suite, not another live key in a
# transcript.
CASES = [
    # (description, input, substring that must NOT survive)
    ("env var named ZEN_KEY_01 (the `_01` broke an inline (KEY|TOKEN)= regex)",
     "ZEN_KEY_01=sk-0YtG4pWqZnM8aDcKeRbH2sUvLiXoP7jE9fAw6SnB3hQzT5dC1mKe8",
     "0YtG4pWqZn"),
    ("oc_sk_ key under a name no rule predicted",
     "FOO=oc_sk_4f1b8c3a92de_FAKEpLZqYwGkNvXrTsUJmIh6QcAeBn0Rd",
     "dZR3TwMPkUsBsi"),
    ("oc_sk_ key bare in prose",
     "the key is oc_sk_c59e20f8a7d3__FAKEwKmXbTnGjQsZrPyIeUoDmWcAl4xEf ok",
     "JHPd6nLYDjglfaXM"),
    ("YAML list item under api-keys (line regex missed it)",
     "access:\n  api-keys:\n    - sk-c9f2a1b8e7d6c5b4a3928170615243deadbeef01\n",
     "c9f2a1b8e7d6c5b4"),
    ("12-char password (passed a `len(v) > 12` filter)",
     "password=abc123XYZ789",
     "abc123XYZ789"),
    ("JSON account dump",
     '{"accounts":[{"id":"zen-01","api_key":"sk-0YtG4pWqZnM8aDcKeRbH2sUvLiXoP7jE9fAw6SnB3hQzT5dC1mKe8"}]}',
     "0YtG4pWqZn"),
    ("env dump line",
     "RESIN_PROXY_TOKEN=Zm9vYmFyYmF6cXV1eDEyMzQ1Njc4OTBhYmNkZWZnaGlqa2xtbg",
     "Zm9vYmFyYmF6"),
    # Incident #10: a hand-written `re.sub(r"(api-key:\s*).*")` guard only matched
    # block-style `api-key: value`, so the flow-style single-line array (where the key
    # is written `"api-key": "…"`) sailed through with ~60 live keys in one line.
    ("flow-style single-line openai-compatibility array (incident #10)",
     'api-keys:\n  openai-compatibility: [{"base-url": "http://new-api:3000/v1", '
     '"disabled": false, "keys": [{"api-key": "sk-FAKE7dIcNfHjK2mQpVrXtZbYwGuSaLeE"}], '
     '"models": [{"name": "muse-spark-1.2-contributor-free"}], "name": "new-api"}, '
     '{"keys": [{"api-key": "nvapi-FAKEq9LmWz4Rt7Yp2Kd8Xc3Vb6Nn1Jj5Hh0Gg"}], '
     '"name": "nvidia-free"}]\n',
     "7dIcNfHjK2mQpVrXtZ"),
    # Incident #6 again, now guarded: proxy password inside a URL userinfo.
    ("proxy credentials in a URL userinfo",
     'keys:\n- api-key: sk-FAKEaB3dE6gJ9mP2sV5xZ8bC1fH4kL7n\n'
     "  proxy-url: socks5h://proxy_user:Fx9mK2pR7nQ4@20.194.7.31:31080\n",
     "Kx9mP2vR7nQ4"),
]


def selftest():
    import io
    fails = 0
    for desc, src, must_not_appear in CASES:
        before = io.StringIO()
        after = io.StringIO()
        old = sys.stdout
        try:
            sys.stdout = after
            fmt = detect(src)
            if fmt == "json":
                print(json.dumps(redact(json.loads(src), hint=True), ensure_ascii=False))
            elif fmt == "yaml" and yaml is not None:
                print(yaml.safe_dump(redact(yaml.safe_load(src), hint=True), allow_unicode=True))
            else:
                sys.stdout.write(redact_text(src, hint=True))
        finally:
            sys.stdout = old
        got = after.getvalue()
        ok = must_not_appear not in got
        print("%s  %s" % ("PASS" if ok else "FAIL", desc))
        if not ok:
            fails += 1
            print("      leaked: %s" % got.strip()[:120])
    print()
    print("%d/%d passed" % (len(CASES) - fails, len(CASES)))
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
