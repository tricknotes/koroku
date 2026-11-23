package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBlueGreenSettingsAndSecondPort(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog := mustCreateApp(t, s, "blog") // 20000
	shop := mustCreateApp(t, s, "shop") // 20001
	if blog.BlueGreen || blog.BlueGreenDrainSeconds != DefaultBlueGreenDrainSeconds || blog.AltHostPort != 0 || blog.WebColor != "" {
		t.Fatalf("defaults = %+v", blog)
	}

	blog.BlueGreen = true
	blog.BlueGreenDrainSeconds = 30
	if err := s.UpdateApp(ctx, blog, testNow); err != nil {
		t.Fatal(err)
	}
	port, err := s.AllocateAltHostPort(ctx, blog.ID, 20000, 20999)
	if err != nil {
		t.Fatal(err)
	}
	if port != 20002 {
		t.Errorf("second port = %d, want the lowest free one, 20002", port)
	}
	// Kept when asked again, and when blue/green is turned off.
	if again, err := s.AllocateAltHostPort(ctx, blog.ID, 20000, 20999); err != nil || again != port {
		t.Errorf("again = %d, %v", again, err)
	}
	got, err := s.GetAppByID(ctx, blog.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.BlueGreen || got.BlueGreenDrainSeconds != 30 || got.AltHostPort != 20002 {
		t.Errorf("app = %+v", got)
	}
	got.BlueGreen = false
	if err := s.UpdateApp(ctx, got, testNow); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetAppByID(ctx, blog.ID); got.AltHostPort != 20002 {
		t.Errorf("second port after turning off = %d", got.AltHostPort)
	}

	// New apps skip the second port too.
	third := mustCreateApp(t, s, "third")
	if third.HostPort != 20003 {
		t.Errorf("third host port = %d, want 20003", third.HostPort)
	}
	// Exhausted range.
	if _, err := s.AllocateAltHostPort(ctx, shop.ID, 20000, 20003); !errors.Is(err, ErrNoFreePort) {
		t.Errorf("full range: %v", err)
	}

	// The database refuses a second port that another app uses as either.
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET alt_host_port = ? WHERE id = ?`, 20000, shop.ID); err == nil {
		t.Error("an alt_host_port equal to another app's host_port was accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET alt_host_port = ? WHERE id = ?`, 20002, shop.ID); err == nil {
		t.Error("a duplicate alt_host_port was accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET alt_host_port = ? WHERE id = ?`, shop.HostPort, shop.ID); err == nil {
		t.Error("an alt_host_port equal to the app's own host_port was accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET host_port = ? WHERE id = ?`, 20002, shop.ID); err == nil {
		t.Error("a host_port equal to another app's alt_host_port was accepted")
	}
}

func TestWebColor(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if _, err := s.AllocateAltHostPort(ctx, app.ID, 20000, 20999); err != nil {
		t.Fatal(err)
	}
	for _, color := range []string{ColorGreen, ColorBlue, ""} {
		if err := s.SetAppWebColor(ctx, app.ID, color); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetAppByID(ctx, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantPort := 20000
		if color == ColorGreen {
			wantPort = 20001
		}
		if got.WebColor != color || got.LivePort() != wantPort {
			t.Errorf("colour %q: app colour %q, live port %d", color, got.WebColor, got.LivePort())
		}
	}
	if err := s.SetAppWebColor(ctx, app.ID, "red"); err == nil {
		t.Error("unknown colour accepted")
	}

	d, _, err := s.EnqueueDeployment(ctx, app.ID, "main", "abc", "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.WebColor != "" || d.SwitchedAt != nil {
		t.Errorf("new deployment = %+v", d)
	}
	at := testNow.Add(time.Minute)
	if err := s.SetDeploymentWebColor(ctx, d.ID, ColorGreen); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentSwitchedAt(ctx, d.ID, &at); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WebColor != ColorGreen || got.SwitchedAt == nil || !got.SwitchedAt.Equal(at) {
		t.Errorf("deployment = %+v", got)
	}
	if err := s.SetDeploymentSwitchedAt(ctx, d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetDeployment(ctx, d.ID); got.SwitchedAt != nil {
		t.Errorf("switch time not cleared: %v", got.SwitchedAt)
	}
}

func TestPublicServiceNames(t *testing.T) {
	app := App{WebService: "web", HostPort: 20000, AltHostPort: 20001}
	if name, color := app.PublicServiceOf("web-blue"); name != "web-blue" || color != "" {
		t.Errorf("without blue/green: %s %s", name, color)
	}
	if app.LiveService("web") != "web" {
		t.Errorf("live service without a colour = %s", app.LiveService("web"))
	}
	app.BlueGreen = true
	if name, color := app.PublicServiceOf("web-green"); name != "web" || color != ColorGreen {
		t.Errorf("green copy = %s %s", name, color)
	}
	if name, color := app.PublicServiceOf("worker"); name != "worker" || color != "" {
		t.Errorf("worker = %s %s", name, color)
	}
	app.WebColor = ColorGreen
	if app.LiveService("web") != "web-green" || app.LiveService("db") != "db" || app.LivePort() != 20001 {
		t.Errorf("live service %s, port %d", app.LiveService("web"), app.LivePort())
	}
	if OtherColor("") != ColorGreen || OtherColor(ColorGreen) != ColorBlue || OtherColor(ColorBlue) != ColorGreen {
		t.Error("OtherColor")
	}
}
