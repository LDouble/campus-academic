package ouc

import "testing"

func TestDetectInteractiveChallengeIgnoresInactiveSourceText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		body         string
		wantDetected bool
		wantRule     string
	}{
		{
			name:     "JSON value",
			body:     `{"message":"请输入验证码","items":[]}`,
			wantRule: challengeRuleStructuredResponse,
		},
		{
			name:     "script template",
			body:     `<form><input name="username"><input name="password"></form><script>const prompt = "请输入验证码"</script>`,
			wantRule: challengeRuleNone,
		},
		{
			name:     "hidden instruction",
			body:     `<div style="display: none">请完成验证码</div>`,
			wantRule: challengeRuleNone,
		},
		{
			name:     "generic security copy on login page",
			body:     `<form><input name="username"><input name="password"><input type="hidden" name="captcha"></form><p>安全验证</p>`,
			wantRule: challengeRuleNone,
		},
		{
			name:         "visible captcha input",
			body:         `<form><input name="username"><input name="captcha"></form>`,
			wantDetected: true,
			wantRule:     challengeRuleVisibleControl,
		},
		{
			name:         "visible instruction",
			body:         `<p>请完成验证码后继续</p>`,
			wantDetected: true,
			wantRule:     challengeRuleVisibleInstruction,
		},
		{
			name:         "standalone security validation",
			body:         `<main><h1>安全验证</h1><p>请确认当前设备</p></main>`,
			wantDetected: true,
			wantRule:     challengeRuleStandaloneValidation,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := detectInteractiveChallenge([]byte(test.body))
			if got.Detected != test.wantDetected || got.Rule != test.wantRule {
				t.Fatalf(
					"detection=%+v want detected=%v rule=%q",
					got,
					test.wantDetected,
					test.wantRule,
				)
			}
		})
	}
}
