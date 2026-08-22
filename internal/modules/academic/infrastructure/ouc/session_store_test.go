package ouc

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/weouc-plus/campus-academic/internal/core/configcenter"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
)

func TestEncryptedRedisSessionStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Redis client: %v", err)
		}
	})
	key := bytes.Repeat([]byte{7}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	config := testOUCConfig()
	current, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse(config.Undergraduate.ServiceURL)
	if err != nil {
		t.Fatal(err)
	}
	current.Jar.SetCookies(origin, []*http.Cookie{{
		Name:   "OUC_SESSION",
		Value:  "ticket-secret",
		Secure: true,
	}})
	state, err := snapshotSession(&session{client: current}, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(ctx, "20260001", "local-password", state, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	keys := server.Keys()
	if len(keys) != 2 {
		t.Fatalf("Redis keys=%v", keys)
	}
	var sessionKey string
	for _, storedKey := range keys {
		if strings.Contains(storedKey, "20260001") ||
			strings.Contains(storedKey, "local-password") {
			t.Fatalf("Redis key contains credentials: %q", storedKey)
		}
		if strings.HasPrefix(storedKey, sessionKeyPrefix) {
			sessionKey = storedKey
		}
	}
	if sessionKey == "" {
		t.Fatalf("session key missing from %v", keys)
	}
	encoded, err := server.Get(sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "ticket-secret") {
		t.Fatal("Redis value contains plaintext cookie")
	}
	loaded, found, err := store.Load(ctx, "20260001", "local-password")
	if err != nil || !found {
		t.Fatalf("Load() found=%v err=%v", found, err)
	}
	restored, err := restoreSession(config, loaded, newSessionClient)
	if err != nil {
		t.Fatal(err)
	}
	cookies := restored.client.Jar.Cookies(origin)
	if len(cookies) != 1 ||
		cookies[0].Name != "OUC_SESSION" ||
		cookies[0].Value != "ticket-secret" {
		t.Fatalf("restored cookies=%+v", cookies)
	}
	if _, found, err = store.Load(ctx, "20260001", "different-password"); err != nil || found {
		t.Fatalf("wrong-password Load() found=%v err=%v", found, err)
	}
	if err = store.DeleteStudent(ctx, "20260001"); err != nil {
		t.Fatal(err)
	}
	if _, found, err = store.Load(ctx, "20260001", "local-password"); err != nil || found {
		t.Fatalf("deleted student Load() found=%v err=%v", found, err)
	}
	server.FastForward(16 * time.Minute)
	if _, found, err = store.Load(ctx, "20260001", "local-password"); err != nil || found {
		t.Fatalf("expired Load() found=%v err=%v", found, err)
	}
}

func TestEncryptedRedisSessionStoreClassifiesCorruptScopedAndLegacyRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{11}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	studentNo, password := "20260005", "corrupt-cache"

	if err = client.Set(ctx, store.scopedCacheKey(studentNo, password, SessionScopeUndergraduate), "corrupt", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); !errors.Is(err, errInvalidCachedSession) {
		t.Fatalf("scoped corrupt error=%v", err)
	}
	if err = client.Del(ctx, store.scopedCacheKey(studentNo, password, SessionScopeUndergraduate)).Err(); err != nil {
		t.Fatal(err)
	}
	if err = client.Set(ctx, store.cacheKey(studentNo, password), "corrupt", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); !errors.Is(err, errInvalidCachedSession) {
		t.Fatalf("legacy corrupt error=%v", err)
	}
}

func TestEncryptedRedisSessionStoreSeparatesScopesAndPreservesStudentIndexTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{8}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	studentNo, password := "20260002", "scope-password"
	identity := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://id.ouc.edu.cn/sso/login", Cookies: []SessionCookie{{Name: "SSO", Value: "identity"}},
	}}}
	undergraduate := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://jwgl2024.ouc.edu.cn/", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "undergraduate"}},
	}}}
	if err = store.SaveScope(ctx, studentNo, password, SessionScopeUndergraduate, undergraduate, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveScope(ctx, studentNo, password, SessionScopeIdentity, identity, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadScope(ctx, studentNo, password, SessionScopeGraduate); err != nil || found {
		t.Fatalf("graduate scope found=%v err=%v", found, err)
	}
	loaded, found, err := store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate)
	if err != nil || !found || loaded.Sets[0].Cookies[0].Value != "undergraduate" {
		t.Fatalf("undergraduate scope=%+v found=%v err=%v", loaded, found, err)
	}
	indexKey := store.studentIndexKey(studentNo)
	if ttl := server.TTL(indexKey); ttl < 59*time.Minute {
		t.Fatalf("student index TTL=%s, want target session lifetime", ttl)
	}
	if err = store.DeleteScope(ctx, studentNo, password, SessionScopeIdentity); err != nil {
		t.Fatal(err)
	}
	if _, found, err = store.LoadScope(ctx, studentNo, password, SessionScopeIdentity); err != nil || found {
		t.Fatalf("identity scope found=%v err=%v", found, err)
	}
	if _, found, err = store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); err != nil || !found {
		t.Fatalf("undergraduate scope found=%v err=%v", found, err)
	}
}

