package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDomainLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")

	d, err := s.AddDomain(ctx, app.ID, "www.example.org", testNow)
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if d.Status != DomainPending || d.IsPrimary || d.ForceActive || d.LastCheckedAt != nil {
		t.Errorf("new domain = %+v", d)
	}
	// Hostnames are unique across apps.
	if _, err := s.AddDomain(ctx, other.ID, "www.example.org", testNow); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate hostname err = %v", err)
	}
	// A domain is only visible through its own app.
	if _, err := s.GetDomain(ctx, other.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-app GetDomain err = %v", err)
	}

	checked := testNow.Add(time.Minute)
	if err := s.RecordDomainCheck(ctx, d.ID, false, "hostname does not resolve", checked); err != nil {
		t.Fatalf("RecordDomainCheck: %v", err)
	}
	d, _ = s.GetDomain(ctx, app.ID, d.ID)
	if d.Status != DomainPending || d.LastError != "hostname does not resolve" || d.LastCheckedAt == nil || !d.LastCheckedAt.Equal(checked) {
		t.Errorf("after failed check = %+v", d)
	}
	pending, _ := s.ListPendingDomains(ctx)
	if len(pending) != 1 {
		t.Errorf("pending = %+v", pending)
	}

	if err := s.RecordDomainCheck(ctx, d.ID, true, "", checked); err != nil {
		t.Fatalf("RecordDomainCheck: %v", err)
	}
	active, _ := s.ListActiveDomains(ctx)
	if len(active) != 1 || active[0].Hostname != "www.example.org" || active[0].LastError != "" {
		t.Errorf("active = %+v", active)
	}

	if err := s.DeleteDomain(ctx, other.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-app delete err = %v", err)
	}
	if err := s.DeleteDomain(ctx, app.ID, d.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if list, _ := s.ListDomains(ctx, app.ID); len(list) != 0 {
		t.Errorf("domains after delete = %+v", list)
	}
}

func TestPrimaryDomain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	apex, _ := s.AddDomain(ctx, app.ID, "example.org", testNow)
	www, _ := s.AddDomain(ctx, app.ID, "www.example.org", testNow)

	if err := s.SetDomainPrimary(ctx, app.ID, www.ID, true); !errors.Is(err, ErrDomainNotActive) {
		t.Errorf("a pending domain must not become primary: %v", err)
	}
	_ = s.RecordDomainCheck(ctx, apex.ID, true, "", testNow)
	_ = s.RecordDomainCheck(ctx, www.ID, true, "", testNow)

	if err := s.SetDomainPrimary(ctx, app.ID, www.ID, true); err != nil {
		t.Fatalf("SetDomainPrimary: %v", err)
	}
	// Choosing another primary moves the role.
	if err := s.SetDomainPrimary(ctx, app.ID, apex.ID, true); err != nil {
		t.Fatalf("SetDomainPrimary: %v", err)
	}
	list, _ := s.ListDomains(ctx, app.ID)
	if len(list) != 2 || !list[0].IsPrimary || list[1].IsPrimary {
		t.Errorf("primary flags = %+v", list)
	}

	// A primary domain that stops verifying loses the role with its status.
	if err := s.RecordDomainCheck(ctx, apex.ID, false, "mismatch", testNow); err != nil {
		t.Fatalf("RecordDomainCheck: %v", err)
	}
	got, _ := s.GetDomain(ctx, app.ID, apex.ID)
	if got.Status != DomainPending || got.IsPrimary {
		t.Errorf("after failed check = %+v", got)
	}

	if err := s.SetDomainPrimary(ctx, app.ID, www.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainPrimary(ctx, app.ID, www.ID, false); err != nil {
		t.Fatalf("clearing primary: %v", err)
	}
	if got, _ := s.GetDomain(ctx, app.ID, www.ID); got.IsPrimary {
		t.Error("primary was not cleared")
	}
	if err := s.SetDomainPrimary(ctx, app.ID, 999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown domain err = %v", err)
	}
}

func TestForceActiveDomain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d, _ := s.AddDomain(ctx, app.ID, "cdn.example.org", testNow)

	if err := s.SetDomainForceActive(ctx, app.ID, d.ID, true); err != nil {
		t.Fatalf("SetDomainForceActive: %v", err)
	}
	// A forced domain stays active even when DNS does not match (CDN in front).
	if err := s.RecordDomainCheck(ctx, d.ID, false, "mismatch", testNow); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDomain(ctx, app.ID, d.ID)
	if got.Status != DomainActive || !got.ForceActive || got.LastError != "mismatch" {
		t.Errorf("forced domain = %+v", got)
	}
	if err := s.SetDomainPrimary(ctx, app.ID, d.ID, true); err != nil {
		t.Fatalf("a forced domain may be primary: %v", err)
	}

	// Removing the force returns it to pending and drops the primary role.
	if err := s.SetDomainForceActive(ctx, app.ID, d.ID, false); err != nil {
		t.Fatalf("SetDomainForceActive(false): %v", err)
	}
	got, _ = s.GetDomain(ctx, app.ID, d.ID)
	if got.Status != DomainPending || got.ForceActive || got.IsPrimary {
		t.Errorf("after unforce = %+v", got)
	}
	// Unforcing a domain that was never forced changes nothing.
	_ = s.RecordDomainCheck(ctx, d.ID, true, "", testNow)
	if err := s.SetDomainForceActive(ctx, app.ID, d.ID, false); err != nil {
		t.Fatalf("no-op unforce: %v", err)
	}
	if got, _ := s.GetDomain(ctx, app.ID, d.ID); got.Status != DomainActive {
		t.Errorf("a verified domain must stay active: %+v", got)
	}
	if err := s.SetDomainForceActive(ctx, app.ID, 999, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown domain err = %v", err)
	}
}

func TestDeletingAppRemovesDomains(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if _, err := s.AddDomain(ctx, app.ID, "www.example.org", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	other := mustCreateApp(t, s, "shop")
	if _, err := s.AddDomain(ctx, other.ID, "www.example.org", testNow); err != nil {
		t.Errorf("the hostname must be free again: %v", err)
	}
}
