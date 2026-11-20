package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const (
	testCode      = "fake-oauth-code-abcdef"
	testUserToken = "ghu_fake_user_token_123456"
	testCallback  = "https://console.example.test/auth/github/callback"
)

func TestAuthorizeURL(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()

	raw := c.AuthorizeURL("state &=/?#", testCallback)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizeURL returned an invalid URL: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != f.srv.URL+"/login/oauth/authorize" {
		t.Errorf("endpoint = %q", got)
	}
	q := u.Query()
	if q.Get("client_id") != testClientID || q.Get("state") != "state &=/?#" || q.Get("redirect_uri") != testCallback {
		t.Errorf("query = %v", q)
	}
	if len(q) != 3 || u.Fragment != "" {
		t.Errorf("unexpected parameters in %q", raw)
	}
	if strings.Contains(raw, testClientSecret) {
		t.Error("the authorize URL must not carry the client secret")
	}

	if q := mustParseQuery(t, c.AuthorizeURL("s", "")); q.Has("redirect_uri") {
		t.Error("an empty redirect URI should be left out")
	}
}

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestExchangeCode(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("credentials belong in the body only: query %q", r.URL.RawQuery)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		want := url.Values{
			"client_id":     {testClientID},
			"client_secret": {testClientSecret},
			"code":          {testCode},
			"redirect_uri":  {testCallback},
		}
		if r.PostForm.Encode() != want.Encode() {
			t.Errorf("form = %v", r.PostForm)
		}
		writeJSON(w, http.StatusOK, map[string]string{"access_token": testUserToken, "token_type": "bearer", "scope": ""})
	})

	got, err := f.client().ExchangeCode(context.Background(), testCode, testCallback)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if got != testUserToken {
		t.Errorf("token = %q", got)
	}
}

func TestExchangeCodeErrorReportedWithStatus200(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSON(w, http.StatusOK, map[string]string{
			"error": "bad_verification_code",
			// Echo the submitted values to prove they are scrubbed.
			"error_description": "The code " + r.PostForm.Get("code") + " is incorrect or expired (" + r.PostForm.Get("client_secret") + ").",
			"error_uri":         "https://docs.example.test/oauth-errors",
		})
	})

	tok, err := f.client().ExchangeCode(context.Background(), testCode, testCallback)
	if err == nil || tok != "" {
		t.Fatalf("expected an error, got token %q, err %v", tok, err)
	}
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("err = %#v, want *OAuthError", err)
	}
	if oauthErr.Code != "bad_verification_code" || !strings.Contains(oauthErr.Description, "incorrect or expired") {
		t.Errorf("OAuthError = %+v", oauthErr)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Error("an OAuth error should match ErrUnauthorized")
	}
	assertNoLeak(t, err, testClientSecret, testCode)
}

func TestExchangeCodeFailuresDoNotLeakCredentials(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"HTTP 500 echoing the form": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			writeError(w, http.StatusInternalServerError, "cannot process "+r.PostForm.Encode()+" / "+r.PostForm.Get("client_secret"))
		},
		"HTML error page": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>" + r.PostForm.Get("client_secret") + r.PostForm.Get("code") + "</html>"))
		},
		"invalid JSON with a token": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"` + testUserToken + `"`))
		},
		"no token in the answer": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"token_type": "bearer"})
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("POST /login/oauth/access_token", handler)
			_, err := f.client().ExchangeCode(context.Background(), testCode, testCallback)
			assertNoLeak(t, err, testClientSecret, url.QueryEscape(testClientSecret), testCode, testUserToken)
		})
	}

	t.Run("unreachable server", func(t *testing.T) {
		f := newFakeGitHub(t)
		c := f.client()
		f.srv.Close()
		_, err := c.ExchangeCode(context.Background(), testCode, testCallback)
		assertNoLeak(t, err, testClientSecret, testCode)
	})

	t.Run("empty code", func(t *testing.T) {
		f := newFakeGitHub(t)
		if _, err := f.client().ExchangeCode(context.Background(), "", testCallback); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("err = %v, want ErrInvalidArgument", err)
		}
		if n := len(f.seen()); n != 0 {
			t.Errorf("%d requests were sent", n)
		}
	})
}

func TestUser(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testUserToken {
			writeError(w, http.StatusUnauthorized, "Bad credentials for "+got)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         583231,
			"login":      "octo",
			"avatar_url": "https://avatars.example.test/u/583231",
			"email":      nil,
		})
	})
	c := f.client()

	user, err := c.User(context.Background(), testUserToken)
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if user.ID != 583231 || user.Login != "octo" || user.AvatarURL != "https://avatars.example.test/u/583231" {
		t.Errorf("User = %+v", user)
	}

	const revoked = "ghu_fake_revoked_token_987654"
	_, err = c.User(context.Background(), revoked)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("err = %#v, want *APIError with status 401", err)
	}
	assertNoLeak(t, err, revoked)

	if _, err := c.User(context.Background(), ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("empty token: err = %v, want ErrInvalidArgument", err)
	}
}
