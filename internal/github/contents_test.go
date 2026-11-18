package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// wrapBase64 encodes data the way the contents API does: base64 broken into
// 60 character lines.
func wrapBase64(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(encoded) > 60 {
		b.WriteString(encoded[:60])
		b.WriteByte('\n')
		encoded = encoded[60:]
	}
	b.WriteString(encoded)
	b.WriteByte('\n')
	return b.String()
}

func fileEntry(name string, data []byte) map[string]any {
	return map[string]any{
		"type": "file", "name": name, "path": name, "size": len(data),
		"encoding": "base64", "content": wrapBase64(data),
	}
}

func lastRequest(f *fakeGitHub) string {
	seen := f.seen()
	if len(seen) == 0 {
		return ""
	}
	return seen[len(seen)-1]
}

func TestListDir(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	listing := []map[string]any{
		{"type": "dir", "name": "deploy", "path": "deploy"},
		{"type": "file", "name": "compose.yaml", "path": "compose.yaml", "size": 120},
		{"type": "file", "name": "Dockerfile", "path": "Dockerfile", "size": 300},
		{"type": "symlink", "name": "docker-compose.yml", "path": "docker-compose.yml"},
		{"type": "submodule", "name": "vendor", "path": "vendor"},
	}
	var gotPath, gotRef string
	hasRef := false
	serve := func(w http.ResponseWriter, r *http.Request) {
		if id, ok := f.installationOf(w, r); !ok || id != 7 {
			return
		}
		gotPath, gotRef, hasRef = r.PathValue("path"), r.URL.Query().Get("ref"), r.URL.Query().Has("ref")
		switch gotPath {
		case "", "deploy/compose files":
			writeJSON(w, http.StatusOK, listing)
		case "compose.yaml":
			writeJSON(w, http.StatusOK, fileEntry("compose.yaml", []byte("services: {}\n")))
		default:
			writeError(w, http.StatusNotFound, "Not Found")
		}
	}
	f.mux.HandleFunc("GET /repos/octo/app/contents", serve)
	f.mux.HandleFunc("GET /repos/octo/app/contents/{path...}", serve)
	c := f.client()
	ctx := context.Background()

	// Repository root on a branch whose name contains a slash.
	for _, root := range []string{"", ".", "/"} {
		names, err := c.ListDir(ctx, "octo", "app", root, "feature/foo")
		if err != nil {
			t.Fatalf("ListDir(%q): %v", root, err)
		}
		if strings.Join(names, ",") != "compose.yaml,Dockerfile" {
			t.Errorf("ListDir(%q) = %q, want the regular files only", root, names)
		}
		if got := lastRequest(f); got != "GET /repos/octo/app/contents?ref=feature%2Ffoo" {
			t.Errorf("request = %q", got)
		}
		if gotRef != "feature/foo" {
			t.Errorf("GitHub saw ref %q", gotRef)
		}
	}

	// A subdirectory with a space; no ref means the default branch.
	if _, err := c.ListDir(ctx, "octo", "app", "/deploy/compose files/", ""); err != nil {
		t.Fatalf("ListDir(subdirectory): %v", err)
	}
	if got := lastRequest(f); got != "GET /repos/octo/app/contents/deploy/compose%20files" {
		t.Errorf("request = %q", got)
	}
	if gotPath != "deploy/compose files" || hasRef {
		t.Errorf("GitHub saw path %q, ref present = %v", gotPath, hasRef)
	}

	// A file is not a directory; a missing path is not found.
	for _, dir := range []string{"compose.yaml", "missing"} {
		if _, err := c.ListDir(ctx, "octo", "app", dir, "main"); !errors.Is(err, ErrNotFound) {
			t.Errorf("ListDir(%q): err = %v, want ErrNotFound", dir, err)
		}
	}
}

