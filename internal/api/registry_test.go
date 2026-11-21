package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
)

func TestRegistryCredential(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	blog := f.app(t, "blog")
	const token = "ghp_registrysecret123"
	path := "/api/apps/blog/registry-credential"

	// Nothing set: the default registry is suggested.
	rec := f.owner.do(http.MethodGet, path, "", nil)
	got := decode[registryCredentialJSON](t, rec)
	if rec.Code != http.StatusOK || got.Configured || got.Registry != "ghcr.io" || got.UpdatedAt != nil {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}

	// A manager of the app sets it; the token is not in the response.
	rec = f.as[managerID].do(http.MethodPut, path,
		`{"registry":" GHCR.io ","username":"octo-bot","token":"`+token+`"}`, originHeader)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got = decode[registryCredentialJSON](t, rec)
	if !got.Configured || got.Registry != "ghcr.io" || got.Username != "octo-bot" || got.UpdatedAt == nil {
		t.Errorf("put: %+v", got)
	}
	cred, err := f.srv.store.GetRegistryCredential(ctx, blog.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cred.TokenEncrypted), token) {
		t.Error("the token is stored in plain text")
	}
	if plain, err := f.srv.box.Open(cred.TokenEncrypted, secrets.RegistryTokenContext(blog.ID)); err != nil || string(plain) != token {
		t.Errorf("stored token = %q, %v", plain, err)
	}

	// Every role that can see the app sees whether one is set, never the token.
	for _, id := range []int64{viewerID, deployerID, managerID} {
		rec := f.as[id].do(http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) || !strings.Contains(rec.Body.String(), `"configured":true`) {
			t.Errorf("get as %d: %d %s", id, rec.Code, rec.Body)
		}
	}
	// Below manage it cannot be changed or removed.
	for _, id := range []int64{viewerID, deployerID} {
		if rec := f.as[id].do(http.MethodPut, path, `{"registry":"ghcr.io","username":"x","token":"y"}`, originHeader); errorCode(t, rec) != "forbidden_role" {
			t.Errorf("put as %d: %d %s", id, rec.Code, rec.Body)
		}
		if rec := f.as[id].do(http.MethodDelete, path, "", originHeader); errorCode(t, rec) != "forbidden_role" {
			t.Errorf("delete as %d: %d %s", id, rec.Code, rec.Body)
		}
	}
	// A member without a role on the app does not learn it exists.
	if rec := f.as[strangerID].do(http.MethodGet, path, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("get as a stranger: %d %s", rec.Code, rec.Body)
	}

	// Without a token the saved one is kept.
	rec = f.owner.do(http.MethodPut, path, `{"registry":"registry.example.com:5000","username":"bot"}`, originHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("put without token: %d %s", rec.Code, rec.Body)
	}
	cred, _ = f.srv.store.GetRegistryCredential(ctx, blog.ID)
	if plain, _ := f.srv.box.Open(cred.TokenEncrypted, secrets.RegistryTokenContext(blog.ID)); string(plain) != token || cred.Registry != "registry.example.com:5000" {
		t.Errorf("after put without token: %+v %q", cred, plain)
	}

	// Validation; messages never echo the token.
	for _, body := range []string{
		`{"registry":"https://ghcr.io","username":"bot","token":"t"}`,
		`{"registry":"ghcr.io/octo","username":"bot","token":"t"}`,
		`{"registry":"ghcr.io","username":"","token":"t"}`,
		`{"registry":"ghcr.io","username":"a:b","token":"t"}`,
		`{"registry":"ghcr.io","username":"bot","token":"has space"}`,
	} {
		if rec := f.owner.do(http.MethodPut, path, body, originHeader); rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "has space") {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	// The other app has none, and a token is required to create one.
	if rec := f.owner.do(http.MethodPut, "/api/apps/shop/registry-credential", `{"registry":"ghcr.io","username":"bot"}`, originHeader); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"token"`) {
		t.Errorf("create without token: %d %s", rec.Code, rec.Body)
	}

	// Saving and removing need a recent login.
	f.clk.Advance(ReauthWindow + time.Minute)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if rec := f.owner.do(method, path, `{"registry":"ghcr.io","username":"bot","token":"new"}`, originHeader); errorCode(t, rec) != "reauth_required" {
			t.Errorf("%s without a recent login: %d %s", method, rec.Code, rec.Body)
		}
	}
	if rec := f.as[managerID].do(http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
		t.Errorf("get without a recent login: %d %s", rec.Code, rec.Body)
	}
	f.owner.login("code-owner")

	rec = f.owner.do(http.MethodDelete, path, "", originHeader)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"configured":true`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, path, "", originHeader); rec.Code != http.StatusNotFound {
		t.Errorf("delete again: %d %s", rec.Code, rec.Body)
	}
}
