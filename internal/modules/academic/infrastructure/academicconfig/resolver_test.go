package academicconfig

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type sourceStub map[string]string

func (s sourceStub) ListDecrypted(context.Context, string) (map[string]string, error) {
	return s, nil
}

func validOUCConfig() OUCConfig {
	return OUCConfig{
		Version:          1,
		RequestTimeoutMS: 12000,
		MaxResponseBytes: 64 * 1024,
		SSOLoginURL:      "https://id.ouc.edu.cn/sso/login",
		PortalServiceURL: "https://my.ouc.edu.cn/manage/common/cas_login/2",
		Undergraduate: EndpointSet{
			ServiceURL: "https://jwgl2024.ouc.edu.cn/",
		},
		Graduate: EndpointSet{
			ServiceURL: "https://pgs.ouc.edu.cn/",
		},
	}
}

func TestResolverValidatesDynamicConfig(t *testing.T) {
	t.Parallel()
	validOUC := `{
		"version":1,
		"sso_login_url":"https://id.ouc.edu.cn/sso/login",
		"portal_service_url":"https://my.ouc.edu.cn/manage/common/cas_login/2?redirect=https%3A%2F%2Fmy.ouc.edu.cn%2Ffrontend%2Fuser%2Finfo",
		"portal_no_auto_redirect":true,
		"trace_enabled":true,
		"request_timeout_ms":12000,
		"session_ttl_seconds":900,
		"max_response_bytes":2097152,
		"user_agent":"Campus Miniapp Academic/1.0",
		"undergraduate":{
			"service_url":"https://jwgl2024.ouc.edu.cn/",
			"periods":{"path":"/api/periods","request_method":"GET","request_encoding":"query","response_encoding":"json"},
			"courses":{"path":"/api/courses","request_method":"GET","request_encoding":"query","period_parameter":"period_id","response_encoding":"json"},
			"grades":{"path":"/api/grades","request_method":"POST","request_encoding":"json","period_parameter":"semester","response_encoding":"json"},
			"exams":{"path":"/api/exams","request_method":"GET","request_encoding":"query","period_parameter":"period_id","response_encoding":"json"},
			"selections":{"path":"/api/selections","request_method":"GET","request_encoding":"query","period_parameter":"period_id","response_encoding":"json"}
		},
		"graduate":{
			"service_url":"https://pgs.ouc.edu.cn/allogene/page/home.htm",
			"periods":{"path":"/api/periods","request_method":"GET","request_encoding":"query","response_encoding":"json"},
			"courses":{"path":"/api/courses","request_method":"POST","request_encoding":"form","period_parameter":"semesterId","response_encoding":"json"},
			"grades":{"path":"/api/grades","request_method":"POST","request_encoding":"form","period_parameter":"semesterId","response_encoding":"json"},
			"exams":{"path":"/api/exams","request_method":"POST","request_encoding":"form","period_parameter":"semesterId","response_encoding":"json"},
			"selections":{"path":"/api/selections","request_method":"POST","request_encoding":"form","period_parameter":"semesterId","response_encoding":"json"}
		}
	}`
	resolver, err := NewResolver(
		context.Background(),
		sourceStub{"active_provider": "ouc", "ouc": validOUC},
		0,
		ProviderPolicy{AllowOUC: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.Resolve()
	if got.ActiveProvider != ProviderOUC ||
		got.OUC.Version != 1 ||
		!got.OUC.TraceEnabled {
		t.Fatalf("snapshot=%+v", got)
	}
	if got.OUC.SSOSessionTTL() != 15*time.Minute {
		t.Fatalf("SSOSessionTTL()=%s, want 15m for legacy-only payload", got.OUC.SSOSessionTTL())
	}
	for _, educationLevel := range []string{"undergraduate", "graduate"} {
		if ttl := got.OUC.TargetSessionTTL(educationLevel); ttl != time.Hour {
			t.Fatalf("TargetSessionTTL(%q)=%s, want 1h for legacy-only payload", educationLevel, ttl)
		}
	}
	if got.OUC.UndergraduateProbePath() != "/jsxsd/framework/xsMainV.htmlx" {
		t.Fatalf("UndergraduateProbePath()=%q, want documented default", got.OUC.UndergraduateProbePath())
	}
	if got.OUC.UndergraduateValidationWindow() != 5*time.Minute {
		t.Fatalf("UndergraduateValidationWindow()=%s, want 5m", got.OUC.UndergraduateValidationWindow())
	}
}

func TestValidateOUCRejectsUnsafeSelectionSessionID(t *testing.T) {
	t.Parallel()
	config := validOUCConfig()
	config.IndexSelectionSessionID = "bad value"
	if err := validateOUC(config); err == nil || !strings.Contains(err.Error(), "index_selection_session_id") {
		t.Fatalf("validateOUC() error=%v", err)
	}
}

func TestResolverRejectsDowngradeAndProductionMock(t *testing.T) {
	t.Parallel()
	if _, err := NewResolver(
		context.Background(),
		sourceStub{"active_provider": "mock"},
		0,
		ProviderPolicy{AllowOUC: true},
		nil,
	); err == nil {
		t.Fatal("production mock provider was accepted")
	}
	if err := validateURL("http://id.ouc.edu.cn:8071/sso/login", "id.ouc.edu.cn"); err == nil {
		t.Fatal("HTTP downgrade was accepted")
	}
}

func TestValidateOUCRejectsRequestTimeoutAboveOperationBudget(t *testing.T) {
	config := OUCConfig{Version: 1, RequestTimeoutMS: 12001}
	if err := validateOUC(config); err == nil || !strings.Contains(err.Error(), "between 1000 and 12000") {
		t.Fatalf("validateOUC() error=%v, want request timeout bound", err)
	}
}

func TestValidateOUCRejectsMixedLegacyAndScopedSessionTTLs(t *testing.T) {
	config := OUCConfig{
		Version:                               1,
		RequestTimeoutMS:                      12000,
		SessionTTLSeconds:                     900,
		SSOSessionTTLSeconds:                  900,
		UndergraduateSessionTTLSeconds:        3600,
		GraduateSessionTTLSeconds:             3600,
		UndergraduateSessionProbePath:         "/jsxsd/framework/xsMainV.htmlx",
		UndergraduateSessionValidationSeconds: 300,
	}
	if err := validateOUC(config); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("validateOUC() error=%v, want mixed-TTL rejection", err)
	}
}

func TestValidateOUCScopedSessionSettings(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantErr    string
		wantSSO    time.Duration
		wantUnder  time.Duration
		wantGrad   time.Duration
		wantWindow time.Duration
	}{
		{
			name: "explicit zeros use defaults",
			payload: `{
				"sso_session_ttl_seconds":0,
				"undergraduate_session_ttl_seconds":0,
				"graduate_session_ttl_seconds":0,
				"undergraduate_session_validation_seconds":0
			}`,
			wantSSO:    15 * time.Minute,
			wantUnder:  time.Hour,
			wantGrad:   time.Hour,
			wantWindow: 5 * time.Minute,
		},
		{
			name:    "negative sso TTL is rejected",
			payload: `{"sso_session_ttl_seconds":-1}`,
			wantErr: "sso_session_ttl_seconds must be between 60 and 3600",
		},
		{
			name:    "undergraduate TTL above limit is rejected",
			payload: `{"undergraduate_session_ttl_seconds":3601}`,
			wantErr: "undergraduate_session_ttl_seconds must be between 60 and 3600",
		},
		{
			name:    "negative graduate TTL is rejected",
			payload: `{"graduate_session_ttl_seconds":-1}`,
			wantErr: "graduate_session_ttl_seconds must be between 60 and 3600",
		},
		{
			name:    "validation window above limit is rejected",
			payload: `{"undergraduate_session_validation_seconds":3601}`,
			wantErr: "undergraduate_session_validation_seconds must be between 30 and 3600",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validOUCConfig()
			if err := json.Unmarshal([]byte(test.payload), &config); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}

			err := validateOUC(config)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("validateOUC() error=%v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateOUC() error=%v", err)
			}
			if got := config.SSOSessionTTL(); got != test.wantSSO {
				t.Fatalf("SSOSessionTTL()=%s, want %s", got, test.wantSSO)
			}
			if got := config.TargetSessionTTL("undergraduate"); got != test.wantUnder {
				t.Fatalf("TargetSessionTTL(undergraduate)=%s, want %s", got, test.wantUnder)
			}
			if got := config.TargetSessionTTL("graduate"); got != test.wantGrad {
				t.Fatalf("TargetSessionTTL(graduate)=%s, want %s", got, test.wantGrad)
			}
			if got := config.UndergraduateValidationWindow(); got != test.wantWindow {
				t.Fatalf("UndergraduateValidationWindow()=%s, want %s", got, test.wantWindow)
			}
		})
	}
}

