"""Page inspection and registration state flow for the CloakBrowser sidecar."""
from __future__ import annotations

import asyncio
import json
import re
from dataclasses import dataclass
from datetime import date
from typing import Any, Awaitable, Callable, Mapping, Optional
from urllib.parse import urlsplit

EMAIL = "#email,input[name='email'],input[type='email'],input[autocomplete='email']"
PASSWORD = "input[type='password'],input[name='password'],input[autocomplete='new-password'],input[autocomplete='current-password']"
CODE = "input[name='code'],input[autocomplete='one-time-code'],input[inputmode='numeric'][maxlength='6']"
NAME = "input[name='name'],input[name='fullName'],input[name='full_name'],input[id='name'],input[id='fullName'],input[id='full-name'],input[autocomplete='name'],input[placeholder='Full name'],input[placeholder='Name'],input[placeholder='全名'],input[placeholder='姓名'],input[aria-label='Full name'],input[aria-label='Name'],input[aria-label='全名'],input[aria-label='姓名']"
PROFILE = "input[name='age'],input[name='birthdate'],input[name='birthday'],input[name='date_of_birth'],input[name='dob'],input[id='age'],input[id*='birth'],input[autocomplete='bday'],input[type='date'],input[placeholder='Age'],input[placeholder='年龄'],input[placeholder='生日'],input[aria-label='Age'],input[aria-label='年龄'],input[aria-label='生日']"
READY = "textarea[name='prompt-textarea'],#prompt-textarea,[data-testid='composer'],[data-testid='composer-input'],[contenteditable='true'][data-lexical-editor='true']"
SUBMIT = "button[type='submit'],input[type='submit'],form button:not([type])"
ACTIONS = "button,a,[role='button']"
CHALLENGE = "iframe[src*='challenge'],iframe[src*='captcha'],iframe[src*='turnstile'],#challenge-running,[data-testid*='captcha'],[class*='captcha']"
RETRY_RE = re.compile(r"try again|retry|重试|再试一次|再試行|もう一度", re.I)
RESEND_RE = re.compile(r"resend code|send again|request another code|resend email|重新发送|重发验证码|再次发送|コードを再送|再送信", re.I)
EXTERNAL_LOGIN_RE = re.compile(r"google|apple|microsoft|phone number|电话号码|電話番号|手机号|携帯電話", re.I)
SUBMIT_ACTION_RE = re.compile(r"^\s*(continue|next|verify|submit|sign in|log in|create account|complete account creation|继续|下一步|验证|提交|登录|创建账户|创建帐号|完成账户创建|完成帐号创建|继续する|次へ|確認|送信|ログイン|アカウント作成|계속|다음|확인|제출|로그인)\s*$", re.I)
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
FAST_POLL_INTERVAL = 0.25
WAIT_POLL_INTERVAL = 0.5
CHALLENGE_POLL_INTERVAL = 1.0
EMAIL_RETRY_INTERVAL = 15.0
INSPECT_SCRIPT = r"""() => {
    const visible = element => {
        if (!element) return false;
        const style = getComputedStyle(element);
        const rect = element.getBoundingClientRect();
        return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
    };
    const first = selector => Array.from(document.querySelectorAll(selector)).find(visible) || null;
    const email = first("#email,input[name='email'],input[type='email'],input[autocomplete='email']");
    const password = first("input[type='password'],input[name='password'],input[autocomplete='new-password'],input[autocomplete='current-password']");
    const code = first("input[name='code'],input[autocomplete='one-time-code'],input[inputmode='numeric'][maxlength='6']");
    const name = first("input[name='name'],input[name='fullName'],input[name='full_name'],input[id='name'],input[id='fullName'],input[id='full-name'],input[autocomplete='name'],input[placeholder='Full name'],input[placeholder='Name'],input[placeholder='全名'],input[placeholder='姓名'],input[aria-label='Full name'],input[aria-label='Name'],input[aria-label='全名'],input[aria-label='姓名']");
    const profile = first("input[name='age'],input[name='birthdate'],input[name='birthday'],input[name='date_of_birth'],input[name='dob'],input[id='age'],input[id*='birth'],input[autocomplete='bday'],input[type='date'],input[placeholder='Age'],input[placeholder='年龄'],input[placeholder='生日'],input[aria-label='Age'],input[aria-label='年龄'],input[aria-label='生日']");
    const actions = Array.from(document.querySelectorAll("button,a,[role='button']")).filter(visible).map(element => (element.innerText || element.textContent || '').trim()).join('\n');
    const alerts = Array.from(document.querySelectorAll("[role='alert'],[aria-live='assertive']")).filter(visible).map(element => (element.innerText || element.textContent || '').trim()).join('\n');
    return {
        url: location.href,
        body: (document.body?.innerText || '').slice(0, 2500),
        documentKey: String(performance.timeOrigin),
        email: !!email,
        emailValue: email ? (email.value || '') : '',
        password: !!password,
        code: !!code,
        codeInvalid: !!code && (code.getAttribute('aria-invalid') === 'true' || /invalid|incorrect|wrong|expired|错误|无效|过期|正しくありません|無効|有効期限/i.test(alerts)),
        name: !!name,
        profileField: profile ? (profile.getAttribute('name') || (profile.type === 'date' ? 'birthdate' : 'age')) : '',
        ready: !!first("textarea[name='prompt-textarea'],#prompt-textarea,[data-testid='composer'],[data-testid='composer-input'],[contenteditable='true'][data-lexical-editor='true']"),
        retry: /try again|retry|重试|再试一次|再試行|もう一度/i.test(actions),
        actions: actions.slice(0, 500),
        challenge: !!first("iframe[src*='challenge'],iframe[src*='captcha'],iframe[src*='turnstile'],#challenge-running,[data-testid*='captcha'],[class*='captcha']")
    };
}"""
ACTION_DIAGNOSTIC_SCRIPT = r"""() => Array.from(document.querySelectorAll("button,input[type='submit'],a,[role='button']")).map(element => {
    const style = getComputedStyle(element);
    const rect = element.getBoundingClientRect();
    return {
        text: (element.innerText || element.value || element.textContent || '').trim().slice(0, 80),
        tag: element.tagName,
        type: element.getAttribute('type') || '',
        visible: style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0,
        disabled: !!element.disabled,
        box: [Math.round(rect.x), Math.round(rect.y), Math.round(rect.width), Math.round(rect.height)]
    };
}).slice(0, 20)"""


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
    document_key: str = ""
    email: bool = False
    email_value: str = ""
    password: bool = False
    code: bool = False
    code_invalid: bool = False
    name: bool = False
    profile_field: str = ""
    ready: bool = False
    retry: bool = False
    challenge: bool = False
    actions: str = ""


