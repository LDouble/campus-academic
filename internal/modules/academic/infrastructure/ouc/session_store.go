package ouc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/weouc-plus/campus-academic/internal/core/configcenter"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
)

const (
	// sessionKeyPrefix is retained solely to read records written before
	// sessions were separated by authority boundary.
	sessionKeyPrefix       = "academic:ouc:session:v1:"
	scopedSessionKeyPrefix = "academic:ouc:session:v2:"
	studentIndexKeyPrefix  = "academic:ouc:student-sessions:v1:"
	maxSessionStateBytes   = 128 * 1024
	maxSessionCookieSets   = 32
	maxSessionCookies      = 128
	maxSessionCookieLength = 4096
)

var errInvalidCachedSession = errors.New("invalid cached OUC session")

// SessionScope bounds persisted cookies to a single authority boundary.
// Values are deliberately stable because they are part of the HMAC cache key.
type SessionScope string

const (
	SessionScopeIdentity      SessionScope = "identity"
	SessionScopeUndergraduate SessionScope = "undergraduate"
	SessionScopeGraduate      SessionScope = "graduate"
)

// SessionCookie is the minimum cookie representation persisted in the
// encrypted short-lived session cache.
type SessionCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Domain is empty for a host-only cookie. A non-empty value is the
	// canonical RFC 6265 domain attribute without its optional leading dot.
	Domain          string `json:"domain,omitempty"`
	Path            string `json:"path,omitempty"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms,omitempty"`
}

// SessionCookieSet contains cookies applicable to one allowlisted HTTPS URL.
type SessionCookieSet struct {
	URL     string          `json:"url"`
	Cookies []SessionCookie `json:"cookies"`
}

// SessionState is a transport-neutral snapshot of an OUC cookie jar.
type SessionState struct {
	Version     int                `json:"version"`
	Sets        []SessionCookieSet `json:"sets"`
	ValidatedAt int64              `json:"validated_at,omitempty"`
}

// SessionStore persists encrypted, credential-bound, short-lived OUC sessions.
type SessionStore interface {
	Load(context.Context, string, string) (SessionState, bool, error)
	Save(context.Context, string, string, SessionState, time.Duration) error
	Delete(context.Context, string, string) error
}

// ScopedSessionStore is implemented by stores that can isolate cookies for
// the identity provider and each academic system. SessionStore remains for
// compatibility with existing test doubles and out-of-tree integrations.
type ScopedSessionStore interface {
	SessionStore
	LoadScope(context.Context, string, string, SessionScope) (SessionState, bool, error)
	SaveScope(context.Context, string, string, SessionScope, SessionState, time.Duration) error
	DeleteScope(context.Context, string, string, SessionScope) error
}

// EncryptedRedisSessionStore stores encrypted session snapshots in Redis. Its
// keys are HMAC fingerprints and never contain a student number or password.
type EncryptedRedisSessionStore struct {
	client         *redis.Client
	cipher         *configcenter.Cipher
	fingerprintKey []byte
}

// NewEncryptedRedisSessionStore creates an encrypted, credential-bound cache.
func NewEncryptedRedisSessionStore(
	client *redis.Client,
	cipher *configcenter.Cipher,
	fingerprintKey []byte,
) (*EncryptedRedisSessionStore, error) {
	if client == nil {
		return nil, fmt.Errorf("OUC session Redis client is required")
	}
	if cipher == nil {
		return nil, fmt.Errorf("OUC session cipher is required")
	}
	if len(fingerprintKey) < 32 {
		return nil, fmt.Errorf("OUC session fingerprint key must contain at least 32 bytes")
	}
	return &EncryptedRedisSessionStore{
		client:         client,
		cipher:         cipher,
		fingerprintKey: append([]byte(nil), fingerprintKey...),
	}, nil
}

// Load decrypts a session only when the current credentials produce the same
// keyed fingerprint as the credentials used to save it.
func (s *EncryptedRedisSessionStore) Load(
	ctx context.Context,
	studentNo string,
	password string,
) (SessionState, bool, error) {
	return s.loadKey(ctx, s.cacheKey(studentNo, password))
}

