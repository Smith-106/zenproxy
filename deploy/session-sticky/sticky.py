#!/usr/bin/env python3
"""Inject a stable OpenAI session id before sub2api account selection.

CPA does not forward conversation headers. sub2api then falls back to a
content seed that includes tools + the full system prompt, which changes
every agent turn and breaks both account stickiness and upstream prompt cache.

This proxy only fronts the sub2api openai-compatibility base URL. It sets
session_id / conversation_id / x-opencode-session and, when absent,
prompt_cache_key. The key is the first user message prefix so later turns of
the same conversation stay on the same zen account.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

UPSTREAM = os.environ.get("UPSTREAM", "http://sub2api:8080").rstrip("/")
LISTEN = os.environ.get("LISTEN", "0.0.0.0:8091")
PREFIX_CHARS = 1500
MIN_PREFIX = 24
# Quota is per egress IP. Only brief-stampede circuit when zenproxy says
# EVERY exit is cooled — never hold for the full daily Reset-After.
CIRCUIT_MIN_RETRY_AFTER = int(os.environ.get("CIRCUIT_MIN_RETRY_AFTER", "30"))
# Short hold: absorb sub2api same-account retry storms, then re-probe IPs.
CIRCUIT_MAX_HOLD = int(os.environ.get("CIRCUIT_MAX_HOLD", "30"))
# Only trip on explicit all-exits-cooled (per-IP pool exhausted right now).
CIRCUIT_REQUIRE_ALL_COOLED = os.environ.get("CIRCUIT_REQUIRE_ALL_COOLED", "1") != "0"

_circuit_lock = threading.Lock()
# model -> (until_epoch, reason)
_circuits: dict[str, tuple[float, str]] = {}


def circuit_open(model: str) -> tuple[bool, int, str]:
    if not model:
        return False, 0, ""
    now = time.time()
    with _circuit_lock:
        until, reason = _circuits.get(model, (0.0, ""))
        if until > now:
            return True, max(1, int(until - now)), reason
        return False, 0, ""


def trip_circuit(model: str, retry_after: float, reason: str) -> None:
    if not model or retry_after < CIRCUIT_MIN_RETRY_AFTER:
        return
    hold = min(float(retry_after), float(CIRCUIT_MAX_HOLD))
    until = time.time() + hold
    with _circuit_lock:
        prev = _circuits.get(model)
        if not prev or until > prev[0]:
            _circuits[model] = (until, reason or "upstream rate limit")
            sys.stderr.write(
                "sticky-circuit-open model=%s hold=%ss reason=%s\n"
                % (model, int(hold), (reason or "")[:80])
            )


def parse_retry_after(headers) -> float:
    raw = headers.get("Retry-After") if headers is not None else None
    if not raw:
        return 0.0
    try:
        return float(str(raw).strip())
    except ValueError:
        return 0.0

HOP = {
    "host",
    "content-length",
    "transfer-encoding",
    "connection",
    "keep-alive",
    "proxy-connection",
    "te",
    "trailer",
    "upgrade",
}

SESSION_HEADERS = (
    "session_id",
    "session-id",
    "conversation_id",
    "x-opencode-session",
    "x-session-id",
    "x-session-affinity",
)


def text_of(content) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for part in content:
            if isinstance(part, str):
                parts.append(part)
            elif isinstance(part, dict):
                if isinstance(part.get("text"), str):
                    parts.append(part["text"])
                elif isinstance(part.get("content"), str):
                    parts.append(part["content"])
        return "\n".join(parts)
    return ""


def first_user_prefix(body: dict) -> str:
    messages = body.get("messages")
    if isinstance(messages, list):
        for msg in messages:
            if isinstance(msg, dict) and msg.get("role") == "user":
                text = text_of(msg.get("content")).strip()
                if text:
                    return text[:PREFIX_CHARS]
    raw = body.get("input")
    if isinstance(raw, str) and raw.strip():
        return raw.strip()[:PREFIX_CHARS]
    if isinstance(raw, list):
        for item in raw:
            if not isinstance(item, dict):
                continue
            role = item.get("role")
            kind = item.get("type")
            if role == "user" or kind in ("input_text", "message"):
                text = text_of(item.get("content") or item.get("text") or "").strip()
                if text:
                    return text[:PREFIX_CHARS]
    return ""


def header_get(headers, name: str) -> str:
    # BaseHTTPRequestHandler headers are case-insensitive.
    value = headers.get(name)
    return value.strip() if isinstance(value, str) else ""


def session_key(body: dict | None, headers) -> str:
    for name in SESSION_HEADERS:
        value = header_get(headers, name)
        if value:
            return value[:200]
    if not isinstance(body, dict):
        return ""
    for field in ("prompt_cache_key", "session_id", "conversation_id", "sessionId", "conversationId"):
        existing = body.get(field)
        if isinstance(existing, str) and existing.strip():
            return existing.strip()[:200]
    meta = body.get("metadata")
    if isinstance(meta, dict):
        for field in ("session_id", "conversation_id", "user_id", "sessionId", "conversationId", "chat_id"):
            val = meta.get(field)
            if isinstance(val, str) and val.strip():
                return val.strip()[:200]
    prefix = first_user_prefix(body)
    if len(prefix) < MIN_PREFIX:
        return ""
    digest = hashlib.sha256(prefix.encode("utf-8", "replace")).hexdigest()[:32]
    return "pfx_" + digest


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_GET(self) -> None:  # noqa: N802
        if self.path.split("?", 1)[0] in ("/healthz", "/health"):
            body = b'{"status":"ok"}\n'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self._forward()

    def do_POST(self) -> None:  # noqa: N802
        self._forward()

    def do_PUT(self) -> None:  # noqa: N802
        self._forward()

    def do_PATCH(self) -> None:  # noqa: N802
        self._forward()

    def do_DELETE(self) -> None:  # noqa: N802
        self._forward()

    def _forward(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        body = None
        content_type = self.headers.get("Content-Type", "")
        if raw and "application/json" in content_type.lower():
            try:
                parsed = json.loads(raw)
            except json.JSONDecodeError:
                parsed = None
            if isinstance(parsed, dict):
                body = parsed
        key = session_key(body, self.headers)
        # Prefer real client/CPA session headers when passthrough is on.
        # Content-hash pfx_* remains the fallback for clients that send none.
        if key and not key.startswith("pfx_"):
            sys.stderr.write("sticky-real-session model=%s key=%s\n" % (
                (body.get("model") if isinstance(body, dict) else None) or "-", key[:36]))
        injected = False
        if key and isinstance(body, dict) and not (
            isinstance(body.get("prompt_cache_key"), str) and body["prompt_cache_key"].strip()
        ):
            body["prompt_cache_key"] = key
            raw = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            injected = True

        headers = {}
        for name, value in self.headers.items():
            if name.lower() in HOP:
                continue
            headers[name] = value
        if key:
            headers["session_id"] = key
            headers["conversation_id"] = key
            headers["x-opencode-session"] = key
        if raw:
            headers["Content-Length"] = str(len(raw))
        elif "Content-Length" in headers:
            headers.pop("Content-Length", None)

        model = ""
        if isinstance(body, dict) and isinstance(body.get("model"), str):
            model = body["model"]
        sys.stderr.write(
            "sticky model=%s key=%s injected=%s bytes=%s\n"
            % (model or "-", (key[:12] + "...") if key else "-", injected, len(raw))
        )

        # Fail-fast circuit: after a long FreeUsageLimit 429, do not wait for
        # sub2api's same-account retry storm (retry_limit=10 × accounts).
        open_, secs, reason = circuit_open(model)
        if open_ and self.command == "POST" and "/chat/completions" in self.path.split("?", 1)[0]:
            msg = json.dumps({
                "error": {
                    "message": "Upstream rate limit exceeded, please retry later (%s)" % reason,
                    "type": "rate_limit_error",
                }
            }).encode()
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", str(secs))
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            sys.stderr.write("sticky-circuit-hit model=%s retry_after=%s\n" % (model, secs))
            return

        req = Request(
            UPSTREAM + self.path,
            data=raw if raw else None,
            headers=headers,
            method=self.command,
        )
        try:
            resp = urlopen(req, timeout=3600)
        except HTTPError as exc:
            resp = exc
        except URLError as exc:
            msg = json.dumps({"error": {"message": "session-sticky upstream unreachable", "detail": str(exc.reason)}}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        if getattr(resp, "status", 0) == 429:
            ra = parse_retry_after(resp.headers)
            body_preview = b""
            try:
                # HTTPError is a file-like; peek via read then we still stream below.
                # For 429 bodies are tiny — buffer then rewrite once.
                body_preview = resp.read()
            except Exception:
                body_preview = b""
            reason = "rate_limit"
            all_cooled = False
            try:
                obj = json.loads(body_preview.decode("utf-8", "replace"))
                err = obj.get("error") if isinstance(obj, dict) else None
                if isinstance(err, dict) and isinstance(err.get("message"), str):
                    reason = err["message"][:120]
                    low = reason.lower()
                    all_cooled = ("all exits cooled" in low) or ("every healthy exit" in low)
                    if ra <= 0 and all_cooled:
                        ra = float(CIRCUIT_MIN_RETRY_AFTER)
            except Exception:
                pass
            # Per-IP quota: a normal short 429 must NOT blackhole the model.
            # Stampede-break only when:
            #   - zenproxy said the whole pool is cooled, or
            #   - Retry-After looks like a daily reset (>= 1h) which only appears
            #     after zenproxy's all-cooled fail-fast (sub2api rewrites the body).
            daily_reset = ra >= 3600
            if (not CIRCUIT_REQUIRE_ALL_COOLED) or all_cooled or daily_reset:
                if ra <= 0:
                    ra = float(CIRCUIT_MIN_RETRY_AFTER)
                trip_circuit(model, ra, reason)
            self.send_response(429)
            for name, value in resp.headers.items():
                if name.lower() in HOP:
                    continue
                self.send_header(name, value)
            if "Retry-After" not in {k.lower() for k in resp.headers.keys()}:
                self.send_header("Retry-After", str(int(ra)))
            self.send_header("Content-Length", str(len(body_preview)))
            self.end_headers()
            try:
                if body_preview:
                    self.wfile.write(body_preview)
            except BrokenPipeError:
                return
            finally:
                resp.close()
            return

        self.send_response(resp.status)
        for name, value in resp.headers.items():
            if name.lower() in HOP:
                continue
            self.send_header(name, value)
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        try:
            while True:
                chunk = resp.read(16384)
                if not chunk:
                    break
                self.wfile.write(b"%X\r\n" % len(chunk))
                self.wfile.write(chunk)
                self.wfile.write(b"\r\n")
            self.wfile.write(b"0\r\n\r\n")
        except BrokenPipeError:
            return
        finally:
            resp.close()


def main() -> None:
    host, _, port = LISTEN.partition(":")
    server = ThreadingHTTPServer((host or "0.0.0.0", int(port or "8091")), Handler)
    sys.stderr.write("session-sticky listening %s -> %s\n" % (LISTEN, UPSTREAM))
    server.serve_forever()


if __name__ == "__main__":
    main()