func TestValidateOUCValidatesUndergraduateSessionProbePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
		errPart string
	}{
		{
			name: "plain path is accepted",
			path: "/jsxsd/framework/xsMainV.htmlx",
		},
		{
			name:    "query is rejected",
			path:    "/jsxsd/framework/xsMainV.htmlx?ticket=secret",
			wantErr: true,
			errPart: "query and fragment are not allowed",
		},
		{
			name:    "fragment is rejected",
			path:    "/jsxsd/framework/xsMainV.htmlx#profile",
			wantErr: true,
			errPart: "query and fragment are not allowed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validOUCConfig()
			config.UndergraduateSessionProbePath = test.path
			err := validateOUC(config)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateOUC() error=%v wantErr=%v", err, test.wantErr)
			}
			if test.errPart != "" && !strings.Contains(err.Error(), test.errPart) {
				t.Fatalf("validateOUC() error=%v, want %q", err, test.errPart)
			}
		})
	}

	operation := OperationEndpoint{
		Path:             "/py/page/student/grkcb.htm?zc=-1",
		RequestMethod:    "GET",
		RequestEncoding:  "query",
		ResponseEncoding: "html",
	}
	if err := validateOperation(operation, false); err != nil {
		t.Fatalf("validateOperation() rejected fixed query parameters: %v", err)
	}
}

