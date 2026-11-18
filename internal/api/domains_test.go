package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// fakeDomainChecker marks hostnames listed in ok as verified.
type fakeDomainChecker struct {
	store *store.Store
	mu    sync.Mutex
	ok    map[string]bool
	calls []string
}

func (f *fakeDomainChecker) Check(ctx context.Context, d store.Domain) (store.Domain, error) {
	f.mu.Lock()
	verified := f.ok[d.Hostname]
	f.calls = append(f.calls, d.Hostname)
	f.mu.Unlock()
	message := ""
	if !verified {
		message = d.Hostname + " does not resolve"
	}
	if err := f.store.RecordDomainCheck(ctx, d.ID, verified, message, time.Date(2026, 11, 18, 3, 5, 0, 0, time.UTC)); err != nil {
		return d, err
	}
	return f.store.GetDomain(ctx, d.AppID, d.ID)
}

type fakeRouter struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeRouter) Sync(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil
}

type domainBody struct {
	ID            int64      `json:"id"`
	Hostname      string     `json:"hostname"`
	Status        string     `json:"status"`
	IsPrimary     bool       `json:"is_primary"`
	ForceActive   bool       `json:"force_active"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastError     string     `json:"last_error"`
	Records       []struct {
		Type, Name, Value string
	} `json:"records"`
}

func newDomainServer(t *testing.T) (http.Handler, *fakeDomainChecker, *fakeRouter) {
	t.Helper()
	checker := &fakeDomainChecker{ok: map[string]bool{}}
	router := &fakeRouter{}
	srv := newTestServer(t, func(o *Options) {
		checker.store = o.Store
		o.Domains = checker
		o.Router = router
		o.Config.PublicIP = "203.0.113.10"
	})
	h := srv.Handler()
	createApp(t, h, "blog")
	return h, checker, router
}

func TestAddAndListDomains(t *testing.T) {
	h, checker, _ := newDomainServer(t)

	empty := decode[struct {
		DefaultHostname string       `json:"default_hostname"`
		Domains         []domainBody `json:"domains"`
	}](t, request(t, h, http.MethodGet, "/api/apps/blog/domains", ""))
	if empty.DefaultHostname != "blog.dev.example.com" || empty.Domains == nil || len(empty.Domains) != 0 {
		t.Errorf("empty list = %+v", empty)
	}

	rec := request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": " WWW.Example.org. "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	d := decode[domainBody](t, rec)
	// The name is normalized, verified once right away, and still pending.
	if d.Hostname != "www.example.org" || d.Status != "pending" || d.LastCheckedAt == nil || d.LastError == "" {
		t.Errorf("domain = %+v", d)
	}
	if len(d.Records) != 2 || d.Records[0].Type != "CNAME" || d.Records[0].Value != "blog.dev.example.com" ||
		d.Records[1].Type != "A" || d.Records[1].Value != "203.0.113.10" {
		t.Errorf("records = %+v", d.Records)
	}

	// DNS already in place: active immediately.
	checker.ok["shop.example.org"] = true
	active := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "shop.example.org"}`))
	if active.Status != "active" || active.LastError != "" {
		t.Errorf("domain = %+v", active)
	}

	list := decode[struct{ Domains []domainBody }](t, request(t, h, http.MethodGet, "/api/apps/blog/domains", ""))
	if len(list.Domains) != 2 || list.Domains[0].Hostname != "shop.example.org" {
		t.Errorf("list = %+v", list)
	}
}

func TestAddDomainValidation(t *testing.T) {
	h, _, _ := newDomainServer(t)
	createApp(t, h, "shop")
	request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`)

	for _, hostname := range []string{"", "not a host", "https://example.org", "203.0.113.10", "*.example.org", "example.org:8443"} {
		rec := request(t, h, http.MethodPost, "/api/apps/blog/domains", fmt.Sprintf(`{"hostname": %q}`, hostname))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("hostname %q: status = %d", hostname, rec.Code)
		}
	}
	// Names under the base domain are assigned automatically.
	for _, hostname := range []string{"other.dev.example.com", "dev.example.com", "a.b.dev.example.com"} {
		rec := request(t, h, http.MethodPost, "/api/apps/blog/domains", fmt.Sprintf(`{"hostname": %q}`, hostname))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "assigned automatically") {
			t.Errorf("hostname %q: %d %s", hostname, rec.Code, rec.Body)
		}
	}
	// A hostname belongs to one app only.
	dup := request(t, h, http.MethodPost, "/api/apps/shop/domains", `{"hostname": "WWW.example.org"}`)
	if dup.Code != http.StatusConflict || !strings.Contains(dup.Body.String(), "hostname_taken") {
		t.Errorf("duplicate: %d %s", dup.Code, dup.Body)
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/missing/domains", `{"hostname": "x.example.org"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing app status = %d", rec.Code)
	}
}

