package ouc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/weouc-plus/campus-academic/internal/modules/academic/application"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/weouc-plus/campus-academic/internal/modules/academic_verification/application"
	"go.uber.org/zap"
	"golang.org/x/net/html"
)

var (
	errOUCHTRedirectPolicy = errors.New("OUC redirect policy rejected")
	invalidLoginText       = []string{
		"用户名或密码错误", "账号或密码错误", "用户名或密码不正确", "账号或密码不正确",
		"用户名或密码有误", "账号或密码有误", "密码错误",
	}
	passwordExpiredText = []string{
		"密码已过期", "密码已经过期", "密码过期", "密码已失效",
	}
	rejectedAppText = []string{"无权访问", "没有权限", "未授权", "用户不存在", "不在本系统", "非本系统用户"}
)

type session struct {
	client *http.Client
	page   []byte
}

type sessionClientFactory func(academicconfig.OUCConfig) (*http.Client, error)

const (
	oucMaxConnsPerHost       = 20
	oucMaxIdleConnsPerHost   = 20
	oucMaxIdleConns          = 60
	oucIdleConnTimeout       = 45 * time.Second
	oucTLSHandshakeTimeout   = 10 * time.Second
	oucAuthenticationTimeout = 10 * time.Second
	oucSSOStageTimeout       = 2 * time.Second
)

type loginForm struct {
	action                    string
	values                    url.Values
	hasChallenge              bool
	hasPasswordExpiryContinue bool
}

func newSessionClient(config academicconfig.OUCConfig) (*http.Client, error) {
	return newSessionClientWithProxy(config, "")
}

func newSessionClientWithProxy(config academicconfig.OUCConfig, proxyURL string) (*http.Client, error) {
	transport, err := newOUCTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return newSessionClientWithTransport(config, transport)
}

// newOUCTransport creates the dedicated connection pool used for OUC traffic.
// The returned transport is safe for concurrent use and must be shared by
// session clients created by a Provider. Session state stays on each client's
// independent cookie jar, never on the shared transport.
func newOUCTransport(proxyURL string) (*http.Transport, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is not a *http.Transport")
	}
	transport = transport.Clone()
	transport.MaxConnsPerHost = oucMaxConnsPerHost
	transport.MaxIdleConnsPerHost = oucMaxIdleConnsPerHost
	transport.MaxIdleConns = oucMaxIdleConns
	transport.IdleConnTimeout = oucIdleConnTimeout
	transport.TLSHandshakeTimeout = oucTLSHandshakeTimeout
	transport.ForceAttemptHTTP2 = true
	if strings.TrimSpace(proxyURL) == "" {
		return transport, nil
	}
	parsedProxy, err := parseOUCHTTPProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	transport.Proxy = http.ProxyURL(parsedProxy)
	return transport, nil
}

func parseOUCHTTPProxy(proxyURL string) (*url.URL, error) {
	parsedProxy, err := url.Parse(proxyURL)
	if err != nil || parsedProxy.Hostname() == "" ||
		(parsedProxy.Scheme != "http" && parsedProxy.Scheme != "https") ||
		parsedProxy.User != nil || parsedProxy.Path != "" && parsedProxy.Path != "/" ||
		parsedProxy.RawQuery != "" || parsedProxy.Fragment != "" {
		return nil, errors.New("invalid OUC HTTP proxy URL")
	}
	return parsedProxy, nil
}

func newSessionClientWithTransport(
	config academicconfig.OUCConfig,
	transport *http.Transport,
) (*http.Client, error) {
	if transport == nil {
		return nil, errors.New("OUC HTTP transport is required")
	}
	baseJar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create OUC cookie jar: %w", err)
	}
	jar := newTrackedCookieJar(baseJar)
	return &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   config.RequestTimeout(),
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("%w: too many redirects", errOUCHTRedirectPolicy)
			}
			// A normal 301/302/303 form hand-off is converted to GET by
			// net/http and is required by the OUC SSO flow. Refuse only
			// 307/308, which would replay the password-bearing POST.
			if len(via) > 0 && via[len(via)-1].Method == http.MethodPost &&
				via[len(via)-1].Response != nil && via[len(via)-1].Response.StatusCode >= 307 {
				return http.ErrUseLastResponse
			}
			if rewriteLegacySSORedirect(request.URL) {
				return nil
			}
			if request.URL.Scheme != "https" || !allowedOUCHost(request.URL.Hostname()) {
				return fmt.Errorf("%w: blocked redirect to %s", errOUCHTRedirectPolicy, request.URL.Redacted())
			}
			return nil
		},
	}, nil
}