def contains(value: str, needles: tuple[str, ...]) -> bool:
    lowered = value.lower()
    return any(item.lower() in lowered for item in needles)


def classify_page(signals: Signals) -> str:
    if contains(signals.body, DISABLED_TEXT):
        return "disabled"
    if signals.challenge or contains(signals.body, CHALLENGE_TEXT):
        return "challenge"
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
    if signals.retry:
        return "retry"
    return "wait"


def poll_interval(state: str) -> float:
    if state in ("email", "password", "profile", "code", "code_rejected", "retry", "ready"):
        return FAST_POLL_INTERVAL
    if state == "challenge":
        return CHALLENGE_POLL_INTERVAL
    return WAIT_POLL_INTERVAL


def ready_url(raw_url: str, signals: Signals) -> bool:
    parsed = urlsplit(raw_url)
    host = (parsed.hostname or "").lower()
    path = parsed.path.lower()
    valid_host = host in ("chatgpt.com", "chat.openai.com") or host.endswith(".chatgpt.com")
    blocked = signals.email or signals.code or signals.password or signals.name or signals.challenge
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


async def visible_input_value(page: Any, selector: str) -> tuple[bool, str]:
    locator = page.locator(selector)
    for index in range(await locator.count()):
        item = locator.nth(index)
        if await item.is_visible():
            return True, await item.input_value()
    return False, ""


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
    values = await page.evaluate(INSPECT_SCRIPT)
    return Signals(
        url=str(values.get("url", "")), body=str(values.get("body", "")),
        document_key=str(values.get("documentKey", "")), email=bool(values.get("email")),
        email_value=str(values.get("emailValue", "")), password=bool(values.get("password")),
        code=bool(values.get("code")), code_invalid=bool(values.get("codeInvalid")),
        name=bool(values.get("name")), profile_field=str(values.get("profileField", "")),
        ready=bool(values.get("ready")), retry=bool(values.get("retry")),
        challenge=bool(values.get("challenge")), actions=str(values.get("actions", "")),
    )