func (s *EncryptedRedisSessionStore) loadKey(ctx context.Context, key string) (SessionState, bool, error) {
	encoded, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return SessionState{}, false, nil
	}
	if err != nil {
		return SessionState{}, false, fmt.Errorf("load encrypted OUC session: %w", err)
	}
	plaintext, err := s.cipher.Decrypt(encoded, key)
	if err != nil {
		return SessionState{}, false, fmt.Errorf("%w: decrypt OUC session: %v", errInvalidCachedSession, err)
	}
	if len(plaintext) > maxSessionStateBytes {
		return SessionState{}, false, fmt.Errorf("%w: state exceeds safe size", errInvalidCachedSession)
	}
	var state SessionState
	decoder := json.NewDecoder(strings.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil {
		return SessionState{}, false, fmt.Errorf("%w: decode OUC session state: %v", errInvalidCachedSession, err)
	}
	if err = validateSessionState(state); err != nil {
		return SessionState{}, false, fmt.Errorf("%w: %v", errInvalidCachedSession, err)
	}
	return state, true, nil
}

// LoadScope loads a v2 scoped record. A v1 record is read only as a migration
// source, filtered to the requested host boundary, and is never extended.
func (s *EncryptedRedisSessionStore) LoadScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
) (SessionState, bool, error) {
	if !validSessionScope(scope) {
		return SessionState{}, false, fmt.Errorf("invalid OUC session scope")
	}
	state, found, err := s.loadKey(ctx, s.scopedCacheKey(studentNo, password, scope))
	if err != nil {
		return state, found, err
	}
	if found {
		if err := validateScopedSessionState(state, scope); err != nil {
			return SessionState{}, false, fmt.Errorf("%w: %v", errInvalidCachedSession, err)
		}
		return state, true, nil
	}
	// The legacy v1 record is intentionally a read-only migration source. A
	// successful scoped save after a real hand-off or query writes v2.
	legacy, legacyFound, err := s.Load(ctx, studentNo, password)
	if err != nil {
		return SessionState{}, false, err
	}
	if legacyFound {
		migrated, filterErr := filterSessionState(legacy, scope)
		if filterErr != nil {
			return SessionState{}, false, fmt.Errorf("%w: %v", errInvalidCachedSession, filterErr)
		}
		if len(migrated.Sets) > 0 {
			return migrated, true, nil
		}
	}
	// Early course-catalog callers isolated undergraduate and graduate cookies
	// by prefixing the v1 cache subject with the education level. Check that
	// historical namespace only after the ordinary v1 source misses or contains
	// no cookies for this target scope.
	legacySubject, historical := legacyScopedSessionSubject(studentNo, scope)
	if !historical {
		return SessionState{}, false, nil
	}
	legacy, legacyFound, err = s.Load(ctx, legacySubject, password)
	if err != nil || !legacyFound {
		return SessionState{}, false, err
	}
	migrated, err := filterSessionState(legacy, scope)
	if err != nil {
		return SessionState{}, false, fmt.Errorf("%w: %v", errInvalidCachedSession, err)
	}
	if len(migrated.Sets) == 0 {
		return SessionState{}, false, nil
	}
	return migrated, true, nil
}

// Save encrypts a session with a bounded TTL.
func (s *EncryptedRedisSessionStore) Save(
	ctx context.Context,
	studentNo string,
	password string,
	state SessionState,
	ttl time.Duration,
) error {
	return s.saveKey(ctx, studentNo, password, s.cacheKey(studentNo, password), state, ttl)
}

