"""Page inspection and registration state flow for the CloakBrowser sidecar."""
from __future__ import annotations

import asyncio
import re
from dataclasses import dataclass
from datetime import date
from typing import Any, Awaitable, Callable, Mapping, Optional
from urllib.parse import urlsplit

EMAIL = "#email,input[name='email'],input[type='email'],input[autocomplete='email']"
PASSWORD = "input[type='password'],input[name='password']"
CODE = "input[name='code'],input[autocomplete='one-time-code']"
NAME = "input[name='name']"
PROFILE = "input[name='age'],input[name='birthdate'],input[name='birthday'],input[type='date']"
READY = "textarea[name='prompt-textarea'],#prompt-textarea,[data-testid='composer'],[contenteditable='true'][data-lexical-editor='true']"
ACTIONS = "button,a,[role='button']"
CHALLENGE = "iframe[src*='challenge'],iframe[src*='captcha'],iframe[src*='turnstile'],#challenge-running,[data-testid*='captcha'],[class*='captcha']"
RETRY_RE = re.compile(r"try again|retry|重试|再试一次|再試行|もう一度", re.I)
RESEND_RE = re.compile(r"resend code|send again|request another code|resend email|重新发送|重发验证码|再次发送|コードを再送|再送信", re.I)
CHALLENGE_TEXT = (
    "verify you are human", "security check", "unusual activity", "captcha",
    "automated access", "验证您是人类", "安全验证", "自动化访问", "可疑活动",
    "人間であることを確認", "セキュリティチェック", "ロボットではない", "不審なアクティビティ",
)
DISABLED_TEXT = (
    "you do not have an account", "deleted or deactivated", "account has been deactivated",
    "账号不存在", "账号已被删除", "账号已停用", "アカウントが存在しません", "削除または無効",
)
REJECTED_TEXT = (
    "invalid code", "incorrect code", "wrong code", "code expired", "验证码错误",
    "验证码无效", "验证码已过期", "验证码不正确", "コードが正しくありません",
    "コードが無効", "コードの有効期限", "認証コードが正しくありません",
)


class SidecarError(Exception):
    def __init__(self, code: str, message: str, retryable: bool = False) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.retryable = retryable


@dataclass
class Signals:
    url: str = ""
    body: str = ""
    email: bool = False
    password: bool = False
    code: bool = False
    code_invalid: bool = False
    name: bool = False
    profile_field: str = ""
    ready: bool = False
    retry: bool = False
    challenge: bool = False


def contains(value: str, needles: tuple[str, ...]) -> bool:
    lowered = value.lower()
    return any(item.lower() in lowered for item in needles)


def classify_page(signals: Signals) -> str:
    if signals.challenge or contains(signals.body, CHALLENGE_TEXT):
        return "challenge"
    if contains(signals.body, DISABLED_TEXT):
        return "disabled"
    if signals.retry:
        return "retry"
    if signals.name and signals.profile_field:
        return "profile"
    if signals.password:
        return "password"
    rejected = signals.code_invalid or contains(signals.body, REJECTED_TEXT)
    if signals.code and rejected:
        return "code_rejected"
    if signals.code:
        return "code"
    if signals.email:
        return "email"
    if signals.ready or ready_url(signals.url, signals):
        return "ready"
    return "wait"


def ready_url(raw_url: str, signals: Signals) -> bool:
    parsed = urlsplit(raw_url)
    host = (parsed.hostname or "").lower()
    path = parsed.path.lower()
    valid_host = host in ("chatgpt.com", "chat.openai.com") or host.endswith(".chatgpt.com")
    blocked = signals.email or signals.code or signals.password or signals.name
    return valid_host and not blocked and not path.startswith("/auth/") and (
        path in ("", "/") or path.startswith(("/c/", "/g/"))
    )


def birthdate_from_age(value: str, today: Optional[date] = None) -> str:
    raw = value.strip()
    current = today or date.today()
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}", raw):
        try:
            return date.fromisoformat(raw).isoformat()
        except ValueError:
            raw = ""
    try:
        years = int(raw)
    except ValueError:
        years = 30
    years = years if 18 <= years <= 100 else 30
    try:
        return current.replace(year=current.year - years).isoformat()
    except ValueError:
        return current.replace(year=current.year - years, day=28).isoformat()


async def visible(page: Any, selector: str) -> bool:
    locator = page.locator(selector)
    for index in range(await locator.count()):
        if await locator.nth(index).is_visible():
            return True
    return False


async def visible_invalid(locator: Any) -> bool:
    for index in range(await locator.count()):
        item = locator.nth(index)
        if await item.is_visible() and await item.get_attribute("aria-invalid") == "true":
            return True
    return False


