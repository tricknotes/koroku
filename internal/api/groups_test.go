package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeGroups keeps groups in the store like the engine, without Docker.
type fakeGroups struct {
	store *store.Store
	// setErr, when set, is returned by SetGroup instead of saving.
	setErr error
}

func (f *fakeGroups) CreateGroup(ctx context.Context, name, environment string) (store.Group, error) {
	return f.store.CreateGroup(ctx, name, environment, time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))
}

func (f *fakeGroups) DeleteGroup(ctx context.Context, id int64) error {
	return f.store.DeleteGroup(ctx, id)
}

func (f *fakeGroups) SetGroup(ctx context.Context, app store.App, groupID *int64, services []string) error {
	if f.setErr != nil {
		return f.setErr
	}
	return f.store.SetAppGroup(ctx, app.ID, groupID, services, time.Now())
}

func newGroupServer(t *testing.T) (http.Handler, *fakeGroups) {
	t.Helper()
	groups := &fakeGroups{}
	srv := newTestServer(t, func(o *Options) {
		groups.store = o.Store
		o.Groups = groups
	})
	return srv.Handler(), groups
}

func TestGroupAPI(t *testing.T) {
	h, _ := newGroupServer(t)
	createApp(t, h, "shop-api")
	createApp(t, h, "shop-web")

	rec := request(t, h, http.MethodPost, "/api/groups", `{"name": " shop ", "environment": "staging"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	g := decode[groupJSON](t, rec)
	if g.Name != "shop" || g.Network != "koroku_group_1" || len(g.Members) != 0 {
		t.Errorf("created group = %+v", g)
	}
	if rec := request(t, h, http.MethodPost, "/api/groups", `{"name": "shop", "environment": "staging"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "group_name_taken") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodPost, "/api/groups", `{"name": "", "environment": "test"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid: %d %s", rec.Code, rec.Body)
	}

	rec = request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 1, "services": ["api"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}
	app := decode[appJSON](t, rec)
	if app.GroupID == nil || *app.GroupID != 1 || len(app.GroupServices) != 1 {
		t.Errorf("app after joining = %+v", app)
	}

	rec = request(t, h, http.MethodGet, "/api/groups", "")
	list := decode[struct {
		Groups []groupJSON `json:"groups"`
	}](t, rec)
	if len(list.Groups) != 1 || len(list.Groups[0].Members) != 1 ||
		list.Groups[0].Members[0].Hostnames[0] != "api.shop-api.koroku.internal" {
		t.Errorf("groups = %s", rec.Body)
	}

	// A grouped app keeps its environment.
	rec = request(t, h, http.MethodPatch, "/api/apps/shop-api", `{"environment": "production"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"environment"`) {
		t.Errorf("environment change in a group: %d %s", rec.Code, rec.Body)
	}

	if rec := request(t, h, http.MethodDelete, "/api/groups/1", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "group_not_empty") {
		t.Errorf("delete non-empty: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodPatch, "/api/groups/1", `{"name": "store"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"store"`) {
		t.Errorf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": null, "services": []}`); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/groups/1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/groups/1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete again: %d %s", rec.Code, rec.Body)
	}
}

func TestSetAppGroupValidation(t *testing.T) {
	h, groups := newGroupServer(t)
	createApp(t, h, "shop-api")
	request(t, h, http.MethodPost, "/api/groups", `{"name": "live", "environment": "production"}`)
	request(t, h, http.MethodPost, "/api/groups", `{"name": "shop", "environment": "staging"}`)

	cases := []struct {
		body, field string
	}{
		{`{"group_id": 1, "services": []}`, "group_id"}, // production group, staging app
		{`{"group_id": 9, "services": []}`, "group_id"}, // no such group
		{`{"group_id": 2, "services": ["API"]}`, "services"},
		{`{"group_id": 2, "services": ["api", "api"]}`, "services"},
		{`{"group_id": null, "services": ["api"]}`, "services"},
	}
	for _, tc := range cases {
		rec := request(t, h, http.MethodPut, "/api/apps/shop-api/group", tc.body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}

	groups.setErr = &compose.PolicyError{Violations: []compose.Violation{{Service: "db", Rule: compose.RuleGroupNameConflict, Message: "conflict"}}}
	rec := request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 2, "services": ["db"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"group_name_conflict"`) {
		t.Errorf("conflict: %d %s", rec.Code, rec.Body)
	}
	groups.setErr = &deploy.UnknownServicesError{Services: []string{"cache"}}
	rec = request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 2, "services": ["cache"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cache") {
		t.Errorf("unknown service: %d %s", rec.Code, rec.Body)
	}
	groups.setErr = deploy.ErrBusy
	rec = request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 2, "services": []}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("busy: %d %s", rec.Code, rec.Body)
	}
}
