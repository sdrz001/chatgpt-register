# 独立 Authenticator 2FA 模块

这是从项目账号列表 `2FA / 2FA✓` 功能中拆出的纯 HTTP 版本，不依赖原项目的
Flask、SQLite、页面或 `botcore`。

## 功能

- 查询当前 MFA 状态；
- 创建 TOTP enrollment；
- 本地生成 RFC 6238 验证码并激活；
- 再次查询 `mfa_info` 确认服务端已经启用；
- 返回 TOTP secret、`otpauth://` URI、factor ID，以及响应中存在的恢复码。

## 输入

需要同一登录会话内保存的：

- ChatGPT Web `access_token`；
- NextAuth `session_token` 或完整 cookies；
- 对应的 `device_id`（缺省时自动生成）；
- 可选代理。

复制 `account.example.json` 后填入自己的数据。该文件以及输出文件含登录凭据和
TOTP secret，传输时应单独保管。

## 运行

```powershell
python -m pip install -r requirements.txt
Copy-Item account.example.json account.json
python enable_2fa.py --account account.json --output mfa_result.json
```

## 作为模块调用

```python
import asyncio
from openai_2fa import enable_authenticator, totp_code

result = asyncio.run(enable_authenticator(
    email="ACCOUNT@example.com",
    access_token="ACCESS_TOKEN",
    session_token="SESSION_TOKEN",
    cookies=[],
    device_id="DEVICE_ID",
    proxy="",
))

print(result["enabled"])
print(result["secret"])
print(totp_code(result["secret"]))
```

## 接口顺序

```text
GET  /backend-api/accounts/mfa_info
POST /backend-api/accounts/mfa/enroll
POST /backend-api/accounts/mfa/user/activate_enrollment
GET  /backend-api/accounts/mfa_info
```

已有 2FA 时只返回状态和 factor ID，不重复 enrollment。