func (s *EncryptedRedisSessionStore) saveKey(
	ctx context.Context,
	studentNo string,
	password string,
	key string,
	state SessionState,
	ttl time.Duration,
) error {
	if ttl <= 0 || ttl > time.Hour {
		return fmt.Errorf("OUC session TTL is outside the safe range")
	}
	if err := validateSessionState(state); err != nil {
		return err
	}
	plaintext, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode OUC session state: %w", err)
	}
	if len(plaintext) > maxSessionStateBytes {
		return fmt.Errorf("OUC session state exceeds safe size")
	}
	encoded, err := s.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		return fmt.Errorf("encrypt OUC session: %w", err)
	}
	indexKey := s.studentIndexKey(studentNo)
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, key, encoded, ttl)
		pipe.SAdd(ctx, indexKey, key)
		// Multiple scoped sessions have different lifetimes. Never let an
		// identity-session refresh shorten the student cleanup index below a
		// still-live target session.
		// EXPIREGT does not apply to a non-volatile key, so establish the
		// first expiry before applying the max-TTL extension.
		pipe.ExpireNX(ctx, indexKey, ttl)
		pipe.ExpireGT(ctx, indexKey, ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("save encrypted OUC session: %w", err)
	}
	return nil
}

// SaveScope saves one authority-bounded session with its own TTL.
func (s *EncryptedRedisSessionStore) SaveScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
	state SessionState,
	ttl time.Duration,
) error {
	if !validSessionScope(scope) {
		return fmt.Errorf("invalid OUC session scope")
	}
	if err := validateScopedSessionState(state, scope); err != nil {
		return err
	}
	return s.saveKey(ctx, studentNo, password, s.scopedCacheKey(studentNo, password, scope), state, ttl)
}

// Delete removes the session bound to the supplied credentials.
func (s *EncryptedRedisSessionStore) Delete(
	ctx context.Context,
	studentNo string,
	password string,
) error {
	key := s.cacheKey(studentNo, password)
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, key)
		pipe.SRem(ctx, s.studentIndexKey(studentNo), key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete encrypted OUC session: %w", err)
	}
	return nil
}

// DeleteScope removes only cookies belonging to the requested authority.
func (s *EncryptedRedisSessionStore) DeleteScope(
	ctx context.Context,
	studentNo string,
	password string,
	scope SessionScope,
) error {
	if !validSessionScope(scope) {
		return fmt.Errorf("invalid OUC session scope")
	}
	key := s.scopedCacheKey(studentNo, password, scope)
	legacyKey := s.cacheKey(studentNo, password)
	legacySubject, hasHistoricalKey := legacyScopedSessionSubject(studentNo, scope)
	var historicalKey string
	if hasHistoricalKey {
		historicalKey = s.cacheKey(legacySubject, password)
	}
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		// A legacy v1 record contains every authority in one value. Once any
		// scope is explicitly rejected it cannot be selectively repaired, so
		// remove that migration source as well. Existing v2 scopes stay intact.
		pipe.Del(ctx, key, legacyKey)
		pipe.SRem(ctx, s.studentIndexKey(studentNo), key, legacyKey)
		if hasHistoricalKey {
			pipe.Del(ctx, historicalKey)
			pipe.SRem(ctx, s.studentIndexKey(legacySubject), historicalKey)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete encrypted OUC scoped session: %w", err)
	}
	return nil
}

