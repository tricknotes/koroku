package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tricknotes/koroku/internal/store"
)

// Read-only API tokens (ARCHITECTURE 10.5) let tools on the developer's
// machine, such as Claude Code, read why a deployment failed. They are
// accepted only as an Authorization: Bearer header and only for the GET
// routes in tokenRoutes.

const (
	// APITokenPrefix starts every token value so that secret scanners can
	// recognise a leaked token: koroku_ro_[A-Za-z0-9_-]{43}.
	APITokenPrefix = "koroku_ro_"
	// maxAPITokens bounds the live tokens of each user; one per machine is
	// plenty.
	maxAPITokens = 20
	// maxTokenName bounds the label of a token, in characters.
	maxTokenName = 100
	// tokenRequestsPerMinute is the rate limit of each token.
	tokenRequestsPerMinute = 120
)

// tokenLifetimes are the expiries the console offers, in days. There is no
// token without an expiry.
var tokenLifetimes = map[int]bool{7: true, 30: true, 90: true}

// tokenRoutes are the only routes an API token may use, as registered on
// the mux. Anything else, including every mutating route and every route
// that returns secrets (environment values, dumps, presigned URLs), answers
// 403 token_not_allowed. A new route is closed to tokens until it is added
// here.
var tokenRoutes = map[string]bool{
	"GET /api/system":                       true,
	"GET /api/apps":                         true,
	"GET /api/apps/{slug}":                  true,
	"GET /api/apps/{slug}/services":         true,
	"GET /api/apps/{slug}/deployments":      true,
	"GET /api/deployments/{id}":             true,
	"GET /api/deployments/{id}/logs":        true,
	"GET /api/apps/{slug}/logs":             true,
	"GET /api/apps/{slug}/logs/history":     true,
	"GET /api/apps/{slug}/env/requirements": true,
}

type tokenKey struct{}

// tokenFrom returns the API token a request was authenticated with.
func tokenFrom(r *http.Request) (store.APIToken, bool) {
	tok, ok := r.Context().Value(tokenKey{}).(store.APIToken)
	return tok, ok
}

// bearerToken returns the token of an Authorization: Bearer header, and
// whether the request carries an Authorization header at all.
func bearerToken(r *http.Request) (token string, present bool) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", false
	}
	if len(values) > 1 {
		return "", true
	}
	scheme, value, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, APITokenPrefix) || len(value) == len(APITokenPrefix) {
		return "", true
	}
	return value, true
}

// tokenLimiter rate-limits each token on its own, so one runaway script does
// not lock out the others.
type tokenLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rateLimiter
}

func (l *tokenLimiter) allow(id string, now time.Time) bool {
	l.mu.Lock()
	lim, ok := l.limiters[id]
	if !ok {
		if l.limiters == nil {
			l.limiters = map[string]*rateLimiter{}
		}
		// Revoked tokens leave their entry behind; drop stale ones so the
		// map stays as small as the set of tokens in use.
		for key, other := range l.limiters {
			if now.Sub(other.start) > time.Hour {
				delete(l.limiters, key)
			}
		}
		lim = &rateLimiter{limit: tokenRequestsPerMinute, window: time.Minute}
		l.limiters[id] = lim
	}
	l.mu.Unlock()
	return lim.allow(now)
}

