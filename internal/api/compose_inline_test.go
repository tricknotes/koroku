package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

const inlineCompose = "services:\n  web:\n    image: ghcr.io/example/app:1\n    environment:\n      SECRET: ${APP_SECRET:?the app's secret}\n      POSTGRES_PASSWORD: literal-password\n"

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCreateAppWithInlineCompose(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()

	if app := createApp(t, h, "plain"); app.ComposeSource != "repository" || app.ComposeContent == nil || *app.ComposeContent != "" || app.ComposeContentHidden {
		t.Errorf("default = %q %v hidden=%v", app.ComposeSource, app.ComposeContent, app.ComposeContentHidden)
	}

	rec := request(t, h, http.MethodPost, "/api/apps", jsonBody(t, map[string]any{
		"slug": "oss", "repo_url": "https://example.com/git/oss.git", "web_port": 8080,
		"compose_source": "inline", "compose_content": strings.ReplaceAll(inlineCompose, "\n", "\r\n"),
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	app := decode[appJSON](t, rec)
	if app.ComposeSource != "inline" || app.ComposeContent == nil || *app.ComposeContent != inlineCompose {
		t.Errorf("created = %q %v", app.ComposeSource, app.ComposeContent)
	}
	stored, err := srv.store.GetApp(context.Background(), "oss")
	if err != nil || !stored.ComposeInline() || stored.ComposeContent != inlineCompose {
		t.Errorf("stored = %+v, %v", stored, err)
	}

	// Back to the repository's file: the text is kept.
	rec = request(t, h, http.MethodPatch, "/api/apps/oss", `{"compose_source":"repository"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if app := decode[appJSON](t, rec); app.ComposeSource != "repository" || *app.ComposeContent != inlineCompose {
		t.Errorf("after switching back = %q %v", app.ComposeSource, *app.ComposeContent)
	}
}

func TestInlineComposeValidation(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	cases := map[string]struct {
		body  map[string]any
		field string
	}{
		"unknown source":   {map[string]any{"compose_source": "upload"}, "compose_source"},
		"inline, no text":  {map[string]any{"compose_source": "inline"}, "compose_content"},
		"inline, blank":    {map[string]any{"compose_source": "inline", "compose_content": " \n"}, "compose_content"},
		"not YAML":         {map[string]any{"compose_source": "inline", "compose_content": "services:\n  web: [\n"}, "compose_content"},
		"not a mapping":    {map[string]any{"compose_source": "inline", "compose_content": "- web\n"}, "compose_content"},
		"only a comment":   {map[string]any{"compose_source": "inline", "compose_content": "# later\n"}, "compose_content"},
		"too large":        {map[string]any{"compose_content": "#" + strings.Repeat("x", store.MaxComposeContentBytes)}, "compose_content"},
		"a NUL character":  {map[string]any{"compose_content": "services: {}\x00"}, "compose_content"},
		"too large inline": {map[string]any{"compose_source": "inline", "compose_content": "services: {}\n#" + strings.Repeat("x", store.MaxComposeContentBytes)}, "compose_content"},
	}
	for name, c := range cases {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog", jsonBody(t, c.body))
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		details, _ := body.Error.Details.(map[string]any)
		if rec.Code != http.StatusUnprocessableEntity || details[c.field] == nil {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// A draft that is not deployed is only bounded in size.
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"compose_content":"services: ["}`); rec.Code != http.StatusOK {
		t.Errorf("draft: %d %s", rec.Code, rec.Body)
	}
}

// The file decides what runs with the app's variables and volumes: only the
// owner changes it, and it is shown to the roles that read the variables.
func TestInlineComposeIsTheOwnersToChange(t *testing.T) {
	f := newRoleFixture(t)
	manager := f.as[managerID]
	for _, body := range []string{
		jsonBody(t, map[string]any{"compose_source": "inline", "compose_content": inlineCompose}),
		jsonBody(t, map[string]any{"compose_content": inlineCompose}),
	} {
		rec := manager.do(http.MethodPatch, "/api/apps/blog", body, originHeader)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_role" ||
			!strings.Contains(rec.Body.String(), `"compose_content"`) {
			t.Errorf("manager PATCH %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	body := jsonBody(t, map[string]any{"compose_source": "inline", "compose_content": inlineCompose})
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", body, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("owner PATCH: %d %s", rec.Code, rec.Body)
	}
	// The settings form sends the current values back.
	if rec := manager.do(http.MethodPatch, "/api/apps/blog", `{"compose_source":"inline","healthcheck_path":"/up"}`, originHeader); rec.Code != http.StatusOK {
		t.Errorf("manager PATCH of the current value: %d %s", rec.Code, rec.Body)
	}
	if rec := manager.do(http.MethodPatch, "/api/apps/blog", `{"compose_source":"repository"}`, originHeader); rec.Code != http.StatusForbidden {
		t.Errorf("manager switching back: %d %s", rec.Code, rec.Body)
	}

	for member, shown := range map[int64]bool{viewerID: false, deployerID: false, managerID: true} {
		for _, target := range []string{"/api/apps/blog", "/api/apps"} {
			rec := f.as[member].do(http.MethodGet, target, "", nil)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"compose_source":"inline"`) {
				t.Fatalf("member %d %s: %d %s", member, target, rec.Code, rec.Body)
			}
			if got := strings.Contains(rec.Body.String(), "literal-password"); got != shown {
				t.Errorf("member %d %s: file shown = %v", member, target, got)
			}
			if got := strings.Contains(rec.Body.String(), `"compose_content_hidden":true`); got == shown {
				t.Errorf("member %d %s: hidden = %v", member, target, got)
			}
		}
	}

	// The record says that the file changed, not what it says.
	acts, err := f.srv.store.ListActivity(context.Background(), f.app(t, "blog").ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	for _, a := range acts {
		if a.Action == actSettingsChanged {
			details, _ := json.Marshal(a.Details)
			recorded += string(details)
		}
	}
	if !strings.Contains(recorded, `{"field":"compose_content","secret":true}`) ||
		!strings.Contains(recorded, `{"field":"compose_source","from":"repository","to":"inline"}`) ||
		strings.Contains(recorded, "literal-password") {
		t.Errorf("activity = %s", recorded)
	}
}

func TestInlineComposeIsHiddenFromTokensAndNeedsARecentLogin(t *testing.T) {
	f := newTokenFixture(t)
	body := jsonBody(t, map[string]any{"compose_source": "inline", "compose_content": inlineCompose})
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", body, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodGet, "/api/apps/blog", "", nil); !strings.Contains(rec.Body.String(), "literal-password") {
		t.Errorf("owner: %s", rec.Body)
	}
	rec := f.withToken(http.MethodGet, "/api/apps/blog", f.created.Value, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "literal-password") ||
		!strings.Contains(rec.Body.String(), `"compose_content":null`) ||
		!strings.Contains(rec.Body.String(), `"compose_content_hidden":true`) {
		t.Errorf("token: %d %s", rec.Code, rec.Body)
	}

	f.clk.Advance(ReauthWindow)
	for _, body := range []string{
		`{"compose_source":"repository"}`,
		`{"compose_content":"services: {}\n"}`,
	} {
		rec := f.owner.do(http.MethodPatch, "/api/apps/blog", body, originHeader)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "reauth_required" {
			t.Errorf("PATCH %s after the window: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", `{"compose_source":"inline","healthcheck_path":"/up"}`, originHeader); rec.Code != http.StatusOK {
		t.Errorf("unchanged after the window: %d %s", rec.Code, rec.Body)
	}
}

// The variables come from the file in the database: no repository is read,
// so an app outside github.com is compared too.
func TestEnvRequirementsOfInlineCompose(t *testing.T) {
	h, gh := newEnvReqServer(t, map[string]string{
		"compose.yaml": "services:\n  web:\n    environment:\n      A: ${FROM_THE_REPOSITORY}\n",
	})
	rec := request(t, h, http.MethodPost, "/api/apps", jsonBody(t, map[string]any{
		"slug": "oss", "repo_url": "https://example.com/git/oss.git", "web_port": 8080,
		"compose_source": "inline", "compose_content": inlineCompose,
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	gh.err = context.DeadlineExceeded
	got := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/oss/env/requirements?ref=main", ""))
	if got.Status != "ok" || got.Path != "koroku.compose.yaml" || got.Ref != "" || got.CommitSHA != "" || len(got.Variables) != 1 ||
		got.Variables[0].Key != "APP_SECRET" || !got.Variables[0].Required || got.Variables[0].Set {
		t.Errorf("requirements = %+v", got)
	}
}

func TestReviewAppCopiesTheInlineCompose(t *testing.T) {
	f := newReviewFixture(t)
	blog := f.app(t, "blog")
	blog.ComposeSource, blog.ComposeContent = store.ComposeSourceInline, inlineCompose
	if err := f.srv.store.UpdateApp(context.Background(), blog, f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if app := f.app(t, "blog-pr-12"); !app.ComposeInline() || app.ComposeContent != inlineCompose {
		t.Errorf("review app = %q %q", app.ComposeSource, app.ComposeContent)
	}
}

func TestEnvPresetsIgnoreTheRepositoryComposeOfAnInlineApp(t *testing.T) {
	gh := &fakeGitHub{files: map[string]string{
		"Gemfile":      "gem \"rails\", \"~> 8.0\"\n",
		"compose.yaml": "services:\n  web:\n    environment:\n      RAILS_ENV: development\n",
	}}
	h := newTestServer(t, func(o *Options) { o.Auth.GitHub = gh }).Handler()

	read := decode[envPresetsOut](t, request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets", ""))
	if len(read.Skipped) != 1 || read.Skipped[0].Key != "RAILS_ENV" {
		t.Fatalf("with the repository's file: %+v", read)
	}
	got := decode[envPresetsOut](t, request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets?compose_source=inline", ""))
	if got.Status != "ok" || got.ComposePath != "" || len(got.Skipped) != 0 || len(got.Presets) == 0 || got.Presets[0].Key != "RAILS_ENV" {
		t.Errorf("inline = %+v", got)
	}
	if rec := request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets?compose_source=upload", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown source: %d", rec.Code)
	}
}