func TestEncryptedRedisSessionStoreReadsLegacyStateByScopeUntilNewSave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{9}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := SessionState{Version: 1, Sets: []SessionCookieSet{
		{URL: "https://id.ouc.edu.cn/sso/login", Cookies: []SessionCookie{{Name: "SSO", Value: "identity"}}},
		{URL: "https://jwgl2024.ouc.edu.cn/", Cookies: []SessionCookie{
			{Name: "SSO", Value: "identity"},
			{Name: "JSESSIONID", Value: "undergraduate"},
		}},
	}}
	if err = store.Save(ctx, "20260003", "legacy-password", legacy, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	state, found, err := store.LoadScope(ctx, "20260003", "legacy-password", SessionScopeUndergraduate)
	if err != nil || !found || len(state.Sets) != 1 || state.Sets[0].URL != "https://jwgl2024.ouc.edu.cn/" ||
		len(state.Sets[0].Cookies) != 1 || state.Sets[0].Cookies[0].Name != "JSESSIONID" {
		t.Fatalf("legacy undergraduate state=%+v found=%v err=%v", state, found, err)
	}
	if _, found, err = store.LoadScope(ctx, "20260003", "legacy-password", SessionScopeGraduate); err != nil || found {
		t.Fatalf("legacy absent scope found=%v err=%v", found, err)
	}
	if err = store.SaveScope(ctx, "20260003", "legacy-password", SessionScopeUndergraduate, state, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Get(store.scopedCacheKey("20260003", "legacy-password", SessionScopeUndergraduate)); err != nil {
		t.Fatalf("successful scoped save did not migrate legacy state: %v", err)
	}
}

func TestEncryptedRedisSessionStoreDeleteScopeStopsLegacyFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{10}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	studentNo, password := "20260004", "legacy-rejected"
	legacy := SessionState{Version: 1, Sets: []SessionCookieSet{
		{URL: "https://id.ouc.edu.cn/sso/login", Cookies: []SessionCookie{{Name: "SSO", Value: "identity"}}},
		{URL: "https://jwgl2024.ouc.edu.cn/", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "expired"}}},
	}}
	if err = store.Save(ctx, studentNo, password, legacy, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, found, loadErr := store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); loadErr != nil || !found {
		t.Fatalf("legacy target found=%v err=%v", found, loadErr)
	}
	if err = store.DeleteScope(ctx, studentNo, password, SessionScopeUndergraduate); err != nil {
		t.Fatal(err)
	}
	if _, found, loadErr := store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); loadErr != nil || found {
		t.Fatalf("rejected legacy target found=%v err=%v", found, loadErr)
	}
}