// serveWithToken handles an API request that carries an Authorization
// header. Cookies are ignored: a token request never gains the powers of a
// session that happens to be sent along.
func (s *Server) serveWithToken(w http.ResponseWriter, r *http.Request, mux *http.ServeMux) {
	value, _ := bearerToken(r)
	if value == "" {
		s.invalidToken(w)
		return
	}
	now := s.clock.Now()
	tok, err := s.store.GetAPIToken(r.Context(), hashToken(value), now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("loading api token failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			return
		}
		s.invalidToken(w)
		return
	}
	if info := requestInfoFrom(r); info != nil {
		info.tokenID = tok.ID
	}
	// A token acts as its user, who must still be allowed in: removing a
	// member deletes its tokens, and this also covers a changed owner.
	owner := s.auth.InsecureNoAuth || (s.auth.AllowedUserID != 0 && tok.GitHubUserID == s.auth.AllowedUserID)
	if !owner {
		allowed, err := s.store.HasAnyRole(r.Context(), tok.GitHubUserID)
		if err != nil {
			s.log.Error("checking the api token's user failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			return
		}
		if !allowed {
			s.invalidToken(w)
			return
		}
	}
	if !s.tokenLimiter.allow(tok.ID, now) {
		w.Header().Set("Retry-After", strconv.Itoa(60))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests with this API token")
		return
	}
	// The route the mux would run must be on the list. Asking the mux
	// itself means the decision cannot drift from the routing.
	if _, pattern := mux.Handler(r); r.Method != http.MethodGet || !tokenRoutes[pattern] {
		writeError(w, http.StatusForbidden, "token_not_allowed", "API tokens cannot use this endpoint")
		return
	}
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) >= touchInterval {
		ip := clientIP(r)
		if err := s.store.TouchAPIToken(r.Context(), tok.ID, now, ip); err != nil {
			s.log.Warn("recording api token use failed", "token_id", tok.ID, "error", err)
		} else {
			tok.LastUsedAt, tok.LastUsedIP = &now, ip
		}
	}
	r = r.Clone(context.WithValue(r.Context(), tokenKey{}, tok))
	r = withActor(r, actor{UserID: tok.GitHubUserID, Login: tok.Login, Owner: owner})
	r.Header.Del("Cookie")
	mux.ServeHTTP(w, r)
}

func (s *Server) invalidToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, "invalid_token", "the API token is invalid, expired or revoked")
}

type apiTokenJSON struct {
	ID string `json:"id"`
	// User is who the token acts as; the owner sees everyone's tokens.
	User       userRefJSON `json:"user"`
	Name       string      `json:"name"`
	CreatedAt  time.Time   `json:"created_at"`
	ExpiresAt  time.Time   `json:"expires_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
	LastUsedIP string      `json:"last_used_ip"`
}

type userRefJSON struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func apiTokenView(tok store.APIToken) apiTokenJSON {
	return apiTokenJSON{
		ID: tok.ID, Name: tok.Name, CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt,
		User:       userRefJSON{ID: tok.GitHubUserID, Login: tok.Login},
		LastUsedAt: tok.LastUsedAt, LastUsedIP: tok.LastUsedIP,
	}
}

// recordsOf is whose tokens, sessions and login history a request may
// see and revoke: everyone's for the owner, its own for a member.
func recordsOf(r *http.Request) int64 {
	a, _ := actorFrom(r)
	if a.Owner {
		return store.AllUsers
	}
	return a.UserID
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAPITokens(r.Context(), recordsOf(r), s.clock.Now())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := []apiTokenJSON{}
	for _, tok := range list {
		out = append(out, apiTokenView(tok))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// validTokenName accepts a short, printable label.
func validTokenName(name string) string {
	switch {
	case name == "":
		return "is required"
	case !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxTokenName:
		return "must be at most 100 characters"
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "must not contain control characters"
	}
	return ""
}

// handleCreateToken issues a token. It needs a recent login: a stolen
// session cookie alone must not turn into a long-lived way in.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	errs := fieldErrors{}
	if msg := validTokenName(body.Name); msg != "" {
		errs["name"] = msg
	}
	if !tokenLifetimes[body.ExpiresInDays] {
		errs["expires_in_days"] = "must be 7, 30 or 90"
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if !s.requireRecentAuth(w, r) {
		return
	}
	secret, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	id, err := randomID()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	value := APITokenPrefix + secret
	now := s.clock.Now()
	who, _ := actorFrom(r)
	tok := store.APIToken{
		ID: id, TokenHash: hashToken(value), Name: body.Name,
		GitHubUserID: who.UserID, Login: who.Login,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour),
	}
	err = s.store.CreateAPIToken(r.Context(), tok, maxAPITokens)
	if errors.Is(err, store.ErrTooManyTokens) {
		writeError(w, http.StatusConflict, "too_many_tokens", "revoke a token before creating another")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("api token created", "token_id", tok.ID, "expires_at", tok.ExpiresAt)
	writeJSON(w, http.StatusCreated, map[string]any{"token": apiTokenView(tok), "value": value})
}

// handleDeleteToken revokes a token. Revoking stops harm, so it needs no
// recent login.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.DeleteAPIToken(r.Context(), recordsOf(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token_not_found", "no such token")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("api token revoked", "token_id", id)
	w.WriteHeader(http.StatusNoContent)
}
