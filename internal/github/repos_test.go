package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// servePages answers a list endpoint from items, honoring page and per_page
// and announcing further pages with a Link header.
func servePages[T any](w http.ResponseWriter, r *http.Request, items []T, wrap func([]T) any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 || size < 1 {
		writeError(w, http.StatusUnprocessableEntity, "page and per_page are required by this fake")
		return
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	if end < len(items) {
		w.Header().Set("Link", fmt.Sprintf(
			`<https://elsewhere.example.test/next?page=%d>; rel="next", <https://elsewhere.example.test/last>; rel="last"`, page+1))
	} else if page > 1 {
		w.Header().Set("Link", `<https://elsewhere.example.test/first>; rel="first", <https://elsewhere.example.test/prev>; rel="prev"`)
	}
	if wrap == nil {
		writeJSON(w, http.StatusOK, items[start:end])
		return
	}
	writeJSON(w, http.StatusOK, wrap(items[start:end]))
}

func fakeCommit(i int) map[string]any {
	return map[string]any{
		"sha": fmt.Sprintf("%040x", i),
		"commit": map[string]any{
			"message":   fmt.Sprintf("Commit %d\n\nBody of commit %d.", i, i),
			"author":    map[string]any{"name": "Mona Lisa", "email": "ignored@example.test", "date": "2026-01-01T10:00:00Z"},
			"committer": map[string]any{"name": "Web Flow", "date": "2026-01-01T12:00:00Z"},
		},
	}
}

func TestListReposCoversEveryInstallationAndPage(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveInstallationTokens()
	f.serveRepoInstallations()

	suspendedAt := "2025-12-01T00:00:00Z"
	installations := []map[string]any{
		{"id": 11, "suspended_at": nil},
		{"id": 12, "suspended_at": suspendedAt},
		{"id": 13},
	}
	f.mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("installations per_page = %q", got)
		}
		// Force two pages regardless of the requested size.
		q := r.URL.Query()
		q.Set("per_page", "2")
		r.URL.RawQuery = q.Encode()
		servePages(w, r, installations, nil)
	})

	repos := map[int64][]map[string]any{}
	for i := range 150 {
		repos[11] = append(repos[11], map[string]any{
			"id": 1000 + i, "name": fmt.Sprintf("svc-%03d", i), "full_name": fmt.Sprintf("octo/svc-%03d", i),
			"private": i%2 == 0, "owner": map[string]any{"login": "octo"}, "default_branch": "main",
		})
	}
	repos[13] = []map[string]any{{
		"id": 9, "name": "App", "full_name": "Acme/App",
		"private": true, "owner": map[string]any{"login": "Acme"}, "default_branch": "trunk",
	}}
	f.mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		id, ok := f.installationOf(w, r)
		if !ok {
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("repositories per_page = %q", got)
		}
		servePages(w, r, repos[id], func(page []map[string]any) any {
			return map[string]any{"total_count": len(repos[id]), "repositories": page}
		})
	})
	c := f.client()

	got, err := c.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 151 {
		t.Fatalf("got %d repositories, want 151", len(got))
	}
	// Sorted by full name, case-insensitively: Acme/App comes first.
	if want := (Repo{ID: 9, FullName: "Acme/App", Owner: "Acme", Name: "App", Private: true, DefaultBranch: "trunk", InstallationID: 13}); got[0] != want {
		t.Errorf("first = %+v, want %+v", got[0], want)
	}
	if want := (Repo{ID: 1149, FullName: "octo/svc-149", Owner: "octo", Name: "svc-149", Private: false, DefaultBranch: "main", InstallationID: 11}); got[150] != want {
		t.Errorf("last = %+v, want %+v", got[150], want)
	}
	for i := 1; i < len(got); i++ {
		if strings.ToLower(got[i-1].FullName) >= strings.ToLower(got[i].FullName) {
			t.Fatalf("not sorted or duplicated at %d: %q, %q", i, got[i-1].FullName, got[i].FullName)
		}
	}

	if n := f.count("GET /app/installations?"); n != 2 {
		t.Errorf("installation pages = %d, want 2", n)
	}
	if n := f.count("GET /installation/repositories?"); n != 3 {
		t.Errorf("repository pages = %d, want 3 (two for installation 11, one for 13)", n)
	}
	if f.mintCount(11) != 1 || f.mintCount(13) != 1 || f.mintCount(12) != 0 {
		t.Errorf("token requests = %d/%d/%d, want 1/0/1 (suspended installations are skipped)",
			f.mintCount(11), f.mintCount(12), f.mintCount(13))
	}

	// Listing primes the installation cache: no /installation lookup follows.
	tok, err := c.RepoToken(context.Background(), "acme", "app")
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok.Value() != "ghs_fake_13_1" || f.count("GET /repos/") != 0 {
		t.Errorf("token %q after %d lookups, want the cached installation", tok.Value(), f.count("GET /repos/"))
	}
}

