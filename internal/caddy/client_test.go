package caddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientLoadPostsConfig(t *testing.T) {
	var method, path, contentType, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		method, path, contentType, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(data)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A trailing slash in the base URL must not produce "//load".
	c := &Client{AdminURL: srv.URL + "/", HTTP: srv.Client()}
	if err := c.Load(context.Background(), []byte(`{"admin":{}}`)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if method != http.MethodPost || path != "/load" {
		t.Errorf("request = %s %s, want POST /load", method, path)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if body != `{"admin":{}}` {
		t.Errorf("body = %q", body)
	}
}

func TestClientLoadReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "{\"error\":\"loading config: loading new config:\\n unknown field\"}\n")
	}))
	defer srv.Close()

	err := (&Client{AdminURL: srv.URL}).Load(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d", statusErr.StatusCode)
	}
	for _, want := range []string{"caddy load", "400", "unknown field"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error %q must be a single line", err)
	}
}

func TestClientLoadTruncatesLongErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("あ", 10000))
	}))
	defer srv.Close()

	err := (&Client{AdminURL: srv.URL}).Load(context.Background(), []byte(`{}`))
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *StatusError, got %v", err)
	}
	if len(statusErr.Body) > maxErrorBody+len("...") || !strings.HasSuffix(statusErr.Body, "...") {
		t.Errorf("body was not truncated: %d bytes", len(statusErr.Body))
	}
	if strings.ContainsRune(statusErr.Body, '�') {
		t.Error("truncation left a broken character")
	}
}

func TestClientLoadEmptyErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := (&Client{AdminURL: srv.URL}).Load(context.Background(), []byte(`{}`))
	if err == nil || !strings.HasSuffix(err.Error(), "unexpected status 503") {
		t.Errorf("error = %v", err)
	}
}

// hangingServer answers nothing until the client gives up.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

func TestClientLoadTimesOut(t *testing.T) {
	srv := hangingServer(t)

	start := time.Now()
	err := (&Client{AdminURL: srv.URL, Timeout: 50 * time.Millisecond}).Load(context.Background(), []byte(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Load returned after %s", elapsed)
	}
}

func TestClientLoadHonorsCancellation(t *testing.T) {
	srv := hangingServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := (&Client{AdminURL: srv.URL}).Load(ctx, []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a cancellation error, got %v", err)
	}
}

func TestClientLoadConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	err := (&Client{AdminURL: url, Timeout: 5 * time.Second}).Load(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "caddy load") {
		t.Errorf("error = %v", err)
	}
}

func TestClientDefaults(t *testing.T) {
	if DefaultAdminURL != "http://127.0.0.1:2019" {
		t.Errorf("DefaultAdminURL = %q", DefaultAdminURL)
	}
}
