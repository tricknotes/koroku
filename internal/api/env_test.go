package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// envListOut decodes the GET /api/apps/{slug}/env response.
type envListOut struct {
	Vars []struct {
		Key     string  `json:"key"`
		Value   *string `json:"value"`
		Pending *string `json:"pending"`
	} `json:"vars"`
	Removed  []string `json:"removed"`
	Compared bool     `json:"compared"`
}

func (l envListOut) value(key string) (string, bool) {
	for _, v := range l.Vars {
		if v.Key == key && v.Value != nil {
			return *v.Value, true
		}
	}
	return "", false
}

func (l envListOut) pending() map[string]string {
	out := map[string]string{}
	for _, v := range l.Vars {
		if v.Pending != nil {
			out[v.Key] = *v.Pending
		}
	}
	return out
}

func (l envListOut) keys() string {
	keys := make([]string, 0, len(l.Vars))
	for _, v := range l.Vars {
		keys = append(keys, v.Key)
	}
	return strings.Join(keys, ",")
}

func TestEnvVarPutAndDelete(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	createApp(t, h, "shop")
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "A", "value": "1"}, {"key": "B", "value": "2"}]}`)

	// Adding one variable keeps the others.
	added := request(t, h, http.MethodPut, "/api/apps/blog/env/C", `{"value": "it's $3"}`)
	if added.Code != http.StatusOK {
		t.Fatalf("add: %d %s", added.Code, added.Body)
	}
	got := decode[envListOut](t, added)
	if got.keys() != "A,B,C" {
		t.Errorf("after add: %s", got.keys())
	}
	if v, _ := got.value("C"); v != "it's $3" {
		t.Errorf("C = %q", v)
	}

	// Changing one replaces only its value.
	got = decode[envListOut](t, request(t, h, http.MethodPut, "/api/apps/blog/env/A", `{"value": "one"}`))
	if v, _ := got.value("A"); v != "one" || got.keys() != "A,B,C" {
		t.Errorf("after change: %+v", got)
	}

	// Renaming keeps the value unless a new one is given, and re-seals it
	// for the new key.
	got = decode[envListOut](t, request(t, h, http.MethodPut, "/api/apps/blog/env/B", `{"key": "BEE"}`))
	if v, _ := got.value("BEE"); v != "2" || got.keys() != "A,BEE,C" {
		t.Errorf("after rename: %+v", got)
	}
	got = decode[envListOut](t, request(t, h, http.MethodPut, "/api/apps/blog/env/BEE", `{"key": "B", "value": "two"}`))
	if v, _ := got.value("B"); v != "two" || got.keys() != "A,B,C" {
		t.Errorf("after rename with value: %+v", got)
	}

	if rec := request(t, h, http.MethodPut, "/api/apps/blog/env/B", `{"key": "A"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "env_var_exists") {
		t.Errorf("rename onto an existing key: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodPut, "/api/apps/blog/env/MISSING", `{"key": "X"}`); rec.Code != http.StatusNotFound {
		t.Errorf("rename of a missing key: %d %s", rec.Code, rec.Body)
	}

	deleted := request(t, h, http.MethodDelete, "/api/apps/blog/env/C", "")
	if deleted.Code != http.StatusOK || decode[envListOut](t, deleted).keys() != "A,B" {
		t.Errorf("delete: %d %s", deleted.Code, deleted.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/blog/env/C", ""); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "env_var_not_found") {
		t.Errorf("delete again: %d %s", rec.Code, rec.Body)
	}

	if other := decode[envListOut](t, request(t, h, http.MethodGet, "/api/apps/shop/env", "")); len(other.Vars) != 0 {
		t.Errorf("another app's env changed: %+v", other)
	}
}

func TestEnvVarPutValidation(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "KEEP", "value": "1"}]}`)

	cases := map[string]struct {
		target, body string
		status       int
	}{
		"bad key":       {"/api/apps/blog/env/NOT-VALID", `{"value": "topsecretvalue"}`, http.StatusUnprocessableEntity},
		"bad new key":   {"/api/apps/blog/env/KEEP", `{"key": "1BAD", "value": "topsecretvalue"}`, http.StatusUnprocessableEntity},
		"no value":      {"/api/apps/blog/env/NEW", `{}`, http.StatusUnprocessableEntity},
		"nul byte":      {"/api/apps/blog/env/NEW", `{"value": "topsecretvalue\u0000"}`, http.StatusUnprocessableEntity},
		"huge value":    {"/api/apps/blog/env/NEW", `{"value": "topsecretvalue` + strings.Repeat("x", maxEnvValueLen) + `"}`, http.StatusUnprocessableEntity},
		"unknown field": {"/api/apps/blog/env/NEW", `{"value": "topsecretvalue", "x": 1}`, http.StatusBadRequest},
		"missing app":   {"/api/apps/nope/env/NEW", `{"value": "topsecretvalue"}`, http.StatusNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPut, c.target, c.body)
			if rec.Code != c.status {
				t.Fatalf("status = %d, body = %.200s", rec.Code, rec.Body)
			}
			if rec.Code != http.StatusOK && strings.Contains(rec.Body.String(), "topsecretvalue") {
				t.Errorf("the error echoes a value: %.200s", rec.Body)
			}
		})
	}
}

// deploy records a succeeded deployment of the app that ran with vars.
func deployWithEnv(t *testing.T, srv *Server, slug string, vars map[string]string) {
	t.Helper()
	ctx := context.Background()
	app, err := srv.store.GetApp(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	d, _, err := srv.store.EnqueueDeployment(ctx, app.ID, "main", testSHA, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.StartDeployment(ctx, d.ID, now); err != nil {
		t.Fatal(err)
	}
	fingerprints := map[string]string{}
	for k, v := range vars {
		fingerprints[k] = srv.box.Fingerprint(secrets.EnvVarContext(app.ID, k), []byte(v))
	}
	if err := srv.store.SetDeploymentEnvFingerprints(ctx, d.ID, fingerprints); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
		t.Fatal(err)
	}
}

func TestEnvPendingChanges(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "A", "value": "1"}, {"key": "B", "value": "2"}]}`)

	// Nothing runs yet: nothing to compare with.
	got := decode[envListOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if got.Compared || len(got.pending()) != 0 || got.Removed == nil || len(got.Removed) != 0 {
		t.Errorf("before the first deploy: %+v", got)
	}

	deployWithEnv(t, srv, "blog", map[string]string{"A": "1", "B": "2", "OLD": "x"})
	request(t, h, http.MethodDelete, "/api/apps/blog/env/OLD", "")
	request(t, h, http.MethodPut, "/api/apps/blog/env/B", `{"value": "changed"}`)
	got = decode[envListOut](t, request(t, h, http.MethodPut, "/api/apps/blog/env/C", `{"value": "new"}`))
	if !got.Compared {
		t.Fatalf("not compared: %+v", got)
	}
	if p := got.pending(); len(p) != 2 || p["B"] != "changed" || p["C"] != "added" {
		t.Errorf("pending = %v", p)
	}
	if strings.Join(got.Removed, ",") != "OLD" {
		t.Errorf("removed = %v", got.Removed)
	}

	// Putting the deployed value back clears the difference.
	got = decode[envListOut](t, request(t, h, http.MethodPut, "/api/apps/blog/env/B", `{"value": "2"}`))
	if _, ok := got.pending()["B"]; ok {
		t.Errorf("B is still pending: %v", got.pending())
	}

	// After a deploy with the saved values there is nothing left.
	deployWithEnv(t, srv, "blog", map[string]string{"A": "1", "B": "2", "C": "new"})
	got = decode[envListOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if !got.Compared || len(got.pending()) != 0 || len(got.Removed) != 0 {
		t.Errorf("after deploying: %+v", got)
	}
}

func TestEnvPendingWithoutFingerprints(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "A", "value": "1"}]}`)
	// A release deployed before fingerprints were recorded.
	ctx := context.Background()
	app, _ := srv.store.GetApp(ctx, "blog")
	now := time.Now()
	d, _, _ := srv.store.EnqueueDeployment(ctx, app.ID, "main", testSHA, "", now)
	_ = srv.store.StartDeployment(ctx, d.ID, now)
	if err := srv.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
		t.Fatal(err)
	}
	got := decode[envListOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if got.Compared || len(got.pending()) != 0 {
		t.Errorf("legacy release: %+v", got)
	}
}

func TestEnvRefusesProvidedNames(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	for _, rec := range []*httptest.ResponseRecorder{
		request(t, h, http.MethodPut, "/api/apps/blog/env/KOROKU_COMMIT_SHA", `{"value": "x"}`),
		request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "KOROKU_ANYTHING", "value": "1"}]}`),
	} {
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "reserved") {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	}
}
