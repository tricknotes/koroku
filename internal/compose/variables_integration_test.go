//go:build integration

package compose

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// TestVariablesMatchCompose checks the scanner against
// `docker compose config --variables`: the same names, the same ? form
// (Compose's "Required") and the same defaults. koroku's own Required
// judgement differs on purpose (ARCHITECTURE 9.4) and is not compared.
func TestVariablesMatchCompose(t *testing.T) {
	path, err := filepath.Abs(filepath.Join(fixtures, "variables.compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := runner.Output(ctx, runner.Exec{}, runner.Cmd{
		Name: "docker",
		Args: []string{"compose", "-p", "koroku-variables-it", "-f", path, "config", "--variables", "--format", "json"},
	})
	if err != nil {
		t.Fatalf("docker compose config --variables: %v", err)
	}
	var fromCompose map[string]struct {
		Name         string
		DefaultValue string
		Required     bool
	}
	if err := json.Unmarshal(out, &fromCompose); err != nil {
		t.Fatalf("decode compose output: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ours := Variables(data)
	if len(ours) != len(fromCompose) {
		t.Errorf("found %d variables, Compose found %d", len(ours), len(fromCompose))
	}
	for _, v := range ours {
		c, ok := fromCompose[v.Name]
		if !ok {
			t.Errorf("%s: not reported by Compose", v.Name)
			continue
		}
		if v.Mandatory != c.Required {
			t.Errorf("%s: mandatory = %t, Compose says required = %t", v.Name, v.Mandatory, c.Required)
		}
		def := ""
		if v.Default != nil {
			def = *v.Default
		}
		if def != c.DefaultValue {
			t.Errorf("%s: default = %q, Compose says %q", v.Name, def, c.DefaultValue)
		}
	}
}