func TestResolverReviewPolicyRejectsOUCProvider(t *testing.T) {
	t.Parallel()
	if _, err := NewResolver(
		context.Background(),
		sourceStub{"active_provider": "ouc"},
		0,
		ProviderPolicy{AllowMock: true},
		nil,
	); err == nil {
		t.Fatal("review provider policy accepted OUC")
	}
}

func TestResolverReviewPolicyKeepsMockAfterOUCHotReload(t *testing.T) {
	t.Parallel()
	source := sourceStub{"active_provider": "mock"}
	resolver, err := NewResolver(
		context.Background(),
		source,
		0,
		ProviderPolicy{AllowMock: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	source["active_provider"] = "ouc"
	if err = resolver.refresh(context.Background()); err == nil {
		t.Fatal("review provider policy accepted OUC during refresh")
	}
	if got := resolver.Resolve().ActiveProvider; got != ProviderMock {
		t.Fatalf("active provider=%q", got)
	}
}

func TestResolverExposesDevelopmentMockCredentialsFromDynamicConfig(t *testing.T) {
	t.Parallel()
	const fixture = `[{"student_no":"20260001","password_hash":"bcrypt"}]`
	resolver, err := NewResolver(
		context.Background(),
		sourceStub{
			"active_provider":  "mock",
			"mock_credentials": fixture,
		},
		0,
		ProviderPolicy{AllowMock: true, AllowOUC: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolver.MockCredentials(); got != fixture {
		t.Fatalf("mock credentials=%q", got)
	}
}

func TestValidateOperationRejectsInvalidDynamicContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		endpoint OperationEndpoint
	}{
		{
			name: "GET with JSON body",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "json",
				PeriodParameter:  "semester",
				ResponseEncoding: "json",
			},
		},
		{
			name: "POST with query encoding",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "POST",
				RequestEncoding:  "query",
				PeriodParameter:  "semester",
				ResponseEncoding: "json",
			},
		},
		{
			name: "period operation without parameter",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				ResponseEncoding: "json",
			},
		},
		{
			name: "absolute URL path",
			endpoint: OperationEndpoint{
				Path:             "https://attacker.example/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				PeriodParameter:  "semester",
				ResponseEncoding: "json",
			},
		},
		{
			name: "single and multiple period parameters",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				PeriodParameter:  "semester",
				PeriodParameters: []string{"xn", "xj"},
				PeriodSeparator:  ":",
				ResponseEncoding: "json",
			},
		},
		{
			name: "multiple parameters without separator",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				PeriodParameters: []string{"xn", "xj"},
				ResponseEncoding: "json",
			},
		},
		{
			name: "duplicate multiple parameters",
			endpoint: OperationEndpoint{
				Path:             "/api/courses",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				PeriodParameters: []string{"xn", "xn"},
				PeriodSeparator:  ":",
				ResponseEncoding: "json",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateOperation(test.endpoint, true); err == nil {
				t.Fatal("invalid operation contract was accepted")
			}
		})
	}
}

func TestValidateOperationAcceptsMultiplePeriodParameters(t *testing.T) {
	t.Parallel()
	endpoint := OperationEndpoint{
		Path:             "/py/page/student/grkcb.htm?zc=-1",
		RequestMethod:    "GET",
		RequestEncoding:  "query",
		PeriodParameters: []string{"xn", "xj"},
		PeriodSeparator:  ":",
		ResponseEncoding: "html",
	}
	if err := validateOperation(endpoint, true); err != nil {
		t.Fatalf("multiple period parameters were rejected: %v", err)
	}
}