func rewriteLegacySSORedirect(target *url.URL) bool {
	if target == nil ||
		target.Scheme != "http" ||
		!strings.EqualFold(target.Hostname(), "id.ouc.edu.cn") ||
		target.Port() != "8071" ||
		!strings.EqualFold(target.EscapedPath(), "/sso/login") ||
		target.User != nil {
		return false
	}
	serviceValues, exists := target.Query()["service"]
	if !exists || len(serviceValues) != 1 {
		return false
	}
	service, err := url.Parse(serviceValues[0])
	if err != nil ||
		service.Scheme != "https" ||
		service.User != nil ||
		!allowedOUCHost(service.Hostname()) {
		return false
	}
	target.Scheme = "https"
	target.Host = "id.ouc.edu.cn"
	return true
}

func authenticate(
	ctx context.Context,
	config academicconfig.OUCConfig,
	studentNo string,
	password string,
	clientFactory sessionClientFactory,
	trace *processTrace,
) (*session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	authContext, authCancel := context.WithTimeout(ctx, oucAuthenticationTimeout)
	defer authCancel()
	if clientFactory == nil {
		clientFactory = newSessionClient
	}
	client, err := clientFactory(config)
	if err != nil {
		return nil, err
	}
	trace.step("sso.client.ready")
	loginURL, err := serviceLoginURL(
		config.SSOLoginURL,
		config.PortalServiceURL,
		config.PortalNoRedirect,
	)
	if err != nil {
		return nil, err
	}
	pageContext, pageCancel := context.WithTimeout(authContext, oucSSOStageTimeout)
	pageRedirected := false
	finalURL, body, err := requestPageWithOptions(
		pageContext,
		client,
		http.MethodGet,
		loginURL,
		nil,
		"",
		config,
		trace,
		requestPageOptions{
			phase:                   "sso_login_page",
			stopRedirectOutsideHost: "id.ouc.edu.cn",
			redirected:              &pageRedirected,
		},
	)
	pageCancel()
	if err != nil {
		return nil, err
	}
	if pageRedirected {
		if finalURL == nil || finalURL.Scheme != "https" || !allowedOUCHost(finalURL.Hostname()) {
			return nil, fmt.Errorf("invalid OUC existing-session handoff URL")
		}
		finalURL, body, err = requestPageWithOptions(
			authContext,
			client,
			http.MethodGet,
			finalURL.String(),
			nil,
			"",
			config,
			trace,
			requestPageOptions{phase: "target_handoff"},
		)
		if err != nil {
			return nil, err
		}
	}
	if finalURL.Hostname() != "id.ouc.edu.cn" {
		trace.step(
			"sso.login.result",
			zap.String("outcome", "existing_session"),
		)
		return &session{client: client, page: body}, nil
	}
	form, err := parseLoginForm(body, finalURL)
	if err != nil {
		trace.step(
			"sso.login_form.parsed",
			zap.String("outcome", "invalid_form"),
		)
		return nil, err
	}
	security, err := parseLoginSecurityConfig(body)
	if err != nil {
		trace.step(
			"sso.security_config.parsed",
			zap.String("outcome", "invalid_config"),
		)
		return nil, err
	}
	trace.step(
		"sso.login_form.parsed",
		zap.String("outcome", "success"),
		zap.Bool("visible_challenge_control", form.hasChallenge),
		zap.Bool("sm2_enabled", security.SM2.Enabled),
	)
	if form.hasChallenge {
		trace.step(
			"sso.challenge.decision",
			zap.Bool("detected", true),
			zap.String("rule", challengeRuleVisibleControl),
		)
		return nil, verificationapp.ErrChallengeRequired
	}
	submittedPassword := password
	if security.SM2.Enabled {
		submittedPassword, err = encryptSM2Password(
			password,
			security.SM2.PublicKey,
		)
		if err != nil {
			return nil, err
		}
	}
	form.values.Set("username", studentNo)
	form.values.Set("password", submittedPassword)
	trace.step(
		"sso.credentials.prepared",
		zap.Bool("sm2_enabled", security.SM2.Enabled),
	)
	if form.values.Get("loginType") == "" {
		form.values.Set("loginType", "username_password")
	}
	actionURL, err := finalURL.Parse(form.action)
	if err != nil || actionURL.Scheme != "https" || actionURL.Hostname() != "id.ouc.edu.cn" {
		return nil, fmt.Errorf("invalid OUC login form action")
	}
	postContext, postCancel := context.WithTimeout(authContext, oucSSOStageTimeout)
	postRedirected := false
	finalURL, body, err = requestPageWithOptions(
		postContext,
		client,
		http.MethodPost,
		actionURL.String(),
		strings.NewReader(form.values.Encode()),
		"application/x-www-form-urlencoded",
		config,
		trace,
		requestPageOptions{
			phase:            "sso_login_submit",
			stopPostRedirect: true,
			redirected:       &postRedirected,
		},
	)
	postCancel()
	form.values.Set("password", "")
	if err != nil {
		return nil, err
	}
	handoffURL := finalURL
	if postRedirected && handoffURL != nil {
		if handoffURL.Scheme != "https" || !allowedOUCHost(handoffURL.Hostname()) {
			return nil, fmt.Errorf("invalid OUC login handoff URL")
		}
		finalURL, body, err = requestPageWithOptions(
			authContext,
			client,
			http.MethodGet,
			handoffURL.String(),
			nil,
			"",
			config,
			trace,
			requestPageOptions{phase: "target_handoff"},
		)
		if err != nil {
			return nil, err
		}
	}
	responseError, hasResponseError, err := parseLoginResponseError(body)
	if err != nil {
		trace.step(
			"sso.login_response.parsed",
			zap.String("outcome", "invalid_error_metadata"),
		)
		// Error metadata is optional upstream presentation data. Keep
		// classifying the page by its stable text and final URL so a format
		// change cannot hide a credential error behind a generic 503.
		hasResponseError = false
	}
	if hasResponseError {
		trace.recordLoginResponseError("upstream_error", responseError.Code, responseError.Msg)
	}
	if responseError.Code == oucPasswordExpiredCode || containsAny(body, passwordExpiredText) {
		var continued bool
		finalURL, body, continued, err = continuePasswordExpiryWarning(
			authContext,
			client,
			finalURL,
			body,
			config,
			trace,
		)
		if err != nil {
			return nil, err
		}
		if !continued {
			trace.step(
				"sso.login.result",
				zap.String("outcome", "password_expired"),
			)
			return nil, verificationapp.ErrPasswordExpired
		}
		responseError, hasResponseError, err = parseLoginResponseError(body)
		if err != nil {
			trace.step(
				"sso.login_response.parsed",
				zap.String("outcome", "invalid_error_metadata"),
			)
			hasResponseError = false
		}
		if hasResponseError {
			trace.recordLoginResponseError(
				"upstream_error_after_continue",
				responseError.Code,
				responseError.Msg,
			)
			if responseError.Code == oucPasswordExpiredCode {
				trace.step(
					"sso.login.result",
					zap.String("outcome", "password_expired"),
				)
				return nil, verificationapp.ErrPasswordExpired
			}
		}
		if containsAny(body, passwordExpiredText) {
			trace.step(
				"sso.login.result",
				zap.String("outcome", "password_expired"),
			)
			return nil, verificationapp.ErrPasswordExpired
		}
	}
	if hasResponseError {
		switch classifyLoginResponseError(responseError) {
		case loginResponseErrorInvalidCredentials:
			trace.step(
				"sso.login.result",
				zap.String("outcome", "invalid_credentials"),
				zap.String("rule", "upstream_message"),
			)
			return nil, verificationapp.ErrInvalidCredentials
		case loginResponseErrorPasswordExpired:
			trace.step(
				"sso.login.result",
				zap.String("outcome", "password_expired"),
				zap.String("rule", "upstream_message"),
			)
			return nil, verificationapp.ErrPasswordExpired
		case loginResponseErrorChallengeRequired:
			trace.step(
				"sso.login.result",
				zap.String("outcome", "challenge_required"),
				zap.String("rule", "upstream_message"),
			)
			return nil, verificationapp.ErrChallengeRequired
		case loginResponseErrorAccountRestricted:
			trace.step(
				"sso.login.result",
				zap.String("outcome", "account_restricted"),
				zap.String("rule", "upstream_message"),
			)
			return nil, verificationapp.ErrAccountRestricted
		}
	}
	if containsAny(body, invalidLoginText) {
		trace.step(
			"sso.login.result",
			zap.String("outcome", "invalid_credentials"),
			zap.String("rule", "invalid_credentials_text"),
		)
		return nil, verificationapp.ErrInvalidCredentials
	}
	detection := detectInteractiveChallenge(body)
	trace.step(
		"sso.challenge.decision",
		zap.Bool("detected", detection.Detected),
		zap.String("rule", detection.Rule),
	)
	if finalURL.Hostname() == "id.ouc.edu.cn" {
		if detection.Detected {
			return nil, verificationapp.ErrChallengeRequired
		}
		trace.step(
			"sso.login.result",
			zap.String("outcome", "authentication_not_completed"),
		)
		// Remaining on the SSO host only proves that authentication did not
		// complete. Without an explicit upstream credential message this may be
		// a changed login flow or another provider failure, so keep it retryable.
		return nil, verificationapp.ErrProviderUnavailable
	}
	if containsAny(body, rejectedAppText) {
		trace.step(
			"sso.login.result",
			zap.String("outcome", "identity_unresolved"),
		)
		return nil, verificationapp.ErrIdentityTypeUnresolved
	}
	trace.step("sso.login.result", zap.String("outcome", "success"))
	return &session{client: client, page: body}, nil
}

