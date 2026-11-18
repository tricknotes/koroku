package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

const (
	sessionCookie = "paas_session"
	stateCookie   = "paas_oauth_state"
	// SessionLifetime is how long a login lasts. The console has root-level
	// power over the host, so sessions are short and do not slide.
	SessionLifetime = 7 * 24 * time.Hour
	stateLifetime   = 10 * time.Minute
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
	// AllowedUserID is the only GitHub user that may log in.
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

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
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
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// lookupSession resolves the session cookie of a request.
func (s *Server) lookupSession(r *http.Request) (store.Session, bool) {
	cookie, err := r.Cookie(sessionCookie)
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
	// The allowed user may have been changed in the configuration.
	if sess.GitHubUserID != s.auth.AllowedUserID {
		return store.Session{}, false
	}
	return sess, true
}

func isMutating(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// guard protects the API: mutating requests must come from the console's
// own origin, and everything under /api/ except /api/session needs a session.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isAPI := strings.HasPrefix(path, "/api/")
		if s.auth.InsecureNoAuth {
			next.ServeHTTP(w, r)
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
		}
		if isAPI && path != "/api/session" && !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "login required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets headers that are right for every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.auth.InsecureNoAuth {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"user":          map[string]any{"id": 0, "login": "local", "avatar_url": ""},
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
		"user":          map[string]any{"id": sess.GitHubUserID, "login": sess.Login, "avatar_url": sess.AvatarURL},
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
	http.Redirect(w, r, s.auth.GitHub.AuthorizeURL(state, s.callbackURL()), http.StatusSeeOther)
}

// handleCallback finishes the web application flow. The user access token
// is used once to read the user's ID and is never stored.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.auth.GitHub == nil {
		loginErrorRedirect(w, r, "not_configured")
		return
	}
	query := r.URL.Query()
	cookie, err := r.Cookie(stateCookie)
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
	if s.auth.AllowedUserID == 0 || user.ID != s.auth.AllowedUserID {
		s.log.Warn("login rejected: not the allowed user", "github_user_id", user.ID, "login", user.Login)
		loginErrorRedirect(w, r, "not_allowed")
		return
	}

	sessionToken, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := s.clock.Now()
	if err := s.store.DeleteExpiredSessions(ctx, now); err != nil {
		s.log.Warn("removing expired sessions failed", "error", err)
	}
	err = s.store.CreateSession(ctx, store.Session{
		TokenHash:    hashToken(sessionToken),
		GitHubUserID: user.ID,
		Login:        user.Login,
		AvatarURL:    user.AvatarURL,
		CreatedAt:    now,
		ExpiresAt:    now.Add(SessionLifetime),
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setCookie(w, sessionCookie, sessionToken, SessionLifetime)
	s.log.Info("login succeeded", "login", user.Login)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(cookie.Value)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.clearCookie(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}
