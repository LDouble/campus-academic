package ouc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type deadlineBody struct{}

func (deadlineBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (deadlineBody) Close() error             { return nil }

type recordingHTTPObserver struct {
	phases []string
}

func (*recordingHTTPObserver) ObserveAcademicUpstreamAttempt(string, string, string) {}
func (*recordingHTTPObserver) ObserveAcademicSessionCache(string, string)            {}
func (*recordingHTTPObserver) ObserveAcademicSessionRecovery(string, string, string) {}

func (o *recordingHTTPObserver) ObserveAcademicOUCHTTP(_, _, phase, _ string, _ time.Duration) {
	o.phases = append(o.phases, phase)
}

func TestRequestPageDoesNotMapRedirectPolicyErrorToRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://attacker.example/", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	transport, err := newOUCTransport("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := newSessionClientWithTransport(testOUCConfig(), transport)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = requestPage(
		context.Background(), client, http.MethodGet, server.URL, nil, "", testOUCConfig(), nil,
	)
	if !errors.Is(err, errOUCHTRedirectPolicy) {
		t.Fatalf("err=%v, want redirect policy error", err)
	}
	if errors.Is(err, verificationapp.ErrProviderRetryable) {
		t.Fatal("redirect policy error must not be retryable")
	}
}

func TestRequestPageMapsResponseBodyDeadlineToRetryable(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       deadlineBody{},
			Request:    request,
		}, nil
	})}
	_, _, err := requestPage(
		context.Background(), client, http.MethodGet,
		"https://id.ouc.edu.cn/sso/login", nil, "", testOUCConfig(), nil,
	)
	if !errors.Is(err, verificationapp.ErrProviderRetryable) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want retryable response-body deadline", err)
	}
}

func TestRequestPageStopsPasswordPostBeforeFollowingRedirect(t *testing.T) {
	var postRequests, getRequests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			postRequests++
			http.Redirect(writer, request, "/handoff", http.StatusFound)
			return
		}
		getRequests++
		_, _ = writer.Write([]byte("handoff"))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: http.DefaultTransport}
	redirected := false
	resultURL, _, err := requestPageWithOptions(
		context.Background(), client, http.MethodPost, server.URL,
		strings.NewReader("password=secret"), "application/x-www-form-urlencoded",
		testOUCConfig(), nil, requestPageOptions{
			stopPostRedirect: true,
			redirected:       &redirected,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !redirected || resultURL == nil || resultURL.Path != "/handoff" {
		t.Fatalf("redirected=%v resultURL=%v", redirected, resultURL)
	}
	if postRequests != 1 || getRequests != 0 {
		t.Fatalf("postRequests=%d getRequests=%d, want one POST and no replay", postRequests, getRequests)
	}
}

func TestAuthenticateMovesExistingSessionHandoffOutsideLoginPageBudget(t *testing.T) {
	config := testOUCConfig()
	config.MaxResponseBytes = 1024
	var loginPageBudget, targetHandoffBudget time.Duration
	observer := &recordingHTTPObserver{}
	clientFactory := func(academicconfig.OUCConfig) (*http.Client, error) {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			deadline, ok := request.Context().Deadline()
			if !ok {
				t.Fatalf("request to %s has no deadline", request.URL.Hostname())
			}
			switch request.URL.Hostname() {
			case "id.ouc.edu.cn":
				loginPageBudget = time.Until(deadline)
				return &http.Response{
					StatusCode: http.StatusFound,
					Status:     "302 Found",
					Header: http.Header{
						"Location": []string{"https://my.ouc.edu.cn/frontend/user/info"},
					},
					Body:    http.NoBody,
					Request: request,
				}, nil
			case "my.ouc.edu.cn":
				targetHandoffBudget = time.Until(deadline)
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("portal identity")),
					Request:    request,
				}, nil
			default:
				t.Fatalf("unexpected request host %q", request.URL.Hostname())
				return nil, nil
			}
		})}, nil
	}
	trace := newProcessTrace(context.Background(), config, nil, "verify").withObserver(observer)
	current, err := authenticate(context.Background(), config, "student", "password", clientFactory, trace)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		t.Fatal("authenticate returned a nil session")
	}
	if string(current.page) != "portal identity" {
		t.Fatalf("page=%q", current.page)
	}
	if loginPageBudget <= 0 || loginPageBudget > oucSSOStageTimeout {
		t.Fatalf("login page budget=%s, want at most %s", loginPageBudget, oucSSOStageTimeout)
	}
	if targetHandoffBudget <= oucSSOStageTimeout {
		t.Fatalf("target handoff budget=%s, want more than login-page budget %s", targetHandoffBudget, oucSSOStageTimeout)
	}
	if got := strings.Join(observer.phases, ","); got != "sso_login_page,target_handoff" {
		t.Fatalf("observed phases=%q", got)
	}
}
