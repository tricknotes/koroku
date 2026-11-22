package github

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func fakePull(number int, headRepo any, merged bool) map[string]any {
	p := map[string]any{
		"number": number, "title": "Add search", "state": "open", "html_url": "https://github.com/octo/app/pull/1",
		"user":       map[string]any{"login": "mona"},
		"updated_at": "2026-11-01T10:00:00Z",
		"head":       map[string]any{"ref": "search", "sha": "0123456789abcdef0123456789abcdef01234567", "repo": headRepo},
		"base":       map[string]any{"ref": "main", "repo": map[string]any{"id": 5}},
	}
	if merged {
		p["state"] = "closed"
		p["merged_at"] = "2026-11-02T10:00:00Z"
	}
	return p
}

func TestListPullRequests(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	f.mux.HandleFunc("GET /repos/octo/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := f.installationOf(w, r); !ok || id != 7 {
			return
		}
		if q := r.URL.Query(); q.Get("state") != "open" || q.Get("sort") != "updated" {
			t.Errorf("query = %v", q)
		}
		servePages(w, r, []map[string]any{
			fakePull(3, map[string]any{"id": 5}, false),
			fakePull(4, map[string]any{"id": 99}, false),
			fakePull(5, nil, false),
		}, nil)
	})
	got, err := f.client().ListPullRequests(context.Background(), "octo", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Number != 3 || got[0].FromFork || got[0].HeadBranch != "search" ||
		got[0].Author != "mona" || got[0].State != "open" {
		t.Fatalf("pull requests = %+v", got)
	}
	if !got[1].FromFork || !got[2].FromFork {
		t.Errorf("forks not recognized: %+v", got[1:])
	}
}

func TestGetPullRequest(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	f.mux.HandleFunc("GET /repos/octo/app/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationOf(w, r); !ok {
			return
		}
		switch r.PathValue("n") {
		case "8":
			writeJSON(w, http.StatusOK, fakePull(8, map[string]any{"id": 5}, true))
		default:
			writeError(w, http.StatusForbidden, "Resource not accessible by integration")
		}
	})
	c := f.client()
	pr, err := c.GetPullRequest(context.Background(), "octo", "app", 8)
	if err != nil || pr.State != "merged" || pr.FromFork {
		t.Fatalf("GetPullRequest = %+v, %v", pr, err)
	}
	if _, err := c.GetPullRequest(context.Background(), "octo", "app", 9); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("without the permission: err = %v", err)
	}
	if _, err := c.GetPullRequest(context.Background(), "octo", "app", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("number 0: err = %v", err)
	}
}
