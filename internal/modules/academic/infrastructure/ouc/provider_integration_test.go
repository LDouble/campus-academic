package ouc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
	"github.com/emmansun/gmsm/sm2"
)

const (
	integrationStudentNo = "integration-student"
	integrationPassword  = "integration-password"
)

type staticConfigResolver struct {
	snapshot academicconfig.Snapshot
}

func (r staticConfigResolver) Resolve() academicconfig.Snapshot {
	return r.snapshot
}

type memoryOUCSessionStore struct {
	mu              sync.RWMutex
	states          map[string]SessionState
	scopedState     map[string]SessionState
	scopeLoadErrors map[string]error
	scopeSaveErrors map[string]error
	scopeLoadHits   map[string]int
	scopeLoadNotify chan struct{}
}

// legacyOnlySessionStore deliberately exposes only the v1 interface so
// compatibility behaviour can be tested independently of scoped storage.
type legacyOnlySessionStore struct {
	store *memoryOUCSessionStore
}

func (s *legacyOnlySessionStore) Load(ctx context.Context, studentNo, password string) (SessionState, bool, error) {
	return s.store.Load(ctx, studentNo, password)
}

func (s *legacyOnlySessionStore) Save(ctx context.Context, studentNo, password string, state SessionState, ttl time.Duration) error {
	return s.store.Save(ctx, studentNo, password, state, ttl)
}

func (s *legacyOnlySessionStore) Delete(ctx context.Context, studentNo, password string) error {
	return s.store.Delete(ctx, studentNo, password)
}

func newMemoryOUCSessionStore() *memoryOUCSessionStore {
	return &memoryOUCSessionStore{
		states:          make(map[string]SessionState),
		scopedState:     make(map[string]SessionState),
		scopeLoadErrors: make(map[string]error),
		scopeSaveErrors: make(map[string]error),
		scopeLoadHits:   make(map[string]int),
	}
}

func (s *memoryOUCSessionStore) Load(
	ctx context.Context,
	studentNo string,
	password string,
) (SessionState, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionState{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[studentNo+"\x00"+password]
	return state, ok, nil
}

func (s *memoryOUCSessionStore) Save(
	ctx context.Context,
	studentNo string,
	password string,
	state SessionState,
	_ time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.states[studentNo+"\x00"+password] = state
	s.mu.Unlock()
	return nil
}

func (s *memoryOUCSessionStore) Delete(
	ctx context.Context,
	studentNo string,
	password string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.states, studentNo+"\x00"+password)
	s.mu.Unlock()
	return nil
}

func (s *memoryOUCSessionStore) LoadScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
) (SessionState, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionState{}, false, err
	}
	key := memoryScopeKey(studentNo, password, scope)
	s.mu.Lock()
	s.scopeLoadHits[key]++
	if s.scopeLoadNotify != nil {
		select {
		case s.scopeLoadNotify <- struct{}{}:
		default:
		}
	}
	defer s.mu.Unlock()
	if err := s.scopeLoadErrors[key]; err != nil {
		return SessionState{}, false, err
	}
	state, ok := s.scopedState[key]
	if ok {
		return state, true, nil
	}
	legacy, legacyFound := s.states[studentNo+"\x00"+password]
	if !legacyFound {
		return SessionState{}, false, nil
	}
	filtered, err := filterSessionState(legacy, scope)
	if err != nil || len(filtered.Sets) == 0 {
		return SessionState{}, false, err
	}
	return filtered, true, nil
}

func (s *memoryOUCSessionStore) SaveScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
	state SessionState,
	_ time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateScopedSessionState(state, scope); err != nil {
		return err
	}
	key := memoryScopeKey(studentNo, password, scope)
	s.mu.RLock()
	saveErr := s.scopeSaveErrors[key]
	s.mu.RUnlock()
	if saveErr != nil {
		return saveErr
	}
	s.mu.Lock()
	s.scopedState[key] = state
	s.mu.Unlock()
	return nil
}

func (s *memoryOUCSessionStore) DeleteScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.scopedState, memoryScopeKey(studentNo, password, scope))
	delete(s.scopeLoadErrors, memoryScopeKey(studentNo, password, scope))
	delete(s.states, studentNo+"\x00"+password)
	s.mu.Unlock()
	return nil
}

func memoryScopeKey(studentNo, password string, scope SessionScope) string {
	return string(scope) + "\x00" + studentNo + "\x00" + password
}

type fakeOUCServer struct {
	server                      *httptest.Server
	privateKey                  *sm2.PrivateKey
	sm2Enabled                  bool
	challenge                   bool
	passwordWarn                bool
	passwordWarnWithoutContinue bool
	loginPostDelay              time.Duration
	portalCASDelay              time.Duration
	malformedLoginErrorMetadata bool
	genericLoginFailure         bool
	loginResponseError          *loginResponseError
	portalBody                  string
	accepted                    map[string]bool
	ssoGets                     atomic.Int32
	loginPosts                  atomic.Int32
	continuePosts               atomic.Int32
	portalIdentityHits          atomic.Int32
	rejectQuery                 atomic.Bool
	rejectQueryAlways           atomic.Bool
	redirectQuery               atomic.Bool
	redirectQueryAlways         atomic.Bool
	failQuery                   atomic.Bool
	failUndergraduateProbe      atomic.Bool
	slowQuery                   atomic.Bool
	malformedQuery              atomic.Bool
	malformedCatalog            atomic.Int32
	selectionFailureQueries     atomic.Int32
	failSelectionFailureQuery   atomic.Bool
	selectionScheduleEntered    atomic.Bool
	failSSO                     atomic.Bool
	requireSSOCookieForService  atomic.Bool
	queryHitsMu                 sync.Mutex
	queryHits                   map[string]int
	serviceHits                 map[string]int
	catalogServiceProbes        atomic.Int32
	contractErrs                []string
	serviceStarted              chan struct{}
	serviceRelease              chan struct{}
}

func newFakeOUCServer(
	t *testing.T,
	sm2Enabled bool,
	accepted map[string]bool,
) *fakeOUCServer {
	t.Helper()
	fake := &fakeOUCServer{
		sm2Enabled:   sm2Enabled,
		accepted:     accepted,
		queryHits:    make(map[string]int),
		serviceHits:  make(map[string]int),
		contractErrs: make([]string, 0),
	}
	if sm2Enabled {
		privateKey, err := sm2.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		fake.privateKey = privateKey
	}
	fake.server = httptest.NewTLSServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (s *fakeOUCServer) clientFactory(t *testing.T) sessionClientFactory {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(s.server.Certificate())
	targetAddress := s.server.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, targetAddress)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: "example.com",
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return func(config academicconfig.OUCConfig) (*http.Client, error) {
		client, err := newSessionClient(config)
		if err != nil {
			return nil, err
		}
		client.Transport = transport
		return client, nil
	}
}

func (s *fakeOUCServer) handle(writer http.ResponseWriter, request *http.Request) {
	host := request.Host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	if strings.ToLower(host) != "id.ouc.edu.cn" {
		if strings.Contains(request.URL.RawQuery, integrationPassword) {
			s.recordContractError("password leaked through a non-SSO query string")
		}
		if request.Body != nil {
			body, err := io.ReadAll(
				io.LimitReader(request.Body, (1<<20)+1),
			)
			if err != nil {
				s.recordContractError("read fake OUC request body: " + err.Error())
			} else {
				request.Body = io.NopCloser(bytes.NewReader(body))
				if strings.Contains(string(body), integrationPassword) {
					s.recordContractError("password leaked to a non-SSO request body")
				}
			}
		}
	}
	switch strings.ToLower(host) {
	case "id.ouc.edu.cn":
		s.handleSSO(writer, request)
	case "my.ouc.edu.cn":
		s.handlePortal(writer, request)
	case "jwgl2024.ouc.edu.cn":
		s.handleAcademic(writer, request, verificationapp.EducationUndergraduate)
	case "pgs.ouc.edu.cn":
		s.handleAcademic(writer, request, verificationapp.EducationGraduate)
	default:
		http.Error(writer, "unexpected host", http.StatusBadRequest)
	}
}