func continuePasswordExpiryWarning(
	ctx context.Context,
	client *http.Client,
	currentURL *url.URL,
	body []byte,
	config academicconfig.OUCConfig,
	trace *processTrace,
) (*url.URL, []byte, bool, error) {
	pageName, found, err := parseLoginResponsePageName(body)
	if err != nil {
		trace.step(
			"sso.password_expiry_continue.decision",
			zap.String("outcome", "invalid_page_metadata"),
		)
		// The upstream error code already proved that the password expired.
		// Invalid optional continuation metadata must not erase that precise
		// result and turn it into a generic provider failure.
		return currentURL, body, false, nil
	}
	if !found || pageName != "resetWarn" {
		trace.step(
			"sso.password_expiry_continue.decision",
			zap.String("outcome", "not_offered"),
		)
		return currentURL, body, false, nil
	}
	form, err := parseLoginForm(body, currentURL)
	if err != nil {
		trace.step(
			"sso.password_expiry_continue.decision",
			zap.String("outcome", "invalid_form"),
		)
		return currentURL, body, false, nil
	}
	if !form.hasPasswordExpiryContinue {
		trace.step(
			"sso.password_expiry_continue.decision",
			zap.String("outcome", "continue_control_absent"),
		)
		return currentURL, body, false, nil
	}
	actionURL, err := currentURL.Parse(form.action)
	if err != nil ||
		actionURL.Scheme != "https" ||
		actionURL.Hostname() != "id.ouc.edu.cn" {
		trace.step(
			"sso.password_expiry_continue.decision",
			zap.String("outcome", "invalid_action"),
		)
		return currentURL, body, false, nil
	}
	// This mirrors the university resetWarn page's “继续” handler. The SSO
	// flow already holds the verified credential state, so the follow-up must
	// carry only the refreshed flow form and continue=1—not the password.
	form.values.Set("username", "")
	form.values.Set("password", "")
	form.values.Set("loginType", "")
	form.values.Set("continue", "1")
	trace.step(
		"sso.password_expiry_continue.decision",
		zap.String("outcome", "submit"),
	)
	finalURL, continuedBody, err := requestPageWithOptions(
		ctx,
		client,
		http.MethodPost,
		actionURL.String(),
		strings.NewReader(form.values.Encode()),
		"application/x-www-form-urlencoded",
		config,
		trace,
		requestPageOptions{phase: "sso_continue"},
	)
	if err != nil {
		return currentURL, body, true, err
	}
	trace.step(
		"sso.password_expiry_continue.result",
		zap.String("outcome", "completed"),
	)
	return finalURL, continuedBody, true, nil
}

