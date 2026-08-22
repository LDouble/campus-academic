package ouc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
)

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

func TestObservedResponseBodyPreservesContextTermination(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		err     error
		outcome string
	}{
		{name: "canceled", err: context.Canceled, outcome: "canceled"},
		{name: "deadline", err: context.DeadlineExceeded, outcome: "deadline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got string
			body := &observedResponseBody{
				ReadCloser: errorReadCloser{err: test.err},
				statusCode: http.StatusOK,
				observe:    func(outcome string) { got = outcome },
			}
			if _, err := body.Read(make([]byte, 1)); !errors.Is(err, test.err) {
				t.Fatalf("read error=%v want=%v", err, test.err)
			}
			if got != test.outcome {
				t.Fatalf("observed outcome=%q want=%q", got, test.outcome)
			}
		})
	}
}

func TestNewOUCTransportSetsConnectionPoolLimits(t *testing.T) {
	t.Parallel()
	transport, err := newOUCTransport("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	if transport.MaxConnsPerHost != oucMaxConnsPerHost ||
		transport.MaxIdleConnsPerHost != oucMaxIdleConnsPerHost ||
		transport.MaxIdleConns != oucMaxIdleConns ||
		transport.IdleConnTimeout != oucIdleConnTimeout ||
		transport.TLSHandshakeTimeout != oucTLSHandshakeTimeout ||
		!transport.ForceAttemptHTTP2 {
		t.Fatalf("unexpected OUC transport limits: %+v", transport)
	}
}

func TestInvalidLoginTextRequiresExplicitCredentialMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		body string
		want bool
	}{
		{body: "用户名或密码错误", want: true},
		{body: "账号或密码不正确", want: true},
		{body: "密码错误", want: true},
		{body: "认证失败", want: false},
		{body: "统一身份认证暂时失败", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.body, func(t *testing.T) {
			t.Parallel()
			if got := containsAny([]byte(test.body), invalidLoginText); got != test.want {
				t.Fatalf("containsAny(%q)=%v want=%v", test.body, got, test.want)
			}
		})
	}
}

func TestClassifyServiceAccess(t *testing.T) {
	t.Parallel()
	expected, err := url.Parse("https://jwgl2024.ouc.edu.cn/")
	if err != nil {
		t.Fatal(err)
	}
	ssoLogin, err := url.Parse("https://id.ouc.edu.cn/sso/login")
	if err != nil {
		t.Fatal(err)
	}
	portal, err := url.Parse("https://my.ouc.edu.cn/frontend/user/info")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		finalURL string
		body     string
		want     serviceAccessOutcome
	}{
		{name: "target granted", finalURL: expected.String(), body: "教务首页", want: serviceAccessGranted},
		{name: "target denied", finalURL: expected.String(), body: "不在本系统", want: serviceAccessTargetRejected},
		{name: "sso login", finalURL: ssoLogin.String(), want: serviceAccessIdentityRejected},
		{name: "information portal", finalURL: portal.String(), want: serviceAccessIdentityRejected},
		{name: "unexpected host", finalURL: "https://other.ouc.edu.cn/", want: serviceAccessTargetRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			finalURL, parseErr := url.Parse(test.finalURL)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if got := classifyServiceAccess(finalURL, expected, ssoLogin, portal, []byte(test.body)); got != test.want {
				t.Fatalf("outcome=%q, want %q", got, test.want)
			}
		})
	}
}

func TestProviderSessionClientsShareConnectionsWithoutSharingCookies(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Session", request.Header.Get("Cookie"))
		_, _ = writer.Write([]byte("ok"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	provider := NewProvider(staticConfigResolver{snapshot: academicconfig.Snapshot{
		ActiveProvider: academicconfig.ProviderOUC,
		OUC:            testOUCConfig(),
	}})
	t.Cleanup(func() { _ = provider.Close() })
	first, err := provider.newClient(testOUCConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.newClient(testOUCConfig())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Jar == second.Jar || first.Transport != second.Transport {
		t.Fatal("session clients must isolate cookie jars while sharing one transport")
	}
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	first.Jar.SetCookies(target, []*http.Cookie{{Name: "session", Value: "first"}})

	response, err := first.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("X-Session"); got != "session=first" {
		t.Fatalf("first client cookie=%q", got)
	}

	response, err = second.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("X-Session"); got != "" {
		t.Fatalf("second client unexpectedly received first client cookie: %q", got)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("connections=%d, want 1 shared keep-alive connection", got)
	}
}

func TestNewSessionClientWithProxy(t *testing.T) {
	t.Parallel()
	client, err := newSessionClientWithProxy(
		testOUCConfig(),
		"http://atrust-gateway:8888",
	)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.Transport)
	}
	request, err := http.NewRequest(http.MethodGet, "https://id.ouc.edu.cn/sso/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := transport.Proxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if proxy == nil || proxy.String() != "http://atrust-gateway:8888" {
		t.Fatalf("proxy=%v", proxy)
	}
}

func TestNewSessionClientRejectsUnsafeProxy(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"socks5://atrust-gateway:1080",
		"http://user:password@atrust-gateway:8888",
		"http://atrust-gateway:8888/path",
	} {
		if _, err := newSessionClientWithProxy(testOUCConfig(), raw); err == nil {
			t.Fatalf("proxy %q was accepted", raw)
		}
	}
}