func TestEncryptedRedisSessionStoreMigratesHistoricalCatalogSubjectsByScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{13}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	studentNo, password := "20260007", "historical-catalog"
	undergraduate := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://jwgl2024.ouc.edu.cn/", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "undergraduate-history"}},
	}}}
	graduate := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://pgs.ouc.edu.cn/allogene/page/home.htm", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "graduate-history"}},
	}}}
	if err = store.Save(ctx, "undergraduate\x00"+studentNo, password, undergraduate, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = store.Save(ctx, "graduate\x00"+studentNo, password, graduate, 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	// With no ordinary v1 record, LoadScope must find the historical
	// undergraduate-prefixed subject and must not cross into graduate state.
	loaded, found, err := store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate)
	if err != nil || !found || len(loaded.Sets) != 1 ||
		loaded.Sets[0].Cookies[0].Value != "undergraduate-history" {
		t.Fatalf("historical undergraduate state=%+v found=%v err=%v", loaded, found, err)
	}

	// An ordinary v1 record that filters to no graduate cookies must not hide
	// the older per-level course-catalog record.
	identity := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://id.ouc.edu.cn/sso/login", Cookies: []SessionCookie{{Name: "SSO", Value: "identity"}},
	}}}
	if err = store.Save(ctx, studentNo, password, identity, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	loaded, found, err = store.LoadScope(ctx, studentNo, password, SessionScopeGraduate)
	if err != nil || !found || len(loaded.Sets) != 1 ||
		loaded.Sets[0].Cookies[0].Value != "graduate-history" {
		t.Fatalf("historical graduate state=%+v found=%v err=%v", loaded, found, err)
	}
	loaded, found, err = store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate)
	if err != nil || !found || loaded.Sets[0].Cookies[0].Value != "undergraduate-history" {
		t.Fatalf("undergraduate isolation state=%+v found=%v err=%v", loaded, found, err)
	}
}