async def profile_name(page: Any) -> str:
    locator = page.locator(PROFILE)
    for index in range(await locator.count()):
        item = locator.nth(index)
        if not await item.is_visible():
            continue
        name = await item.get_attribute("name")
        field_type = await item.get_attribute("type")
        return name or ("birthdate" if field_type == "date" else "age")
    return ""


async def inspect_page(page: Any) -> Signals:
    body_locator = page.locator("body")
    body = ""
    if await body_locator.count():
        body = (await body_locator.inner_text(timeout=5000))[:2500]
    actions = "\n".join(await page.locator(ACTIONS).all_text_contents())
    code_locator = page.locator(CODE)
    code = await visible(page, CODE)
    return Signals(
        url=page.url,
        body=body,
        email=await visible(page, EMAIL),
        password=await visible(page, PASSWORD),
        code=code,
        code_invalid=code and await visible_invalid(code_locator),
        name=await visible(page, NAME),
        profile_field=await profile_name(page),
        ready=await visible(page, READY),
        retry=bool(RETRY_RE.search(actions)),
        challenge=await visible(page, CHALLENGE),
    )


async def inspect_context(context: Any) -> tuple[Any, Signals, str]:
    rank = {
        "challenge": 10, "disabled": 9, "retry": 8, "profile": 7, "password": 6,
        "code_rejected": 5, "code": 4, "email": 3, "ready": 2, "wait": 1,
    }
    best: Optional[tuple[Any, Signals, str]] = None
    for page in reversed(context.pages):
        if page.is_closed():
            continue
        signals = await inspect_page(page)
        state = classify_page(signals)
        if best is None or rank[state] > rank[best[2]]:
            best = (page, signals, state)
    if best is not None:
        return best
    page = await context.new_page()
    return page, Signals(url=page.url), "wait"


async def actionable(page: Any, selector: str, editable: bool = False, timeout: float = 20.0) -> Any:
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while loop.time() < deadline:
        candidates = page.locator(selector)
        for index in range(await candidates.count()):
            item = candidates.nth(index)
            okay = await item.is_visible() and await item.is_enabled()
            okay = okay and (not editable or await item.is_editable())
            if not okay:
                continue
            bounds = await item.bounding_box()
            await asyncio.sleep(0.15)
            if bounds and bounds == await item.bounding_box():
                return item
        await asyncio.sleep(0.2)
    raise SidecarError("element_timeout", "page action element did not become stable", True)


async def type_value(page: Any, selector: str, value: str) -> None:
    item = await actionable(page, selector, editable=True)
    await item.click()
    await item.press("Control+A")
    await item.type(value)


async def click_submit(page: Any) -> None:
    await (await actionable(page, "button[type='submit']")).click()


async def click_action(page: Any, pattern: re.Pattern[str], required: bool) -> bool:
    actions = page.locator(ACTIONS)
    for index in range(await actions.count()):
        item = actions.nth(index)
        text = (await item.inner_text()).strip()
        if await item.is_visible() and pattern.search(text) and await item.is_enabled():
            await item.click()
            return True
    if required:
        raise SidecarError("element_timeout", "page action disappeared", True)
    return False


@dataclass
class SubmissionGate:
    submitted_at: Optional[float] = None

    def pending(self) -> bool:
        return self.submitted_at is None

    def mark(self, now: float) -> None:
        self.submitted_at = now

    def ensure_progress(self, now: float, code: str, message: str) -> None:
        if self.submitted_at is not None and now - self.submitted_at >= 30:
            raise SidecarError(code, message, True)


