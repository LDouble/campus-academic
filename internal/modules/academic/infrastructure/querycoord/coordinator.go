// Package querycoord coalesces and bounds private school-system reads.
package querycoord

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/LDouble/campus-academic/internal/core/ratelimitconfig"
	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

const (
	operationCourses                 = "courses"
	operationGrades                  = "grades"
	operationExams                   = "exams"
	operationSelections              = "selections"
	operationCourseSelectionSchedule = "course_selection_schedule"

	staleFallbackDeadline    = "deadline"
	staleFallbackUnavailable = "unavailable"
	staleFallbackBusy        = "busy"
	staleFallbackCircuit     = "circuit"

	circuitEventOpen   = "open"
	circuitEventReject = "reject"
	circuitEventReset  = "reset"

	cacheModeNormal        = "normal"
	cacheModeNoStale       = "no_stale"
	cacheModeForceUpstream = "force_upstream"
)

// Config controls compressed encrypted caching and distributed downstream protection.
type Config struct {
	CacheMode                   string
	CoursesTimeout              time.Duration
	GradesTimeout               time.Duration
	ExamsTimeout                time.Duration
	SelectionsTimeout           time.Duration
	StaleRefreshTimeout         time.Duration
	LeaseTTL                    time.Duration
	PollInterval                time.Duration
	MaxConcurrent               int
	GlobalRate                  int
	GlobalBurst                 int
	RateLimits                  ratelimitconfig.Provider
	RetryAfter                  time.Duration
	CoursesFreshTTL             time.Duration
	CoursesStaleTTL             time.Duration
	GradesFreshTTL              time.Duration
	GradesStaleTTL              time.Duration
	ExamsFreshTTL               time.Duration
	ExamsStaleTTL               time.Duration
	SelectionsFreshTTL          time.Duration
	SelectionsStaleTTL          time.Duration
	CircuitThreshold            int
	CircuitWindow               time.Duration
	CircuitOpenDuration         time.Duration
	CircuitMinimumSamples       int
	CircuitDeadlineThreshold    int
	CircuitDeadlineRatio        float64
	CircuitHardProtectionCount  int
	CircuitHardProtectionWindow time.Duration
}

type cachePolicy struct {
	fresh time.Duration
	stale time.Duration
}

// Coordinator wraps a real academic provider with cache, request coalescing,
// per-student serialization and a distributed global token bucket.
type Coordinator struct {
	next         application.Provider
	store        *store
	config       Config
	log          *zap.Logger
	observer     Observer
	group        singleflight.Group
	callSequence atomic.Uint64
	permits      chan struct{}
	bookkeeping  chan struct{}
}

// New creates a distributed academic query coordinator. The master key is
// expanded into isolated AES-GCM and HMAC keys before use.
func New(
	next application.Provider,
	client *redis.Client,
	masterKey []byte,
	config Config,
	log *zap.Logger,
	options ...Option,
) (*Coordinator, error) {
	if next == nil {
		return nil, fmt.Errorf("academic query provider is required")
	}
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("academic query cache key must contain exactly 32 bytes")
	}
	if config.CacheMode == "" {
		config.CacheMode = cacheModeNormal
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	encryptionKey, err := hkdf.Key(
		sha256.New,
		masterKey,
		nil,
		"campus/academic-query/cache-encryption/v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive academic query cache encryption key: %w", err)
	}
	fingerprintKey, err := hkdf.Key(
		sha256.New,
		masterKey,
		nil,
		"campus/academic-query/cache-fingerprint/v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive academic query cache fingerprint key: %w", err)
	}
	cacheStore, err := newStore(client, encryptionKey, fingerprintKey)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = zap.NewNop()
	}
	coordinator := &Coordinator{
		next: next, store: cacheStore, config: config, log: log,
		observer: noopObserver{},
		permits:  make(chan struct{}, config.MaxConcurrent),
		// One timed-out leader can enqueue a deadline marker, a failure marker,
		// and two lease releases. Keep those detached tasks strictly bounded.
		bookkeeping: make(chan struct{}, config.MaxConcurrent*4),
	}
	for _, option := range options {
		option(coordinator)
	}
	return coordinator, nil
}

// ListCourses returns one cached or coalesced timetable snapshot.
func (c *Coordinator) ListCourses(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	result, err := c.ListCoursesWithCache(ctx, student, credential, periodID)
	return result.Records, err
}

