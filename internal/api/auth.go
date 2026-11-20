package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

const (
	// Cookie names. Over HTTPS they carry the __Host- prefix (see cookieName).
	sessionCookie = "koroku_session"
	stateCookie   = "koroku_oauth_state"
	returnCookie  = "koroku_oauth_return"
	// SessionLifetime is how long a login lasts. The console has root-level
	// power over the host, so sessions are short and do not slide.
	SessionLifetime = 7 * 24 * time.Hour
	stateLifetime   = 10 * time.Minute
	// ReauthWindow is how long after a login dangerous operations are
	// allowed without logging in again (ARCHITECTURE 10.3).
	ReauthWindow = 10 * time.Minute
	// touchInterval throttles last-seen updates: the console polls every
	// few seconds and each update is a database write.
	touchInterval = time.Minute
	// maxUserAgent bounds what is stored per session and login event.
	maxUserAgent = 512
)

// GitHub is the part of the GitHub App client the API uses.
type GitHub interface {
	AuthorizeURL(state, redirectURI string) string
	ExchangeCode(ctx context.Context, code, redirectURI string) (string, error)
	User(ctx context.Context, userToken string) (github.User, error)
	ListRepos(ctx context.Context) ([]github.Repo, error)
	ListBranches(ctx context.Context, owner, repo string) ([]github.Branch, error)
	ListCommits(ctx context.Context, owner, repo, ref string, limit int) ([]github.Commit, error)
	ListDir(ctx context.Context, owner, repo, dir, ref string) ([]string, error)
	ReadFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// AuthOptions configure login and request protection.
type AuthOptions struct {
	// GitHub performs the login flow. Nil means login is not configured;
	// the API then stays locked unless InsecureNoAuth is set.
	GitHub GitHub
	// AllowedUserID is the owner: the GitHub user who may do everything
	// and manages the members (ARCHITECTURE 10.7).
	AllowedUserID int64
	// ConsoleURL is the public origin of the console, e.g.
	// https://console.dev.example.com. It defines the OAuth callback URL,
	// the accepted Origin of mutating requests and whether cookies are Secure.
	ConsoleURL string
	// InsecureNoAuth disables authentication and the Origin check. It is
	// meant for local development and tests only.
	InsecureNoAuth bool
}

type sessionKey struct{}

// sessionFrom returns the session attached to the request by guard.
func sessionFrom(r *http.Request) (store.Session, bool) {
	sess, ok := r.Context().Value(sessionKey{}).(store.Session)
	return sess, ok
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomID returns a short random identifier that is safe to show.
func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// clientIP returns the address of the browser. koroku listens on loopback
// behind Caddy, which replaces any X-Forwarded-For a client sends with the
// address it sees (it trusts no proxies by default), so the header is
// believed only on connections from loopback and only its last entry.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if fwd := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); fwd != nil {
				return fwd.String()
			}
		}
	}
	return host
}

// userAgent returns the User-Agent header, cut to a storable length.
func userAgent(r *http.Request) string {
	ua := r.UserAgent()
	if len(ua) > maxUserAgent {
		ua = strings.ToValidUTF8(ua[:maxUserAgent], "")
	}
	return ua
}

// safeReturnPath accepts only a path on the console itself, so the login
// cannot be used to redirect to another site.
func safeReturnPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Server) secureCookies() bool {
	return !strings.HasPrefix(s.auth.ConsoleURL, "http://")
}

// cookieName adds the __Host- prefix when the console is served over HTTPS.
// The apps this platform hosts live on sibling subdomains of the console,
// and a page on a sibling subdomain can set cookies for the shared parent
// domain. Browsers accept a __Host- cookie only from the exact host, with
// Secure and Path=/, so a hosted app cannot plant or replace the console's
// session or login state cookie.
func (s *Server) cookieName(name string) string {
	if s.secureCookies() {
		return "__Host-" + name
	}
	return name
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(name), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// lookupSession resolves the session cookie of a request.
func (s *Server) lookupSession(r *http.Request) (store.Session, bool) {
	cookie, err := r.Cookie(s.cookieName(sessionCookie))
	if err != nil || cookie.Value == "" {
		return store.Session{}, false
	}
	sess, err := s.store.GetSession(r.Context(), hashToken(cookie.Value), s.clock.Now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("loading session failed", "error", err)
		}
		return store.Session{}, false
	}
	// The owner may have been changed in the configuration, and a
	// member may have lost its last role.
	if allowed, err := s.userAllowed(r.Context(), sess.GitHubUserID); err != nil || !allowed {
		if err != nil {
			s.log.Error("checking the session's user failed", "error", err)
		}
		return store.Session{}, false
	}
	if now := s.clock.Now(); now.Sub(sess.LastSeenAt) >= touchInterval {
		ip, ua := clientIP(r), userAgent(r)
		if err := s.store.TouchSession(r.Context(), sess.TokenHash, now, ip, ua); err != nil {
			s.log.Warn("recording session use failed", "error", err)
		} else {
			sess.LastSeenAt, sess.IP, sess.UserAgent = now, ip, ua
		}
	}
	return sess, true
}

