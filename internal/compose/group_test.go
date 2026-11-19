package compose

import (
	"strings"
	"testing"
)

// groupProject is a normalized project as `docker compose config` prints it
// for a repository with an API, a database and a few unusual services.
func groupProject(t *testing.T) Project {
	t.Helper()
	p, err := ParseProject([]byte(`{
		"name": "koroku-shop-api",
		"services": {
			"api":     {"image": "example/api", "networks": {"default": null}},
			"db":      {"image": "postgres:17", "networks": {"default": null}},
			"worker":  {"image": "example/api", "networks": {"default": null}, "container_name": "shop-worker"},
			"sidecar": {"image": "example/proxy", "network_mode": "service:api"},
			"Legacy":  {"image": "example/legacy", "networks": {"default": null}}
		},
		"networks": {"default": {"name": "koroku-shop-api_default"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckGroupServices(t *testing.T) {
	p := groupProject(t)
	if got := CheckGroupServices(p, []string{"api", "db"}); len(got) != 0 {
		t.Errorf("ordinary services: %v", got)
	}
	got := CheckGroupServices(p, []string{"worker", "sidecar", "cache", "Legacy"})
	var summary []string
	for _, v := range got {
		if v.Rule != RuleGroupService || v.Relaxable {
			t.Errorf("violation %v: want a non-relaxable %s", v, RuleGroupService)
		}
		summary = append(summary, v.Service)
	}
	if strings.Join(summary, ",") != "Legacy,cache,sidecar,worker" {
		t.Errorf("violations for %v", summary)
	}
	for _, v := range got {
		want := map[string]string{"Legacy": "DNS label", "cache": "not defined", "sidecar": "network_mode", "worker": "container_name"}[v.Service]
		if !strings.Contains(v.Message, want) {
			t.Errorf("%s: message %q lacks %q", v.Service, v.Message, want)
		}
	}
}

func TestValidGroupService(t *testing.T) {
	for name, want := range map[string]bool{
		"api": true, "api-v2": true, "a": true, "9": true,
		"API": false, "api_v2": false, "api.v2": false, "-api": false, "api-": false, "": false,
	} {
		if got := ValidGroupService(name); got != want {
			t.Errorf("ValidGroupService(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPolicyRefusesGroupNetworks(t *testing.T) {
	for _, external := range []bool{true, false} {
		p := groupProject(t)
		delete(p.Services, "sidecar")
		delete(p.Services, "worker")
		p.Networks["shared"] = Network{Name: "koroku_group_3", External: external}
		svc := p.Services["api"]
		svc.Networks["shared"] = nil
		p.Services["api"] = svc
		got := CheckPolicy(p, PolicyParams{ProjectName: "koroku-shop-api", WebService: "api", Relax: true})
		if len(got.Violations) != 1 || got.Violations[0].Rule != RuleGroupNetwork {
			t.Errorf("external=%v: violations %v, waived %v", external, got.Violations, got.Waived)
		}
	}
}