type serviceAccessOutcome string

const (
	serviceAccessGranted          serviceAccessOutcome = "granted"
	serviceAccessIdentityRejected serviceAccessOutcome = "identity_rejected"
	serviceAccessTargetRejected   serviceAccessOutcome = "target_rejected"
)

func accessService(
	ctx context.Context,
	current *session,
	config academicconfig.OUCConfig,
	serviceURL string,
	service string,
	trace *processTrace,
) ([]byte, serviceAccessOutcome, error) {
	loginURL, err := serviceLoginURL(config.SSOLoginURL, serviceURL, false)
	if err != nil {
		return nil, serviceAccessTargetRejected, err
	}
	finalURL, body, err := requestPageWithOptions(
		ctx,
		current.client,
		http.MethodGet,
		loginURL,
		nil,
		"",
		config,
		trace,
		requestPageOptions{phase: "target_handoff"},
	)
	if err != nil {
		return nil, serviceAccessTargetRejected, err
	}
	expected, _ := url.Parse(serviceURL)
	ssoLogin, _ := url.Parse(config.SSOLoginURL)
	portal, _ := url.Parse(config.PortalServiceURL)
	outcome := classifyServiceAccess(finalURL, expected, ssoLogin, portal, body)
	trace.step(
		"service.probe.result",
		zap.String("service", service),
		zap.Bool("accessible", outcome == serviceAccessGranted),
		zap.String("outcome", string(outcome)),
	)
	return body, outcome, nil
}

