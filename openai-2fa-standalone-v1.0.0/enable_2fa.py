from __future__ import annotations

import argparse
import asyncio
import json
from pathlib import Path
from typing import Any

from openai_2fa import enable_authenticator


def _cookies(value: Any) -> list[dict[str, Any]]:
    if isinstance(value, list):
        return [item for item in value if isinstance(item, dict)]
    if isinstance(value, str) and value.strip():
        parsed = json.loads(value)
        if isinstance(parsed, list):
            return [item for item in parsed if isinstance(item, dict)]
    return []


async def main() -> None:
    parser = argparse.ArgumentParser(description="Enable Authenticator TOTP")
    parser.add_argument("--account", required=True, help="account JSON input")
    parser.add_argument("--output", default="mfa_result.json", help="result JSON")
    args = parser.parse_args()

    account_path = Path(args.account).expanduser().resolve()
    account = json.loads(account_path.read_text(encoding="utf-8-sig"))
    result = await enable_authenticator(
        email=str(account.get("email") or ""),
        access_token=str(account.get("access_token") or ""),
        session_token=str(
            account.get("chatgpt_session_token")
            or account.get("session_token")
            or ""
        ),
        cookies=_cookies(
            account.get("chatgpt_cookies")
            or account.get("cookies")
            or account.get("chatgpt_cookie_json")
        ),
        device_id=str(
            account.get("chatgpt_device_id") or account.get("device_id") or ""
        ),
        proxy=str(account.get("proxy") or ""),
    )
    output = Path(args.output).expanduser().resolve()
    output.write_text(
        json.dumps(result, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    print(f"enabled={result['enabled']} already_enabled={result['already_enabled']}")
    print(f"result={output}")


if __name__ == "__main__":
    asyncio.run(main())