func TestEncryptedRedisSessionStoreDeletesHistoricalCatalogSubjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{14}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	studentNo, password := "20260008", "historical-delete"
	undergraduateSubject := "undergraduate\x00" + studentNo
	graduateSubject := "graduate\x00" + studentNo
	undergraduate := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://jwgl2024.ouc.edu.cn/", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "undergraduate-history"}},
	}}}
	graduate := SessionState{Version: 1, Sets: []SessionCookieSet{{
		URL: "https://pgs.ouc.edu.cn/allogene/page/home.htm", Cookies: []SessionCookie{{Name: "JSESSIONID", Value: "graduate-history"}},
	}}}
	if err = store.Save(ctx, undergraduateSubject, password, undergraduate, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = store.Save(ctx, graduateSubject, password, graduate, 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	if err = store.DeleteScope(ctx, studentNo, password, SessionScopeUndergraduate); err != nil {
		t.Fatal(err)
	}
	if _, found, loadErr := store.LoadScope(ctx, studentNo, password, SessionScopeUndergraduate); loadErr != nil || found {
		t.Fatalf("deleted historical undergraduate found=%v err=%v", found, loadErr)
	}
	if _, found, loadErr := store.LoadScope(ctx, studentNo, password, SessionScopeGraduate); loadErr != nil || !found {
		t.Fatalf("graduate removed by undergraduate scope delete found=%v err=%v", found, loadErr)
	}
	if _, getErr := server.Get(store.cacheKey(undergraduateSubject, password)); getErr == nil {
		t.Fatal("historical undergraduate key survived scoped delete")
	}

	if err = store.DeleteStudent(ctx, studentNo); err != nil {
		t.Fatal(err)
	}
	if _, found, loadErr := store.LoadScope(ctx, studentNo, password, SessionScopeGraduate); loadErr != nil || found {
		t.Fatalf("historical graduate survived student delete found=%v err=%v", found, loadErr)
	}
	if _, getErr := server.Get(store.cacheKey(graduateSubject, password)); getErr == nil {
		t.Fatal("historical graduate key survived student delete")
	}
}

func TestScopedSnapshotDoesNotExtendParentDomainIdentityCookie(t *testing.T) {
	t.Parallel()
	config := testOUCConfig()
	client, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	identityURL, _ := url.Parse("https://id.ouc.edu.cn/sso/login")
	targetURL, _ := url.Parse("https://jwgl2024.ouc.edu.cn/")
	client.Jar.SetCookies(identityURL, []*http.Cookie{{
		Name: "OUC_SSO", Value: "identity", Domain: ".ouc.edu.cn", Path: "/", Secure: true,
	}})
	client.Jar.SetCookies(targetURL, []*http.Cookie{{
		Name: "JSESSIONID", Value: "undergraduate", Path: "/", Secure: true,
	}})
	if got := client.Jar.Cookies(targetURL); len(got) != 2 {
		t.Fatalf("test setup target cookies=%+v, want parent and target cookies", got)
	}

	current := &session{client: client}
	identity, err := snapshotSessionScope(current, config, SessionScopeIdentity, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := snapshotSessionScope(current, config, SessionScopeUndergraduate, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(identity.Sets) != 1 || len(identity.Sets[0].Cookies) != 1 || identity.Sets[0].Cookies[0].Name != "OUC_SSO" {
		t.Fatalf("identity snapshot=%+v", identity)
	}
	if len(target.Sets) != 1 || len(target.Sets[0].Cookies) != 1 || target.Sets[0].Cookies[0].Name != "JSESSIONID" {
		t.Fatalf("target snapshot=%+v", target)
	}
	restored, err := restoreSession(config, target, newSessionClient)
	if err != nil {
		t.Fatal(err)
	}
	if cookies := restored.client.Jar.Cookies(identityURL); len(cookies) != 0 {
		t.Fatalf("target snapshot leaked cookies to identity host: %+v", cookies)
	}
}

func TestRecoverySnapshotPreservesIdentityCookieProvenanceWithoutTargetCookie(t *testing.T) {
	t.Parallel()
	config := testOUCConfig()
	client, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	identityURL, _ := url.Parse("https://id.ouc.edu.cn/sso/login")
	targetURL, _ := url.Parse("https://jwgl2024.ouc.edu.cn/")
	client.Jar.SetCookies(identityURL, []*http.Cookie{{
		Name: "OUC_SSO", Value: "identity", Domain: ".ouc.edu.cn", Path: "/", Secure: true,
	}})

	state, err := snapshotSessionForRecovery(
		&session{client: client},
		SessionScopeUndergraduate,
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreSession(config, state, newSessionClient)
	if err != nil {
		t.Fatal(err)
	}
	cookies := restored.client.Jar.Cookies(targetURL)
	if len(cookies) != 1 || cookies[0].Name != "OUC_SSO" || cookies[0].Value != "identity" {
		t.Fatalf("recovery snapshot lost identity hand-off cookie: %+v", cookies)
	}
	if _, err = snapshotSessionScope(
		restored,
		config,
		SessionScopeUndergraduate,
		time.Now(),
	); err == nil {
		t.Fatal("identity parent-domain cookie became persistable as a target cookie")
	}
}

func TestScopedSessionStateRoundTripPreservesParentDomainAndHostOnlyCookies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	key := bytes.Repeat([]byte{12}, 32)
	cipher, err := configcenter.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncryptedRedisSessionStore(client, cipher, key)
	if err != nil {
		t.Fatal(err)
	}
	config := testOUCConfig()
	currentClient, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	identityURL, _ := url.Parse("https://id.ouc.edu.cn/sso/login")
	portalURL, _ := url.Parse("https://my.ouc.edu.cn/frontend/user/info")
	currentClient.Jar.SetCookies(identityURL, []*http.Cookie{
		{Name: "PARENT", Value: "sso", Domain: ".OUC.EDU.CN", Path: "/", Secure: true},
		{Name: "HOST_ONLY", Value: "id", Path: "/", Secure: true},
	})
	state, err := snapshotSessionScope(&session{client: currentClient}, config, SessionScopeIdentity, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveScope(ctx, "20260006", "domain-password", SessionScopeIdentity, state, time.Hour); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.LoadScope(ctx, "20260006", "domain-password", SessionScopeIdentity)
	if err != nil || !found {
		t.Fatalf("LoadScope() found=%v err=%v", found, err)
	}
	var parentDomain, hostOnlyDomain string
	for _, cookie := range loaded.Sets[0].Cookies {
		switch cookie.Name {
		case "PARENT":
			parentDomain = cookie.Domain
		case "HOST_ONLY":
			hostOnlyDomain = cookie.Domain
		}
	}
	if parentDomain != "ouc.edu.cn" || hostOnlyDomain != "" {
		t.Fatalf("persisted domains parent=%q host-only=%q", parentDomain, hostOnlyDomain)
	}
	restored, err := restoreSession(config, loaded, newSessionClient)
	if err != nil {
		t.Fatal(err)
	}
	portalCookies := restored.client.Jar.Cookies(portalURL)
	if len(portalCookies) != 1 || portalCookies[0].Name != "PARENT" || portalCookies[0].Value != "sso" {
		t.Fatalf("parent-domain cookie did not cross id/my after round-trip: %+v", portalCookies)
	}
	identityCookies := restored.client.Jar.Cookies(identityURL)
	if len(identityCookies) != 2 {
		t.Fatalf("restored identity cookies=%+v, want parent and host-only cookies", identityCookies)
	}
}

func TestCanonicalSessionCookieDomainNormalizesASCII(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		domain    string
		want      string
		wantValid bool
	}{
		{name: "host only", domain: "", want: "", wantValid: true},
		{name: "mixed case parent", domain: ".OUC.EDU.CN", want: "ouc.edu.cn", wantValid: true},
		{name: "mixed case host", domain: "JWGL2024.OUC.EDU.CN", want: "jwgl2024.ouc.edu.cn", wantValid: true},
		{name: "leading whitespace", domain: " .OUC.EDU.CN", wantValid: false},
		{name: "trailing whitespace", domain: ".OUC.EDU.CN ", wantValid: false},
		{name: "two leading dots", domain: "..ouc.edu.cn", wantValid: false},
		{name: "empty label", domain: "ouc..edu.cn", wantValid: false},
		{name: "trailing dot", domain: "ouc.edu.cn.", wantValid: false},
		{name: "underscore", domain: "ouc_edu.cn", wantValid: false},
		{name: "label edge hyphen", domain: "-ouc.edu.cn", wantValid: false},
		{name: "non ASCII", domain: "海大.edu.cn", wantValid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := canonicalSessionCookieDomain(test.domain)
			if got != test.want || valid != test.wantValid {
				t.Fatalf("canonicalSessionCookieDomain(%q)=(%q,%v), want (%q,%v)", test.domain, got, valid, test.want, test.wantValid)
			}
		})
	}
}

func TestTargetScopedSessionRejectsParentDomainCookie(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		scope SessionScope
		url   string
	}{
		{name: "undergraduate", scope: SessionScopeUndergraduate, url: "https://jwgl2024.ouc.edu.cn/"},
		{name: "graduate", scope: SessionScopeGraduate, url: "https://pgs.ouc.edu.cn/allogene/page/home.htm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			state := SessionState{Version: 1, Sets: []SessionCookieSet{{
				URL:     test.url,
				Cookies: []SessionCookie{{Name: "OUC_SSO", Value: "shared", Domain: "ouc.edu.cn"}},
			}}}
			if err := validateScopedSessionState(state, test.scope); err == nil {
				t.Fatal("target scoped session accepted a parent-domain cookie")
			}
		})
	}
}