// ListCoursesWithCache returns course rows with provenance when Redis supplied
// the response. A successful downstream read intentionally has no metadata.
func (c *Coordinator) ListCoursesWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[domain.CourseSchedule], error) {
	return execute(
		ctx,
		c,
		student,
		credential,
		operationCourses,
		periodID,
		c.policy(operationCourses),
		func(callContext context.Context) (domain.CourseSchedule, error) {
			return c.next.ListCourses(callContext, student, credential, periodID)
		},
	)
}

func (c *Coordinator) GetCourseSelectionSchedule(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (domain.CourseSchedule, error) {
	scheduleProvider, ok := c.next.(interface {
		GetCourseSelectionSchedule(context.Context, application.StudentReference, application.Credential, string) (domain.CourseSchedule, error)
	})
	if !ok {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	return scheduleProvider.GetCourseSelectionSchedule(ctx, student, credential, periodID)
}

// ListGrades returns cached or coalesced released grades.
func (c *Coordinator) ListGrades(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Grade, error) {
	result, err := c.ListGradesWithCache(ctx, student, credential, periodID)
	return result.Records, err
}

// ListGradesWithCache returns grade rows with optional cache provenance.
func (c *Coordinator) ListGradesWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.Grade], error) {
	return execute(
		ctx,
		c,
		student,
		credential,
		operationGrades,
		periodID,
		c.policy(operationGrades),
		func(callContext context.Context) ([]domain.Grade, error) {
			return c.next.ListGrades(callContext, student, credential, periodID)
		},
	)
}

// ListExams returns cached or coalesced examination rows.
func (c *Coordinator) ListExams(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Exam, error) {
	result, err := c.ListExamsWithCache(ctx, student, credential, periodID)
	return result.Records, err
}

// ListExamsWithCache returns exam rows with optional cache provenance.
func (c *Coordinator) ListExamsWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.Exam], error) {
	return execute(
		ctx,
		c,
		student,
		credential,
		operationExams,
		periodID,
		c.policy(operationExams),
		func(callContext context.Context) ([]domain.Exam, error) {
			return c.next.ListExams(callContext, student, credential, periodID)
		},
	)
}

// ListCourseSelections returns cached or coalesced selection rows.
func (c *Coordinator) ListCourseSelections(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	result, err := c.ListCourseSelectionsWithCache(ctx, student, credential, periodID)
	return result.Records, err
}

// ListCourseSelectionsWithCache returns selection rows with optional cache provenance.
func (c *Coordinator) ListCourseSelectionsWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.CourseSelection], error) {
	return execute(
		ctx,
		c,
		student,
		credential,
		operationSelections,
		periodID,
		c.policy(operationSelections),
		func(callContext context.Context) ([]domain.CourseSelection, error) {
			return c.next.ListCourseSelections(callContext, student, credential, periodID)
		},
	)
}

