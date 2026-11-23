package compose

import (
	"slices"
	"strings"
	"testing"
)

func blueGreenParams(p Project) OverrideParams {
	return OverrideParams{
		Project:    p,
		WebService: "web", WebPort: 3000, HostPort: 20001,
		EnvFile:       "/var/lib/koroku/apps/blog/env",
		DefaultMemory: "512m",
		ProjectName:   "koroku-blog",
		ReleaseTag:    "r43",
		Production:    true,
		BlueGreen: &BlueGreenParams{
			ComposeFile:    "compose.yaml",
			AltHostPort:    20002,
			ReleaseEnvFile: "/var/lib/koroku/apps/blog/env.release",
		},
	}
}

func TestBlueGreenOverrideGolden(t *testing.T) {
	params := blueGreenParams(loadProject(t, "rails-postgres"))
	got, err := Override(params)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "rails-postgres.bluegreen.override.yaml", got)

	// The database keeps its configuration from one release to the next:
	// it does not read the commit variables.
	if want := []string{"migrate", "web", "web-blue", "web-green", "worker"}; !slices.Equal(params.ReleaseEnvServices(), want) {
		t.Errorf("ReleaseEnvServices = %v, want %v", params.ReleaseEnvServices(), want)
	}
	// Reservations are recorded under the public service, once.
	if _, ok := params.Reservations()["web"]; !ok || len(params.Reservations()) != 3 {
		t.Errorf("Reservations = %v", params.Reservations())
	}

	// An image-only public service keeps its image and pull policy; a
	// service whose image follows the commit reads the commit variables.
	image := blueGreenParams(loadProject(t, "image-only"))
	image.WebPort = 80
	image.ReleaseTag = ""
	image.Production = false
	image.BlueGreen.CommitImages = []string{"cache"}
	got, err = Override(image)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "image-only.bluegreen.override.yaml", got)

	bad := blueGreenParams(loadProject(t, "rails-postgres"))
	bad.BlueGreen.AltHostPort = bad.HostPort
	if _, err := Override(bad); err == nil {
		t.Error("the same port for both colours was accepted")
	}
}

func TestCommitImageServices(t *testing.T) {
	actual := Project{Services: map[string]Service{
		"web":    {Build: &Build{}},
		"worker": {Image: "ghcr.io/octo/app:sha-aaaaaaa"},
		"db":     {Image: "postgres:17"},
	}}
	probe := Project{Services: map[string]Service{
		"web":    {Build: &Build{}},
		"worker": {Image: "ghcr.io/octo/app:sha-0000000"},
		"db":     {Image: "postgres:17"},
	}}
	if got := CommitImageServices(actual, probe); !slices.Equal(got, []string{"worker"}) {
		t.Errorf("CommitImageServices = %v", got)
	}
}

func TestCheckBlueGreen(t *testing.T) {
	var b strings.Builder
	for _, v := range CheckBlueGreen(loadProject(t, "bluegreen-violations"), "web") {
		if v.Relaxable {
			t.Errorf("%s must not be relaxable", v)
		}
		b.WriteString(v.String() + "\n")
	}
	checkGolden(t, "bluegreen-violations.policy.txt", []byte(b.String()))

	if got := CheckBlueGreen(loadProject(t, "rails-postgres"), "web"); len(got) != 0 {
		t.Errorf("rails-postgres: %v", got)
	}
	// A one-shot public service cannot be replaced side by side.
	oneShot := loadProject(t, "rails-postgres")
	if got := CheckBlueGreen(oneShot, "migrate"); len(got) != 2 || !strings.Contains(got[0].Message, "one-shot") {
		t.Errorf("one-shot: %v", got)
	}

	output := `cannot extend service "web-blue" in /x/koroku.override.yaml: service "web" not found in compose.yaml`
	if !ExtendsFailure(output, "web") || ExtendsFailure(output, "app") || ExtendsFailure("yaml: line 3", "web") {
		t.Error("ExtendsFailure")
	}
}

// TestBlueGreenPolicyPorts checks the merged project of a blue/green
// release: each colour publishes its own port, the public service none.
func TestBlueGreenPolicyPorts(t *testing.T) {
	bind := func(port string) []Port {
		return []Port{{HostIP: "127.0.0.1", Target: 3000, Published: port, Protocol: "tcp"}}
	}
	params := PolicyParams{
		WebService: "web", WebPort: 3000, HostPort: 20001,
		BlueGreen: true, AltHostPort: 20002,
		EnvFile: "/apps/blog/env", ReleaseEnvFile: "/apps/blog/env.release",
		ReleaseDir: "/apps/blog/releases/43",
	}
	release := []EnvFile{{Path: "/apps/blog/env"}, {Path: "/apps/blog/env.release"}}
	good := Project{Services: map[string]Service{
		"web":       {EnvFiles: release, StopGracePeriodRaw: "4m"},
		"web-blue":  {Ports: bind("20001"), EnvFiles: release, StopGracePeriodRaw: "4m"},
		"web-green": {Ports: bind("20002"), EnvFiles: release, StopGracePeriodRaw: "4m"},
		"db":        {StopGracePeriodRaw: "4m"},
	}}
	if r := CheckPolicy(good, params); !r.OK() {
		t.Errorf("good project: %v", r.Violations)
	}

	swapped := Project{Services: map[string]Service{
		"web":       {Ports: bind("20001")},
		"web-blue":  {Ports: bind("20002")},
		"web-green": {Ports: bind("20001")},
	}}
	r := CheckPolicy(swapped, params)
	var services []string
	for _, v := range r.Violations {
		if v.Rule == RulePortExposure {
			services = append(services, v.Service)
		}
	}
	if !slices.Equal(services, []string{"web", "web-blue", "web-green"}) {
		t.Errorf("port findings = %v", r.Violations)
	}
}