func TestListBranchesFollowsPages(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	var branches []map[string]any
	for i := range 120 {
		branches = append(branches, map[string]any{
			"name":   fmt.Sprintf("feature/topic-%03d", i),
			"commit": map[string]any{"sha": fmt.Sprintf("%040x", i)},
		})
	}
	f.mux.HandleFunc("GET /repos/octo/app/branches", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := f.installationOf(w, r); !ok || id != 7 {
			return
		}
		servePages(w, r, branches, nil)
	})

	got, err := f.client().ListBranches(context.Background(), "octo", "app")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("got %d branches, want 120", len(got))
	}
	if want := (Branch{Name: "feature/topic-119", SHA: fmt.Sprintf("%040x", 119)}); got[119] != want {
		t.Errorf("last = %+v, want %+v", got[119], want)
	}
	want := []string{
		"GET /repos/octo/app/branches?page=1&per_page=100",
		"GET /repos/octo/app/branches?page=2&per_page=100",
	}
	var seen []string
	for _, r := range f.seen() {
		if strings.Contains(r, "/branches") {
			seen = append(seen, r)
		}
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests = %q, want %q", seen, want)
	}
}

func TestPaginationStopsAtTheCap(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	f.mux.HandleFunc("GET /repos/octo/app/branches", func(w http.ResponseWriter, r *http.Request) {
		// A listing that never ends.
		w.Header().Set("Link", `<https://elsewhere.example.test/next>; rel="next"`)
		writeJSON(w, http.StatusOK, []map[string]any{{"name": "b" + r.URL.Query().Get("page"), "commit": map[string]any{"sha": "abc"}}})
	})

	got, err := f.client().ListBranches(context.Background(), "octo", "app")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(got) != maxPages || f.count("GET /repos/octo/app/branches") != maxPages {
		t.Errorf("got %d branches from %d requests, want %d", len(got), f.count("GET /repos/octo/app/branches"), maxPages)
	}
	for _, r := range f.seen() {
		if strings.Contains(r, "elsewhere") {
			t.Errorf("the Link target must not be followed: %s", r)
		}
	}
}