func execute[T any](
	ctx context.Context,
	coordinator *Coordinator,
	student application.StudentReference,
	credential application.Credential,
	operation,
	periodID string,
	policy cachePolicy,
	loader func(context.Context) (T, error),
) (value application.QueryResult[T], err error) {
	var zero application.QueryResult[T]
	defer func() {
		coordinator.observer.ObserveAcademicQuery(
			operation,
			queryOutcome(err),
			student.EducationLevel,
		)
	}()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if credential.StudentNo != student.StudentNo {
		return zero, application.ErrInvalidCredentials
	}
	credentialDigest := coordinator.store.digest(
		"credential-v1",
		credential.StudentNo,
		credential.Password,
	)
	resultDigest := coordinator.store.digest(
		resultCacheDiscriminator(operation),
		student.Provider,
		student.EducationLevel,
		student.StudentNo,
		operation,
		periodID,
		credentialDigest,
	)
	resultKey := coordinator.store.resultKey(resultDigest)
	var cached T
	state, cacheMetadata, err := coordinator.store.loadWithMetadata(ctx, resultKey, &cached)
	coordinator.observer.ObserveAcademicCache(
		operation,
		cacheStateLabel(state, err),
		student.EducationLevel,
	)
	bypassObserved := false
	coordinator.observeCacheBypass(
		operation,
		state,
		err,
		student.EducationLevel,
		&bypassObserved,
	)
	if err != nil {
		coordinator.log.Warn("academic query cache read failed", zap.String("operation", operation), zap.Error(err))
	} else if state == cacheFresh && coordinator.returnsFresh() {
		return application.QueryResult[T]{Records: cached, Cache: cacheMetadata}, nil
	}
	callerID := coordinator.callSequence.Add(1)
	var leaderID atomic.Uint64
	load := func() (any, error) {
		leaderID.Store(callerID)
		if coordinator.config.CacheMode != cacheModeForceUpstream {
			coordinator.observeSingleflight(operation, "leader", student.EducationLevel)
		}
		leaderContext, cancel := detachedTimeoutContext(
			ctx,
			coordinator.timeout(operation, state),
		)
		defer cancel()
		return loadDistributed(
			leaderContext,
			coordinator,
			student,
			operation,
			resultKey,
			resultDigest,
			policy,
			&bypassObserved,
			loader,
		)
	}
	var resultChannel <-chan singleflight.Result
	if coordinator.config.CacheMode == cacheModeForceUpstream {
		forceResultChannel := make(chan singleflight.Result, 1)
		resultChannel = forceResultChannel
		go func() {
			value, loadErr := load()
			forceResultChannel <- singleflight.Result{Val: value, Err: loadErr}
		}()
	} else {
		resultChannel = coordinator.group.DoChan(resultDigest, load)
	}
	select {
	case result := <-resultChannel:
		if coordinator.config.CacheMode != cacheModeForceUpstream && leaderID.Load() != callerID {
			// The load closure runs only for the singleflight leader. A caller
			// receiving a result without running it waited on another request.
			coordinator.observeSingleflight(operation, "follower", student.EducationLevel)
		}
		if result.Err != nil {
			if coordinator.allowsStale() && state == cacheStale && staleEligible(result.Err) {
				coordinator.observer.ObserveAcademicStaleFallback(
					operation,
					staleFallbackReason(result.Err),
					student.EducationLevel,
				)
				return application.QueryResult[T]{Records: cached, Cache: cacheMetadata}, nil
			}
			return zero, result.Err
		}
		value, ok := result.Val.(application.QueryResult[T])
		if !ok {
			return zero, application.ErrProviderUnavailable
		}
		return value, nil
	case <-ctx.Done():
		if coordinator.config.CacheMode != cacheModeForceUpstream {
			knownLeaderID := leaderID.Load()
			if knownLeaderID != 0 && knownLeaderID != callerID {
				coordinator.observeSingleflight(operation, "follower", student.EducationLevel)
			} else if knownLeaderID == 0 {
				// DoChan may not have started the leader closure yet. Keep the
				// buffered result receive alive so a caller that stops waiting is
				// still classified as a follower once the leader is known.
				go func() {
					<-resultChannel
					if leaderID.Load() != callerID {
						coordinator.observeSingleflight(operation, "follower", student.EducationLevel)
					}
				}()
			}
		}
		return zero, ctx.Err()
	}
}

func (c *Coordinator) observeSingleflight(operation, role, educationLevel string) {
	if observer, ok := c.observer.(SingleflightObserver); ok {
		observer.ObserveAcademicSingleflight(operation, role, educationLevel)
	}
}

func resultCacheDiscriminator(operation string) string {
	if operation == operationCourses {
		// Courses now cache a CourseSchedule object rather than a bare course
		// slice. Keep the global record version stable so grades, exams, and
		// selections are not invalidated, while making old course keys unreadable.
		// v3 adds the per-course note to the cached snapshot. Isolate v2 so a
		// deployment immediately returns notes instead of serving old snapshots
		// with an implicitly empty field until their stale TTL expires.
		return "result-courses-v3"
	}
	return "result-v1"
}

// detachedTimeoutContext lets a shared leader survive one caller canceling,
// while preserving an existing end-to-end deadline as the hard upper bound.
func detachedTimeoutContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		return context.WithDeadline(detached, deadline)
	}
	return context.WithTimeout(detached, timeout)
}

