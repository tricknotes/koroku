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
	_, err := f.store.SetAppGroup(ctx, app.ID, groupID, services, time.Now())
	return err
}

func (f *fakeGroups) CreateLink(ctx context.Context, app store.App, name string, target store.App, targetService string, services []string) (store.Link, error) {
	if f.setErr != nil {
		return store.Link{}, f.setErr
	}
	return f.store.CreateLink(ctx, app.ID, name, target.ID, targetService, services, time.Now())
}

func (f *fakeGroups) DeleteLink(ctx context.Context, app store.App, id int64) error {
	return f.store.DeleteLink(ctx, app.ID, id)
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
	if g.Name != "shop" || len(g.Members) != 0 {
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
		list.Groups[0].Members[0].Services[0] != "api" {
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

	groups.setErr = &deploy.LinkServiceError{Violations: []compose.Violation{
		{Service: "worker", Rule: compose.RuleLinkService, Message: "the service sets container_name"},
	}}
	rec := request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 2, "services": ["worker"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"services":"worker: the service sets container_name"`) {
		t.Errorf("service that cannot join: %d %s", rec.Code, rec.Body)
	}
	groups.setErr = deploy.ErrBusy
	rec = request(t, h, http.MethodPut, "/api/apps/shop-api/group", `{"group_id": 2, "services": []}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("busy: %d %s", rec.Code, rec.Body)
	}
}

func TestLinkAPI(t *testing.T) {
	h, groups := newGroupServer(t)
	createApp(t, h, "accounts")
	createApp(t, h, "shop")
	createApp(t, h, "outsider")
	request(t, h, http.MethodPost, "/api/groups", `{"name": "team", "environment": "staging"}`)
	request(t, h, http.MethodPut, "/api/apps/accounts/group", `{"group_id": 1, "services": ["auth"]}`)

	// Not in a group yet.
	rec := request(t, h, http.MethodPost, "/api/apps/shop/links", `{"name": "auth", "target_app": "accounts", "target_service": "auth", "services": ["web"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"target_app"`) {
		t.Errorf("outside a group: %d %s", rec.Code, rec.Body)
	}
	request(t, h, http.MethodPut, "/api/apps/shop/group", `{"group_id": 1, "services": []}`)

	for body, field := range map[string]string{
		`{"name": "Auth", "target_app": "accounts", "target_service": "auth", "services": ["web"]}`:    "name",
		`{"name": "auth", "target_app": "accounts", "target_service": "db", "services": ["web"]}`:      "target_service",
		`{"name": "auth", "target_app": "outsider", "target_service": "auth", "services": ["web"]}`:    "target_app",
		`{"name": "auth", "target_app": "shop", "target_service": "auth", "services": ["web"]}`:        "target_app",
		`{"name": "auth", "target_app": "accounts", "target_service": "auth", "services": []}`:         "services",
		`{"name": "auth", "target_app": "accounts", "target_service": "auth", "services": ["w", "w"]}`: "services",
	} {
		rec := request(t, h, http.MethodPost, "/api/apps/shop/links", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"`+field+`"`) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}

	rec = request(t, h, http.MethodPost, "/api/apps/shop/links", `{"name": "auth", "target_app": "accounts", "target_service": "auth", "services": ["web", "worker"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	link := decode[linkJSON](t, rec)
	if link.Name != "auth" || link.TargetApp != "accounts" || !strings.HasPrefix(link.Network, "koroku_link_") {
		t.Errorf("link = %+v", link)
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/shop/links", `{"name": "auth", "target_app": "accounts", "target_service": "auth", "services": ["web"]}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "link_name_taken") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
	}

	// The name shadows a service of the deployed consumer.
	groups.setErr = &deploy.LinkServiceError{Violations: []compose.Violation{{Service: "db", Rule: compose.RuleLinkService, Message: "the link name \"db\" is also a service of this app"}}}
	rec = request(t, h, http.MethodPost, "/api/apps/shop/links", `{"name": "db", "target_app": "accounts", "target_service": "auth", "services": ["web"]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"name":"db: the link name`) {
		t.Errorf("shadowing name: %d %s", rec.Code, rec.Body)
	}
	groups.setErr = nil

	rec = request(t, h, http.MethodGet, "/api/apps/shop/links", "")
	if !strings.Contains(rec.Body.String(), `"target_service":"auth"`) {
		t.Errorf("list: %s", rec.Body)
	}
	rec = request(t, h, http.MethodGet, "/api/groups", "")
	if !strings.Contains(rec.Body.String(), `"links":[{"id":1,"name":"auth","target_app":"accounts"`) {
		t.Errorf("group with links: %s", rec.Body)
	}

	if rec := request(t, h, http.MethodDelete, "/api/apps/shop/links/1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodDelete, "/api/apps/shop/links/1", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "link_not_found") {
		t.Errorf("delete again: %d %s", rec.Code, rec.Body)
	}
}