// recentlyAuthenticated reports whether the request's session was issued
// within ReauthWindow. Without authentication there is nothing to repeat.
func (s *Server) recentlyAuthenticated(r *http.Request) bool {
	if s.auth.InsecureNoAuth {
		return true
	}
	sess, ok := sessionFrom(r)
	return ok && s.clock.Now().Sub(sess.CreatedAt) < ReauthWindow
}

// requireRecentAuth refuses a dangerous operation unless the user logged in
// within ReauthWindow, so that a stolen session cookie alone cannot delete
// apps, read secrets or turn off safeguards. It reports whether to go on.
func (s *Server) requireRecentAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.recentlyAuthenticated(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "reauth_required", "log in again to do this")
	return false
}

func isMutating(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// guard protects the API: mutating requests must come from the console's
// own origin, and everything under /api/ except /api/session needs a session
// or, for a few read-only routes, an API token (ARCHITECTURE 10.5).
func (s *Server) guard(mux *http.ServeMux) http.Handler {
	var next http.Handler = mux
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isAPI := strings.HasPrefix(path, "/api/")
		// Browsers label where a request comes from. The console only ever
		// calls its API from its own origin, so anything a browser marks as
		// coming from another site (including a hosted app on a sibling
		// subdomain) is refused before it can cost anything. Requests
		// without the header (curl, old browsers) are judged by the checks
		// below.
		if site := r.Header.Get("Sec-Fetch-Site"); isAPI && site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "forbidden_origin", "cross-site requests to the API are not allowed")
			return
		}
		if _, present := bearerToken(r); isAPI && present {
			// A token request carries no cookie powers and may only GET
			// routes on the token list, so the Origin and Content-Type
			// checks for cookie-authenticated mutations do not apply.
			if s.auth.InsecureNoAuth && !isLoopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "forbidden_host", "in --insecure-no-auth mode the API answers only on a loopback host name")
				return
			}
			s.serveWithToken(w, r, mux)
			return
		}
		if isMutating(r.Method) && isAPI && r.ContentLength != 0 &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			// A form or text/plain POST needs no CORS preflight; JSON does.
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "request bodies must be application/json")
			return
		}
		if s.auth.InsecureNoAuth {
			// Development mode has no login, so the browser of whoever runs
			// it is the attack surface: any web page could otherwise post to
			// 127.0.0.1, and DNS rebinding could read from it.
			if !isLoopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "forbidden_host", "in --insecure-no-auth mode the API answers only on a loopback host name")
				return
			}
			// The console is served over plain HTTP by koroku itself, or over
			// HTTPS by a Caddy that listens on loopback only.
			if origin := r.Header.Get("Origin"); isMutating(r.Method) && origin != "" &&
				origin != "http://"+r.Host && origin != "https://"+r.Host {
				writeError(w, http.StatusForbidden, "forbidden_origin", "the request does not come from the console")
				return
			}
			next.ServeHTTP(w, withActor(r, actor{Login: "local", Owner: true}))
			return
		}
		if isMutating(r.Method) && (isAPI || strings.HasPrefix(path, "/auth/")) {
			// Browsers always send Origin on such requests. Requiring an
			// exact match blocks cross-site requests even if a cookie leaks
			// past SameSite.
			if origin := r.Header.Get("Origin"); origin == "" || origin != s.auth.ConsoleURL {
				writeError(w, http.StatusForbidden, "forbidden_origin", "the request does not come from the console")
				return
			}
		}
		sess, ok := s.lookupSession(r)
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess))
			r = withActor(r, actor{
				UserID: sess.GitHubUserID, Login: sess.Login,
				Owner: sess.GitHubUserID == s.auth.AllowedUserID,
			})
		}
		if isAPI && path != "/api/session" && !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "login required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy fits the built console: scripts and styles come from
// its own files only (the build has no inline script), the API is same
// origin, and the only foreign resource is the GitHub avatar of the user.
// Inline style attributes are allowed because templates may set them.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https://avatars.githubusercontent.com; " +
	"connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// isLoopbackHost reports whether a Host header names this machine itself.
// Names under .localhost always resolve to loopback (RFC 6761), so a page
// served from another site cannot reach them through DNS rebinding; the
// local Caddy serves the console as console.dev.localhost.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// securityHeaders sets headers that are right for every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.secureCookies() {
			// The console is only ever meant to be reached over HTTPS.
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.auth.InsecureNoAuth {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"user":          map[string]any{"id": 0, "login": "local", "avatar_url": "", "owner": true},
			"reauth_until":  nil,
		})
		return
	}
	sess, ok := sessionFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user": map[string]any{
			"id": sess.GitHubUserID, "login": sess.Login, "avatar_url": sess.AvatarURL,
			"owner": sess.GitHubUserID == s.auth.AllowedUserID,
		},
		"reauth_until": sess.CreatedAt.Add(ReauthWindow),
	})
}

func (s *Server) callbackURL() string { return s.auth.ConsoleURL + "/auth/github/callback" }

