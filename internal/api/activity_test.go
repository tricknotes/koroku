package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

type activityList struct {
	Entries []struct {
		ID     int64          `json:"id"`
		Actor  *actorJSON     `json:"actor"`
		Action string         `json:"action"`
		Detail map[string]any `json:"details"`
	} `json:"entries"`
	NextBefore *int64 `json:"next_before"`
}

func (f *roleFixture) activity(t *testing.T, b *browser, query string) activityList {
	t.Helper()
	rec := b.do(http.MethodGet, "/api/apps/blog/activity"+query, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("activity%s: %d %s", query, rec.Code, rec.Body)
	}
	var out activityList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestActivityRecordsWhoDidWhat walks through every recorded operation on
// blog as its manager (and the owner for what only the owner may do), then
// reads the activity as the viewer.
func TestActivityRecordsWhoDidWhat(t *testing.T) {
	f := newRoleFixture(t)
	f.srv.backups = &fakeBackups{store: f.srv.store, dump: "dump"}
	f.srv.copier = &fakeCopier{store: f.srv.store, logDir: t.TempDir()}
	manager := f.as[managerID]
	must := func(b *browser, method, path, body string, want int) []byte {
		t.Helper()
		rec := b.do(method, path, body, originHeader)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}

	must(manager, http.MethodPatch, "/api/apps/blog", `{"memory_limit":"512m","healthcheck_path":"/up","web_port":3000}`, http.StatusOK)
	// Saving the same values again changes nothing and records nothing.
	must(manager, http.MethodPatch, "/api/apps/blog", `{"memory_limit":"512m"}`, http.StatusOK)

	must(manager, http.MethodPut, "/api/apps/blog/env/SECRET", `{"value":"hunter2"}`, http.StatusOK)
	must(manager, http.MethodPut, "/api/apps/blog/env/SECRET", `{"value":"hunter3"}`, http.StatusOK)
	must(manager, http.MethodPut, "/api/apps/blog/env/SECRET", `{"value":"hunter3"}`, http.StatusOK)
	must(manager, http.MethodPut, "/api/apps/blog/env/SECRET", `{"key":"TOKEN"}`, http.StatusOK)
	must(manager, http.MethodDelete, "/api/apps/blog/env/TOKEN", "", http.StatusOK)
	must(manager, http.MethodPut, "/api/apps/blog/env/KEEP", `{"value":"same-value"}`, http.StatusOK)
	must(manager, http.MethodPut, "/api/apps/blog/env",
		`{"vars":[{"key":"KEEP","value":"same-value"},{"key":"API_KEY","value":"s3cret-a"}]}`, http.StatusOK)

	var d domainBody
	_ = json.Unmarshal(must(manager, http.MethodPost, "/api/apps/blog/domains", `{"hostname":"www.example.com"}`, http.StatusCreated), &d)
	must(manager, http.MethodPost, fmt.Sprintf("/api/apps/blog/domains/%d/verify", d.ID), "", http.StatusOK)
	must(manager, http.MethodPatch, fmt.Sprintf("/api/apps/blog/domains/%d", d.ID), `{"force_active":true,"is_primary":true}`, http.StatusOK)
	must(manager, http.MethodDelete, fmt.Sprintf("/api/apps/blog/domains/%d", d.ID), "", http.StatusNoContent)

	must(manager, http.MethodPost, "/api/apps/blog/stop", "", http.StatusOK)
	must(manager, http.MethodPost, "/api/apps/blog/start", "", http.StatusOK)
	must(manager, http.MethodPost, "/api/apps/blog/restart", "", http.StatusOK)

	must(manager, http.MethodPost, "/api/apps/blog/backups/restore", `{"snapshot_id":"0123abcd"}`, http.StatusOK)
	must(manager, http.MethodGet, "/api/apps/blog/backups/89abcdef/download", "", http.StatusOK)

	must(manager, http.MethodPut, "/api/apps/blog/tasks", `{"tasks":[{"name":"migrate","service":"web","command":"bin/rails db:migrate"}]}`, http.StatusOK)
	must(manager, http.MethodPost, "/api/apps/blog/task-runs", `{"task":"migrate"}`, http.StatusAccepted)

	// The owner copies shop into blog and changes the viewer's role.
	must(f.owner, http.MethodPost, "/api/apps/blog/db-copies", `{"source":"shop","confirm":"blog"}`, http.StatusAccepted)
	must(f.owner, http.MethodPut, fmt.Sprintf("/api/members/%d/roles/blog", deployerID), `{"role":"manage"}`, http.StatusOK)
	must(f.owner, http.MethodDelete, fmt.Sprintf("/api/members/%d/roles/blog", deployerID), "", http.StatusOK)

	viewer := f.as[viewerID]
	raw := viewer.do(http.MethodGet, "/api/apps/blog/activity?limit=100", "", nil).Body.String()
	for _, secret := range []string{"hunter2", "hunter3", "s3cret-a", "same-value"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the activity holds the value %q: %s", secret, raw)
		}
	}
	list := f.activity(t, viewer, "?limit=100")
	var actions []string
	for i := len(list.Entries) - 1; i >= 0; i-- {
		e := list.Entries[i]
		want := "max"
		if strings.HasPrefix(e.Action, "member.") || strings.HasPrefix(e.Action, "db_copy.") || e.Action == actAppCreated {
			want = "octo"
		}
		if e.Actor == nil || e.Actor.Login != want {
			t.Errorf("%s by %+v, want %s", e.Action, e.Actor, want)
		}
		actions = append(actions, e.Action)
	}
	wantActions := []string{
		actAppCreated, actSettingsChanged,
		actEnvChanged, actEnvChanged, actEnvChanged, actEnvChanged, actEnvChanged, actEnvChanged,
		actDomainAdded, actDomainVerified, actDomainForced, actDomainPrimary, actDomainDeleted,
		actAppStopped, actAppStarted, actAppRestarted,
		actVolumeRestored, actDumpDownloaded,
		actTasksChanged, actTaskRunStarted,
		actDBCopyStarted, actMemberRoleSet, actMemberRoleRemove,
	}
	if !slices.Equal(actions, wantActions) {
		t.Fatalf("actions =\n%v\nwant\n%v", actions, wantActions)
	}
	at := func(i int) map[string]any { return list.Entries[len(list.Entries)-1-i].Detail }
	settings, _ := json.Marshal(at(1))
	if want := `{"changes":[{"field":"healthcheck_path","from":"/","to":"/up"},{"field":"memory_limit","from":"","to":"512m"}]}`; string(settings) != want {
		t.Errorf("settings details = %s", settings)
	}
	envDetails := []string{}
	for i := 2; i <= 7; i++ {
		b, _ := json.Marshal(at(i))
		envDetails = append(envDetails, string(b))
	}
	wantEnv := []string{
		`{"added":["SECRET"],"changed":[],"removed":[],"renamed":[],"replaced":false}`,
		`{"added":[],"changed":["SECRET"],"removed":[],"renamed":[],"replaced":false}`,
		`{"added":[],"changed":[],"removed":[],"renamed":[{"from":"SECRET","to":"TOKEN"}],"replaced":false}`,
		`{"added":[],"changed":[],"removed":["TOKEN"],"renamed":[],"replaced":false}`,
		`{"added":["KEEP"],"changed":[],"removed":[],"renamed":[],"replaced":false}`,
		`{"added":["API_KEY"],"changed":[],"removed":[],"renamed":[],"replaced":true}`,
	}
	if !slices.Equal(envDetails, wantEnv) {
		t.Errorf("env details =\n%s\nwant\n%s", strings.Join(envDetails, "\n"), strings.Join(wantEnv, "\n"))
	}
	if got := at(8); got["hostname"] != "www.example.com" || got["status"] != store.DomainPending {
		t.Errorf("domain added = %v", got)
	}
	if got := at(20); got["source"] != "shop" || got["source_kind"] != "live" {
		t.Errorf("copy = %v", got)
	}
	role, _ := json.Marshal(at(21))
	if want := fmt.Sprintf(`{"from":"deploy","member":{"id":%d,"login":"dee"},"to":"manage"}`, deployerID); string(role) != want {
		t.Errorf("role details = %s", role)
	}

	// The copy is recorded on the source too, without naming the target.
	shop, err := f.srv.store.ListActivity(t.Context(), f.app(t, "shop").ID, 0, 10)
	if err != nil || len(shop) != 2 || shop[0].Action != actDBCopySent || strings.Contains(string(shop[0].Details), "blog") {
		t.Errorf("shop activity = %+v, %v", shop, err)
	}
}