class VerificationCodes:
    def __init__(self, request_code: Callable[[int], Awaitable[str]]) -> None:
        self.request_code = request_code
        self.attempts = 0
        self.phase = "idle"
        self.submitted_at = 0.0
        self.submitted_key = ""
        self.rejection_key = ""
        self.refresh_at = 0.0

    async def step(self, page: Any, signals: Signals, state: str, now: float) -> None:
        key = page_key(signals)
        if self.phase == "idle" and state == "code":
            await self._submit(page, key, now)
            return
        if self.phase == "submitted":
            await self._submitted_step(page, state, key, now)
            return
        if self.phase == "await_refresh":
            await self._refresh_step(page, state, key, now)

    async def _submitted_step(self, page: Any, state: str, key: str, now: float) -> None:
        if state == "code":
            self.saw_clean_page = True
        elif state == "code_rejected" and key != self.submitted_key:
            await self._reject(page, key, now)
            return
        if now - self.submitted_at >= 40:
            raise SidecarError("code_stalled", "verification page did not advance", True)

    async def _refresh_step(self, page: Any, state: str, key: str, now: float) -> None:
        changed = state == "code" or key != self.rejection_key
        if changed and now > self.refresh_at:
            await self._submit(page, key, now)
            return
        if now - self.refresh_at >= 30:
            raise SidecarError("code_refresh_stalled", "verification page did not refresh", True)

    async def _reject(self, page: Any, key: str, now: float) -> None:
        if self.attempts >= 3:
            raise SidecarError("code_invalid", "verification code was rejected three times", True)
        await click_action(page, RESEND_RE, False)
        self.phase = "await_refresh"
        self.rejection_key = key
        self.refresh_at = now

    async def _submit(self, page: Any, key: str, now: float) -> None:
        if self.attempts >= 3:
            raise SidecarError("code_invalid", "verification code attempts are exhausted", True)
        self.attempts += 1
        code = await self.request_code(self.attempts)
        await type_value(page, CODE, code)
        await click_submit(page)
        self.phase = "submitted"
        self.submitted_at = now
        self.submitted_key = key


def page_key(signals: Signals) -> str:
    body = re.sub(r"\s+", " ", signals.body).strip()
    return f"{signals.url}\n{body}\n{signals.code_invalid}"


class RegistrationFlow:
    def __init__(
        self,
        payload: Mapping[str, Any],
        request_code: Callable[[int], Awaitable[str]],
        log: Callable[[str], Awaitable[None]],
    ) -> None:
        self.payload = payload
        self.log = log
        self.codes = VerificationCodes(request_code)
        self.email = SubmissionGate()
        self.password = SubmissionGate()
        self.profile = SubmissionGate()
        self.retry = SubmissionGate()

    async def run(self, context: Any) -> None:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + 240
        while loop.time() < deadline:
            page, signals, state = await inspect_context(context)
            if await self._step(page, signals, state, loop.time()):
                return
            await asyncio.sleep(1)
        raise SidecarError("registration_timeout", "registration did not reach ready state", True)

    async def _step(self, page: Any, signals: Signals, state: str, now: float) -> bool:
        if state == "challenge":
            raise SidecarError("challenge_required", "interactive challenge or automation warning detected")
        if state == "disabled":
            raise SidecarError("account_taken", "account is deleted, deactivated, or already used")
        if state == "retry":
            await self._handle_retry(page, now)
            return False
        if state == "email":
            await self._handle_email(page, now)
            return False
        if state == "password":
            await self._handle_password(page, now)
            return False
        if state in ("code", "code_rejected"):
            await self.codes.step(page, signals, state, now)
            return False
        if state == "profile":
            await self._handle_profile(page, now)
            return False
        if state != "ready":
            return False
        await self.log("registration ready; reading session")
        return True

    async def _handle_retry(self, page: Any, now: float) -> None:
        if self.retry.pending():
            await click_action(page, RETRY_RE, True)
            self.retry.mark(now)
            return
        self.retry.ensure_progress(now, "temporary_error", "temporary page error remained after one retry")

    async def _handle_email(self, page: Any, now: float) -> None:
        if self.email.pending():
            await type_value(page, EMAIL, str(self.payload["email"]))
            await click_submit(page)
            self.email.mark(now)
            return
        self.email.ensure_progress(now, "email_stalled", "email page did not advance")

    async def _handle_password(self, page: Any, now: float) -> None:
        if self.password.pending():
            await type_value(page, PASSWORD, str(self.payload["password"]))
            await click_submit(page)
            self.password.mark(now)
            return
        self.password.ensure_progress(now, "password_stalled", "password page did not advance")

    async def _handle_profile(self, page: Any, now: float) -> None:
        if not self.profile.pending():
            self.profile.ensure_progress(now, "profile_stalled", "profile page did not advance")
            return
        await type_value(page, NAME, str(self.payload["full_name"]))
        field = await actionable(page, PROFILE, editable=True)
        await fill_profile_field(field, str(self.payload["age"]))
        await click_submit(page)
        self.profile.mark(now)


async def fill_profile_field(field: Any, age_value: str) -> None:
    name = ((await field.get_attribute("name")) or "").lower()
    field_type = await field.get_attribute("type")
    if name in ("birthdate", "birthday") or field_type == "date":
        await field.fill(birthdate_from_age(age_value))
        await field.dispatch_event("input")
        await field.dispatch_event("change")
        await field.blur()
        return
    age = age_value if age_value.isdigit() else "30"
    await field.click()
    await field.press("Control+A")
    await field.type(age)
    await field.blur()
