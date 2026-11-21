package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
)

func TestRegistryCredentialRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")
	box, err := secrets.NewBox(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetRegistryCredential(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no credential yet: err = %v", err)
	}
	sealed, err := box.Seal([]byte("ghp_secret"), secrets.RegistryTokenContext(app.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("ghp_secret")) {
		t.Fatal("the sealed token contains the plain text")
	}
	in := RegistryCredential{AppID: app.ID, Registry: "ghcr.io", Username: "octo", TokenEncrypted: sealed, UpdatedAt: testNow}
	if err := s.PutRegistryCredential(ctx, in); err != nil {
		t.Fatalf("PutRegistryCredential: %v", err)
	}
	got, err := s.GetRegistryCredential(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetRegistryCredential: %v", err)
	}
	if got.Registry != "ghcr.io" || got.Username != "octo" || !got.UpdatedAt.Equal(testNow) {
		t.Errorf("got %+v", got)
	}
	plain, err := box.Open(got.TokenEncrypted, secrets.RegistryTokenContext(app.ID))
	if err != nil || string(plain) != "ghp_secret" {
		t.Errorf("Open = %q, %v", plain, err)
	}
	// The ciphertext is bound to its app: copied to another app it does not open.
	if _, err := box.Open(got.TokenEncrypted, secrets.RegistryTokenContext(other.ID)); err == nil {
		t.Error("the token opened under another app's context")
	}
	if _, err := s.GetRegistryCredential(ctx, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the other app has no credential: err = %v", err)
	}

	// Saving again replaces it.
	in.Registry, in.Username, in.UpdatedAt = "registry.example.com:5000", "bot", testNow.Add(time.Hour)
	if err := s.PutRegistryCredential(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRegistryCredential(ctx, app.ID); got.Registry != "registry.example.com:5000" || got.Username != "bot" {
		t.Errorf("after replace: %+v", got)
	}

	if err := s.DeleteRegistryCredential(ctx, app.ID); err != nil {
		t.Fatalf("DeleteRegistryCredential: %v", err)
	}
	if err := s.DeleteRegistryCredential(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}

	// Deleting the app deletes its credential.
	in.AppID = other.ID
	in.TokenEncrypted, _ = box.Seal([]byte("t"), secrets.RegistryTokenContext(other.ID))
	if err := s.PutRegistryCredential(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials left after deleting the app: %d, %v", n, err)
	}
}