func loadDistributed[T any](
	ctx context.Context,
	coordinator *Coordinator,
	student application.StudentReference,
	operation,
	resultKey,
	queryDigest string,
	policy cachePolicy,
	bypassObserved *bool,
	loader func(context.Context) (T, error),
) (application.QueryResult[T], error) {
	var zero application.QueryResult[T]
	queryLeaseKey := coordinator.store.queryLeaseKey(queryDigest)
	failureKey := coordinator.store.failureKey(queryDigest)
	circuitKey := coordinator.store.circuitKey(operation, student.EducationLevel)
	ticker := time.NewTicker(coordinator.config.PollInterval)
	defer ticker.Stop()
	for {
		var cached T
		state, cacheMetadata, err := coordinator.store.loadWithMetadata(ctx, resultKey, &cached)
		coordinator.observeCacheBypass(
			operation,
			state,
			err,
			student.EducationLevel,
			bypassObserved,
		)
		if err != nil {
			coordinator.log.Warn("academic query cache read failed", zap.String("operation", operation), zap.Error(err))
		} else if state == cacheFresh && coordinator.returnsFresh() {
			return application.QueryResult[T]{Records: cached, Cache: cacheMetadata}, nil
		}
		open, retryAfter, _, circuitErr := coordinator.store.circuitOpen(ctx, circuitKey, "", 0)
		if circuitErr != nil {
			return zero, application.ErrProviderUnavailable
		}
		if open {
			coordinator.observer.ObserveAcademicCircuit(
				operation,
				circuitEventReject,
				student.EducationLevel,
			)
			if coordinator.allowsStale() && state == cacheStale {
				coordinator.observer.ObserveAcademicStaleFallback(
					operation,
					staleFallbackCircuit,
					student.EducationLevel,
				)
				return application.QueryResult[T]{Records: cached, Cache: cacheMetadata}, nil
			}
			return zero, &application.ProviderBusyError{RetryAfter: retryAfter}
		}
		if coordinator.config.CacheMode != cacheModeForceUpstream {
			failureReason, failedRecently, failureErr := coordinator.store.failedRecently(ctx, failureKey)
			if failureErr != nil {
				return zero, application.ErrProviderUnavailable
			}
			if failedRecently {
				if coordinator.allowsStale() && state == cacheStale {
					coordinator.observer.ObserveAcademicStaleFallback(
						operation,
						failureReason,
						student.EducationLevel,
					)
					return application.QueryResult[T]{Records: cached, Cache: cacheMetadata}, nil
				}
				return zero, &application.ProviderBusyError{RetryAfter: coordinator.config.RetryAfter}
			}
		}
		queryToken, acquired, err := coordinator.store.acquireLease(
			ctx,
			queryLeaseKey,
			coordinator.config.LeaseTTL,
		)
		if err != nil {
			return zero, application.ErrProviderUnavailable
		}
		if acquired {
			return loadAsLeader(
				ctx,
				coordinator,
				student,
				operation,
				resultKey,
				queryLeaseKey,
				queryToken,
				failureKey,
				circuitKey,
				policy,
				state,
				cached,
				cacheMetadata,
				bypassObserved,
				loader,
			)
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-ticker.C:
		}
	}
}