func TestListCommits(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	var commits []map[string]any
	for i := range 250 {
		commits = append(commits, fakeCommit(i))
	}
	var lastSHA, lastPerPage string
	hasSHA := false
	f.mux.HandleFunc("GET /repos/octo/app/commits", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		q := r.URL.Query()
		lastSHA, lastPerPage, hasSHA = q.Get("sha"), q.Get("per_page"), q.Has("sha")
		if len(q) > 3 {
			t.Errorf("unexpected query parameters: %v", q)
		}
		servePages(w, r, commits, nil)
	})
	c := f.client()
	ctx := context.Background()

	// A ref with characters that are special in a query string stays one value.
	const ref = "feature/a&per_page=1#x?y=z"
	got, err := c.ListCommits(ctx, "octo", "app", ref, 150)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(got) != 150 {
		t.Fatalf("got %d commits, want exactly the limit of 150", len(got))
	}
	if lastSHA != ref || lastPerPage != "100" {
		t.Errorf("sha = %q, per_page = %q", lastSHA, lastPerPage)
	}
	want := Commit{
		SHA:     fmt.Sprintf("%040x", 3),
		Message: "Commit 3\n\nBody of commit 3.",
		Author:  "Mona Lisa",
		Date:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	if !got[3].Date.Equal(want.Date) {
		t.Errorf("Date = %v, want the commit date %v", got[3].Date, want.Date)
	}
	got[3].Date = want.Date
	if got[3] != want {
		t.Errorf("commit = %+v, want %+v", got[3], want)
	}
	if n := f.count("GET /repos/octo/app/commits?"); n != 2 {
		t.Errorf("requests = %d, want 2 pages", n)
	}

	// A small limit shrinks the page; no ref means the default branch.
	got, err = c.ListCommits(ctx, "octo", "app", "", 5)
	if err != nil || len(got) != 5 {
		t.Fatalf("ListCommits(limit 5) = %d commits, %v", len(got), err)
	}
	if hasSHA || lastPerPage != "5" {
		t.Errorf("sha present = %v, per_page = %q; want no sha and per_page=5", hasSHA, lastPerPage)
	}

	// No limit falls back to the default.
	got, err = c.ListCommits(ctx, "octo", "app", "main", 0)
	if err != nil || len(got) != 30 {
		t.Fatalf("ListCommits(limit 0) = %d commits, %v", len(got), err)
	}
	if lastPerPage != "30" {
		t.Errorf("per_page = %q, want 30", lastPerPage)
	}
}

func TestResolveCommitEscapesTheRef(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	var gotRef string
	f.mux.HandleFunc("GET /repos/octo/app/commits/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		gotRef = r.PathValue("ref")
		if r.URL.RawQuery != "" {
			t.Errorf("the ref leaked into the query: %q", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, fakeCommit(77))
	})
	c := f.client()

	cases := map[string]string{
		"feature/foo":        "/repos/octo/app/commits/feature%2Ffoo",
		"release/2026/01":    "/repos/octo/app/commits/release%2F2026%2F01",
		"v1.0?ref=main#frag": "/repos/octo/app/commits/v1.0%3Fref=main%23frag",
		"100%/done":          "/repos/octo/app/commits/100%25%2Fdone",
		"0123abc":            "/repos/octo/app/commits/0123abc",
	}
	for ref, wantPath := range cases {
		t.Run(ref, func(t *testing.T) {
			commit, err := c.ResolveCommit(context.Background(), "octo", "app", ref)
			if err != nil {
				t.Fatalf("ResolveCommit: %v", err)
			}
			if commit.SHA != fmt.Sprintf("%040x", 77) || !strings.HasPrefix(commit.Message, "Commit 77") {
				t.Errorf("commit = %+v", commit)
			}
			if gotRef != ref {
				t.Errorf("GitHub saw ref %q, want %q", gotRef, ref)
			}
			requests := f.seen()
			if last := requests[len(requests)-1]; last != "GET "+wantPath {
				t.Errorf("request = %q, want %q", last, "GET "+wantPath)
			}
		})
	}
}

func TestResolveCommitUnknownRef(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newFakeGitHub(t).withRepo("octo/app", 7)
			f.mux.HandleFunc("GET /repos/octo/app/commits/{ref}", func(w http.ResponseWriter, r *http.Request) {
				writeError(w, status, "No commit found for SHA: "+r.PathValue("ref"))
			})
			_, err := f.client().ResolveCommit(context.Background(), "octo", "app", "no-such-branch")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != status {
				t.Errorf("err = %#v, want *APIError with status %d", err, status)
			}
			if !strings.Contains(err.Error(), "No commit found for SHA: no-such-branch") {
				t.Errorf("error %q should carry GitHub's message", err)
			}
		})
	}
}

func TestStatusCodesMapToTypedErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		header  http.Header
		is      []error
		isNot   []error
		message string
	}{
		{name: "404", status: 404, is: []error{ErrNotFound}, isNot: []error{ErrUnauthorized, ErrRateLimited}},
		{name: "401", status: 401, is: []error{ErrUnauthorized}, isNot: []error{ErrNotFound, ErrRateLimited}},
		{name: "403", status: 403, is: []error{ErrUnauthorized}, isNot: []error{ErrNotFound, ErrRateLimited}},
		{name: "403 primary rate limit", status: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}},
			is: []error{ErrRateLimited}, isNot: []error{ErrUnauthorized, ErrNotFound}},
		{name: "403 secondary rate limit", status: 403, header: http.Header{"Retry-After": {"60"}},
			is: []error{ErrRateLimited}, isNot: []error{ErrUnauthorized}},
		{name: "429", status: 429, is: []error{ErrRateLimited}, isNot: []error{ErrUnauthorized, ErrNotFound}},
		{name: "422 outside commit lookup", status: 422, isNot: []error{ErrNotFound, ErrUnauthorized, ErrRateLimited}},
		{name: "500", status: 500, isNot: []error{ErrNotFound, ErrUnauthorized, ErrRateLimited}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t).withRepo("octo/app", 7)
			f.mux.HandleFunc("GET /repos/octo/app/branches", func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				writeError(w, tc.status, "upstream says no\r\n  (line two)")
			})
			_, err := f.client().ListBranches(context.Background(), "octo", "app")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %#v, want *APIError", err)
			}
			if apiErr.Status != tc.status {
				t.Errorf("Status = %d, want %d", apiErr.Status, tc.status)
			}
			if strings.ContainsAny(apiErr.Message, "\r\n") || !strings.Contains(apiErr.Message, "upstream says no") {
				t.Errorf("Message = %q, want GitHub's message on one line", apiErr.Message)
			}
			for _, target := range tc.is {
				if !errors.Is(err, target) {
					t.Errorf("errors.Is(err, %v) = false", target)
				}
			}
			for _, target := range tc.isNot {
				if errors.Is(err, target) {
					t.Errorf("errors.Is(err, %v) = true", target)
				}
			}
			if !strings.Contains(err.Error(), "list branches of octo/app") {
				t.Errorf("error %q lacks context", err)
			}
			assertNoLeak(t, err, "ghs_fake_7_1")
		})
	}
}

func TestErrorMessagesAreTruncatedAndNonJSONBodiesDropped(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	f.mux.HandleFunc("GET /repos/octo/app/branches", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "x"+strings.Repeat("é", 5000))
	})
	f.mux.HandleFunc("GET /repos/octo/app/commits", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy error with " + r.Header.Get("Authorization") + "</html>"))
	})
	c := f.client()

	_, err := c.ListBranches(context.Background(), "octo", "app")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %#v", err)
	}
	if len(apiErr.Message) > maxErrorMessage+3 || !strings.HasSuffix(apiErr.Message, "...") {
		t.Errorf("Message has %d bytes, want at most %d", len(apiErr.Message), maxErrorMessage+3)
	}
	if strings.ContainsRune(apiErr.Message, '�') {
		t.Error("truncation split a multi-byte character")
	}

	_, err = c.ListCommits(context.Background(), "octo", "app", "main", 10)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || apiErr.Message != "Bad Gateway" {
		t.Errorf("err = %v, want the status text only", err)
	}
	assertNoLeak(t, err, "ghs_fake_7_1")
}

