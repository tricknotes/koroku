package compose

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// buildOf parses a one-service Compose config whose build section is given.
func buildOf(t *testing.T, build string) *Build {
	t.Helper()
	p, err := ParseProject([]byte(fmt.Sprintf(`{"services": {"web": {"build": %s}}}`, build)))
	if err != nil {
		t.Fatal(err)
	}
	return p.Services["web"].Build
}

func TestCompareBuilds(t *testing.T) {
	const stagingDir = "/var/lib/koroku/apps/shop-staging/releases/12"
	const prodDir = "/var/lib/koroku/apps/shop/releases/30"
	section := func(dir, args string) string {
		return fmt.Sprintf(`{"context": %q, "dockerfile": "Dockerfile", "target": "prod",
			"additional_contexts": {"assets": %q}, "args": {%s}}`, dir, dir+"/assets", args)
	}

	tests := []struct {
		name      string
		a, b      string
		wantArgs  []string
		wantOther bool
	}{
		{
			name: "same commit in two release directories",
			a:    section(stagingDir, `"RAILS_ENV": "production", "N": 1`),
			b:    section(prodDir, `"N": 1, "RAILS_ENV": "production"`),
		},
		{
			name:     "interpolated arg differs",
			a:        section(stagingDir, `"ASSET_HOST": "https://staging.example.com", "RAILS_ENV": "production"`),
			b:        section(prodDir, `"ASSET_HOST": "https://cdn.example.com", "RAILS_ENV": "production"`),
			wantArgs: []string{"ASSET_HOST"},
		},
		{
			name:     "arg only on one side",
			a:        section(stagingDir, `"B": "1"`),
			b:        section(prodDir, `"A": null, "B": "1"`),
			wantArgs: []string{"A"},
		},
		{
			name:      "different target",
			a:         section(stagingDir, ``),
			b:         strings.Replace(section(prodDir, ``), `"prod"`, `"debug"`, 1),
			wantOther: true,
		},
		{
			name:      "context outside the release directory",
			a:         section(stagingDir, ``),
			b:         section("/srv/elsewhere", ``),
			wantOther: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := CompareBuilds(buildOf(t, tt.a), stagingDir, buildOf(t, tt.b), prodDir)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(diff.DifferingArgs, tt.wantArgs) || diff.OtherDiffers != tt.wantOther {
				t.Errorf("diff = %+v, want args %v other %v", diff, tt.wantArgs, tt.wantOther)
			}
			if diff.Same() != (len(tt.wantArgs) == 0 && !tt.wantOther) {
				t.Errorf("Same() = %v", diff.Same())
			}
		})
	}
}

func TestCompareBuildsNeedsBuildSections(t *testing.T) {
	if _, err := CompareBuilds(nil, "/a", buildOf(t, `{"context": "/b"}`), "/b"); err == nil {
		t.Error("CompareBuilds accepted a missing build section")
	}
	// A Build that was not decoded from Compose output carries no raw section.
	if _, err := CompareBuilds(&Build{Context: "/a"}, "/a", &Build{Context: "/b"}, "/b"); err == nil {
		t.Error("CompareBuilds accepted builds without raw sections")
	}
}
