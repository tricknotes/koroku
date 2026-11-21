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
