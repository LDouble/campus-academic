package querycoord

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/core/ratelimitconfig"
	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type recordingProvider struct {
	calls            atomic.Int32
	active           atomic.Int32
	maxActive        atomic.Int32
	delay            time.Duration
	release          <-chan struct{}
	acceptedPassword string
	fail             atomic.Bool
	deadline         atomic.Bool
	onContextDone    func()
}

type blockingRedisHook struct {
	enabled atomic.Bool
	release chan struct{}
}

func (*blockingRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *blockingRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if h.enabled.Load() {
			select {
			case <-h.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, command)
	}
}

func (h *blockingRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if h.enabled.Load() {
			select {
			case <-h.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, commands)
	}
}

type mutableRateLimits struct {
	policy ratelimitconfig.Policy
}

type recordingCoordinatorObserver struct {
	mu       sync.Mutex
	cache    map[string]int
	bypasses map[string]int
	queries  map[string]int
	circuits map[string]int
	stale    map[string]int
}

func newRecordingCoordinatorObserver() *recordingCoordinatorObserver {
	return &recordingCoordinatorObserver{
		cache:    make(map[string]int),
		bypasses: make(map[string]int),
		queries:  make(map[string]int),
		circuits: make(map[string]int),
		stale:    make(map[string]int),
	}
}

func (o *recordingCoordinatorObserver) ObserveAcademicCache(operation, state, educationLevel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cache[strings.Join([]string{operation, state, educationLevel}, "/")]++
}

func (o *recordingCoordinatorObserver) ObserveAcademicCacheBypass(operation, state, educationLevel, mode string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bypasses[strings.Join([]string{operation, state, educationLevel, mode}, "/")]++
}

func (o *recordingCoordinatorObserver) ObserveAcademicQuery(operation, outcome, educationLevel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queries[strings.Join([]string{operation, outcome, educationLevel}, "/")]++
}

func (o *recordingCoordinatorObserver) ObserveAcademicCircuit(operation, event, educationLevel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.circuits[strings.Join([]string{operation, event, educationLevel}, "/")]++
}

func (o *recordingCoordinatorObserver) ObserveAcademicStaleFallback(operation, reason, educationLevel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stale[strings.Join([]string{operation, reason, educationLevel}, "/")]++
}

func (p *mutableRateLimits) Resolve() ratelimitconfig.Policy { return p.policy }

