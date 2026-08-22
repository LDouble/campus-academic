package ouc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/weouc-plus/campus-academic/internal/modules/academic/application"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/weouc-plus/campus-academic/internal/modules/academic_verification/application"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOUCProcessTraceIsUsefulAndRedacted(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	config := integrationOUCConfig()
	config.TraceEnabled = true
	core, observed := observer.New(zap.InfoLevel)
	provider := NewProvider(
		staticConfigResolver{snapshot: academicconfig.Snapshot{
			ActiveProvider: academicconfig.ProviderOUC,
			OUC:            config,
		}},
		WithLogger(zap.New(core)),
		WithSessionStore(newMemoryOUCSessionStore()),
		withSessionClientFactory(fake.clientFactory(t)),
	)
	if _, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       integrationPassword,
			EducationLevel: verificationapp.EducationUndergraduate,
		},
	); err != nil {
		t.Fatal(err)
	}
	student := application.StudentReference{
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
	fake.failQuery.Store(true)
	if _, err := provider.ListGrades(context.Background(), student, credential, "2026-1"); err == nil {
		t.Fatal("expected the injected upstream failure")
	}
	var output strings.Builder
	for _, entry := range observed.All() {
		_, _ = fmt.Fprintf(&output, "level=%s %s", entry.Level.String(), entry.Message)
		for key, value := range entry.ContextMap() {
			_, _ = fmt.Fprintf(&output, " %s=%v", key, value)
		}
		output.WriteByte('\n')
	}
	logged := output.String()
	foundBoundedUpstreamFailure := false
	for _, entry := range observed.All() {
		contextMap := entry.ContextMap()
		if entry.Level == zap.WarnLevel &&
			contextMap["stage"] == "http.response.error" &&
			contextMap["outcome"] == "http_status" &&
			contextMap["error_kind"] == "upstream_5xx" &&
			contextMap["student_masked"] == "in***************nt" {
			foundBoundedUpstreamFailure = true
			break
		}
	}
	if !foundBoundedUpstreamFailure {
		t.Fatalf("trace did not contain one correlated, bounded upstream failure:\n%s", logged)
	}
	for _, required := range []string{
		"trace_id=ouc-",
		"stage=http.response.finish",
		"request_host=id.ouc.edu.cn",
		"response_host=my.ouc.edu.cn",
		"stage=sso.challenge.decision",
		"rule=structured_response",
		"stage=verify.finish",
		"outcome=success",
		"student_masked=in***************nt",
		"education_level=undergraduate",
		"stage=target_session_load",
		"stage=identity_session_load",
		"stage=sso_handoff",
		"stage=business_query",
		"stage=response_classification",
		"stage=response_parse",
		"stage=session_save",
		"stage=http.response.error",
		"error_kind=upstream_5xx",
		"outcome=business_query_failure",
		"level=warn",
	} {
		if !strings.Contains(logged, required) {
			t.Fatalf("trace is missing %q:\n%s", required, logged)
		}
	}
	for _, forbidden := range []string{
		integrationStudentNo,
		integrationPassword,
		"ST-integration",
		"ticket=",
		"service=https",
		"redirect=",
		"集成测试同学",
		"OUC_SSO",
		"PORTAL_SESSION",
	} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("trace leaked forbidden value %q:\n%s", forbidden, logged)
		}
	}
}

func TestOUCProcessTraceRecordsUpstreamLoginCodeAndMessage(t *testing.T) {
	fake := newFakeOUCServer(t, false, map[string]bool{
		verificationapp.EducationUndergraduate: true,
	})
	fake.loginResponseError = &loginResponseError{
		Code: 40400,
		Msg:  "账号或密码错误",
	}
	config := integrationOUCConfig()
	config.TraceEnabled = false
	core, observed := observer.New(zap.InfoLevel)
	provider := NewProvider(
		staticConfigResolver{snapshot: academicconfig.Snapshot{
			ActiveProvider: academicconfig.ProviderOUC,
			OUC:            config,
		}},
		WithLogger(zap.New(core)),
		WithSessionStore(newMemoryOUCSessionStore()),
		withSessionClientFactory(fake.clientFactory(t)),
	)
	_, err := provider.Verify(
		context.Background(),
		verificationapp.VerificationCommand{
			StudentNo:      integrationStudentNo,
			Password:       "wrong-integration-password",
			EducationLevel: verificationapp.EducationUndergraduate,
		},
	)
	if !errors.Is(err, verificationapp.ErrInvalidCredentials) {
		t.Fatalf("verification error=%v want=%v", err, verificationapp.ErrInvalidCredentials)
	}
	for _, entry := range observed.All() {
		fields := entry.ContextMap()
		if fields["stage"] == "sso.login_response.parsed" &&
			fields["upstream_code"] == int64(40400) &&
			fields["upstream_message"] == "账号或密码错误" {
			return
		}
	}
	t.Fatalf("trace did not contain the upstream login code and message: %+v", observed.All())
}

func TestMaskStudentNo(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "20260001", want: "20****01"},
		{value: "12345", want: "*****"},
		{value: "", want: ""},
		{value: " 20260001 ", want: "20****01"},
		{value: "20" + strings.Repeat("1", 200) + "01", want: "20" + strings.Repeat("*", 28) + "01"},
	} {
		if got := maskStudentNo(test.value); got != test.want {
			t.Fatalf("maskStudentNo(%q)=%q want=%q", test.value, got, test.want)
		}
	}
}

func TestSafeLogPathRedactsOpaqueAndSensitiveSegments(t *testing.T) {
	t.Parallel()
	got := safeLogPath("/sso/ticket/ST-secret-value/student/20260001/callback/0123456789abcdef0123456789abcdef")
	if got != "/sso/ticket/:redacted/student/:redacted/callback/:redacted" {
		t.Fatalf("safeLogPath()=%q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "20260001") || strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("safeLogPath leaked an opaque value: %q", got)
	}
}

func TestSafeURLFieldsExposeOnlySortedQueryKeys(t *testing.T) {
	fields := safeURLFields(
		"request",
		"https://pgs.ouc.edu.cn/py/page/student/grkcb.htm?xn=2019&zc=-1&xj=11",
	)
	core, observed := observer.New(zap.InfoLevel)
	zap.New(core).Info("request", fields...)
	contextMap := observed.All()[0].ContextMap()

	keys, ok := contextMap["request_query_keys"].([]interface{})
	if !ok {
		t.Fatalf("request_query_keys=%T", contextMap["request_query_keys"])
	}
	got := make([]string, 0, len(keys))
	for _, key := range keys {
		got = append(got, fmt.Sprint(key))
	}
	if strings.Join(got, ",") != "xj,xn,zc" {
		t.Fatalf("request_query_keys=%v", got)
	}
	for _, forbidden := range []string{"2019", "-1", "11"} {
		if strings.Contains(fmt.Sprint(contextMap), forbidden) {
			t.Fatalf("query value leaked: %s", forbidden)
		}
	}
}