func TestInvalidNamesAreRejectedBeforeAnyRequest(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	c := f.client()
	ctx := context.Background()

	names := [][2]string{
		{"", "app"},
		{"octo", ""},
		{"octo/evil", "app"},
		{"..", "app"},
		{"octo", ".."},
		{"octo", "."},
		{"octo", "app/installation"},
		{"octo", "app?per_page=1"},
		{"octo", "app#frag"},
		{"octo", "app%2Fx"},
		{"oc to", "app"},
		{"octo\n", "app"},
		{"-octo", "app"},
		{strings.Repeat("a", 40), "app"},
		{"octo", strings.Repeat("a", 101)},
		{"octo", "アプリ"},
	}
	for _, n := range names {
		owner, repo := n[0], n[1]
		calls := map[string]func() error{
			"RepoInstallationID": func() error { _, err := c.RepoInstallationID(ctx, owner, repo); return err },
			"RepoToken":          func() error { _, err := c.RepoToken(ctx, owner, repo); return err },
			"ListBranches":       func() error { _, err := c.ListBranches(ctx, owner, repo); return err },
			"ListCommits":        func() error { _, err := c.ListCommits(ctx, owner, repo, "main", 1); return err },
			"ResolveCommit":      func() error { _, err := c.ResolveCommit(ctx, owner, repo, "main"); return err },
			"ListDir":            func() error { _, err := c.ListDir(ctx, owner, repo, "", "main"); return err },
			"ReadFile":           func() error { _, err := c.ReadFile(ctx, owner, repo, "compose.yaml", "main"); return err },
		}
		for method, call := range calls {
			if err := call(); !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("%s(%q, %q): err = %v, want ErrInvalidArgument", method, owner, repo, err)
			}
		}
	}

	refs := []string{".", "..", "a/../b", "main branch", "main\n", "x\x00y", strings.Repeat("r", 256)}
	for _, ref := range refs {
		calls := map[string]func() error{
			"ListCommits":   func() error { _, err := c.ListCommits(ctx, "octo", "app", ref, 1); return err },
			"ResolveCommit": func() error { _, err := c.ResolveCommit(ctx, "octo", "app", ref); return err },
			"ListDir":       func() error { _, err := c.ListDir(ctx, "octo", "app", "", ref); return err },
			"ReadFile":      func() error { _, err := c.ReadFile(ctx, "octo", "app", "compose.yaml", ref); return err },
		}
		for method, call := range calls {
			if err := call(); !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("%s(ref %q): err = %v, want ErrInvalidArgument", method, ref, err)
			}
		}
	}
	if _, err := c.ResolveCommit(ctx, "octo", "app", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("ResolveCommit(empty ref): err = %v, want ErrInvalidArgument", err)
	}

	if seen := f.seen(); len(seen) != 0 {
		t.Errorf("requests were sent for invalid input: %q", seen)
	}
}

func TestValidNamesAreAccepted(t *testing.T) {
	for _, n := range [][2]string{
		{"octo", "app"},
		{"Octo-Org", ".github"},
		{"o", "a.b_c-d"},
		{"managed_user", "App.js"},
		{strings.Repeat("a", 39), strings.Repeat("r", 100)},
	} {
		if err := validateRepo(n[0], n[1]); err != nil {
			t.Errorf("validateRepo(%q, %q): %v", n[0], n[1], err)
		}
	}
}

func TestSplitFullName(t *testing.T) {
	owner, repo, err := SplitFullName("octo/app.js")
	if err != nil || owner != "octo" || repo != "app.js" {
		t.Errorf("SplitFullName = %q, %q, %v", owner, repo, err)
	}
	for _, bad := range []string{"", "octo", "octo/", "/app", "octo/app/extra", "octo/..", "octo /app"} {
		if _, _, err := SplitFullName(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("SplitFullName(%q): err = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestCanceledContextIsReported(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	c := f.client()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.ListBranches(ctx, "octo", "app"); !errors.Is(err, context.Canceled) {
		t.Errorf("ListBranches: err = %v, want context.Canceled", err)
	}
	if _, err := c.ListRepos(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListRepos: err = %v, want context.Canceled", err)
	}
	if _, err := c.User(ctx, testUserToken); !errors.Is(err, context.Canceled) {
		t.Errorf("User: err = %v, want context.Canceled", err)
	}
}
