package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

const (
	testAppID        = 4242
	testClientID     = "Iv1.fakeclientid"
	testClientSecret = "fake-client-secret-0123456789"
)

var testEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testKey is a throwaway RSA key generated once per test binary. No key
// material is stored in the repository.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// fakeGitHub is an in-process stand-in for api.github.com and github.com.
// Tests register the endpoints they need on mux.
type fakeGitHub struct {
	t     *testing.T
	mux   *http.ServeMux
	srv   *httptest.Server
	clock *clock.Fake

	mu         sync.Mutex
	requests   []string         // "METHOD /escaped/path?query" in arrival order
	lastBearer string           // credential of the most recent request
	issued     map[string]int64 // installation token -> installation id
	mints      map[int64]int    // installation id -> tokens issued
	installs   map[string]int64 // lower-cased "owner/repo" -> installation id
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:        t,
		mux:      http.NewServeMux(),
		clock:    clock.NewFake(testEpoch),
		issued:   make(map[string]int64),
		mints:    make(map[int64]int),
		installs: make(map[string]int64),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.lastBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Unlock()

		if got := r.Header.Get("User-Agent"); got != "paasd" {
			t.Errorf("%s %s: User-Agent = %q, want paasd", r.Method, r.URL.Path, got)
		}
		if r.URL.Path == "/login/oauth/access_token" {
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Errorf("OAuth exchange: Accept = %q", got)
			}
		} else {
			if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("%s %s: Accept = %q", r.Method, r.URL.Path, got)
			}
			if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
				t.Errorf("%s %s: X-GitHub-Api-Version = %q", r.Method, r.URL.Path, got)
			}
		}
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a Client wired to the fake server and the fake clock.
func (f *fakeGitHub) client() *Client {
	f.t.Helper()
	c, err := New(Config{
		AppID:        testAppID,
		PrivateKey:   testKey(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		APIBaseURL:   f.srv.URL,
		WebBaseURL:   f.srv.URL,
		Clock:        f.clock,
	})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return c
}

// serveInstallationTokens registers the token endpoint. It requires a valid
// app JWT and issues tokens that expire one hour after the fake clock's now.
func (f *fakeGitHub) serveInstallationTokens() {
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		f.mu.Lock()
		f.mints[id]++
		value := fmt.Sprintf("ghs_fake_%d_%d", id, f.mints[id])
		f.issued[value] = id
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      value,
			"expires_at": f.clock.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
}

// serveRepoInstallations registers GET /repos/{owner}/{repo}/installation,
// answering from the repositories added with addRepo.
func (f *fakeGitHub) serveRepoInstallations() {
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		f.mu.Lock()
		id, ok := f.installs[strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("repo"))]
		f.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id})
	})
}

// withRepo sets the fake up so installation tokens for fullName can be minted.
func (f *fakeGitHub) withRepo(fullName string, installationID int64) *fakeGitHub {
	f.serveInstallationTokens()
	f.serveRepoInstallations()
	f.addRepo(fullName, installationID)
	return f
}

func (f *fakeGitHub) addRepo(fullName string, installationID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs[strings.ToLower(fullName)] = installationID
}

// requireAppJWT answers 401 unless the request carries a JWT signed by testKey
// that is valid at the fake clock's time.
func (f *fakeGitHub) requireAppJWT(w http.ResponseWriter, r *http.Request) bool {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := verifyJWT(bearer, &testKey().PublicKey)
	if err == nil {
		now := f.clock.Now().Unix()
		if claims.Issuer != strconv.Itoa(testAppID) || claims.IssuedAt > now || claims.ExpiresAt <= now {
			err = errors.New("claims rejected")
		}
	}
	if err != nil {
		f.t.Errorf("%s %s: app JWT rejected: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	return true
}

// installationOf returns the installation whose token authenticates r, or
// answers 401.
func (f *fakeGitHub) installationOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	id, ok := f.issued[bearer]
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("%s %s: request without a valid installation token", r.Method, r.URL.Path)
		writeError(w, http.StatusUnauthorized, "Bad credentials")
	}
	return id, ok
}

// seen returns the requests received so far.
func (f *fakeGitHub) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// count returns how many requests started with prefix ("METHOD /path").
func (f *fakeGitHub) count(prefix string) int {
	n := 0
	for _, r := range f.seen() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) mintCount(installationID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints[installationID]
}

func (f *fakeGitHub) bearer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBearer
}

type jwtClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

// verifyJWT checks the RS256 signature of token with pub and returns its claims.
func verifyJWT(token string, pub *rsa.PublicKey) (jwtClaims, error) {
	var claims jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, fmt.Errorf("JWT has %d parts, want 3", len(parts))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, fmt.Errorf("header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return claims, fmt.Errorf("header: %w", err)
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		return claims, fmt.Errorf("header = %+v, want RS256 / JWT", header)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, fmt.Errorf("signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], signature); err != nil {
		return claims, fmt.Errorf("signature: %w", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, fmt.Errorf("claims: %w", err)
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return claims, fmt.Errorf("claims: %w", err)
	}
	return claims, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// assertNoLeak fails when any rendering of err contains one of the secrets.
func assertNoLeak(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	rendered := []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)}
	for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
		rendered = append(rendered, fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e))
	}
	for _, s := range secrets {
		if s == "" {
			t.Fatal("assertNoLeak called with an empty secret")
		}
		for _, text := range rendered {
			if strings.Contains(text, s) {
				t.Errorf("error leaks a secret: %s", strings.ReplaceAll(text, s, "<SECRET>"))
			}
		}
	}
}
