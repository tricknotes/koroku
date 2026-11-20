package compose

import (
	"strings"
	"testing"
)

// linkProject is a normalized project as `docker compose config` prints it
// for a repository with a web app, a database and a few unusual services.
func linkProject(t *testing.T) Project {
	t.Helper()
	p, err := ParseProject([]byte(`{
		"name": "koroku-shop",
		"services": {
			"web":     {"image": "example/web", "networks": {"default": null}},
			"db":      {"image": "postgres:17", "networks": {"default": null}},
			"worker":  {"image": "example/web", "networks": {"default": null}, "container_name": "shop-worker"},
			"sidecar": {"image": "example/proxy", "network_mode": "service:web"},
			"Legacy":  {"image": "example/legacy", "networks": {"default": null}}
		},
		"networks": {"default": {"name": "koroku-shop_default"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckLinkServices(t *testing.T) {
	p := linkProject(t)
	if got := CheckLinkServices(p, []string{"web", "db"}); len(got) != 0 {
		t.Errorf("ordinary services: %v", got)
	}
	got := CheckLinkServices(p, []string{"worker", "sidecar", "cache", "Legacy", "cache"})
	want := map[string]string{"Legacy": "DNS label", "cache": "not defined", "sidecar": "network_mode", "worker": "container_name"}
	if len(got) != len(want) {
		t.Fatalf("violations = %v", got)
	}
	for _, v := range got {
		if v.Rule != RuleLinkService || v.Relaxable || !strings.Contains(v.Message, want[v.Service]) {
			t.Errorf("violation %+v", v)
		}
	}
}

func TestCheckLinks(t *testing.T) {
	p := linkProject(t)
	if got := CheckLinks(p, []LinkUse{{Name: "auth", Services: []string{"web"}}}); len(got) != 0 {
		t.Errorf("ordinary link: %v", got)
	}
	got := CheckLinks(p, []LinkUse{
		{Name: "db", Services: []string{"web"}},          // shadows the app's own db
		{Name: "shop-worker", Services: []string{"web"}}, // a container name of the app
		{Name: "search", Services: []string{"sidecar"}},  // cannot be attached
	})
	var names []string
	for _, v := range got {
		names = append(names, v.Service)
	}
	if strings.Join(names, ",") != "db,shop-worker,sidecar" {
		t.Errorf("violations = %v", got)
	}
}

func TestValidLinkName(t *testing.T) {
	for name, want := range map[string]bool{
		"api": true, "api-v2": true, "a": true, "9": true,
		"API": false, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false, "api_v2": false, "api.v2": false, "-api": false, "api-": false, "": false,
	} {
		if got := ValidLinkName(name); got != want {
			t.Errorf("ValidLinkName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPolicyRefusesLinkNetworks(t *testing.T) {
	for _, external := range []bool{true, false} {
		p := linkProject(t)
		for _, name := range []string{"sidecar", "worker", "Legacy"} {
			delete(p.Services, name)
		}
		p.Networks["shared"] = Network{Name: "koroku_link_1a2b3c4d_3", External: external}
		svc := p.Services["web"]
		svc.Networks["shared"] = nil
		p.Services["web"] = svc
		got := CheckPolicy(p, PolicyParams{ProjectName: "koroku-shop", WebService: "web", Relax: true})
		if len(got.Violations) != 1 || got.Violations[0].Rule != RuleLinkNetwork {
			t.Errorf("external=%v: violations %v, waived %v", external, got.Violations, got.Waived)
		}
	}
}