func classifyServiceAccess(
	finalURL *url.URL,
	expected *url.URL,
	ssoLogin *url.URL,
	portal *url.URL,
	body []byte,
) serviceAccessOutcome {
	if finalURL == nil || finalURL.Scheme != "https" {
		return serviceAccessTargetRejected
	}
	finalHost := finalURL.Hostname()
	if expected != nil && finalHost == expected.Hostname() {
		if containsAny(body, rejectedAppText) {
			return serviceAccessTargetRejected
		}
		return serviceAccessGranted
	}
	if (ssoLogin != nil && finalHost == ssoLogin.Hostname()) ||
		(portal != nil && finalHost == portal.Hostname()) {
		return serviceAccessIdentityRejected
	}
	return serviceAccessTargetRejected
}

func isAcademicLoginPage(body []byte) bool {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	detected := false
	hasPasswordInput := false
	hasLoginText := strings.Contains(string(body), "登录教务")
	walk(root, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		if node.Data == "input" {
			for _, attribute := range node.Attr {
				if strings.EqualFold(attribute.Key, "type") && strings.EqualFold(attribute.Val, "password") {
					hasPasswordInput = true
				}
			}
		}
		if detected || node.Data != "form" {
			return
		}
		loginFormName := false
		loginAction := false
		for _, attribute := range node.Attr {
			key := strings.ToLower(attribute.Key)
			value := strings.TrimSpace(attribute.Val)
			switch key {
			case "id", "name":
				loginFormName = strings.EqualFold(value, "loginForm")
			case "action":
				parsed, parseErr := url.Parse(value)
				loginAction = parseErr == nil &&
					strings.EqualFold(parsed.Path, "/jsxsd/xk/LoginToXk")
			}
		}
		hasPassword := false
		walk(node, func(descendant *html.Node) {
			if descendant.Type != html.ElementNode || descendant.Data != "input" {
				return
			}
			for _, attribute := range descendant.Attr {
				if strings.EqualFold(attribute.Key, "type") &&
					strings.EqualFold(attribute.Val, "password") {
					hasPassword = true
				}
			}
		})
		detected = loginAction || (loginFormName && hasPassword)
	})
	return detected || (hasLoginText && hasPasswordInput)
}