func TestValidateEndpointAllowsUnfilteredGrades(t *testing.T) {
	t.Parallel()
	operation := OperationEndpoint{
		Path:             "/py/page/student/grkcgl.htm",
		RequestMethod:    "GET",
		RequestEncoding:  "query",
		ResponseEncoding: "html",
	}
	endpoint := EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		Grades:     operation,
	}
	if err := validateEndpoint(endpoint, "pgs.ouc.edu.cn"); err != nil {
		t.Fatalf("unfiltered grade operation was rejected: %v", err)
	}
}

func TestValidateEndpointAcceptsCoursesFallback(t *testing.T) {
	t.Parallel()
	endpoint := EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		CoursesFallback: OperationEndpoint{
			Path:             "/py/page/student/xkgrcx.htm",
			RequestMethod:    "GET",
			RequestEncoding:  "query",
			PeriodParameters: []string{"xn", "xj"},
			PeriodSeparator:  ":",
			ResponseEncoding: "html",
		},
	}
	if err := validateEndpoint(endpoint, "pgs.ouc.edu.cn"); err != nil {
		t.Fatalf("courses fallback operation was rejected: %v", err)
	}
}

func TestValidateEndpointAllowsLocallyFilteredSelections(t *testing.T) {
	t.Parallel()
	operation := OperationEndpoint{
		Path:             "/py/page/student/grkcgl.htm",
		RequestMethod:    "GET",
		RequestEncoding:  "query",
		ResponseEncoding: "html",
	}
	endpoint := EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		Selections: operation,
	}
	if err := validateEndpoint(endpoint, "pgs.ouc.edu.cn"); err != nil {
		t.Fatalf("locally filtered selection operation was rejected: %v", err)
	}
}

func TestValidateEndpointAllowsOperationsPendingCapture(t *testing.T) {
	t.Parallel()
	endpoint := EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
	}
	if err := validateEndpoint(endpoint, "pgs.ouc.edu.cn"); err != nil {
		t.Fatalf("optional operations were rejected: %v", err)
	}
}

func TestValidateCourseCatalogOperation(t *testing.T) {
	t.Parallel()
	endpoint := EndpointSet{
		ServiceURL: "https://jwgl2024.ouc.edu.cn/",
		CourseCatalog: OperationEndpoint{
			Path: "/jsxsd/xkgl/loadXkkbList", RequestMethod: "GET", RequestEncoding: "query",
			PeriodParameter: "xnxqval", PageParameter: "pageNum", PageSizeParameter: "pageSize", PageSize: 20,
			ResponseEncoding: "json",
		},
	}
	if err := validateEndpoint(endpoint, "jwgl2024.ouc.edu.cn"); err != nil {
		t.Fatalf("catalog operation rejected: %v", err)
	}
	endpoint.CourseCatalog.PageParameter = ""
	if err := validateEndpoint(endpoint, "jwgl2024.ouc.edu.cn"); err == nil {
		t.Fatal("catalog operation without a page parameter was accepted")
	}
}

func TestValidateCourseCatalogAllowsPageSize500(t *testing.T) {
	t.Parallel()
	endpoint := EndpointSet{
		ServiceURL: "https://jwgl2024.ouc.edu.cn/",
		CourseCatalog: OperationEndpoint{
			Path: "/jsxsd/xkgl/loadXkkbList", RequestMethod: "GET", RequestEncoding: "query",
			PeriodParameter: "xnxqval", PageParameter: "pageNum", PageSizeParameter: "pageSize", PageSize: MaxCourseCatalogPageSize,
			ResponseEncoding: "json",
		},
	}
	if err := validateEndpoint(endpoint, "jwgl2024.ouc.edu.cn"); err != nil {
		t.Fatalf("catalog operation with page size %d rejected: %v", MaxCourseCatalogPageSize, err)
	}
	endpoint.CourseCatalog.PageSize++
	if err := validateEndpoint(endpoint, "jwgl2024.ouc.edu.cn"); err == nil {
		t.Fatal("catalog operation above the page-size limit was accepted")
	}
}

func TestValidateCourseCatalogAllowsServerFixedPageSize(t *testing.T) {
	t.Parallel()
	endpoint := EndpointSet{
		ServiceURL: "https://pgs.ouc.edu.cn/",
		CourseCatalog: OperationEndpoint{
			Path: "/py/page/student/lnsjCxdc.htm", RequestMethod: "POST", RequestEncoding: "form",
			PeriodParameters: []string{"kkxn", "kckkxj"}, PeriodSeparator: ":",
			PageParameter: "pageId", PageSize: 20, ResponseEncoding: "html",
		},
	}
	if err := validateEndpoint(endpoint, "pgs.ouc.edu.cn"); err != nil {
		t.Fatalf("catalog operation with server-fixed page size rejected: %v", err)
	}
}
