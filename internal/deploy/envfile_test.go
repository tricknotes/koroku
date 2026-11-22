package deploy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenderEnvFile(t *testing.T) {
	got, err := RenderEnvFile(map[string]string{
		"PLAIN":      "abc-123_./:@%+=,",
		"EMPTY":      "",
		"SPACES":     "hello world",
		"HASH":       "a #b",
		"DOLLAR":     "pa$$word ${HOME} $USER",
		"BACKSLASH":  `C:\path\n`,
		"DQUOTE":     `say "hi"`,
		"SQUOTE":     "it's $5",
		"MULTILINE":  "line1\nline2\r\n",
		"SQ_AND_ESC": `it's "quoted" \ and $x`,
		"_UNDER":     "1",
	})
	if err != nil {
		t.Fatalf("RenderEnvFile: %v", err)
	}
	want := strings.Join([]string{
		`BACKSLASH='C:\path\n'`,
		`DOLLAR='pa$$word ${HOME} $USER'`,
		`DQUOTE='say "hi"'`,
		`EMPTY=`,
		`HASH='a #b'`,
		`MULTILINE="line1\nline2\r\n"`,
		`PLAIN=abc-123_./:@%+=,`,
		`SPACES='hello world'`,
		`SQUOTE="it's \$5"`,
		`SQ_AND_ESC="it's \"quoted\" \\ and \$x"`,
		`_UNDER=1`,
		``,
	}, "\n")
	if got != want {
		t.Errorf("RenderEnvFile mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestParseEnvFileValuesRoundTrips(t *testing.T) {
	vars := map[string]string{
		"PLAIN":      "abc-123_./:@%+=,",
		"EMPTY":      "",
		"SPACES":     "hello world",
		"EQUALS":     "a=b=c",
		"DOLLAR":     "pa$$word ${HOME} $USER",
		"BACKSLASH":  `C:\path\n`,
		"SQUOTE":     "it's $5",
		"MULTILINE":  "line1\nline2\r\n",
		"SQ_AND_ESC": `it's "quoted" \ and $x \n`,
	}
	content, err := RenderEnvFile(vars)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range ParseEnvFileValues(content) {
		got[v] = true
	}
	for key, value := range vars {
		if !got[value] {
			t.Errorf("value of %s (%q) not read back; got %q", key, value, ParseEnvFileValues(content))
		}
	}
}

func TestRenderEnvFileRejectsBadInput(t *testing.T) {
	for _, key := range []string{"", "1ABC", "A-B", "A B", "A=B", "KEY\n", strings.Repeat("A", 256)} {
		if _, err := RenderEnvFile(map[string]string{key: "v"}); err == nil {
			t.Errorf("key %q must be rejected", key)
		}
		if ValidEnvKey(key) {
			t.Errorf("ValidEnvKey(%q) = true", key)
		}
	}
	if _, err := RenderEnvFile(map[string]string{"A": "x\x00y"}); err == nil {
		t.Error("a NUL byte must be rejected")
	}
	if got, err := RenderEnvFile(nil); err != nil || got != "" {
		t.Errorf("empty input: %q, %v", got, err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "env")
	if err := writeFileAtomic(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if err := writeFileAtomic(path, []byte("A=2\n"), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "A=2\n" || info.Mode().Perm() != 0o600 {
		t.Errorf("content = %q, mode = %04o", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestHTTPHealthChecker(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			if hits.Add(1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
			}
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/login":
			http.Redirect(w, r, "/nowhere", http.StatusFound)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	checker := HTTPHealthChecker{Interval: 5 * time.Millisecond}
	ctx := context.Background()

	if err := checker.Wait(ctx, srv.URL+"/flaky", time.Second); err != nil {
		t.Errorf("flaky endpoint should eventually pass: %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("attempts = %d, want 3", hits.Load())
	}
	// Anything below 500 proves the app is answering.
	for _, path := range []string{"/login", "/missing"} {
		if err := checker.Wait(ctx, srv.URL+path, time.Second); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	err := checker.Wait(ctx, srv.URL+"/broken", 60*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("broken endpoint: err = %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	err = checker.Wait(ctx, closed.URL, 60*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Errorf("refused connection: err = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := checker.Wait(canceled, srv.URL+"/broken", time.Second); err != context.Canceled {
		t.Errorf("canceled context: err = %v", err)
	}
}

func TestProvidedEnv(t *testing.T) {
	got := ProvidedEnv(Provided{CommitSHA: "0123456789abcdef0123456789abcdef01234567"})
	if got["KOROKU_COMMIT_SHA"] != "0123456789abcdef0123456789abcdef01234567" || got["KOROKU_COMMIT_SHORT"] != "0123456" || len(got) != 2 {
		t.Errorf("ProvidedEnv = %v", got)
	}
	got = ProvidedEnv(Provided{CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		AppURL: "https://shop-staging-pr-12.dev.example.com", PRNumber: 12})
	if got["KOROKU_APP_URL"] != "https://shop-staging-pr-12.dev.example.com" ||
		got["KOROKU_APP_HOST"] != "shop-staging-pr-12.dev.example.com" || got["KOROKU_PR_NUMBER"] != "12" {
		t.Errorf("ProvidedEnv of a review app = %v", got)
	}
	for k := range got {
		if !strings.HasPrefix(k, ProvidedEnvPrefix) {
			t.Errorf("%s does not start with %s", k, ProvidedEnvPrefix)
		}
	}
}