func TestTrackedCookieJarUsesRFCSlotAcrossOUCSubdomains(t *testing.T) {
	t.Parallel()
	config := testOUCConfig()
	client, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	identityURL, _ := url.Parse("https://id.ouc.edu.cn/sso/login")
	targetURL, _ := url.Parse("https://jwgl2024.ouc.edu.cn/")
	client.Jar.SetCookies(identityURL, []*http.Cookie{
		{Name: "ID_HOST", Value: "identity-host", Path: "/", Secure: true},
		{Name: "SHARED", Value: "old", Domain: ".ouc.edu.cn", Path: "/", Secure: true},
	})
	client.Jar.SetCookies(targetURL, []*http.Cookie{
		{Name: "TARGET_HOST", Value: "target-host", Path: "/", Secure: true},
		{Name: "SHARED", Value: "new", Domain: ".ouc.edu.cn", Path: "/", Secure: true},
	})
	jar := client.Jar.(*trackedCookieJar)
	identity, err := jar.snapshotScope(SessionScopeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	target, err := jar.snapshotScope(SessionScopeUndergraduate)
	if err != nil {
		t.Fatal(err)
	}
	if got := identity.Sets[0].Cookies; len(got) != 1 || got[0].Name != "ID_HOST" {
		t.Fatalf("identity snapshot resurrected overwritten shared cookie: %+v", identity)
	}
	if got := target.Sets[0].Cookies; len(got) != 1 || got[0].Name != "TARGET_HOST" {
		t.Fatalf("target snapshot retained parent-domain cookie: %+v", target)
	}

	client.Jar.SetCookies(targetURL, []*http.Cookie{{
		Name: "SHARED", Value: "", Domain: ".ouc.edu.cn", Path: "/", Secure: true, MaxAge: -1,
	}})
	identity, err = jar.snapshotScope(SessionScopeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if got := identity.Sets[0].Cookies; len(got) != 1 || got[0].Name != "ID_HOST" {
		t.Fatalf("identity snapshot resurrected deleted shared cookie: %+v", identity)
	}
}

func TestTrackedCookieJarPersistsAbsoluteMaxAgeExpiry(t *testing.T) {
	t.Parallel()
	config := testOUCConfig()
	client, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	targetURL, _ := url.Parse("https://jwgl2024.ouc.edu.cn/jsxsd/framework/xsMainV.htmlx")
	jar := client.Jar.(*trackedCookieJar)
	baseTime := time.Now().Truncate(time.Millisecond)
	jar.now = func() time.Time { return baseTime }
	client.Jar.SetCookies(targetURL, []*http.Cookie{{
		Name: "SHORT", Value: "value", Path: "/", Secure: true, MaxAge: 2,
	}})

	state, err := jar.snapshotScope(SessionScopeUndergraduate)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := baseTime.Add(2 * time.Second).UnixMilli()
	if got := state.Sets[0].Cookies[0].ExpiresAtUnixMS; got != wantExpiry {
		t.Fatalf("persisted expiry=%d, want %d", got, wantExpiry)
	}

	jar.now = func() time.Time { return baseTime.Add(3 * time.Second) }
	if _, err = jar.snapshotScope(SessionScopeUndergraduate); err == nil {
		t.Fatal("expired Max-Age cookie was included in scoped snapshot")
	}
	state.Sets[0].Cookies[0].ExpiresAtUnixMS = time.Now().Add(-time.Second).UnixMilli()
	if _, err = restoreSession(config, state, newSessionClient); err == nil {
		t.Fatal("expired persisted cookie was restored")
	}
}

func TestTrackedCookieJarUsesStandardDefaultPath(t *testing.T) {
	t.Parallel()
	config := testOUCConfig()
	client, err := newSessionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	sourceURL, _ := url.Parse("https://jwgl2024.ouc.edu.cn/a/b%2Fc")
	client.Jar.SetCookies(sourceURL, []*http.Cookie{
		{Name: "STABLE", Value: "stable", Path: "/", Secure: true},
		{Name: "RELATIVE", Value: "temporary", Path: "relative", Secure: true},
	})
	jar := client.Jar.(*trackedCookieJar)
	state, err := jar.snapshotScope(SessionScopeUndergraduate)
	if err != nil {
		t.Fatal(err)
	}
	got := state.Sets[0].Cookies
	if len(got) != 2 {
		t.Fatalf("default-path snapshot=%+v", state)
	}
	var relativePath string
	for _, cookie := range got {
		if cookie.Name == "RELATIVE" {
			relativePath = cookie.Path
		}
	}
	if relativePath != "/a/b" {
		t.Fatalf("relative cookie path=%q, want /a/b", relativePath)
	}

	client.Jar.SetCookies(sourceURL, []*http.Cookie{{
		Name: "RELATIVE", Value: "", Path: "/a/b", Secure: true, MaxAge: -1,
	}})
	state, err = jar.snapshotScope(SessionScopeUndergraduate)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Sets[0].Cookies; len(got) != 1 || got[0].Name != "STABLE" {
		t.Fatalf("relative-path cookie was resurrected: %+v", state)
	}
}

func TestSessionStateRejectsUnsafeContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state SessionState
	}{
		{
			name: "unknown version",
			state: SessionState{
				Version: 2,
				Sets: []SessionCookieSet{{
					URL: "https://id.ouc.edu.cn/sso/login",
					Cookies: []SessionCookie{{
						Name: "session", Value: "value",
					}},
				}},
			},
		},
		{
			name: "untrusted host",
			state: SessionState{
				Version: 1,
				Sets: []SessionCookieSet{{
					URL: "https://attacker.example/session",
					Cookies: []SessionCookie{{
						Name: "session", Value: "value",
					}},
				}},
			},
		},
		{
			name: "empty cookie name",
			state: SessionState{
				Version: 1,
				Sets: []SessionCookieSet{{
					URL: "https://id.ouc.edu.cn/sso/login",
					Cookies: []SessionCookie{{
						Name: "", Value: "value",
					}},
				}},
			},
		},
		{
			name: "cookie domain outside OUC boundary",
			state: SessionState{
				Version: 1,
				Sets: []SessionCookieSet{{
					URL: "https://id.ouc.edu.cn/sso/login",
					Cookies: []SessionCookie{{
						Name: "session", Value: "value", Domain: "attacker.example",
					}},
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateSessionState(test.state); err == nil {
				t.Fatal("unsafe session state was accepted")
			}
		})
	}
}

func testOUCConfig() academicconfig.OUCConfig {
	return academicconfig.OUCConfig{
		SSOLoginURL:       "https://id.ouc.edu.cn/sso/login",
		PortalServiceURL:  "https://my.ouc.edu.cn/manage/common/cas_login/2?redirect=https%3A%2F%2Fmy.ouc.edu.cn%2Ffrontend%2Fuser%2Finfo",
		PortalNoRedirect:  true,
		RequestTimeoutMS:  1000,
		SessionTTLSeconds: 900,
		Undergraduate: academicconfig.EndpointSet{
			ServiceURL: "https://jwgl2024.ouc.edu.cn/",
			Periods: academicconfig.OperationEndpoint{
				Path: "/api/periods",
			},
			Courses: academicconfig.OperationEndpoint{
				Path: "/api/courses",
			},
			Grades: academicconfig.OperationEndpoint{
				Path: "/api/grades",
			},
			Exams: academicconfig.OperationEndpoint{
				Path: "/api/exams",
			},
			Selections: academicconfig.OperationEndpoint{
				Path: "/api/selections",
			},
		},
		Graduate: academicconfig.EndpointSet{
			ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
			Periods: academicconfig.OperationEndpoint{
				Path: "/allogene/api/periods",
			},
			Courses: academicconfig.OperationEndpoint{
				Path: "/allogene/api/courses",
			},
			Grades: academicconfig.OperationEndpoint{
				Path: "/allogene/api/grades",
			},
			Exams: academicconfig.OperationEndpoint{
				Path: "/allogene/api/exams",
			},
			Selections: academicconfig.OperationEndpoint{
				Path: "/allogene/api/selections",
			},
		},
	}
}
