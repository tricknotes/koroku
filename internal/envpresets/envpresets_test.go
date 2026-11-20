package envpresets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tricknotes/koroku/internal/github"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const rails70Production = `Rails.application.configure do
  config.public_file_server.enabled = ENV["RAILS_SERVE_STATIC_FILES"].present?
  if ENV["RAILS_LOG_TO_STDOUT"].present?
    logger           = ActiveSupport::Logger.new(STDOUT)
    config.logger    = ActiveSupport::TaggedLogging.new(logger)
  end
end
`

const rails71Production = `Rails.application.configure do
  config.logger = ActiveSupport::Logger.new(STDOUT)
end
`

// repos are the repositories of the golden tests: file path to content.
var repos = map[string]map[string]string{
	"rails-7.0": {
		"Gemfile":                           "source \"https://rubygems.org\"\n\ngem \"rails\", \"~> 7.0.8\"\ngem \"pg\"\n",
		"config/environments/production.rb": rails70Production,
		"config/application.rb":             "require_relative \"boot\"\n",
		// Assets only: no NODE_ENV next to Rails.
		"package.json": `{"name": "app", "scripts": {"build": "esbuild app/javascript/*.*"}}`,
		"compose.yaml": `services:
  web:
    build: .
    environment:
      RACK_ENV: production
      RAILS_ENV: ${RAILS_ENV:-development}
  db:
    image: postgres:17
`,
	},
	"rails-7.1": {
		"Gemfile":                           "gem 'rails', '~> 7.1'\n",
		"config/environments/production.rb": rails71Production,
	},
	"railties": {
		"Gemfile": "gem(\"railties\")\ngem \"sinatra\"\n",
	},
	"sinatra": {
		"Gemfile":   "source 'https://rubygems.org'\ngem 'sinatra'\ngem 'puma'\n",
		"config.ru": "require './app'\nrun Sinatra::Application\n",
	},
	"config-ru": {
		"config.ru": "run ->(env) { [200, {}, [\"ok\"]] }\n",
	},
	"laravel": {
		"artisan":       "#!/usr/bin/env php\n<?php\n",
		"composer.json": `{"require": {"php": "^8.2", "laravel/framework": "^11.0"}}`,
		"package.json":  `{"private": true, "scripts": {"build": "vite build"}}`,
		"docker-compose.yml": `services:
  app:
    build: .
    environment:
      - APP_DEBUG=false
`,
	},
	"laravel-package": {
		// A library depending on Laravel, without an application.
		"composer.json": `{"require": {"laravel/framework": "^11.0"}}`,
	},
	"django": {
		"manage.py": `#!/usr/bin/env python
import os, sys

def main():
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")
    from django.core.management import execute_from_command_line
    execute_from_command_line(sys.argv)
`,
		"mysite/settings.py": `import os
SECRET_KEY = os.environ.get("DJANGO_SECRET_KEY", "insecure")
DEBUG = os.environ.get("DEBUG", "False") == "True"
`,
	},
	"django-package-settings": {
		"manage.py":                        "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'config.settings')\nimport django\n",
		"config/settings/__init__.py":      "import environ\nenv = environ.Env()\nSECRET_KEY = env.str('SECRET_KEY')\n",
		"requirements.txt":                 "Django==5.1\n",
		"config/settings/development.py":   "",
		"config/settings/__unrelated__.py": "",
	},
	"django-hardcoded": {
		"manage.py":           "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'blog.settings')\nfrom django.core.management import execute_from_command_line\n",
		"blog/settings.py":    "SECRET_KEY = 'django-insecure-abc'\nDEBUG = True\n",
		"package.json":        "{}",
		"blog/__init__.py":    "",
		"blog/other_file.txt": "SECRET_KEY",
	},
	"phoenix": {
		"mix.exs": `defmodule App.MixProject do
  defp deps do
    [
      {:phoenix, "~> 1.7.14"},
      {:ecto_sql, "~> 3.10"}
    ]
  end