def navigation_interrupted(exc: BaseException) -> bool:
    text = str(exc).lower()
    return "execution context was destroyed" in text or "because of a navigation" in text


async def inspect_context(context: Any) -> tuple[Any, Signals, str]:
    rank = {
        "ready": 11, "challenge": 10, "disabled": 9, "retry": 8, "profile": 7,
        "password": 6, "code_rejected": 5, "code": 4, "email": 3, "wait": 1,
    }
    best: Optional[tuple[Any, Signals, str]] = None
    for page in reversed(tuple(context.pages)):
        if page.is_closed():
            continue
        try:
            signals = await inspect_page(page)
        except Exception as exc:
            if page.is_closed() or navigation_interrupted(exc):
                continue
            raise
        state = classify_page(signals)
        if best is None or rank[state] > rank[best[2]]:
            best = (page, signals, state)
    if best is not None:
        return best
    pages = tuple(page for page in context.pages if not page.is_closed())
    if pages:
        page = pages[-1]
        return page, Signals(url=page.url), "wait"
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


async def set_input_value(item: Any, value: str) -> None:
    await item.evaluate(
        """(element, value) => {
            const prototype = element instanceof HTMLTextAreaElement
                ? HTMLTextAreaElement.prototype
                : HTMLInputElement.prototype;
            const setter = Object.getOwnPropertyDescriptor(prototype, 'value').set;
            setter.call(element, value);
            element.dispatchEvent(new Event('input', { bubbles: true }));
            element.dispatchEvent(new Event('change', { bubbles: true }));
        }""",
        value,
    )


async def fill_value(page: Any, selector: str, value: str) -> None:
    item = await actionable(page, selector, editable=True)
    await set_input_value(item, value)
    if await item.input_value() != value:
        await item.fill(value)
    if await item.input_value() != value:
        raise SidecarError("input_stalled", "page input did not retain the requested value", True)


async def action_diagnostic(page: Any) -> str:
    values = await page.evaluate(ACTION_DIAGNOSTIC_SCRIPT)
    return json.dumps(values, ensure_ascii=False, separators=(",", ":"))[:2000]


async def click_submit(page: Any) -> None:
    loop = asyncio.get_running_loop()
    deadline = loop.time() + 20
    while loop.time() < deadline:
        candidates = page.locator(ACTIONS)
        for index in range(await candidates.count()):
            item = candidates.nth(index)
            text = (await item.inner_text()).strip()
            button_type = str((await item.get_attribute("type")) or "").lower()
            if EXTERNAL_LOGIN_RE.search(text) or (button_type != "submit" and not SUBMIT_ACTION_RE.match(text)):
                continue
            okay = await item.is_visible() and await item.is_enabled()
            bounds = await item.bounding_box() if okay else None
            await asyncio.sleep(0.15)
            if bounds and bounds == await item.bounding_box():
                await item.click()
                return
        await asyncio.sleep(0.2)
    detail = await action_diagnostic(page)
    raise SidecarError("element_timeout", f"form submit button did not become stable; actions={detail}", True)


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