func TestServiceLoginURLControlsNoAutoRedirect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		noAutoRedirect bool
		wantValue      string
	}{
		{name: "portal binding", noAutoRedirect: true, wantValue: "1"},
		{name: "academic service", noAutoRedirect: false, wantValue: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw, err := serviceLoginURL(
				"https://id.ouc.edu.cn/sso/login?noAutoRedirect=stale",
				"https://jwgl2024.ouc.edu.cn/",
				test.noAutoRedirect,
			)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsed.Query().Get("service"); got != "https://jwgl2024.ouc.edu.cn/" {
				t.Fatalf("service=%q", got)
			}
			if got := parsed.Query().Get("noAutoRedirect"); got != test.wantValue {
				t.Fatalf("noAutoRedirect=%q want=%q", got, test.wantValue)
			}
		})
	}
}

func TestParseLoginFormPreservesFlowAndDetectsInteractiveChallenge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		captchaInput  string
		wantChallenge bool
	}{
		{
			name:         "hidden captcha field",
			captchaInput: `<input type="hidden" name="captcha" value="">`,
		},
		{
			name:         "captcha field in hidden container",
			captchaInput: `<div hidden><input type="text" name="captcha" value=""></div>`,
		},
		{
			name:          "visible captcha field",
			captchaInput:  `<input type="text" name="captcha" value="">`,
			wantChallenge: true,
		},
	}
	base, err := url.Parse("https://id.ouc.edu.cn/sso/login?service=portal")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := `<form method="post">
				<input type="text" name="username">
				<input type="password" name="password">
				<input type="hidden" name="flowId" value="flow-test">
				` + test.captchaInput + `
			</form>`
			form, parseErr := parseLoginForm([]byte(body), base)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if got := form.values.Get("flowId"); got != "flow-test" {
				t.Fatalf("flowId=%q", got)
			}
			if form.hasChallenge != test.wantChallenge {
				t.Fatalf(
					"hasChallenge=%v want=%v",
					form.hasChallenge,
					test.wantChallenge,
				)
			}
			if !strings.HasPrefix(form.action, "https://id.ouc.edu.cn/") {
				t.Fatalf("form action=%q", form.action)
			}
		})
	}
}

func TestIsAcademicLoginPage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "undergraduate login action",
			body: `<html><form action="/jsxsd/xk/LoginToXk">
				<input type="text" name="userAccount">
				<input type="password" name="userPassword">
			</form></html>`,
			want: true,
		},
		{
			name: "login form name and password",
			body: `<html><form name="loginForm">
				<input type="password" name="password">
			</form></html>`,
			want: true,
		},
		{
			name: "login text alone is ordinary page",
			body: `<html><p>如需登录教务系统，请使用学校统一认证入口。</p></html>`,
			want: false,
		},
		{
			name: "login text and password input",
			body: `<html><p>登录教务</p><input type="password" name="password"></html>`,
			want: true,
		},
		{
			name: "ordinary academic form",
			body: `<html><form name="gradeFilter">
				<input type="text" name="kcmc">
			</form></html>`,
		},
		{
			name: "json response",
			body: `{"code":0,"data":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isAcademicLoginPage([]byte(test.body)); got != test.want {
				t.Fatalf("isAcademicLoginPage()=%v want=%v", got, test.want)
			}
		})
	}
}

func TestRewriteLegacySSORedirect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rawURL  string
		want    string
		rewrote bool
	}{
		{
			name: "graduate legacy SSO redirect",
			rawURL: "http://id.ouc.edu.cn:8071/sso/login?" +
				"service=https%3A%2F%2Fpgs.ouc.edu.cn%2Fpy%2Fpage%2Fstudent%2Fgrkcgl.htm",
			want: "https://id.ouc.edu.cn/sso/login?" +
				"service=https%3A%2F%2Fpgs.ouc.edu.cn%2Fpy%2Fpage%2Fstudent%2Fgrkcgl.htm",
			rewrote: true,
		},
		{
			name: "outside service is rejected",
			rawURL: "http://id.ouc.edu.cn:8071/sso/login?" +
				"service=https%3A%2F%2Fattacker.example%2Fcallback",
		},
		{
			name: "different HTTP endpoint is rejected",
			rawURL: "http://id.ouc.edu.cn:8071/other?" +
				"service=https%3A%2F%2Fpgs.ouc.edu.cn%2F",
		},
		{
			name: "HTTPS redirect is unchanged",
			rawURL: "https://id.ouc.edu.cn/sso/login?" +
				"service=https%3A%2F%2Fpgs.ouc.edu.cn%2F",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target, err := url.Parse(test.rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := rewriteLegacySSORedirect(target); got != test.rewrote {
				t.Fatalf("rewrote=%v want=%v", got, test.rewrote)
			}
			if test.rewrote && target.String() != test.want {
				t.Fatalf("URL=%q want=%q", target.String(), test.want)
			}
		})
	}
}