func loadAsLeader[T any](
	ctx context.Context,
	c *Coordinator,
	student application.StudentReference,
	operation,
	resultKey,
	queryLeaseKey,
	queryToken string,
	failureKey string,
	circuitKey string,
	policy cachePolicy,
	staleState cacheState,
	staleValue T,
	staleMetadata *application.CacheMetadata,
	bypassObserved *bool,
	loader func(context.Context) (T, error),
) (application.QueryResult[T], error) {
	var zero application.QueryResult[T]
	defer func() {
		if err := c.releaseLease(ctx, queryLeaseKey, queryToken); err != nil {
			c.log.Warn("academic query lease release failed", zap.String("operation", operation), zap.Error(err))
		}
	}()
	studentDigest := c.store.digest(
		"student",
		student.StudentNo,
	)
	studentLeaseKey := c.store.studentLeaseKey(studentDigest)
	studentToken, err := c.waitForStudentLease(ctx, studentLeaseKey)
	if err != nil {
		return zero, err
	}
	defer func() {
		if releaseErr := c.releaseLease(ctx, studentLeaseKey, studentToken); releaseErr != nil {
			c.log.Warn("academic student lease release failed", zap.String("operation", operation), zap.Error(releaseErr))
		}
	}()
	// Another credential for the same student may have populated the result
	// while this request waited for the per-student lease.
	var refreshed T
	state, refreshedMetadata, err := c.store.loadWithMetadata(ctx, resultKey, &refreshed)
	c.observeCacheBypass(
		operation,
		state,
		err,
		student.EducationLevel,
		bypassObserved,
	)
	switch {
	case err != nil:
		c.log.Warn("academic query cache read failed", zap.String("operation", operation), zap.Error(err))
	case state == cacheFresh && c.returnsFresh():
		return application.QueryResult[T]{Records: refreshed, Cache: refreshedMetadata}, nil
	case state == cacheStale && c.allowsStale():
		staleState = state
		staleValue = refreshed
		staleMetadata = refreshedMetadata
	}
	probeTTL := c.config.LeaseTTL
	if deadline, ok := ctx.Deadline(); ok {
		probeTTL = time.Until(deadline)
		if probeTTL <= 0 {
			return zero, ctx.Err()
		}
	}
	open, retryAfter, probeAcquired, err := c.store.circuitOpen(
		ctx,
		circuitKey,
		queryToken,
		probeTTL,
	)
	if err != nil {
		return zero, application.ErrProviderUnavailable
	}
	if open {
		c.observer.ObserveAcademicCircuit(operation, circuitEventReject, student.EducationLevel)
		if c.allowsStale() && staleState == cacheStale {
			c.observer.ObserveAcademicStaleFallback(
				operation,
				staleFallbackCircuit,
				student.EducationLevel,
			)
			return application.QueryResult[T]{Records: staleValue, Cache: staleMetadata}, nil
		}
		return zero, &application.ProviderBusyError{RetryAfter: retryAfter}
	}
	releaseProbeOnExit := probeAcquired
	defer func() {
		if !releaseProbeOnExit {
			return
		}
		if releaseErr := c.releaseCircuitProbe(ctx, circuitKey, queryToken); releaseErr != nil {
			c.log.Warn(
				"academic query circuit probe release failed",
				zap.String("operation", operation),
				zap.Error(releaseErr),
			)
		}
	}()
	release, err := c.acquirePermit(ctx)
	if err != nil {
		return zero, err
	}
	defer release()
	globalRate := c.config.GlobalRate
	globalBurst := c.config.GlobalBurst
	revision := "bootstrap"
	if c.config.RateLimits != nil {
		policy := c.config.RateLimits.Resolve()
		if policy.Configured() {
			globalRate = policy.AcademicQuery.GlobalRate
			globalBurst = policy.AcademicQuery.GlobalBurst
			revision = policy.Revision()
		}
	}
	allowed, retryAfter, err := c.store.allowGlobal(
		ctx,
		revision,
		globalRate,
		globalBurst,
	)
	if err != nil {
		return zero, application.ErrProviderUnavailable
	}
	if !allowed {
		if retryAfter < c.config.RetryAfter {
			retryAfter = c.config.RetryAfter
		}
		c.markFailure(ctx, operation, failureKey, staleFallbackBusy)
		if c.allowsStale() && staleState == cacheStale {
			c.observer.ObserveAcademicStaleFallback(
				operation,
				staleFallbackBusy,
				student.EducationLevel,
			)
			return application.QueryResult[T]{Records: staleValue, Cache: staleMetadata}, nil
		}
		return zero, &application.ProviderBusyError{RetryAfter: retryAfter}
	}
	value, err := loader(ctx)
	if err != nil {
		if staleEligible(err) {
			reason := staleFallbackReason(err)
			if errors.Is(err, context.DeadlineExceeded) {
				releaseProbeOnExit = false
				c.recordTimedOutFailure(
					operation,
					student.EducationLevel,
					circuitKey,
					failureKey,
					queryToken,
					reason,
				)
			} else {
				c.markFailure(ctx, operation, failureKey, reason)
			}
			if c.allowsStale() && staleState == cacheStale {
				c.log.Warn("academic query using stale cache", zap.String("operation", operation))
				c.observer.ObserveAcademicStaleFallback(
					operation,
					reason,
					student.EducationLevel,
				)
				return application.QueryResult[T]{Records: staleValue, Cache: staleMetadata}, nil
			}
		}
		return zero, err
	}
	probeToken := ""
	if probeAcquired {
		probeToken = queryToken
	}
	c.resetCircuit(ctx, operation, student.EducationLevel, circuitKey, time.Now(), probeToken)
	cacheContext, cancelCacheWrite := context.WithTimeout(ctx, time.Second)
	defer cancelCacheWrite()
	if err = c.store.save(
		cacheContext,
		resultKey,
		c.store.studentIndexKey(student.StudentNo),
		value,
		policy.fresh,
		policy.stale,
	); err != nil {
		c.log.Warn("academic query cache write failed", zap.String("operation", operation), zap.Error(err))
	}
	return application.QueryResult[T]{Records: value}, nil
}