func TestVerifyDomain(t *testing.T) {
	h, checker, _ := newDomainServer(t)
	d := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`))

	checker.ok["www.example.org"] = true
	rec := request(t, h, http.MethodPost, fmt.Sprintf("/api/apps/blog/domains/%d/verify", d.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := decode[domainBody](t, rec); got.Status != "active" {
		t.Errorf("domain = %+v", got)
	}
	for _, id := range []string{"999", "abc", "0"} {
		if rec := request(t, h, http.MethodPost, "/api/apps/blog/domains/"+id+"/verify", ""); rec.Code != http.StatusNotFound {
			t.Errorf("domain %s status = %d", id, rec.Code)
		}
	}
}

func TestDomainOfAnotherAppIsNotReachable(t *testing.T) {
	h, _, _ := newDomainServer(t)
	createApp(t, h, "shop")
	d := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`))

	path := fmt.Sprintf("/api/apps/shop/domains/%d", d.ID)
	if rec := request(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete through another app: status = %d", rec.Code)
	}
	if rec := request(t, h, http.MethodPatch, path, `{"force_active": true}`); rec.Code != http.StatusNotFound {
		t.Errorf("patch through another app: status = %d", rec.Code)
	}
}

func TestUpdateDomain(t *testing.T) {
	h, checker, router := newDomainServer(t)
	d := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`))
	path := fmt.Sprintf("/api/apps/blog/domains/%d", d.ID)

	// A pending domain cannot be the redirect target.
	rec := request(t, h, http.MethodPatch, path, `{"is_primary": true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("primary on pending: %d %s", rec.Code, rec.Body)
	}

	// Forcing it active (CDN in front) and making it primary in one request.
	before := router.calls
	rec = request(t, h, http.MethodPatch, path, `{"force_active": true, "is_primary": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	got := decode[domainBody](t, rec)
	if got.Status != "active" || !got.ForceActive || !got.IsPrimary {
		t.Errorf("domain = %+v", got)
	}
	if router.calls != before+1 {
		t.Errorf("routes were not synced: %d -> %d", before, router.calls)
	}

	// Removing the force re-verifies; DNS still does not match.
	checker.calls = nil
	rec = request(t, h, http.MethodPatch, path, `{"force_active": false}`)
	got = decode[domainBody](t, rec)
	if got.Status != "pending" || got.ForceActive || got.IsPrimary {
		t.Errorf("after unforce = %+v", got)
	}
	if len(checker.calls) != 1 {
		t.Errorf("unforcing must verify once, calls = %v", checker.calls)
	}

	if rec := request(t, h, http.MethodPatch, path, `{"status": "active"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field status = %d", rec.Code)
	}
}

func TestDeleteDomain(t *testing.T) {
	h, _, router := newDomainServer(t)
	d := decode[domainBody](t, request(t, h, http.MethodPost, "/api/apps/blog/domains", `{"hostname": "www.example.org"}`))
	path := fmt.Sprintf("/api/apps/blog/domains/%d", d.ID)

	before := router.calls
	if rec := request(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if router.calls != before+1 {
		t.Error("routes were not synced after deletion")
	}
	if rec := request(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d", rec.Code)
	}
	list := decode[struct{ Domains []domainBody }](t, request(t, h, http.MethodGet, "/api/apps/blog/domains", ""))
	if len(list.Domains) != 0 {
		t.Errorf("list = %+v", list)
	}
}

func TestCreatingAnAppSyncsRoutes(t *testing.T) {
	router := &fakeRouter{}
	h := newTestServer(t, func(o *Options) { o.Router = router }).Handler()
	createApp(t, h, "blog")
	if router.calls != 1 {
		t.Errorf("router calls = %d, want 1", router.calls)
	}
}
