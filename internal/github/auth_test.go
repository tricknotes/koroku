package github

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePrivateKey(t *testing.T) {
	key := testKey()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"PKCS#1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
		"PKCS#8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePrivateKey(pem.EncodeToMemory(block))
			if err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
			if !got.Equal(key) {
				t.Error("parsed key differs from the original")
			}
		})
	}
}

func TestParsePrivateKeyRejectsBadInput(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	garbage := []byte("not-a-key-but-must-not-be-echoed")
	cases := map[string][]byte{
		"empty":            nil,
		"not PEM":          garbage,
		"garbage PKCS#1":   pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: garbage}),
		"garbage PKCS#8":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: garbage}),
		"non-RSA PKCS#8":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"unsupported type": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: garbage}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			key, err := ParsePrivateKey(input)
			if err == nil || key != nil {
				t.Fatalf("expected an error, got key=%v err=%v", key != nil, err)
			}
			if strings.Contains(err.Error(), string(garbage)) {
				t.Errorf("error echoes the input: %v", err)
			}
		})
	}
}

func TestNewValidatesConfig(t *testing.T) {
	valid := Config{AppID: 1, PrivateKey: testKey(), ClientID: "id", ClientSecret: "secret"}
	if _, err := New(valid); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
	cases := map[string]func(*Config){
		"zero app id":       func(c *Config) { c.AppID = 0 },
		"nil key":           func(c *Config) { c.PrivateKey = nil },
		"empty client id":   func(c *Config) { c.ClientID = "" },
		"empty secret":      func(c *Config) { c.ClientSecret = "" },
		"relative api base": func(c *Config) { c.APIBaseURL = "api.github.com" },
		"api base query":    func(c *Config) { c.APIBaseURL = "https://api.example.test/?x=1" },
		"web base userinfo": func(c *Config) { c.WebBaseURL = "https://user:pw@example.test" },
		"web base scheme":   func(c *Config) { c.WebBaseURL = "ftp://example.test" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(Config{AppID: 1, PrivateKey: testKey(), ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.apiBase != "https://api.github.com" || c.webBase != "https://github.com" {
		t.Errorf("base URLs = %q, %q", c.apiBase, c.webBase)
	}
	if c.http == nil || c.http.Timeout <= 0 {
		t.Error("the default HTTP client must have a timeout")
	}
	if c.clock == nil {
		t.Error("the default clock is missing")
	}

	c, err = New(Config{AppID: 1, PrivateKey: testKey(), ClientID: "id", ClientSecret: "secret",
		APIBaseURL: "https://ghe.example.test/api/v3/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.apiBase != "https://ghe.example.test/api/v3" {
		t.Errorf("apiBase = %q, the trailing slash should be trimmed", c.apiBase)
	}
}

func TestAppJWTClaimsAndSignature(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveInstallationTokens()
	c := f.client()

	if _, err := c.InstallationToken(context.Background(), 7); err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	claims, err := verifyJWT(f.bearer(), &testKey().PublicKey)
	if err != nil {
		t.Fatalf("the JWT sent to GitHub does not verify: %v", err)
	}
	now := testEpoch.Unix()
	if claims.Issuer != "4242" {
		t.Errorf("iss = %q, want the app id", claims.Issuer)
	}
	if claims.IssuedAt != now-60 {
		t.Errorf("iat = %d, want 60s before now (%d)", claims.IssuedAt, now-60)
	}
	if claims.ExpiresAt <= now || claims.ExpiresAt-now > 600 {
		t.Errorf("exp = now%+ds, want within (0, 600]", claims.ExpiresAt-now)
	}

	// A signature made with another key must not verify (guards the helper).
	other := rsa.PublicKey{N: new(big.Int).Add(testKey().N, big.NewInt(2)), E: testKey().E}
	if _, err := verifyJWT(f.bearer(), &other); err == nil {
		t.Error("verifyJWT accepted a signature under a different key")
	}
}

func TestInstallationTokenIsCachedUntilFiveMinutesBeforeExpiry(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveInstallationTokens()
	c := f.client()
	ctx := context.Background()

	first, err := c.InstallationToken(ctx, 7)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if first.Value() != "ghs_fake_7_1" {
		t.Errorf("token = %q", first.Value())
	}
	if want := testEpoch.Add(time.Hour); !first.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", first.ExpiresAt, want)
	}

	// Still more than five minutes left: served from the cache.
	f.clock.Advance(55*time.Minute - time.Second)
	again, err := c.InstallationToken(ctx, 7)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if again.Value() != first.Value() || f.mintCount(7) != 1 {
		t.Errorf("expected the cached token, got %q after %d requests", again.Value(), f.mintCount(7))
	}

	// Exactly five minutes left: refreshed.
	f.clock.Advance(time.Second)
	refreshed, err := c.InstallationToken(ctx, 7)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if refreshed.Value() != "ghs_fake_7_2" || f.mintCount(7) != 2 {
		t.Errorf("expected a fresh token, got %q after %d requests", refreshed.Value(), f.mintCount(7))
	}
	if want := f.clock.Now().Add(time.Hour); !refreshed.ExpiresAt.Equal(want) {
		t.Errorf("refreshed ExpiresAt = %v, want %v", refreshed.ExpiresAt, want)
	}

	// Each installation has its own token.
	other, err := c.InstallationToken(ctx, 8)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if other.Value() != "ghs_fake_8_1" || f.mintCount(7) != 2 {
		t.Errorf("installation 8 got %q; installation 7 minted %d times", other.Value(), f.mintCount(7))
	}
}

func TestInstallationTokenConcurrentCallersShareOneRequest(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveInstallationTokens()
	c := f.client()

	const callers = 32
	values := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := c.InstallationToken(context.Background(), int64(1+i%2))
			values[i], errs[i] = tok.Value(), err
		}()
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if want := fmt.Sprintf("ghs_fake_%d_1", 1+i%2); values[i] != want {
			t.Errorf("caller %d got %q, want %q", i, values[i], want)
		}
	}
	if f.mintCount(1) != 1 || f.mintCount(2) != 1 {
		t.Errorf("token requests = %d and %d, want 1 each", f.mintCount(1), f.mintCount(2))
	}
}

func TestInstallationTokenWaiterHonorsContext(t *testing.T) {
	f := newFakeGitHub(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before the server is closed

	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      "ghs_fake_slow",
			"expires_at": f.clock.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	c := f.client()

	type result struct {
		tok Token
		err error
	}
	holder := make(chan result, 1)
	go func() {
		tok, err := c.InstallationToken(context.Background(), 7)
		holder <- result{tok, err}
	}()
	<-entered // the first caller now holds the slot

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.InstallationToken(ctx, 7); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting caller: err = %v, want context.DeadlineExceeded", err)
	}

	unblock()
	got := <-holder
	if got.err != nil || got.tok.Value() != "ghs_fake_slow" {
		t.Errorf("first caller: token %q, err %v", got.tok.Value(), got.err)
	}
}

func TestInstallationTokenFailureDoesNotLeakCredentials(t *testing.T) {
	f := newFakeGitHub(t)
	var mu sync.Mutex
	fail := true
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failing := fail
		mu.Unlock()
		if failing {
			// A hostile or buggy upstream echoes the credential it received.
			writeError(w, http.StatusUnauthorized, "Bad credentials: "+r.Header.Get("Authorization"))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      "ghs_fake_recovered",
			"expires_at": f.clock.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	c := f.client()

	_, err := c.InstallationToken(context.Background(), 7)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %#v, want *APIError with status 401", err)
	}
	if !strings.Contains(apiErr.Message, "[REDACTED]") {
		t.Errorf("Message = %q, the echoed credential should be replaced", apiErr.Message)
	}
	jwt := f.bearer()
	if strings.Count(jwt, ".") != 2 {
		t.Fatalf("the fake did not capture a JWT: %q", jwt)
	}
	assertNoLeak(t, err, jwt, strings.Split(jwt, ".")[2], testClientSecret)

	// The failure is not cached.
	mu.Lock()
	fail = false
	mu.Unlock()
	tok, err := c.InstallationToken(context.Background(), 7)
	if err != nil || tok.Value() != "ghs_fake_recovered" {
		t.Errorf("after recovery: token %q, err %v", tok.Value(), err)
	}
}

func TestInstallationTokenRejectsMalformedResponses(t *testing.T) {
	const leaked = "ghs_fake_in_broken_body"
	bodies := map[string]string{
		"no token":     `{"expires_at":"2026-01-02T04:04:05Z"}`,
		"no expiry":    `{"token":"` + leaked + `"}`,
		"bad expiry":   `{"token":"` + leaked + `","expires_at":"soon"}`,
		"invalid JSON": `{"token":"` + leaked + `",`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			})
			tok, err := f.client().InstallationToken(context.Background(), 7)
			if err == nil {
				t.Fatalf("expected an error, got token %v", tok)
			}
			assertNoLeak(t, err, leaked)
		})
	}
}

