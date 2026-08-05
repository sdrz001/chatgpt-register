#!/usr/bin/env python3
"""Async CloakBrowser sidecar speaking the codexreg JSONL v1 protocol."""
from __future__ import annotations

import argparse
import asyncio
import base64
import importlib
import json
import math
import re
import shutil
import sys
import tempfile
import threading
from pathlib import Path
from typing import Any, Mapping, Optional, Sequence, TextIO
from urllib.parse import quote, urlsplit

try:
    from cloakbrowser_flow import (
        RegistrationFlow,
        SidecarError,
        Signals,
        birthdate_from_age,
        classify_page,
        inspect_context,
        ready_url,
    )
except ModuleNotFoundError as exc:
    if exc.name != "cloakbrowser_flow":
        raise
    from internal.codexreg.cloakbrowser_flow import (
        RegistrationFlow,
        SidecarError,
        Signals,
        birthdate_from_age,
        classify_page,
        inspect_context,
        ready_url,
    )

VERSION = 1
SCREENSHOT_LIMIT = 1 << 20
LOGIN_URL = "https://chatgpt.com/auth/login"
SESSION_URL = "https://chatgpt.com/api/auth/session"
EMAIL_RE = re.compile(r"(?i)[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9.-]+\.[a-z]{2,}")
PROXY_RE = re.compile(r"(?i)(https?|socks5)://[^/@\s]+@")
JWT_RE = re.compile(r"\b[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*\b")
TOKEN_RE = re.compile(r"(?i)((?:access[_-]?token|bearer)\s*[:=]?\s*)[^\s,;]+")
CODE_RE = re.compile(r"\b[0-9]{4,8}\b")
PROXY_SCHEMES = {"http", "https", "socks5"}


class JsonWriter:
    def __init__(self, stream: TextIO) -> None:
        self.stream = stream
        self.lock = asyncio.Lock()

    async def send(self, value: Mapping[str, Any]) -> None:
        line = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
        async with self.lock:
            self.stream.write(line + "\n")
            self.stream.flush()


def message(kind: str, request_id: str, **fields: Any) -> dict[str, Any]:
    return {"version": VERSION, "type": kind, "request_id": request_id, **fields}


def screenshot_message(request_id: str, png: bytes) -> dict[str, Any]:
    if len(png) > SCREENSHOT_LIMIT:
        raise ValueError("screenshot exceeds 1MiB")
    if not png.startswith(b"\x89PNG\r\n\x1a\n"):
        raise ValueError("screenshot is not PNG")
    encoded = base64.b64encode(png).decode("ascii")
    return message("screenshot", request_id, data=encoded)


def redact_sensitive(value: str, secrets: Sequence[str] = ()) -> str:
    text = str(value)
    for secret in secrets:
        if secret and secret.strip():
            text = text.replace(secret.strip(), "[redacted]")
    text = EMAIL_RE.sub("[email]", text)
    text = PROXY_RE.sub(r"\1://[redacted]@", text)
    text = JWT_RE.sub("[token]", text)
    text = TOKEN_RE.sub(r"\1[redacted]", text)
    return CODE_RE.sub("[code]", text)


def parse_message(line: bytes) -> dict[str, Any]:
    try:
        raw = json.loads(line)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SidecarError("protocol_error", "invalid JSONL message") from exc
    if not isinstance(raw, dict):
        raise SidecarError("protocol_error", "message must be an object")
    if raw.get("version") != VERSION or isinstance(raw.get("version"), bool):
        raise SidecarError("protocol_error", "protocol version must be 1")
    if not isinstance(raw.get("type"), str) or not raw["type"]:
        raise SidecarError("protocol_error", "message type is required")
    request_id = raw.get("request_id")
    if not isinstance(request_id, str) or not request_id.strip():
        raise SidecarError("protocol_error", "request_id is required")
    return raw


def validate_start(raw: Mapping[str, Any]) -> dict[str, Any]:
    if raw.get("type") != "start":
        raise SidecarError("protocol_error", "first message must be start")
    payload = raw.get("payload")
    if not isinstance(payload, dict):
        raise SidecarError("protocol_error", "start payload must be an object")
    result = {key: required_text(payload, key) for key in ("email", "password", "full_name", "age")}
    proxy = payload.get("proxy", "")
    if proxy is None:
        proxy = ""
    if not isinstance(proxy, str) or not isinstance(payload.get("headless"), bool):
        raise SidecarError("invalid_payload", "payload proxy/headless has invalid type")
    result.update(proxy=normalize_proxy(proxy), headless=payload["headless"])
    return result