func (s *fakeOUCServer) handleSSO(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		s.ssoGets.Add(1)
		if request.URL.Query().Get("service") == integrationOUCConfig().PortalServiceURL {
			if request.URL.Query().Get("noAutoRedirect") != "1" {
				s.recordContractError("portal SSO request omitted noAutoRedirect=1")
			}
		}
		if s.failSSO.CompareAndSwap(true, false) {
			http.Error(writer, "temporary SSO failure", http.StatusBadGateway)
			return
		}
		forceLogin := request.URL.Query().Get("forceLogin") == "1"
		if cookie, err := request.Cookie("OUC_SSO"); !forceLogin && err == nil && cookie.Value == "authenticated" {
			s.redirectToService(writer, request)
			return
		}
		publicKey := ""
		if s.sm2Enabled {
			ecdhPublicKey, err := sm2.PublicKeyToECDH(&s.privateKey.PublicKey)
			if err != nil {
				s.recordContractError("convert fake SM2 public key: " + err.Error())
				http.Error(writer, "SM2 setup failed", http.StatusInternalServerError)
				return
			}
			publicKey = base64.StdEncoding.EncodeToString(ecdhPublicKey.Bytes())
		}
		config, err := json.Marshal(map[string]any{
			"sm2": map[string]any{
				"enabled":   s.sm2Enabled,
				"publicKey": publicKey,
			},
		})
		if err != nil {
			s.recordContractError("encode fake SSO config: " + err.Error())
			http.Error(writer, "config failed", http.StatusInternalServerError)
			return
		}
		captchaType := "hidden"
		if s.challenge {
			captchaType = "text"
		}
		s.writeFormat(
			writer,
			`<form method="post">
				<input name="username">
				<input name="password" type="password">
				<input name="loginType" value="username_password">
				<input name="flowId" value="flow-integration">
				<input name="captcha" type="%s" value="">
			</form>
			<script>var ssoConfig = %s;</script>`,
			captchaType,
			config,
		)
		return
	}
	if request.Method != http.MethodPost {
		http.Error(writer, "unsupported method", http.StatusMethodNotAllowed)
		return
	}
	s.loginPosts.Add(1)
	if s.loginPostDelay > 0 {
		timer := time.NewTimer(s.loginPostDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			return
		}
	}
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	if request.Form.Get("continue") == "1" {
		s.continuePosts.Add(1)
		if request.Form.Get("flowId") != "flow-continue" {
			s.recordContractError("continue request did not preserve refreshed flowId")
			s.writeString(writer, "登录流程已失效")
			return
		}
		if request.Form.Get("username") != "" ||
			request.Form.Get("password") != "" ||
			request.Form.Get("loginType") != "" {
			s.recordContractError("continue request resubmitted credentials")
			s.writeString(writer, "登录流程无效")
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     "OUC_SSO",
			Value:    "authenticated",
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		s.redirectToService(writer, request)
		return
	}
	password := request.Form.Get("password")
	if s.sm2Enabled {
		ciphertext, err := base64.StdEncoding.DecodeString(password)
		if err != nil {
			s.recordContractError("decode submitted SM2 password: " + err.Error())
			s.writeString(writer, "用户名或密码错误")
			return
		}
		plaintext, err := s.privateKey.Decrypt(rand.Reader, ciphertext, nil)
		if err != nil {
			s.recordContractError("decrypt submitted SM2 password: " + err.Error())
			s.writeString(writer, "用户名或密码错误")
			return
		}
		password = string(plaintext)
	}
	if request.Form.Get("username") != integrationStudentNo ||
		password != integrationPassword ||
		request.Form.Get("flowId") != "flow-integration" {
		if s.genericLoginFailure {
			s.writeString(writer, "认证失败")
			return
		}
		if s.loginResponseError != nil {
			encoded, err := json.Marshal(s.loginResponseError)
			if err != nil {
				s.recordContractError("encode login response error: " + err.Error())
				http.Error(writer, "encode login response error", http.StatusInternalServerError)
				return
			}
			s.writeFormat(writer, `<script>var error = %s;</script>`, encoded)
			return
		}
		if s.malformedLoginErrorMetadata {
			s.writeString(writer, `<script>var error = {;</script>用户名或密码错误`)
			return
		}
		s.writeString(writer, "用户名或密码错误")
		return
	}
	if s.passwordWarn {
		continueControl := `<input name="continue" value="">`
		if s.passwordWarnWithoutContinue {
			continueControl = ""
		}
		s.writeFormat(
			writer,
			`<form method="post">
				<input name="username">
				<input name="password" type="password">
				<input name="loginType" value="username_password">
				<input name="flowId" value="flow-continue">
				%s
			</form>
			<script>
				var error = {"code":40605,"msg":"您的密码已过期建议立即修改"};
				var pageName = "resetWarn";
			</script>`,
			continueControl,
		)
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     "OUC_SSO",
		Value:    "authenticated",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	s.redirectToService(writer, request)
}

func (s *fakeOUCServer) redirectToService(
	writer http.ResponseWriter,
	request *http.Request,
) {
	target, err := url.Parse(request.URL.Query().Get("service"))
	if err != nil || target.Scheme != "https" || !allowedOUCHost(target.Hostname()) {
		s.recordContractError("invalid fake service URL")
		http.Error(writer, "invalid service", http.StatusBadRequest)
		return
	}
	query := target.Query()
	query.Set("ticket", "ST-integration")
	target.RawQuery = query.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

func (s *fakeOUCServer) handlePortal(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/manage/common/cas_login/2":
		if s.portalCASDelay > 0 {
			timer := time.NewTimer(s.portalCASDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-request.Context().Done():
				return
			}
		}
		if request.URL.Query().Get("ticket") == "" {
			s.recordContractError("portal CAS callback omitted ticket")
			http.Error(writer, "missing ticket", http.StatusBadRequest)
			return
		}
		redirect := request.URL.Query().Get("redirect")
		if redirect != "https://my.ouc.edu.cn/frontend/user/info" {
			s.recordContractError("portal CAS callback used unexpected identity redirect")
			http.Error(writer, "invalid redirect", http.StatusBadRequest)
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     "PORTAL_SESSION",
			Value:    "portal",
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
		})
		http.Redirect(writer, request, redirect, http.StatusFound)
	case "/frontend/user/info":
		cookie, err := request.Cookie("PORTAL_SESSION")
		if err != nil || cookie.Value != "portal" {
			http.Error(writer, "missing portal session", http.StatusUnauthorized)
			return
		}
		s.portalIdentityHits.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		body := s.portalBody
		if body == "" {
			body = `{"e":0,"m":"操作成功","d":{"info":{"uid":7,"name":"集成测试同学","xgh":"integration-student","identity":"校友","identity_id":"4001","mobile":"","email":""}}}`
		}
		s.writeString(writer, body)
	default:
		http.Error(writer, "unexpected portal path", http.StatusNotFound)
	}
}