func TestInstallationTokenRejectsInvalidID(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	for _, id := range []int64{0, -1} {
		if _, err := c.InstallationToken(context.Background(), id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("id %d: err = %v, want ErrInvalidArgument", id, err)
		}
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

func TestRepoTokenLooksUpInstallationAndCachesIt(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	c := f.client()
	ctx := context.Background()

	tok, err := c.RepoToken(ctx, "octo", "app")
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok.Value() != "ghs_fake_7_1" {
		t.Errorf("token = %q", tok.Value())
	}
	id, err := c.RepoInstallationID(ctx, "octo", "app")
	if err != nil || id != 7 {
		t.Errorf("RepoInstallationID = %d, %v", id, err)
	}
	lookups := f.count("GET /repos/octo/app/installation")

	// Within the TTL neither the lookup nor the token is requested again,
	// whatever the letter case of the name.
	f.clock.Advance(9 * time.Minute)
	if _, err := c.RepoToken(ctx, "Octo", "App"); err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if got := f.count("GET /repos/"); got != lookups {
		t.Errorf("installation lookups = %d, want %d (cached)", got, lookups)
	}
	if f.mintCount(7) != 1 {
		t.Errorf("token requests = %d, want 1", f.mintCount(7))
	}

	// After the TTL the installation is looked up again.
	f.clock.Advance(2 * time.Minute)
	if _, err := c.RepoToken(ctx, "octo", "app"); err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if got := f.count("GET /repos/octo/app/installation"); got != lookups+1 {
		t.Errorf("installation lookups = %d, want %d", got, lookups+1)
	}
}

func TestRepoTokenForUninstalledRepository(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	c := f.client()

	_, err := c.RepoToken(context.Background(), "octo", "elsewhere")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "octo/elsewhere") {
		t.Errorf("error %q should name the repository", err)
	}
	if f.mintCount(7) != 0 {
		t.Error("no token should be requested when the lookup fails")
	}
}

func TestTokenNeverPrintsItsValue(t *testing.T) {
	const value = "ghs_fake_super_secret_value"
	tok := newToken(value, testEpoch.Add(time.Hour))
	if tok.Value() != value || tok.IsZero() {
		t.Fatalf("Value() = %q, IsZero() = %v", tok.Value(), tok.IsZero())
	}
	if !(Token{}).IsZero() || (Token{}).Value() != "" {
		t.Error("the zero Token should be empty")
	}

	type wrapper struct {
		Name  string
		Token Token
		inner Token
		ptr   *Token
	}
	w := wrapper{Name: "deploy", Token: tok, inner: tok, ptr: &tok}

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("minted", "token", tok, "wrapper", w, "pointer", &tok)
	slog.New(slog.NewTextHandler(&logged, nil)).Info("minted", "token", tok, "wrapper", w)

	encoded, err := json.Marshal(map[string]any{"token": tok, "wrapper": w})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	outputs := []string{
		tok.String(),
		fmt.Sprint(tok),
		fmt.Errorf("fetch with %v failed", tok).Error(),
		logged.String(),
		string(encoded),
	}
	// Every verb on every shape, including ones vet would flag for a constant format.
	for _, v := range []any{tok, &tok, w, &w, []Token{tok}, map[string]Token{"t": tok}} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
			outputs = append(outputs, fmt.Sprintf(format, v))
		}
	}
	for _, out := range outputs {
		if strings.Contains(out, value) || strings.Contains(strings.ToLower(out), fmt.Sprintf("%x", value)) {
			t.Errorf("token value leaked: %s", strings.ReplaceAll(out, value, "<SECRET>"))
		}
	}
	if got := fmt.Sprintf("%v", tok); got != "[REDACTED]" {
		t.Errorf("%%v = %q", got)
	}
}

func TestConfigAndClientNeverPrintSecrets(t *testing.T) {
	cfg := Config{AppID: testAppID, PrivateKey: testKey(), ClientID: testClientID, ClientSecret: testClientSecret}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("starting", "config", cfg, "client", c)

	outputs := []string{
		fmt.Sprintf("%v|%+v|%#v|%s", cfg, cfg, cfg, cfg),
		fmt.Sprintf("%v|%+v|%#v", &cfg, &cfg, &cfg),
		fmt.Sprintf("%v|%+v|%#v|%s", c, c, c, c),
		logged.String(),
	}
	secrets := []string{testClientSecret, testKey().D.String(), testKey().Primes[0].String()}
	for _, out := range outputs {
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("secret leaked: %s", strings.ReplaceAll(out, s, "<SECRET>"))
			}
		}
	}
	if !strings.Contains(outputs[0], "4242") {
		t.Errorf("Config should still identify the app: %s", outputs[0])
	}
}
