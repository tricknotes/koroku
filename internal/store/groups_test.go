package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestGroups(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

	shop, err := st.CreateGroup(ctx, "shop", EnvironmentStaging, now)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if shop.Network() != "koroku_group_1" {
		t.Errorf("Network() = %q", shop.Network())
	}
	if _, err := st.CreateGroup(ctx, "shop", EnvironmentProduction, now); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: err = %v, want ErrConflict", err)
	}
	live, err := st.CreateGroup(ctx, "live", EnvironmentProduction, now)
	if err != nil {
		t.Fatal(err)
	}

	api := mustCreateApp(t, st, "api")
	web := mustCreateApp(t, st, "web")
	if err := st.SetAppGroup(ctx, api.ID, &shop.ID, []string{"api"}, now); err != nil {
		t.Fatalf("SetAppGroup: %v", err)
	}
	if err := st.SetAppGroup(ctx, web.ID, &shop.ID, nil, now); err != nil {
		t.Fatalf("SetAppGroup without services: %v", err)
	}
	if err := st.SetAppGroup(ctx, web.ID, &live.ID, nil, now); !errors.Is(err, ErrGroupEnvironment) {
		t.Errorf("joining a production group from staging: err = %v, want ErrGroupEnvironment", err)
	}
	missing := int64(99)
	if err := st.SetAppGroup(ctx, web.ID, &missing, nil, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown group: err = %v, want ErrNotFound", err)
	}

	got, err := st.GetApp(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID == nil || *got.GroupID != shop.ID || !slices.Equal(got.GroupServices, []string{"api"}) {
		t.Errorf("api: group %v services %v", got.GroupID, got.GroupServices)
	}
	members, err := st.ListGroupMembers(ctx, shop.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0].Slug != "api" || members[1].Slug != "web" {
		t.Errorf("members = %v", members)
	}

	// The environment of a grouped app is fixed.
	got.Environment = EnvironmentProduction
	if err := st.UpdateApp(ctx, got, now); !errors.Is(err, ErrGroupEnvironment) {
		t.Errorf("changing the environment in a group: err = %v, want ErrGroupEnvironment", err)
	}

	if err := st.DeleteGroup(ctx, shop.ID); !errors.Is(err, ErrGroupNotEmpty) {
		t.Errorf("deleting a group with members: err = %v, want ErrGroupNotEmpty", err)
	}
	for _, app := range []App{api, web} {
		if err := st.SetAppGroup(ctx, app.ID, nil, nil, now); err != nil {
			t.Fatalf("leave: %v", err)
		}
	}
	got, _ = st.GetApp(ctx, "api")
	if got.GroupID != nil || len(got.GroupServices) != 0 {
		t.Errorf("after leaving: group %v services %v", got.GroupID, got.GroupServices)
	}
	if err := st.RenameGroup(ctx, shop.ID, "live"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename to a taken name: err = %v", err)
	}
	if err := st.DeleteGroup(ctx, shop.ID); err != nil {
		t.Errorf("DeleteGroup: %v", err)
	}
	groups, _ := st.ListGroups(ctx)
	if len(groups) != 1 || groups[0].Name != "live" {
		t.Errorf("groups = %v", groups)
	}
}
