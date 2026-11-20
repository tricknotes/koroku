package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
)

func TestWebhookCheck(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Deps)
		want   Status
		says   string
	}{
		{"healthy", func(*Deps) {}, OK, "blog deploy on push"},
		{"nobody uses it", func(d *Deps) {
			d.AutoDeployApps = nil
			d.Config.GitHub.WebhookSecret = ""
		}, Skip, "no app deploys on push"},
		{"secret missing", func(d *Deps) { d.Config.GitHub.WebhookSecret = "" }, Warn, "webhook_secret is not set"},
		{"no push event", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.app.Events = []string{"pull_request"}
			d.GitHub = app
		}, Warn, "does not subscribe to push events"},
		{"no push event and no app", func(d *Deps) {
			d.AutoDeployApps = nil
			app := d.GitHub.(fakeApp)
			app.app.Events = nil
			d.GitHub = app
		}, OK, "no app deploys on push"},
		{"wrong URL", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.hook.URL = "https://example.com/hook"
			d.GitHub = app
		}, Warn, "delivers its webhook to"},
		{"hook config unreadable", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.hookErr = errors.New("boom")
			d.GitHub = app
		}, Warn, "could not read the App's webhook settings"},
		{"database unreadable", func(d *Deps) { d.AutoDeployErr = errors.New("locked") }, Warn, "could not read which apps"},
		{"App unreadable", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.appErr = errors.New("down")
			d.GitHub = app
		}, Skip, "could not be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := healthy(t)
			tt.modify(&d)
			r := byName(Run(context.Background(), d))["github.webhook"]
			if r.Status != tt.want || !strings.Contains(r.Message, tt.says) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Message, tt.want, tt.says)
			}
			if strings.Contains(r.Message, d.Config.GitHub.WebhookSecret) && d.Config.GitHub.WebhookSecret != "" {
				t.Error("the message shows the secret")
			}
		})
	}
}

var _ GitHubWebhook = (*github.Client)(nil)
