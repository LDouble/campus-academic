package ouc

import "testing"

func TestParseLoginResponseError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		wantFound bool
		wantCode  int
		wantMsg   string
		wantErr   bool
	}{
		{
			name:      "password expired",
			body:      `<script>var error = {"code":40605,"msg":"密码已过期"};</script>`,
			wantFound: true,
			wantCode:  oucPasswordExpiredCode,
			wantMsg:   "密码已过期",
		},
		{
			name:      "other upstream error",
			body:      `<script>var error = {"code":40001,"msg":"登录失败"};</script>`,
			wantFound: true,
			wantCode:  40001,
			wantMsg:   "登录失败",
		},
		{
			name:      "unicode escaped message",
			body:      `<script>var error = {"code":40400,"msg":"\u8D26\u53F7\u6216\u5BC6\u7801\u9519\u8BEF"};</script>`,
			wantFound: true,
			wantCode:  40400,
			wantMsg:   "账号或密码错误",
		},
		{
			name: "no upstream error",
			body: `<script>var error = null;</script>`,
		},
		{
			name: "marker absent",
			body: `<form method="post"></form>`,
		},
		{
			name:    "malformed metadata",
			body:    `<script>var error = {;</script>`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			responseError, found, err := parseLoginResponseError([]byte(test.body))
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, test.wantErr)
			}
			if found != test.wantFound ||
				responseError.Code != test.wantCode ||
				responseError.Msg != test.wantMsg {
				t.Fatalf(
					"responseError=%+v found=%t wantCode=%d wantMsg=%q wantFound=%t",
					responseError,
					found,
					test.wantCode,
					test.wantMsg,
					test.wantFound,
				)
			}
		})
	}
}

func TestClassifyLoginResponseError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  string
		want loginResponseErrorKind
	}{
		{name: "invalid credentials", msg: "账号或密码错误", want: loginResponseErrorInvalidCredentials},
		{name: "password expired", msg: "您的密码已过期建议立即修改", want: loginResponseErrorPasswordExpired},
		{name: "captcha", msg: "请输入验证码", want: loginResponseErrorChallengeRequired},
		{name: "validation code", msg: "需要校验码", want: loginResponseErrorChallengeRequired},
		{name: "device confirmation", msg: "需要完成设备确认", want: loginResponseErrorChallengeRequired},
		{name: "account locked", msg: "账号已锁定", want: loginResponseErrorAccountRestricted},
		{name: "account locked after password failures", msg: "密码错误次数过多，账号已锁定", want: loginResponseErrorAccountRestricted},
		{name: "account frozen", msg: "当前账号已冻结", want: loginResponseErrorAccountRestricted},
		{name: "unknown", msg: "认证失败", want: loginResponseErrorUnknown},
		{name: "empty", msg: "  ", want: loginResponseErrorUnknown},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := classifyLoginResponseError(loginResponseError{Code: 1, Msg: test.msg})
			if got != test.want {
				t.Fatalf("classifyLoginResponseError(%q)=%q want=%q", test.msg, got, test.want)
			}
		})
	}
}

func TestParseLoginResponsePageName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		want      string
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "password reset warning",
			body:      `<script>var pageName = "resetWarn";</script>`,
			want:      "resetWarn",
			wantFound: true,
		},
		{
			name: "marker absent",
			body: `<form method="post"></form>`,
		},
		{
			name:    "malformed metadata",
			body:    `<script>var pageName = resetWarn;</script>`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pageName, found, err := parseLoginResponsePageName(
				[]byte(test.body),
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, test.wantErr)
			}
			if pageName != test.want || found != test.wantFound {
				t.Fatalf(
					"pageName=%q found=%t want=%q wantFound=%t",
					pageName,
					found,
					test.want,
					test.wantFound,
				)
			}
		})
	}
}