func (s *fakeOUCServer) handleAcademic(
	writer http.ResponseWriter,
	request *http.Request,
	educationLevel string,
) {
	if request.URL.Path == "/" ||
		request.URL.Path == "/allogene/page/home.htm" {
		s.catalogServiceProbes.Add(1)
		s.queryHitsMu.Lock()
		s.serviceHits[educationLevel]++
		s.queryHitsMu.Unlock()
		if s.serviceStarted != nil && s.serviceRelease != nil {
			select {
			case s.serviceStarted <- struct{}{}:
			default:
			}
			<-s.serviceRelease
		}
	}
	if !s.accepted[educationLevel] {
		s.writeString(writer, "不在本系统")
		return
	}
	if request.URL.Path == "/" ||
		request.URL.Path == "/allogene/page/home.htm" {
		if s.requireSSOCookieForService.Load() {
			cookie, err := request.Cookie("OUC_SSO")
			if err != nil || cookie.Value != "authenticated" {
				http.Redirect(writer, request, "https://id.ouc.edu.cn/sso/login", http.StatusFound)
				return
			}
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     "ACADEMIC_SESSION",
			Value:    educationLevel,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
		})
		if educationLevel == verificationapp.EducationGraduate {
			writer.Header().Set("Content-Type", "text/html")
			s.writeString(
				writer,
				`<a class="user name" href="/allogene/page/home.htm">集成测试同学</a>`,
			)
		} else {
			writer.Header().Set("Content-Type", "text/html")
			s.writeString(
				writer,
				`<a href="/jsxsd/framework/xsMainV_new.htmlx">新版首页</a>`,
			)
		}
		return
	}
	cookie, err := request.Cookie("ACADEMIC_SESSION")
	if err != nil || cookie.Value != educationLevel {
		http.Redirect(
			writer,
			request,
			"https://id.ouc.edu.cn/sso/login",
			http.StatusFound,
		)
		return
	}
	s.queryHitsMu.Lock()
	s.queryHits[educationLevel+":"+request.URL.Path]++
	s.queryHitsMu.Unlock()
	if request.URL.Path == "/jsxsd/framework/xsMainV_new.htmlx" ||
		request.URL.Path == "/jsxsd/framework/xsMainV.htmlx" {
		if s.failUndergraduateProbe.CompareAndSwap(true, false) {
			http.Error(writer, "temporary probe failure", http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(
			writer,
			`<div class="right-person">
				<p>集成测试同学</p>
				<ul class="right-person-hover">
					<li><span>修改密码</span></li>
					<li><span>注销登录</span></li>
				</ul>
			</div>`,
		)
		return
	}
	if s.failQuery.CompareAndSwap(true, false) {
		http.Error(writer, "temporary academic failure", http.StatusBadGateway)
		return
	}
	if s.slowQuery.CompareAndSwap(true, false) {
		<-request.Context().Done()
		return
	}
	if s.redirectQueryAlways.Load() || s.redirectQuery.CompareAndSwap(true, false) {
		serviceURL := "https://" + request.Host + request.URL.RequestURI()
		loginURL := integrationOUCConfig().SSOLoginURL +
			"?forceLogin=1&service=" + url.QueryEscape(serviceURL)
		http.Redirect(writer, request, loginURL, http.StatusFound)
		return
	}
	if s.rejectQueryAlways.Load() || s.rejectQuery.CompareAndSwap(true, false) {
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(
			writer,
			`<!doctype html><html><title>登录</title>
				<form name="loginForm" action="/jsxsd/xk/LoginToXk">
					<input type="text" name="userAccount">
					<input type="password" name="userPassword">
				</form>
			</html>`,
		)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if s.malformedQuery.CompareAndSwap(true, false) {
		s.writeString(writer, `{"unexpected":true}`)
		return
	}
	if request.URL.Path == "/jsxsd/xsxk/newXsxkzx" {
		if request.URL.Query().Get("jx0502zbid") != "selection-session-test" || request.URL.Query().Get("isallsc") != "" {
			http.Error(writer, "invalid selection entry", http.StatusBadRequest)
			return
		}
		s.selectionScheduleEntered.Store(true)
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(writer, `<html>selection context entered</html>`)
		return
	}
	if request.URL.Path == "/jsxsd/xsxk/xsxk_tzsm" {
		if !s.selectionScheduleEntered.Load() {
			http.Error(writer, "selection context required", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(writer, `<table id="tbData"><thead><tr><th><div>选课号</div></th><th><div>课程号</div></th><th><div>课程名称</div></th><th><div>上课教师</div></th><th><div>上课时间</div></th></tr></thead><tbody><tr><td>SELECT-001</td><td>OUC1001</td><td>已选课程</td><td>测试教师</td><td>1-8周 星期一 1-2节;1-8周 星期三 3-4节</td></tr></tbody></table>`)
		return
	}
	if strings.HasSuffix(request.URL.Path, "/graduate/empty-courses") {
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(writer, `<table class="table table-course"><tr><th colspan="2">时间</th><th>星期一</th><th>星期二</th><th>星期三</th><th>星期四</th><th>星期五</th><th>星期六</th><th>星期日</th></tr><tr><td>上午</td><td>第1节</td><td></td><td></td><td></td><td></td><td></td><td></td><td></td></tr></table>`)
		return
	}
	if strings.HasSuffix(request.URL.Path, "/graduate/fallback-courses") {
		writer.Header().Set("Content-Type", "text/html")
		s.writeString(writer, `<table class="table table-bordered table-striped"><thead><tr><th>开课学年</th><th>开课学期</th><th>班级编号</th><th>课程名称</th><th>学分</th><th>任课教师</th><th>时间与地点</th><th>备注</th></tr></thead><tbody><tr><td>2026-2027</td><td>夏秋</td><td>GRFALLBACK001</td><td>历史回退课程</td><td>2</td><td>回退教师</td><td>( 4,9-11,14周 )||星期二||第1-2节||(鱼山校区||鱼山楼群||教学楼101)</td><td></td></tr></tbody></table>`)
		return
	}
	switch {
	case strings.HasSuffix(request.URL.Path, "/catalog"):
		if educationLevel == verificationapp.EducationGraduate {
			writer.Header().Set("Content-Type", "text/html")
			s.writeString(writer, `<!doctype html><html><body><p>共 1 条，共 1 页</p><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-001</td><td>海洋科学</td><td>海洋学院</td><td>30/12</td><td>中文</td><td>教师甲</td><td>周一1-2节</td><td></td></tr></table></body></html>`)
			return
		}
		if s.malformedCatalog.Add(-1) >= 0 {
			s.writeString(writer, `{"code":0,"count":517,"data":null}`)
			return
		}
		s.writeString(writer, `{"code":0,"count":100,"data":[{"xnxq01id":"2026-2027-1","xkh":"CAT-1","kcmc":"课程目录测试"}]}`)
	case strings.HasSuffix(request.URL.Path, "/periods"):
		s.writeString(
			writer,
			`[{"id":"2026-1","label":"2025-2026学年春季学期","start_date":"2026-02-23","week_count":20,"is_current":true}]`,
		)
	case strings.HasSuffix(request.URL.Path, "/courses"):
		s.writeString(
			writer,
			`[{"id":"course-1","courseCode":"OUC1001","courseName":"海洋科学导论","teacherName":"测试教师","dayOfWeek":2,"startSection":1,"endSection":2,"weeks":[1,2]}]`,
		)
	case strings.HasSuffix(request.URL.Path, "/grades"):
		s.writeString(
			writer,
			`[{"id":"grade-1","courseCode":"OUC1001","courseName":"海洋科学导论","credit":2,"score":92,"note":"密码错误仅为课程示例"}]`,
		)
	case strings.HasSuffix(request.URL.Path, "/exams"):
		s.writeString(
			writer,
			`[{"id":"exam-1","courseCode":"OUC1001","courseName":"海洋科学导论","startAt":"2026-06-20T01:00:00Z","endAt":"2026-06-20T03:00:00Z"}]`,
		)
	case strings.Contains(request.URL.Path, "/selections/"):
		if request.URL.Query().Get("lx") == "tkrz" {
			s.selectionFailureQueries.Add(1)
			for key, want := range map[string]string{
				"lx":              "tkrz",
				"type":            "list",
				"cxsj":            "tkjg",
				"pageNum":         "1",
				"pageSize":        "20",
				"sf_request_type": "ajax",
			} {
				if got := request.URL.Query().Get(key); got != want {
					s.recordContractError(fmt.Sprintf("selection supplement query %s=%q want %q", key, got, want))
				}
			}
			if s.failSelectionFailureQuery.Load() {
				http.Error(writer, "temporary selection supplement failure", http.StatusBadGateway)
				return
			}
			s.writeString(
				writer,
				`[{"id":"selection-failed-1","courseCode":"OUC1002","courseName":"抽签落选课程","credit":2,"status":"","tklx":"抽签落选"},{"id":"selection-personal-1","courseCode":"OUC1003","courseName":"个人退选课程","credit":2,"status":"","tklx":"个人退选"}]`,
			)
			return
		}
		s.writeString(
			writer,
			`[{"id":"selection-1","courseCode":"OUC1001","courseName":"海洋科学导论","credit":2,"status":"selected"}]`,
		)
	default:
		http.Error(writer, "unknown academic operation", http.StatusNotFound)
	}
}

func (s *fakeOUCServer) writeString(writer io.Writer, value string) {
	if _, err := io.WriteString(writer, value); err != nil {
		s.recordContractError("write fake OUC response: " + err.Error())
	}
}

func (s *fakeOUCServer) writeFormat(
	writer io.Writer,
	format string,
	values ...any,
) {
	if _, err := fmt.Fprintf(writer, format, values...); err != nil {
		s.recordContractError("write formatted fake OUC response: " + err.Error())
	}
}

func (s *fakeOUCServer) recordContractError(message string) {
	s.queryHitsMu.Lock()
	s.contractErrs = append(s.contractErrs, message)
	s.queryHitsMu.Unlock()
}

func (s *fakeOUCServer) assertNoContractErrors(t *testing.T) {
	t.Helper()
	s.queryHitsMu.Lock()
	defer s.queryHitsMu.Unlock()
	if len(s.contractErrs) != 0 {
		t.Fatalf("fake OUC contract errors=%v", s.contractErrs)
	}
}

func (s *fakeOUCServer) assertQueryCoverage(
	t *testing.T,
	educationLevel string,
) {
	t.Helper()
	config := integrationOUCConfig()
	endpoint := config.Undergraduate
	if educationLevel == verificationapp.EducationGraduate {
		endpoint = config.Graduate
	}
	paths := []string{
		endpoint.Periods.Path,
		endpoint.Courses.Path,
		endpoint.Grades.Path,
		endpoint.Exams.Path,
		strings.ReplaceAll(
			endpoint.Selections.Path,
			"{period_id}",
			"2026-1",
		),
	}
	s.queryHitsMu.Lock()
	defer s.queryHitsMu.Unlock()
	for _, path := range paths {
		want := 1
		if educationLevel == verificationapp.EducationUndergraduate &&
			path == strings.ReplaceAll(endpoint.Selections.Path, "{period_id}", "2026-1") {
			want = 2
		}
		s.assertQueryHitCountLocked(t, educationLevel, path, want)
	}
}

func (s *fakeOUCServer) assertQueryHit(
	t *testing.T,
	educationLevel string,
	path string,
) {
	t.Helper()
	s.queryHitsMu.Lock()
	defer s.queryHitsMu.Unlock()
	s.assertQueryHitLocked(t, educationLevel, path)
}

func (s *fakeOUCServer) assertQueryHitLocked(
	t *testing.T,
	educationLevel string,
	path string,
) {
	s.assertQueryHitCountLocked(t, educationLevel, path, 1)
}

func (s *fakeOUCServer) assertQueryHitCountLocked(
	t *testing.T,
	educationLevel string,
	path string,
	want int,
) {
	t.Helper()
	key := educationLevel + ":" + path
	if s.queryHits[key] != want {
		t.Errorf("query hit %q=%d want=%d", key, s.queryHits[key], want)
	}
}

func (s *fakeOUCServer) assertOnlyServiceAccessed(
	t *testing.T,
	educationLevel string,
) {
	t.Helper()
	otherEducationLevel := verificationapp.EducationUndergraduate
	if educationLevel == verificationapp.EducationUndergraduate {
		otherEducationLevel = verificationapp.EducationGraduate
	}
	s.queryHitsMu.Lock()
	defer s.queryHitsMu.Unlock()
	if s.serviceHits[educationLevel] < 1 {
		t.Errorf(
			"selected service hit %q=%d want at least 1",
			educationLevel,
			s.serviceHits[educationLevel],
		)
	}
	if s.serviceHits[otherEducationLevel] != 0 {
		t.Errorf(
			"unselected service hit %q=%d want 0",
			otherEducationLevel,
			s.serviceHits[otherEducationLevel],
		)
	}
}

func (s *fakeOUCServer) assertNoAcademicServiceAccessed(t *testing.T) {
	t.Helper()
	s.queryHitsMu.Lock()
	defer s.queryHitsMu.Unlock()
	for educationLevel, hits := range s.serviceHits {
		if hits != 0 {
			t.Errorf("academic service hit %q=%d want 0", educationLevel, hits)
		}
	}
}

func TestOUCProviderFullFlowWithPlainAndSM2Login(t *testing.T) {
	tests := []struct {
		name           string
		educationLevel string
		sm2Enabled     bool
	}{
		{
			name:           "undergraduate plaintext login",
			educationLevel: verificationapp.EducationUndergraduate,
		},
		{
			name:           "graduate SM2 login",
			educationLevel: verificationapp.EducationGraduate,
			sm2Enabled:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, test.sm2Enabled, map[string]bool{
				verificationapp.EducationUndergraduate: true,
				verificationapp.EducationGraduate:      true,
			})
			config := integrationOUCConfig()
			provider := NewProvider(
				staticConfigResolver{snapshot: academicconfig.Snapshot{
					ActiveProvider: academicconfig.ProviderOUC,
					OUC:            config,
				}},
				WithSessionStore(newMemoryOUCSessionStore()),
				withSessionClientFactory(fake.clientFactory(t)),
			)
			result, err := provider.Verify(
				context.Background(),
				verificationapp.VerificationCommand{
					StudentNo:      integrationStudentNo,
					Password:       integrationPassword,
					EducationLevel: test.educationLevel,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.RealName != "集成测试同学" ||
				result.Provider != verificationapp.ProviderOUC ||
				result.EducationLevel != test.educationLevel {
				t.Fatalf("verification result=%+v", result)
			}
			fake.assertNoAcademicServiceAccessed(t)
			student := application.StudentReference{
				UserID:         7,
				StudentNo:      integrationStudentNo,
				Provider:       verificationapp.ProviderOUC,
				EducationLevel: test.educationLevel,
			}
			credential := application.Credential{
				StudentNo: integrationStudentNo,
				Password:  integrationPassword,
			}
			periods, err := provider.ListPeriods(
				context.Background(),
				student,
				credential,
			)
			if err != nil || len(periods) != 1 || periods[0].ID != "2026-1" {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			courses, err := provider.ListCourses(
				context.Background(),
				student,
				credential,
				periods[0].ID,
			)
			if err != nil || len(courses.Courses) != 1 || courses.Courses[0].Name != "海洋科学导论" {
				t.Fatalf("courses=%+v err=%v", courses, err)
			}
			grades, err := provider.ListGrades(
				context.Background(),
				student,
				credential,
				periods[0].ID,
			)
			if err != nil || len(grades) != 1 || grades[0].Score == nil {
				t.Fatalf("grades=%+v err=%v", grades, err)
			}
			exams, err := provider.ListExams(
				context.Background(),
				student,
				credential,
				periods[0].ID,
			)
			if err != nil || len(exams) != 1 {
				t.Fatalf("exams=%+v err=%v", exams, err)
			}
			selections, err := provider.ListCourseSelections(
				context.Background(),
				student,
				credential,
				periods[0].ID,
			)
			wantSelections := 1
			if test.educationLevel == verificationapp.EducationUndergraduate {
				wantSelections = 2
			}
			if err != nil || len(selections) != wantSelections {
				t.Fatalf("selections=%+v err=%v", selections, err)
			}
			if test.educationLevel == verificationapp.EducationUndergraduate {
				if got := fake.selectionFailureQueries.Load(); got != 1 {
					t.Fatalf("selection failure query count=%d want=1", got)
				}
				failed := selections[1]
				if failed.Status != domain.CourseSelectionFailed ||
					failed.ResultText == nil || *failed.ResultText != "抽签落选" ||
					failed.Schedule != "抽签落选" {
					t.Fatalf("failed selection=%+v", failed)
				}
				for _, selection := range selections {
					if selection.ResultText != nil && strings.Contains(*selection.ResultText, "个人退选") {
						t.Fatalf("personal withdrawal was not filtered: %+v", selection)
					}
				}
			}
			if got := fake.loginPosts.Load(); got != 1 {
				t.Fatalf("login POST count=%d want=1", got)
			}
			fake.assertOnlyServiceAccessed(t, test.educationLevel)
			fake.assertQueryCoverage(t, test.educationLevel)
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderUndergraduateSelectionSupplementIsBestEffort(t *testing.T) {
	t.Parallel()
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.failSelectionFailureQuery.Store(true)
	provider := newIntegrationProvider(t, fake)
	student := application.StudentReference{
		UserID:         7,
		StudentNo:      integrationStudentNo,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{
		StudentNo: integrationStudentNo,
		Password:  integrationPassword,
	}
	selections, err := provider.ListCourseSelections(
		context.Background(),
		student,
		credential,
		"2026-2027-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selections) != 1 || selections[0].ID != "selection-1" {
		t.Fatalf("selections=%+v want original selection only", selections)
	}
	if got := fake.selectionFailureQueries.Load(); got != 1 {
		t.Fatalf("selection failure query count=%d want=1", got)
	}
	fake.queryHitsMu.Lock()
	got := fake.queryHits[verificationapp.EducationUndergraduate+":/undergraduate/selections/2026-2027-1"]
	fake.queryHitsMu.Unlock()
	if got != 2 {
		t.Fatalf("selection query count=%d want=2", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderCourseSelectionScheduleEntersSelectionContextBeforeReadingTable(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{verificationapp.EducationUndergraduate: true})
	provider := newIntegrationProvider(t, fake)
	student := application.StudentReference{UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC, EducationLevel: verificationapp.EducationUndergraduate}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	schedule, err := provider.GetCourseSelectionSchedule(context.Background(), student, credential, "2026-2027-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 2 || schedule.Courses[0].ClassNum != "SELECT-001" || schedule.Courses[1].ClassNum != "SELECT-001" {
		t.Fatalf("schedule=%+v", schedule)
	}
	fake.assertQueryHit(t, verificationapp.EducationUndergraduate, "/jsxsd/xsxk/newXsxkzx")
	fake.assertQueryHit(t, verificationapp.EducationUndergraduate, "/jsxsd/xsxk/xsxk_tzsm")
}

func TestOUCProviderGraduateCoursesFallsBackToSelectionHistoryWhenGridIsEmpty(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	config := integrationOUCConfig()
	config.Graduate.Courses = academicconfig.OperationEndpoint{
		Path:             "/graduate/empty-courses",
		RequestMethod:    http.MethodGet,
		RequestEncoding:  "query",
		PeriodParameters: []string{"xn", "xj"},
		PeriodSeparator:  ":",
		ResponseEncoding: "html",
	}
	config.Graduate.CoursesFallback = academicconfig.OperationEndpoint{
		Path:             "/graduate/fallback-courses",
		RequestMethod:    http.MethodGet,
		RequestEncoding:  "query",
		PeriodParameters: []string{"xn", "xj"},
		PeriodSeparator:  ":",
		ResponseEncoding: "html",
	}
	provider := newIntegrationProviderWithConfig(t, fake, newMemoryOUCSessionStore(), config)
	student := application.StudentReference{
		UserID:         7,
		StudentNo:      integrationStudentNo,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationGraduate,
	}
	courses, err := provider.ListCourses(
		context.Background(),
		student,
		application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword},
		"2026:11",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(courses.Courses) != 1 {
		t.Fatalf("courses=%+v", courses.Courses)
	}
	course := courses.Courses[0]
	if course.Name != "历史回退课程" ||
		course.CourseCode != "GRFALLBACK" ||
		course.Weekday != 2 ||
		course.StartSection != 1 ||
		course.EndSection != 2 ||
		!sameIntegerValues(course.Weeks, []int{4, 9, 10, 11, 14}) {
		t.Fatalf("fallback course=%+v", course)
	}
	fake.assertQueryHit(t, verificationapp.EducationGraduate, "/graduate/empty-courses")
	fake.assertQueryHit(t, verificationapp.EducationGraduate, "/graduate/fallback-courses")
	fake.assertNoContractErrors(t)
}

func TestOUCProviderContinuesPastPasswordExpiryWarning(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	fake.passwordWarn = true
	provider := newIntegrationProvider(t, fake)
	result, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       integrationPassword,
			EducationLevel: verificationapp.EducationGraduate,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RealName != "集成测试同学" ||
		result.EducationLevel != verificationapp.EducationGraduate {
		t.Fatalf("verification result=%+v", result)
	}
	if got := fake.loginPosts.Load(); got != 2 {
		t.Fatalf("login POST count=%d want=2", got)
	}
	if got := fake.continuePosts.Load(); got != 1 {
		t.Fatalf("continue POST count=%d want=1", got)
	}
	fake.assertNoAcademicServiceAccessed(t)
	fake.assertNoContractErrors(t)
}

func TestOUCProviderDoesNotReplayTimedOutLoginPost(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	fake.loginPostDelay = 2500 * time.Millisecond
	provider := newIntegrationProvider(t, fake)
	_, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       integrationPassword,
			EducationLevel: verificationapp.EducationGraduate,
		},
	)
	if !errors.Is(err, verificationapp.ErrProviderRetryable) {
		t.Fatalf("verification error=%v want=%v", err, verificationapp.ErrProviderRetryable)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderKeepsHandoffWithinAuthenticationBudget(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	fake.portalCASDelay = 2500 * time.Millisecond
	provider := newIntegrationProvider(t, fake)
	result, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       integrationPassword,
			EducationLevel: verificationapp.EducationGraduate,
		},
	)
	if err != nil || result.RealName != "集成测试同学" {
		t.Fatalf("verification result=%+v err=%v", result, err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderExposesPasswordExpiryWithoutContinueControl(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	fake.passwordWarn = true
	fake.passwordWarnWithoutContinue = true
	provider := newIntegrationProvider(t, fake)
	_, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       integrationPassword,
			EducationLevel: verificationapp.EducationGraduate,
		},
	)
	if !errors.Is(err, verificationapp.ErrPasswordExpired) {
		t.Fatalf("verification error=%v want=%v", err, verificationapp.ErrPasswordExpired)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	if got := fake.continuePosts.Load(); got != 0 {
		t.Fatalf("continue POST count=%d want=0", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderVerificationErrors(t *testing.T) {
	tests := []struct {
		name                        string
		accepted                    map[string]bool
		password                    string
		educationLevel              string
		challenge                   bool
		malformedLoginErrorMetadata bool
		genericLoginFailure         bool
		loginResponseError          *loginResponseError
		portalBody                  string
		expectedError               error
		loginPosts                  int32
		portalIdentityHits          int32
	}{
		{
			name: "invalid credentials",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:       "wrong-integration-password",
			educationLevel: verificationapp.EducationUndergraduate,
			expectedError:  verificationapp.ErrInvalidCredentials,
			loginPosts:     1,
		},
		{
			name: "invalid credentials from decoded upstream message",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:           "wrong-integration-password",
			educationLevel:     verificationapp.EducationUndergraduate,
			loginResponseError: &loginResponseError{Code: 40400, Msg: "账号或密码错误"},
			expectedError:      verificationapp.ErrInvalidCredentials,
			loginPosts:         1,
		},
		{
			name: "challenge from decoded upstream message",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:           "wrong-integration-password",
			educationLevel:     verificationapp.EducationUndergraduate,
			loginResponseError: &loginResponseError{Code: 41100, Msg: "请输入验证码"},
			expectedError:      verificationapp.ErrChallengeRequired,
			loginPosts:         1,
		},
		{
			name: "validation code challenge from decoded upstream message",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:           "wrong-integration-password",
			educationLevel:     verificationapp.EducationUndergraduate,
			loginResponseError: &loginResponseError{Code: 41100, Msg: "需要校验码"},
			expectedError:      verificationapp.ErrChallengeRequired,
			loginPosts:         1,
		},
		{
			name: "restricted account from decoded upstream message",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:           "wrong-integration-password",
			educationLevel:     verificationapp.EducationUndergraduate,
			loginResponseError: &loginResponseError{Code: 41102, Msg: "账号已锁定"},
			expectedError:      verificationapp.ErrAccountRestricted,
			loginPosts:         1,
		},
		{
			name: "invalid credentials with malformed upstream metadata",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:                    "wrong-integration-password",
			educationLevel:              verificationapp.EducationUndergraduate,
			malformedLoginErrorMetadata: true,
			expectedError:               verificationapp.ErrInvalidCredentials,
			loginPosts:                  1,
		},
		{
			name: "generic authentication failure is provider unavailable",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:            "wrong-integration-password",
			educationLevel:      verificationapp.EducationUndergraduate,
			genericLoginFailure: true,
			expectedError:       verificationapp.ErrProviderUnavailable,
			loginPosts:          1,
		},
		{
			name:               "portal rejects identity",
			accepted:           map[string]bool{},
			password:           integrationPassword,
			educationLevel:     verificationapp.EducationUndergraduate,
			portalBody:         `{"e":1,"m":"操作失败","d":null}`,
			expectedError:      verificationapp.ErrProviderUnavailable,
			loginPosts:         1,
			portalIdentityHits: 1,
		},
		{
			name:               "portal response malformed",
			accepted:           map[string]bool{},
			password:           integrationPassword,
			educationLevel:     verificationapp.EducationUndergraduate,
			portalBody:         `{"e":0`,
			expectedError:      verificationapp.ErrProviderUnavailable,
			loginPosts:         1,
			portalIdentityHits: 1,
		},
		{
			name:               "portal student number mismatch",
			accepted:           map[string]bool{},
			password:           integrationPassword,
			educationLevel:     verificationapp.EducationUndergraduate,
			portalBody:         `{"e":0,"d":{"info":{"name":"集成测试同学","xgh":"another-student","identity":"学生"}}}`,
			expectedError:      verificationapp.ErrProviderUnavailable,
			loginPosts:         1,
			portalIdentityHits: 1,
		},
		{
			name: "interactive challenge",
			accepted: map[string]bool{
				verificationapp.EducationUndergraduate: true,
			},
			password:       integrationPassword,
			educationLevel: verificationapp.EducationUndergraduate,
			challenge:      true,
			expectedError:  verificationapp.ErrChallengeRequired,
			loginPosts:     0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, false, test.accepted)
			fake.challenge = test.challenge
			fake.malformedLoginErrorMetadata = test.malformedLoginErrorMetadata
			fake.genericLoginFailure = test.genericLoginFailure
			fake.loginResponseError = test.loginResponseError
			fake.portalBody = test.portalBody
			provider := newIntegrationProvider(t, fake)
			_, err := provider.Verify(
				context.Background(),
				verificationapp.VerificationCommand{
					StudentNo:      integrationStudentNo,
					Password:       test.password,
					EducationLevel: test.educationLevel,
				},
			)
			if !errors.Is(err, test.expectedError) {
				t.Fatalf("verification error=%v want=%v", err, test.expectedError)
			}
			if got := fake.loginPosts.Load(); got != test.loginPosts {
				t.Fatalf("login POST count=%d want=%d", got, test.loginPosts)
			}
			if got := fake.portalIdentityHits.Load(); got != test.portalIdentityHits {
				t.Fatalf(
					"portal identity hit count=%d want=%d",
					got,
					test.portalIdentityHits,
				)
			}
			fake.assertNoAcademicServiceAccessed(t)
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderQueryExposesCredentialErrors(t *testing.T) {
	tests := []struct {
		name          string
		password      string
		configure     func(*fakeOUCServer)
		expectedError error
	}{
		{
			name:          "invalid credentials",
			password:      "wrong-integration-password",
			expectedError: application.ErrInvalidCredentials,
		},
		{
			name:     "password expired without continue control",
			password: integrationPassword,
			configure: func(fake *fakeOUCServer) {
				fake.passwordWarn = true
				fake.passwordWarnWithoutContinue = true
			},
			expectedError: application.ErrPasswordExpired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, false, map[string]bool{
				verificationapp.EducationUndergraduate: true,
			})
			if test.configure != nil {
				test.configure(fake)
			}
			provider := newIntegrationProvider(t, fake)
			_, err := provider.ListCourses(
				context.Background(),
				application.StudentReference{
					UserID:         7,
					StudentNo:      integrationStudentNo,
					Provider:       verificationapp.ProviderOUC,
					EducationLevel: verificationapp.EducationUndergraduate,
				},
				application.Credential{
					StudentNo: integrationStudentNo,
					Password:  test.password,
				},
				"2025-2026-2",
			)
			if !errors.Is(err, test.expectedError) {
				t.Fatalf("query error=%v want=%v", err, test.expectedError)
			}
			if got := fake.continuePosts.Load(); got != 0 {
				t.Fatalf("continue POST count=%d want=0", got)
			}
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderReauthenticatesAfterCachedSessionExpires(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	store := newMemoryOUCSessionStore()
	err := store.Save(
		context.Background(),
		integrationStudentNo,
		integrationPassword,
		SessionState{
			Version: 1,
			Sets: []SessionCookieSet{
				{
					URL: "https://id.ouc.edu.cn/sso/login",
					Cookies: []SessionCookie{
						{Name: "OUC_SSO", Value: "expired"},
					},
				},
			},
		},
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := newIntegrationProviderWithStore(t, fake, store)
	periods, err := provider.ListPeriods(
		context.Background(),
		application.StudentReference{
			UserID:         7,
			StudentNo:      integrationStudentNo,
			Provider:       verificationapp.ProviderOUC,
			EducationLevel: verificationapp.EducationUndergraduate,
		},
		application.Credential{
			StudentNo: integrationStudentNo,
			Password:  integrationPassword,
		},
	)
	if err != nil || len(periods) != 1 {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	fake.assertQueryHit(
		t,
		verificationapp.EducationUndergraduate,
		integrationOUCConfig().Undergraduate.Periods.Path,
	)
	fake.assertNoContractErrors(t)
}

func TestOUCProviderRecoversFromInvalidScopedSessionCache(t *testing.T) {
	tests := []struct {
		name  string
		scope SessionScope
	}{
		{name: "target cache", scope: SessionScopeUndergraduate},
		{name: "identity cache", scope: SessionScopeIdentity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, false, map[string]bool{
				verificationapp.EducationUndergraduate: true,
			})
			store := newMemoryOUCSessionStore()
			key := memoryScopeKey(integrationStudentNo, integrationPassword, test.scope)
			store.scopeLoadErrors[key] = fmt.Errorf("%w: test corruption", errInvalidCachedSession)
			provider := newIntegrationProviderWithStore(t, fake, store)

			periods, err := provider.ListPeriods(
				context.Background(),
				application.StudentReference{
					UserID:         7,
					StudentNo:      integrationStudentNo,
					Provider:       verificationapp.ProviderOUC,
					EducationLevel: verificationapp.EducationUndergraduate,
				},
				application.Credential{
					StudentNo: integrationStudentNo,
					Password:  integrationPassword,
				},
			)
			if err != nil || len(periods) != 1 {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			if got := fake.loginPosts.Load(); got != 1 {
				t.Fatalf("credential login count=%d want=1", got)
			}
			store.mu.RLock()
			_, retained := store.scopeLoadErrors[key]
			store.mu.RUnlock()
			if retained {
				t.Fatal("invalid cache marker was not removed")
			}
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderTreatsScopedSessionCacheReadFailureAsBestEffort(t *testing.T) {
	tests := []struct {
		name  string
		scope SessionScope
	}{
		{name: "target cache", scope: SessionScopeUndergraduate},
		{name: "identity cache", scope: SessionScopeIdentity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, false, map[string]bool{
				verificationapp.EducationUndergraduate: true,
			})
			store := newMemoryOUCSessionStore()
			key := memoryScopeKey(integrationStudentNo, integrationPassword, test.scope)
			store.scopeLoadErrors[key] = errors.New("session cache unavailable")
			provider := newIntegrationProviderWithStore(t, fake, store)

			periods, err := provider.ListPeriods(
				context.Background(),
				application.StudentReference{
					UserID:         7,
					StudentNo:      integrationStudentNo,
					Provider:       verificationapp.ProviderOUC,
					EducationLevel: verificationapp.EducationUndergraduate,
				},
				application.Credential{
					StudentNo: integrationStudentNo,
					Password:  integrationPassword,
				},
			)
			if err != nil || len(periods) != 1 {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			if got := fake.loginPosts.Load(); got != 1 {
				t.Fatalf("credential login count=%d want=1", got)
			}
			store.mu.RLock()
			_, retained := store.scopeLoadErrors[key]
			store.mu.RUnlock()
			if !retained {
				t.Fatal("session-cache infrastructure error was treated as invalid cache")
			}
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderUsesRecoveredSessionWhenTargetCacheWriteFails(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	store := newMemoryOUCSessionStore()
	targetKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeUndergraduate,
	)
	store.scopeSaveErrors[targetKey] = errors.New("target cache unavailable")
	provider := newIntegrationProviderWithStore(t, fake, store)

	periods, err := provider.ListPeriods(
		context.Background(),
		application.StudentReference{
			UserID:         7,
			StudentNo:      integrationStudentNo,
			Provider:       verificationapp.ProviderOUC,
			EducationLevel: verificationapp.EducationUndergraduate,
		},
		application.Credential{
			StudentNo: integrationStudentNo,
			Password:  integrationPassword,
		},
	)
	if err != nil || len(periods) != 1 {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential login count=%d want=1", got)
	}
	store.mu.RLock()
	_, persisted := store.scopedState[targetKey]
	store.mu.RUnlock()
	if persisted {
		t.Fatal("test setup unexpectedly persisted target session")
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderRetriesOnlyWhenCachedOperationProvesSessionExpiry(t *testing.T) {
	tests := []struct {
		name             string
		configure        func(*fakeOUCServer)
		requestTimeoutMS int
		expectedError    error
		expectedLogins   int32
		expectedAttempts int
	}{
		{
			name: "service returns login page",
			configure: func(fake *fakeOUCServer) {
				fake.rejectQuery.Store(true)
			},
			expectedLogins:   1,
			expectedAttempts: 2,
		},
		{
			name: "service redirects to SSO login",
			configure: func(fake *fakeOUCServer) {
				fake.redirectQuery.Store(true)
			},
			expectedLogins:   1,
			expectedAttempts: 2,
		},
		{
			name: "persistent service login page is provider unavailable",
			configure: func(fake *fakeOUCServer) {
				fake.rejectQueryAlways.Store(true)
			},
			expectedError:    application.ErrProviderUnavailable,
			expectedLogins:   1,
			expectedAttempts: 2,
		},
		{
			name: "persistent SSO redirect is provider unavailable",
			configure: func(fake *fakeOUCServer) {
				fake.redirectQueryAlways.Store(true)
			},
			expectedError:    application.ErrProviderUnavailable,
			expectedLogins:   1,
			expectedAttempts: 2,
		},
		{
			name: "service returns bad gateway",
			configure: func(fake *fakeOUCServer) {
				fake.failQuery.Store(true)
			},
			expectedError:    application.ErrProviderUnavailable,
			expectedLogins:   1,
			expectedAttempts: 1,
		},
		{
			name: "service request times out",
			configure: func(fake *fakeOUCServer) {
				fake.slowQuery.Store(true)
			},
			requestTimeoutMS: 1000,
			expectedError:    context.DeadlineExceeded,
			expectedLogins:   1,
			expectedAttempts: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeOUCServer(t, false, map[string]bool{
				verificationapp.EducationUndergraduate: true,
			})
			config := integrationOUCConfig()
			if test.requestTimeoutMS > 0 {
				config.RequestTimeoutMS = test.requestTimeoutMS
			}
			provider := newIntegrationProviderWithConfig(
				t,
				fake,
				newMemoryOUCSessionStore(),
				config,
			)
			student := application.StudentReference{
				UserID:         7,
				StudentNo:      integrationStudentNo,
				Provider:       verificationapp.ProviderOUC,
				EducationLevel: verificationapp.EducationUndergraduate,
			}
			credential := application.Credential{
				StudentNo: integrationStudentNo,
				Password:  integrationPassword,
			}
			periods, err := provider.ListPeriods(
				context.Background(),
				student,
				credential,
			)
			if err != nil || len(periods) != 1 {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			test.configure(fake)
			grades, err := provider.ListGrades(
				context.Background(),
				student,
				credential,
				periods[0].ID,
			)
			if !errors.Is(err, test.expectedError) {
				t.Fatalf("grades error=%v want=%v", err, test.expectedError)
			}
			if test.expectedError == nil && len(grades) != 1 {
				t.Fatalf("grades=%+v want one result", grades)
			}
			if got := fake.loginPosts.Load(); got != test.expectedLogins {
				t.Fatalf("login POST count=%d want=%d", got, test.expectedLogins)
			}
			gradesPath := integrationOUCConfig().Undergraduate.Grades.Path
			fake.queryHitsMu.Lock()
			queryKey := verificationapp.EducationUndergraduate + ":" + gradesPath
			gradeAttempts := fake.queryHits[queryKey]
			fake.queryHitsMu.Unlock()
			if gradeAttempts != test.expectedAttempts {
				t.Fatalf(
					"grade query attempts=%d want=%d",
					gradeAttempts,
					test.expectedAttempts,
				)
			}
			fake.assertNoContractErrors(t)
		})
	}
}

func TestOUCProviderDoesNotRetryTransientSessionSetupFailure(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.failSSO.Store(true)
	provider := newIntegrationProvider(t, fake)
	periods, err := provider.ListPeriods(
		context.Background(),
		application.StudentReference{
			UserID:         7,
			StudentNo:      integrationStudentNo,
			Provider:       verificationapp.ProviderOUC,
			EducationLevel: verificationapp.EducationUndergraduate,
		},
		application.Credential{
			StudentNo: integrationStudentNo,
			Password:  integrationPassword,
		},
	)
	if !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("periods=%+v err=%v want=%v", periods, err, application.ErrProviderUnavailable)
	}
	if got := fake.ssoGets.Load(); got != 1 {
		t.Fatalf("SSO GET count=%d want=1", got)
	}
	if got := fake.loginPosts.Load(); got != 0 {
		t.Fatalf("login POST count=%d want=0", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderUsesValidatedUndergraduateTargetWithoutSSOProbe(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	store := newMemoryOUCSessionStore()
	provider := newIntegrationProviderWithStore(t, fake, store)
	student := application.StudentReference{
		UserID:         7,
		StudentNo:      integrationStudentNo,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{
		StudentNo: integrationStudentNo,
		Password:  integrationPassword,
	}
	periods, err := provider.ListPeriods(context.Background(), student, credential)
	if err != nil || len(periods) != 1 {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	initialSSOGets := fake.ssoGets.Load()
	initialLogins := fake.loginPosts.Load()
	grades, err := provider.ListGrades(context.Background(), student, credential, periods[0].ID)
	if err != nil || len(grades) != 1 {
		t.Fatalf("grades=%+v err=%v", grades, err)
	}
	if got := fake.ssoGets.Load() - initialSSOGets; got != 0 {
		t.Fatalf("SSO GET count=%d want=0", got)
	}
	if got := fake.loginPosts.Load(); got != initialLogins {
		t.Fatalf("login POST count=%d want unchanged %d", got, initialLogins)
	}

	if got := fake.loginPosts.Load(); got != initialLogins {
		t.Fatalf("login POST count=%d want unchanged %d", got, initialLogins)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderKeepsUndergraduateTargetOnIndeterminateProbe(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	store := newMemoryOUCSessionStore()
	provider := newIntegrationProviderWithStore(t, fake, store)
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	if _, err := provider.ListPeriods(context.Background(), student, credential); err != nil {
		t.Fatal(err)
	}
	key := memoryScopeKey(integrationStudentNo, integrationPassword, SessionScopeUndergraduate)
	store.mu.Lock()
	state := store.scopedState[key]
	state.ValidatedAt = 0
	store.scopedState[key] = state
	store.mu.Unlock()
	fake.failUndergraduateProbe.Store(true)
	if _, err := provider.ListGrades(context.Background(), student, credential, "2026-1"); !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("indeterminate probe error=%v want provider unavailable", err)
	}
	store.mu.RLock()
	_, retained := store.scopedState[key]
	store.mu.RUnlock()
	if !retained {
		t.Fatal("undergraduate target session was deleted after a 5xx probe")
	}
	if _, err := provider.ListGrades(context.Background(), student, credential, "2026-1"); err != nil {
		t.Fatalf("query after probe recovery: %v", err)
	}
}

func TestOUCProviderCoalescesScopedRecoveryWithoutSharingCookieJars(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationGraduate: true,
	})
	provider := newIntegrationProviderWithStore(t, fake, newMemoryOUCSessionStore())
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationGraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			periods, err := provider.ListPeriods(context.Background(), student, credential)
			if err == nil && len(periods) != 1 {
				err = fmt.Errorf("period count=%d", len(periods))
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential login count=%d want=1", got)
	}
}

func TestOUCProviderRecoveryDoesNotPersistIdentityCookieInTargetScope(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.serviceStarted = make(chan struct{}, 1)
	fake.serviceRelease = make(chan struct{})
	fake.requireSSOCookieForService.Store(true)
	var releaseService sync.Once
	release := func() {
		releaseService.Do(func() { close(fake.serviceRelease) })
	}
	t.Cleanup(release)
	store := newMemoryOUCSessionStore()
	store.scopeLoadNotify = make(chan struct{}, 8)
	identityKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeIdentity,
	)
	store.scopedState[identityKey] = SessionState{
		Version: 1,
		Sets: []SessionCookieSet{{
			URL: "https://id.ouc.edu.cn/sso/login",
			Cookies: []SessionCookie{{
				Name: "OUC_SSO", Value: "authenticated", Domain: "ouc.edu.cn", Path: "/",
			}},
		}},
	}
	provider := newIntegrationProviderWithStore(t, fake, store)
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	targetKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeUndergraduate,
	)

	results := make(chan error, 2)
	query := func() {
		periods, err := provider.ListPeriods(context.Background(), student, credential)
		if err == nil && len(periods) != 1 {
			err = fmt.Errorf("period count=%d", len(periods))
		}
		results <- err
	}
	go query()
	select {
	case <-fake.serviceStarted:
	case <-time.After(time.Second):
		t.Fatal("leader did not reach target hand-off")
	}
	go query()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		store.mu.RLock()
		loads := store.scopeLoadHits[targetKey]
		store.mu.RUnlock()
		if loads >= 3 {
			break
		}
		select {
		case <-store.scopeLoadNotify:
		case <-deadline.C:
			t.Fatalf("follower did not join recovery; target loads=%d want>=3", loads)
		}
	}
	release()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	if got := fake.loginPosts.Load(); got != 0 {
		t.Fatalf("credential login count=%d want=0", got)
	}
	if got := fake.catalogServiceProbes.Load(); got != 1 {
		t.Fatalf("target hand-off count=%d want=1", got)
	}
	store.mu.RLock()
	target, found := store.scopedState[targetKey]
	store.mu.RUnlock()
	if !found {
		t.Fatal("target session was not persisted after successful queries")
	}
	for _, set := range target.Sets {
		for _, cookie := range set.Cookies {
			if cookie.Name == "OUC_SSO" || cookie.Domain == "ouc.edu.cn" {
				t.Fatalf("target session retained identity cookie: %+v", target)
			}
		}
	}
	if len(target.Sets) != 1 || len(target.Sets[0].Cookies) != 1 ||
		target.Sets[0].Cookies[0].Name != "ACADEMIC_SESSION" {
		t.Fatalf("target session=%+v want only academic cookie", target)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderDoesNotRenewTargetSessionForUnparseableResponse(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	store := newMemoryOUCSessionStore()
	observer := &recordingOUCObserver{}
	config := integrationOUCConfig()
	provider := NewProvider(
		staticConfigResolver{snapshot: academicconfig.Snapshot{
			ActiveProvider: academicconfig.ProviderOUC,
			OUC:            config,
		}},
		WithSessionStore(store),
		withSessionClientFactory(fake.clientFactory(t)),
		WithObserver(observer),
	)
	student := application.StudentReference{
		UserID:         7,
		StudentNo:      integrationStudentNo,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{
		StudentNo: integrationStudentNo,
		Password:  integrationPassword,
	}

	if _, err := provider.ListCourses(context.Background(), student, credential, "2026-1"); err != nil {
		t.Fatal(err)
	}
	targetKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeUndergraduate,
	)
	store.mu.RLock()
	validatedAt := store.scopedState[targetKey].ValidatedAt
	store.mu.RUnlock()
	if validatedAt == 0 {
		t.Fatal("successful query did not validate target session")
	}

	time.Sleep(2 * time.Millisecond)
	fake.malformedQuery.Store(true)
	if _, err := provider.ListCourses(context.Background(), student, credential, "2026-1"); err == nil {
		t.Fatal("unparseable business response unexpectedly succeeded")
	}
	store.mu.RLock()
	after := store.scopedState[targetKey].ValidatedAt
	store.mu.RUnlock()
	if after != validatedAt {
		t.Fatalf("target session renewed after parse failure: before=%d after=%d", validatedAt, after)
	}
	successes := 0
	for _, event := range observer.events {
		if event == "courses/success/undergraduate" {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful upstream attempts=%d, want only the parseable response", successes)
	}
	invalidResponses := 0
	for _, event := range observer.events {
		if event == "courses/invalid_response/undergraduate" {
			invalidResponses++
		}
	}
	if invalidResponses != 1 {
		t.Fatalf("invalid upstream responses=%d, want the parse failure recorded", invalidResponses)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderPreservesIdentitySessionAfterTargetDenial(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
		verificationapp.EducationGraduate:      true,
	})
	store := newMemoryOUCSessionStore()
	provider := newIntegrationProviderWithStore(t, fake, store)
	credential := application.Credential{
		StudentNo: integrationStudentNo,
		Password:  integrationPassword,
	}
	graduate := application.StudentReference{
		UserID:         7,
		StudentNo:      integrationStudentNo,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationGraduate,
	}
	undergraduate := graduate
	undergraduate.EducationLevel = verificationapp.EducationUndergraduate

	if _, err := provider.ListCourses(context.Background(), graduate, credential, "2026-1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("initial credential logins=%d, want 1", got)
	}
	if err := store.DeleteScope(
		context.Background(),
		integrationStudentNo,
		integrationPassword,
		SessionScopeGraduate,
	); err != nil {
		t.Fatal(err)
	}
	fake.accepted[verificationapp.EducationUndergraduate] = false
	if _, err := provider.ListCourses(context.Background(), undergraduate, credential, "2026-1"); !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("undergraduate denial error=%v, want provider unavailable", err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential logins after target denial=%d, want identity reuse without retry", got)
	}
	if _, found, err := store.LoadScope(
		context.Background(),
		integrationStudentNo,
		integrationPassword,
		SessionScopeIdentity,
	); err != nil || !found {
		t.Fatalf("identity session after target denial found=%v err=%v", found, err)
	}
	if _, err := provider.ListCourses(context.Background(), graduate, credential, "2026-1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential logins after cross-scope recovery=%d, want 1", got)
	}
	fake.assertNoContractErrors(t)
}

func TestOUCProviderScopedRecoverySurvivesLeaderCancellation(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.serviceStarted = make(chan struct{}, 1)
	fake.serviceRelease = make(chan struct{})
	provider := newIntegrationProviderWithStore(t, fake, newMemoryOUCSessionStore())
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, err := provider.ListPeriods(leaderCtx, student, credential)
		leaderResult <- err
	}()
	select {
	case <-fake.serviceStarted:
	case <-time.After(time.Second):
		t.Fatal("leader did not reach target hand-off")
	}
	cancelLeader()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error=%v want context canceled", err)
	}

	followerResult := make(chan error, 1)
	go func() {
		periods, err := provider.ListPeriods(context.Background(), student, credential)
		if err == nil && len(periods) != 1 {
			err = fmt.Errorf("period count=%d", len(periods))
		}
		followerResult <- err
	}()
	close(fake.serviceRelease)
	select {
	case err := <-followerResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not reuse detached recovery")
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential login count=%d want=1", got)
	}
}

func TestOUCProviderScopedRecoveryRetriesAfterSharedLeaderDeadline(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.serviceStarted = make(chan struct{}, 1)
	fake.serviceRelease = make(chan struct{})
	var releaseService sync.Once
	release := func() {
		releaseService.Do(func() { close(fake.serviceRelease) })
	}
	t.Cleanup(release)
	store := newMemoryOUCSessionStore()
	store.scopeLoadNotify = make(chan struct{}, 8)
	provider := newIntegrationProviderWithStore(t, fake, store)
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	targetKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeUndergraduate,
	)

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), time.Second)
	defer cancelLeader()
	leaderResult := make(chan error, 1)
	go func() {
		_, err := provider.ListPeriods(leaderCtx, student, credential)
		leaderResult <- err
	}()
	select {
	case <-fake.serviceStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not reach target hand-off")
	}

	followerCtx, cancelFollower := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFollower()
	followerResult := make(chan error, 1)
	go func() {
		periods, err := provider.ListPeriods(followerCtx, student, credential)
		if err == nil && len(periods) != 1 {
			err = fmt.Errorf("period count=%d", len(periods))
		}
		followerResult <- err
	}()

	joinDeadline := time.NewTimer(time.Second)
	defer joinDeadline.Stop()
	for {
		store.mu.RLock()
		loads := store.scopeLoadHits[targetKey]
		store.mu.RUnlock()
		if loads >= 3 {
			break
		}
		select {
		case <-store.scopeLoadNotify:
		case <-joinDeadline.C:
			t.Fatalf("follower did not join recovery; target loads=%d want>=3", loads)
		}
	}

	select {
	case err := <-leaderResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("leader error=%v want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader exceeded its original deadline")
	}
	select {
	case <-fake.serviceStarted:
		// The still-valid follower became the next leader after the first flight
		// reached its initiating caller's deadline.
	case err := <-followerResult:
		t.Fatalf("follower inherited leader deadline before retry: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not re-elect recovery after shared deadline")
	}
	release()
	select {
	case err := <-followerResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not complete retried recovery")
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("credential login count=%d want=1", got)
	}
	if got := fake.catalogServiceProbes.Load(); got != 2 {
		t.Fatalf("target hand-off count=%d want=2", got)
	}
}

func TestOUCProviderDoesNotRetrySharedInnerDeadlineBeforeLeaderDeadline(t *testing.T) {
	store := newMemoryOUCSessionStore()
	store.scopeLoadNotify = make(chan struct{}, 8)
	config := integrationOUCConfig()
	config.RequestTimeoutMS = 5000
	clientStarted := make(chan struct{}, 1)
	clientRelease := make(chan struct{})
	var clientLoads atomic.Int32
	provider := NewProvider(
		staticConfigResolver{snapshot: academicconfig.Snapshot{
			ActiveProvider: academicconfig.ProviderOUC,
			OUC:            config,
		}},
		WithSessionStore(store),
		withSessionClientFactory(func(academicconfig.OUCConfig) (*http.Client, error) {
			clientLoads.Add(1)
			select {
			case clientStarted <- struct{}{}:
			default:
			}
			<-clientRelease
			return nil, context.DeadlineExceeded
		}),
	)
	student := application.StudentReference{
		UserID: 7, StudentNo: integrationStudentNo, Provider: verificationapp.ProviderOUC,
		EducationLevel: verificationapp.EducationUndergraduate,
	}
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	targetKey := memoryScopeKey(
		integrationStudentNo,
		integrationPassword,
		SessionScopeUndergraduate,
	)

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelLeader()
	results := make(chan error, 2)
	go func() {
		_, err := provider.ListPeriods(leaderCtx, student, credential)
		results <- err
	}()
	select {
	case <-clientStarted:
	case <-time.After(time.Second):
		t.Fatal("leader did not start session client creation")
	}
	go func() {
		_, err := provider.ListPeriods(context.Background(), student, credential)
		results <- err
	}()

	joinDeadline := time.NewTimer(time.Second)
	defer joinDeadline.Stop()
	for {
		store.mu.RLock()
		loads := store.scopeLoadHits[targetKey]
		store.mu.RUnlock()
		if loads >= 3 {
			break
		}
		select {
		case <-store.scopeLoadNotify:
		case <-joinDeadline.C:
			t.Fatalf("follower did not join recovery; target loads=%d want>=3", loads)
		}
	}
	// Keep the first call in flight briefly after the follower's outer cache
	// load, ensuring it reaches DoChan before the injected loader error returns.
	time.Sleep(25 * time.Millisecond)
	close(clientRelease)
	for range 2 {
		select {
		case err := <-results:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("shared inner error=%v want deadline exceeded", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shared inner deadline result did not return")
		}
	}
	if err := leaderCtx.Err(); err != nil {
		t.Fatalf("leader deadline unexpectedly expired: %v", err)
	}
	if got := clientLoads.Load(); got != 1 {
		t.Fatalf("session client loads=%d want=1 without deadline retry", got)
	}
}

func TestOUCProviderLegacyCatalogCacheIsSeparatedByEducationLevel(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
		verificationapp.EducationGraduate:      true,
	})
	config := integrationOUCConfig()
	config.Graduate.CourseCatalog = academicconfig.OperationEndpoint{
		Path: "/graduate/catalog", RequestMethod: http.MethodGet, RequestEncoding: "query",
		PeriodParameter: "xnxqval", PageParameter: "pageNum", PageSizeParameter: "pageSize",
		PageSize: 20, ResponseEncoding: "html",
	}
	memoryStore := newMemoryOUCSessionStore()
	provider := newIntegrationProviderWithConfig(t, fake, &legacyOnlySessionStore{store: memoryStore}, config)
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	for _, test := range []struct {
		educationLevel string
		periodID       string
	}{
		{verificationapp.EducationUndergraduate, "2026-2027-1"},
		{verificationapp.EducationGraduate, "2026:11"},
	} {
		if _, err := provider.ListCourseCatalogPage(
			context.Background(), credential, test.educationLevel, test.periodID, 1,
		); err != nil {
			t.Fatalf("%s catalog: %v", test.educationLevel, err)
		}
	}
	memoryStore.mu.RLock()
	defer memoryStore.mu.RUnlock()
	if len(memoryStore.states) != 2 {
		t.Fatalf("legacy catalog cache entries=%d want=2", len(memoryStore.states))
	}
	for _, educationLevel := range []string{
		verificationapp.EducationUndergraduate,
		verificationapp.EducationGraduate,
	} {
		key := educationLevel + "\x00" + integrationStudentNo + "\x00" + integrationPassword
		if _, found := memoryStore.states[key]; !found {
			t.Fatalf("legacy catalog cache missing %s scope", educationLevel)
		}
	}
}

func TestOUCProviderRetriesMalformedCatalogResponseOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.malformedCatalog.Store(1)
	provider := newIntegrationProvider(t, fake)
	page, err := provider.ListCourseCatalogPage(
		context.Background(),
		application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword},
		verificationapp.EducationUndergraduate,
		"2026-2027-1",
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page != 3 || page.TotalCount != 100 || len(page.Entries) != 1 {
		t.Fatalf("page=%+v", page)
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	fake.queryHitsMu.Lock()
	attempts := fake.queryHits[verificationapp.EducationUndergraduate+":"+integrationOUCConfig().Undergraduate.CourseCatalog.Path]
	fake.queryHitsMu.Unlock()
	if attempts != 2 {
		t.Fatalf("catalog query attempts=%d want=2", attempts)
	}
}

func TestOUCProviderDoesNotProbePortalForCachedCatalogPage(t *testing.T) {
	t.Parallel()
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	provider := newIntegrationProvider(t, fake)
	credential := application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword}
	for page := 1; page <= 2; page++ {
		if _, err := provider.ListCourseCatalogPage(
			context.Background(), credential, verificationapp.EducationUndergraduate, "2026-2027-1", page,
		); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.loginPosts.Load(); got != 1 {
		t.Fatalf("login POST count=%d want=1", got)
	}
	if got := fake.catalogServiceProbes.Load(); got != 1 {
		t.Fatalf("catalog service probes=%d want=1", got)
	}
}

func TestOUCProviderRejectsRepeatedMalformedCatalogResponse(t *testing.T) {
	t.Parallel()
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.malformedCatalog.Store(3)
	provider := newIntegrationProvider(t, fake)
	_, err := provider.ListCourseCatalogPage(
		context.Background(),
		application.Credential{StudentNo: integrationStudentNo, Password: integrationPassword},
		verificationapp.EducationUndergraduate,
		"2026-2027-1",
		4,
	)
	if !errors.Is(err, application.ErrContractChanged) {
		t.Fatalf("error=%v want=%v", err, application.ErrContractChanged)
	}
}

func newIntegrationProvider(t *testing.T, fake *fakeOUCServer) *Provider {
	t.Helper()
	return newIntegrationProviderWithStore(
		t,
		fake,
		newMemoryOUCSessionStore(),
	)
}

func newIntegrationProviderWithStore(
	t *testing.T,
	fake *fakeOUCServer,
	store SessionStore,
) *Provider {
	t.Helper()
	return newIntegrationProviderWithConfig(
		t,
		fake,
		store,
		integrationOUCConfig(),
	)
}

func newIntegrationProviderWithConfig(
	t *testing.T,
	fake *fakeOUCServer,
	store SessionStore,
	config academicconfig.OUCConfig,
) *Provider {
	t.Helper()
	return NewProvider(
		staticConfigResolver{snapshot: academicconfig.Snapshot{
			ActiveProvider: academicconfig.ProviderOUC,
			OUC:            config,
		}},
		WithSessionStore(store),
		withSessionClientFactory(fake.clientFactory(t)),
	)
}

func integrationOUCConfig() academicconfig.OUCConfig {
	operation := func(path string) academicconfig.OperationEndpoint {
		return academicconfig.OperationEndpoint{
			Path:             path,
			RequestMethod:    http.MethodGet,
			RequestEncoding:  "query",
			PeriodParameter:  "semester",
			ResponseEncoding: "json",
		}
	}
	undergraduate := academicconfig.EndpointSet{
		ServiceURL: "https://jwgl2024.ouc.edu.cn/",
		Periods:    operation("/undergraduate/periods"),
		Courses:    operation("/undergraduate/courses"),
		Grades:     operation("/undergraduate/grades"),
		Exams:      operation("/undergraduate/exams"),
		Selections: operation("/undergraduate/selections/{period_id}"),
	}
	undergraduate.CourseCatalog = academicconfig.OperationEndpoint{
		Path: "/undergraduate/catalog", RequestMethod: http.MethodGet, RequestEncoding: "query",
		PeriodParameter: "xnxqval", PageParameter: "pageNum", PageSizeParameter: "pageSize",
		PageSize: 20, ResponseEncoding: "json",
	}
	undergraduate.Periods.PeriodParameter = ""
	undergraduate.Selections.PeriodParameter = ""
	graduate := academicconfig.EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		Periods:    operation("/graduate/periods"),
		Courses:    operation("/graduate/courses"),
		Grades:     operation("/graduate/grades"),
		Exams:      operation("/graduate/exams"),
		Selections: operation("/graduate/selections/{period_id}"),
	}
	graduate.Periods.PeriodParameter = ""
	graduate.Selections.PeriodParameter = ""
	return academicconfig.OUCConfig{
		Version:                 1,
		SSOLoginURL:             "https://id.ouc.edu.cn/sso/login",
		PortalServiceURL:        "https://my.ouc.edu.cn/manage/common/cas_login/2?redirect=https%3A%2F%2Fmy.ouc.edu.cn%2Ffrontend%2Fuser%2Finfo",
		PortalNoRedirect:        true,
		RequestTimeoutMS:        5000,
		SessionTTLSeconds:       900,
		MaxResponseBytes:        1 << 20,
		UserAgent:               "Campus-OUC-Integration-Test/1.0",
		IndexSelectionSessionID: "selection-session-test",
		Undergraduate:           undergraduate,
		Graduate:                graduate,
	}
}

var _ SessionStore = (*memoryOUCSessionStore)(nil)
