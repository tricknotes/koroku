package compose

import (
	"strings"
	"testing"
)

// groupProject is a normalized project as `docker compose config` prints it
// for a repository with an API, a database and a custom network.
func groupProject(t *testing.T) Project {
	t.Helper()
	p, err := ParseProject([]byte(`{
		"name": "koroku-shop-api",
		"services": {
			"api":    {"image": "example/api", "networks": {"default": null}},
			"db":     {"image": "postgres:17", "networks": {"default": null}},
			"worker": {"image": "example/api", "networks": {"backend": {"aliases": ["jobs"]}, "default": null}, "container_name": "shop-worker"},
			"sidecar": {"image": "example/proxy", "network_mode": "service:api"}
		},
		"networks": {
			"default": {"name": "koroku-shop-api_default"},
			"backend": {"name": "koroku-shop-api_backend"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGroupOverrideGolden(t *testing.T) {
	got, err := GroupOverride(GroupOverrideParams{
		Project:  groupProject(t),
		Slug:     "shop-api",
		Network:  "koroku_group_3",
		Services: []string{"worker", "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "group.override.yaml", got)
}

func TestGroupOverrideWithoutServices(t *testing.T) {
	got, err := GroupOverride(GroupOverrideParams{Project: groupProject(t), Slug: "shop-api", Network: "koroku_group_3"})
	if err != nil || got != nil {
		t.Errorf("GroupOverride() = %q, %v; want nil, nil", got, err)
	}
}

func TestGroupOverrideErrors(t *testing.T) {
	p := groupProject(t)
	taken := groupProject(t)
	taken.Networks["koroku_group"] = Network{Name: "koroku-shop-api_koroku_group"}
	cases := []struct {
		name   string
		params GroupOverrideParams
		want   string
	}{
		{"missing service", GroupOverrideParams{Project: p, Slug: "s", Network: "koroku_group_1", Services: []string{"cache"}}, "not defined"},
		{"network mode", GroupOverrideParams{Project: p, Slug: "s", Network: "koroku_group_1", Services: []string{"sidecar"}}, "network_mode"},
		{"wrong network name", GroupOverrideParams{Project: p, Slug: "s", Network: "koroku-other_default", Services: []string{"api"}}, "unexpected name"},
		{"key taken", GroupOverrideParams{Project: taken, Slug: "s", Network: "koroku_group_1", Services: []string{"api"}}, "koroku_group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GroupOverride(tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
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

// mergedGroupProject is the project after the group override was applied.
func mergedGroupProject(t *testing.T, network string, external bool, services ...string) Project {
	t.Helper()
	p := groupProject(t)
	p.Networks["koroku_group"] = Network{Name: network, External: external}
	for _, name := range services {
		svc := p.Services[name]
		svc.Networks["koroku_group"] = &ServiceNetwork{Aliases: []string{name + ".shop-api.koroku.internal"}}
		p.Services[name] = svc
	}
	delete(p.Services, "sidecar")
	return p
}

func TestPolicyGroupNetwork(t *testing.T) {
	params := PolicyParams{
		ProjectName:   "koroku-shop-api",
		WebService:    "api",
		GroupNetwork:  "koroku_group_3",
		GroupServices: []string{"api"},
	}
	rules := func(r PolicyResult) []string {
		var out []string
		for _, v := range r.Violations {
			out = append(out, v.Rule+":"+v.Service)
		}
		return out
	}

	t.Run("own group, chosen service", func(t *testing.T) {
		got := CheckPolicy(mergedGroupProject(t, "koroku_group_3", true, "api"), params)
		if !got.OK() {
			t.Errorf("violations: %v", got.Violations)
		}
	})
	t.Run("service not chosen", func(t *testing.T) {
		got := CheckPolicy(mergedGroupProject(t, "koroku_group_3", true, "api", "db"), params)
		if strings.Join(rules(got), ",") != "group_network:db" {
			t.Errorf("violations: %v", rules(got))
		}
	})
	t.Run("another group, even relaxed", func(t *testing.T) {
		relaxed := params
		relaxed.Relax = true
		got := CheckPolicy(mergedGroupProject(t, "koroku_group_4", true, "api"), relaxed)
		if strings.Join(rules(got), ",") != "group_network:" {
			t.Errorf("violations: %v", rules(got))
		}
	})
	t.Run("not in a group", func(t *testing.T) {
		outside := params
		outside.GroupNetwork, outside.GroupServices, outside.Relax = "", nil, true
		got := CheckPolicy(mergedGroupProject(t, "koroku_group_3", true, "api"), outside)
		if strings.Join(rules(got), ",") != "group_network:" {
			t.Errorf("violations: %v", rules(got))
		}
	})
	t.Run("declared by the project instead of external", func(t *testing.T) {
		got := CheckPolicy(mergedGroupProject(t, "koroku_group_3", false, "api"), params)
		if strings.Join(rules(got), ",") != "group_network:" {
			t.Errorf("violations: %v", rules(got))
		}
	})
}

func TestCheckGroupNames(t *testing.T) {
	p := groupProject(t)
	self := MemberOf("shop-api", p, []string{"api", "worker"})
	if strings.Join(self.Joined, ",") != "api,worker,shop-worker" {
		t.Errorf("Joined = %v", self.Joined)
	}

	// Another app with its own "db" and "api", which puts "db" on the network.
	other := GroupMember{Slug: "shop-web", Names: []string{"api", "db", "web"}, Joined: []string{"db"}}
	got := CheckGroupNames(self, []GroupMember{other})
	if len(got) != 2 || got[0].Service != "api" || got[1].Service != "db" {
		t.Fatalf("violations = %v", got)
	}
	for _, v := range got {
		if v.Rule != RuleGroupNameConflict || v.Relaxable {
			t.Errorf("violation %v: want a non-relaxable %s", v, RuleGroupNameConflict)
		}
	}

	quiet := GroupMember{Slug: "shop-web", Names: []string{"web", "cache"}, Joined: []string{"web"}}
	if got := CheckGroupNames(self, []GroupMember{quiet}); len(got) != 0 {
		t.Errorf("violations = %v, want none", got)
	}
}