end
`,
		"package.json": "{}",
	},
	"elixir": {
		"mix.exs": "defmodule Worker.MixProject do\n  defp deps, do: [{:jason, \"~> 1.4\"}]\nend\n",
	},
	"node": {
		"package.json": `{"name": "api", "scripts": {"start": "node server.js"}}`,
		"compose.yaml": "services:\n  web:\n    build: .\n    environment:\n      NODE_ENV: ${NODE_ENV}\n",
	},
	"node-compose-literal": {
		"package.json": `{"name": "api"}`,
		"compose.yml":  "services:\n  web:\n    build: .\n    environment:\n      - NODE_ENV=development\n  worker:\n    environment:\n      - NODE_ENV=production\n",
	},
	"node-chosen-compose": {
		"package.json":        `{"name": "api"}`,
		"compose.yaml":        "services:\n  web:\n    environment:\n      NODE_ENV: development\n",
		"deploy/compose.yaml": "services:\n  web:\n    build: ..\n",
	},
	"nothing": {
		"README.md": "# docs\n",
		"Gemfile":   "gem 'jekyll'\n",
	},
}

// composePaths are the Compose files chosen on the create screen.
var composePaths = map[string]string{
	"node-chosen-compose": "deploy/compose.yaml",
}

// detect runs Detect on an in-memory repository.
func detect(t *testing.T, files map[string]string, composePath string) (Detection, []string) {
	t.Helper()
	fsys := fstest.MapFS{}
	var root []string
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
		if !strings.Contains(name, "/") {
			root = append(root, name)
		}
	}
	var reads []string
	read := func(name string) ([]byte, error) {
		reads = append(reads, name)
		return fs.ReadFile(fsys, name)
	}
	d, err := Detect(root, read, composePath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return d, reads
}

func TestDetectGolden(t *testing.T) {
	for name, files := range repos {
		t.Run(name, func(t *testing.T) {
			d, _ := detect(t, files, composePaths[name])
			got, err := json.MarshalIndent(d, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", name+".json")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("detection differs from %s:\n%s", golden, got)
			}
		})
	}
}

func TestDetectReadsOnlyWhatItNeeds(t *testing.T) {
	_, reads := detect(t, repos["node"], "")
	// package.json is a signal by its presence; only the Compose file is read.
	if strings.Join(reads, ",") != "compose.yaml" {
		t.Errorf("reads = %v, want only compose.yaml", reads)
	}
	_, reads = detect(t, repos["sinatra"], "")
	if strings.Join(reads, ",") != "Gemfile" {
		t.Errorf("reads = %v, want the Gemfile once", reads)
	}
}

func TestDetectStopsOnReadErrors(t *testing.T) {
	boom := errors.New("rate limited")
	read := func(string) ([]byte, error) { return nil, boom }
	if _, err := Detect([]string{"Gemfile"}, read, ""); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the read error", err)
	}
}

func TestGenerate(t *testing.T) {
	formats := map[string]*regexp.Regexp{
		GenRailsSecretKeyBase:   regexp.MustCompile(`^[0-9a-f]{128}$`),
		GenPhoenixSecretKeyBase: regexp.MustCompile(`^[A-Za-z0-9+/]{64}$`),
		GenLaravelAppKey:        regexp.MustCompile(`^base64:[A-Za-z0-9+/]{43}=$`),
		GenDjangoSecretKey:      regexp.MustCompile(`^[A-Za-z0-9]{50}$`),
	}
	for generator, format := range formats {
		a, err := Generate(generator, rand.Reader)
		if err != nil {
			t.Fatalf("%s: %v", generator, err)
		}
		b, _ := Generate(generator, rand.Reader)
		if !format.MatchString(a) {
			t.Errorf("%s: %q does not match %s", generator, a, format)
		}
		if a == b {
			t.Errorf("%s: two values are the same", generator)
		}
	}
	if _, err := Generate("nope", rand.Reader); err == nil {
		t.Error("an unknown generator should fail")
	}
}

func TestGenerateRejectsBiasedBytes(t *testing.T) {
	// 248 and above would favour the first characters of the alphabet.
	src := bytes.NewReader(append(bytes.Repeat([]byte{255}, 50), bytes.Repeat([]byte{1}, 50)...))
	got, err := Generate(GenDjangoSecretKey, src)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("b", 50) {
		t.Errorf("got %q", got)
	}
	if _, err := Generate(GenRailsSecretKeyBase, bytes.NewReader(nil)); err == nil {
		t.Error("a failing random source should fail")
	}
}

// fakeFiles serves a repository like the GitHub client.
type fakeFiles struct {
	files   map[string]string
	listErr error
	calls   []string
}

func (f *fakeFiles) ListDir(_ context.Context, owner, repo, dir, ref string) ([]string, error) {
	f.calls = append(f.calls, fmt.Sprintf("list %s/%s %q @%s", owner, repo, dir, ref))
	if f.listErr != nil {
		return nil, f.listErr
	}
	var names []string
	for name := range f.files {
		if !strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (f *fakeFiles) ReadFile(_ context.Context, owner, repo, path, ref string) ([]byte, error) {
	f.calls = append(f.calls, fmt.Sprintf("read %s/%s %s @%s", owner, repo, path, ref))
	content, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("github: %w", github.ErrNotFound)
	}
	if content == "TOO LARGE" {
		return nil, fmt.Errorf("github: %w", github.ErrTooLarge)
	}
	return []byte(content), nil
}

func TestReader(t *testing.T) {
	ctx := context.Background()
	req := Request{Owner: "octo", Repo: "blog", Ref: "next"}

	if got := NewReader(nil, nil).Read(ctx, req); got.Reason != ReasonNotConfigured {
		t.Errorf("without clients: %+v", got)
	}

	app := &fakeFiles{files: map[string]string{"Gemfile": "gem 'rails'\n", "compose.yaml": "TOO LARGE"}}
	public := &fakeFiles{}
	got := NewReader(app, public).Read(ctx, req)
	if got.Status != StatusOK || len(got.Detection.Presets) != 3 || got.Detection.ComposePath != "" {
		t.Errorf("through the App: %+v", got)
	}
	if len(public.calls) != 0 {
		t.Errorf("the anonymous client was used: %v", public.calls)
	}
	if app.calls[0] != `list octo/blog "" @next` {
		t.Errorf("calls = %v", app.calls)
	}

	// The App cannot see the repository: read it anonymously.
	hidden := &fakeFiles{listErr: github.ErrNotFound}
	public = &fakeFiles{files: map[string]string{"package.json": "{}"}}
	got = NewReader(hidden, public).Read(ctx, req)
	if got.Status != StatusOK || len(got.Detection.Presets) != 1 {
		t.Errorf("anonymously: %+v", got)
	}

	for err, reason := range map[error]string{
		github.ErrNotFound:         ReasonRepoUnavailable,
		github.ErrRateLimited:      ReasonRateLimited,
		errors.New("network down"): ReasonGitHubError,
	} {
		failing := &fakeFiles{listErr: err}
		got := NewReader(failing, nil).Read(ctx, req)
		if got.Status != StatusUnavailable || got.Reason != reason {
			t.Errorf("%v: got %+v, want %s", err, got, reason)
		}
	}
}
