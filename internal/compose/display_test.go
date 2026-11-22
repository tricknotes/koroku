package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplayConfigGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "display.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := DisplayConfig(data, nil)
	if err != nil {
		t.Fatalf("DisplayConfig: %v", err)
	}
	checkGolden(t, "display.config.yaml", []byte(got))
}

func TestDisplayConfigMasksValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "display.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := DisplayConfig(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{
		"literal-list-password",      // environment as a list
		"postgres://app",             // environment as a mapping
		"RAILS_LOG_TO_STDOUT: \"1\"", // even harmless literals
		"${BUNDLE_TOKEN}",            // build args
		"3.4",                        // build args, literal
		"literal-secret-content",     // top-level secret content
	} {
		if strings.Contains(got, hidden) {
			t.Errorf("output contains %q:\n%s", hidden, got)
		}
	}
	for _, shown := range []string{
		"POSTGRES_PASSWORD=[MASKED]",
		"- PGDATA",
		"SECRET_KEY_BASE: null",
		"BUNDLE_TOKEN: \"[MASKED]\"",
		"environment: DB_PASSWORD", // a variable name, not a value
		"server_name ${HOST};",     // configs are not secrets
		"mem_limit: \"${WEB_MEMORY:-512m}\"",
		"path: /var/lib/koroku/apps/blog/env",
	} {
		if !strings.Contains(got, shown) {
			t.Errorf("output lacks %q:\n%s", shown, got)
		}
	}
}

func TestDisplayConfigAppliesTheMask(t *testing.T) {
	got, err := DisplayConfig([]byte(`{"services": {"web": {"labels": {"note": "x-s3cret-x"}}}}`),
		func(s string) string { return strings.ReplaceAll(s, "s3cret", "[MASKED]") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `note: "x-[MASKED]-x"`) {
		t.Errorf("got:\n%s", got)
	}
}

func TestDisplayConfigRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "[]", "{", `{"a":1} {"b":2}`, `"secret"`} {
		_, err := DisplayConfig([]byte(in), nil)
		if err == nil {
			t.Errorf("DisplayConfig(%q) succeeded", in)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error repeats the input: %v", err)
		}
	}
}

func TestYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		"postgres:16":       "postgres:16",
		"/var/lib/koroku":   "/var/lib/koroku",
		"unless-stopped":    "unless-stopped",
		"127.0.0.1":         `"127.0.0.1"`,
		"20001":             `"20001"`,
		"3.4":               `"3.4"`,
		"true":              `"true"`,
		"no":                `"no"`,
		"":                  `""`,
		"-x":                `"-x"`,
		"a b":               `"a b"`,
		"${X}":              `"${X}"`,
		"line\nbreak":       `"line\nbreak"`,
		"x # not a comment": `"x # not a comment"`,
	} {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %s, want %s", in, got, want)
		}
	}
}
