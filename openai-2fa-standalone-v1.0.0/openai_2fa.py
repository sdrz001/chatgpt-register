"""Standalone Authenticator-TOTP enrollment client.

Inputs are a matching ChatGPT Web access token and NextAuth browser session.
The module has no dependency on the original Flask project or its database.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import re
import secrets
import struct
import time
import uuid
from typing import Any, Callable, Iterable
from urllib.parse import quote

import httpx


CHATGPT_BASE = "https://chatgpt.com"
MFA_INFO_PATH = "/backend-api/accounts/mfa_info"
MFA_ENROLL_PATH = "/backend-api/accounts/mfa/enroll"
MFA_ACTIVATE_PATH = "/backend-api/accounts/mfa/user/activate_enrollment"


def normalize_totp_secret(secret: str) -> str:
    value = re.sub(r"[^A-Z2-7]", "", str(secret or "").upper())
    if not value:
        raise ValueError("empty TOTP secret")
    return value


def totp_code(
    secret: str,
    *,
    at_time: float | int | None = None,
    digits: int = 6,
    period: int = 30,
) -> str:
    """Generate an RFC 6238 SHA-1 code without a third-party OTP package."""

    normalized = normalize_totp_secret(secret)
    padded = normalized + "=" * ((8 - len(normalized) % 8) % 8)
    key = base64.b32decode(padded, casefold=True)
    counter = int(time.time() if at_time is None else at_time) // int(period)
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    number = struct.unpack(">I", digest[offset : offset + 4])[0] & 0x7FFFFFFF
    return str(number % (10**digits)).zfill(digits)


def _trace_headers() -> dict[str, str]:
    trace_id = secrets.token_hex(16)
    parent_id = secrets.token_hex(8)
    return {
        "traceparent": f"00-{trace_id}-{parent_id}-01",
        "tracestate": "dd=s:1;o:rum",
        "x-datadog-origin": "rum",
        "x-datadog-parent-id": str(int(parent_id, 16)),
        "x-datadog-sampling-priority": "1",
        "x-datadog-trace-id": str(int(trace_id[-16:], 16)),
    }


def _headers(access_token: str, device_id: str, *, json_body: bool) -> dict[str, str]:
    headers = {
        "accept": "application/json",
        "accept-language": "en-US,en;q=0.9",
        "authorization": f"Bearer {access_token}",
        "oai-device-id": device_id,
        "origin": CHATGPT_BASE,
        "referer": CHATGPT_BASE + "/#settings/Security",
        "sec-ch-ua": '"Chromium";v="146", "Not_A Brand";v="99"',
        "sec-ch-ua-mobile": "?0",
        "sec-ch-ua-platform": '"Windows"',
        "sec-fetch-dest": "empty",
        "sec-fetch-mode": "cors",
        "sec-fetch-site": "same-origin",
        "user-agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/146.0.0.0 Safari/537.36"
        ),
    }
    if json_body:
        headers["content-type"] = "application/json"
    headers.update(_trace_headers())
    return headers


def restore_chatgpt_session(
    client: httpx.AsyncClient,
    *,
    session_token: str = "",
    cookies: Iterable[dict[str, Any]] | None = None,
) -> None:
    names: set[str] = set()
    for item in cookies or ():
        if not isinstance(item, dict):
            continue
        name = str(item.get("name") or "").strip()
        value = str(item.get("value") or "")
        if not name or not value:
            continue
        names.add(name)
        client.cookies.set(
            name,
            value,
            domain=str(item.get("domain") or ".chatgpt.com"),
            path=str(item.get("path") or "/"),
        )
    token = str(session_token or "").strip()
    if token and "__Secure-next-auth.session-token" not in names:
        client.cookies.set(
            "__Secure-next-auth.session-token",
            token,
            domain=".chatgpt.com",
            path="/",
        )


async def _request(
    client: httpx.AsyncClient,
    method: str,
    path: str,
    *,
    headers: dict[str, str],
    json_body: dict[str, Any] | None = None,
) -> dict[str, Any]:
    last_error: Exception | None = None
    for attempt in range(1, 4):
        try:
            response = await client.request(
                method,
                CHATGPT_BASE + path,
                headers=headers,
                json=json_body,
            )
            if response.status_code >= 500 and attempt < 3:
                await _sleep(0.4 * attempt)
                continue
            try:
                data = response.json()
            except Exception:
                data = {}
            if 200 <= response.status_code < 300 and isinstance(data, dict):
                return data
            detail = " ".join(response.text.split())[:500] or repr(data)[:500]
            raise RuntimeError(
                f"2FA {path} failed: HTTP {response.status_code}: {detail}"
            )
        except (httpx.TimeoutException, httpx.TransportError) as exc:
            last_error = exc
            if attempt >= 3:
                raise
            await _sleep(0.4 * attempt)
    raise RuntimeError(f"2FA request failed: {last_error}")


async def _sleep(seconds: float) -> None:
    import asyncio

    await asyncio.sleep(seconds)


def _recovery_codes(*values: Any) -> list[str]:
    found: list[str] = []

    def visit(value: Any, key: str = "") -> None:
        if isinstance(value, dict):
            for child_key, child in value.items():
                visit(child, str(child_key).lower())
        elif isinstance(value, (list, tuple)):
            for child in value:
                visit(child, key)
        elif "recovery" in key or "backup" in key:
            text = str(value or "").strip()
            if text and text not in found:
                found.append(text)

    for item in values:
        visit(item)
    return found


async def get_mfa_info(
    *,
    access_token: str,
    session_token: str = "",
    cookies: Iterable[dict[str, Any]] | None = None,
    device_id: str = "",
    proxy: str = "",
    client: httpx.AsyncClient | None = None,
) -> dict[str, Any]:
    token = str(access_token or "").strip()
    if not token:
        raise ValueError("missing ChatGPT Web access_token")
    did = str(device_id or "").strip() or str(uuid.uuid4())
    owned = client is None
    if client is None:
        client = httpx.AsyncClient(
            proxy=str(proxy or "").strip() or None,
            timeout=httpx.Timeout(30.0),
            follow_redirects=True,
        )
    try:
        restore_chatgpt_session(client, session_token=session_token, cookies=cookies)
        return await _request(
            client,
            "GET",
            MFA_INFO_PATH,
            headers=_headers(token, did, json_body=False),
        )
    finally:
        if owned:
            await client.aclose()


async def enable_authenticator(
    *,
    email: str,
    access_token: str,
    session_token: str = "",
    cookies: Iterable[dict[str, Any]] | None = None,
    device_id: str = "",
    proxy: str = "",
    client: httpx.AsyncClient | None = None,
    code_factory: Callable[[str], str] = totp_code,
) -> dict[str, Any]:
    """Enroll TOTP, activate it, then verify mfa_info in one session."""

    token = str(access_token or "").strip()
    if not token:
        raise ValueError("missing ChatGPT Web access_token")
    did = str(device_id or "").strip() or str(uuid.uuid4())
    owned = client is None
    if client is None:
        client = httpx.AsyncClient(
            proxy=str(proxy or "").strip() or None,
            timeout=httpx.Timeout(30.0),
            follow_redirects=True,
        )
    try:
        restore_chatgpt_session(client, session_token=session_token, cookies=cookies)
        before = await _request(
            client,
            "GET",
            MFA_INFO_PATH,
            headers=_headers(token, did, json_body=False),
        )
        if bool(before.get("mfa_enabled") or before.get("mfa_enabled_v2")):
            return {
                "enabled": True,
                "already_enabled": True,
                "secret": "",
                "provisioning_uri": "",
                "recovery_codes": [],
                "factor_id": str(before.get("native_default_factor_id") or ""),
                "mfa_info": before,
            }

        enroll = await _request(
            client,
            "POST",
            MFA_ENROLL_PATH,
            headers=_headers(token, did, json_body=True),
            json_body={"factor_type": "totp"},
        )
        secret = normalize_totp_secret(str(enroll.get("secret") or ""))
        enrollment_session_id = str(enroll.get("session_id") or "").strip()
        factor = enroll.get("factor") if isinstance(enroll.get("factor"), dict) else {}
        factor_id = str(factor.get("id") or "").strip()
        if not enrollment_session_id:
            raise RuntimeError("2FA enroll returned no session_id")

        activated = await _request(
            client,
            "POST",
            MFA_ACTIVATE_PATH,
            headers=_headers(token, did, json_body=True),
            json_body={
                "code": str(code_factory(secret) or "").strip(),
                "factor_type": "totp",
                "session_id": enrollment_session_id,
            },
        )
        if activated.get("success") is False:
            raise RuntimeError(f"2FA activation rejected: {activated!r}")

        after = await _request(
            client,
            "GET",
            MFA_INFO_PATH,
            headers=_headers(token, did, json_body=False),
        )
        if not bool(after.get("mfa_enabled") or after.get("mfa_enabled_v2")):
            raise RuntimeError("2FA activation was not confirmed by mfa_info")

        label = quote(str(email or "account").strip(), safe="")
        return {
            "enabled": True,
            "already_enabled": False,
            "secret": secret,
            "provisioning_uri": (
                f"otpauth://totp/OpenAI:{label}?secret={secret}&issuer=OpenAI"
            ),
            "recovery_codes": _recovery_codes(enroll, activated, after),
            "factor_id": str(after.get("native_default_factor_id") or factor_id),
            "mfa_info": after,
        }
    finally:
        if owned:
            await client.aclose()