func (p *recordingProvider) ListCourses(
	ctx context.Context,
	_ application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	if err := p.begin(ctx); err != nil {
		return domain.CourseSchedule{}, err
	}
	defer p.end()
	if p.acceptedPassword != "" && credential.Password != p.acceptedPassword {
		return domain.CourseSchedule{}, application.ErrInvalidCredentials
	}
	if p.deadline.Load() {
		return domain.CourseSchedule{}, context.DeadlineExceeded
	}
	if p.fail.Load() {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	return domain.CourseSchedule{Courses: []domain.Course{{
		ID: "course-1", PeriodID: periodID, Name: "敏感课程名称",
	}}}, nil
}

func (p *recordingProvider) ListGrades(
	ctx context.Context,
	_ application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Grade, error) {
	if err := p.begin(ctx); err != nil {
		return nil, err
	}
	defer p.end()
	if p.acceptedPassword != "" && credential.Password != p.acceptedPassword {
		return nil, application.ErrInvalidCredentials
	}
	if p.deadline.Load() {
		return nil, context.DeadlineExceeded
	}
	if p.fail.Load() {
		return nil, application.ErrProviderUnavailable
	}
	score := 96.0
	return []domain.Grade{{ID: "grade-1", PeriodID: periodID, Score: &score}}, nil
}

func (p *recordingProvider) ListExams(
	ctx context.Context,
	_ application.StudentReference,
	_ application.Credential,
	periodID string,
) ([]domain.Exam, error) {
	if err := p.begin(ctx); err != nil {
		return nil, err
	}
	defer p.end()
	return []domain.Exam{{ID: "exam-1", PeriodID: periodID}}, nil
}

func (p *recordingProvider) ListCourseSelections(
	ctx context.Context,
	_ application.StudentReference,
	_ application.Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	if err := p.begin(ctx); err != nil {
		return nil, err
	}
	defer p.end()
	return []domain.CourseSelection{{ID: "selection-1", PeriodID: periodID}}, nil
}

func (p *recordingProvider) begin(ctx context.Context) error {
	p.calls.Add(1)
	active := p.active.Add(1)
	for {
		maximum := p.maxActive.Load()
		if active <= maximum || p.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	if p.release != nil {
		select {
		case <-ctx.Done():
			if p.onContextDone != nil {
				p.onContextDone()
			}
			p.active.Add(-1)
			return ctx.Err()
		case <-p.release:
			return nil
		}
	}
	if p.delay <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		if p.onContextDone != nil {
			p.onContextDone()
		}
		p.active.Add(-1)
		return ctx.Err()
	case <-time.After(p.delay):
		return nil
	}
}

func (p *recordingProvider) end() { p.active.Add(-1) }

func testConfig() Config {
	return Config{
		CacheMode:           cacheModeNormal,
		CoursesTimeout:      2 * time.Second,
		GradesTimeout:       2 * time.Second,
		ExamsTimeout:        2 * time.Second,
		SelectionsTimeout:   2 * time.Second,
		StaleRefreshTimeout: time.Second,
		LeaseTTL:            3 * time.Second,
		PollInterval:        10 * time.Millisecond,
		MaxConcurrent:       1000,
		GlobalRate:          1000,
		GlobalBurst:         1000,
		RetryAfter:          time.Second,
		CoursesFreshTTL:     time.Second,
		CoursesStaleTTL:     time.Minute,
		GradesFreshTTL:      time.Second,
		GradesStaleTTL:      time.Minute,
		ExamsFreshTTL:       time.Second,
		ExamsStaleTTL:       time.Minute,
		SelectionsFreshTTL:  time.Second,
		SelectionsStaleTTL:  time.Minute,
		CircuitThreshold:    3,
		CircuitWindow:       30 * time.Second,
		CircuitOpenDuration: 15 * time.Second,
	}
}

func newTestCoordinator(
	t *testing.T,
	server *miniredis.Miniredis,
	provider application.Provider,
	config Config,
) *Coordinator {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	coordinator, err := New(
		provider,
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func testIdentity() (application.StudentReference, application.Credential) {
	return application.StudentReference{
		UserID: 7, StudentNo: "20260001", Provider: "ouc", EducationLevel: "undergraduate",
	}, application.Credential{StudentNo: "20260001", Password: "private-password"}
}

func TestCoordinatorObservesCacheAndQueryOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		providerFail bool
		calls        int
		wantErr      error
		wantCache    map[string]int
		wantQueries  map[string]int
	}{
		{
			name:  "miss then fresh cache",
			calls: 2,
			wantCache: map[string]int{
				"courses/miss/undergraduate":  1,
				"courses/fresh/undergraduate": 1,
			},
			wantQueries: map[string]int{"courses/success/undergraduate": 2},
		},
		{
			name:         "provider unavailable",
			providerFail: true,
			calls:        1,
			wantErr:      application.ErrProviderUnavailable,
			wantCache:    map[string]int{"courses/miss/undergraduate": 1},
			wantQueries:  map[string]int{"courses/unavailable/undergraduate": 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			provider := &recordingProvider{}
			provider.fail.Store(test.providerFail)
			observer := newRecordingCoordinatorObserver()
			coordinator, err := New(
				provider,
				client,
				[]byte("0123456789abcdef0123456789abcdef"),
				testConfig(),
				zap.NewNop(),
				WithObserver(observer),
			)
			if err != nil {
				t.Fatal(err)
			}
			student, credential := testIdentity()
			for range test.calls {
				_, callErr := coordinator.ListCourses(
					context.Background(),
					student,
					credential,
					"2026-spring",
				)
				if !errors.Is(callErr, test.wantErr) {
					t.Fatalf("error=%v, want %v", callErr, test.wantErr)
				}
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if !mapsEqual(observer.cache, test.wantCache) {
				t.Fatalf("cache observations=%v, want %v", observer.cache, test.wantCache)
			}
			if !mapsEqual(observer.queries, test.wantQueries) {
				t.Fatalf("query observations=%v, want %v", observer.queries, test.wantQueries)
			}
		})
	}
}

func TestCircuitUsesDeadlineRatioAndMinimumSamples(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	config := coordinator.config
	config.CircuitMinimumSamples = 10
	config.CircuitDeadlineThreshold = 3
	config.CircuitDeadlineRatio = 0.2
	config.CircuitHardProtectionCount = 20
	config.CircuitHardProtectionWindow = time.Minute
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	hardWindow := config.CircuitHardProtectionWindow
	base := time.Now()
	for index := 0; index < 8; index++ {
		if _, err := coordinator.store.resetCircuitWithPolicy(
			context.Background(), key, base.Add(time.Duration(index)*time.Second),
			config.CircuitWindow, true, hardWindow,
		); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 2; index++ {
		_, _, opened, err := coordinator.store.recordDeadlineWithPolicy(
			context.Background(), key, 10, 3, 0.2, 20, hardWindow,
			config.CircuitWindow, config.CircuitOpenDuration,
			base.Add(time.Duration(index+8)*time.Second), "",
		)
		if err != nil || opened {
			t.Fatalf("deadline %d opened=%v err=%v", index, opened, err)
		}
	}
	_, _, opened, err := coordinator.store.recordDeadlineWithPolicy(
		context.Background(), key, 10, 3, 0.2, 20, hardWindow,
		config.CircuitWindow, config.CircuitOpenDuration, base.Add(10*time.Second), "",
	)
	if err != nil || !opened {
		t.Fatalf("ratio policy opened=%v err=%v", opened, err)
	}
}

func TestCircuitUsesRollingSampleWindow(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	window := 2 * time.Second
	hardWindow := 10 * time.Second
	base := time.Now()
	if _, err := coordinator.store.resetCircuitWithPolicy(
		context.Background(), key, base, window, true, hardWindow,
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond)
	for index := 0; index < 2; index++ {
		if _, err := coordinator.store.resetCircuitWithPolicy(
			context.Background(), key, base.Add(time.Duration(index+1)*time.Second), window, true, hardWindow,
		); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1300 * time.Millisecond)
	for index := 0; index < 3; index++ {
		_, _, opened, err := coordinator.store.recordDeadlineWithPolicy(
			context.Background(), key, 5, 3, 0.5, 100, hardWindow,
			window, time.Second, base.Add(time.Duration(index+3)*time.Second), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 && opened {
			t.Fatalf("deadline %d opened circuit before rolling minimum sample count", index+1)
		}
		if index == 2 && !opened {
			t.Fatal("rolling window did not open after three deadlines among five recent samples")
		}
	}
}

func TestInFlightSuccessDoesNotCloseOpenCircuit(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	config := coordinator.config
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	openedAt := time.Now()
	if _, _, opened, err := coordinator.store.recordDeadline(
		context.Background(), key, 1, config.CircuitWindow, config.CircuitOpenDuration, openedAt, "",
	); err != nil || !opened {
		t.Fatalf("open circuit=%v err=%v", opened, err)
	}
	if reset, err := coordinator.store.resetCircuitWithPolicy(
		context.Background(), key, openedAt.Add(time.Second), config.CircuitWindow, false, config.CircuitWindow,
	); err != nil || reset {
		t.Fatalf("in-flight success reset=%v err=%v, want preserved open circuit", reset, err)
	}
	open, _, _, err := coordinator.store.circuitOpen(context.Background(), key, "", 0)
	if err != nil || !open {
		t.Fatalf("circuit open=%v err=%v, want open protection to remain active", open, err)
	}
}

func TestInFlightSuccessPreservesLongerCircuitTTL(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	completedAt := time.Now()
	server.SetTime(completedAt)
	window := time.Second
	openDuration := 5 * time.Second
	if _, _, opened, err := coordinator.store.recordDeadlineWithPolicy(
		context.Background(), key, 1, 1, 1, 1, window, window, openDuration, completedAt, "",
	); err != nil || !opened {
		t.Fatalf("open circuit=%v err=%v", opened, err)
	}
	if _, err := coordinator.store.resetCircuitWithPolicy(
		context.Background(), key, completedAt, window, true, window,
	); err != nil {
		t.Fatal(err)
	}
	ttl, err := coordinator.store.client.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < openDuration {
		t.Fatalf("circuit TTL=%s, want at least open duration %s", ttl, openDuration)
	}
}

func mapsEqual(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func TestConcurrentQueriesAreCoalescedAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{delay: 100 * time.Millisecond}
	first := newTestCoordinator(t, server, provider, testConfig())
	second := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	start := make(chan struct{})
	errorsChannel := make(chan error, 20)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		coordinator := first
		if index%2 == 1 {
			coordinator = second
		}
		go func() {
			defer wait.Done()
			<-start
			rows, err := coordinator.ListCourses(
				context.Background(), student, credential, "2026-spring",
			)
			if err == nil && (len(rows.Courses) != 1 || rows.Courses[0].ID != "course-1") {
				err = errors.New("unexpected coalesced result")
			}
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want 1", calls)
	}
}

func TestDifferentOperationsForOneStudentAreSerialized(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{delay: 80 * time.Millisecond}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	start := make(chan struct{})
	errorsChannel := make(chan error, 2)
	go func() {
		<-start
		_, err := coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
		errorsChannel <- err
	}()
	go func() {
		<-start
		_, err := coordinator.ListGrades(context.Background(), student, credential, "2026-spring")
		errorsChannel <- err
	}()
	close(start)
	for range 2 {
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
	}
	if maximum := provider.maxActive.Load(); maximum != 1 {
		t.Fatalf("maximum per-student concurrency=%d, want 1", maximum)
	}
}

func TestCanceledCallKeepsDetachedLeaderInsideConcurrencyBound(t *testing.T) {
	server := miniredis.RunT(t)
	releaseProvider := make(chan struct{})
	provider := &recordingProvider{release: releaseProvider}
	config := testConfig()
	config.MaxConcurrent = 1
	coordinator := newTestCoordinator(t, server, provider, config)
	firstStudent, firstCredential := testIdentity()
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := coordinator.ListCourses(
			firstContext, firstStudent, firstCredential, "2026-spring",
		)
		firstResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for provider.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if provider.calls.Load() == 0 {
		t.Fatal("detached leader did not start")
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller error=%v, want context canceled", err)
	}
	secondStudent := firstStudent
	secondStudent.UserID = 8
	secondStudent.StudentNo = "20260002"
	secondCredential := firstCredential
	secondCredential.StudentNo = secondStudent.StudentNo
	secondResult := make(chan error, 1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := coordinator.ListCourses(
			context.Background(), secondStudent, secondCredential, "2026-spring",
		)
		secondResult <- err
	}()
	<-secondStarted
	time.Sleep(30 * time.Millisecond)
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls while detached leader active=%d, want 1", calls)
	}
	close(releaseProvider)
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	if maximum := provider.maxActive.Load(); maximum != 1 {
		t.Fatalf("maximum downstream concurrency=%d, want 1", maximum)
	}
}

func TestDetachedLeaderDoesNotExtendCallerDeadline(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{release: make(chan struct{})}
	config := testConfig()
	config.CoursesTimeout = time.Second
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := coordinator.ListCourses(ctx, student, credential, "2026-spring")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query error=%v, want deadline exceeded", err)
	}
	deadline := time.Now().Add(time.Second)
	for provider.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if provider.active.Load() != 0 {
		t.Fatal("detached leader outlived the caller's end-to-end deadline")
	}
	if elapsed := time.Since(started); elapsed >= config.CoursesTimeout {
		t.Fatalf("detached leader elapsed=%s, configured timeout=%s", elapsed, config.CoursesTimeout)
	}
}

func TestCachedAcademicDataIsEncryptedAndKeysHideIdentity(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	if _, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	foundResult := false
	for _, key := range server.Keys() {
		if strings.Contains(key, student.StudentNo) || strings.Contains(key, credential.Password) {
			t.Fatalf("Redis key exposes academic identity: %q", key)
		}
		if !strings.Contains(key, ":result:") {
			continue
		}
		foundResult = true
		encoded, err := server.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(encoded, student.StudentNo) ||
			strings.Contains(encoded, credential.Password) ||
			strings.Contains(encoded, "敏感课程名称") {
			t.Fatal("Redis cache contains plaintext academic data")
		}
	}
	if !foundResult {
		t.Fatal("encrypted result cache was not written")
	}
}

func TestCachedResultRequiresSameCredential(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{acceptedPassword: "private-password"}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	rows, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(rows.Courses) != 1 {
		t.Fatalf("warm cache rows=%+v err=%v", rows, err)
	}
	wrongCredential := credential
	wrongCredential.Password = "wrong-password"
	rows, err = coordinator.ListCourses(
		context.Background(), student, wrongCredential, "2026-spring",
	)
	if !errors.Is(err, application.ErrInvalidCredentials) {
		t.Fatalf("wrong credential rows=%+v err=%v, want invalid credentials", rows, err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls=%d, want cache warm and credential validation", calls)
	}
}

func TestMismatchedStudentCredentialIsRejected(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	credential.StudentNo = "20260002"
	rows, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if !errors.Is(err, application.ErrInvalidCredentials) {
		t.Fatalf("mismatched identity rows=%+v err=%v, want invalid credentials", rows, err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("provider calls=%d, want mismatch rejected before downstream", calls)
	}
}

func TestStaleResultIsSharedDuringProviderFailure(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	if _, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
	provider.fail.Store(true)
	first, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(first.Courses) != 1 {
		t.Fatalf("first stale result=%+v err=%v", first, err)
	}
	second, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(second.Courses) != 1 {
		t.Fatalf("shared stale result=%+v err=%v", second, err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls=%d, want initial and one failed refresh", calls)
	}
}

func TestCacheAwareQueryReportsFreshAndStaleProvenance(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()

	direct, err := coordinator.ListCoursesWithCache(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(direct.Records.Courses) != 1 || direct.Cache != nil {
		t.Fatalf("direct result=%+v err=%v; direct responses must not expose cache metadata", direct, err)
	}
	fresh, err := coordinator.ListCoursesWithCache(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || fresh.Cache == nil || fresh.Cache.State != application.CacheStateFresh ||
		fresh.Cache.CachedAt.IsZero() || fresh.Cache.FreshUntil.Before(fresh.Cache.CachedAt) {
		t.Fatalf("fresh result=%+v err=%v", fresh, err)
	}

	time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
	provider.fail.Store(true)
	stale, err := coordinator.ListCoursesWithCache(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || stale.Cache == nil || stale.Cache.State != application.CacheStateStale ||
		!stale.Cache.CachedAt.Equal(fresh.Cache.CachedAt) ||
		!stale.Cache.FreshUntil.Equal(fresh.Cache.FreshUntil) {
		t.Fatalf("stale result=%+v err=%v", stale, err)
	}
}

func TestCacheModesControlResultCacheReturns(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		expireFresh     bool
		providerFailure bool
		wantCalls       int32
		wantErr         error
		wantStale       int
		wantBypasses    int
	}{
		{name: "normal returns fresh", mode: cacheModeNormal, wantCalls: 1},
		{name: "no stale returns fresh", mode: cacheModeNoStale, wantCalls: 1},
		{
			name: "force upstream bypasses fresh", mode: cacheModeForceUpstream,
			wantCalls: 2, wantBypasses: 1,
		},
		{
			name: "normal returns stale on provider failure", mode: cacheModeNormal,
			expireFresh: true, providerFailure: true, wantCalls: 2, wantStale: 1,
		},
		{
			name: "no stale rejects stale on provider failure", mode: cacheModeNoStale,
			expireFresh: true, providerFailure: true, wantCalls: 2,
			wantErr: application.ErrProviderUnavailable, wantBypasses: 1,
		},
		{
			name: "force upstream rejects stale on provider failure", mode: cacheModeForceUpstream,
			expireFresh: true, providerFailure: true, wantCalls: 2,
			wantErr: application.ErrProviderUnavailable, wantBypasses: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			provider := &recordingProvider{}
			observer := newRecordingCoordinatorObserver()
			config := testConfig()
			config.CacheMode = test.mode
			coordinator, err := New(
				provider,
				client,
				[]byte("0123456789abcdef0123456789abcdef"),
				config,
				zap.NewNop(),
				WithObserver(observer),
			)
			if err != nil {
				t.Fatal(err)
			}
			student, credential := testIdentity()
			if _, err = coordinator.ListCourses(context.Background(), student, credential, "2026-spring"); err != nil {
				t.Fatalf("warm cache: %v", err)
			}
			if test.expireFresh {
				time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
			}
			provider.fail.Store(test.providerFailure)
			_, err = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v, want %v", err, test.wantErr)
			}
			if calls := provider.calls.Load(); calls != test.wantCalls {
				t.Fatalf("provider calls=%d, want %d", calls, test.wantCalls)
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if got := observer.stale["courses/unavailable/undergraduate"]; got != test.wantStale {
				t.Fatalf("stale fallbacks=%d, want %d", got, test.wantStale)
			}
			bypassKey := strings.Join([]string{"courses", "fresh", "undergraduate", test.mode}, "/")
			if test.expireFresh {
				bypassKey = strings.Join([]string{"courses", "stale", "undergraduate", test.mode}, "/")
			}
			if got := observer.bypasses[bypassKey]; got != test.wantBypasses {
				t.Fatalf("cache bypasses=%d, want %d", got, test.wantBypasses)
			}
		})
	}
}

func TestForceUpstreamSkipsFreshCacheAfterStudentLease(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	observer := newRecordingCoordinatorObserver()
	config := testConfig()
	config.CacheMode = cacheModeForceUpstream
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	coordinator, err := New(
		provider,
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}
	student, credential := testIdentity()
	studentLeaseKey := coordinator.store.studentLeaseKey(coordinator.store.digest("student", student.StudentNo))
	token, acquired, err := coordinator.store.acquireLease(context.Background(), studentLeaseKey, config.LeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("acquire student lease: acquired=%v err=%v", acquired, err)
	}
	result := make(chan error, 1)
	go func() {
		_, callErr := coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
		result <- callErr
	}()
	select {
	case err = <-result:
		t.Fatalf("force-upstream request bypassed student lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	credentialDigest := coordinator.store.digest("credential-v1", credential.StudentNo, credential.Password)
	resultDigest := coordinator.store.digest(
		resultCacheDiscriminator(operationCourses),
		student.Provider,
		student.EducationLevel,
		student.StudentNo,
		operationCourses,
		"2026-spring",
		credentialDigest,
	)
	if err = coordinator.store.save(
		context.Background(),
		coordinator.store.resultKey(resultDigest),
		coordinator.store.studentIndexKey(student.StudentNo),
		domain.CourseSchedule{Courses: []domain.Course{{ID: "cached-course", PeriodID: "2026-spring"}}},
		config.CoursesFreshTTL,
		config.CoursesStaleTTL,
	); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.store.releaseLease(context.Background(), studentLeaseKey, token); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want 1", calls)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if bypasses := observer.bypasses["courses/fresh/undergraduate/force_upstream"]; bypasses != 1 {
		t.Fatalf("cache bypasses=%d, want 1", bypasses)
	}
}

func TestCoursesIgnoreLegacySliceCacheKey(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	credentialDigest := coordinator.store.digest(
		"credential-v1", credential.StudentNo, credential.Password,
	)
	legacyDigest := coordinator.store.digest(
		"result-v1",
		student.Provider,
		student.EducationLevel,
		student.StudentNo,
		operationCourses,
		"2026-spring",
		credentialDigest,
	)
	if err := coordinator.store.save(
		context.Background(),
		coordinator.store.resultKey(legacyDigest),
		coordinator.store.studentIndexKey(student.StudentNo),
		[]domain.Course{{ID: "legacy-course", PeriodID: "2026-spring"}},
		config.CoursesFreshTTL,
		config.CoursesStaleTTL,
	); err != nil {
		t.Fatal(err)
	}

	schedule, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls.Load())
	}
	if len(schedule.Courses) != 1 || schedule.Courses[0].ID != "course-1" {
		t.Fatalf("schedule=%+v", schedule)
	}
}

func TestCoursesIgnoreV2SnapshotWithoutCourseNotes(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	credentialDigest := coordinator.store.digest(
		"credential-v1", credential.StudentNo, credential.Password,
	)
	v2Digest := coordinator.store.digest(
		"result-courses-v2",
		student.Provider,
		student.EducationLevel,
		student.StudentNo,
		operationCourses,
		"2026-spring",
		credentialDigest,
	)
	if err := coordinator.store.save(
		context.Background(),
		coordinator.store.resultKey(v2Digest),
		coordinator.store.studentIndexKey(student.StudentNo),
		domain.CourseSchedule{Courses: []domain.Course{{ID: "v2-course"}}},
		config.CoursesFreshTTL,
		config.CoursesStaleTTL,
	); err != nil {
		t.Fatal(err)
	}

	schedule, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || len(schedule.Courses) != 1 || schedule.Courses[0].ID != "course-1" {
		t.Fatalf("provider calls=%d schedule=%+v", provider.calls.Load(), schedule)
	}
}

func TestStaleFallsBackWhenRefreshBudgetExpiresBeforeProviderCall(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	config.MaxConcurrent = 1
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	if _, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
	coordinator.permits <- struct{}{}
	started := time.Now()
	rows, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	<-coordinator.permits
	if err != nil || len(rows.Courses) != 1 {
		t.Fatalf("stale rows=%+v err=%v", rows, err)
	}
	if elapsed := time.Since(started); elapsed < config.StaleRefreshTimeout ||
		elapsed > config.StaleRefreshTimeout+500*time.Millisecond {
		t.Fatalf("stale refresh elapsed=%s, budget=%s", elapsed, config.StaleRefreshTimeout)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want only the cache-warming call", calls)
	}
}

func TestTimedOutStaleRefreshDoesNotWaitForDetachedBookkeeping(t *testing.T) {
	server := miniredis.RunT(t)
	hook := &blockingRedisHook{release: make(chan struct{})}
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	provider := &recordingProvider{}
	config := testConfig()
	coordinator, err := New(
		provider,
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	student, credential := testIdentity()
	if _, err = coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
	provider.delay = 2 * config.StaleRefreshTimeout
	provider.onContextDone = func() { hook.enabled.Store(true) }
	started := time.Now()
	rows, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	elapsed := time.Since(started)
	close(hook.release)
	if err != nil || len(rows.Courses) != 1 {
		t.Fatalf("stale rows=%+v err=%v", rows, err)
	}
	if elapsed >= config.StaleRefreshTimeout+500*time.Millisecond {
		t.Fatalf(
			"stale refresh elapsed=%s, budget=%s; detached bookkeeping blocked the response",
			elapsed,
			config.StaleRefreshTimeout,
		)
	}
}

func TestTimeoutFailureMarkerSuppressesImmediateRetry(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{delay: 2 * time.Second}
	config := testConfig()
	config.CoursesTimeout = time.Second
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	_, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out query error=%v, want deadline exceeded", err)
	}
	credentialDigest := coordinator.store.digest(
		"credential-v1", credential.StudentNo, credential.Password,
	)
	resultDigest := coordinator.store.digest(
		resultCacheDiscriminator(operationCourses),
		student.Provider,
		student.EducationLevel,
		student.StudentNo,
		operationCourses,
		"2026-spring",
		credentialDigest,
	)
	failureKey := coordinator.store.failureKey(resultDigest)
	markerDeadline := time.Now().Add(500 * time.Millisecond)
	for {
		_, found, markerErr := coordinator.store.failedRecently(context.Background(), failureKey)
		if markerErr != nil {
			t.Fatal(markerErr)
		}
		if found {
			break
		}
		if time.Now().After(markerDeadline) {
			t.Fatal("timeout failure marker was not recorded asynchronously")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err = coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if !errors.Is(err, application.ErrProviderBusy) {
		t.Fatalf("immediate retry error=%v, want provider busy", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want one timed out attempt", calls)
	}
}

func TestForceUpstreamIgnoresRecentFailureMarker(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	provider.fail.Store(true)
	config := testConfig()
	config.CacheMode = cacheModeForceUpstream
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()

	_, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("initial query error=%v, want provider unavailable", err)
	}
	provider.fail.Store(false)
	rows, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(rows.Courses) != 1 {
		t.Fatalf("force-upstream retry rows=%+v err=%v", rows, err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls=%d, want one failed and one successful attempt", calls)
	}
}

func TestForceUpstreamDoesNotCoalesceConcurrentRequests(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{delay: 50 * time.Millisecond}
	config := testConfig()
	config.CacheMode = cacheModeForceUpstream
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	start := make(chan struct{})
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := coordinator.ListCourses(
				context.Background(), student, credential, "2026-spring",
			)
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls=%d, want one call per force-upstream request", calls)
	}
	if maximum := provider.maxActive.Load(); maximum != 1 {
		t.Fatalf("maximum concurrent provider calls=%d, want distributed lease serialization", maximum)
	}
}

func TestDeleteStudentRemovesEveryIndexedResult(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	if _, err := coordinator.ListCourses(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ListGrades(
		context.Background(), student, credential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.DeleteStudent(context.Background(), student.StudentNo); err != nil {
		t.Fatal(err)
	}
	for _, key := range server.Keys() {
		if strings.Contains(key, ":result:") || strings.Contains(key, ":index:student:") {
			t.Fatalf("student cache survived revocation: %q", key)
		}
	}
}

func TestDeleteStudentWaitsForInFlightQuery(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{delay: 100 * time.Millisecond}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	queryResult := make(chan error, 1)
	go func() {
		_, err := coordinator.ListCourses(
			context.Background(), student, credential, "2026-spring",
		)
		queryResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for provider.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if provider.calls.Load() == 0 {
		t.Fatal("provider query did not start")
	}
	if err := coordinator.DeleteStudent(context.Background(), student.StudentNo); err != nil {
		t.Fatal(err)
	}
	if err := <-queryResult; err != nil {
		t.Fatal(err)
	}
	for _, key := range server.Keys() {
		if strings.Contains(key, ":result:") || strings.Contains(key, ":index:student:") {
			t.Fatalf("in-flight query restored revoked cache: %q", key)
		}
	}
}

func TestCredentialFingerprintPreservesWhitespace(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	if coordinator.store.digest(" password") == coordinator.store.digest("password") {
		t.Fatal("distinct credential bytes share a fingerprint")
	}
}

func TestCoordinatorSupportsExamAndSelectionQueries(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	student, credential := testIdentity()
	exams, err := coordinator.ListExams(context.Background(), student, credential, "2026-spring")
	if err != nil || len(exams) != 1 || exams[0].PeriodID != "2026-spring" {
		t.Fatalf("exams=%+v err=%v", exams, err)
	}
	selections, err := coordinator.ListCourseSelections(
		context.Background(), student, credential, "2026-spring",
	)
	if err != nil || len(selections) != 1 || selections[0].PeriodID != "2026-spring" {
		t.Fatalf("selections=%+v err=%v", selections, err)
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls=%d, want 2", calls)
	}
}

func TestValidateConfigRejectsUnsafeBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "operation timeout", mutate: func(config *Config) { config.CoursesTimeout = 500 * time.Millisecond }},
		{name: "lease TTL", mutate: func(config *Config) { config.LeaseTTL = config.ExamsTimeout }},
		{name: "poll interval", mutate: func(config *Config) { config.PollInterval = time.Millisecond }},
		{name: "max concurrent", mutate: func(config *Config) { config.MaxConcurrent = 0 }},
		{name: "global rate", mutate: func(config *Config) { config.GlobalRate = 0 }},
		{name: "retry after", mutate: func(config *Config) { config.RetryAfter = 0 }},
		{name: "cache TTL", mutate: func(config *Config) { config.CoursesFreshTTL = 0 }},
		{name: "cache mode", mutate: func(config *Config) { config.CacheMode = "disabled" }},
		{name: "circuit threshold", mutate: func(config *Config) { config.CircuitThreshold = 0 }},
		{name: "circuit deadline ratio NaN", mutate: func(config *Config) { config.CircuitDeadlineRatio = math.NaN() }},
		{name: "circuit deadline ratio positive infinity", mutate: func(config *Config) { config.CircuitDeadlineRatio = math.Inf(1) }},
		{name: "circuit deadline ratio negative infinity", mutate: func(config *Config) { config.CircuitDeadlineRatio = math.Inf(-1) }},
		{name: "circuit window", mutate: func(config *Config) { config.CircuitWindow = 0 }},
		{name: "circuit open duration", mutate: func(config *Config) { config.CircuitOpenDuration = time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.mutate(&config)
			if err := validateConfig(config); err == nil {
				t.Fatal("unsafe query coordinator config was accepted")
			}
		})
	}
	if err := validateConfig(testConfig()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestRedisIntegerDecoding(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    int64
		wantErr bool
	}{
		{name: "integer", value: int64(7), want: 7},
		{name: "string", value: "8", want: 8},
		{name: "invalid string", value: "eight", wantErr: true},
		{name: "invalid type", value: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := redisInteger(test.value)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("redisInteger(%v)=%d, %v; want %d, error=%v", test.value, got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestGlobalRateLimitIsSharedAcrossStudents(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	config.GlobalRate = 1
	config.GlobalBurst = 1
	coordinator := newTestCoordinator(t, server, provider, config)
	firstStudent, firstCredential := testIdentity()
	if _, err := coordinator.ListCourses(
		context.Background(), firstStudent, firstCredential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	secondStudent := firstStudent
	secondStudent.UserID = 8
	secondStudent.StudentNo = "20260002"
	secondCredential := firstCredential
	secondCredential.StudentNo = secondStudent.StudentNo
	_, err := coordinator.ListCourses(
		context.Background(), secondStudent, secondCredential, "2026-spring",
	)
	if !errors.Is(err, application.ErrProviderBusy) {
		t.Fatalf("second cold query error=%v, want provider busy", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want 1", calls)
	}
}

func TestGlobalRateLimitHotUpdateStartsNewBucket(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	initial := testRateLimitPolicy(t, 1, 1)
	limits := &mutableRateLimits{policy: initial}
	config := testConfig()
	config.RateLimits = limits
	coordinator := newTestCoordinator(t, server, provider, config)
	student, credential := testIdentity()
	var err error
	if _, err = coordinator.ListCourses(context.Background(), student, credential, "2026-spring"); err != nil {
		t.Fatal(err)
	}

	secondStudent := student
	secondStudent.UserID = 8
	secondStudent.StudentNo = "20260002"
	secondCredential := credential
	secondCredential.StudentNo = secondStudent.StudentNo
	if _, err = coordinator.ListCourses(
		context.Background(),
		secondStudent,
		secondCredential,
		"2026-spring",
	); !errors.Is(err, application.ErrProviderBusy) {
		t.Fatalf("initial global bucket was not saturated: %v", err)
	}

	limits.policy = testRateLimitPolicy(t, 1, 2)
	thirdStudent := student
	thirdStudent.UserID = 9
	thirdStudent.StudentNo = "20260003"
	thirdCredential := credential
	thirdCredential.StudentNo = thirdStudent.StudentNo
	if _, err = coordinator.ListCourses(
		context.Background(),
		thirdStudent,
		thirdCredential,
		"2026-spring",
	); err != nil {
		t.Fatalf("updated policy reused saturated global bucket: %v", err)
	}
}

func TestOperationTimeoutDependsOnCacheState(t *testing.T) {
	config := testConfig()
	config.CoursesTimeout = 10 * time.Second
	config.GradesTimeout = 10 * time.Second
	config.ExamsTimeout = 12 * time.Second
	config.SelectionsTimeout = 12 * time.Second
	config.StaleRefreshTimeout = 5 * time.Second
	coordinator := &Coordinator{config: config}
	tests := []struct {
		operation string
		state     cacheState
		mode      string
		want      time.Duration
	}{
		{operation: operationCourses, state: cacheMiss, mode: cacheModeNormal, want: 10 * time.Second},
		{operation: operationGrades, state: cacheMiss, mode: cacheModeNormal, want: 10 * time.Second},
		{operation: operationExams, state: cacheMiss, mode: cacheModeNormal, want: 12 * time.Second},
		{operation: operationSelections, state: cacheMiss, mode: cacheModeNormal, want: 12 * time.Second},
		{operation: operationCourses, state: cacheStale, mode: cacheModeNormal, want: 5 * time.Second},
		{operation: operationExams, state: cacheStale, mode: cacheModeNormal, want: 5 * time.Second},
		{operation: operationCourses, state: cacheStale, mode: cacheModeNoStale, want: 10 * time.Second},
		{operation: operationExams, state: cacheStale, mode: cacheModeForceUpstream, want: 12 * time.Second},
	}
	for _, test := range tests {
		coordinator.config.CacheMode = test.mode
		if got := coordinator.timeout(test.operation, test.state); got != test.want {
			t.Errorf("timeout(%s, %d)=%s, want %s", test.operation, test.state, got, test.want)
		}
	}
}

func TestCircuitOpensAfterConsecutiveDeadlinesAndRejectsColdQuery(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	provider.deadline.Store(true)
	observer := newRecordingCoordinatorObserver()
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	config := testConfig()
	coordinator, err := New(
		provider,
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < config.CircuitThreshold; index++ {
		student, credential := indexedIdentity(index)
		_, err = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline query %d error=%v", index, err)
		}
	}
	student, credential := indexedIdentity(config.CircuitThreshold)
	_, err = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	if !errors.Is(err, application.ErrProviderBusy) {
		t.Fatalf("open circuit error=%v, want provider busy", err)
	}
	retryAfter, ok := application.ProviderRetryAfter(err)
	if !ok || retryAfter <= 0 || retryAfter > config.CircuitOpenDuration {
		t.Fatalf("open circuit retry-after=%s ok=%v", retryAfter, ok)
	}
	if calls := provider.calls.Load(); calls != int32(config.CircuitThreshold) {
		t.Fatalf("provider calls=%d, want %d", calls, config.CircuitThreshold)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.circuits["courses/open/undergraduate"] != 1 ||
		observer.circuits["courses/reject/undergraduate"] != 1 {
		t.Fatalf("circuit observations=%v", observer.circuits)
	}
}

func TestExpiredCircuitAllowsOnlyOneHalfOpenProbe(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	config := coordinator.config
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	completedAt := time.Now()
	server.SetTime(completedAt)

	_, _, opened, err := coordinator.store.recordDeadline(
		context.Background(),
		key,
		1,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		completedAt,
		"",
	)
	if err != nil || !opened {
		t.Fatalf("open circuit: opened=%v err=%v", opened, err)
	}
	server.FastForward(config.CircuitOpenDuration + time.Millisecond)
	server.SetTime(completedAt.Add(config.CircuitOpenDuration + time.Millisecond))

	open, _, probe, err := coordinator.store.circuitOpen(
		context.Background(), key, "probe-1", config.CoursesTimeout,
	)
	if err != nil || open || !probe {
		t.Fatalf("first half-open request: open=%v probe=%v err=%v", open, probe, err)
	}
	open, retryAfter, probe, err := coordinator.store.circuitOpen(
		context.Background(), key, "probe-2", config.CoursesTimeout,
	)
	if err != nil || !open || probe || retryAfter <= 0 {
		t.Fatalf(
			"concurrent half-open request: open=%v retryAfter=%s probe=%v err=%v",
			open,
			retryAfter,
			probe,
			err,
		)
	}
	open, _, probe, err = coordinator.store.circuitOpen(
		context.Background(), key, "probe-1", config.CoursesTimeout,
	)
	if err != nil || open || probe {
		t.Fatalf("probe owner recheck: open=%v probe=%v err=%v", open, probe, err)
	}

	_, _, opened, err = coordinator.store.recordDeadline(
		context.Background(),
		key,
		config.CircuitThreshold,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		completedAt.Add(time.Second),
		"probe-1",
	)
	if err != nil || !opened {
		t.Fatalf("failed half-open probe: opened=%v err=%v", opened, err)
	}
}

func TestCoordinatorSerializesHalfOpenProbeAcrossDistinctQueries(t *testing.T) {
	server := miniredis.RunT(t)
	release := make(chan struct{})
	provider := &recordingProvider{release: release}
	coordinator := newTestCoordinator(t, server, provider, testConfig())
	config := coordinator.config
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	completedAt := time.Now()
	server.SetTime(completedAt)
	if _, _, opened, err := coordinator.store.recordDeadline(
		context.Background(),
		key,
		1,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		completedAt,
		"",
	); err != nil || !opened {
		t.Fatalf("open circuit: opened=%v err=%v", opened, err)
	}
	server.FastForward(config.CircuitOpenDuration + time.Millisecond)
	server.SetTime(completedAt.Add(config.CircuitOpenDuration + time.Millisecond))

	firstDone := make(chan error, 1)
	firstStudent, firstCredential := indexedIdentity(200)
	go func() {
		_, err := coordinator.ListCourses(
			context.Background(), firstStudent, firstCredential, "2026-spring",
		)
		firstDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for provider.active.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active := provider.active.Load(); active != 1 {
		t.Fatalf("half-open probe did not reach provider: active=%d", active)
	}

	secondStudent, secondCredential := indexedIdentity(201)
	_, err := coordinator.ListCourses(
		context.Background(), secondStudent, secondCredential, "2026-fall",
	)
	if !errors.Is(err, application.ErrProviderBusy) {
		t.Fatalf("concurrent distinct query error=%v, want provider busy", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d, want one half-open probe", calls)
	}
	close(release)
	if err = <-firstDone; err != nil {
		t.Fatalf("half-open probe failed: %v", err)
	}
}

func TestDelayedDeadlineIsCountedAfterNewerSuccess(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	config := coordinator.config
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	base := time.Now()

	if _, _, _, err := coordinator.store.recordDeadline(
		context.Background(),
		key,
		config.CircuitThreshold,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		base,
		"",
	); err != nil {
		t.Fatal(err)
	}
	reset, err := coordinator.store.resetCircuit(
		context.Background(), key, base.Add(2*time.Second), config.CircuitWindow,
	)
	if err != nil || !reset {
		t.Fatalf("reset after success: reset=%v err=%v", reset, err)
	}

	// Simulate an older timeout whose asynchronous Redis bookkeeping runs
	// only after the newer successful upstream call has reset the scope. The
	// completion event still belongs in the rolling deadline window.
	if _, _, opened, err := coordinator.store.recordDeadline(
		context.Background(),
		key,
		config.CircuitThreshold,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		base.Add(time.Second),
		"",
	); err != nil || opened {
		t.Fatalf("delayed deadline: opened=%v err=%v", opened, err)
	}

	for index := 0; index < config.CircuitThreshold-2; index++ {
		_, _, opened, err := coordinator.store.recordDeadline(
			context.Background(),
			key,
			config.CircuitThreshold,
			config.CircuitWindow,
			config.CircuitOpenDuration,
			base.Add(time.Duration(3+index)*time.Second),
			"",
		)
		if err != nil || opened {
			t.Fatalf("new deadline %d: opened=%v err=%v", index, opened, err)
		}
	}
	_, _, opened, err := coordinator.store.recordDeadline(
		context.Background(),
		key,
		config.CircuitThreshold,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		base.Add(10*time.Second),
		"",
	)
	if err != nil || !opened {
		t.Fatalf("threshold deadline: opened=%v err=%v", opened, err)
	}
}

func TestOutOfOrderDeadlineStillCountsAfterSuccess(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	config := coordinator.config
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	base := time.Now()

	recordDeadline := func(name string, completedAt time.Time, wantOpened bool) {
		t.Helper()
		_, _, opened, err := coordinator.store.recordDeadline(
			context.Background(),
			key,
			config.CircuitThreshold,
			config.CircuitWindow,
			config.CircuitOpenDuration,
			completedAt,
			"",
		)
		if err != nil || opened != wantOpened {
			t.Fatalf("%s: opened=%v, want %v, err=%v", name, opened, wantOpened, err)
		}
	}

	// Redis observes F2 before the earlier success S1 and the oldest failure
	// F0. S1 must not discard either completed deadline from the rolling
	// protection window.
	recordDeadline("newer failure F2", base.Add(3*time.Second), false)
	reset, err := coordinator.store.resetCircuit(
		context.Background(), key, base.Add(2*time.Second), config.CircuitWindow,
	)
	if err != nil || reset {
		t.Fatalf("out-of-order success S1: reset=%v err=%v", reset, err)
	}
	recordDeadline("older failure F0", base.Add(time.Second), false)
	recordDeadline("second post-success failure", base.Add(4*time.Second), true)
}

func TestCircuitResetFallsBackToBoundedBackgroundBookkeeping(t *testing.T) {
	server := miniredis.RunT(t)
	observer := newRecordingCoordinatorObserver()
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	config := testConfig()
	coordinator, err := New(
		&recordingProvider{},
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := coordinator.store.circuitKey(operationCourses, "undergraduate")
	completedAt := time.Now()
	if _, _, _, err = coordinator.store.recordDeadline(
		context.Background(),
		key,
		config.CircuitThreshold,
		config.CircuitWindow,
		config.CircuitOpenDuration,
		completedAt.Add(-time.Second),
		"",
	); err != nil {
		t.Fatal(err)
	}

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	coordinator.resetCircuit(
		canceledContext,
		operationCourses,
		"undergraduate",
		key,
		completedAt,
	)
	resetObserved := func() bool {
		observer.mu.Lock()
		defer observer.mu.Unlock()
		return observer.circuits["courses/reset/undergraduate"] == 1
	}
	deadline := time.Now().Add(time.Second)
	for (server.HGet(key, "failures") != "" || !resetObserved()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if failures := server.HGet(key, "failures"); failures != "" {
		t.Fatalf("background reset left failures=%q", failures)
	}
	if got := server.HGet(key, "latest_success_at"); got != strconv.FormatInt(completedAt.UnixNano(), 10) {
		t.Fatalf("latest success=%q", got)
	}
	if !resetObserved() {
		observer.mu.Lock()
		defer observer.mu.Unlock()
		t.Fatalf("circuit observations=%v", observer.circuits)
	}
}

func TestCircuitIsIsolatedByEducationLevelAndIgnoresNonDeadlineFailures(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	provider.deadline.Store(true)
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	for index := 0; index < config.CircuitThreshold; index++ {
		student, credential := indexedIdentity(index)
		_, _ = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	}
	student, credential := indexedIdentity(100)
	student.EducationLevel = "graduate"
	_, err := coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("graduate query error=%v, want independent deadline", err)
	}

	otherServer := miniredis.RunT(t)
	otherProvider := &recordingProvider{}
	otherProvider.fail.Store(true)
	other := newTestCoordinator(t, otherServer, otherProvider, config)
	for index := 0; index < config.CircuitThreshold+1; index++ {
		student, credential = indexedIdentity(index)
		_, err = other.ListCourses(context.Background(), student, credential, "2026-fall")
		if !errors.Is(err, application.ErrProviderUnavailable) {
			t.Fatalf("unavailable query %d error=%v", index, err)
		}
	}
	if calls := otherProvider.calls.Load(); calls != int32(config.CircuitThreshold+1) {
		t.Fatalf("non-deadline provider calls=%d", calls)
	}
}

func TestSuccessfulUpstreamCallResetsDeadlineCircuitCount(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	provider.deadline.Store(true)
	config := testConfig()
	coordinator := newTestCoordinator(t, server, provider, config)
	for index := 0; index < config.CircuitThreshold-1; index++ {
		student, credential := indexedIdentity(index)
		_, _ = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	}
	provider.deadline.Store(false)
	student, credential := indexedIdentity(50)
	if _, err := coordinator.ListCourses(context.Background(), student, credential, "2026-spring"); err != nil {
		t.Fatal(err)
	}
	provider.deadline.Store(true)
	for index := 0; index < config.CircuitThreshold-1; index++ {
		student, credential = indexedIdentity(60 + index)
		_, err := coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("post-reset deadline query %d error=%v", index, err)
		}
	}
	student, credential = indexedIdentity(90)
	_, err := coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third post-reset query error=%v, want the opening deadline", err)
	}
}

func TestOpenCircuitReturnsStaleWithoutCallingProvider(t *testing.T) {
	server := miniredis.RunT(t)
	provider := &recordingProvider{}
	config := testConfig()
	observer := newRecordingCoordinatorObserver()
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	coordinator, err := New(
		provider,
		client,
		[]byte("0123456789abcdef0123456789abcdef"),
		config,
		zap.NewNop(),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}
	staleStudent, staleCredential := indexedIdentity(100)
	if _, err = coordinator.ListCourses(
		context.Background(), staleStudent, staleCredential, "2026-spring",
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.CoursesFreshTTL + 50*time.Millisecond)
	provider.deadline.Store(true)
	for index := 0; index < config.CircuitThreshold; index++ {
		student, credential := indexedIdentity(index)
		_, _ = coordinator.ListCourses(context.Background(), student, credential, "2026-spring")
	}
	callsBefore := provider.calls.Load()
	rows, err := coordinator.ListCourses(
		context.Background(), staleStudent, staleCredential, "2026-spring",
	)
	if err != nil || len(rows.Courses) != 1 {
		t.Fatalf("stale rows=%+v err=%v", rows, err)
	}
	if calls := provider.calls.Load(); calls != callsBefore {
		t.Fatalf("provider calls=%d, want %d", calls, callsBefore)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.stale["courses/circuit/undergraduate"] != 1 {
		t.Fatalf("stale observations=%v", observer.stale)
	}
}

func TestCacheV3CompressesEncryptsAndRoundTrips(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.resultKey("compressed")
	indexKey := coordinator.store.studentIndexKey("20260001")
	value := []string{strings.Repeat("repeated-academic-value-", 10_000)}
	plainPayload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.store.save(context.Background(), key, indexKey, value, time.Minute, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	encoded, err := server.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "repeated-academic-value") {
		t.Fatal("encrypted cache contains plaintext")
	}
	plaintext, err := coordinator.store.cipher.Decrypt(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	var record cacheRecord
	if err = json.Unmarshal([]byte(plaintext), &record); err != nil {
		t.Fatal(err)
	}
	if record.Version != cacheVersion || record.Encoding != cacheEncodingZstd {
		t.Fatalf("cache record version=%d encoding=%q", record.Version, record.Encoding)
	}
	if record.CachedAt == 0 || record.FreshUntil-record.CachedAt != time.Minute.Milliseconds() ||
		record.StaleUntil-record.CachedAt != (60*24*time.Hour).Milliseconds() {
		t.Fatalf("cache record times=%+v", record)
	}
	if len(record.Payload) >= len(plainPayload) {
		t.Fatalf("compressed payload=%d, original=%d", len(record.Payload), len(plainPayload))
	}
	var loaded []string
	state, err := coordinator.store.load(context.Background(), key, &loaded)
	if err != nil || state != cacheFresh || len(loaded) != 1 || loaded[0] != value[0] {
		t.Fatalf("loaded state=%d value-size=%d err=%v", state, len(loaded), err)
	}
}

func TestCacheReadsV2WithoutInventingCachedAt(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.resultKey("v2")
	now := time.Now()
	plaintext, err := json.Marshal(cacheRecord{
		Version: cacheVersionV2, Encoding: cacheEncodingZstd,
		FreshUntil: now.Add(time.Minute).UnixMilli(),
		StaleUntil: now.Add(time.Hour).UnixMilli(),
		Payload:    coordinator.store.encoder.EncodeAll([]byte(`["legacy-value"]`), nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := coordinator.store.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		t.Fatal(err)
	}
	server.Set(key, encoded)
	var loaded []string
	state, metadata, err := coordinator.store.loadWithMetadata(context.Background(), key, &loaded)
	if err != nil || state != cacheFresh || metadata != nil || len(loaded) != 1 || loaded[0] != "legacy-value" {
		t.Fatalf("state=%d metadata=%+v value=%v err=%v", state, metadata, loaded, err)
	}
}

func TestCacheRejectsV3WithInvalidTimestampOrdering(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.resultKey("invalid-time-order")
	now := time.Now()
	plaintext, err := json.Marshal(cacheRecord{
		Version: cacheVersion, Encoding: cacheEncodingZstd,
		CachedAt:   now.Add(2 * time.Minute).UnixMilli(),
		FreshUntil: now.Add(time.Minute).UnixMilli(),
		StaleUntil: now.Add(time.Hour).UnixMilli(),
		Payload:    coordinator.store.encoder.EncodeAll([]byte(`["invalid"]`), nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := coordinator.store.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		t.Fatal(err)
	}
	server.Set(key, encoded)
	var loaded []string
	state, metadata, err := coordinator.store.loadWithMetadata(context.Background(), key, &loaded)
	if err != nil || state != cacheMiss || metadata != nil || server.Exists(key) {
		t.Fatalf("state=%d metadata=%+v exists=%v err=%v", state, metadata, server.Exists(key), err)
	}
}

func TestCacheReadsEncryptedV1Record(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	key := coordinator.store.resultKey("legacy")
	payload, err := json.Marshal([]string{"legacy-value"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	plaintext, err := json.Marshal(cacheRecordV1{
		Version: cacheVersionV1, FreshUntil: now.Add(time.Minute).UnixMilli(),
		StaleUntil: now.Add(time.Hour).UnixMilli(), Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := coordinator.store.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		t.Fatal(err)
	}
	server.Set(key, encoded)
	var loaded []string
	state, err := coordinator.store.load(context.Background(), key, &loaded)
	if err != nil || state != cacheFresh || len(loaded) != 1 || loaded[0] != "legacy-value" {
		t.Fatalf("loaded state=%d value=%v err=%v", state, loaded, err)
	}
}

func TestCacheRejectsCorruptAndOversizedCompressedPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload func(*store) []byte
	}{
		{name: "corrupt", payload: func(*store) []byte { return []byte("not-zstd") }},
		{name: "oversized", payload: func(cacheStore *store) []byte {
			return cacheStore.encoder.EncodeAll(make([]byte, maxCachePayloadSize+1), nil)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := miniredis.RunT(t)
			coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
			key := coordinator.store.resultKey(test.name)
			now := time.Now()
			plaintext, err := json.Marshal(cacheRecord{
				Version: cacheVersion, Encoding: cacheEncodingZstd,
				FreshUntil: now.Add(time.Minute).UnixMilli(),
				StaleUntil: now.Add(time.Hour).UnixMilli(),
				Payload:    test.payload(coordinator.store),
			})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := coordinator.store.cipher.Encrypt(string(plaintext), key)
			if err != nil {
				t.Fatal(err)
			}
			server.Set(key, encoded)
			var value []string
			state, err := coordinator.store.load(context.Background(), key, &value)
			if err == nil || state != cacheMiss {
				t.Fatalf("state=%d err=%v, want rejected cache", state, err)
			}
			if server.Exists(key) {
				t.Fatal("rejected cache record was not deleted")
			}
		})
	}
}

func TestCacheRejectsUnknownVersionAndEncoding(t *testing.T) {
	tests := []cacheRecord{
		{Version: 99, Encoding: cacheEncodingZstd},
		{Version: cacheVersion, Encoding: "unknown"},
	}
	for index, record := range tests {
		server := miniredis.RunT(t)
		coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
		key := coordinator.store.resultKey(strconv.Itoa(index))
		now := time.Now()
		record.FreshUntil = now.Add(time.Minute).UnixMilli()
		record.StaleUntil = now.Add(time.Hour).UnixMilli()
		record.Payload = coordinator.store.encoder.EncodeAll([]byte(`[]`), nil)
		plaintext, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := coordinator.store.cipher.Encrypt(string(plaintext), key)
		if err != nil {
			t.Fatal(err)
		}
		server.Set(key, encoded)
		var value []string
		state, err := coordinator.store.load(context.Background(), key, &value)
		if err == nil || state != cacheMiss || server.Exists(key) {
			t.Fatalf("record=%+v state=%d exists=%v err=%v", record, state, server.Exists(key), err)
		}
	}
}

func TestStudentIndexTTLNeverShortensBelowResultLifetime(t *testing.T) {
	server := miniredis.RunT(t)
	coordinator := newTestCoordinator(t, server, &recordingProvider{}, testConfig())
	indexKey := coordinator.store.studentIndexKey("20260001")
	longTTL := 60 * 24 * time.Hour
	if err := coordinator.store.save(
		context.Background(), coordinator.store.resultKey("long"), indexKey,
		[]string{"long"}, time.Minute, longTTL,
	); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.store.save(
		context.Background(), coordinator.store.resultKey("short"), indexKey,
		[]string{"short"}, time.Minute, time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if ttl := server.TTL(indexKey); ttl < longTTL-time.Second {
		t.Fatalf("student index TTL=%s, want at least %s", ttl, longTTL-time.Second)
	}
}

func indexedIdentity(index int) (application.StudentReference, application.Credential) {
	studentNo := "2026" + strconv.Itoa(10_000+index)
	return application.StudentReference{
		UserID: uint64(index + 1), StudentNo: studentNo, Provider: "ouc", EducationLevel: "undergraduate",
	}, application.Credential{StudentNo: studentNo, Password: "private-password"}
}

func testRateLimitPolicy(t *testing.T, rate, burst int) ratelimitconfig.Policy {
	t.Helper()
	document := strings.Replace(
		ratelimitconfig.DefaultDocument(),
		`"global_rate": 10`,
		`"global_rate": `+strconv.Itoa(rate),
		1,
	)
	document = strings.Replace(
		document,
		`"global_burst": 20`,
		`"global_burst": `+strconv.Itoa(burst),
		1,
	)
	policy, err := ratelimitconfig.ParseDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
