package ouc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

type recordingOUCObserver struct {
	events []string
}

type recordingContractDiagnosticCapture struct {
	samples []ContractDiagnosticSample
}

func (c *recordingContractDiagnosticCapture) Capture(sample ContractDiagnosticSample) {
	c.samples = append(c.samples, sample)
}

func (o *recordingOUCObserver) ObserveAcademicUpstreamAttempt(operation, outcome, educationLevel string) {
	o.events = append(o.events, strings.Join([]string{operation, outcome, educationLevel}, "/"))
}

func (o *recordingOUCObserver) ObserveAcademicSessionCache(educationLevel, outcome string) {
	o.events = append(o.events, strings.Join([]string{"session_cache", educationLevel, outcome}, "/"))
}

func (o *recordingOUCObserver) ObserveAcademicSessionRecovery(educationLevel, stage, outcome string) {
	o.events = append(o.events, strings.Join([]string{"session_recovery", educationLevel, stage, outcome}, "/"))
}

func TestWithObserverInjectsBoundedAttemptSink(t *testing.T) {
	observer := &recordingOUCObserver{}
	provider := NewProvider(nil, WithObserver(observer))
	provider.observer.ObserveAcademicUpstreamAttempt("courses", "success", "undergraduate")
	if len(observer.events) != 1 || observer.events[0] != "courses/success/undergraduate" {
		t.Fatalf("observer events=%v", observer.events)
	}
}

func TestTraceQueryParseCapturesCompleteFailedResponse(t *testing.T) {
	capture := &recordingContractDiagnosticCapture{}
	body := []byte("<html>unexpected selection table</html>")
	traceQueryParse(queryResponse{
		trace: &processTrace{},
		diagnostic: ContractDiagnosticSample{
			Body:      body,
			Encoding:  "html",
			Operation: "query.selections",
			Failure:   "contract_error",
		},
		capture: capture,
	}, errors.New("selection parser contract changed"), 0)
	if len(capture.samples) != 1 {
		t.Fatalf("capture count=%d, want 1", len(capture.samples))
	}
	if string(capture.samples[0].Body) != string(body) {
		t.Fatalf("captured body=%q, want %q", capture.samples[0].Body, body)
	}
}

func TestUpstreamAttemptOutcome(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		fallback string
		want     string
	}{
		{name: "canceled", err: context.Canceled, fallback: "request_error", want: "canceled"},
		{name: "deadline", err: context.DeadlineExceeded, fallback: "request_error", want: "deadline"},
		{name: "wrapped deadline", err: fmt.Errorf("query: %w", context.DeadlineExceeded), fallback: "session_error", want: "deadline"},
		{name: "fallback", err: errors.New("network"), fallback: "request_error", want: "request_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamAttemptOutcome(test.err, test.fallback); got != test.want {
				t.Fatalf("outcome=%q, want %q", got, test.want)
			}
		})
	}
}

