package github

import (
	"errors"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	// The example from GitHub's documentation on validating deliveries.
	const documented = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got := Sign(secret, body); got != documented {
		t.Fatalf("Sign = %s", got)
	}
	if err := VerifySignature(secret, body, documented); err != nil {
		t.Errorf("valid: %v", err)
	}
	bad := map[string]string{
		"missing":      "",
		"no prefix":    documented[len("sha256="):],
		"sha1":         "sha1=757107ea0eb2509fc211221cce984b8a37570b6d",
		"not hex":      "sha256=zz",
		"short":        "sha256=757107ea",
		"other digest": "sha256=" + "00" + documented[len("sha256=")+2:],
	}
	for name, header := range bad {
		if err := VerifySignature(secret, body, header); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := VerifySignature(secret, []byte("Hello, World?"), documented); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed body: %v", err)
	}
	if err := VerifySignature(nil, body, Sign(nil, body)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("empty secret: %v", err)
	}
}

func TestParsePushEvent(t *testing.T) {
	ev, err := ParsePushEvent([]byte(`{"ref": "refs/heads/main", "after": "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		"repository": {"id": 5, "full_name": "octo/blog"}, "pusher": {"name": "octo-git"}, "sender": {"login": "octo"},
		"head_commit": {"id": "abcdef0123456789abcdef0123456789abcdef01", "message": "Fix"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if branch, ok := ev.Branch(); !ok || branch != "main" || ev.RemovesRef() {
		t.Errorf("branch = %q %v", branch, ok)
	}
	if ev.After != "abcdef0123456789abcdef0123456789abcdef01" || ev.RepoID != 5 || ev.RepoFullName != "octo/blog" ||
		ev.Pusher != "octo" || ev.HeadMessage != "Fix" {
		t.Errorf("%+v", ev)
	}

	tag, _ := ParsePushEvent([]byte(`{"ref": "refs/tags/v1", "after": "a", "repository": {"id": 5}, "pusher": {"name": "octo-git"}}`))
	if _, ok := tag.Branch(); ok || tag.Pusher != "octo-git" {
		t.Errorf("tag: %+v", tag)
	}
	deleted, _ := ParsePushEvent([]byte(`{"ref": "refs/heads/x", "after": "0000000000000000000000000000000000000000", "deleted": true, "repository": {"id": 5}}`))
	if !deleted.RemovesRef() {
		t.Error("a deletion is not reported")
	}
	// The head commit's message is used only when it is the pushed commit.
	other, _ := ParsePushEvent([]byte(`{"ref": "refs/heads/x", "after": "b", "repository": {"id": 5}, "head_commit": {"id": "c", "message": "m"}}`))
	if other.HeadMessage != "" {
		t.Errorf("message = %q", other.HeadMessage)
	}
	for _, body := range []string{`{`, `{"ref": "refs/heads/main"}`, `{"repository": {"id": 1}}`} {
		if _, err := ParsePushEvent([]byte(body)); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
}

func TestParseWorkflowRunEvent(t *testing.T) {
	body := `{"action": "completed",
		"workflow": {"id": 7, "path": ".github/workflows/image.yml"},
		"workflow_run": {"id": 9, "path": ".github/workflows/image.yml@refs/heads/main",
			"head_branch": "main", "head_sha": "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
			"conclusion": "success", "event": "workflow_run",
			"html_url": "https://github.com/octo/blog/actions/runs/9",
			"head_repository": {"id": 5, "full_name": "octo/blog"},
			"actor": {"login": "octo"}},
		"repository": {"id": 5, "full_name": "octo/blog"},
		"sender": {"login": "octo-bot"}}`
	ev, err := ParseWorkflowRunEvent([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkflowRunEvent{
		Action: "completed", Conclusion: "success", WorkflowPath: ".github/workflows/image.yml",
		HeadBranch: "main", HeadSHA: "abcdef0123456789abcdef0123456789abcdef01",
		RepoID: 5, RepoFullName: "octo/blog", HeadRepoID: 5, HeadRepoFullName: "octo/blog",
		Actor: "octo", RunURL: "https://github.com/octo/blog/actions/runs/9",
	}
	if ev != want {
		t.Errorf("got %+v\nwant %+v", ev, want)
	}
	if !ev.FromHeadRepository() {
		t.Error("the run's own repository was taken for a fork")
	}

	// Without the workflow object the run's path is used, normalized; a
	// fork is told apart; the sender stands in for a missing actor.
	fork, err := ParseWorkflowRunEvent([]byte(`{"action": "completed",
		"workflow_run": {"path": "octo/blog/.github/workflows/ci.yaml@refs/pull/3/merge", "head_branch": "main",
			"head_repository": {"id": 6, "full_name": "mallory/blog"}},
		"repository": {"id": 5, "full_name": "octo/blog"}, "sender": {"login": "mallory"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if fork.WorkflowPath != ".github/workflows/ci.yaml" || fork.FromHeadRepository() || fork.Actor != "mallory" {
		t.Errorf("fork = %+v", fork)
	}

	for _, body := range []string{`{`, `{"action": "completed", "repository": {"id": 5}}`, `{"workflow_run": {}}`} {
		if _, err := ParseWorkflowRunEvent([]byte(body)); err == nil {
			t.Errorf("%s was accepted", body)
		}
	}
}

func TestWorkflowFilePath(t *testing.T) {
	for in, want := range map[string]string{
		".github/workflows/image.yml":                 ".github/workflows/image.yml",
		".github/workflows/image.yml@refs/heads/main": ".github/workflows/image.yml",
		"octo/blog/.github/workflows/image.yml@v1":    ".github/workflows/image.yml",
		"dynamic/pages/pages-build-deployment":        "dynamic/pages/pages-build-deployment",
	} {
		if got := WorkflowFilePath(in); got != want {
			t.Errorf("WorkflowFilePath(%q) = %q, want %q", in, got, want)
		}
	}
}
