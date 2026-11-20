package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMembersAndRoles(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog := mustCreateApp(t, s, "blog")
	shop := mustCreateApp(t, s, "shop")

	if err := s.AddMember(ctx, Member{GitHubUserID: 7, Login: "mia", AddedAt: testNow}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, Member{GitHubUserID: 7, Login: "mia", AddedAt: testNow}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate member: err = %v", err)
	}
	if ok, _ := s.HasAnyRole(ctx, 7); ok {
		t.Error("a new member has no role")
	}
	if err := s.SetAppRole(ctx, blog.ID, 7, RoleView); err != nil {
		t.Fatalf("SetAppRole: %v", err)
	}
	if err := s.SetAppRole(ctx, blog.ID, 7, RoleManage); err != nil {
		t.Fatalf("SetAppRole again: %v", err)
	}
	if err := s.SetAppRole(ctx, shop.ID, 7, RoleDeploy); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppRole(ctx, shop.ID, 8, RoleView); !errors.Is(err, ErrNotFound) {
		t.Errorf("role for a non-member: err = %v", err)
	}
	if role, _ := s.AppRole(ctx, blog.ID, 7); role != RoleManage {
		t.Errorf("role on blog = %q", role)
	}
	members, err := s.ListMembers(ctx)
	if err != nil || len(members) != 1 || members[0].Roles[blog.ID] != RoleManage || members[0].Roles[shop.ID] != RoleDeploy {
		t.Fatalf("ListMembers = %+v, %v", members, err)
	}
	if err := s.UpdateMemberProfile(ctx, 7, "mia2", "https://avatars.example/7"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.GetMember(ctx, 7); m.Login != "mia2" || len(m.Roles) != 2 {
		t.Errorf("GetMember = %+v", m)
	}

	// Deleting an app takes its roles with it.
	if err := s.DeleteApp(ctx, shop.ID); err != nil {
		t.Fatal(err)
	}
	if roles, _ := s.RolesOf(ctx, 7); len(roles) != 1 {
		t.Errorf("roles after deleting shop = %v", roles)
	}

	// Sessions and tokens of users without a role are revoked; the owner's stay.
	for i, uid := range []int64{7, 42, 9} {
		id := string(rune('a' + i))
		if err := s.CreateSession(ctx, Session{TokenHash: "h" + id, ID: id, GitHubUserID: uid, CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateAPIToken(ctx, APIToken{ID: id, TokenHash: "t" + id, GitHubUserID: uid, CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour)}, 5); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.RevokeCredentialsWithoutAccess(ctx, 42); err != nil || n != 2 {
		t.Errorf("revoke: %d, %v (want the session and token of 9)", n, err)
	}
	if err := s.SetAppRole(ctx, blog.ID, 7, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RevokeCredentialsWithoutAccess(ctx, 42); n != 2 {
		t.Errorf("revoke after the last role: %d", n)
	}
	if list, _ := s.ListSessions(ctx, AllUsers, testNow); len(list) != 1 || list[0].GitHubUserID != 42 {
		t.Errorf("sessions left = %+v", list)
	}

	if err := s.DeleteMember(ctx, 7); err != nil {
		t.Fatalf("DeleteMember: %v", err)
	}
	if err := s.DeleteMember(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: err = %v", err)
	}
}

func TestActorIsRecorded(t *testing.T) {
	s := openTest(t)
	app := mustCreateApp(t, s, "blog")
	ctx := WithActor(context.Background(), Actor{UserID: 7, Login: "mia"})
	d, _, err := s.EnqueueDeployment(ctx, app.ID, "main", "abc", "msg", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Actor == nil || d.Actor.UserID != 7 || d.Actor.Login != "mia" {
		t.Errorf("deployment actor = %+v", d.Actor)
	}
	run, err := s.StartTaskRun(ctx, TaskRun{AppID: app.ID, Service: "web", Command: "true"}, testNow)
	if err != nil || run.Actor == nil || run.Actor.Login != "mia" {
		t.Errorf("task run actor = %+v, %v", run.Actor, err)
	}
	// Without an actor (koroku itself) nothing is recorded.
	d2, _, err := s.EnqueueDeployment(context.Background(), app.ID, "main", "abc", "msg", testNow)
	if err != nil || d2.Actor != nil {
		t.Errorf("deployment without actor = %+v, %v", d2.Actor, err)
	}
}

func TestAssignUnownedAPITokens(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.CreateAPIToken(ctx, APIToken{ID: "old", TokenHash: "h", CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour)}, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignUnownedAPITokens(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListAPITokens(ctx, 42, testNow); len(list) != 1 {
		t.Errorf("owner's tokens = %+v", list)
	}
}