// DeleteStudent removes every cached academic query for one retained student number.
func (c *Coordinator) DeleteStudent(ctx context.Context, studentNo string) error {
	studentLeaseKey := c.store.studentLeaseKey(c.store.digest("student", studentNo))
	token, err := c.waitForStudentLease(ctx, studentLeaseKey)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := c.releaseLease(ctx, studentLeaseKey, token); releaseErr != nil {
			c.log.Warn("academic student lease release after revocation failed", zap.Error(releaseErr))
		}
	}()
	return c.store.deleteStudent(ctx, studentNo)
}

func (c *Coordinator) markFailure(ctx context.Context, operation, key, reason string) {
	markerContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := c.store.markFailure(markerContext, key, reason, c.config.RetryAfter); err != nil {
		c.log.Warn("academic query failure marker write failed", zap.String("operation", operation), zap.Error(err))
	}
}

func (c *Coordinator) recordDeadline(
	ctx context.Context,
	operation,
	educationLevel,
	key,
	probeToken string,
	completedAt time.Time,
) {
	markerContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	policy := c.circuitPolicy()
	_, _, opened, err := c.store.recordDeadlineWithPolicy(
		markerContext,
		key,
		policy.minimumSamples,
		policy.deadlineThreshold,
		policy.deadlineRatio,
		policy.hardProtectionCount,
		policy.hardProtectionWindow,
		c.config.CircuitWindow,
		c.config.CircuitOpenDuration,
		completedAt,
		probeToken,
	)
	if err != nil {
		c.log.Warn("academic query circuit update failed", zap.String("operation", operation), zap.Error(err))
		return
	}
	if opened {
		c.observer.ObserveAcademicCircuit(operation, circuitEventOpen, educationLevel)
	}
}

type circuitPolicy struct {
	minimumSamples       int
	deadlineThreshold    int
	deadlineRatio        float64
	hardProtectionCount  int
	hardProtectionWindow time.Duration
}

func (c *Coordinator) circuitPolicy() circuitPolicy {
	config := c.config
	minimumSamples := config.CircuitMinimumSamples
	deadlineThreshold := config.CircuitDeadlineThreshold
	deadlineRatio := config.CircuitDeadlineRatio
	hardProtectionCount := config.CircuitHardProtectionCount
	hardProtectionWindow := config.CircuitHardProtectionWindow
	if minimumSamples == 0 {
		minimumSamples = config.CircuitThreshold
	}
	if deadlineThreshold == 0 {
		deadlineThreshold = config.CircuitThreshold
	}
	if deadlineRatio == 0 {
		deadlineRatio = 1
	}
	if hardProtectionCount == 0 {
		hardProtectionCount = config.CircuitThreshold
	}
	if hardProtectionWindow == 0 {
		hardProtectionWindow = config.CircuitWindow
	}
	return circuitPolicy{minimumSamples, deadlineThreshold, deadlineRatio, hardProtectionCount, hardProtectionWindow}
}

func (c *Coordinator) resetCircuit(
	ctx context.Context,
	operation,
	educationLevel,
	key string,
	completedAt time.Time,
	probeTokens ...string,
) {
	probeToken := ""
	if len(probeTokens) > 0 {
		probeToken = probeTokens[0]
	}
	resetContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	reset, err := c.store.resetCircuitWithProbePolicy(
		resetContext,
		key,
		completedAt,
		c.config.CircuitWindow,
		c.config.CircuitMinimumSamples > 0,
		c.circuitPolicy().hardProtectionWindow,
		probeToken,
	)
	if err != nil {
		if resetContext.Err() != nil {
			c.enqueueBookkeeping(operation, "circuit reset", func(backgroundContext context.Context) error {
				backgroundReset, backgroundErr := c.store.resetCircuitWithProbePolicy(
					backgroundContext,
					key,
					completedAt,
					c.config.CircuitWindow,
					c.config.CircuitMinimumSamples > 0,
					c.circuitPolicy().hardProtectionWindow,
					probeToken,
				)
				if backgroundErr != nil {
					return backgroundErr
				}
				if backgroundReset {
					c.observer.ObserveAcademicCircuit(operation, circuitEventReset, educationLevel)
				}
				return nil
			})
			return
		}
		c.log.Warn("academic query circuit reset failed", zap.String("operation", operation), zap.Error(err))
		return
	}
	if reset {
		c.observer.ObserveAcademicCircuit(operation, circuitEventReset, educationLevel)
	}
}