def required_text(payload: Mapping[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or not value.strip():
        raise SidecarError("invalid_payload", f"payload.{key} is required")
    return value.strip()


def normalize_proxy(value: str) -> str:
    raw = value.strip()
    if not raw:
        return ""
    if "://" in raw:
        if any(character.isspace() for character in raw):
            raise invalid_proxy()
        validate_proxy_url(raw)
        return raw
    parts = raw.split(":")
    if len(parts) == 2:
        validate_proxy_host_port(parts[0], parts[1])
        return "http://" + raw
    if len(parts) == 4 and all(parts):
        validate_proxy_host_port(parts[0], parts[1])
        username = quote(parts[2], safe="")
        password = quote(parts[3], safe="")
        return f"http://{username}:{password}@{parts[0]}:{parts[1]}"
    raise invalid_proxy()


def validate_proxy_url(raw: str) -> None:
    try:
        parsed = urlsplit(raw)
        port = parsed.port
    except ValueError as exc:
        raise invalid_proxy() from exc
    if parsed.scheme.lower() not in PROXY_SCHEMES or not parsed.hostname or port is None:
        raise invalid_proxy()
    if parsed.path not in ("", "/") or parsed.query or parsed.fragment:
        raise invalid_proxy()
    has_username = parsed.username is not None
    has_password = parsed.password is not None
    if has_username != has_password or (has_username and (not parsed.username or not parsed.password)):
        raise invalid_proxy()


def validate_proxy_host_port(host: str, port: str) -> None:
    if not host or any(character.isspace() for character in host + port):
        raise invalid_proxy()
    if any(item in host for item in ("/", "@", "[", "]")):
        raise invalid_proxy()
    if not port.isdigit() or not 1 <= int(port) <= 65535:
        raise invalid_proxy()


def invalid_proxy() -> SidecarError:
    return SidecarError("invalid_payload", "proxy format is invalid")
class Registration:
    def __init__(self, writer: JsonWriter, request_id: str, payload: Mapping[str, Any]) -> None:
        self.writer = writer
        self.request_id = request_id
        self.payload = payload
        self.code_waiter: Optional[asyncio.Future[str]] = None
        self.context: Any = None
        self.secrets = [str(payload[key]) for key in ("email", "password", "proxy") if payload.get(key)]

    async def log(self, text: str) -> None:
        clean = redact_sensitive(text, self.secrets)
        await self.writer.send(message("log", self.request_id, level="info", message=clean))

    async def request_code(self, attempt: int) -> str:
        if self.code_waiter is not None:
            raise SidecarError("protocol_error", "verification response is already pending")
        if not 1 <= attempt <= 3:
            raise SidecarError("code_invalid", "verification code attempts are exhausted", True)
        self.code_waiter = asyncio.get_running_loop().create_future()
        await self.writer.send(message("code_request", self.request_id, attempt=attempt))
        try:
            code = await self.code_waiter
        finally:
            self.code_waiter = None
        self.secrets.append(code)
        return code

    def accept_code(self, code: Any) -> None:
        pending = self.code_waiter is not None and not self.code_waiter.done()
        if not pending or not isinstance(code, str) or not code.strip():
            raise SidecarError("protocol_error", "unexpected or empty code_response")
        self.code_waiter.set_result(code.strip())

    async def screenshot(self) -> None:
        page, _, _ = await inspect_context(self.context)
        masks = [page.locator("input,textarea,[contenteditable='true']")]
        png = await page.screenshot(type="png", mask=masks, animations="disabled")
        for _ in range(4):
            if len(png) <= SCREENSHOT_LIMIT:
                await self.writer.send(screenshot_message(self.request_id, png))
                return
            size = await page.evaluate("() => ({width: innerWidth, height: innerHeight})")
            factor = max(0.2, math.sqrt(SCREENSHOT_LIMIT / len(png)) * 0.8)
            clip = scaled_clip(size, factor)
            png = await page.screenshot(type="png", clip=clip, mask=masks, animations="disabled")

    async def registration_loop(self, context: Any) -> None:
        flow = RegistrationFlow(self.payload, self.request_code, self.log)
        await flow.run(context)


def scaled_clip(size: Mapping[str, Any], factor: float) -> dict[str, int]:
    return {
        "x": 0,
        "y": 0,
        "width": max(160, int(size["width"] * factor)),
        "height": max(120, int(size["height"] * factor)),
    }


async def launch_context(registration: Registration, profile: Path) -> Any:
    options: dict[str, Any] = {
        "headless": registration.payload["headless"],
        "humanize": True,
        "human_preset": "careful",
    }
    if registration.payload["proxy"]:
        options.update(proxy=registration.payload["proxy"], geoip=True)
    launch_task = asyncio.create_task(load_launcher()(str(profile), **options))
    try:
        registration.context = await asyncio.shield(launch_task)
    except asyncio.CancelledError:
        await settle_cancelled_launch(registration, launch_task)
        raise
    return registration.context


async def settle_cancelled_launch(registration: Registration, task: asyncio.Task[Any]) -> None:
    done, _ = await asyncio.wait((task,), timeout=0.5)
    if done:
        capture_launch_result(registration, task)
        return
    task.add_done_callback(lambda finished: close_late_context(registration, finished))
    task.cancel()


def capture_launch_result(registration: Registration, task: asyncio.Task[Any]) -> None:
    try:
        registration.context = task.result()
    except BaseException as exc:
        if not isinstance(exc, asyncio.CancelledError):
            diagnostic("cancelled browser launch failed", exc, registration.secrets)


def close_late_context(registration: Registration, task: asyncio.Task[Any]) -> None:
    capture_launch_result(registration, task)
    if registration.context is None:
        return
    try:
        asyncio.get_running_loop().create_task(close_context(registration))
    except RuntimeError:
        pass


async def browse(registration: Registration) -> str:
    root = Path(__file__).resolve().parents[2] / ".cloakbrowser-profiles"
    root.mkdir(exist_ok=True)
    profile = Path(tempfile.mkdtemp(prefix=f"task-{registration.request_id[:12]}-", dir=root))
    try:
        context = await launch_context(registration, profile)
        page = context.pages[-1] if context.pages else await context.new_page()
        await page.goto(LOGIN_URL, wait_until="domcontentloaded", timeout=120000)
        await registration.log("registration page loaded")
        await registration.registration_loop(context)
        page, _, _ = await inspect_context(context)
        token = await read_access_token(page)
        registration.secrets.append(token)
        return token
    except SidecarError:
        if registration.context is not None:
            await emit_screenshot(registration)
        raise
    finally:
        await close_context(registration)
        shutil.rmtree(profile, ignore_errors=True)


async def close_context(registration: Registration) -> None:
    context = registration.context
    registration.context = None
    if context is None:
        return
    close_task = asyncio.create_task(context.close())
    done, _ = await asyncio.wait((close_task,), timeout=1.0)
    if not done:
        close_task.cancel()
        diagnostic("context close timed out", TimeoutError(), registration.secrets)
        return
    try:
        close_task.result()
    except BaseException as exc:
        if not isinstance(exc, asyncio.CancelledError):
            diagnostic("context close failed", exc, registration.secrets)


async def read_access_token(page: Any) -> str:
    response = await page.goto(SESSION_URL, wait_until="domcontentloaded", timeout=60000)
    if response is None or not response.ok:
        raise SidecarError("session_invalid", "session endpoint returned an error", True)
    body = await page.locator("body").inner_text(timeout=15000)
    try:
        session = json.loads(body)
    except json.JSONDecodeError as exc:
        raise SidecarError("session_invalid", "session endpoint did not return JSON", True) from exc
    if not isinstance(session, dict):
        raise SidecarError("session_invalid", "session endpoint did not return an object", True)
    token = session.get("accessToken")
    if not isinstance(token, str) or not token.strip():
        raise SidecarError("access_token_missing", "session response is missing accessToken", True)
    return token.strip()


async def emit_screenshot(registration: Registration) -> None:
    try:
        await registration.screenshot()
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        diagnostic("screenshot failed", exc, registration.secrets)


def load_launcher() -> Any:
    module = importlib.import_module("cloakbrowser")
    launcher = getattr(module, "launch_persistent_context_async", None)
    if launcher is None:
        raise SidecarError("cloakbrowser_import", "cloakbrowser persistent async launcher is missing")
    return launcher


def diagnostic(prefix: str, exc: BaseException, secrets: Sequence[str] = ()) -> None:
    sys.stderr.write(f"{prefix}: {redact_sensitive(str(exc), secrets)}\n")
    sys.stderr.flush()


async def read_line() -> bytes:
    loop = asyncio.get_running_loop()
    future: asyncio.Future[bytes] = loop.create_future()

    def read_stdin() -> None:
        try:
            line = sys.stdin.buffer.readline()
        except (OSError, ValueError) as exc:
            loop.call_soon_threadsafe(future.set_exception, exc)
        else:
            loop.call_soon_threadsafe(future.set_result, line)

    threading.Thread(target=read_stdin, daemon=True).start()
    return await future


async def command_loop(registration: Registration, worker: asyncio.Task[str]) -> None:
    while True:
        line = await read_line()
        if not line:
            raise SidecarError("input_closed", "stdin closed before completion")
        raw = parse_message(line)
        if raw["request_id"] != registration.request_id:
            raise SidecarError("protocol_error", "request_id mismatch")
        if raw["type"] == "code_response":
            registration.accept_code(raw.get("code"))
        elif raw["type"] == "cancel":
            worker.cancel()
            return
        else:
            raise SidecarError("protocol_error", "unexpected input message type")


async def run_registration(registration: Registration) -> str:
    worker = asyncio.create_task(browse(registration))
    commands = asyncio.create_task(command_loop(registration, worker))
    try:
        done, _ = await asyncio.wait((worker, commands), return_when=asyncio.FIRST_COMPLETED)
        if commands in done:
            await handle_command_completion(commands, worker)
        try:
            return await worker
        except asyncio.CancelledError as exc:
            raise SidecarError("cancelled", "request cancelled") from exc
    finally:
        commands.cancel()
        await asyncio.gather(commands, return_exceptions=True)


async def handle_command_completion(commands: asyncio.Task[None], worker: asyncio.Task[str]) -> None:
    error = commands.exception()
    if error is None and not worker.done():
        await asyncio.gather(worker, return_exceptions=True)
        raise SidecarError("cancelled", "request cancelled")
    if error is not None:
        worker.cancel()
        await asyncio.gather(worker, return_exceptions=True)
        raise error


async def protocol_main(writer: JsonWriter) -> int:
    request_id = "protocol"
    registration: Optional[Registration] = None
    try:
        raw = await read_start()
        request_id = raw["request_id"]
        registration = Registration(writer, request_id, validate_start(raw))
        await writer.send(message("ready", request_id))
        token = await run_registration(registration)
        await writer.send(message("result", request_id, access_token=token))
        return 0
    except asyncio.CancelledError:
        await send_error(writer, request_id, "cancelled", "request cancelled", False)
        return 2
    except SidecarError as exc:
        secrets = registration.secrets if registration else ()
        await send_error(writer, request_id, exc.code, redact_sensitive(exc.message, secrets), exc.retryable)
        return 2
    except Exception as exc:
        secrets = registration.secrets if registration else ()
        diagnostic("sidecar failure", exc, secrets)
        await send_error(writer, request_id, "browser_error", "browser operation failed", True)
        return 2


async def read_start() -> dict[str, Any]:
    line = await read_line()
    if not line:
        raise SidecarError("protocol_error", "missing start message")
    return parse_message(line)


async def send_error(
    writer: JsonWriter,
    request_id: str,
    code: str,
    detail: str,
    retryable: bool,
) -> None:
    await writer.send(message("error", request_id, code=code, message=detail, retryable=retryable))


def health(stream: TextIO) -> int:
    ok = sys.version_info >= (3, 10)
    code = "ok"
    detail = "python and cloakbrowser are ready"
    if not ok:
        code = "python_version"
        detail = "Python 3.10 or newer is required"
    else:
        try:
            load_launcher()
        except (ImportError, AttributeError, SidecarError):
            ok = False
            code = "cloakbrowser_import"
            detail = "cloakbrowser import failed"
    line = json.dumps(
        message("health", "health", ok=ok, code=code, message=detail),
        ensure_ascii=False,
        separators=(",", ":"),
    )
    stream.write(line + "\n")
    stream.flush()
    return 0 if ok else 1


def main() -> int:
    parser = argparse.ArgumentParser(description="CloakBrowser codexreg JSONL sidecar")
    parser.add_argument("--health", action="store_true")
    args = parser.parse_args()
    protocol_stdout = sys.stdout
    sys.stdout = sys.stderr
    if args.health:
        return health(protocol_stdout)
    return asyncio.run(protocol_main(JsonWriter(protocol_stdout)))
if __name__ == "__main__":
    raise SystemExit(main())