// probeUndergraduateSession checks the stable undergraduate shell directly.
// It never goes through CAS: a redirect or login document is therefore a
// precise target-cookie rejection, while request failures remain indeterminate.
func probeUndergraduateSession(
	ctx context.Context,
	current *session,
	config academicconfig.OUCConfig,
	trace *processTrace,
) (bool, error) {
	target, err := academicProfileURL(config.Undergraduate.ServiceURL, config.UndergraduateProbePath())
	if err != nil {
		trace.failure("undergraduate_probe", "invalid_configuration", "provider_error")
		return false, err
	}
	trace.step("undergraduate_probe", zap.String("outcome", "start"))
	probeContext, cancel := context.WithTimeout(ctx, oucSSOStageTimeout)
	defer cancel()
	finalURL, body, err := requestPageWithOptions(
		probeContext, current.client, http.MethodGet, target, nil, "", config, trace,
		requestPageOptions{phase: "target_validate"},
	)
	if err != nil {
		trace.failure("undergraduate_probe", "indeterminate", academicErrorKind(err))
		return false, err
	}
	expected, _ := url.Parse(config.Undergraduate.ServiceURL)
	valid := finalURL != nil && finalURL.Scheme == "https" &&
		finalURL.Hostname() == expected.Hostname() && !isAcademicLoginPage(body)
	outcome := "valid"
	if !valid {
		outcome = "rejected"
	}
	trace.step("undergraduate_probe", zap.String("outcome", outcome))
	return valid, nil
}

func requestPage(
	ctx context.Context,
	client *http.Client,
	method string,
	target string,
	body io.Reader,
	contentType string,
	config academicconfig.OUCConfig,
	trace *processTrace,
) (*url.URL, []byte, error) {
	return requestPageWithOptions(ctx, client, method, target, body, contentType, config, trace, requestPageOptions{})
}

type requestPageOptions struct {
	accept                  string
	referer                 string
	origin                  string
	requestedWith           string
	phase                   string
	stopPostRedirect        bool
	stopRedirectOutsideHost string
	redirected              *bool
}

