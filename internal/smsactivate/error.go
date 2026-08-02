package smsactivate

import (
	"errors"
	"fmt"
	"strings"
)

const CodeConfig = "CONFIG_ERROR"

type Error struct {
	Code    string
	Message string
	cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func ErrorCode(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return ErrorCode(err) == code
}

var errorMessages = map[string]string{
	"BAD_KEY":              "API Key 无效",
	"BAD_ACTION":           "接口 action 错误",
	"BAD_SERVICE":          "服务代码错误",
	"NO_NUMBERS":           "该国家暂无 OpenAI 可用号码",
	"NO_BALANCE":           "余额不足，请充值",
	"NO_ACTIVATION":        "激活 ID 无效或已结束",
	"WRONG_ACTIVATION_ID":  "激活 ID 无效或已结束",
	"WRONG_MAX_PRICE":      "最高单价设置过低",
	"BANNED":               "接码平台账号已被封禁",
	"EARLY_CANCEL_DENIED":  "购买后暂时不能取消",
	"BAD_STATUS":           "激活状态参数错误",
	"CHANNELS_LIMIT":       "并发激活数已达上限",
	"ACCOUNT_INACTIVE":     "接码平台账号未激活",
	"ORDER_ALREADY_EXISTS": "OpenAI 已有活动订单",
	"NO_ACTIVATIONS":       "没有活动中的激活",
}

func configError(message string) error {
	return &Error{Code: CodeConfig, Message: message}
}

func codedError(code, details string) error {
	code = strings.TrimSpace(code)
	message, ok := errorMessages[code]
	if !ok {
		message = strings.TrimSpace(details)
		if message == "" {
			message = code
		}
		if message == "" {
			message = "接码平台返回未知错误"
		}
	}
	return &Error{Code: code, Message: message}
}

func responseError(operation, response string) error {
	response = strings.TrimSpace(response)
	return &Error{
		Code:    "UNEXPECTED_RESPONSE",
		Message: fmt.Sprintf("%s 返回格式异常：%s", operation, response),
	}
}