func (c *Coordinator) recordTimedOutFailure(
	operation,
	educationLevel,
	circuitKey,
	failureKey,
	probeToken,
	reason string,
) {
	completedAt := time.Now()
	c.enqueueBookkeeping(operation, "timeout markers", func(ctx context.Context) error {
		c.recordDeadline(ctx, operation, educationLevel, circuitKey, probeToken, completedAt)
		c.markFailure(ctx, operation, failureKey, reason)
		return nil
	})
}

func (c *Coordinator) releaseCircuitProbe(ctx context.Context, key, token string) error {
	releaseContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := c.store.releaseCircuitProbe(releaseContext, key, token)
	if err == nil || releaseContext.Err() == nil {
		return err
	}
	c.enqueueBookkeeping("", "circuit probe release", func(backgroundContext context.Context) error {
		return c.store.releaseCircuitProbe(backgroundContext, key, token)
	})
	return nil
}

func (c *Coordinator) enqueueBookkeeping(
	operation,
	name string,
	task func(context.Context) error,
) {
	select {
	case c.bookkeeping <- struct{}{}:
		go func() {
			defer func() { <-c.bookkeeping }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := task(ctx); err != nil {
				c.log.Warn(
					"academic query background bookkeeping failed",
					zap.String("operation", operation),
					zap.String("bookkeeping", name),
					zap.Error(err),
				)
			}
		}()
	default:
		c.log.Warn(
			"academic query background bookkeeping queue is full",
			zap.String("operation", operation),
			zap.String("bookkeeping", name),
		)
	}
}

func (c *Coordinator) releaseLease(ctx context.Context, key, token string) error {
	releaseContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := c.store.releaseLease(releaseContext, key, token)
	if err == nil || releaseContext.Err() == nil {
		return err
	}
	c.enqueueBookkeeping("", "lease release", func(backgroundContext context.Context) error {
		return c.store.releaseLease(backgroundContext, key, token)
	})
	return nil
}