func TestReadFile(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	content := []byte("services:\n  web:\n    image: nginx:1.27\n    ports:\n      - \"8080:80\"\n# " + strings.Repeat("padding ", 40) + "\n")
	var gotPath, gotRef string
	f.mux.HandleFunc("GET /repos/octo/app/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := f.installationOf(w, r); !ok || id != 7 {
			return
		}
		gotPath, gotRef = r.PathValue("path"), r.URL.Query().Get("ref")
		if len(r.URL.Query()) > 1 {
			t.Errorf("unexpected query parameters: %v", r.URL.Query())
		}
		switch gotPath {
		case "empty.txt":
			writeJSON(w, http.StatusOK, fileEntry("empty.txt", nil))
		case "deploy":
			writeJSON(w, http.StatusOK, []map[string]any{{"type": "file", "name": "compose.yaml"}})
		case "link":
			writeJSON(w, http.StatusOK, map[string]any{"type": "symlink", "name": "link", "target": "../elsewhere"})
		case "vendor":
			writeJSON(w, http.StatusOK, map[string]any{"type": "submodule", "name": "vendor"})
		case "broken":
			writeJSON(w, http.StatusOK, map[string]any{"type": "file", "name": "broken", "size": 3, "encoding": "base64", "content": "@@@"})
		case "missing":
			writeError(w, http.StatusNotFound, "Not Found")
		default:
			writeJSON(w, http.StatusOK, fileEntry("compose.yaml", content))
		}
	})
	c := f.client()
	ctx := context.Background()

	cases := []struct {
		path, ref, wantRequest string
	}{
		{"compose.yaml", "main", "GET /repos/octo/app/contents/compose.yaml?ref=main"},
		{"/deploy/prod/compose.yaml", "release/2026&x=1", "GET /repos/octo/app/contents/deploy/prod/compose.yaml?ref=release%2F2026%26x%3D1"},
		{"dir with space/my file#1?.yml", "", "GET /repos/octo/app/contents/dir%20with%20space/my%20file%231%3F.yml"},
		{"100%/docker-compose.yml", "v1.0", "GET /repos/octo/app/contents/100%25/docker-compose.yml?ref=v1.0"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := c.ReadFile(ctx, "octo", "app", tc.path, tc.ref)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("content differs: %q", got)
			}
			if req := lastRequest(f); req != tc.wantRequest {
				t.Errorf("request = %q, want %q", req, tc.wantRequest)
			}
			if gotPath != strings.Trim(tc.path, "/") || gotRef != tc.ref {
				t.Errorf("GitHub saw path %q and ref %q", gotPath, gotRef)
			}
		})
	}

	if got, err := c.ReadFile(ctx, "octo", "app", "empty.txt", "main"); err != nil || len(got) != 0 {
		t.Errorf("empty file: %q, %v", got, err)
	}
	for _, path := range []string{"deploy", "link", "vendor", "missing"} {
		if _, err := c.ReadFile(ctx, "octo", "app", path, "main"); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadFile(%q): err = %v, want ErrNotFound", path, err)
		}
	}
	if _, err := c.ReadFile(ctx, "octo", "app", "broken", "main"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile(broken base64): err = %v, want a decoding error", err)
	}
}

func TestReadFileRejectsOversizedFiles(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	exact := bytes.Repeat([]byte("a"), MaxFileSize)
	f.mux.HandleFunc("GET /repos/octo/app/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("path") {
		case "exact.bin":
			writeJSON(w, http.StatusOK, fileEntry("exact.bin", exact))
		case "declared-large.bin":
			// What GitHub sends for files between 1 and 100 MB: no content.
			writeJSON(w, http.StatusOK, map[string]any{
				"type": "file", "name": "declared-large.bin", "size": MaxFileSize + 1, "encoding": "none", "content": "",
			})
		case "lying.bin":
			entry := fileEntry("lying.bin", append(exact, 'b'))
			entry["size"] = 10
			writeJSON(w, http.StatusOK, entry)
		case "huge-response.bin":
			entry := fileEntry("huge-response.bin", nil)
			entry["content"] = strings.Repeat("A", maxResponseBytes)
			writeJSON(w, http.StatusOK, entry)
		}
	})
	c := f.client()
	ctx := context.Background()

	got, err := c.ReadFile(ctx, "octo", "app", "exact.bin", "main")
	if err != nil || len(got) != MaxFileSize {
		t.Fatalf("a file of exactly MaxFileSize must be readable: %d bytes, %v", len(got), err)
	}
	for _, path := range []string{"declared-large.bin", "lying.bin"} {
		data, err := c.ReadFile(ctx, "octo", "app", path, "main")
		if !errors.Is(err, ErrTooLarge) || data != nil {
			t.Errorf("ReadFile(%q): %d bytes, err = %v, want ErrTooLarge", path, len(data), err)
		}
	}
	// A body beyond the response cap is refused without being buffered whole.
	if data, err := c.ReadFile(ctx, "octo", "app", "huge-response.bin", "main"); err == nil || data != nil {
		t.Errorf("ReadFile(huge response): %d bytes, err = %v, want an error", len(data), err)
	}
}

func TestContentPathsCannotEscapeTheRepository(t *testing.T) {
	f := newFakeGitHub(t).withRepo("octo/app", 7)
	c := f.client()
	ctx := context.Background()

	for _, path := range []string{"../other/compose.yaml", "a/../../b", "a/./b", "a//b", "..", "a/..", "a\x00b", "a\nb"} {
		if _, err := c.ReadFile(ctx, "octo", "app", path, "main"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ReadFile(%q): err = %v, want ErrInvalidArgument", path, err)
		}
		if _, err := c.ListDir(ctx, "octo", "app", path, "main"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ListDir(%q): err = %v, want ErrInvalidArgument", path, err)
		}
	}
	for _, path := range []string{"", "/", "//"} {
		if _, err := c.ReadFile(ctx, "octo", "app", path, "main"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ReadFile(%q): err = %v, want ErrInvalidArgument", path, err)
		}
	}
	if seen := f.seen(); len(seen) != 0 {
		t.Errorf("requests were sent for invalid paths: %q", seen)
	}
}
