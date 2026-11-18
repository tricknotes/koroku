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
		TokenHash: "hash-1", GitHubUserID: 42, Login: "octo", AvatarURL: "https://example.com/a.png",
		CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(ctx, sess); err == nil {
		t.Error("a duplicate token hash must be rejected")
	}

	got, err := s.GetSession(ctx, "hash-1", testNow.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.GitHubUserID != 42 || got.Login != "octo" || got.AvatarURL != sess.AvatarURL || !got.ExpiresAt.Equal(sess.ExpiresAt) {
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
	other.TokenHash, other.ExpiresAt = "hash-2", testNow.Add(48*time.Hour)
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
