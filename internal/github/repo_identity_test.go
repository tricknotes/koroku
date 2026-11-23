package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func fakeRepo(id int64, fullName string) map[string]any {
	owner, name, _ := strings.Cut(fullName, "/")
	return map[string]any{
		"id": id, "name": name, "full_name": fullName, "private": true,
		"owner": map[string]any{"login": owner}, "default_branch": "main",
	}
}

func TestGetRepo(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 11)
	f.mux.HandleFunc("GET /repos/octo/app", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, fakeRepo(1296269, "octo/app"))
	})
	f.mux.HandleFunc("GET /repos/octo/empty", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"full_name": "octo/empty"})
	})
	f.addRepo("octo/empty", 11)
	f.addRepo("octo/gone", 11)
	c := f.client()
	ctx := context.Background()

	got, err := c.GetRepo(ctx, "octo", "app")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	want := Repo{ID: 1296269, FullName: "octo/app", Owner: "octo", Name: "app", Private: true, DefaultBranch: "main"}
	if got != want {
		t.Errorf("GetRepo = %+v, want %+v", got, want)
	}

	// The App is not installed on the repository: no token, no request.
	if _, err := c.GetRepo(ctx, "someone", "public"); !errors.Is(err, ErrNotFound) {
		t.Errorf("not installed: err = %v, want ErrNotFound", err)
	}
	// Installed, but the repository is gone.
	if _, err := c.GetRepo(ctx, "octo", "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("gone: err = %v, want ErrNotFound", err)
	}
	// A response without an ID identifies nothing.
	if _, err := c.GetRepo(ctx, "octo", "empty"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("no id: err = %v, want an error that is not ErrNotFound", err)
	}
	if _, err := c.GetRepo(ctx, "octo", "a/b"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("invalid name: err = %v", err)
	}
}

// GitHub answers the old name of a renamed or transferred repository with
// 301 to /repositories/{id} on the same host. The client follows it with
// the credentials of the first request, for the installation lookup (app
// JWT) and for the repository (installation token), and the result is the
// repository under its new name with the same ID.
func TestGetRepoFollowsTheOldNameOfARenamedRepository(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveInstallationTokens()
	f.mux.HandleFunc("GET /repos/octo/old/installation", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/77/installation", http.StatusMovedPermanently)
	})
	f.mux.HandleFunc("GET /repositories/77/installation", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 11})
	})
	f.mux.HandleFunc("GET /repos/octo/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/77", http.StatusMovedPermanently)
	})
	f.mux.HandleFunc("GET /repositories/77", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, fakeRepo(77, "octo/new"))
	})
	c := f.client()

	got, err := c.GetRepo(context.Background(), "octo", "old")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if got.ID != 77 || got.FullName != "octo/new" {
		t.Errorf("GetRepo = %+v, want repository 77 as octo/new", got)
	}
	want := []string{
		"GET /repos/octo/old/installation",
		"GET /repositories/77/installation",
		"POST /app/installations/11/access_tokens",
		"GET /repos/octo/old",
		"GET /repositories/77",
	}
	if seen := f.seen(); strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests = %v, want %v", seen, want)
	}
}

// A redirect that leaves the API host does not take the token along.
func TestRedirectToAnotherHostDropsTheCredentials(t *testing.T) {
	var mu sync.Mutex
	var authorization []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorization = append(authorization, r.Header.Get("Authorization"))
		mu.Unlock()
		writeJSON(w, http.StatusOK, fakeRepo(99, "octo/elsewhere"))
	}))
	t.Cleanup(other.Close)
	// The same listener under another host name.
	elsewhere := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	if elsewhere == other.URL {
		t.Skipf("the test server does not listen on 127.0.0.1: %s", other.URL)
	}

	f := newFakeGitHub(t).withRepo("octo/moved", 11)
	f.mux.HandleFunc("GET /repos/octo/moved", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		http.Redirect(w, r, elsewhere+"/repositories/99", http.StatusMovedPermanently)
	})

	if _, err := f.client().GetRepo(context.Background(), "octo", "moved"); err != nil {
		t.Skipf("localhost does not reach the test server: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authorization) != 1 || authorization[0] != "" {
		t.Errorf("Authorization sent to the other host: %d request(s), %q", len(authorization), authorization)
	}
}

func TestGetRepoAnonymously(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /repos/octo/public", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
		writeJSON(w, http.StatusOK, fakeRepo(5, "octo/public"))
	})
	f.mux.HandleFunc("GET /repos/octo/private", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Not Found")
	})
	c, err := NewAnonymous(f.srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetRepo(context.Background(), "octo", "public")
	if err != nil || got.ID != 5 {
		t.Fatalf("GetRepo = %+v, %v", got, err)
	}
	if _, err := c.GetRepo(context.Background(), "octo", "private"); !errors.Is(err, ErrNotFound) {
		t.Errorf("private: err = %v, want ErrNotFound", err)
	}
}
