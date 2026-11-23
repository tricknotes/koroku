package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeUsers resolves GitHub logins for adding members.
type fakeUsers map[string]github.User

func (f fakeUsers) UserByLogin(_ context.Context, login string) (github.User, error) {
	if u, ok := f[strings.ToLower(login)]; ok {
		return u, nil
	}
	return github.User{}, github.ErrNotFound
}

// Users of the role fixture. The owner is 42 (octo).
const (
	viewerID   = 101
	deployerID = 102
	managerID  = 103
	strangerID = 104 // a member with a role on shop only
)

type roleFixture struct {
	srv   *Server
	h     http.Handler
	clk   *clock.Fake
	gh    *fakeGitHub
	owner *browser
	// as holds a logged-in browser per member ID.
	as map[int64]*browser
}

var originHeader = map[string]string{"Origin": consoleURL}

// newRoleFixture creates the staging app blog and the production app shop,
// and members with each role on blog plus one with a role on shop only,
// all logged in.
func newRoleFixture(t *testing.T, extra ...func(*Options)) *roleFixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	gh := defaultGitHub()
	members := map[int64]string{viewerID: "vic", deployerID: "dee", managerID: "max", strangerID: "sam"}
	users := fakeUsers{}
	for id, login := range members {
		code, token := "code-"+login, "token-"+login
		gh.codes[code] = token
		gh.users[token] = github.User{ID: id, Login: login}
		users[login] = github.User{ID: id, Login: login}
	}
	users["octo"] = github.User{ID: 42, Login: "octo"}
	users["newbie"] = github.User{ID: 111, Login: "newbie", AvatarURL: "https://avatars.example/11"}
	srv := newTestServer(t, func(o *Options) {
		o.Clock = clk
		o.Auth = AuthOptions{AllowedUserID: 42, ConsoleURL: consoleURL, GitHub: gh}
		o.Users = users
		for _, m := range extra {
			m(o)
		}
	})
	f := &roleFixture{srv: srv, h: srv.Handler(), clk: clk, gh: gh, as: map[int64]*browser{}}
	f.owner = newBrowser(t, f.h)
	f.owner.login("code-owner")
	for _, body := range []string{
		`{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog"}`,
		`{"slug":"shop","web_port":3000,"repo_full_name":"octo/shop","environment":"production"}`,
	} {
		if rec := f.owner.do(http.MethodPost, "/api/apps", body, originHeader); rec.Code != http.StatusCreated {
			t.Fatalf("create app: %d %s", rec.Code, rec.Body)
		}
	}
	ctx := context.Background()
	blog, _ := srv.store.GetApp(ctx, "blog")
	shop, _ := srv.store.GetApp(ctx, "shop")
	roles := map[int64][2]any{
		viewerID: {blog.ID, store.RoleView}, deployerID: {blog.ID, store.RoleDeploy},
		managerID: {blog.ID, store.RoleManage}, strangerID: {shop.ID, store.RoleManage},
	}
	for id, login := range members {
		if err := srv.store.AddMember(ctx, store.Member{GitHubUserID: id, Login: login, AddedAt: clk.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.SetAppRole(ctx, roles[id][0].(int64), id, roles[id][1].(string)); err != nil {
			t.Fatal(err)
		}
		b := newBrowser(t, f.h)
		if rec := b.login("code-" + login); rec.Header().Get("Location") != "/" {
			t.Fatalf("login of %s: %d %s", login, rec.Code, rec.Header().Get("Location"))
		}
		f.as[id] = b
	}
	return f
}

func (f *roleFixture) app(t *testing.T, slug string) store.App {
	t.Helper()
	app, err := f.srv.store.GetApp(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// fillPattern turns a route pattern into a request path for the fixture.
func fillPattern(pattern, slug string, id int64) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	r := strings.NewReplacer(
		"{slug}", slug, "{id}", strconv.FormatInt(id, 10), "{owner}", "octo", "{repo}", slug,
		"{key}", "SECRET", "{snapshot}", "0123abcd",
	)
	return method, r.Replace(path)
}

func TestMemberLoginNeedsARole(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	f.gh.codes["code-newbie"] = "token-newbie"
	f.gh.users["token-newbie"] = github.User{ID: 111, Login: "newbie"}

	// A member without any role is refused like a stranger.
	if err := f.srv.store.AddMember(ctx, store.Member{GitHubUserID: 111, Login: "newbie", AddedAt: f.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, f.h)
	if rec := b.login("code-newbie"); !strings.Contains(rec.Header().Get("Location"), "login_error=not_allowed") {
		t.Fatalf("member without a role: %s", rec.Header().Get("Location"))
	}
	if err := f.srv.store.SetAppRole(ctx, f.app(t, "blog").ID, 111, store.RoleView); err != nil {
		t.Fatal(err)
	}
	if rec := b.login("code-newbie"); rec.Header().Get("Location") != "/" {
		t.Fatalf("member with a role: %s", rec.Header().Get("Location"))
	}
	var session struct {
		User struct {
			Login string `json:"login"`
			Owner bool   `json:"owner"`
		} `json:"user"`
	}
	_ = json.Unmarshal(b.do(http.MethodGet, "/api/session", "", nil).Body.Bytes(), &session)
	if session.User.Login != "newbie" || session.User.Owner {
		t.Errorf("session user = %+v", session.User)
	}
	// Strangers stay out.
	if rec := newBrowser(t, f.h).login("code-stranger"); !strings.Contains(rec.Header().Get("Location"), "not_allowed") {
		t.Errorf("stranger: %s", rec.Header().Get("Location"))
	}
}

// TestRoutesDeclareAccess keeps the routes open to every member short and
// deliberate: a new route is for the owner or needs a role on an app unless
// it is added here.
func TestRoutesDeclareAccess(t *testing.T) {
	srv := newTestServer(t, nil)
	open := map[string]bool{
		"GET /api/sessions": true, "DELETE /api/sessions/{id}": true, "POST /api/sessions/revoke-others": true,
		"GET /api/login-events": true, "GET /api/tokens": true, "POST /api/tokens": true,
		"DELETE /api/tokens/{id}": true, "GET /api/system": true, "GET /api/apps": true, "GET /api/backups": true,
	}
	seen := map[string]bool{}
	for _, rt := range srv.apiRoutes() {
		if seen[rt.pattern] {
			t.Errorf("%s is listed twice", rt.pattern)
		}
		seen[rt.pattern] = true
		if (rt.access.scope == scopeUser) != open[rt.pattern] {
			t.Errorf("%s: scope %d, open to every member = %v", rt.pattern, rt.access.scope, open[rt.pattern])
		}
		if rt.access.scope == scopeApp && !strings.Contains(rt.pattern, "{slug}") {
			t.Errorf("%s needs a role on {slug} but has none", rt.pattern)
		}
		if rt.access.scope != scopeUser && rt.access.scope != scopeOwner && rt.access.level == levelNone {
			t.Errorf("%s has no level", rt.pattern)
		}
	}
}

// TestOwnerRoutesRefuseMembers sends every owner-only route as a member who
// manages blog: each answers 403 forbidden_role before doing anything.
func TestOwnerRoutesRefuseMembers(t *testing.T) {
	f := newRoleFixture(t)
	manager := f.as[managerID]
	for _, rt := range f.srv.apiRoutes() {
		if rt.access.scope != scopeOwner {
			continue
		}
		method, path := fillPattern(rt.pattern, "blog", viewerID)
		body := ""
		if isMutating(method) {
			body = `{"login":"newbie","role":"view","images":true}`
		}
		rec := manager.do(method, path, body, originHeader)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_role" {
			t.Errorf("%s as a member: %d %s", rt.pattern, rec.Code, rec.Body)
		}
	}
	// The app still exists and the members are unchanged.
	if _, err := f.srv.store.GetApp(context.Background(), "blog"); err != nil {
		t.Errorf("blog: %v", err)
	}
	if rec := f.owner.do(http.MethodGet, "/api/system/docker", "", nil); rec.Code == http.StatusForbidden {
		t.Errorf("the owner was refused: %s", rec.Body)
	}
}

// TestAppRoutesNeedTheirLevel sends every route that acts on an app as each
// member: below the route's level it answers 403 forbidden_role, at or
// above it gets past the check, and a member without a role on the app is
// told the app does not exist.
func TestAppRoutesNeedTheirLevel(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	blog := f.app(t, "blog")
	d, _, err := f.srv.store.EnqueueDeployment(ctx, blog.ID, "main", testSHA, "first", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.srv.store.StartTaskRun(ctx, store.TaskRun{AppID: blog.ID, Service: "web", Command: "true"}, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[scope]int64{scopeApp: 1, scopeDeployment: d.ID, scopeTaskRun: run.ID}
	levels := map[int64]level{viewerID: levelView, deployerID: levelDeploy, managerID: levelManage}
	checked := 0
	for _, rt := range f.srv.apiRoutes() {
		id, ok := ids[rt.access.scope]
		if !ok || rt.pattern == "DELETE /api/apps/{slug}" {
			// Deleting narrows manage to review apps in the handler
			// (TestDeleteReviewApp).
			continue
		}
		method, path := fillPattern(rt.pattern, "blog", id)
		body := ""
		if isMutating(method) {
			body = `{}`
		}
		for member, have := range levels {
			if have >= rt.access.level && (strings.HasSuffix(path, "/logs") || strings.HasSuffix(rt.pattern, "/start") || strings.HasSuffix(rt.pattern, "/stop") || strings.HasSuffix(rt.pattern, "/restart")) {
				// Streams and operations that would run; the level check is
				// covered by the members below the level.
				continue
			}
			rec := f.as[member].do(method, path, body, originHeader)
			refused := rec.Code == http.StatusForbidden && errorCode(t, rec) == "forbidden_role"
			if want := have < rt.access.level; refused != want {
				t.Errorf("%s as %s: %d %s (want refused = %v)", rt.pattern, have, rec.Code, rec.Body, want)
			}
		}
		rec := f.as[strangerID].do(method, path, body, originHeader)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s as a member without a role on blog: %d %s", rt.pattern, rec.Code, rec.Body)
		}
		checked++
	}
	if checked < 40 {
		t.Errorf("checked only %d routes", checked)
	}
}

// A member cannot tell an app it has no role on, or a deployment, task run
// or database copy of such an app, from one that does not exist: every
// route answers both with the same status and the same body.
func TestHiddenRecordsAnswerLikeMissingOnes(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	blog, shop := f.app(t, "blog"), f.app(t, "shop")
	d, _, err := f.srv.store.EnqueueDeployment(ctx, blog.ID, "main", testSHA, "first", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.srv.store.StartTaskRun(ctx, store.TaskRun{AppID: blog.ID, Service: "web", Command: "true"}, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.srv.store.StartDBCopy(ctx, blog, shop, store.CopySourceLive, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	hidden := map[scope]int64{scopeApp: 0, scopeDeployment: d.ID, scopeTaskRun: run.ID, scopeDBCopy: c.ID}
	want := map[scope]string{
		scopeApp:        `{"error":{"code":"app_not_found","message":"no such app"}}`,
		scopeDeployment: `{"error":{"code":"deployment_not_found","message":"no such deployment"}}`,
		scopeTaskRun:    `{"error":{"code":"task_run_not_found","message":"no such task run"}}`,
		scopeDBCopy:     `{"error":{"code":"db_copy_not_found","message":"no such database copy"}}`,
	}
	// The stranger has a role on shop only; the owner sees what the handler
	// itself answers for a record that does not exist.
	stranger := f.as[strangerID]
	checked := map[scope]int{}
	for _, rt := range f.srv.apiRoutes() {
		id, ok := hidden[rt.access.scope]
		if !ok {
			continue
		}
		body := ""
		if method, _ := fillPattern(rt.pattern, "blog", id); isMutating(method) {
			body = `{}`
		}
		ask := func(b *browser, slug string, id int64) (int, string) {
			method, path := fillPattern(rt.pattern, slug, id)
			rec := b.do(method, path, body, originHeader)
			return rec.Code, strings.TrimSpace(rec.Body.String())
		}
		hiddenCode, hiddenBody := ask(stranger, "blog", id)
		if hiddenCode != http.StatusNotFound || hiddenBody != want[rt.access.scope] {
			t.Errorf("%s on a record without a role: %d %s, want 404 %s", rt.pattern, hiddenCode, hiddenBody, want[rt.access.scope])
		}
		for who, b := range map[string]*browser{"a member": stranger, "the owner": f.owner} {
			code, got := ask(b, "nope", 987654)
			if code != hiddenCode || got != hiddenBody {
				t.Errorf("%s: a record without a role answers %d %s, a missing one answers %s %d %s",
					rt.pattern, hiddenCode, hiddenBody, who, code, got)
			}
		}
		checked[rt.access.scope]++
	}
	for sc := range hidden {
		if checked[sc] == 0 {
			t.Errorf("no route of scope %d was checked", sc)
		}
	}
}

func decodeApps(t *testing.T, rec *httptest.ResponseRecorder) []appJSON {
	t.Helper()
	var out struct {
		Apps []appJSON `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("apps: %v %s", err, rec.Body)
	}
	return out.Apps
}

func TestMembersSeeOnlyTheirApps(t *testing.T) {
	f := newRoleFixture(t)
	apps := decodeApps(t, f.as[viewerID].do(http.MethodGet, "/api/apps", "", nil))
	if len(apps) != 1 || apps[0].Slug != "blog" || apps[0].Role != "view" {
		t.Errorf("viewer's apps = %+v", apps)
	}
	all := decodeApps(t, f.owner.do(http.MethodGet, "/api/apps", "", nil))
	if len(all) != 2 || all[0].Role != "owner" {
		t.Errorf("owner's apps = %+v", all)
	}
	if rec := f.as[viewerID].do(http.MethodGet, "/api/apps/shop", "", nil); rec.Code != http.StatusNotFound || errorCode(t, rec) != "app_not_found" {
		t.Errorf("viewer GET shop: %d %s", rec.Code, rec.Body)
	}
	// Repositories are readable through an app deployed from them only.
	if rec := f.as[viewerID].do(http.MethodGet, "/api/repos/octo/shop/branches", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("viewer branches of shop: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[viewerID].do(http.MethodGet, "/api/repos/octo/blog/branches", "", nil); rec.Code != http.StatusOK {
		t.Errorf("viewer branches of blog: %d %s", rec.Code, rec.Body)
	}
}

func TestEnvValuesNeedManage(t *testing.T) {
	f := newRoleFixture(t)
	if rec := f.owner.do(http.MethodPut, "/api/apps/blog/env/SECRET", `{"value":"hunter2"}`, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("put env: %d %s", rec.Code, rec.Body)
	}
	for member, wantValue := range map[int64]bool{viewerID: false, deployerID: false, managerID: true} {
		rec := f.as[member].do(http.MethodGet, "/api/apps/blog/env", "", nil)
		var env envListJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != http.StatusOK || len(env.Vars) != 1 || env.Vars[0].Key != "SECRET" {
			t.Fatalf("member %d: %d %s", member, rec.Code, rec.Body)
		}
		if got := env.Vars[0].Value != nil; got != wantValue || env.ValuesForbidden == wantValue {
			t.Errorf("member %d: value shown = %v, forbidden = %v", member, got, env.ValuesForbidden)
		}
		if !wantValue && strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("member %d saw the value", member)
		}
	}
}

func TestOwnerOnlySettings(t *testing.T) {
	f := newRoleFixture(t)
	manager := f.as[managerID]
	for _, body := range []string{
		`{"relax_policy":true}`,
		`{"repo_full_name":"octo/other"}`,
		`{"promote_from":"shop"}`,
		`{"environment":"production"}`,
	} {
		rec := manager.do(http.MethodPatch, "/api/apps/blog", body, originHeader)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_role" {
			t.Errorf("PATCH %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	// Sending the current values back (as the settings form does) is fine.
	rec := manager.do(http.MethodPatch, "/api/apps/blog",
		`{"relax_policy":false,"repo_full_name":"octo/blog","promote_from":"","memory_limit":"768m"}`, originHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager PATCH: %d %s", rec.Code, rec.Body)
	}
	if app := f.app(t, "blog"); app.MemoryLimit != "768m" {
		t.Errorf("memory limit = %q", app.MemoryLimit)
	}
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", `{"relax_policy":true}`, originHeader); rec.Code != http.StatusOK {
		t.Errorf("owner PATCH: %d %s", rec.Code, rec.Body)
	}
}

// Automatic deployment on push is the manager's to turn on for a staging
// app, but only the owner makes a production app deploy pushes by itself.
func TestProductionAutoDeployIsOwnerOnly(t *testing.T) {
	f := newRoleFixture(t)
	manager := f.as[managerID]
	if rec := manager.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy":true}`, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("staging auto-deploy by the manager: %d %s", rec.Code, rec.Body)
	}
	if rec := manager.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy":false}`, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("turning it off: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", `{"environment":"production"}`, originHeader); rec.Code != http.StatusOK {
		t.Fatalf("owner makes blog production: %d %s", rec.Code, rec.Body)
	}
	rec := manager.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy":true}`, originHeader)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_role" || !strings.Contains(rec.Body.String(), "auto_deploy") {
		t.Errorf("production auto-deploy by the manager: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy":true}`, originHeader); rec.Code != http.StatusOK {
		t.Errorf("production auto-deploy by the owner: %d %s", rec.Code, rec.Body)
	}
	// Which workflow a production app waits for is the owner's too.
	rec = manager.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy_workflow":".github/workflows/image.yml"}`, originHeader)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "auto_deploy") {
		t.Errorf("production workflow by the manager: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy_workflow":".github/workflows/image.yml"}`, originHeader); rec.Code != http.StatusOK {
		t.Errorf("production workflow by the owner: %d %s", rec.Code, rec.Body)
	}
}

func TestDeploymentsRecordTheActor(t *testing.T) {
	f := newRoleFixture(t)
	rec := f.as[deployerID].do(http.MethodPost, "/api/apps/blog/deployments", `{"ref":"main"}`, originHeader)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var d deploymentJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Actor == nil || d.Actor.ID != deployerID || d.Actor.Login != "dee" {
		t.Errorf("actor = %+v", d.Actor)
	}
	list := f.as[viewerID].do(http.MethodGet, "/api/apps/blog/deployments", "", nil)
	if !strings.Contains(list.Body.String(), `"actor":{"id":102,"login":"dee"}`) {
		t.Errorf("history: %s", list.Body)
	}
	// A member who may deploy blog may not deploy shop.
	if rec := f.as[deployerID].do(http.MethodPost, "/api/apps/shop/deployments", `{"ref":"main"}`, originHeader); rec.Code != http.StatusNotFound {
		t.Errorf("deploy shop: %d %s", rec.Code, rec.Body)
	}
}

func TestDBCopyNeedsManageOnTheSource(t *testing.T) {
	f := newRoleFixture(t)
	rec := f.as[managerID].do(http.MethodPost, "/api/apps/blog/db-copies",
		`{"source":"shop","source_kind":"live","confirm":"blog"}`, originHeader)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "source_not_found" {
		t.Errorf("copy from an app without a role: %d %s", rec.Code, rec.Body)
	}
}

func TestMembersAPI(t *testing.T) {
	f := newRoleFixture(t)
	add := func(login string) *httptest.ResponseRecorder {
		return f.owner.do(http.MethodPost, "/api/members", `{"login":"`+login+`"}`, originHeader)
	}
	rec := add("@Newbie")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var m memberJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.GitHubUserID != 111 || m.Login != "newbie" || len(m.Roles) != 0 {
		t.Errorf("member = %+v", m)
	}
	for login, code := range map[string]string{
		"newbie": "member_exists", "octo": "is_owner", "nobody": "user_not_found", "-bad-": "validation_failed",
	} {
		if rec := add(login); errorCode(t, rec) != code {
			t.Errorf("add %s: %d %s, want %s", login, rec.Code, rec.Body, code)
		}
	}

	rec = f.owner.do(http.MethodPut, "/api/members/111/roles/shop", `{"role":"deploy"}`, originHeader)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"roles":[{"app":"shop","role":"deploy"}]`) {
		t.Errorf("set role: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodPut, "/api/members/111/roles/shop", `{"role":"admin"}`, originHeader); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad role: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodPut, "/api/members/42/roles/shop", `{"role":"view"}`, originHeader); rec.Code != http.StatusNotFound {
		t.Errorf("role for the owner: %d %s", rec.Code, rec.Body)
	}

	list := f.owner.do(http.MethodGet, "/api/members", "", nil)
	if !strings.Contains(list.Body.String(), `"login":"newbie"`) || !strings.Contains(list.Body.String(), `"login":"vic"`) {
		t.Errorf("list: %s", list.Body)
	}

	// Granting needs a recent login; taking away does not.
	f.clk.Advance(ReauthWindow + time.Minute)
	if rec := f.owner.do(http.MethodPut, "/api/members/111/roles/blog", `{"role":"view"}`, originHeader); errorCode(t, rec) != "reauth_required" {
		t.Errorf("grant without a recent login: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/members/111/roles/shop", "", originHeader); rec.Code != http.StatusOK {
		t.Errorf("take away: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/members/111", "", originHeader); rec.Code != http.StatusNoContent {
		t.Errorf("remove: %d %s", rec.Code, rec.Body)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/members/111", "", originHeader); rec.Code != http.StatusNotFound {
		t.Errorf("remove again: %d %s", rec.Code, rec.Body)
	}
}

func TestRemovingAccessEndsSessionsAndTokens(t *testing.T) {
	f := newRoleFixture(t)
	viewer := f.as[viewerID]
	rec := viewer.do(http.MethodPost, "/api/tokens", `{"name":"laptop","expires_in_days":7}`, originHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	var created createdToken
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	withToken := func(target string) *httptest.ResponseRecorder {
		req := localRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+created.Value)
		return send(f.h, req)
	}
	// The token acts as the viewer.
	if apps := decodeApps(t, withToken("/api/apps")); len(apps) != 1 || apps[0].Slug != "blog" {
		t.Errorf("token apps = %+v", apps)
	}
	if rec := withToken("/api/apps/shop"); rec.Code != http.StatusNotFound {
		t.Errorf("token GET shop: %d", rec.Code)
	}

	// Taking the last role away ends both.
	if rec := f.owner.do(http.MethodDelete, "/api/members/101/roles/blog", "", originHeader); rec.Code != http.StatusOK {
		t.Fatalf("take away: %d %s", rec.Code, rec.Body)
	}
	if rec := viewer.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("session after losing the last role: %d", rec.Code)
	}
	if rec := withToken("/api/apps"); rec.Code != http.StatusUnauthorized {
		t.Errorf("token after losing the last role: %d", rec.Code)
	}
	if list, _ := f.srv.store.ListSessions(context.Background(), viewerID, f.clk.Now()); len(list) != 0 {
		t.Errorf("sessions left: %+v", list)
	}

	// Removing a member ends its sessions too.
	if rec := f.owner.do(http.MethodDelete, "/api/members/102", "", originHeader); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[deployerID].do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("removed member's session: %d", rec.Code)
	}
	// Deleting an app ends the sessions of members who had a role on it only.
	if rec := f.owner.do(http.MethodDelete, "/api/apps/shop", "", originHeader); rec.Code != http.StatusNoContent {
		t.Fatalf("delete shop: %d %s", rec.Code, rec.Body)
	}
	if list, _ := f.srv.store.ListSessions(context.Background(), strangerID, f.clk.Now()); len(list) != 0 {
		t.Errorf("sessions of the member of shop: %+v", list)
	}
	if rec := f.as[managerID].do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusOK {
		t.Errorf("manager of blog: %d", rec.Code)
	}
}

func TestOwnerSeesEveryonesSessions(t *testing.T) {
	f := newRoleFixture(t)
	type listed struct {
		Sessions []sessionJSON `json:"sessions"`
	}
	var mine, all listed
	_ = json.Unmarshal(f.as[viewerID].do(http.MethodGet, "/api/sessions", "", nil).Body.Bytes(), &mine)
	_ = json.Unmarshal(f.owner.do(http.MethodGet, "/api/sessions", "", nil).Body.Bytes(), &all)
	if len(mine.Sessions) != 1 || mine.Sessions[0].User.Login != "vic" {
		t.Errorf("viewer's sessions = %+v", mine.Sessions)
	}
	if len(all.Sessions) != 5 {
		t.Errorf("owner sees %d sessions", len(all.Sessions))
	}
	// A member cannot end another user's session; the owner can.
	var other string
	for _, s := range all.Sessions {
		if s.User.Login == "dee" {
			other = s.ID
		}
	}
	if rec := f.as[viewerID].do(http.MethodDelete, "/api/sessions/"+other, "", originHeader); rec.Code != http.StatusNotFound {
		t.Errorf("member ends another's session: %d", rec.Code)
	}
	if rec := f.owner.do(http.MethodDelete, "/api/sessions/"+other, "", originHeader); rec.Code != http.StatusNoContent {
		t.Errorf("owner ends a member's session: %d", rec.Code)
	}
	if rec := f.as[deployerID].do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("ended session still works: %d", rec.Code)
	}

	var events struct {
		Events []loginEventJSON `json:"events"`
	}
	_ = json.Unmarshal(f.as[viewerID].do(http.MethodGet, "/api/login-events", "", nil).Body.Bytes(), &events)
	for _, ev := range events.Events {
		if ev.GitHubUserID != viewerID {
			t.Errorf("viewer sees the login of %d", ev.GitHubUserID)
		}
	}
}

func TestMemberSystemSummary(t *testing.T) {
	f := newRoleFixture(t)
	if rec := f.as[viewerID].do(http.MethodGet, "/api/system", "", nil); rec.Code != http.StatusOK {
		t.Errorf("system: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[viewerID].do(http.MethodPost, "/api/backups", "{}", originHeader); errorCode(t, rec) != "forbidden_role" {
		t.Errorf("viewer starts a backup: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[managerID].do(http.MethodPost, "/api/backups", "{}", originHeader); errorCode(t, rec) == "forbidden_role" {
		t.Errorf("manager starts a backup: %d %s", rec.Code, rec.Body)
	}
}
