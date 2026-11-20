package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAPITokens(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	tok := APIToken{ID: "id-1", TokenHash: "hash-1", Name: "laptop", CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour)}
	if err := s.CreateAPIToken(ctx, tok, 5); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	dup := tok
	dup.ID = "id-x"
	if err := s.CreateAPIToken(ctx, dup, 5); err == nil {
		t.Error("a duplicate hash must be rejected")
	}

	got, err := s.GetAPIToken(ctx, "hash-1", testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetAPIToken: %v", err)
	}
	if got.ID != "id-1" || got.Name != "laptop" || got.LastUsedAt != nil || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("token = %+v", got)
	}
	if _, err := s.GetAPIToken(ctx, "hash-1", testNow.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token err = %v", err)
	}
	if _, err := s.GetAPIToken(ctx, "unknown", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token err = %v", err)
	}

	used := testNow.Add(2 * time.Minute)
	if err := s.TouchAPIToken(ctx, "id-1", used, "203.0.113.5"); err != nil {
		t.Fatalf("TouchAPIToken: %v", err)
	}
	second := APIToken{ID: "id-2", TokenHash: "hash-2", Name: "desk", CreatedAt: testNow.Add(time.Minute), ExpiresAt: testNow.Add(48 * time.Hour)}
	if err := s.CreateAPIToken(ctx, second, 5); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAPITokens(ctx, testNow.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(list) != 2 || list[0].ID != "id-2" || list[1].LastUsedAt == nil || !list[1].LastUsedAt.Equal(used) || list[1].LastUsedIP != "203.0.113.5" {
		t.Errorf("list = %+v", list)
	}

	// The limit counts unexpired tokens only; expired ones are removed.
	third := APIToken{ID: "id-3", TokenHash: "hash-3", Name: "x", CreatedAt: testNow.Add(2 * time.Hour), ExpiresAt: testNow.Add(3 * time.Hour)}
	if err := s.CreateAPIToken(ctx, third, 2); err != nil {
		t.Errorf("an expired token must not count toward the limit: %v", err)
	}
	fourth := APIToken{ID: "id-4", TokenHash: "hash-4", Name: "y", CreatedAt: testNow.Add(2 * time.Hour), ExpiresAt: testNow.Add(3 * time.Hour)}
	if err := s.CreateAPIToken(ctx, fourth, 2); !errors.Is(err, ErrTooManyTokens) {
		t.Errorf("over the limit err = %v", err)
	}

	if err := s.DeleteAPIToken(ctx, "id-2"); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}
	if err := s.DeleteAPIToken(ctx, "id-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
	if _, err := s.GetAPIToken(ctx, "hash-2", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token err = %v", err)
	}
}