func loginErrorRedirect(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?login_error="+url.QueryEscape(code), http.StatusSeeOther)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth.GitHub == nil {
		loginErrorRedirect(w, r, "not_configured")
		return
	}
	state, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setCookie(w, stateCookie, state, stateLifetime)
	if back := safeReturnPath(r.URL.Query().Get("return_to")); back != "/" {
		s.setCookie(w, returnCookie, url.QueryEscape(back), stateLifetime)
	} else {
		s.clearCookie(w, returnCookie)
	}
	http.Redirect(w, r, s.auth.GitHub.AuthorizeURL(state, s.callbackURL()), http.StatusSeeOther)
}

// handleCallback finishes the web application flow. The user access token
// is used once to read the user's ID and is never stored.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.auth.GitHub == nil {
		loginErrorRedirect(w, r, "not_configured")
		return
	}
	// Anyone can reach this endpoint with a made-up code and state; without
	// a limit each such request would make koroku call GitHub with the App's
	// client secret.
	if !s.callbackLimiter.allow(s.clock.Now()) {
		loginErrorRedirect(w, r, "github_error")
		return
	}
	query := r.URL.Query()
	cookie, err := r.Cookie(s.cookieName(stateCookie))
	s.clearCookie(w, stateCookie)
	state := query.Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		loginErrorRedirect(w, r, "state_mismatch")
		return
	}
	code := query.Get("code")
	if code == "" {
		// The user declined, or GitHub reported an error.
		loginErrorRedirect(w, r, "github_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, err := s.auth.GitHub.ExchangeCode(ctx, code, s.callbackURL())
	if err != nil {
		s.log.Warn("github code exchange failed", "error", err)
		loginErrorRedirect(w, r, "github_error")
		return
	}
	user, err := s.auth.GitHub.User(ctx, token)
	if err != nil {
		s.log.Warn("reading the github user failed", "error", err)
		loginErrorRedirect(w, r, "github_error")
		return
	}
	// The owner, and members with a role on at least one app, may log in
	// (ARCHITECTURE 10.7). Nobody may when the owner is not configured.
	allowed := false
	if s.auth.AllowedUserID != 0 {
		allowed, err = s.userAllowed(ctx, user.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if !allowed {
		s.log.Warn("login rejected: neither the owner nor a member with a role", "github_user_id", user.ID, "login", user.Login)
		s.recordLogin(ctx, r, user, store.LoginNotAllowed)
		loginErrorRedirect(w, r, store.LoginNotAllowed)
		return
	}
	if user.ID != s.auth.AllowedUserID {
		// The login may have been renamed since the member was added.
		if err := s.store.UpdateMemberProfile(ctx, user.ID, user.Login, user.AvatarURL); err != nil {
			s.log.Warn("updating the member's profile failed", "error", err)
		}
	}

	sessionToken, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	sessionID, err := randomID()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := s.clock.Now()
	if err := s.store.DeleteExpiredSessions(ctx, now); err != nil {
		s.log.Warn("removing expired sessions failed", "error", err)
	}
	// Logging in again (to confirm a dangerous operation) replaces the
	// browser's session instead of piling up a new one each time.
	if old, err := r.Cookie(s.cookieName(sessionCookie)); err == nil && old.Value != "" {
		if err := s.store.DeleteSession(ctx, hashToken(old.Value)); err != nil {
			s.log.Warn("removing the previous session failed", "error", err)
		}
	}
	err = s.store.CreateSession(ctx, store.Session{
		TokenHash:    hashToken(sessionToken),
		ID:           sessionID,
		GitHubUserID: user.ID,
		Login:        user.Login,
		AvatarURL:    user.AvatarURL,
		CreatedAt:    now,
		ExpiresAt:    now.Add(SessionLifetime),
		IP:           clientIP(r),
		UserAgent:    userAgent(r),
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.recordLogin(ctx, r, user, store.LoginSucceeded)
	s.setCookie(w, sessionCookie, sessionToken, SessionLifetime)
	s.log.Info("login succeeded", "login", user.Login)
	back := "/"
	if c, err := r.Cookie(s.cookieName(returnCookie)); err == nil {
		if p, err := url.QueryUnescape(c.Value); err == nil {
			back = safeReturnPath(p)
		}
	}
	s.clearCookie(w, returnCookie)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// recordLogin adds a login attempt of a known GitHub user to the history.
// A failure to record does not change the outcome of the login.
func (s *Server) recordLogin(ctx context.Context, r *http.Request, user github.User, outcome string) {
	err := s.store.AddLoginEvent(ctx, store.LoginEvent{
		At:           s.clock.Now(),
		Outcome:      outcome,
		GitHubUserID: user.ID,
		Login:        user.Login,
		IP:           clientIP(r),
		UserAgent:    userAgent(r),
	})
	if err != nil {
		s.log.Warn("recording the login failed", "error", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName(sessionCookie)); err == nil && cookie.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(cookie.Value)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.clearCookie(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

// rateLimiter allows a fixed number of events per window.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
}

func (l *rateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= l.window {
		l.start, l.count = now, 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}
