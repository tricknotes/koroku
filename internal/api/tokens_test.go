package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

// syncBuffer collects log output written from handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type tokenFixture struct {
	srv     *Server
	h       http.Handler
	clk     *clock.Fake
	owner   *browser
	logs    *syncBuffer
	created createdToken
}

type createdToken struct {
	Token apiTokenJSON `json:"token"`
	Value string       `json:"value"`
}

var tokenPattern = regexp.MustCompile(`^koroku_ro_[A-Za-z0-9_-]{43}$`)

// newTokenFixture logs in, creates the app "blog" and issues one token.
func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	logs := &syncBuffer{}
	clk := clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	srv := newTestServer(t, func(o *Options) {
		o.Clock = clk
		o.Auth = AuthOptions{AllowedUserID: 42, ConsoleURL: consoleURL, GitHub: defaultGitHub()}
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h := srv.Handler()
	b := newBrowser(t, h)
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}
	if rec := b.do(http.MethodPost, "/api/apps", `{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog"}`, origin); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d %s", rec.Code, rec.Body)
	}
	f := &tokenFixture{srv: srv, h: h, clk: clk, owner: b, logs: logs}
	f.created = f.issue(t, `{"name":"laptop","expires_in_days":30}`)
	return f
}

func (f *tokenFixture) issue(t *testing.T, body string) createdToken {
	t.Helper()
	rec := f.owner.do(http.MethodPost, "/api/tokens", body, map[string]string{"Origin": consoleURL})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: %d %s", rec.Code, rec.Body)
	}
	var out createdToken
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// withToken sends a request carrying only the bearer token (no cookies).
func (f *tokenFixture) withToken(method, target, token string, headers map[string]string) *httptest.ResponseRecorder {
	req := localRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return send(f.h, req)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func TestAPITokenIssueListRevoke(t *testing.T) {
	f := newTokenFixture(t)
	created := f.created
	if !tokenPattern.MatchString(created.Value) {
		t.Errorf("token value %q does not match the documented pattern", created.Value)
	}
	want := f.clk.Now().Add(30 * 24 * time.Hour)
	if created.Token.Name != "laptop" || !created.Token.ExpiresAt.Equal(want) || created.Token.LastUsedAt != nil {
		t.Errorf("token = %+v", created.Token)
	}

	// Only the hash is stored.
	stored, err := f.srv.store.ListAPITokens(context.Background(), f.clk.Now())
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored tokens: %v %v", stored, err)
	}
	if stored[0].TokenHash != hashToken(created.Value) || strings.Contains(stored[0].TokenHash, created.Value) {
		t.Errorf("stored hash = %q", stored[0].TokenHash)
	}

	// The list never shows the value.
	list := f.owner.do(http.MethodGet, "/api/tokens", "", nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), created.Value) || strings.Contains(list.Body.String(), "value") {
		t.Errorf("list: %d %s", list.Code, list.Body)
	}
	var listed struct {
		Tokens []apiTokenJSON `json:"tokens"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &listed)
	if len(listed.Tokens) != 1 || listed.Tokens[0].ID != created.Token.ID {
		t.Errorf("tokens = %+v", listed.Tokens)
	}

	origin := map[string]string{"Origin": consoleURL}
	for _, body := range []string{
		`{"name":"","expires_in_days":30}`,
		`{"name":"x","expires_in_days":365}`,
		`{"name":"x","expires_in_days":0}`,
		`{"name":"` + strings.Repeat("x", 101) + `","expires_in_days":7}`,
		`{"name":"a\u0007b","expires_in_days":7}`,
	} {
		if rec := f.owner.do(http.MethodPost, "/api/tokens", body, origin); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}

	// Issuing needs a recent login; revoking does not.
	f.clk.Advance(ReauthWindow + time.Minute)
	if rec := f.owner.do(http.MethodPost, "/api/tokens", `{"name":"late","expires_in_days":7}`, origin); rec.Code != http.StatusForbidden || errorCode(t, rec) != "reauth_required" {
		t.Errorf("create after the reauth window: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/tokens/"+created.Token.ID, "", origin); rec.Code != http.StatusNoContent {
		t.Errorf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/tokens/"+created.Token.ID, "", origin); rec.Code != http.StatusNotFound || errorCode(t, rec) != "token_not_found" {
		t.Errorf("revoke twice: %d %s", rec.Code, rec.Body)
	}
	if rec := f.withToken(http.MethodGet, "/api/apps", created.Value, nil); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_token" {
		t.Errorf("revoked token: %d %s", rec.Code, rec.Body)
	}
}

func TestAPITokenLimit(t *testing.T) {
	f := newTokenFixture(t)
	for i := 1; i < maxAPITokens; i++ {
		f.issue(t, `{"name":"t","expires_in_days":7}`)
	}
	rec := f.owner.do(http.MethodPost, "/api/tokens", `{"name":"one more","expires_in_days":7}`, map[string]string{"Origin": consoleURL})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "too_many_tokens" {
		t.Errorf("over the limit: %d %s", rec.Code, rec.Body)
	}
}

func TestAPITokenAllowlist(t *testing.T) {
	f := newTokenFixture(t)
	token := f.created.Value
	// Something to read: a deployment of blog.
	if rec := f.owner.do(http.MethodPost, "/api/apps/blog/deployments", `{"ref":"main"}`, map[string]string{"Origin": consoleURL}); rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}

	allowed := []string{
		"/api/system",
		"/api/apps",
		"/api/apps/blog",
		"/api/apps/blog/services",
		"/api/apps/blog/deployments",
		"/api/deployments/1",
		"/api/deployments/1/logs?format=text",
		"/api/apps/blog/logs/history",
		"/api/apps/blog/env/requirements",
	}
	for _, target := range allowed {
		rec := f.withToken(http.MethodGet, target, token, nil)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("GET %s with a token: %d %s", target, rec.Code, rec.Body)
		}
	}
	// The ones with a fixed answer in this setup.
	for _, target := range []string{"/api/system", "/api/apps", "/api/apps/blog", "/api/apps/blog/deployments", "/api/deployments/1", "/api/apps/blog/env/requirements"} {
		if rec := f.withToken(http.MethodGet, target, token, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s with a token: %d %s", target, rec.Code, rec.Body)
		}
	}

	denied := []struct{ method, target string }{
		// Secrets and data exports stay cookie-only.
		{http.MethodGet, "/api/apps/blog/env"},
		{http.MethodGet, "/api/apps/blog/backups/4bba301e/download"},
		{http.MethodGet, "/api/apps/blog/logs/archives/url?key=logs/blog/web/2026/11/18/04.log.gz"},
		{http.MethodGet, "/api/apps/blog/logs/archives"},
		{http.MethodGet, "/api/apps/blog/backups"},
		{http.MethodGet, "/api/backups"},
		{http.MethodGet, "/api/sessions"},
		{http.MethodGet, "/api/login-events"},
		{http.MethodGet, "/api/tokens"},
		{http.MethodGet, "/api/repos"},
		{http.MethodGet, "/api/system/docker"},
		{http.MethodGet, "/api/session"},
		{http.MethodGet, "/api/nope"},
		{http.MethodHead, "/api/apps"},
		// Nothing that changes state.
		{http.MethodPost, "/api/apps"},
		{http.MethodPatch, "/api/apps/blog"},
		{http.MethodDelete, "/api/apps/blog"},
		{http.MethodPost, "/api/apps/blog/deployments"},
		{http.MethodPost, "/api/apps/blog/stop"},
		{http.MethodPut, "/api/apps/blog/env/SECRET"},
		{http.MethodPost, "/api/tokens"},
		{http.MethodDelete, "/api/tokens/" + f.created.Token.ID},
		{http.MethodPost, "/api/system/prune"},
	}
	for _, tc := range denied {
		rec := f.withToken(tc.method, tc.target, token, nil)
		if rec.Code != http.StatusForbidden || (tc.method != http.MethodHead && errorCode(t, rec) != "token_not_allowed") {
			t.Errorf("%s %s with a token: %d %s", tc.method, tc.target, rec.Code, rec.Body)
		}
	}
	// The denied mutations did not happen.
	if rec := f.withToken(http.MethodGet, "/api/apps/blog", token, nil); rec.Code != http.StatusOK {
		t.Errorf("blog after denied requests: %d", rec.Code)
	}
}

// TestAPITokenRoutesAreRegistered keeps the token list in step with the
// routing: a pattern that no longer matches a registered route would
// silently close (or, after a rename, never open) an endpoint.
func TestAPITokenRoutesAreRegistered(t *testing.T) {
	mux := newTestServer(t, nil).routes()
	for pattern := range tokenRoutes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || method != http.MethodGet {
			t.Errorf("token route %q is not a GET pattern", pattern)
			continue
		}
		path = strings.NewReplacer("{slug}", "blog", "{id}", "1").Replace(path)
		if _, got := mux.Handler(localRequest(http.MethodGet, path, nil)); got != pattern {
			t.Errorf("GET %s is routed to %q, not the token route %q", path, got, pattern)
		}
	}
}

func TestAPITokenRejected(t *testing.T) {
	f := newTokenFixture(t)
	token := f.created.Value
	for name, header := range map[string]string{
		"unknown":      "Bearer " + APITokenPrefix + strings.Repeat("A", 43),
		"no prefix":    "Bearer " + strings.TrimPrefix(token, APITokenPrefix),
		"prefix only":  "Bearer " + APITokenPrefix,
		"empty":        "Bearer ",
		"basic scheme": "Basic " + token,
		"bare token":   token,
	} {
		req := localRequest(http.MethodGet, "/api/apps", nil)
		req.Header.Set("Authorization", header)
		rec := send(f.h, req)
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_token" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
			t.Errorf("%s: WWW-Authenticate = %q", name, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// Two Authorization headers are ambiguous.
	req := localRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Authorization", "Bearer "+token)
	if rec := send(f.h, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("two headers: %d", rec.Code)
	}
	// A lower-case scheme is fine (RFC 9110 schemes are case-insensitive).
	req = localRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Set("Authorization", "bearer "+token)
	if rec := send(f.h, req); rec.Code != http.StatusOK {
		t.Errorf("lower-case scheme: %d %s", rec.Code, rec.Body)
	}

	// Never from the query string or a cookie.
	for _, target := range []string{"/api/apps?access_token=" + token, "/api/apps?token=" + token} {
		if rec := send(f.h, localRequest(http.MethodGet, target, nil)); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthenticated" {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
	req = localRequest(http.MethodGet, "/api/apps", nil)
	req.AddCookie(&http.Cookie{Name: secureSessionCookie, Value: token})
	if rec := send(f.h, req); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthenticated" {
		t.Errorf("token as cookie: %d %s", rec.Code, rec.Body)
	}

	// Expiry: valid until the moment it expires.
	f.clk.Advance(30*24*time.Hour - time.Second)
	if rec := f.withToken(http.MethodGet, "/api/apps", token, nil); rec.Code != http.StatusOK {
		t.Errorf("just before expiry: %d %s", rec.Code, rec.Body)
	}
	f.clk.Advance(time.Second)
	if rec := f.withToken(http.MethodGet, "/api/apps", token, nil); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_token" {
		t.Errorf("expired: %d %s", rec.Code, rec.Body)
	}
}

func TestAPITokenIgnoresCookies(t *testing.T) {
	f := newTokenFixture(t)
	origin := map[string]string{"Origin": consoleURL}
	// A logged-in browser that also sends a bad token gets no cookie powers.
	if rec := f.owner.do(http.MethodGet, "/api/apps", "", map[string]string{"Authorization": "Bearer koroku_ro_nope"}); rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_token" {
		t.Errorf("session + bad token: %d %s", rec.Code, rec.Body)
	}
	// With a valid token, cookie-only routes stay closed even though the
	// session alone could use them.
	auth := map[string]string{"Authorization": "Bearer " + f.created.Value}
	if rec := f.owner.do(http.MethodGet, "/api/apps/blog/env", "", auth); rec.Code != http.StatusForbidden || errorCode(t, rec) != "token_not_allowed" {
		t.Errorf("session + token on env: %d %s", rec.Code, rec.Body)
	}
	auth["Origin"] = consoleURL
	if rec := f.owner.do(http.MethodDelete, "/api/apps/blog", "", auth); rec.Code != http.StatusForbidden {
		t.Errorf("session + token delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodGet, "/api/apps/blog", "", origin); rec.Code != http.StatusOK {
		t.Errorf("the app must still exist: %d", rec.Code)
	}
}

func TestAPITokenKeepsBrowserChecks(t *testing.T) {
	f := newTokenFixture(t)
	rec := f.withToken(http.MethodGet, "/api/apps", f.created.Value, map[string]string{"Sec-Fetch-Site": "same-site"})
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_origin" {
		t.Errorf("cross-site token request: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("security headers are missing on token requests")
	}
}

func TestAPITokenRateLimitAndLastUsed(t *testing.T) {
	f := newTokenFixture(t)
	token := f.created.Value
	lastUsed := func() *time.Time {
		list, err := f.srv.store.ListAPITokens(context.Background(), f.clk.Now())
		if err != nil || len(list) != 1 {
			t.Fatalf("tokens: %v %v", list, err)
		}
		return list[0].LastUsedAt
	}

	first := f.clk.Now()
	for i := range tokenRequestsPerMinute {
		if rec := f.withToken(http.MethodGet, "/api/system", token, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
		if i == 0 {
			f.clk.Advance(10 * time.Second)
		}
	}
	// Recorded once, not on every request.
	if got := lastUsed(); got == nil || !got.Equal(first) {
		t.Errorf("last used = %v, want %v", got, first)
	}
	rec := f.withToken(http.MethodGet, "/api/system", token, nil)
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("over the limit: %d %s", rec.Code, rec.Body)
	}
	// Another token has its own budget.
	other := f.issue(t, `{"name":"desk","expires_in_days":7}`)
	if rec := f.withToken(http.MethodGet, "/api/system", other.Value, nil); rec.Code != http.StatusOK {
		t.Errorf("other token: %d", rec.Code)
	}

	f.clk.Advance(time.Minute)
	if rec := f.withToken(http.MethodGet, "/api/system", token, nil); rec.Code != http.StatusOK {
		t.Errorf("after a minute: %d", rec.Code)
	}
	list, _ := f.srv.store.ListAPITokens(context.Background(), f.clk.Now())
	for _, tok := range list {
		if tok.ID == f.created.Token.ID && (tok.LastUsedAt == nil || !tok.LastUsedAt.Equal(f.clk.Now()) || tok.LastUsedIP != "192.0.2.1") {
			t.Errorf("last use = %v %q", tok.LastUsedAt, tok.LastUsedIP)
		}
	}
}

func TestAPITokenNeverLogged(t *testing.T) {
	f := newTokenFixture(t)
	token := f.created.Value
	f.withToken(http.MethodGet, "/api/apps", token, nil)
	f.withToken(http.MethodGet, "/api/apps/blog/env", token, nil)
	f.withToken(http.MethodPost, "/api/apps", token, nil)
	f.withToken(http.MethodGet, "/api/apps", token+"x", nil)
	f.withToken(http.MethodGet, "/api/deployments/999/logs?format=text", token, nil)
	f.owner.do(http.MethodDelete, "/api/tokens/"+f.created.Token.ID, "", map[string]string{"Origin": consoleURL})
	f.withToken(http.MethodGet, "/api/apps", token, nil)

	logs := f.logs.String()
	secret := strings.TrimPrefix(token, APITokenPrefix)
	if strings.Contains(logs, secret) || strings.Contains(logs, hashToken(token)) {
		t.Errorf("the token or its hash reached the log:\n%s", logs)
	}
	if !strings.Contains(logs, "token_id="+f.created.Token.ID) {
		t.Errorf("token requests are not attributed in the log:\n%s", logs)
	}
}

func TestAPITokenInNoAuthMode(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	rec := request(t, h, http.MethodPost, "/api/tokens", `{"name":"dev","expires_in_days":7}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created createdToken
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	req := localRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Set("Authorization", "Bearer "+created.Value)
	if rec := send(h, req); rec.Code != http.StatusOK {
		t.Errorf("token on loopback: %d %s", rec.Code, rec.Body)
	}
	// The token list still applies without login.
	req = localRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+created.Value)
	if rec := send(h, req); rec.Code != http.StatusForbidden {
		t.Errorf("cookie-only route: %d %s", rec.Code, rec.Body)
	}
	req = localRequest(http.MethodGet, "/api/apps", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+created.Value)
	if rec := send(h, req); rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_host" {
		t.Errorf("non-loopback host: %d %s", rec.Code, rec.Body)
	}
}