// DeleteStudent removes every indexed school session for a retained student
// number. Index keys are HMAC fingerprints and never expose the student number.
func (s *EncryptedRedisSessionStore) DeleteStudent(
	ctx context.Context,
	studentNo string,
) error {
	subjects := []string{
		studentNo,
		string(SessionScopeUndergraduate) + "\x00" + studentNo,
		string(SessionScopeGraduate) + "\x00" + studentNo,
	}
	indexKeys := make([]string, 0, len(subjects))
	keysByName := make(map[string]struct{})
	for _, subject := range subjects {
		indexKey := s.studentIndexKey(subject)
		indexKeys = append(indexKeys, indexKey)
		keys, err := s.client.SMembers(ctx, indexKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("list encrypted OUC sessions: %w", err)
		}
		for _, key := range keys {
			keysByName[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keysByName))
	for key := range keysByName {
		keys = append(keys, key)
	}
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		if len(keys) > 0 {
			pipe.Del(ctx, keys...)
		}
		pipe.Del(ctx, indexKeys...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete student OUC sessions: %w", err)
	}
	return nil
}

func (s *EncryptedRedisSessionStore) cacheKey(studentNo string, password string) string {
	mac := hmac.New(sha256.New, s.fingerprintKey)
	_, _ = mac.Write([]byte(strings.TrimSpace(studentNo)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(password))
	return sessionKeyPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *EncryptedRedisSessionStore) scopedCacheKey(studentNo string, password string, scope SessionScope) string {
	mac := hmac.New(sha256.New, s.fingerprintKey)
	_, _ = mac.Write([]byte("scope"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.TrimSpace(studentNo)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(password))
	return scopedSessionKeyPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func legacyScopedSessionSubject(studentNo string, scope SessionScope) (string, bool) {
	switch scope {
	case SessionScopeUndergraduate, SessionScopeGraduate:
		return string(scope) + "\x00" + studentNo, true
	default:
		return "", false
	}
}

func (s *EncryptedRedisSessionStore) studentIndexKey(studentNo string) string {
	mac := hmac.New(sha256.New, s.fingerprintKey)
	_, _ = mac.Write([]byte("student-index"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.TrimSpace(studentNo)))
	return studentIndexKeyPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validateSessionState(state SessionState) error {
	if state.Version != 1 {
		return fmt.Errorf("unsupported OUC session state version")
	}
	if len(state.Sets) == 0 || len(state.Sets) > maxSessionCookieSets {
		return fmt.Errorf("OUC session cookie-set count is outside the safe range")
	}
	totalCookies := 0
	for _, set := range state.Sets {
		parsed, err := url.Parse(set.URL)
		if err != nil ||
			parsed.Scheme != "https" ||
			!allowedOUCHost(parsed.Hostname()) ||
			parsed.User != nil ||
			parsed.RawQuery != "" ||
			parsed.Fragment != "" {
			return fmt.Errorf("OUC session contains a non-allowlisted URL")
		}
		totalCookies += len(set.Cookies)
		if totalCookies > maxSessionCookies {
			return fmt.Errorf("OUC session cookie count exceeds safe limit")
		}
		for _, cookie := range set.Cookies {
			if strings.TrimSpace(cookie.Name) == "" ||
				len(cookie.Name) > maxSessionCookieLength ||
				len(cookie.Value) > maxSessionCookieLength ||
				cookie.ExpiresAtUnixMS < 0 ||
				(cookie.Path != "" && (!strings.HasPrefix(cookie.Path, "/") || len(cookie.Path) > 1024 ||
					strings.ContainsAny(cookie.Path, "\r\n\x00"))) {
				return fmt.Errorf("OUC session contains an invalid cookie")
			}
			if !validSessionCookieDomain(parsed.Hostname(), cookie.Domain) {
				return fmt.Errorf("OUC session contains an invalid cookie domain")
			}
		}
	}
	if totalCookies == 0 {
		return fmt.Errorf("OUC session contains no cookies")
	}
	return nil
}

func validSessionScope(scope SessionScope) bool {
	switch scope {
	case SessionScopeIdentity, SessionScopeUndergraduate, SessionScopeGraduate:
		return true
	default:
		return false
	}
}

func validateScopedSessionState(state SessionState, scope SessionScope) error {
	if err := validateSessionState(state); err != nil {
		return err
	}
	for _, set := range state.Sets {
		parsed, _ := url.Parse(set.URL)
		if !scopeAllowsHost(scope, parsed.Hostname()) {
			return fmt.Errorf("OUC session contains a cookie outside its scope")
		}
		if scope != SessionScopeIdentity {
			for _, cookie := range set.Cookies {
				if cookieDomain, _ := canonicalSessionCookieDomain(cookie.Domain); cookieDomain == "ouc.edu.cn" {
					return fmt.Errorf("OUC target session contains a parent-domain cookie")
				}
			}
		}
	}
	return nil
}

func scopeAllowsHost(scope SessionScope, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch scope {
	case SessionScopeIdentity:
		return host == "id.ouc.edu.cn" || host == "my.ouc.edu.cn"
	case SessionScopeUndergraduate:
		return host == "jwgl2024.ouc.edu.cn"
	case SessionScopeGraduate:
		return host == "pgs.ouc.edu.cn"
	default:
		return false
	}
}

func filterSessionState(state SessionState, scope SessionScope) (SessionState, error) {
	if err := validateSessionState(state); err != nil {
		return SessionState{}, err
	}
	identityCookies := make(map[string]struct{})
	if scope != SessionScopeIdentity {
		for _, set := range state.Sets {
			parsed, _ := url.Parse(set.URL)
			if !scopeAllowsHost(SessionScopeIdentity, parsed.Hostname()) {
				continue
			}
			for _, cookie := range set.Cookies {
				identityCookies[cookie.Name+"\x00"+cookie.Value] = struct{}{}
			}
		}
	}
	filtered := SessionState{Version: 1, ValidatedAt: state.ValidatedAt}
	for _, set := range state.Sets {
		parsed, _ := url.Parse(set.URL)
		if !scopeAllowsHost(scope, parsed.Hostname()) {
			continue
		}
		filteredSet := SessionCookieSet{URL: set.URL}
		for _, cookie := range set.Cookies {
			// Legacy v1 records did not preserve Domain. Their parent-domain
			// cookies are still identified by the old identity-cookie comparison
			// below. New records retain Domain and can be rejected directly.
			if scope != SessionScopeIdentity {
				cookieDomain, _ := canonicalSessionCookieDomain(cookie.Domain)
				if cookieDomain == "ouc.edu.cn" {
					continue
				}
			}
			if _, identityCookie := identityCookies[cookie.Name+"\x00"+cookie.Value]; identityCookie {
				continue
			}
			filteredSet.Cookies = append(filteredSet.Cookies, cookie)
		}
		if len(filteredSet.Cookies) > 0 {
			filtered.Sets = append(filtered.Sets, filteredSet)
		}
	}
	return filtered, nil
}

func snapshotSession(
	current *session,
	config academicconfig.OUCConfig,
) (SessionState, error) {
	if current == nil || current.client == nil || current.client.Jar == nil {
		return SessionState{}, fmt.Errorf("OUC session cookie jar is unavailable")
	}
	state := SessionState{Version: 1}
	for _, origin := range sessionOrigins(config) {
		cookies := current.client.Jar.Cookies(origin)
		if len(cookies) == 0 {
			continue
		}
		set := SessionCookieSet{URL: origin.String()}
		for _, cookie := range cookies {
			set.Cookies = append(set.Cookies, SessionCookie{
				Name:  cookie.Name,
				Value: cookie.Value,
			})
		}
		state.Sets = append(state.Sets, set)
	}
	if err := validateSessionState(state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func snapshotSessionScope(
	current *session,
	_ academicconfig.OUCConfig,
	scope SessionScope,
	validatedAt time.Time,
) (SessionState, error) {
	if current == nil || current.client == nil || current.client.Jar == nil {
		return SessionState{}, fmt.Errorf("OUC session cookie jar is unavailable")
	}
	jar, ok := current.client.Jar.(*trackedCookieJar)
	if !ok {
		return SessionState{}, fmt.Errorf("OUC scoped session cookie provenance is unavailable")
	}
	state, err := jar.snapshotScope(scope)
	if err != nil {
		return SessionState{}, err
	}
	if err = validateScopedSessionState(state, scope); err != nil {
		return SessionState{}, err
	}
	if !validatedAt.IsZero() {
		state.ValidatedAt = validatedAt.UnixMilli()
	}
	return state, nil
}

// snapshotSessionForRecovery is an ephemeral, multi-authority snapshot used
// only to give each singleflight waiter an independent CookieJar. It preserves
// identity-cookie provenance for an immediate target hand-off, while the
// subsequent persistent target snapshot still filters those cookies out.
func snapshotSessionForRecovery(
	current *session,
	targetScope SessionScope,
) (SessionState, error) {
	if current == nil || current.client == nil || current.client.Jar == nil {
		return SessionState{}, fmt.Errorf("OUC session cookie jar is unavailable")
	}
	if targetScope == SessionScopeIdentity || !validSessionScope(targetScope) {
		return SessionState{}, fmt.Errorf("invalid OUC target session scope")
	}
	jar, ok := current.client.Jar.(*trackedCookieJar)
	if !ok {
		return SessionState{}, fmt.Errorf("OUC scoped session cookie provenance is unavailable")
	}
	state, err := jar.snapshotForRecovery(targetScope)
	if err != nil {
		return SessionState{}, err
	}
	if err = validateSessionState(state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func restoreSession(
	config academicconfig.OUCConfig,
	state SessionState,
	clientFactory sessionClientFactory,
) (*session, error) {
	if err := validateSessionState(state); err != nil {
		return nil, err
	}
	if clientFactory == nil {
		clientFactory = newSessionClient
	}
	client, err := clientFactory(config)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	restoredCookies := 0
	for _, set := range state.Sets {
		origin, parseErr := url.Parse(set.URL)
		if parseErr != nil {
			return nil, fmt.Errorf("parse OUC session URL: %w", parseErr)
		}
		cookies := make([]*http.Cookie, 0, len(set.Cookies))
		for _, cookie := range set.Cookies {
			var expires time.Time
			if cookie.ExpiresAtUnixMS > 0 {
				expires = time.UnixMilli(cookie.ExpiresAtUnixMS)
				if !expires.After(now) {
					continue
				}
			}
			cookies = append(cookies, &http.Cookie{
				Name:    cookie.Name,
				Value:   cookie.Value,
				Domain:  cookie.Domain,
				Path:    cookie.Path,
				Expires: expires,
				Secure:  true,
			})
		}
		if len(cookies) == 0 {
			continue
		}
		client.Jar.SetCookies(origin, cookies)
		restoredCookies += len(cookies)
	}
	if restoredCookies == 0 {
		return nil, fmt.Errorf("OUC session contains no live cookies")
	}
	return &session{client: client}, nil
}

func canonicalSessionCookieDomain(domain string) (string, bool) {
	if domain == "" {
		return "", true
	}
	if strings.TrimSpace(domain) != domain {
		return "", false
	}
	canonical := strings.ToLower(strings.TrimPrefix(domain, "."))
	if canonical == "" || len(canonical) > 253 || strings.HasPrefix(canonical, ".") {
		return "", false
	}
	for _, label := range strings.Split(canonical, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if (character < 'a' || character > 'z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return "", false
			}
		}
	}
	return canonical, true
}

// validSessionCookieDomain permits only host-only cookies, cookies scoped to
// the source host, and the OUC parent domain needed by identity SSO. The
// caller decides whether a scope may use the parent-domain exception.
func validSessionCookieDomain(sourceHost string, domain string) bool {
	canonical, ok := canonicalSessionCookieDomain(domain)
	if !ok || canonical == "" {
		return ok
	}
	sourceHost = strings.ToLower(strings.TrimSpace(sourceHost))
	return canonical == sourceHost || canonical == "ouc.edu.cn"
}

func sessionOrigins(config academicconfig.OUCConfig) []*url.URL {
	rawURLs := []string{
		config.SSOLoginURL,
		config.PortalServiceURL,
		config.Undergraduate.ServiceURL,
		config.Graduate.ServiceURL,
	}
	for _, endpoint := range []academicconfig.EndpointSet{
		config.Undergraduate,
		config.Graduate,
	} {
		base, err := url.Parse(endpoint.ServiceURL)
		if err != nil {
			continue
		}
		for _, operation := range []academicconfig.OperationEndpoint{
			endpoint.Periods,
			endpoint.Courses,
			endpoint.Grades,
			endpoint.Exams,
			endpoint.Selections,
			endpoint.CourseCatalog,
		} {
			reference, parseErr := url.Parse(
				strings.ReplaceAll(operation.Path, "{period_id}", "session"),
			)
			if parseErr == nil {
				rawURLs = append(rawURLs, base.ResolveReference(reference).String())
			}
		}
	}
	seen := make(map[string]struct{}, len(rawURLs))
	result := make([]*url.URL, 0, len(rawURLs))
	for _, raw := range rawURLs {
		parsed, err := url.Parse(raw)
		if err != nil ||
			parsed.Scheme != "https" ||
			!allowedOUCHost(parsed.Hostname()) {
			continue
		}
		parsed.RawQuery = ""
		parsed.Fragment = ""
		key := parsed.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, parsed)
	}
	return result
}

var _ SessionStore = (*EncryptedRedisSessionStore)(nil)
