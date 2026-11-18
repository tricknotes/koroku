package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	sess := Session{
		TokenHash: "hash-1", ID: "id-1", GitHubUserID: 42, Login: "octo", AvatarURL: "https://example.com/a.png",
		CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(ctx, sess); err == nil {
		t.Error("a duplicate token hash must be rejected")
	}
	dupID := sess
	dupID.TokenHash = "hash-x"
	if err := s.CreateSession(ctx, dupID); err == nil {
		t.Error("a duplicate id must be rejected")
	}

	got, err := s.GetSession(ctx, "hash-1", testNow.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.GitHubUserID != 42 || got.ID != "id-1" || got.Login != "octo" || got.AvatarURL != sess.AvatarURL ||
		!got.ExpiresAt.Equal(sess.ExpiresAt) || !got.LastSeenAt.Equal(sess.CreatedAt) {
		t.Errorf("session = %+v", got)
	}
	if _, err := s.GetSession(ctx, "unknown", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session err = %v", err)
	}
	// Expiry is exclusive: at the expiry instant the session is gone.
	if _, err := s.GetSession(ctx, "hash-1", testNow.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session err = %v", err)
	}

	other := sess
	other.TokenHash, other.ID, other.ExpiresAt = "hash-2", "id-2", testNow.Add(48*time.Hour)
	_ = s.CreateSession(ctx, other)
	if err := s.DeleteExpiredSessions(ctx, testNow.Add(2*time.Hour)); err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count)
	if count != 1 {
		t.Errorf("sessions left = %d, want 1", count)
	}

	if err := s.DeleteSession(ctx, "hash-2"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "hash-2", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session err = %v", err)
	}
	if err := s.DeleteSession(ctx, "hash-2"); err != nil {
		t.Errorf("deleting twice must not fail: %v", err)
	}
}

func TestSessionListTouchAndRevoke(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	add := func(hash, id string, user int64, created time.Time, expires time.Duration) {
		t.Helper()
		err := s.CreateSession(ctx, Session{
			TokenHash: hash, ID: id, GitHubUserID: user,
			CreatedAt: created, ExpiresAt: created.Add(expires),
		})
		if err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
	}
	add("h-old", "old", 42, testNow, time.Hour)
	add("h-new", "new", 42, testNow.Add(time.Minute), time.Hour)
	add("h-gone", "gone", 42, testNow.Add(-2*time.Hour), time.Hour) // expired
	add("h-other", "other", 7, testNow, time.Hour)

	list, err := s.ListSessions(ctx, 42, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 2 || list[0].ID != "new" || list[1].ID != "old" {
		t.Fatalf("ListSessions = %+v, want new then old", list)
	}

	seen := testNow.Add(5 * time.Minute)
	if err := s.TouchSession(ctx, "h-old", seen, "203.0.113.5", "Agent/1"); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	got, _ := s.GetSession(ctx, "h-old", seen)
	if !got.LastSeenAt.Equal(seen) || got.IP != "203.0.113.5" || got.UserAgent != "Agent/1" {
		t.Errorf("touched session = %+v", got)
	}

	if err := s.DeleteSessionByID(ctx, 7, "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting another user's session err = %v, want ErrNotFound", err)
	}
	n, err := s.DeleteOtherSessions(ctx, 42, "h-new", testNow.Add(2*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("DeleteOtherSessions = %d, %v; want 1 (expired ones are not counted)", n, err)
	}
	if _, err := s.GetSession(ctx, "h-other", testNow); err != nil {
		t.Errorf("another user's session must survive: %v", err)
	}
	if err := s.DeleteSessionByID(ctx, 42, "new"); err != nil {
		t.Fatalf("DeleteSessionByID: %v", err)
	}
	if list, _ := s.ListSessions(ctx, 42, testNow); len(list) != 0 {
		t.Errorf("sessions left = %+v", list)
	}
}

func TestLoginEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := range loginEventsKept + 5 {
		err := s.AddLoginEvent(ctx, LoginEvent{
			At: testNow.Add(time.Duration(i) * time.Second), Outcome: LoginSucceeded,
			GitHubUserID: 42, Login: "octo", IP: "203.0.113.5", UserAgent: "Agent/1",
		})
		if err != nil {
			t.Fatalf("AddLoginEvent: %v", err)
		}
	}
	if err := s.AddLoginEvent(ctx, LoginEvent{At: testNow, Outcome: "bogus", GitHubUserID: 1}); err == nil {
		t.Error("an unknown outcome must be rejected")
	}
	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_events`).Scan(&count)
	if count != loginEventsKept {
		t.Errorf("events kept = %d, want %d", count, loginEventsKept)
	}
	got, err := s.ListLoginEvents(ctx, 3)
	if err != nil {
		t.Fatalf("ListLoginEvents: %v", err)
	}
	if len(got) != 3 || !got[0].At.Equal(testNow.Add(time.Duration(loginEventsKept+4)*time.Second)) || got[0].Login != "octo" {
		t.Errorf("ListLoginEvents = %+v", got)
	}
}
