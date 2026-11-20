package envreq

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
)

type fakeFiles struct {
	dirs    map[string][]string // ref -> root names; missing ref = repo not visible
	files   map[string]string   // ref + ":" + path -> content
	readErr error
	calls   int
}

func (f *fakeFiles) ListDir(_ context.Context, _, _, dir, ref string) ([]string, error) {
	f.calls++
	names, ok := f.dirs[ref]
	if !ok || dir != "" {
		return nil, fmt.Errorf("list: %w", github.ErrNotFound)
	}
	return names, nil
}

func (f *fakeFiles) ReadFile(_ context.Context, _, _, path, ref string) ([]byte, error) {
	f.calls++
	if f.readErr != nil {
		return nil, f.readErr
	}
	content, ok := f.files[ref+":"+path]
	if !ok {
		return nil, fmt.Errorf("read: %w", github.ErrNotFound)
	}
	return []byte(content), nil
}

const sha = "9fceb02d0ae598e95dc970b74767f19372d61af8"

func TestReadCompose(t *testing.T) {
	files := &fakeFiles{
		dirs:  map[string][]string{sha: {"README.md", "docker-compose.yml"}},
		files: map[string]string{sha + ":docker-compose.yml": "x: ${A:?the A} ${B:-b}\n"},
	}
	r := NewReader(files, nil)
	req := Request{Owner: "octo", Repo: "blog", SHA: sha, Source: SourceCompose}
	got := r.Read(context.Background(), req)
	b := "b"
	want := Result{Status: StatusOK, Path: "docker-compose.yml", Variables: []Variable{
		{Key: "A", Required: true, Description: "the A"},
		{Key: "B", Default: &b},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
	calls := files.calls
	r.Read(context.Background(), req)
	if files.calls != calls {
		t.Errorf("second read called GitHub again (%d -> %d calls)", calls, files.calls)
	}
}

func TestReadDotenv(t *testing.T) {
	files := &fakeFiles{
		dirs:  map[string][]string{sha: {".env.example"}},
		files: map[string]string{sha + ":config/env.sample": "# Mail server\n# host only\nSMTP_HOST=\n\nexport API_KEY=xxx\nnot a line\n"},
	}
	got := NewReader(files, nil).Read(context.Background(), Request{Owner: "o", Repo: "r", SHA: sha, Source: SourceDotenv, ExamplePath: "config/env.sample"})
	want := Result{Status: StatusOK, Path: "config/env.sample", Variables: []Variable{
		{Key: "API_KEY", Required: true},
		{Key: "SMTP_HOST", Required: true, Description: "Mail server host only"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
}

func TestReadUnavailable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		files *fakeFiles
		req   Request
		want  Result
	}{
		{"off", &fakeFiles{}, Request{Source: SourceOff}, Result{Status: StatusOff}},
		{"repo not visible", &fakeFiles{dirs: map[string][]string{}}, Request{SHA: sha, Source: SourceCompose},
			Unavailable(ReasonRepoUnavailable, "")},
		{"no compose file", &fakeFiles{dirs: map[string][]string{sha: {"README.md"}}}, Request{SHA: sha, Source: SourceCompose},
			Unavailable(ReasonFileNotFound, "")},
		{"no example file", &fakeFiles{dirs: map[string][]string{sha: {}}}, Request{SHA: sha, Source: SourceDotenv},
			Unavailable(ReasonFileNotFound, ".env.example")},
		{"rate limited", &fakeFiles{dirs: map[string][]string{sha: {}}, readErr: fmt.Errorf("x: %w", github.ErrRateLimited)},
			Request{SHA: sha, Source: SourceCompose, ComposePath: "deploy/compose.yaml"},
			Unavailable(ReasonRateLimited, "deploy/compose.yaml")},
		{"other error", &fakeFiles{dirs: map[string][]string{sha: {}}, readErr: errors.New("boom")},
			Request{SHA: sha, Source: SourceDotenv},
			Unavailable(ReasonGitHubError, ".env.example")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewReader(tc.files, nil).Read(ctx, tc.req); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Read = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := NewReader(nil, nil).Read(ctx, Request{Source: SourceCompose}); got.Reason != ReasonNotConfigured {
		t.Errorf("nil files: reason = %q", got.Reason)
	}
}

func TestReadDoesNotCacheTransientFailures(t *testing.T) {
	files := &fakeFiles{dirs: map[string][]string{sha: {"compose.yaml"}}, readErr: errors.New("boom")}
	r := NewReader(files, nil)
	req := Request{SHA: sha, Source: SourceCompose}
	if got := r.Read(context.Background(), req); got.Reason != ReasonGitHubError {
		t.Fatalf("reason = %q", got.Reason)
	}
	files.readErr = nil
	files.files = map[string]string{sha + ":compose.yaml": "x: ${A}\n"}
	if got := r.Read(context.Background(), req); got.Status != StatusOK || len(got.Variables) != 1 {
		t.Fatalf("after recovery: %+v", got)
	}
}

func TestCacheIsBounded(t *testing.T) {
	files := &fakeFiles{dirs: map[string][]string{}}
	r := NewReader(files, nil)
	for i := 0; i < cacheSize+10; i++ {
		ref := fmt.Sprintf("%040d", i)
		files.dirs[ref] = []string{"compose.yaml"}
		files.files = map[string]string{ref + ":compose.yaml": "x: 1\n"}
		r.Read(context.Background(), Request{SHA: ref, Source: SourceCompose})
	}
	if len(r.cache) != cacheSize || len(r.order) != cacheSize {
		t.Errorf("cache holds %d entries (%d in order), want %d", len(r.cache), len(r.order), cacheSize)
	}
}

func TestCompare(t *testing.T) {
	set, unlisted := Compare([]Variable{{Key: "A"}, {Key: "B"}}, []string{"Z", "A", "C"})
	if !reflect.DeepEqual(set, map[string]bool{"A": true, "B": false}) {
		t.Errorf("set = %v", set)
	}
	if !reflect.DeepEqual(unlisted, []string{"C", "Z"}) {
		t.Errorf("unlisted = %v", unlisted)
	}
}

// A repository the App cannot see is read anonymously; one it can see, or
// one it fails on for another reason, is not (anonymous calls are scarce).
func TestReadFallsBackToAnonymous(t *testing.T) {
	ctx := context.Background()
	req := Request{Owner: "o", Repo: "r", SHA: sha, Source: SourceCompose}
	public := func() *fakeFiles {
		return &fakeFiles{
			dirs:  map[string][]string{sha: {"compose.yaml"}},
			files: map[string]string{sha + ":compose.yaml": "x: ${A}\n"},
		}
	}

	notInstalled, anon := &fakeFiles{}, public()
	if got := NewReader(notInstalled, anon).Read(ctx, req); got.Status != StatusOK || len(got.Variables) != 1 {
		t.Errorf("App not installed = %+v", got)
	}
	if anon.calls != 2 {
		t.Errorf("anonymous calls = %d, want 2 (list and read)", anon.calls)
	}

	installed, anon := public(), public()
	NewReader(installed, anon).Read(ctx, req)
	if anon.calls != 0 {
		t.Errorf("the App could read it, yet %d anonymous calls were made", anon.calls)
	}

	anon = public()
	if got := NewReader(nil, anon).Read(ctx, req); got.Status != StatusOK {
		t.Errorf("no App = %+v", got)
	}

	limited := &limitedFiles{}
	anon = public()
	if got := NewReader(limited, anon).Read(ctx, req); got.Reason != ReasonRateLimited || anon.calls != 0 {
		t.Errorf("App rate limited = %+v, anonymous calls %d", got, anon.calls)
	}

	if got := NewReader(&fakeFiles{}, &fakeFiles{}).Read(ctx, req); got.Reason != ReasonRepoUnavailable {
		t.Errorf("private and not installed = %+v", got)
	}
}

type limitedFiles struct{}

func (limitedFiles) ListDir(context.Context, string, string, string, string) ([]string, error) {
	return nil, fmt.Errorf("list: %w", github.ErrRateLimited)
}

func (limitedFiles) ReadFile(context.Context, string, string, string, string) ([]byte, error) {
	return nil, fmt.Errorf("read: %w", github.ErrRateLimited)
}

func TestCompareTreatsProvidedVariablesAsSet(t *testing.T) {
	set, _ := Compare([]Variable{{Key: "KOROKU_COMMIT_SHORT"}, {Key: "API_KEY"}}, nil)
	if !set["KOROKU_COMMIT_SHORT"] || set["API_KEY"] {
		t.Errorf("set = %v", set)
	}
}
