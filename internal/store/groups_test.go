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
	if _, err := st.CreateGroup(ctx, "shop", EnvironmentProduction, now); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: err = %v, want ErrConflict", err)
	}
	live, err := st.CreateGroup(ctx, "live", EnvironmentProduction, now)
	if err != nil {
		t.Fatal(err)
	}

	api := mustCreateApp(t, st, "api")
	web := mustCreateApp(t, st, "web")
	if _, err := st.SetAppGroup(ctx, api.ID, &shop.ID, []string{"api"}, now); err != nil {
		t.Fatalf("SetAppGroup: %v", err)
	}
	if _, err := st.SetAppGroup(ctx, web.ID, &shop.ID, nil, now); err != nil {
		t.Fatalf("SetAppGroup without services: %v", err)
	}
	if _, err := st.SetAppGroup(ctx, web.ID, &live.ID, nil, now); !errors.Is(err, ErrGroupEnvironment) {
		t.Errorf("joining a production group from staging: err = %v, want ErrGroupEnvironment", err)
	}
	missing := int64(99)
	if _, err := st.SetAppGroup(ctx, web.ID, &missing, nil, now); !errors.Is(err, ErrNotFound) {
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
		if _, err := st.SetAppGroup(ctx, app.ID, nil, nil, now); err != nil {
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

func TestLinks(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	shop, _ := st.CreateGroup(ctx, "shop", EnvironmentStaging, now)
	other, _ := st.CreateGroup(ctx, "other", EnvironmentStaging, now)
	accounts := mustCreateApp(t, st, "accounts")
	web := mustCreateApp(t, st, "web")
	outsider := mustCreateApp(t, st, "outsider")
	if _, err := st.SetAppGroup(ctx, accounts.ID, &shop.ID, []string{"auth", "db"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAppGroup(ctx, web.ID, &shop.ID, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAppGroup(ctx, outsider.ID, &other.ID, []string{"auth"}, now); err != nil {
		t.Fatal(err)
	}

	link, err := st.CreateLink(ctx, web.ID, "auth", accounts.ID, "auth", []string{"web", "worker"}, now)
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if link.AppSlug != "web" || link.TargetAppSlug != "accounts" || len(link.Services) != 2 {
		t.Errorf("link = %+v", link)
	}
	if got := link.Network("1a2b3c4d"); got != "koroku_link_1a2b3c4d_1" {
		t.Errorf("Network() = %q", got)
	}
	if _, err := st.CreateLink(ctx, web.ID, "auth", accounts.ID, "db", []string{"web"}, now); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: err = %v", err)
	}
	for name, tc := range map[string]struct {
		target  int64
		service string
	}{
		"not exposed": {accounts.ID, "cache"},
		"other group": {outsider.ID, "auth"},
		"itself":      {web.ID, "web"},
		"no such app": {999, "auth"},
	} {
		if _, err := st.CreateLink(ctx, web.ID, "x", tc.target, tc.service, []string{"web"}, now); !errors.Is(err, ErrLinkTarget) {
			t.Errorf("%s: err = %v, want ErrLinkTarget", name, err)
		}
	}
	if _, err := st.CreateLink(ctx, web.ID, "db", accounts.ID, "db", []string{"web"}, now); err != nil {
		t.Fatal(err)
	}

	// Un-exposing "db" removes the link to it, and only that one.
	gone, err := st.SetAppGroup(ctx, accounts.ID, &shop.ID, []string{"auth"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Name != "db" {
		t.Errorf("removed = %+v", gone)
	}
	links, _ := st.ListLinksOf(ctx, accounts.ID)
	if len(links) != 1 || links[0].Name != "auth" {
		t.Errorf("links after un-exposing = %+v", links)
	}

	// Leaving removes every link from or to the app.
	gone, err = st.SetAppGroup(ctx, web.ID, nil, nil, now)
	if err != nil || len(gone) != 1 || gone[0].Name != "auth" {
		t.Errorf("leaving: %+v, %v", gone, err)
	}
	if all, _ := st.ListLinks(ctx); len(all) != 0 {
		t.Errorf("links left: %+v", all)
	}

	// Deleting an app deletes its links.
	if _, err := st.SetAppGroup(ctx, web.ID, &shop.ID, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLink(ctx, web.ID, "auth", accounts.ID, "auth", []string{"web"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteApp(ctx, accounts.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.ListLinks(ctx); len(all) != 0 {
		t.Errorf("links after deleting the target: %+v", all)
	}
}

func TestInstallationID(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	first, err := st.InstallationID(ctx)
	if err != nil || len(first) != 8 {
		t.Fatalf("InstallationID() = %q, %v", first, err)
	}
	again, _ := st.InstallationID(ctx)
	if again != first {
		t.Errorf("the ID changed: %q then %q", first, again)
	}
}