func TestActivityPagesAndAccess(t *testing.T) {
	f := newRoleFixture(t)
	for range 3 {
		if rec := f.as[managerID].do(http.MethodPost, "/api/apps/blog/restart", "", originHeader); rec.Code != http.StatusOK {
			t.Fatalf("restart: %d %s", rec.Code, rec.Body)
		}
	}
	// Created + 3 restarts.
	first := f.activity(t, f.as[viewerID], "?limit=3")
	if len(first.Entries) != 3 || first.NextBefore == nil || *first.NextBefore != first.Entries[2].ID {
		t.Fatalf("first page = %+v", first)
	}
	rest := f.activity(t, f.as[viewerID], fmt.Sprintf("?limit=3&before=%d", *first.NextBefore))
	if len(rest.Entries) != 1 || rest.Entries[0].Action != actAppCreated || rest.NextBefore != nil {
		t.Errorf("second page = %+v", rest)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?before=x", "?before=0"} {
		if rec := f.as[viewerID].do(http.MethodGet, "/api/apps/blog/activity"+q, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	// A member without a role on the app does not see it at all.
	if rec := f.as[strangerID].do(http.MethodGet, "/api/apps/blog/activity", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("stranger: %d %s", rec.Code, rec.Body)
	}
	// Refused operations record nothing.
	if rec := f.as[viewerID].do(http.MethodPost, "/api/apps/blog/stop", "", originHeader); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer stop: %d", rec.Code)
	}
	if got := f.activity(t, f.owner, ""); len(got.Entries) != 4 {
		t.Errorf("entries after a refused stop = %d", len(got.Entries))
	}
}

func TestSettingChangesHideSecrets(t *testing.T) {
	before := store.App{MemoryLimit: "256m"}
	after := store.App{MemoryLimit: "512m", BackupExcludeVolumes: []string{}}
	saved := settingFields
	t.Cleanup(func() { settingFields = saved })
	settingFields = append(slices.Clone(saved), settingField{
		name: "registry_password", secret: true, get: func(a store.App) any { return a.MemoryLimit },
	})
	got, _ := json.Marshal(settingChanges(before, after, "", "blog-staging"))
	want := `[{"field":"memory_limit","from":"256m","to":"512m"},{"field":"registry_password","secret":true},{"field":"promote_from","from":"","to":"blog-staging"}]`
	if string(got) != want {
		t.Errorf("changes = %s", got)
	}
}