func TestQueryProviderErrorPreservesContextTermination(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "deadline", err: fmt.Errorf("request: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
		{name: "canceled", err: fmt.Errorf("request: %w", context.Canceled), want: context.Canceled},
		{name: "retryable", err: verificationapp.NewProviderRetryableError(errors.New("temporary upstream")), want: application.ErrProviderRetryable},
		{name: "invalid credentials", err: verificationapp.ErrInvalidCredentials, want: application.ErrInvalidCredentials},
		{name: "password expired", err: verificationapp.ErrPasswordExpired, want: application.ErrPasswordExpired},
		{name: "challenge", err: verificationapp.ErrChallengeRequired, want: application.ErrChallengeRequired},
		{name: "restricted account", err: verificationapp.ErrAccountRestricted, want: application.ErrAccountRestricted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := queryProviderError(test.err); !errors.Is(got, test.want) {
				t.Fatalf("queryProviderError()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestDetachedRecoveryContextPreservesEarlierDeadlineAndIgnoresCancellation(t *testing.T) {
	tests := []struct {
		name              string
		parentTimeout     time.Duration
		configuredTimeout time.Duration
		wantParent        bool
	}{
		{
			name:              "incoming deadline is earlier",
			parentTimeout:     time.Minute,
			configuredTimeout: 2 * time.Minute,
			wantParent:        true,
		},
		{
			name:              "configured budget is earlier",
			parentTimeout:     2 * time.Minute,
			configuredTimeout: time.Minute,
			wantParent:        false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parentDeadline := time.Now().Add(test.parentTimeout)
			parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
			before := time.Now()
			recovery, cancelRecovery := detachedRecoveryContext(parent, test.configuredTimeout)
			t.Cleanup(cancelRecovery)

			gotDeadline, ok := recovery.Deadline()
			if !ok {
				t.Fatal("detached recovery has no deadline")
			}
			if test.wantParent {
				if !gotDeadline.Equal(parentDeadline) {
					t.Fatalf("recovery deadline=%s want parent deadline=%s", gotDeadline, parentDeadline)
				}
			} else {
				minimum := before.Add(test.configuredTimeout)
				maximum := time.Now().Add(test.configuredTimeout)
				if gotDeadline.Before(minimum) || gotDeadline.After(maximum) {
					t.Fatalf("recovery deadline=%s outside configured budget [%s,%s]", gotDeadline, minimum, maximum)
				}
			}

			cancelParent()
			if err := recovery.Err(); err != nil {
				t.Fatalf("parent cancellation propagated to detached recovery: %v", err)
			}
		})
	}
}

func TestAcademicRequestSupportsDynamicTransport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		serviceURL  string
		endpoint    academicconfig.OperationEndpoint
		wantQuery   string
		wantBody    string
		contentType string
	}{
		{
			name:       "GET query",
			serviceURL: "https://jwgl2024.ouc.edu.cn/",
			endpoint: academicconfig.OperationEndpoint{
				Path: "/api/courses", RequestMethod: "GET",
				RequestEncoding: "query", PeriodParameter: "semester",
			},
			wantQuery: "semester=2026-1",
		},
		{
			name:       "GET query preserves blank all-period parameter",
			serviceURL: "https://jwgl2024.ouc.edu.cn/",
			endpoint: academicconfig.OperationEndpoint{
				Path:          "/jsxsd/kscj/cjcx_list?pageNum=1&pageSize=200&kksj=&kcmc=&xsfs=all&sfxsbcxq=1",
				RequestMethod: "GET", RequestEncoding: "query", PeriodParameter: "kksj",
			},
			wantQuery: "pageNum=1&pageSize=200&kksj=&kcmc=&xsfs=all&sfxsbcxq=1",
		},
		{
			name:       "POST form",
			serviceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			endpoint: academicconfig.OperationEndpoint{
				Path: "/api/courses", RequestMethod: "POST",
				RequestEncoding: "form", PeriodParameter: "xnxq",
			},
			wantBody: "xnxq=2026-1", contentType: "application/x-www-form-urlencoded",
		},
		{
			name:       "POST JSON",
			serviceURL: "https://jwgl2024.ouc.edu.cn/",
			endpoint: academicconfig.OperationEndpoint{
				Path: "/api/courses", RequestMethod: "POST",
				RequestEncoding: "json", PeriodParameter: "periodId",
			},
			wantBody: `{"periodId":"2026-1"}`, contentType: "application/json",
		},
		{
			name:       "GET query with multiple period parameters",
			serviceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			endpoint: academicconfig.OperationEndpoint{
				Path:             "/py/page/student/grkcb.htm?zc=-1",
				RequestMethod:    "GET",
				RequestEncoding:  "query",
				PeriodParameters: []string{"xn", "xj"},
				PeriodSeparator:  ":",
			},
			wantQuery: "xj=11&xn=2017&zc=-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			periodID := "2026-1"
			switch test.name {
			case "GET query preserves blank all-period parameter":
				periodID = ""
			case "GET query with multiple period parameters":
				periodID = "2017:11"
			}
			target, body, contentType, err := academicRequest(
				test.serviceURL,
				test.endpoint,
				periodID,
			)
			if err != nil {
				t.Fatal(err)
			}
			parsed, _ := url.Parse(target)
			if parsed.RawQuery != test.wantQuery {
				t.Fatalf("query=%q", parsed.RawQuery)
			}
			data, _ := io.ReadAll(body)
			if strings.TrimSpace(string(data)) != test.wantBody || contentType != test.contentType {
				t.Fatalf("body=%q contentType=%q", data, contentType)
			}
		})
	}
}

