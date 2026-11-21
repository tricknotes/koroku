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
		{"healthy", func(*Deps) {}, OK, "blog, shop deploy on push"},
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

func TestWorkflowCheck(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Deps)
		want   Status
		says   string
	}{
		{"healthy", func(*Deps) {}, OK, "shop deploy after their workflow succeeds"},
		{"nobody waits", func(d *Deps) {
			d.WorkflowApps = nil
			app := d.GitHub.(fakeApp)
			app.app.Events = []string{"push"}
			delete(app.app.Permissions, "actions")
			d.GitHub = app
		}, Skip, "no app waits for a workflow"},
		{"no actions permission", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.app.Permissions = map[string]string{"contents": "read", "metadata": "read", "deployments": "write"}
			d.GitHub = app
		}, Warn, "lacks actions: read"},
		{"no workflow_run event", func(d *Deps) {
			app := d.GitHub.(fakeApp)
			app.app.Events = []string{"push"}
			d.GitHub = app
		}, Warn, "does not subscribe to workflow_run events"},
		{"database unreadable", func(d *Deps) { d.WorkflowAppsErr = errors.New("locked") }, Warn, "could not read which apps wait"},
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
			results := byName(Run(context.Background(), d))
			r := results["github.workflow"]
			if r.Status != tt.want || !strings.Contains(r.Message, tt.says) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Message, tt.want, tt.says)
			}
		})
	}
	// Actions read is not an extra permission.
	if r := byName(Run(context.Background(), healthy(t)))["github.app"]; r.Status != OK {
		t.Errorf("github.app: %s %q", r.Status, r.Message)
	}
}

var _ GitHubWebhook = (*github.Client)(nil)
