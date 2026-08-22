package ouc

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// trackedCookieJar preserves the host that set each cookie. net/http's public
// CookieJar API intentionally returns only cookies applicable to a URL, which
// is insufficient for separating a parent-domain SSO cookie from a cookie set
// by an academic target. The underlying jar remains the source of truth for
// request behaviour; this side index is used only for encrypted snapshots.
type trackedCookieJar struct {
	base    http.CookieJar
	mu      sync.RWMutex
	records map[string]trackedCookieRecord
	now     func() time.Time
}

type trackedCookieRecord struct {
	sourceURL  string
	sourceHost string
	// domain is the effective domain used for RFC 6265 cookie-slot identity.
	domain string
	// cookieDomain is the persisted Domain attribute. Its empty value is
	// meaningful: it represents a host-only cookie.
	cookieDomain string
	path         string
	name         string
	value        string
	expires      time.Time
}

func newTrackedCookieJar(base http.CookieJar) *trackedCookieJar {
	return &trackedCookieJar{
		base:    base,
		records: make(map[string]trackedCookieRecord),
		now:     time.Now,
	}
}

func (j *trackedCookieJar) Cookies(target *url.URL) []*http.Cookie {
	return j.base.Cookies(target)
}

func (j *trackedCookieJar) SetCookies(source *url.URL, cookies []*http.Cookie) {
	j.base.SetCookies(source, cookies)
	if source == nil || source.Scheme != "https" || !allowedOUCHost(source.Hostname()) {
		return
	}
	sourceURL := *source
	sourceURL.User = nil
	sourceURL.RawQuery = ""
	sourceURL.Fragment = ""
	sourceHost := strings.ToLower(sourceURL.Hostname())
	now := j.now()

	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		if cookie == nil || cookie.Valid() != nil || strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		cookieDomain, validDomain := canonicalSessionCookieDomain(cookie.Domain)
		if !validDomain || !validSessionCookieDomain(sourceHost, cookieDomain) {
			continue
		}
		domain := cookieDomain
		if domain == "" {
			domain = sourceHost
		}
		if domain != sourceHost && domain != "ouc.edu.cn" {
			continue
		}
		path := cookie.Path
		if path == "" || path[0] != '/' {
			path = defaultCookiePath(sourceURL.Path)
		}
		// RFC 6265 identifies a cookie slot by effective domain, path, and
		// name—not by the host that most recently set it. Keeping provenance in
		// the value lets a cross-subdomain overwrite or deletion replace the old
		// source instead of resurrecting it from this side index later.
		key := strings.Join([]string{domain, path, cookie.Name}, "\x00")
		expires := cookie.Expires
		if cookie.MaxAge > 0 {
			// Max-Age takes precedence over Expires. Persist the resulting
			// absolute deadline so restoring a Redis snapshot cannot extend a
			// short-lived upstream cookie to the target session TTL.
			expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
		}
		if cookie.MaxAge < 0 || (!expires.IsZero() && !expires.After(now)) {
			delete(j.records, key)
			continue
		}
		j.records[key] = trackedCookieRecord{
			sourceURL:    sourceURL.String(),
			sourceHost:   sourceHost,
			domain:       domain,
			cookieDomain: cookieDomain,
			path:         path,
			name:         cookie.Name,
			value:        cookie.Value,
			expires:      expires,
		}
	}
}

func (j *trackedCookieJar) snapshotScope(scope SessionScope) (SessionState, error) {
	return j.snapshotRecords(false, scope)
}

// snapshotForRecovery creates an in-memory hand-off snapshot while retaining
// the authority that originally set every cookie. Parent-domain target cookies
// are safe here because this state is never persisted and later target-only
// snapshots still reject them.
func (j *trackedCookieJar) snapshotForRecovery(targetScope SessionScope) (SessionState, error) {
	return j.snapshotRecords(true, SessionScopeIdentity, targetScope)
}

func (j *trackedCookieJar) snapshotRecords(
	allowTargetParentDomain bool,
	scopes ...SessionScope,
) (SessionState, error) {
	if len(scopes) == 0 {
		return SessionState{}, fmt.Errorf("OUC session scope is required")
	}
	allowedScopes := make(map[SessionScope]struct{}, len(scopes))
	for _, scope := range scopes {
		if !validSessionScope(scope) {
			return SessionState{}, fmt.Errorf("invalid OUC session scope")
		}
		allowedScopes[scope] = struct{}{}
	}
	now := j.now()
	j.mu.Lock()
	records := make([]trackedCookieRecord, 0, len(j.records))
	for key, record := range j.records {
		if !record.expires.IsZero() && !record.expires.After(now) {
			delete(j.records, key)
			continue
		}
		included := false
		for scope := range allowedScopes {
			if !scopeAllowsHost(scope, record.sourceHost) {
				continue
			}
			// Persistent target snapshots must never retain a cookie scoped
			// to a parent domain: such a cookie would also be sent to SSO
			// after the shorter identity TTL expired. Recovery snapshots are
			// ephemeral and retain the original Domain and source provenance.
			if !allowTargetParentDomain &&
				scope != SessionScopeIdentity &&
				record.domain != record.sourceHost {
				continue
			}
			included = true
			break
		}
		if !included {
			continue
		}
		records = append(records, record)
	}
	j.mu.Unlock()
	if len(records) == 0 {
		return SessionState{}, fmt.Errorf("OUC scoped session contains no authority-owned cookies")
	}
	sort.Slice(records, func(i, k int) bool {
		if records[i].sourceURL != records[k].sourceURL {
			return records[i].sourceURL < records[k].sourceURL
		}
		if records[i].name != records[k].name {
			return records[i].name < records[k].name
		}
		return records[i].path < records[k].path
	})
	state := SessionState{Version: 1}
	for _, record := range records {
		last := len(state.Sets) - 1
		if last < 0 || state.Sets[last].URL != record.sourceURL {
			state.Sets = append(state.Sets, SessionCookieSet{URL: record.sourceURL})
			last++
		}
		state.Sets[last].Cookies = append(state.Sets[last].Cookies, SessionCookie{
			Name:            record.name,
			Value:           record.value,
			Domain:          record.cookieDomain,
			Path:            record.path,
			ExpiresAtUnixMS: unixMilliOrZero(record.expires),
		})
	}
	return state, nil
}

func unixMilliOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func defaultCookiePath(escapedPath string) string {
	if escapedPath == "" || escapedPath[0] != '/' {
		return "/"
	}
	lastSlash := strings.LastIndex(escapedPath, "/")
	if lastSlash <= 0 {
		return "/"
	}
	return escapedPath[:lastSlash]
}

var _ http.CookieJar = (*trackedCookieJar)(nil)