func requestPageWithOptions(
	ctx context.Context,
	client *http.Client,
	method string,
	target string,
	body io.Reader,
	contentType string,
	config academicconfig.OUCConfig,
	trace *processTrace,
	options requestPageOptions,
) (*url.URL, []byte, error) {
	started := time.Now()
	phase := options.phase
	if phase == "" {
		phase = requestPhase(method, target)
	}
	startFields := []zap.Field{zap.String("method", method)}
	startFields = append(startFields, safeURLFields("request", target)...)
	trace.step("http.request.start", startFields...)
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		trace.failure(
			"http.request.error",
			"invalid_request",
			"invalid_request",
			zap.Duration("elapsed", time.Since(started)),
		)
		return nil, nil, fmt.Errorf("create OUC request: %w", err)
	}
	if config.UserAgent != "" {
		request.Header.Set("User-Agent", config.UserAgent)
	}
	accept := options.accept
	if accept == "" {
		accept = "text/html,application/json;q=0.9,*/*;q=0.8"
	}
	request.Header.Set("Accept", accept)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if options.referer != "" {
		request.Header.Set("Referer", options.referer)
	}
	if options.origin != "" {
		request.Header.Set("Origin", options.origin)
	}
	if options.requestedWith != "" {
		request.Header.Set("X-Requested-With", options.requestedWith)
	}
	requestClient := client
	clientCopy := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clientCopy.Transport = observedRoundTripper{
		base:  transport,
		trace: trace,
		phase: phase,
	}
	if options.stopPostRedirect || options.stopRedirectOutsideHost != "" {
		clientCopy.CheckRedirect = func(redirectRequest *http.Request, via []*http.Request) error {
			if options.stopPostRedirect && len(via) > 0 && via[len(via)-1].Method == http.MethodPost {
				return http.ErrUseLastResponse
			}
			if client.CheckRedirect != nil {
				if err := client.CheckRedirect(redirectRequest, via); err != nil {
					return err
				}
			}
			if options.stopRedirectOutsideHost != "" &&
				!strings.EqualFold(redirectRequest.URL.Hostname(), options.stopRedirectOutsideHost) {
				return http.ErrUseLastResponse
			}
			return nil
		}
	}
	requestClient = &clientCopy
	response, err := requestClient.Do(request)
	if err != nil {
		trace.failure(
			"http.request.error",
			"failure",
			safeRequestErrorKind(err),
			zap.Duration("elapsed", time.Since(started)),
		)
		if errors.Is(err, errOUCHTRedirectPolicy) {
			return nil, nil, fmt.Errorf("OUC redirect policy: %w", err)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, verificationapp.NewProviderRetryableError(context.DeadlineExceeded)
		}
		return nil, nil, verificationapp.NewProviderRetryableError(fmt.Errorf("request OUC service: %w", err))
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, config.MaxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		trace.failure(
			"http.response.error",
			"read_error",
			"network_error",
			zap.Int("status_code", response.StatusCode),
			zap.Duration("elapsed", time.Since(started)),
		)
		return nil, nil, verificationapp.NewProviderRetryableError(fmt.Errorf("read OUC response: %w", err))
	}
	finishFields := []zap.Field{
		zap.Int("status_code", response.StatusCode),
		zap.String(
			"content_type",
			safeContentType(response.Header.Get("Content-Type")),
		),
		zap.Int("response_bytes", len(data)),
		zap.Duration("elapsed", time.Since(started)),
	}
	finishFields = append(
		finishFields,
		safeParsedURLFields("response", response.Request.URL)...,
	)
	trace.step("http.response.finish", finishFields...)
	if int64(len(data)) > config.MaxResponseBytes {
		trace.failure(
			"http.response.error",
			"response_too_large",
			"contract_error",
			zap.Int("status_code", response.StatusCode),
			zap.Duration("elapsed", time.Since(started)),
		)
		return nil, nil, fmt.Errorf("OUC response exceeds configured limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		errorKind := "upstream_4xx"
		if response.StatusCode >= 500 {
			errorKind = "upstream_5xx"
		}
		trace.failure(
			"http.response.error",
			"http_status",
			errorKind,
			zap.Int("status_code", response.StatusCode),
			zap.Duration("elapsed", time.Since(started)),
		)
		return nil, nil, fmt.Errorf("OUC service returned HTTP %d", response.StatusCode)
	}
	resultURL := response.Request.URL
	if options.redirected != nil && isFollowableRedirect(response.StatusCode) {
		resultURL, err = response.Location()
		if err != nil {
			return nil, nil, fmt.Errorf("OUC handoff location is invalid: %w", err)
		}
		*options.redirected = true
	}
	return resultURL, data, nil
}

