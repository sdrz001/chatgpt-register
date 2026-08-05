import asyncio
import base64
import importlib.util
import io
import json
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
        self.assertEqual(sidecar.classify_page(base), "retry")
        base.body = "Please verify you are human"
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

    def test_disabled_and_japanese_challenge_text(self):
        self.assertEqual(sidecar.classify_page(sidecar.Signals(body="账号已停用")), "disabled")
        self.assertEqual(sidecar.classify_page(sidecar.Signals(body="人間であることを確認")), "challenge")

    def test_ready_url_requires_clean_chat_page(self):
        self.assertTrue(sidecar.ready_url("https://chatgpt.com/", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://chatgpt.com/auth/login", sidecar.Signals()))
        self.assertFalse(sidecar.ready_url("https://example.test/", sidecar.Signals()))


class SanitizingAndMessageTests(unittest.TestCase):
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


class VerificationFlowTests(unittest.IsolatedAsyncioTestCase):
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
        with mock.patch.object(flow_module, "type_value", new=mock.AsyncMock()), mock.patch.object(
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
        with mock.patch.object(flow_module, "type_value", new=mock.AsyncMock()), mock.patch.object(
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


if __name__ == "__main__":
    unittest.main()