func (c *Coordinator) waitForStudentLease(ctx context.Context, key string) (string, error) {
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		token, acquired, err := c.store.acquireLease(ctx, key, c.config.LeaseTTL)
		if err != nil {
			return "", application.ErrProviderUnavailable
		}
		if acquired {
			return token, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) acquirePermit(ctx context.Context) (func(), error) {
	select {
	case c.permits <- struct{}{}:
		return func() { <-c.permits }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func staleEligible(err error) bool {
	return errors.Is(err, application.ErrProviderUnavailable) ||
		errors.Is(err, application.ErrProviderRetryable) ||
		errors.Is(err, application.ErrProviderBusy) ||
		errors.Is(err, context.DeadlineExceeded)
}

func staleFallbackReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return staleFallbackDeadline
	case errors.Is(err, application.ErrProviderBusy):
		return staleFallbackBusy
	case errors.Is(err, application.ErrProviderRetryable):
		return "retryable"
	default:
		return staleFallbackUnavailable
	}
}

func (c *Coordinator) timeout(operation string, state cacheState) time.Duration {
	if state == cacheStale && c.allowsStale() {
		return c.config.StaleRefreshTimeout
	}
	switch operation {
	case operationCourses:
		return c.config.CoursesTimeout
	case operationGrades:
		return c.config.GradesTimeout
	case operationExams:
		return c.config.ExamsTimeout
	default:
		return c.config.SelectionsTimeout
	}
}

func (c *Coordinator) returnsFresh() bool {
	return c.config.CacheMode != cacheModeForceUpstream
}

func (c *Coordinator) allowsStale() bool {
	return c.config.CacheMode == cacheModeNormal
}

func (c *Coordinator) observeCacheBypass(
	operation string,
	state cacheState,
	err error,
	educationLevel string,
	observed *bool,
) {
	if observed == nil || *observed || err != nil {
		return
	}
	bypassed := (state == cacheFresh && !c.returnsFresh()) ||
		(state == cacheStale && !c.allowsStale())
	if !bypassed {
		return
	}
	c.observer.ObserveAcademicCacheBypass(
		operation,
		cacheStateLabel(state, nil),
		educationLevel,
		c.config.CacheMode,
	)
	*observed = true
}

func (c *Coordinator) policy(operation string) cachePolicy {
	switch operation {
	case operationCourses:
		return cachePolicy{fresh: c.config.CoursesFreshTTL, stale: c.config.CoursesStaleTTL}
	case operationGrades:
		return cachePolicy{fresh: c.config.GradesFreshTTL, stale: c.config.GradesStaleTTL}
	case operationExams:
		return cachePolicy{fresh: c.config.ExamsFreshTTL, stale: c.config.ExamsStaleTTL}
	default:
		return cachePolicy{fresh: c.config.SelectionsFreshTTL, stale: c.config.SelectionsStaleTTL}
	}
}

func validateConfig(config Config) error {
	switch config.CacheMode {
	case cacheModeNormal, cacheModeNoStale, cacheModeForceUpstream:
	default:
		return fmt.Errorf("academic query cache mode must be normal, no_stale, or force_upstream")
	}
	timeouts := []time.Duration{
		config.CoursesTimeout,
		config.GradesTimeout,
		config.ExamsTimeout,
		config.SelectionsTimeout,
		config.StaleRefreshTimeout,
	}
	var maximumTimeout time.Duration
	for _, timeout := range timeouts {
		if timeout < time.Second || timeout > 30*time.Second {
			return fmt.Errorf("academic query operation timeout must be between 1s and 30s")
		}
		if timeout > maximumTimeout {
			maximumTimeout = timeout
		}
	}
	if config.LeaseTTL <= maximumTimeout || config.LeaseTTL > 2*time.Minute {
		return fmt.Errorf("academic query lease TTL must exceed every operation timeout and be at most 2m")
	}
	if config.PollInterval < 10*time.Millisecond || config.PollInterval > time.Second {
		return fmt.Errorf("academic query poll interval must be between 10ms and 1s")
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > 1024 {
		return fmt.Errorf("academic query max concurrency must be between 1 and 1024")
	}
	if config.GlobalRate < 1 || config.GlobalRate > 1000 ||
		config.GlobalBurst < 1 || config.GlobalBurst > 2000 {
		return fmt.Errorf("academic query global rate or burst is outside the safe range")
	}
	if config.RetryAfter < time.Second || config.RetryAfter > time.Minute {
		return fmt.Errorf("academic query retry-after must be between 1s and 1m")
	}
	for _, policy := range []cachePolicy{
		{fresh: config.CoursesFreshTTL, stale: config.CoursesStaleTTL},
		{fresh: config.GradesFreshTTL, stale: config.GradesStaleTTL},
		{fresh: config.ExamsFreshTTL, stale: config.ExamsStaleTTL},
		{fresh: config.SelectionsFreshTTL, stale: config.SelectionsStaleTTL},
	} {
		if policy.fresh < time.Second || policy.stale <= policy.fresh || policy.stale > 90*24*time.Hour {
			return fmt.Errorf("academic query cache TTLs are outside the safe range")
		}
	}
	if config.CircuitThreshold < 1 || config.CircuitThreshold > 100 {
		if config.CircuitMinimumSamples == 0 && config.CircuitDeadlineThreshold == 0 && config.CircuitHardProtectionCount == 0 {
			return fmt.Errorf("academic query circuit threshold must be between 1 and 100")
		}
	}
	policy := (&Coordinator{config: config}).circuitPolicy()
	if policy.minimumSamples < 1 || policy.minimumSamples > 10000 ||
		policy.deadlineThreshold < 1 || policy.deadlineThreshold > 10000 ||
		math.IsNaN(policy.deadlineRatio) || math.IsInf(policy.deadlineRatio, 0) ||
		policy.deadlineRatio < 0.01 || policy.deadlineRatio > 1 ||
		policy.hardProtectionCount < 1 || policy.hardProtectionCount > 10000 {
		return fmt.Errorf("academic query circuit ratio policy is outside the safe range")
	}
	if policy.hardProtectionWindow < time.Second || policy.hardProtectionWindow > 10*time.Minute {
		return fmt.Errorf("academic query circuit hard protection window must be between 1s and 10m")
	}
	if config.CircuitWindow < time.Second || config.CircuitWindow > 10*time.Minute {
		return fmt.Errorf("academic query circuit window must be between 1s and 10m")
	}
	if config.CircuitOpenDuration < 15*time.Second || config.CircuitOpenDuration > 30*time.Second {
		return fmt.Errorf("academic query circuit open duration must be between 15s and 30s")
	}
	return nil
}

var _ application.Provider = (*Coordinator)(nil)