func isFollowableRedirect(statusCode int) bool {
	switch statusCode {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

type observedRoundTripper struct {
	base  http.RoundTripper
	trace *processTrace
	phase string
}

func (t observedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := t.base.RoundTrip(request)
	if err != nil {
		outcome := "transport_error"
		if errors.Is(err, context.Canceled) {
			outcome = "canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			outcome = "deadline"
		}
		t.trace.observeHTTP(requestHost(request.URL.String()), t.phase, outcome, time.Since(started))
		return nil, err
	}
	if response.Body == nil {
		t.trace.observeHTTP(requestHost(request.URL.String()), t.phase, httpOutcome(response.StatusCode), time.Since(started))
		return response, nil
	}
	response.Body = &observedResponseBody{
		ReadCloser: response.Body,
		observe: func(outcome string) {
			t.trace.observeHTTP(requestHost(request.URL.String()), t.phase, outcome, time.Since(started))
		},
		statusCode: response.StatusCode,
	}
	return response, nil
}

type observedResponseBody struct {
	io.ReadCloser
	observe    func(string)
	statusCode int
	once       sync.Once
}

func (b *observedResponseBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if err != nil {
		outcome := httpBodyErrorOutcome(b.statusCode, err)
		b.once.Do(func() { b.observe(outcome) })
	}
	return count, err
}

func (b *observedResponseBody) Close() error {
	err := b.ReadCloser.Close()
	outcome := httpBodyErrorOutcome(b.statusCode, err)
	b.once.Do(func() { b.observe(outcome) })
	return err
}

func httpBodyErrorOutcome(statusCode int, err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return httpOutcome(statusCode)
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "transport_error"
	}
}

func httpOutcome(statusCode int) string {
	if statusCode < http.StatusOK || statusCode >= http.StatusBadRequest {
		return "upstream_error"
	}
	return "success"
}

func requestHost(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || parsed == nil || !allowedOUCHost(parsed.Hostname()) {
		return "unknown"
	}
	return strings.ToLower(parsed.Hostname())
}

func requestPhase(method, target string) string {
	parsed, err := url.Parse(target)
	if err != nil || parsed == nil {
		return "business_query"
	}
	if strings.EqualFold(parsed.Hostname(), "id.ouc.edu.cn") && strings.EqualFold(parsed.Path, "/sso/login") {
		if method == http.MethodPost {
			return "sso_login_submit"
		}
		return "sso_login_page"
	}
	return "business_query"
}

func serviceLoginURL(
	loginURL string,
	serviceURL string,
	noAutoRedirect bool,
) (string, error) {
	parsed, err := url.Parse(loginURL)
	if err != nil {
		return "", fmt.Errorf("parse OUC login URL: %w", err)
	}
	query := parsed.Query()
	query.Set("service", serviceURL)
	if noAutoRedirect {
		query.Set("noAutoRedirect", "1")
	} else {
		query.Del("noAutoRedirect")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func parseLoginForm(body []byte, base *url.URL) (loginForm, error) {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return loginForm{}, fmt.Errorf("parse OUC login page: %w", err)
	}
	var selected *html.Node
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if selected != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == "form" {
			selected = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	if selected == nil {
		return loginForm{}, fmt.Errorf("OUC login form not found")
	}
	form := loginForm{action: base.String(), values: make(url.Values)}
	for _, attribute := range selected.Attr {
		if attribute.Key == "action" && strings.TrimSpace(attribute.Val) != "" {
			form.action = attribute.Val
		}
	}
	var collect func(*html.Node, bool)
	collect = func(node *html.Node, ancestorHidden bool) {
		hidden := ancestorHidden || elementIsHidden(node)
		if node.Type == html.ElementNode && (node.Data == "input" || node.Data == "button") {
			name, value, inputType := "", "", ""
			for _, attribute := range node.Attr {
				switch strings.ToLower(attribute.Key) {
				case "name":
					name = attribute.Val
				case "value":
					value = attribute.Val
				case "type":
					inputType = strings.ToLower(attribute.Val)
				}
			}
			if name != "" && node.Data == "input" {
				form.values.Set(name, value)
				lowerName := strings.ToLower(name)
				if !hidden && inputType != "hidden" &&
					isChallengeControlName(lowerName) {
					form.hasChallenge = true
				}
			}
			if strings.EqualFold(strings.TrimSpace(name), "continue") ||
				(!hidden && (inputType == "submit" || inputType == "button") &&
					strings.Contains(strings.TrimSpace(value), "继续")) ||
				(!hidden && node.Data == "button" &&
					strings.Contains(strings.TrimSpace(nodeText(node)), "继续")) {
				form.hasPasswordExpiryContinue = true
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collect(child, hidden)
		}
	}
	collect(selected, false)
	if form.values.Get("flowId") == "" {
		return loginForm{}, fmt.Errorf("OUC login flowId not found")
	}
	return form, nil
}

func allowedOUCHost(host string) bool {
	switch strings.ToLower(host) {
	case "id.ouc.edu.cn", "my.ouc.edu.cn", "jwgl2024.ouc.edu.cn", "pgs.ouc.edu.cn":
		return true
	default:
		return false
	}
}

func containsAny(body []byte, candidates []string) bool {
	text := strings.ToLower(string(body))
	for _, candidate := range candidates {
		if strings.Contains(text, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func queryProviderError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return application.NewProviderRetryableError(context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, verificationapp.ErrProviderRetryable):
		return application.NewProviderRetryableError(err)
	case errors.Is(err, verificationapp.ErrInvalidCredentials):
		return application.ErrInvalidCredentials
	case errors.Is(err, verificationapp.ErrPasswordExpired):
		return application.ErrPasswordExpired
	case errors.Is(err, verificationapp.ErrChallengeRequired):
		return application.ErrChallengeRequired
	case errors.Is(err, verificationapp.ErrAccountRestricted):
		return application.ErrAccountRestricted
	default:
		return application.ErrProviderUnavailable
	}
}
