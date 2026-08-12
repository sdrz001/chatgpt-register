import asyncio
import base64
import importlib.util
import io
import json
import shutil
import subprocess
import sys
import unittest
from datetime import date
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("cloakbrowser_sidecar.py")
SPEC = importlib.util.spec_from_file_location("cloakbrowser_sidecar", MODULE_PATH)
sidecar = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = sidecar
SPEC.loader.exec_module(sidecar)


class ProtocolTests(unittest.TestCase):
    def test_parse_and_validate_nested_start(self):
        raw = {
            "version": 1,
            "type": "start",
            "request_id": "request-1",
            "payload": {
                "email": "person@example.test",
                "password": "secret",
                "full_name": "Test User",
                "age": "25",
                "proxy": "",
                "headless": True,
            },
        }
        parsed = sidecar.parse_message(json.dumps(raw).encode())
        payload = sidecar.validate_start(parsed)
        self.assertEqual(payload["email"], "person@example.test")
        self.assertTrue(payload["headless"])

    def test_validate_and_normalize_start_proxy(self):
        raw = {
            "type": "start",
            "payload": {
                "email": "person@example.test",
                "password": "secret",
                "full_name": "Test User",
                "age": "1990-05-06",
                "proxy": "proxy.test:8080:user:pass",
                "headless": False,
            },
        }
        payload = sidecar.validate_start(raw)
        self.assertEqual(payload["age"], "1990-05-06")
        self.assertEqual(payload["proxy"], "http://user:pass@proxy.test:8080")

    def test_protocol_rejects_invalid_envelopes(self):
        cases = [
            b"not-json",
            b"[]",
            b'{"version":2,"type":"start","request_id":"r"}',
            b'{"version":1,"type":"","request_id":"r"}',
            b'{"version":1,"type":"start","request_id":""}',
        ]
        for case in cases:
            with self.subTest(case=case), self.assertRaises(sidecar.SidecarError):
                sidecar.parse_message(case)

    def test_start_rejects_missing_or_invalid_payload(self):
        with self.assertRaises(sidecar.SidecarError) as caught:
            sidecar.validate_start({"type": "start", "payload": {"headless": "yes"}})
        self.assertEqual(caught.exception.code, "invalid_payload")

    def test_start_rejects_flat_fields(self):
        flat = {
            "type": "start",
            "email": "person@example.test",
            "password": "secret",
            "full_name": "Test User",
            "age": "25",
            "proxy": "",
            "headless": True,
        }
        with self.assertRaises(sidecar.SidecarError) as caught:
            sidecar.validate_start(flat)
        self.assertEqual(caught.exception.code, "protocol_error")

    def test_normalize_proxy_supported_shapes(self):
        cases = {
            "": "",
            "   ": "",
            "proxy.test:8080": "http://proxy.test:8080",
            " user:pass@proxy.test:8080 ": "http://user:pass@proxy.test:8080",
            "user@name:p/a ss@proxy.test:8080": "http://user%40name:p%2Fa%20ss@proxy.test:8080",
            " proxy.test:8080:user:pass ": "http://user:pass@proxy.test:8080",
            "proxy.test:8080:user@name:p/a ss": "http://user%40name:p%2Fa%20ss@proxy.test:8080",
            "http://proxy.test:8080": "http://proxy.test:8080",
            "https://user:pass@proxy.test:443": "https://user:pass@proxy.test:443",
            "socks5://proxy.test:1080": "socks5://proxy.test:1080",
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                self.assertEqual(sidecar.normalize_proxy(value), expected)

    def test_normalize_proxy_rejects_malformed_values(self):
        cases = (
            "proxy.test",
            "proxy.test:bad",
            "proxy.test:0",
            "proxy.test:65536",
            ":8080",
            "proxy.test:8080:user",
            "proxy.test:8080::pass",
            "ftp://proxy.test:21",
            "http://proxy.test",
            "http://user@proxy.test:8080",
            "http://:pass@proxy.test:8080",
            "http://proxy.test:8080/path",
            "http://proxy.test:8080?query=1",
            "http://proxy.test:8080 bad",
        )
        for value in cases:
            with self.subTest(value=value), self.assertRaises(sidecar.SidecarError) as caught:
                sidecar.normalize_proxy(value)
            self.assertEqual(caught.exception.code, "invalid_payload")


class LaunchTests(unittest.IsolatedAsyncioTestCase):
    async def test_launch_context_enables_humanized_input(self):
        payload = {
            "email": "person@example.test",
            "password": "secret",
            "proxy": "",
            "headless": False,
        }
        registration = sidecar.Registration(sidecar.JsonWriter(io.StringIO()), "request", payload)
        context = object()
        launcher = mock.AsyncMock(return_value=context)
        with mock.patch.object(sidecar, "load_launcher", return_value=launcher):
            result = await sidecar.launch_context(registration, Path("profile"))
        self.assertIs(result, context)
        launcher.assert_awaited_once_with("profile", headless=False, humanize=True)

    async def test_launch_context_keeps_proxy_geoip_with_humanize(self):
        payload = {
            "email": "person@example.test",
            "password": "secret",
            "proxy": "http://proxy.example.test:8080",
            "headless": True,
        }
        registration = sidecar.Registration(sidecar.JsonWriter(io.StringIO()), "request", payload)
        launcher = mock.AsyncMock(return_value=object())
        with mock.patch.object(sidecar, "load_launcher", return_value=launcher):
            await sidecar.launch_context(registration, Path("profile"))
        launcher.assert_awaited_once_with(
            "profile", headless=True, humanize=True,
            proxy="http://proxy.example.test:8080", geoip=True
        )


class ConversionTests(unittest.TestCase):
    def test_birthdate_accepts_iso_and_converts_age(self):
        today = date(2026, 6, 15)
        self.assertEqual(sidecar.birthdate_from_age("1991-02-03", today), "1991-02-03")
        self.assertEqual(sidecar.birthdate_from_age("25", today), "2001-06-15")
        self.assertEqual(sidecar.birthdate_from_age("bad", today), "1996-06-15")

    def test_birthdate_handles_leap_day(self):
        self.assertEqual(sidecar.birthdate_from_age("20", date(2024, 2, 29)), "2004-02-29")
        self.assertEqual(sidecar.birthdate_from_age("19", date(2024, 2, 29)), "2005-02-28")


class ClassificationTests(unittest.TestCase):
    def test_poll_interval_adapts_to_page_state(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        for state in ("email", "password", "profile", "code", "code_rejected", "retry", "ready"):
            with self.subTest(state=state):
                self.assertEqual(flow_module.poll_interval(state), 0.25)
        self.assertEqual(flow_module.poll_interval("wait"), 0.5)
        self.assertEqual(flow_module.poll_interval("challenge"), 1.0)

    def test_page_classification_priority(self):
        base = sidecar.Signals(
            url="https://chatgpt.com/",
            email=True,
            password=True,
            code=True,
            code_invalid=True,
            name=True,
            profile_field="birthdate",
            ready=True,
            retry=True,
        )
        self.assertEqual(sidecar.classify_page(base), "profile")
        base.body = "Please verify you are human"
        base.challenge = True
        self.assertEqual(sidecar.classify_page(base), "challenge")
        base.body = "Your account has been deactivated"
        self.assertEqual(sidecar.classify_page(base), "disabled")
        base.body, base.challenge = "", False
        base.retry = False
        self.assertEqual(sidecar.classify_page(base), "profile")
        base.name = False
        self.assertEqual(sidecar.classify_page(base), "password")
        base.password = False
        self.assertEqual(sidecar.classify_page(base), "code_rejected")

    def test_disabled_and_localized_challenge_text(self):
        self.assertEqual(sidecar.classify_page(sidecar.Signals(body="账号已停用")), "disabled")
        self.assertEqual(sidecar.classify_page(sidecar.Signals(body="人間であることを確認")), "challenge")
        self.assertEqual(
            sidecar.classify_page(sidecar.Signals(body="验证成功。正在等待 chatgpt.com 响应")),
            "challenge",
        )

    def test_localized_age_selector_and_challenge_priority(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        self.assertIn("input[placeholder='年龄']", flow_module.PROFILE)
        signals = sidecar.Signals(name=True, profile_field="age", challenge=True)
        self.assertEqual(sidecar.classify_page(signals), "challenge")
        segmented = sidecar.Signals(
            url="https://auth.openai.com/about-you", name=True,
            profile_field="birthdate_segments", profile_value="2026-08-12",
        )
        self.assertEqual(sidecar.classify_page(segmented), "profile")

    def test_ready_url_requires_clean_chat_page(self):
        self.assertTrue(sidecar.ready_url("https://chatgpt.com/", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://chatgpt.com/", sidecar.Signals(challenge=True)))
        self.assertFalse(sidecar.ready_url("https://chatgpt.com/auth/login", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://chatgpt.com/api/auth/session", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://chatgpt.com/new-layout", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://example.test/", sidecar.Signals()))


class ContextSelectionTests(unittest.IsolatedAsyncioTestCase):
    def test_inspection_script_is_valid_javascript(self):
        node = shutil.which("node")
        if node is None:
            self.skipTest("node is not installed")
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        for script in (flow_module.INSPECT_SCRIPT, flow_module.ACTION_DIAGNOSTIC_SCRIPT):
            with self.subTest(script=script[:40]):
                result = subprocess.run(
                    [node, "-e", "new Function('return (' + process.argv[1] + ')')", script],
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(".join('\\n')", flow_module.INSPECT_SCRIPT)

    async def test_atomic_inspection_recognizes_existing_account_home(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        page = mock.MagicMock()
        page.evaluate = mock.AsyncMock(return_value={
            "url": "https://chatgpt.com/", "body": "ChatGPT", "documentKey": "1",
            "email": False, "emailValue": "", "password": False, "code": False,
            "codeInvalid": False, "name": False, "profileField": "", "ready": True,
            "retry": False, "challenge": False,
        })
        signals = await flow_module.inspect_page(page)
        self.assertEqual(sidecar.classify_page(signals), "ready")
        page.evaluate.assert_awaited_once()
        page.locator.assert_not_called()

    async def test_navigation_during_inspection_waits_instead_of_failing(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        page = mock.MagicMock()
        page.url = "https://chatgpt.com/"
        page.is_closed.return_value = False
        context = mock.MagicMock()
        context.pages = [page]
        with mock.patch.object(
            flow_module, "inspect_page", new=mock.AsyncMock(
                side_effect=RuntimeError("Execution context was destroyed, most likely because of a navigation")
            )
        ):
            selected, signals, state = await flow_module.inspect_context(context)
        self.assertIs(selected, page)
        self.assertEqual(signals.url, page.url)
        self.assertEqual(state, "wait")
        context.new_page.assert_not_called()

    async def test_ready_page_wins_over_stale_auth_page(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        ready_page = mock.MagicMock()
        stale_page = mock.MagicMock()
        ready_page.is_closed.return_value = False
        stale_page.is_closed.return_value = False
        context = mock.MagicMock()
        context.pages = [ready_page, stale_page]

        async def inspect(page):
            if page is ready_page:
                return sidecar.Signals(url="https://chatgpt.com/", ready=True)
            return sidecar.Signals(url="https://chatgpt.com/auth/email", email=True)

        with mock.patch.object(flow_module, "inspect_page", side_effect=inspect):
            page, _, state = await flow_module.inspect_context(context)
        self.assertIs(page, ready_page)
        self.assertEqual(state, "ready")

    async def test_action_timeout_continues_when_page_advanced(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        log = mock.AsyncMock()
        flow = sidecar.RegistrationFlow({}, mock.AsyncMock(), log)
        page = mock.MagicMock()
        page.url = "https://chatgpt.com/auth/login"
        page.is_closed.return_value = False
        context = mock.MagicMock()
        context.pages = [page]
        email = sidecar.Signals(
            url="https://chatgpt.com/auth/login", document_key="email-document", email=True
        )
        code = sidecar.Signals(
            url="https://auth.openai.com/email-verification", document_key="code-document", code=True
        )
        ready = sidecar.Signals(
            url="https://chatgpt.com/", document_key="ready-document", ready=True
        )
        with mock.patch.object(
            flow_module, "inspect_context", new=mock.AsyncMock(
                side_effect=[(page, email, "email"), (page, code, "code"), (page, ready, "ready")]
            )
        ), mock.patch.object(
            flow, "_step", new=mock.AsyncMock(
                side_effect=[sidecar.SidecarError("element_timeout", "stale email element", True), True]
            )
        ), mock.patch.object(flow_module.asyncio, "sleep", new=mock.AsyncMock()):
            selected = await flow.run(context)
        self.assertIs(selected, page)
        self.assertIn(
            mock.call("page advanced during email action; continuing with state=code"),
            log.await_args_list,
        )


class SanitizingAndMessageTests(unittest.TestCase):
    def test_redaction_keeps_auth_hosts_and_masks_long_jwt(self):
        jwt = "abc." + "p" * 16 + "." + "s" * 8
        text = "host=auth.openai.com host=chatgpt.com token=" + jwt
        clean = sidecar.redact_sensitive(text)
        self.assertIn("auth.openai.com", clean)
        self.assertIn("chatgpt.com", clean)
        self.assertNotIn(jwt, clean)
        self.assertIn("[token]", clean)

    def test_redaction_keeps_natural_token_logs_and_masks_credentials(self):
        status = "session token acquired; accessToken plan=free"
        self.assertEqual(sidecar.redact_sensitive(status), status)
        clean = sidecar.redact_sensitive("access_token=secret-value; Bearer bearer-value")
        self.assertNotIn("secret-value", clean)
        self.assertNotIn("bearer-value", clean)
        self.assertEqual(clean, "access_token=[redacted]; Bearer [redacted]")

    def test_redaction_masks_birthdate_formats(self):
        clean = sidecar.redact_sensitive("birthdates 1991-02-03 1991/02/03 02/03/1991 1991 / 02 / 03")
        for value in ("1991-02-03", "1991/02/03", "02/03/1991", "1991 / 02 / 03"):
            self.assertNotIn(value, clean)
        self.assertEqual(clean.count("[date]"), 4)

    def test_network_diagnostic_keeps_safe_redirect_chain(self):
        response = mock.MagicMock()
        response.url = "https://auth.openai.com/api/accounts/authorize"
        response.status = 302
        response.headers = {"location": "https://chatgpt.com/auth/callback"}
        response.request.resource_type = "document"
        response.request.method = "GET"
        message = sidecar.network_response_message(response)
        self.assertIsNotNone(message)
        self.assertIn("auth.openai.com", message)
        self.assertIn("chatgpt.com", message)
        self.assertIn("status=302", message)
        self.assertNotIn("[token]", message)

    def test_registration_flushes_scheduled_network_logs(self):
        async def run():
            stream = io.StringIO()
            registration = sidecar.Registration(
                sidecar.JsonWriter(stream), "request", {"email": "person@example.test"}
            )
            registration.schedule_log("network fetch POST host=chatgpt.com path=/ces/v1/rgstr status=202")
            await registration.flush_logs()
            return [json.loads(line) for line in stream.getvalue().splitlines()]

        messages = asyncio.run(run())
        self.assertEqual(len(messages), 1)
        self.assertEqual(messages[0]["type"], "log")
        self.assertIn("/ces/v1/rgstr", messages[0]["message"])
        self.assertFalse(asyncio.iscoroutine(messages[0]))

    def test_redaction_covers_all_sensitive_shapes(self):
        token = "header.payload.signature"
        text = "person@example.test secret-pass http://user:proxy-pass@host:8080 code 654321 access_token=" + token
        clean = sidecar.redact_sensitive(text, ("person@example.test", "secret-pass", "proxy-pass", token))
        for secret in ("person@example.test", "secret-pass", "proxy-pass", "654321", token):
            self.assertNotIn(secret, clean)
        self.assertIn("[redacted]", clean)
        self.assertIn("[code]", clean)

    def test_screenshot_message_limit_and_png(self):
        png = b"\x89PNG\r\n\x1a\n" + b"payload"
        result = sidecar.screenshot_message("request", png)
        self.assertEqual(base64.b64decode(result["data"]), png)
        self.assertEqual(result["version"], 1)
        with self.assertRaises(ValueError):
            sidecar.screenshot_message("request", b"x" * (sidecar.SCREENSHOT_LIMIT + 1))
        with self.assertRaises(ValueError):
            sidecar.screenshot_message("request", b"not png")

    def test_json_writer_outputs_one_protocol_line(self):
        stream = io.StringIO()
        writer = sidecar.JsonWriter(stream)
        asyncio.run(writer.send(sidecar.message("ready", "request")))
        self.assertEqual(json.loads(stream.getvalue()), {"version": 1, "type": "ready", "request_id": "request"})

    def test_health_does_not_launch_browser(self):
        stream = io.StringIO()
        with mock.patch.object(sidecar, "load_launcher", return_value=object()):
            status = sidecar.health(stream)
        result = json.loads(stream.getvalue())
        self.assertEqual(status, 0)
        self.assertEqual(result["type"], "health")
        self.assertTrue(result["ok"])

    def test_health_import_failure_has_exit_one(self):
        stream = io.StringIO()
        detail = r"No module at D:\\private\\venv\\site-packages\\cloakbrowser.py"
        with mock.patch.object(sidecar, "load_launcher", side_effect=ImportError(detail)):
            status = sidecar.health(stream)
        result = json.loads(stream.getvalue())
        self.assertEqual(status, 1)
        self.assertFalse(result["ok"])
        self.assertEqual(result["code"], "cloakbrowser_import")
        self.assertEqual(result["message"], "cloakbrowser import failed")
        self.assertNotIn("private", stream.getvalue())


class InputFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_fill_value_writes_complete_value_once(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        field = mock.AsyncMock()
        field.input_value.return_value = "person@example.test"
        page = mock.MagicMock()
        with mock.patch.object(flow_module, "actionable", new=mock.AsyncMock(return_value=field)):
            await flow_module.fill_value(page, "input[name='email']", "person@example.test")
        field.evaluate.assert_awaited_once()
        self.assertEqual(field.evaluate.await_args.args[1], "person@example.test")
        field.fill.assert_not_awaited()
        field.click.assert_not_awaited()
        field.press.assert_not_awaited()
        field.type.assert_not_awaited()

    async def test_age_field_uses_fill(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        field = mock.AsyncMock()
        field.get_attribute.side_effect = ["age", "", "text", "", "", ""]
        field.input_value.return_value = "25"
        await flow_module.fill_profile_field(field, "25")
        field.evaluate.assert_awaited_once()
        self.assertEqual(field.evaluate.await_args.args[1], "25")
        field.fill.assert_not_awaited()
        field.type.assert_not_awaited()
        field.blur.assert_awaited_once()

    async def test_birthdate_field_converts_age_to_iso_date(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        field = mock.AsyncMock()
        field.get_attribute.side_effect = ["", "birthdate-input", "text", "bday", "生日日期", "出生日期"]
        field.input_value.return_value = "1991-02-03"
        with mock.patch.object(flow_module, "birthdate_from_age", return_value="1991-02-03"), mock.patch.object(
            flow_module.asyncio, "sleep", new=mock.AsyncMock()
        ):
            value = await flow_module.fill_profile_field(field, "35")
        self.assertEqual(value, "1991-02-03")
        field.evaluate.assert_awaited_once()
        self.assertEqual(field.evaluate.await_args.args[1], "1991-02-03")
        field.fill.assert_not_awaited()
        field.dispatch_event.assert_not_awaited()
        field.blur.assert_awaited_once()

    async def test_localized_birthdate_recovers_after_controlled_input_rollback(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        field = mock.AsyncMock()
        attributes = {
            "name": "", "id": "", "type": "text", "autocomplete": "",
            "placeholder": "出生日期", "aria-label": "出生日期",
        }
        field.get_attribute.side_effect = lambda key: attributes[key]
        field.input_value.side_effect = ["2026/08/12", "2026/08/12", "1991/02/03"]
        with mock.patch.object(flow_module, "birthdate_from_age", return_value="1991-02-03"), mock.patch.object(
            flow_module.asyncio, "sleep", new=mock.AsyncMock()
        ):
            value = await flow_module.fill_profile_field(field, "35")
        self.assertEqual(value, "1991/02/03")
        self.assertEqual(field.evaluate.await_args.args[1], "1991/02/03")
        field.fill.assert_awaited_once_with("1991/02/03")
        self.assertEqual(field.blur.await_count, 2)

    async def test_segmented_birthdate_fills_year_month_day(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        segments = [mock.AsyncMock() for _ in range(3)]
        values = {"year": "1991", "month": "08", "day": "12"}
        for segment, kind in zip(segments, ("year", "month", "day")):
            segment.get_attribute.side_effect = lambda key, kind=kind: kind if key == "data-type" else ""
            segment.get_by_kind = kind
        locator = mock.MagicMock()
        locator.count = mock.AsyncMock(return_value=3)
        locator.nth.side_effect = segments
        group = mock.MagicMock()
        group.is_visible = mock.AsyncMock(return_value=True)
        group.locator.return_value = locator
        groups = mock.MagicMock()
        groups.count = mock.AsyncMock(return_value=1)
        groups.nth.return_value = group
        page = mock.MagicMock()
        page.locator.return_value = groups
        with mock.patch.object(flow_module, "birthdate_from_age", return_value="1991-08-12"), mock.patch.object(
            flow_module, "fill_date_segment", new=mock.AsyncMock()
        ) as fill_segment, mock.patch.object(
            flow_module, "date_segment_value", new=mock.AsyncMock(side_effect=["1991", "08", "12"])
        ):
            value = await flow_module.fill_segmented_birthdate(page, "35")
        self.assertEqual(value, "1991-08-12")
        self.assertEqual(
            fill_segment.await_args_list,
            [mock.call(segment, values[segment.get_by_kind]) for segment in segments],
        )

    async def test_segmented_birthdate_uses_localized_labels(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        segments = [mock.AsyncMock() for _ in range(3)]
        labels = ("年", "月", "日")
        for segment, label in zip(segments, labels):
            segment.get_attribute.side_effect = lambda key, label=label: label if key == "aria-label" else ""
        locator = mock.MagicMock()
        locator.count = mock.AsyncMock(return_value=3)
        locator.nth.side_effect = segments
        group = mock.MagicMock()
        group.is_visible = mock.AsyncMock(return_value=True)
        group.locator.return_value = locator
        groups = mock.MagicMock()
        groups.count = mock.AsyncMock(return_value=1)
        groups.nth.return_value = group
        page = mock.MagicMock()
        page.locator.return_value = groups
        with mock.patch.object(flow_module, "birthdate_from_age", return_value="1991-08-12"), mock.patch.object(
            flow_module, "fill_date_segment", new=mock.AsyncMock()
        ) as fill_segment, mock.patch.object(
            flow_module, "date_segment_value", new=mock.AsyncMock(side_effect=["1991", "08", "12"])
        ):
            await flow_module.fill_segmented_birthdate(page, "35")
        self.assertEqual(
            [item.args[1] for item in fill_segment.await_args_list],
            ["1991", "08", "12"],
        )

    async def test_segmented_birthdate_does_not_mix_separate_groups(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        groups = []
        for kinds in (("year",), ("month", "day")):
            segments = []
            for kind in kinds:
                segment = mock.AsyncMock()
                segment.get_attribute.side_effect = lambda key, kind=kind: kind if key == "data-type" else ""
                segments.append(segment)
            locator = mock.MagicMock()
            locator.count = mock.AsyncMock(return_value=len(segments))
            locator.nth.side_effect = segments
            group = mock.MagicMock()
            group.is_visible = mock.AsyncMock(return_value=True)
            group.locator.return_value = locator
            groups.append(group)
        group_locator = mock.MagicMock()
        group_locator.count = mock.AsyncMock(return_value=2)
        group_locator.nth.side_effect = groups
        page = mock.MagicMock()
        page.locator.return_value = group_locator
        with self.assertRaises(sidecar.SidecarError) as caught:
            await flow_module.fill_segmented_birthdate(page, "35")
        self.assertEqual(caught.exception.code, "element_timeout")

    async def test_chinese_about_you_selectors_are_supported(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        self.assertIn("input[placeholder='全名']", flow_module.NAME)
        self.assertIn("input[aria-label='全名']", flow_module.NAME)
        self.assertIn("input[placeholder='年龄']", flow_module.PROFILE)
        self.assertIn("input[placeholder='出生日期']", flow_module.PROFILE)
        self.assertEqual(
            sidecar.classify_page(sidecar.Signals(
                url="https://auth.openai.com/about-you", name=True, profile_field="age"
            )),
            "profile",
        )

    async def test_submit_selector_excludes_sso_buttons(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        self.assertNotIn("form button,", flow_module.SUBMIT)
        self.assertIn("button[type='submit']", flow_module.SUBMIT)
        self.assertIn("button:not([type])", flow_module.SUBMIT)
        self.assertNotIn("button[type='button']", flow_module.SUBMIT)

    async def test_click_submit_skips_external_login_buttons(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        google = mock.AsyncMock()
        google.inner_text.return_value = "使用 Google 账户继续"
        google.get_attribute.return_value = "button"
        submit = mock.AsyncMock()
        submit.inner_text.return_value = "继续"
        submit.get_attribute.return_value = "button"
        for item in (google, submit):
            item.is_visible.return_value = True
            item.is_enabled.return_value = True
            item.bounding_box.return_value = {"x": 1, "y": 1, "width": 100, "height": 40}
        candidates = mock.MagicMock()
        candidates.count = mock.AsyncMock(return_value=2)
        candidates.nth.side_effect = [google, submit]
        page = mock.MagicMock()
        page.locator.return_value = candidates
        with mock.patch.object(flow_module.asyncio, "sleep", new=mock.AsyncMock()):
            await flow_module.click_submit(page)
        page.locator.assert_called_once_with(flow_module.ACTIONS)
        google.click.assert_not_awaited()
        submit.click.assert_awaited_once()
        submit.evaluate.assert_not_awaited()

    async def test_click_submit_accepts_localized_complete_account_action(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        submit = mock.AsyncMock()
        submit.inner_text.return_value = "完成帐户创建"
        submit.get_attribute.return_value = "button"
        submit.is_visible.return_value = True
        submit.is_enabled.return_value = True
        submit.bounding_box.return_value = {"x": 1, "y": 1, "width": 100, "height": 40}
        candidates = mock.MagicMock()
        candidates.count = mock.AsyncMock(return_value=1)
        candidates.nth.return_value = submit
        page = mock.MagicMock()
        page.locator.return_value = candidates
        with mock.patch.object(flow_module.asyncio, "sleep", new=mock.AsyncMock()):
            await flow_module.click_submit(page)
        submit.click.assert_awaited_once()

    async def test_click_submit_accepts_nonstandard_next_action(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        submit = mock.AsyncMock()
        submit.inner_text.return_value = "下一步"
        submit.get_attribute.return_value = "button"
        submit.is_visible.return_value = True
        submit.is_enabled.return_value = True
        submit.bounding_box.return_value = {"x": 1, "y": 1, "width": 100, "height": 40}
        candidates = mock.MagicMock()
        candidates.count = mock.AsyncMock(return_value=1)
        candidates.nth.return_value = submit
        page = mock.MagicMock()
        page.locator.return_value = candidates
        with mock.patch.object(flow_module.asyncio, "sleep", new=mock.AsyncMock()):
            await flow_module.click_submit(page)
        submit.click.assert_awaited_once()
        submit.evaluate.assert_not_awaited()

    async def test_fill_value_falls_back_when_framework_rolls_back_value(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        field = mock.AsyncMock()
        field.input_value.side_effect = ["", "person@example.test"]
        page = mock.MagicMock()
        with mock.patch.object(flow_module, "actionable", new=mock.AsyncMock(return_value=field)):
            await flow_module.fill_value(page, flow_module.EMAIL, "person@example.test")
        field.fill.assert_awaited_once_with("person@example.test")

    async def test_new_profile_page_fills_name_and_birthdate(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"full_name": "Reese Brooks", "age": "35"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        name_field = mock.AsyncMock()
        birthdate_field = mock.AsyncMock()
        with mock.patch.object(
            flow_module, "actionable", new=mock.AsyncMock(side_effect=[name_field, birthdate_field])
        ), mock.patch.object(flow_module, "stable_input_value", new=mock.AsyncMock()) as fill_name, mock.patch.object(
            flow_module, "fill_profile_field", new=mock.AsyncMock(return_value="1991-02-03")
        ) as fill_birthdate, mock.patch.object(flow_module, "click_submit", new=mock.AsyncMock()) as submit:
            await flow._handle_profile(page, sidecar.Signals(), 1.0)
        fill_name.assert_awaited_once_with(name_field, "Reese Brooks")
        fill_birthdate.assert_awaited_once_with(birthdate_field, "35")
        submit.assert_awaited_once_with(page)

    async def test_segmented_profile_page_fills_segments_before_submit(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"full_name": "Reese Brooks", "age": "35"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        name_field = mock.AsyncMock()
        signals = sidecar.Signals(profile_field="birthdate_segments", profile_key="segments")
        with mock.patch.object(
            flow_module, "actionable", new=mock.AsyncMock(return_value=name_field)
        ), mock.patch.object(flow_module, "stable_input_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "fill_segmented_birthdate", new=mock.AsyncMock(return_value="1991-08-12")
        ) as fill_segments, mock.patch.object(flow_module, "click_submit", new=mock.AsyncMock()) as submit:
            await flow._handle_profile(page, signals, 1.0)
        fill_segments.assert_awaited_once_with(page, "35")
        submit.assert_awaited_once_with(page)

    def test_profile_submission_gate_throttles_validation_retries(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        gate = flow_module.ProfileSubmissionGate()
        gate.mark(1.0, "form-1", "Reese Brooks", "1991/02/03")
        stable = sidecar.Signals(profile_key="form-1", name_value="Reese Brooks", profile_value="1991/02/03")
        rollback = sidecar.Signals(profile_key="form-1", name_value="", profile_value="2026/08/12")
        self.assertFalse(gate.should_submit(rollback, 1.5))
        self.assertTrue(gate.should_submit(rollback, 2.0))
        self.assertFalse(gate.should_submit(stable, 29.9))

    async def test_profile_page_waits_for_navigation_without_resubmitting(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"full_name": "Reese Brooks", "age": "35"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        field = mock.AsyncMock()
        initial = sidecar.Signals(profile_key="form-1")
        stable = sidecar.Signals(profile_key="form-1", name_value="Reese Brooks", profile_value="1991-02-03")
        rollback = sidecar.Signals(profile_key="form-1", name_value="", profile_value="2026-08-12")
        with mock.patch.object(
            flow_module, "actionable", new=mock.AsyncMock(return_value=field)
        ), mock.patch.object(flow_module, "stable_input_value", new=mock.AsyncMock()) as fill_name, mock.patch.object(
            flow_module, "fill_profile_field", new=mock.AsyncMock(return_value="1991-02-03")
        ) as fill_profile, mock.patch.object(flow_module, "click_submit", new=mock.AsyncMock()) as submit, mock.patch.object(
            flow_module, "monotonic_time", side_effect=[1.0, 3.0]
        ):
            await flow._handle_profile(page, initial, 1.0)
            await flow._handle_profile(page, stable, 2.0)
            await flow._handle_profile(page, rollback, 3.0)
            await flow._handle_profile(page, stable, 29.9)
        self.assertEqual(fill_name.await_count, 2)
        self.assertEqual(fill_profile.await_count, 2)
        self.assertEqual(submit.await_count, 2)
        with self.assertRaises(sidecar.SidecarError) as caught:
            await flow._handle_profile(page, stable, 33.0)
        self.assertEqual(caught.exception.code, "profile_stalled")


class PasswordFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_password_page_waits_for_navigation_without_resubmitting(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"password": "long-enough-password"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit, mock.patch.object(flow_module, "monotonic_time", return_value=1.0):
            await flow._handle_password(page, 1.0)
            await flow._handle_password(page, 4.0)
            await flow._handle_password(page, 29.9)
        self.assertEqual(fill.await_count, 1)
        self.assertEqual(submit.await_count, 1)

    async def test_password_page_errors_after_three_submit_attempts(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"password": "long-enough-password"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ), mock.patch.object(flow_module, "monotonic_time", return_value=1.0):
            await flow._handle_password(page, 1.0)
            await flow._handle_password(page, 4.0)
            await flow._handle_password(page, 29.9)
            with self.assertRaises(sidecar.SidecarError) as caught:
                await flow._handle_password(page, 31.0)
        self.assertEqual(caught.exception.code, "password_stalled")


class ChallengeFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_transient_challenge_waits_and_then_clears(self):
        flow = sidecar.RegistrationFlow({}, mock.AsyncMock(), mock.AsyncMock())
        page = mock.MagicMock()
        self.assertFalse(await flow._step(page, sidecar.Signals(challenge=True), "challenge", 1.0))
        self.assertFalse(await flow._step(page, sidecar.Signals(), "wait", 5.0))
        self.assertIsNone(flow.challenge_since)

    async def test_persistent_challenge_waits_for_slow_verification(self):
        flow = sidecar.RegistrationFlow({}, mock.AsyncMock(), mock.AsyncMock())
        page = mock.MagicMock()
        await flow._step(page, sidecar.Signals(challenge=True), "challenge", 1.0)
        self.assertFalse(await flow._step(page, sidecar.Signals(challenge=True), "challenge", 91.0))
        with self.assertRaises(sidecar.SidecarError) as caught:
            await flow._step(page, sidecar.Signals(challenge=True), "challenge", 121.0)
        self.assertEqual(caught.exception.code, "challenge_required")

    async def test_verification_success_wait_transitions_to_email(self):
        flow = sidecar.RegistrationFlow({}, mock.AsyncMock(), mock.AsyncMock())
        page = mock.MagicMock()
        waiting = sidecar.Signals(body="验证成功。正在等待 chatgpt.com 响应")
        await flow._step(page, sidecar.Signals(challenge=True), "challenge", 1.0)
        self.assertFalse(await flow._step(page, waiting, sidecar.classify_page(waiting), 91.0))
        handler = mock.AsyncMock()
        with mock.patch.object(flow, "_handle_email", handler):
            self.assertFalse(await flow._step(page, sidecar.Signals(email=True), "email", 92.0))
        handler.assert_awaited_once()
        self.assertIsNone(flow.challenge_since)


class EmailFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_same_email_page_with_value_does_not_resubmit(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        first = sidecar.Signals(
            url="https://auth.openai.com/email", document_key="document-1", email=True
        )
        retained = sidecar.Signals(
            url=first.url,
            document_key=first.document_key,
            email=True,
            email_value="person@example.test",
        )
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit:
            await flow._handle_email(page, first, 1.0)
            await flow._handle_email(page, retained, 2.0)
        self.assertEqual(fill.await_count, 1)
        self.assertEqual(submit.await_count, 1)

    async def test_same_email_page_retries_swallowed_submit_click(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        signals = sidecar.Signals(
            url="https://auth.openai.com/email", document_key="document-1", email=True,
            email_value="person@example.test",
        )
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit, mock.patch.object(flow_module, "monotonic_time", side_effect=[1.0, 16.0, 31.0]):
            await flow._handle_email(page, signals, 1.0)
            await flow._handle_email(page, signals, 15.9)
            await flow._handle_email(page, signals, 16.0)
            await flow._handle_email(page, signals, 31.0)
        self.assertEqual(fill.await_count, 3)
        self.assertEqual(submit.await_count, 3)

    async def test_slow_humanized_click_does_not_trigger_immediate_resubmit(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        signals = sidecar.Signals(
            url="https://chatgpt.com/auth/login", document_key="document-1", email=True,
            email_value="person@example.test",
        )
        clock = 1.0

        async def slow_click(_page):
            nonlocal clock
            clock = 4.0

        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", side_effect=slow_click
        ) as submit, mock.patch.object(flow_module, "monotonic_time", side_effect=lambda: clock):
            await flow._handle_email(page, signals, 1.0)
            await flow._handle_email(page, signals, 4.0)
        self.assertEqual(flow.email.submitted_at, 4.0)
        self.assertEqual(fill.await_count, 1)
        self.assertEqual(submit.await_count, 1)

    async def test_new_prefilled_email_page_resubmits_after_stability_delay(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        first = sidecar.Signals(
            url="https://auth.openai.com/email", document_key="document-1", email=True
        )
        second = sidecar.Signals(
            url="https://chatgpt.com/auth/email", document_key="document-2", email=True,
            email_value="person@example.test",
        )
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit:
            await flow._handle_email(page, first, 1.0)
            await flow._handle_email(page, second, 2.0)
            await flow._handle_email(page, second, 6.9)
            await flow._handle_email(page, second, 7.0)
        self.assertEqual(fill.await_count, 2)
        self.assertEqual(submit.await_count, 2)

    async def test_new_email_page_waits_before_resubmitting(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        first = sidecar.Signals(
            url="https://auth.openai.com/email", document_key="document-1", email=True
        )
        second = sidecar.Signals(
            url="https://chatgpt.com/auth/email", document_key="document-2", email=True
        )
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit:
            await flow._handle_email(page, first, 1.0)
            await flow._handle_email(page, second, 2.0)
            await flow._handle_email(page, second, 6.9)
            await flow._handle_email(page, second, 7.0)
        self.assertEqual(fill.await_count, 2)
        self.assertEqual(submit.await_count, 2)

    async def test_cleared_email_field_resubmits_after_stability_delay(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        signals = sidecar.Signals(
            url="https://auth.openai.com/email", document_key="document-1", email=True
        )
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit, mock.patch.object(flow_module, "monotonic_time", side_effect=[1.0, 16.0]):
            await flow._handle_email(page, signals, 1.0)
            await flow._handle_email(page, signals, 2.0)
            await flow._handle_email(page, signals, 15.9)
            await flow._handle_email(page, signals, 16.0)
        self.assertEqual(fill.await_count, 2)
        self.assertEqual(submit.await_count, 2)

    async def test_email_submission_is_limited_to_three_attempts(self):
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        flow = sidecar.RegistrationFlow(
            {"email": "person@example.test"}, mock.AsyncMock(), mock.AsyncMock()
        )
        page = mock.MagicMock()
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ), mock.patch.object(flow_module, "monotonic_time", side_effect=[1.0, 16.0, 31.0]):
            signals = sidecar.Signals(
                url="https://auth.openai.com/email", document_key="document-1", email=True
            )
            await flow._handle_email(page, signals, 1.0)
            await flow._handle_email(page, signals, 15.9)
            await flow._handle_email(page, signals, 16.0)
            await flow._handle_email(page, signals, 30.9)
            await flow._handle_email(page, signals, 31.0)
            await flow._handle_email(page, signals, 32.0)
            with self.assertRaises(sidecar.SidecarError) as caught:
                await flow._handle_email(page, signals, 61.0)
        self.assertEqual(caught.exception.code, "email_stalled")


class VerificationFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_slow_code_fetch_starts_stall_timer_after_submission(self):
        clock = 100.0

        async def request_code(_attempt):
            nonlocal clock
            clock = 160.0
            return "123456"

        page = mock.MagicMock()
        codes = sidecar.RegistrationFlow({}, request_code, mock.AsyncMock()).codes
        signals = sidecar.Signals(url="https://chatgpt.com/verify", body="Enter code", code=True)
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ), mock.patch.object(flow_module, "monotonic_time", side_effect=lambda: clock):
            await codes.step(page, signals, "code", 100.0)
            self.assertEqual(codes.submitted_at, 160.0)
            await codes.step(page, signals, "code", 161.0)

    async def test_code_page_retries_same_code_when_submit_click_is_swallowed(self):
        requested = []

        async def request_code(attempt):
            requested.append(attempt)
            return "123456"

        page = mock.MagicMock()
        codes = sidecar.RegistrationFlow({}, request_code, mock.AsyncMock()).codes
        signals = sidecar.Signals(url="https://chatgpt.com/verify", body="Enter code", code=True)
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        clock = 1.0
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()) as fill, mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ) as submit, mock.patch.object(flow_module, "monotonic_time", side_effect=lambda: clock):
            await codes.step(page, signals, "code", 1.0)
            clock = 4.0
            await codes.step(page, signals, "code", 4.0)
            clock = 7.0
            await codes.step(page, signals, "code", 7.0)
        self.assertEqual(requested, [1])
        self.assertEqual(fill.await_count, 3)
        self.assertEqual(submit.await_count, 3)

    async def test_rejected_page_requests_once_until_page_changes(self):
        requested = []

        async def request_code(attempt):
            requested.append(attempt)
            return str(100000 + attempt)

        page = mock.MagicMock()
        codes = sidecar.RegistrationFlow({}, request_code, mock.AsyncMock()).codes
        clean = sidecar.Signals(url="https://chatgpt.com/verify", body="Enter code", code=True)
        submitted = sidecar.Signals(url=clean.url, body="", code=True)
        rejected = sidecar.Signals(url=clean.url, body="Invalid code", code=True, code_invalid=True)
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ), mock.patch.object(flow_module, "click_action", new=mock.AsyncMock(return_value=True)):
            await codes.step(page, clean, "code", 1.0)
            await codes.step(page, submitted, "code", 1.5)
            await codes.step(page, rejected, "code_rejected", 2.0)
            await codes.step(page, rejected, "code_rejected", 3.0)
            await codes.step(page, rejected, "code_rejected", 4.0)
            self.assertEqual(requested, [1])
            await codes.step(page, clean, "code", 5.0)
            self.assertEqual(requested, [1, 2])

    async def test_three_fresh_codes_are_the_limit(self):
        requested = []

        async def request_code(attempt):
            requested.append(attempt)
            return str(200000 + attempt)

        page = mock.MagicMock()
        codes = sidecar.RegistrationFlow({}, request_code, mock.AsyncMock()).codes
        flow_module = sys.modules[sidecar.RegistrationFlow.__module__]
        with mock.patch.object(flow_module, "fill_value", new=mock.AsyncMock()), mock.patch.object(
            flow_module, "click_submit", new=mock.AsyncMock()
        ), mock.patch.object(flow_module, "click_action", new=mock.AsyncMock(return_value=True)):
            for attempt in range(1, 4):
                clean = sidecar.Signals(url="https://chatgpt.com/verify", body=f"Enter code {attempt}", code=True)
                rejected = sidecar.Signals(
                    url=clean.url,
                    body=f"Invalid code {attempt}",
                    code=True,
                    code_invalid=True,
                )
                await codes.step(page, clean, "code", float(attempt * 3))
                if attempt < 3:
                    await codes.step(page, rejected, "code_rejected", float(attempt * 3 + 1))
            self.assertEqual(requested, [1, 2, 3])
            with self.assertRaises(sidecar.SidecarError) as caught:
                await codes.step(page, rejected, "code_rejected", 10.0)
            self.assertEqual(caught.exception.code, "code_invalid")


class SessionTests(unittest.IsolatedAsyncioTestCase):
    def session_page(self, responses, bodies):
        page = mock.MagicMock()
        page.goto = mock.AsyncMock(side_effect=responses)
        body = mock.AsyncMock()
        body.inner_text = mock.AsyncMock(side_effect=bodies)
        page.locator.return_value = body
        return page

    async def test_access_token_uses_current_logged_in_page(self):
        response = mock.MagicMock(ok=True)
        page = self.session_page(
            [response], [json.dumps({"accessToken": "header.payload.signature"})]
        )
        token = await sidecar.read_access_token(page)
        self.assertEqual(token, "header.payload.signature")
        page.goto.assert_awaited_once_with(
            sidecar.SESSION_URL, wait_until="domcontentloaded", timeout=15000
        )

    async def test_access_token_retries_transient_session_failure(self):
        page = self.session_page(
            [mock.MagicMock(ok=False), mock.MagicMock(ok=True)],
            [json.dumps({"accessToken": "header.payload.signature"})],
        )
        with mock.patch.object(sidecar.asyncio, "sleep", new=mock.AsyncMock()):
            token = await sidecar.read_access_token(page)
        self.assertEqual(token, "header.payload.signature")
        self.assertEqual(page.goto.await_count, 2)

    async def test_access_token_waits_for_cookie_stability(self):
        response = mock.MagicMock(ok=True)
        page = self.session_page(
            [response, response, response],
            ["{}", "{}", json.dumps({"accessToken": "header.payload.signature"})],
        )
        with mock.patch.object(sidecar.asyncio, "sleep", new=mock.AsyncMock()) as sleep:
            token = await sidecar.read_access_token(page)
        self.assertEqual(token, "header.payload.signature")
        self.assertEqual(page.goto.await_count, 3)
        self.assertEqual(sleep.await_count, 2)

    async def test_existing_account_direct_login_still_reads_session(self):
        registration = mock.MagicMock()
        registration.request_id = "request"
        registration.secrets = []
        registration.payload = {}
        registration.log = mock.AsyncMock()
        context = mock.MagicMock()
        login_page = mock.MagicMock()
        login_page.goto = mock.AsyncMock()
        context.pages = [login_page]
        registration.registration_loop = mock.AsyncMock(return_value=login_page)
        with mock.patch.object(sidecar, "launch_context", new=mock.AsyncMock(return_value=context)), mock.patch.object(
            sidecar, "read_access_token", new=mock.AsyncMock(return_value="header.payload.signature")
        ) as read_token, mock.patch.object(
            sidecar, "close_context", new=mock.AsyncMock()
        ), mock.patch.object(sidecar.tempfile, "mkdtemp", return_value="profile"), mock.patch.object(sidecar.shutil, "rmtree"):
            token = await sidecar.browse(registration)
        self.assertEqual(token, "header.payload.signature")
        registration.registration_loop.assert_awaited_once_with(context)
        read_token.assert_awaited_once_with(login_page, registration.log)
        self.assertEqual(
            registration.log.await_args_list[-2],
            mock.call("login detected; reading session in current page"),
        )

    async def test_browser_exception_still_emits_screenshot(self):
        registration = mock.MagicMock()
        registration.request_id = "request"
        registration.context = object()
        registration.secrets = []
        registration.payload = {}
        with mock.patch.object(sidecar, "launch_context", new=mock.AsyncMock(side_effect=RuntimeError("boom"))), mock.patch.object(
            sidecar, "emit_screenshot", new=mock.AsyncMock()
        ) as screenshot, mock.patch.object(sidecar, "close_context", new=mock.AsyncMock()), mock.patch.object(
            sidecar.tempfile, "mkdtemp", return_value="profile"
        ), mock.patch.object(sidecar.shutil, "rmtree"):
            with self.assertRaises(RuntimeError):
                await sidecar.browse(registration)
        screenshot.assert_awaited_once_with(registration)


if __name__ == "__main__":
    unittest.main()
