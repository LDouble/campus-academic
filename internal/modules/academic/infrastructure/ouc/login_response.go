package ouc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const oucPasswordExpiredCode = 40605

var (
	loginResponseErrorMarker    = []byte("var error =")
	loginResponsePageNameMarker = []byte("var pageName =")
)

type loginResponseError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type loginResponseErrorKind string

const (
	loginResponseErrorUnknown            loginResponseErrorKind = "unknown"
	loginResponseErrorInvalidCredentials loginResponseErrorKind = "invalid_credentials"
	loginResponseErrorPasswordExpired    loginResponseErrorKind = "password_expired"
	loginResponseErrorChallengeRequired  loginResponseErrorKind = "challenge_required"
	loginResponseErrorAccountRestricted  loginResponseErrorKind = "account_restricted"
)

var (
	loginChallengeText    = []string{"验证码", "校验码", "滑动验证", "设备确认"}
	accountRestrictedText = []string{"锁定", "冻结"}
)

func parseLoginResponseError(body []byte) (loginResponseError, bool, error) {
	index := bytes.Index(body, loginResponseErrorMarker)
	if index < 0 {
		return loginResponseError{}, false, nil
	}
	reader := bytes.NewReader(body[index+len(loginResponseErrorMarker):])
	var responseError *loginResponseError
	if err := json.NewDecoder(reader).Decode(&responseError); err != nil {
		return loginResponseError{}, false, fmt.Errorf(
			"decode OUC login response error: %w",
			err,
		)
	}
	if responseError == nil {
		return loginResponseError{}, false, nil
	}
	return *responseError, true, nil
}

func classifyLoginResponseError(responseError loginResponseError) loginResponseErrorKind {
	message := []byte(strings.TrimSpace(responseError.Msg))
	if len(message) == 0 {
		return loginResponseErrorUnknown
	}
	switch {
	case containsAny(message, passwordExpiredText):
		return loginResponseErrorPasswordExpired
	case containsAny(message, accountRestrictedText):
		return loginResponseErrorAccountRestricted
	case containsAny(message, loginChallengeText):
		return loginResponseErrorChallengeRequired
	case containsAny(message, invalidLoginText):
		return loginResponseErrorInvalidCredentials
	default:
		return loginResponseErrorUnknown
	}
}

func parseLoginResponsePageName(body []byte) (string, bool, error) {
	index := bytes.Index(body, loginResponsePageNameMarker)
	if index < 0 {
		return "", false, nil
	}
	reader := bytes.NewReader(body[index+len(loginResponsePageNameMarker):])
	var pageName string
	if err := json.NewDecoder(reader).Decode(&pageName); err != nil {
		return "", false, fmt.Errorf(
			"decode OUC login response page name: %w",
			err,
		)
	}
	return pageName, true, nil
}
