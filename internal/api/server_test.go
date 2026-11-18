package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

type fakeProber struct {
	info system.Info
}

func (f fakeProber) Info(context.Context) system.Info { return f.info }

func (f fakeProber) DiskUsage() (system.Disk, error) {
	if f.info.Disk == nil {
		return system.Disk{}, nil
	}
	return *f.info.Disk, nil
}

func newTestServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.BaseDomain = "dev.example.com"
	cfg.ConsoleHost = "console.dev.example.com"
	cfg.DataDir = t.TempDir()
	box, err := secrets.NewBox([]byte(strings.Repeat("k", secrets.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Auth:     AuthOptions{InsecureNoAuth: true},
		Clock:    clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)),
		Box:      box,
		Deployer: &fakeDeployer{store: st, logDir: t.TempDir()},
		Refs:     fakeRefs{"main": testSHA},
		Config:   cfg,
		Store:    st,
		System: fakeProber{info: system.Info{
			Disk:          &system.Disk{TotalBytes: 100, UsedBytes: 40, UsedPercent: 40},
			Memory:        &system.Memory{TotalBytes: 4096, AvailableBytes: 1024},
			DockerVersion: "29.8.0",
		}},
		Web: fstest.MapFS{
			"index.html":        {Data: []byte("<!doctype html><title>console</title>")},
			"assets/app-abc.js": {Data: []byte("console.log('app')")},
			"robots.txt":        {Data: []byte("User-agent: *\nDisallow: /\n")},
		},
		Version: "test",
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestSystemEndpoint(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	rec := do(t, h, http.MethodGet, "/api/system")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Version       string         `json:"version"`
		BaseDomain    string         `json:"base_domain"`
		Disk          *system.Disk   `json:"disk"`
		Memory        *system.Memory `json:"memory"`
		DockerVersion string         `json:"docker_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != "test" || body.BaseDomain != "dev.example.com" || body.DockerVersion != "29.8.0" {
		t.Errorf("unexpected body: %s", rec.Body)
	}
	if body.Disk == nil || body.Disk.UsedPercent != 40 || body.Memory == nil || body.Memory.TotalBytes != 4096 {
		t.Errorf("unexpected resources: %s", rec.Body)
	}
}

func TestSystemEndpointWithPartialInfo(t *testing.T) {
	h := newTestServer(t, func(o *Options) { o.System = fakeProber{} }).Handler()
	rec := do(t, h, http.MethodGet, "/api/system")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["disk"] != nil || body["memory"] != nil {
		t.Errorf("expected null disk and memory, got %s", rec.Body)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	rec := do(t, h, http.MethodGet, "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q", body.Error.Code)
	}
}

func TestSPAServing(t *testing.T) {
	h := newTestServer(t, nil).Handler()

	t.Run("index", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>console</title>") {
			t.Errorf("status = %d, body = %q", rec.Code, rec.Body)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q", cc)
		}
	})
	t.Run("client-side route falls back to index", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/apps/demo/deployments")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>console</title>") {
			t.Errorf("status = %d, body = %q", rec.Code, rec.Body)
		}
	})
	t.Run("hashed asset is cached", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/assets/app-abc.js")
		if rec.Code != http.StatusOK || rec.Body.String() != "console.log('app')" {
			t.Errorf("status = %d, body = %q", rec.Code, rec.Body)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("Cache-Control = %q", cc)
		}
	})
	t.Run("plain file", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/robots.txt")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Disallow") {
			t.Errorf("status = %d, body = %q", rec.Code, rec.Body)
		}
	})
	t.Run("non-GET is rejected", func(t *testing.T) {
		rec := do(t, h, http.MethodPost, "/apps")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d", rec.Code)
		}
	})
}

func TestSPAWithoutBuild(t *testing.T) {
	for name, web := range map[string]fstest.MapFS{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			h := newTestServer(t, func(o *Options) {
				if web == nil {
					o.Web = nil
				} else {
					o.Web = web
				}
			}).Handler()
			rec := do(t, h, http.MethodGet, "/")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d", rec.Code)
			}
			// The API keeps working without the frontend.
			if rec := do(t, h, http.MethodGet, "/api/system"); rec.Code != http.StatusOK {
				t.Errorf("api status = %d", rec.Code)
			}
		})
	}
}

func TestPanicIsRecovered(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := do(t, h, http.MethodGet, "/api/system")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic value leaked to the client")
	}
}
