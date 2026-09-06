from __future__ import annotations

import unittest

import httpx

from openai_2fa import enable_authenticator, totp_code


class StandaloneTwoFactorTests(unittest.IsolatedAsyncioTestCase):
    def test_rfc6238_sha1_vector(self):
        secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
        self.assertEqual(totp_code(secret, at_time=59, digits=8), "94287082")

    async def test_enroll_activate_verify_sequence(self):
        calls: list[httpx.Request] = []
        replies = iter(
            [
                {"mfa_enabled": False},
                {
                    "secret": "JBSWY3DPEHPK3PXP",
                    "session_id": "SESSION",
                    "factor": {"id": "FACTOR"},
                },
                {"success": True, "recovery_codes": ["RECOVERY-1"]},
                {"mfa_enabled": True, "native_default_factor_id": "FACTOR"},
            ]
        )

        def handler(request: httpx.Request) -> httpx.Response:
            calls.append(request)
            return httpx.Response(200, json=next(replies), request=request)

        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            result = await enable_authenticator(
                email="ACCOUNT@example.com",
                access_token="ACCESS_TOKEN",
                session_token="SESSION_TOKEN",
                device_id="DEVICE_ID",
                client=client,
                code_factory=lambda _secret: "123456",
            )

        self.assertTrue(result["enabled"])
        self.assertEqual(result["secret"], "JBSWY3DPEHPK3PXP")
        self.assertEqual(result["recovery_codes"], ["RECOVERY-1"])
        self.assertEqual(
            [(request.method, request.url.path) for request in calls],
            [
                ("GET", "/backend-api/accounts/mfa_info"),
                ("POST", "/backend-api/accounts/mfa/enroll"),
                ("POST", "/backend-api/accounts/mfa/user/activate_enrollment"),
                ("GET", "/backend-api/accounts/mfa_info"),
            ],
        )
        self.assertEqual(calls[1].headers["authorization"], "Bearer ACCESS_TOKEN")
        self.assertEqual(calls[1].headers["oai-device-id"], "DEVICE_ID")

    async def test_already_enabled_stops_before_enroll(self):
        count = 0

        def handler(request: httpx.Request) -> httpx.Response:
            nonlocal count
            count += 1
            return httpx.Response(
                200,
                json={"mfa_enabled": True, "native_default_factor_id": "EXISTING"},
                request=request,
            )

        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            result = await enable_authenticator(
                email="ACCOUNT@example.com",
                access_token="ACCESS_TOKEN",
                session_token="SESSION_TOKEN",
                client=client,
            )
        self.assertTrue(result["already_enabled"])
        self.assertEqual(count, 1)


if __name__ == "__main__":
    unittest.main()