@dataclass
class EmailSubmissionGate:
    attempts: int = 0
    submitted_at: Optional[float] = None
    submitted_key: str = ""
    empty_since: Optional[float] = None

    def should_submit(self, signals: Signals, now: float) -> bool:
        if self.attempts == 0:
            return True
        if self.attempts >= 3:
            return False
        stable_key = email_page_key(signals)
        if stable_key == self.submitted_key:
            self.empty_since = None
            return self.submitted_at is not None and now - self.submitted_at >= EMAIL_RETRY_INTERVAL
        if self.empty_since is None:
            self.empty_since = now
            return False
        return now - self.empty_since >= 5

    def mark(self, signals: Signals, now: float) -> None:
        self.attempts += 1
        self.submitted_at = now
        self.submitted_key = email_page_key(signals)
        self.empty_since = None

    def ensure_attempt_available(self) -> None:
        if self.attempts >= 3:
            raise SidecarError("email_stalled", "email was requested more than three times", True)

    def ensure_progress(self, now: float) -> None:
        if self.empty_since is not None:
            return
        if self.attempts >= 3 and self.submitted_at is not None and now - self.submitted_at >= 30:
            raise SidecarError("email_stalled", "email page did not advance after three submissions", True)


def monotonic_time() -> float:
    return asyncio.get_running_loop().time()


class VerificationCodes:
    def __init__(self, request_code: Callable[[int], Awaitable[str]]) -> None:
        self.request_code = request_code
        self.attempts = 0
        self.phase = "idle"
        self.submitted_at = 0.0
        self.submitted_key = ""
        self.current_code = ""
        self.current_submissions = 0
        self.rejection_key = ""
        self.refresh_at = 0.0
        self.waited_seconds = 0.0

    def consume_waited_seconds(self) -> float:
        waited = self.waited_seconds
        self.waited_seconds = 0.0
        return waited

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
        if state == "code_rejected" and key != self.submitted_key:
            await self._reject(page, key, now)
            return
        if state == "code" and now - self.submitted_at >= 3 and self.current_submissions < 3:
            await fill_value(page, CODE, self.current_code)
            await click_submit(page)
            self.current_submissions += 1
            self.submitted_at = monotonic_time()
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
        started_at = monotonic_time()
        try:
            code = await self.request_code(self.attempts)
        finally:
            self.waited_seconds += monotonic_time() - started_at
        self.current_code = code
        self.current_submissions = 1
        await fill_value(page, CODE, code)
        await click_submit(page)
        self.phase = "submitted"
        self.submitted_at = monotonic_time()
        self.submitted_key = key


def page_key(signals: Signals) -> str:
    body = re.sub(r"\s+", " ", signals.body).strip()
    return f"{signals.url}\n{body}\n{signals.code_invalid}"