func TestUndergraduateSelectionFailureRequestOverridesConfiguredQuery(t *testing.T) {
	t.Parallel()
	endpoint := academicconfig.OperationEndpoint{
		Path:            "/jsxsd/xkgl/loadXsxkjgList?lx=xkrz&type=list&pageNum=1&pageSize=200",
		RequestMethod:   http.MethodGet,
		RequestEncoding: "query",
		PeriodParameter: "xnxqid",
	}
	target, body, contentType, err := academicRequestWithValues(
		"https://jwgl2024.ouc.edu.cn/",
		endpoint,
		"2026-2027-2",
		undergraduateSelectionFailureRequestValues(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		t.Fatalf("content type=%q want empty", contentType)
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatalf("payload=%q want empty", payload)
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	want := map[string]string{
		"lx":              "tkrz",
		"type":            "list",
		"cxsj":            "tkjg",
		"pageNum":         "1",
		"pageSize":        "20",
		"xnxqid":          "2026-2027-2",
		"sf_request_type": "ajax",
	}
	for key, value := range want {
		if query.Get(key) != value {
			t.Fatalf("query[%q]=%q want %q; full query=%q", key, query.Get(key), value, parsed.RawQuery)
		}
	}
}

func TestMergeCourseSelectionsPreservesPrimaryAndRemovesExactDuplicates(t *testing.T) {
	t.Parallel()
	primary := []domain.CourseSelection{
		{ID: "selected-1", CourseName: "已选课程", Status: domain.CourseSelectionSelected},
		{ID: "shared-1", CourseName: "重新选中的课程", Status: domain.CourseSelectionSelected},
	}
	supplement := []domain.CourseSelection{
		{ID: "shared-1", CourseName: "历史退选记录", Status: domain.CourseSelectionFailed},
		{ID: "failed-1", CourseName: "抽签落选课程", Status: domain.CourseSelectionFailed},
		{ID: "failed-1", CourseName: "抽签落选课程", Status: domain.CourseSelectionFailed},
		{ID: "failed-1", CourseName: "个人退选课程", Status: domain.CourseSelectionFailed},
	}
	merged := mergeCourseSelections(primary, supplement)
	if len(merged) != 5 {
		t.Fatalf("merged=%+v want 5 records", merged)
	}
	if merged[0].ID != "selected-1" || merged[1].ID != "shared-1" ||
		merged[2].ID != "shared-1" || merged[3].ID != "failed-1" || merged[4].ID != "failed-1" {
		t.Fatalf("merged order/ids=%+v", merged)
	}
	if merged[1].Status != domain.CourseSelectionSelected || merged[1].CourseName != "重新选中的课程" {
		t.Fatalf("primary record was replaced=%+v", merged[1])
	}
	if merged[2].Status != domain.CourseSelectionFailed || merged[2].CourseName != "历史退选记录" {
		t.Fatalf("distinct same-ID history was removed=%+v", merged[2])
	}
	if merged[3].CourseName != "抽签落选课程" || merged[4].CourseName != "个人退选课程" {
		t.Fatalf("distinct same-ID supplement records=%+v", merged[2:])
	}
}

func TestFilterPersonalWithdrawals(t *testing.T) {
	t.Parallel()
	drawResult := "抽签落选"
	personalResult := "个人退选"
	adminResult := "管理员退选"
	items := []domain.CourseSelection{
		{ID: "selected-1", Status: domain.CourseSelectionSelected},
		{ID: "draw-1", Status: domain.CourseSelectionFailed, ResultText: &drawResult},
		{ID: "personal-1", Status: domain.CourseSelectionFailed, ResultText: &personalResult},
		{ID: "admin-1", Status: domain.CourseSelectionFailed, ResultText: &adminResult},
		{ID: "pending-1", Status: domain.CourseSelectionPending, ResultText: &personalResult},
	}
	filtered := filterPersonalWithdrawals(items)
	if len(filtered) != 4 {
		t.Fatalf("filtered=%+v want 4 records", filtered)
	}
	for index, wantID := range []string{"selected-1", "draw-1", "admin-1", "pending-1"} {
		if filtered[index].ID != wantID {
			t.Fatalf("filtered[%d]=%+v want id %q", index, filtered[index], wantID)
		}
	}
}

func TestAcademicRequestRejectsInvalidMultiplePeriodID(t *testing.T) {
	t.Parallel()
	_, _, _, err := academicRequest(
		"https://pgs.ouc.edu.cn/allogene/page/home.htm",
		academicconfig.OperationEndpoint{
			Path:             "/py/page/student/grkcb.htm",
			RequestMethod:    "GET",
			RequestEncoding:  "query",
			PeriodParameters: []string{"xn", "xj"},
			PeriodSeparator:  ":",
		},
		"2017",
	)
	if err == nil {
		t.Fatal("invalid multiple-parameter period ID was accepted")
	}
}

func TestAcademicCatalogRequestMatchesCapturedContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		serviceURL string
		endpoint   academicconfig.OperationEndpoint
		periodID   string
		page       int
		wantQuery  string
		wantBody   string
	}{
		{
			name: "undergraduate GET AJAX query", serviceURL: "https://jwgl2024.ouc.edu.cn/",
			endpoint: academicconfig.OperationEndpoint{
				Path: "/jsxsd/xkgl/loadXkkbList", RequestMethod: "GET", RequestEncoding: "query",
				PeriodParameter: "xnxqval", Parameters: map[string]string{"sf_request_type": "ajax"},
				PageParameter: "pageNum", PageSizeParameter: "pageSize", PageSize: 20,
			},
			periodID: "2026-2027-1", page: 3,
			wantQuery: "pageNum=3&pageSize=20&sf_request_type=ajax&xnxqval=2026-2027-1",
		},
		{
			name: "graduate POST form with server page size", serviceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			endpoint: academicconfig.OperationEndpoint{
				Path: "/py/page/student/lnsjCxdc.htm", RequestMethod: "POST", RequestEncoding: "form",
				PeriodParameters: []string{"kkxn", "kckkxj"}, PeriodSeparator: ":",
				Parameters:    map[string]string{"operateType": "search", "key": "", "kkyx": "-1", "kcxz": "-1", "skyy": "-1", "tskc": "", "kcbh": "", "kcmc": "", "jsgh": "", "jsxm": ""},
				PageParameter: "pageId", PageSize: 20,
			},
			periodID: "2026:11", page: 3,
			wantBody: "jsgh=&jsxm=&kcbh=&kckkxj=11&kcmc=&kcxz=-1&key=&kkxn=2026&kkyx=-1&operateType=search&pageId=3&skyy=-1&tskc=",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, body, _, err := academicCatalogRequest(test.serviceURL, test.endpoint, test.periodID, test.page)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.RawQuery != test.wantQuery {
				t.Fatalf("query=%q want=%q", parsed.RawQuery, test.wantQuery)
			}
			data, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.wantBody {
				t.Fatalf("body=%q want=%q", data, test.wantBody)
			}
		})
	}
}

