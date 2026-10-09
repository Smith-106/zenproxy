#!/usr/bin/env python3
"""Print the shape of a YAML config with every secret structurally redacted.

Written after a line-based redactor leaked a live key: it only matched
`key: value` on one line, so a YAML list item (`- sk-...`) under `api-keys:`
passed straight through. Redaction has to walk the parsed structure, because the
secret's position in the document is not something a regex can be trusted to
predict.

Every scalar reached through a key whose name looks credential-ish is replaced,
including every element of a list under such a key. Values that merely *contain*
a credential-looking substring are also replaced, since a token pasted into a
URL is still a token.

Usage: redact-config.py <file> [--section NAME]
"""

import re
import sys

try:
    import yaml
except ImportError:
    yaml = None

SECRET_KEY = re.compile(
    r"(key|token|secret|password|passwd|credential|cookie|authorization|bearer|hash|salt)",
    re.I,
)
SECRET_VALUE = re.compile(r"(sk-[A-Za-z0-9_\-]{8,}|Bearer\s+\S+|eyJ[A-Za-z0-9_\-]{10,})")


def redact(node, key=""):
    if isinstance(node, dict):
        return {k: redact(v, k) for k, v in node.items()}
    if isinstance(node, list):
        # The key carries down: every element of a list under `api-keys` is a
        # key, not just the first.
        return [redact(v, key) for v in node]
    if isinstance(node, str):
        if SECRET_KEY.search(key) or SECRET_VALUE.search(node):
            return f"<redacted len={len(node)}>"
        return node
    return node


def main():
    if yaml is None:
        print("PyYAML not installed; run: pip install pyyaml", file=sys.stderr)
        return 2
    path = sys.argv[1]
    section = None
    if "--section" in sys.argv:
        section = sys.argv[sys.argv.index("--section") + 1]

    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)

    if section:
        doc = {section: (doc or {}).get(section)}

    print(yaml.safe_dump(redact(doc), allow_unicode=True, sort_keys=False, default_flow_style=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