def email_page_key(signals: Signals) -> str:
    return f"{signals.document_key}\n{signals.url}"


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
        self.email = EmailSubmissionGate()
        self.password = SubmissionGate()
        self.profile = SubmissionGate()
        self.retry = SubmissionGate()
        self.challenge_since: Optional[float] = None
        self.last_state_key = ""

    async def run(self, context: Any) -> Any:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + 240
        while loop.time() < deadline:
            page, signals, state = await inspect_context(context)
            page_urls = " | ".join(
                f"{urlsplit(item.url).netloc}{urlsplit(item.url).path}"
                for item in context.pages if not item.is_closed()
            )
            state_key = f"{state}\n{signals.url}\n{signals.document_key}\n{page_urls}"
            if state_key != self.last_state_key:
                parsed = urlsplit(signals.url)
                fields = ",".join(name for name, present in (
                    ("email", signals.email), ("password", signals.password), ("code", signals.code),
                    ("name", signals.name), ("profile", bool(signals.profile_field)), ("ready", signals.ready),
                    ("challenge", signals.challenge),
                ) if present) or "none"
                actions = re.sub(r"\s+", " ", signals.actions).strip()[:240] or "none"
                body = re.sub(r"\s+", " ", signals.body).strip()[:320] or "none"
                await self.log(f"page state={state} url={parsed.netloc}{parsed.path} pages={page_urls} fields={fields} actions={actions} body={body}")
                self.last_state_key = state_key
            try:
                done = await self._step(page, signals, state, loop.time())
            except SidecarError as exc:
                if exc.code not in ("element_timeout", "input_stalled"):
                    raise
                _, current_signals, current_state = await inspect_context(context)
                same_document = current_signals.document_key == signals.document_key
                if current_state == state and same_document:
                    raise
                await self.log(f"page advanced during {state} action; continuing with state={current_state}")
                done = False
            deadline += self.codes.consume_waited_seconds()
            if done:
                return page
            await asyncio.sleep(poll_interval(state))
        raise SidecarError("registration_timeout", "registration did not reach ready state", True)

    async def _step(self, page: Any, signals: Signals, state: str, now: float) -> bool:
        if state == "challenge":
            if self.challenge_since is None:
                self.challenge_since = now
                await self.log("security check detected; waiting for browser verification")
                return False
            if now - self.challenge_since >= 30:
                raise SidecarError("challenge_required", "interactive challenge remained after verification wait")
            return False
        self.challenge_since = None
        if state == "disabled":
            raise SidecarError("account_taken", "account is deleted, deactivated, or already used")
        if state == "retry":
            await self._handle_retry(page, now)
            return False
        if state == "email":
            await self._handle_email(page, signals, now)
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
            self.retry.mark(monotonic_time())
            return
        self.retry.ensure_progress(now, "temporary_error", "temporary page error remained after one retry")

    async def _handle_email(self, page: Any, signals: Signals, now: float) -> None:
        if self.email.should_submit(signals, now):
            self.email.ensure_attempt_available()
            if self.email.attempts:
                await self.log("email requested again; resubmitting")
            await self.log(f"email submission {self.email.attempts + 1}/3: filling field")
            await fill_value(page, EMAIL, str(self.payload["email"]))
            await self.log(f"email submission {self.email.attempts + 1}/3: clicking continue")
            await click_submit(page)
            self.email.mark(signals, monotonic_time())
            await self.log(f"email submission {self.email.attempts}/3: click completed")
            return
        self.email.ensure_progress(now)

    async def _handle_password(self, page: Any, now: float) -> None:
        if self.password.pending():
            await self.log("password submission: filling field")
            await fill_value(page, PASSWORD, str(self.payload["password"]))
            await self.log("password submission: clicking continue")
            await click_submit(page)
            self.password.mark(monotonic_time())
            await self.log("password submission: click completed; waiting for navigation")
            return
        self.password.ensure_progress(now, "password_stalled", "password page did not advance after submission")

    async def _handle_profile(self, page: Any, now: float) -> None:
        if self.profile.pending():
            await self.log("profile submission: filling fields")
            await fill_value(page, NAME, str(self.payload["full_name"]))
            field = await actionable(page, PROFILE, editable=True)
            await fill_profile_field(field, str(self.payload["age"]))
            await self.log("profile submission: clicking continue")
            await click_submit(page)
            self.profile.mark(monotonic_time())
            await self.log("profile submission: click completed; waiting for navigation")
            return
        self.profile.ensure_progress(now, "profile_stalled", "profile page did not advance after submission")


async def fill_profile_field(field: Any, age_value: str) -> None:
    metadata = {
        key: str((await field.get_attribute(key)) or "").lower()
        for key in ("name", "id", "type", "autocomplete", "placeholder", "aria-label")
    }
    attributes = " ".join(metadata.values())
    is_birthdate = metadata["type"] == "date" or any(
        marker in attributes for marker in ("birth", "birthday", "date_of_birth", "dob", "bday")
    )
    value = birthdate_from_age(age_value) if is_birthdate else (age_value if age_value.isdigit() else "30")
    await set_input_value(field, value)
    if await field.input_value() != value:
        await field.fill(value)
    if await field.input_value() != value:
        raise SidecarError("input_stalled", "profile input did not retain the requested value", True)
    await field.blur()