func TestAcademicCatalogRequestDefaultsToContinuousPages(t *testing.T) {
	t.Parallel()
	target, body, _, err := academicCatalogRequest(
		"https://pgs.ouc.edu.cn/",
		academicconfig.OperationEndpoint{
			Path: "/py/page/student/lnsjCxdc.htm", RequestMethod: "POST", RequestEncoding: "form",
			PeriodParameters: []string{"kkxn", "kckkxj"}, PeriodSeparator: ":",
			PageParameter: "pageId", PageSize: 20,
		},
		"2026:11", 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, _ := url.Parse(target); parsed.RawQuery != "" {
		t.Fatalf("query=%q", parsed.RawQuery)
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "kckkxj=11&kkxn=2026&pageId=3" {
		t.Fatalf("body=%q", payload)
	}
}

func TestCatalogRequestOptionsMatchBrowserTransport(t *testing.T) {
	t.Parallel()
	undergraduate := catalogRequestOptions(verificationapp.EducationUndergraduate, "https://jwgl2024.ouc.edu.cn/", "https://jwgl2024.ouc.edu.cn/jsxsd/xkgl/loadXkkbList")
	if undergraduate.requestedWith != "XMLHttpRequest" || undergraduate.referer != "https://jwgl2024.ouc.edu.cn/jsxsd/xkgl/xkkb_find" || !strings.Contains(undergraduate.accept, "application/json") {
		t.Fatalf("undergraduate options=%+v", undergraduate)
	}
	graduate := catalogRequestOptions(verificationapp.EducationGraduate, "https://pgs.ouc.edu.cn/allogene/page/home.htm", "https://pgs.ouc.edu.cn/py/page/student/lnsjCxdc.htm")
	if graduate.origin != "https://pgs.ouc.edu.cn" || graduate.referer != "https://pgs.ouc.edu.cn/py/page/student/lnsjCxdc.htm" {
		t.Fatalf("graduate options=%+v", graduate)
	}
}

func TestAcademicProfileURLStaysOnConfiguredService(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		service string
		path    string
		want    string
		wantErr bool
	}{
		{
			name:    "undergraduate profile",
			service: "https://jwgl2024.ouc.edu.cn/",
			path:    "/jsxsd/framework/xsMainV_new.htmlx",
			want:    "https://jwgl2024.ouc.edu.cn/jsxsd/framework/xsMainV_new.htmlx",
		},
		{
			name:    "graduate profile",
			service: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			path:    "/allogene/page/studentInfo.htm",
			want:    "https://pgs.ouc.edu.cn/allogene/page/studentInfo.htm",
		},
		{
			name:    "absolute URL is rejected",
			service: "https://jwgl2024.ouc.edu.cn/",
			path:    "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			wantErr: true,
		},
		{
			name:    "network path is rejected",
			service: "https://jwgl2024.ouc.edu.cn/",
			path:    "//outside.example.com/profile",
			wantErr: true,
		},
		{
			name:    "query is rejected",
			service: "https://jwgl2024.ouc.edu.cn/",
			path:    "/jsxsd/framework/xsMainV_new.htmlx?ticket=secret",
			wantErr: true,
		},
		{
			name:    "relative path is rejected",
			service: "https://jwgl2024.ouc.edu.cn/",
			path:    "profile",
			wantErr: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := academicProfileURL(test.service, test.path)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("URL=%q want=%q", got, test.want)
			}
		})
	}
}

func TestProviderRejectsUnconfiguredOperationBeforeLogin(t *testing.T) {
	t.Parallel()
	provider := NewProvider(staticConfigResolver{snapshot: academicconfig.Snapshot{
		ActiveProvider: academicconfig.ProviderOUC,
		OUC: academicconfig.OUCConfig{
			Undergraduate: academicconfig.EndpointSet{
				ServiceURL: "https://jwgl2024.ouc.edu.cn/",
			},
		},
	}})
	_, err := provider.ListCourseSelections(
		context.Background(),
		application.StudentReference{
			UserID:         1,
			StudentNo:      "fake-student",
			Provider:       verificationapp.ProviderOUC,
			EducationLevel: verificationapp.EducationUndergraduate,
		},
		application.Credential{
			StudentNo: "fake-student",
			Password:  "fake-password",
		},
		"2025-2026-3",
	)
	if !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("error=%v", err)
	}
}
