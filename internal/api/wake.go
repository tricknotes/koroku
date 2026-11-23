package api

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/store"
)

// Waking sleeping apps (ARCHITECTURE 8.4). While an app sleeps, Caddy sends
// the requests that pass everything in front of the app to koroku with
// caddy.WakeHeader naming the app. koroku wakes the app, holds the request
// for a while and then proxies it to the app's port.

// AppWaker wakes a sleeping app. Wake returns once the app answers or ctx
// ends; the wake itself goes on without ctx. It returns nil at once for an
// app that is not asleep.
type AppWaker interface {
	Wake(ctx context.Context, app store.App) error
}

// DefaultWakeHold is how long a request waits for its app to wake before
// it is answered with 503 and Retry-After.
const DefaultWakeHold = 60 * time.Second

// wakeRetryAfter is the Retry-After of the "starting" answer, in seconds;
// the browser page reloads itself after the same time.
const wakeRetryAfter = 5

// wakeManualTimeout bounds a wake started from the console.
const wakeManualTimeout = 10 * time.Minute

// wakeDispatch hands the requests Caddy forwards for a sleeping app to the
// wake handler, before the console's security headers: the answer is the
// app's own. The header counts only on the hostname of the app it names;
// the console host and the access protection paths never wake anything,
// whatever headers they carry.
func (s *Server) wakeDispatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.Header.Get(caddy.WakeHeader)
		path := r.URL.Path
		if slug == "" || path == caddy.GateCheckPath || strings.HasPrefix(path, caddy.ReservedPrefix) ||
			requestHostname(r) == strings.ToLower(s.cfg.ConsoleHost) {
			next.ServeHTTP(w, r)
			return
		}
		app, err := s.appForHostname(r.Context(), requestHostname(r))
		if errors.Is(err, store.ErrNotFound) || (err == nil && app.Slug != slug) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			s.log.Error("loading the app to wake failed", "error", err)
			writeWakePage(w, r, http.StatusBadGateway, wakeFailedPage)
			return
		}
		s.handleWake(w, r, app)
	})
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request, app store.App) {
	if app.DesiredState != store.DesiredRunning || app.CurrentDeploymentID == nil {
		// Stopped meanwhile; Caddy is about to show its 503 page.
		writeWakePage(w, r, http.StatusServiceUnavailable, wakeStoppedPage)
		return
	}
	if app.SleepingSince != nil {
		if s.waker == nil {
			writeWakePage(w, r, http.StatusServiceUnavailable, wakeFailedPage)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.wakeHold)
		err := s.waker.Wake(ctx, app)
		cancel()
		switch {
		case err == nil:
		case r.Context().Err() != nil:
			return // the client went away
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
			w.Header().Set("Retry-After", strconv.Itoa(wakeRetryAfter))
			writeWakePage(w, r, http.StatusServiceUnavailable, wakeStartingPage)
			return
		default:
			writeWakePage(w, r, http.StatusBadGateway, wakeFailedPage)
			return
		}
	}
	s.proxyToApp(w, r, app)
}

// proxyToApp forwards the request to the app's public service as Caddy
// would: the same Host and X-Forwarded-* headers, responses not buffered,
// upgrades (WebSocket) passed through.
func (s *Server) proxyToApp(w http.ResponseWriter, r *http.Request, app store.App) {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(app.LivePort()))}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// Rewrite starts without these; Caddy has set them already.
			for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(name); len(v) > 0 {
					pr.Out.Header[name] = v
				}
			}
			pr.Out.Header.Del(caddy.WakeHeader)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				s.log.Warn("proxying to a woken app failed", "app", app.Slug, "error", err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

type wakePage struct {
	Title   string
	Message string
	Refresh bool
	// Text is the answer for clients other than browsers.
	Text string
}

var (
	wakeStartingPage = wakePage{
		Title:   "起動しています",
		Message: "しばらくアクセスがなかったため休止していたアプリを起動しています。このページは自動で読み直します。",
		Refresh: true,
		Text:    "the app is starting after a sleep; retry shortly",
	}
	wakeFailedPage = wakePage{
		Title:   "起動できませんでした",
		Message: "休止していたアプリを起動できませんでした。しばらくしてから、もう一度アクセスしてください。",
		Text:    "the app could not be started after a sleep",
	}
	wakeStoppedPage = wakePage{
		Title:   "停止中",
		Message: "このアプリは停止中です。しばらくしてから、もう一度アクセスしてください。",
		Text:    "the app is stopped",
	}
)

// wakePageTemplate is a plain page without scripts or outside resources.
var wakePageTemplate = template.Must(template.New("wake").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
{{if .Refresh}}<meta http-equiv="refresh" content="{{.RetryAfter}}">{{end}}
<title>{{.Title}}</title>
</head>
<body style="font-family: sans-serif; margin: 20vh auto 0; max-width: 32rem; padding: 0 1rem; color: #333;">
<h1 style="font-size: 1.25rem;">{{.Title}}</h1>
<p>{{.Message}}</p>
</body>
</html>
`))

// writeWakePage answers a browser's GET with an HTML page and anything else
// with one line of text.
func writeWakePage(w http.ResponseWriter, r *http.Request, status int, page wakePage) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	browser := (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
	if !browser {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(page.Text + "\n"))
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = wakePageTemplate.Execute(w, struct {
		wakePage
		RetryAfter int
	}{page, wakeRetryAfter})
}

// handleWakeApp wakes a sleeping app from the console and answers with the
// app once it is awake.
func (s *Server) handleWakeApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if app.SleepingSince != nil {
		if s.waker == nil {
			writeError(w, http.StatusServiceUnavailable, "wake_unavailable", "this koroku cannot wake apps")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), wakeManualTimeout)
		err := s.waker.Wake(ctx, app)
		cancel()
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.log.Warn("waking the app from the console failed", "app", app.Slug, "error", err)
			writeError(w, http.StatusBadGateway, "wake_failed",
				"the app could not be woken: its containers did not start or it did not pass the health check")
			return
		}
		s.recordActivity(r, app.ID, actAppWoken, nil)
	}
	updated, err := s.store.GetAppByID(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	j, err := s.appToJSON(r, updated)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}
