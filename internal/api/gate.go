package api

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/store"
)

// Access protection of apps (ARCHITECTURE 8.3). Caddy asks gateCheck about
// every request to a protected hostname; a browser without a valid gate
// cookie is sent to the console's /auth/gate, which hands a one-time code to
// /.koroku/auth/callback on the app host, which sets the cookie.

const (
	// gateCodeLifetime bounds how long a one-time code in a redirect URL
	// is worth anything.
	gateCodeLifetime = time.Minute
	// maxGateReturn bounds the return path carried through the redirects.
	maxGateReturn    = 2048
	gateCallbackPath = caddy.GateAuthPrefix + "callback"
	gateLogoutPath   = caddy.GateAuthPrefix + "logout"
)

// requestHostname is the Host of a request without port, lowercased.
func requestHostname(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// isAppHostname reports whether host is an automatic app hostname, a name
// under the base domain other than the console's.
func (s *Server) isAppHostname(host string) bool {
	return s.cfg.BaseDomain != "" && host != strings.ToLower(s.cfg.ConsoleHost) &&
		strings.HasSuffix(host, "."+strings.ToLower(s.cfg.BaseDomain))
}

// appForHostname returns the app served on host: <slug>.<base> or an active
// custom domain. It returns store.ErrNotFound for anything else.
func (s *Server) appForHostname(ctx context.Context, host string) (store.App, error) {
	if host == "" || host == strings.ToLower(s.cfg.ConsoleHost) {
		return store.App{}, store.ErrNotFound
	}
	if s.isAppHostname(host) {
		slug := strings.TrimSuffix(host, "."+strings.ToLower(s.cfg.BaseDomain))
		if slug == "" || strings.Contains(slug, ".") {
			return store.App{}, store.ErrNotFound
		}
		return s.store.GetApp(ctx, slug)
	}
	d, err := s.store.GetActiveDomainByHostname(ctx, host)
	if err != nil {
		return store.App{}, err
	}
	return s.store.GetAppByID(ctx, d.AppID)
}

// protectedApp is appForHostname limited to apps with access protection.
func (s *Server) protectedApp(ctx context.Context, host string) (store.App, bool, error) {
	app, err := s.appForHostname(ctx, host)
	if errors.Is(err, store.ErrNotFound) {
		return store.App{}, false, nil
	}
	if err != nil {
		return store.App{}, false, err
	}
	return app, app.AccessProtection, nil
}

// gateReturnPath accepts only a path on the same host, like the console's
// own return_to, and not an absurdly long one.
func gateReturnPath(p string) string {
	if len(p) > maxGateReturn {
		return "/"
	}
	return safeReturnPath(p)
}

// consoleGateURL is where a browser without a gate cookie for host goes.
func (s *Server) consoleGateURL(host, back string) string {
	return s.auth.ConsoleURL + "/auth/gate?" + url.Values{"host": {host}, "return": {back}}.Encode()
}

// mayOpenApp reports whether a console user may open a protected app: the
// owner, or a member with any role on it (ARCHITECTURE 10.7).
func (s *Server) mayOpenApp(ctx context.Context, userID int64, app store.App) bool {
	if s.auth.AllowedUserID != 0 && userID == s.auth.AllowedUserID {
		return true
	}
	role, err := s.store.AppRole(ctx, app.ID, userID)
	if err != nil {
		s.log.Error("loading the role for the gate failed", "error", err)
		return false
	}
	return role != ""
}

// gateGranted reports whether the request carries a gate cookie for host
// whose console session is still valid and belongs to a user who may open
// the app.
func (s *Server) gateGranted(r *http.Request, host string, app store.App) bool {
	cookie, err := r.Cookie(caddy.GateCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	sess, err := s.store.GateGrantSession(r.Context(), hashToken(cookie.Value), host, s.clock.Now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("loading gate grant failed", "error", err)
		}
		return false
	}
	return s.mayOpenApp(r.Context(), sess.GitHubUserID, app)
}

// handleGateCheck answers Caddy's forward_auth for a protected hostname:
// 204 lets the request through; anything else is sent to the browser.
func (s *Server) handleGateCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	host := requestHostname(r)
	app, err := s.appForHostname(r.Context(), host)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	if err != nil {
		s.log.Error("gate check failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The database is the truth: an app whose protection was just turned
	// off passes even before Caddy's configuration catches up.
	if !app.AccessProtection || s.auth.InsecureNoAuth || s.gateGranted(r, host, app) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Header.Get("X-Forwarded-Method") {
	case http.MethodGet, http.MethodHead:
		back := gateReturnPath(r.Header.Get("X-Forwarded-Uri"))
		http.Redirect(w, r, s.consoleGateURL(host, back), http.StatusFound)
	default:
		// A form post or fetch cannot be replayed after a login.
		http.Error(w, "login required: open this page in the browser to log in", http.StatusUnauthorized)
	}
}

// handleGateStart runs on the console: it turns the console session into a
// one-time code for one protected hostname.
func (s *Server) handleGateStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	host := strings.ToLower(query.Get("host"))
	back := gateReturnPath(query.Get("return"))
	app, protected, err := s.protectedApp(r.Context(), host)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !protected {
		// Never redirect to a host that is not a protected app of this
		// platform: the console must not become an open redirector.
		writeGatePage(w, http.StatusBadRequest, gatePage{
			Title:   "このアドレスは開けません",
			Message: "koroku が保護しているアプリのホスト名ではありません。",
		})
		return
	}
	if s.auth.InsecureNoAuth {
		http.Redirect(w, r, "https://"+host+back, http.StatusSeeOther)
		return
	}
	sess, ok := sessionFrom(r)
	if !ok {
		here := "/auth/gate?" + url.Values{"host": {host}, "return": {back}}.Encode()
		http.Redirect(w, r, "/auth/github/login?"+url.Values{"return_to": {here}}.Encode(), http.StatusSeeOther)
		return
	}
	if !s.mayOpenApp(r.Context(), sess.GitHubUserID, app) {
		writeGatePage(w, http.StatusForbidden, gatePage{
			Title:   "このアプリを開く権限がありません",
			Message: "このアプリのロールを持っていません。オーナーにロールの追加を頼んでください。",
		})
		return
	}
	code, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := s.clock.Now()
	if err := s.store.CreateGateCode(r.Context(), hashToken(code), host, sess.TokenHash, now.Add(gateCodeLifetime), now); err != nil {
		s.internalError(w, r, err)
		return
	}
	target := "https://" + host + gateCallbackPath + "?" + url.Values{"code": {code}, "return": {back}}.Encode()
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleGateCallback runs on the app host: it redeems the one-time code and
// sets the host-only gate cookie.
func (s *Server) handleGateCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	host := requestHostname(r)
	app, protected, err := s.protectedApp(r.Context(), host)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !protected {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	back := gateReturnPath(query.Get("return"))
	if s.auth.InsecureNoAuth {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	retry := gatePage{
		Title:    "ログインをやり直してください",
		Message:  "ログインのリンクが無効か、期限が切れています（リンクは 1 回だけ、1 分以内に使えます）。",
		LinkURL:  s.consoleGateURL(host, back),
		LinkText: "もう一度ログインする",
	}
	code := query.Get("code")
	now := s.clock.Now()
	sessionHash, err := "", store.ErrNotFound
	if code != "" {
		sessionHash, err = s.store.RedeemGateCode(r.Context(), hashToken(code), host, now)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeGatePage(w, http.StatusBadRequest, retry)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	sess, err := s.store.GetSession(r.Context(), sessionHash, now)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !s.mayOpenApp(r.Context(), sess.GitHubUserID, app)) {
		// The console session ended between the code and its use, or the
		// user lost its role on the app.
		writeGatePage(w, http.StatusBadRequest, retry)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	token, err := randomToken()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	err = s.store.CreateGateGrant(r.Context(), store.GateGrant{
		TokenHash:        hashToken(token),
		Hostname:         host,
		SessionTokenHash: sessionHash,
		CreatedAt:        now,
		ExpiresAt:        sess.ExpiresAt,
	}, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Host-only (__Host- forbids Domain), so neither another app nor a
	// sibling subdomain receives or can overwrite it. App hosts are always
	// served over HTTPS by Caddy, hence Secure regardless of the console.
	http.SetCookie(w, &http.Cookie{
		Name:     caddy.GateCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sess.ExpiresAt.Sub(now) / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleGateLogout forgets the gate cookie of this host only.
func (s *Server) handleGateLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, err := s.appForHostname(r.Context(), requestHostname(r)); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	if cookie, err := r.Cookie(caddy.GateCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteGateGrant(r.Context(), hashToken(cookie.Value)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: caddy.GateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	writeGatePage(w, http.StatusOK, gatePage{
		Title:    "ログアウトしました",
		Message:  "このアドレスでの閲覧を終了しました。管理画面からはログアウトしていないため、もう一度開くと自動でログインします。",
		LinkURL:  s.auth.ConsoleURL + "/",
		LinkText: "管理画面を開く",
	})
}

// gateMux serves the requests Caddy forwards from app hosts. Nothing else
// of koroku is reachable on those hosts (see hostDispatch).
func (s *Server) gateMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+caddy.GateCheckPath, s.handleGateCheck)
	mux.HandleFunc("GET "+gateCallbackPath, s.handleGateCallback)
	mux.HandleFunc("GET "+gateLogoutPath, s.handleGateLogout)
	mux.HandleFunc("POST "+gateLogoutPath, s.handleGateLogout)
	mux.HandleFunc("/", http.NotFound)
	return mux
}

// hostDispatch sends the access protection paths to the gate handlers and
// refuses everything else on app hostnames, so that a request for an app
// host can never reach the console, its API or its session cookie. The
// console host never serves the gate paths.
func (s *Server) hostDispatch(console, gate http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isGate := path == caddy.GateCheckPath || strings.HasPrefix(path, caddy.ReservedPrefix)
		host := requestHostname(r)
		switch {
		case isGate && host == strings.ToLower(s.cfg.ConsoleHost):
			http.NotFound(w, r)
		case isGate:
			gate.ServeHTTP(w, r)
		case s.isAppHostname(host):
			http.NotFound(w, r)
		default:
			console.ServeHTTP(w, r)
		}
	})
}

type gatePage struct {
	Title    string
	Message  string
	LinkURL  string
	LinkText string
}

// gatePageTemplate is a plain page without scripts (the console's CSP
// applies to it too).
var gatePageTemplate = template.Must(template.New("gate").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
</head>
<body style="font-family: sans-serif; margin: 20vh auto 0; max-width: 32rem; padding: 0 1rem; color: #333;">
<h1 style="font-size: 1.25rem;">{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .LinkURL}}<p><a href="{{.LinkURL}}">{{.LinkText}}</a></p>{{end}}
</body>
</html>
`))

func writeGatePage(w http.ResponseWriter, status int, page gatePage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = gatePageTemplate.Execute(w, page)
}
