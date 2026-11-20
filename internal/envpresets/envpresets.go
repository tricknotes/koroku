// Package envpresets infers, from the files of a repository, environment
// variables a production deployment of its framework usually needs, so the
// create-app screen can prefill them (ARCHITECTURE 9.8). Detection is pure:
// it only looks at the files it is given. Secret values are generated
// separately with Generate.
package envpresets

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/tricknotes/koroku/internal/compose"
)

// Frameworks a Detection can name.
const (
	FrameworkRails   = "rails"
	FrameworkRack    = "rack"
	FrameworkLaravel = "laravel"
	FrameworkDjango  = "django"
	FrameworkPhoenix = "phoenix"
	FrameworkElixir  = "elixir"
	FrameworkNode    = "node"
)

// Generators of secret values (see Generate).
const (
	GenRailsSecretKeyBase   = "rails_secret_key_base"
	GenPhoenixSecretKeyBase = "phoenix_secret_key_base"
	GenLaravelAppKey        = "laravel_app_key"
	GenDjangoSecretKey      = "django_secret_key"
)

// Preset is one variable to prefill.
type Preset struct {
	Key string `json:"key"`
	// Value is the fixed value; empty when Generator is set.
	Value string `json:"value,omitempty"`
	// Generator names how to generate a secret value, empty for a fixed one.
	Generator string `json:"generator,omitempty"`
	Framework string `json:"framework"`
	// File and Match say why: the file that was read and what in it
	// matched. An empty Match means the file's presence is the reason.
	File  string `json:"file"`
	Match string `json:"match,omitempty"`
}

// Skipped is a preset left out because the Compose file sets the variable
// to a literal value, which the injected env file could not override.
type Skipped struct {
	Key     string `json:"key"`
	Service string `json:"service"`
}

// Detection is what a repository suggests.
type Detection struct {
	// ComposePath is the Compose file that was read, empty when none.
	ComposePath string    `json:"compose_path"`
	Frameworks  []string  `json:"frameworks"`
	Presets     []Preset  `json:"presets"`
	Skipped     []Skipped `json:"skipped"`
}

// ReadFunc returns a file of the repository by its slash-separated path. A
// missing (or unreadable, such as too large) file yields an error matching
// fs.ErrNotExist; any other error stops the detection.
type ReadFunc func(name string) ([]byte, error)

var (
	gemLine        = regexp.MustCompile(`(?m)^[ \t]*gem[ \t(]+["']([A-Za-z0-9_.-]+)["']`)
	phoenixDep     = regexp.MustCompile(`\{\s*:phoenix\s*,`)
	settingsModule = regexp.MustCompile(`DJANGO_SETTINGS_MODULE["']\s*,\s*["']([A-Za-z_][A-Za-z0-9_.]*)["']`)
	djangoSecret   = regexp.MustCompile(`(?:environ\s*\.\s*get\s*\(|environ\s*\[|getenv\s*\(|\benv(?:\.[a-z_]+)?\s*\()\s*["'](DJANGO_SECRET_KEY|SECRET_KEY)["']`)
)

// Detect applies the rules of ARCHITECTURE 9.8 to a repository whose root
// holds the regular files in root. composePath names the Compose file whose
// literal environment entries win over presets; empty means the first of
// compose.DefaultFiles in the root.
func Detect(root []string, read ReadFunc, composePath string) (Detection, error) {
	d := detector{root: root, read: read, seen: map[string]bool{}}
	for _, rule := range []func() error{d.rails, d.rack, d.laravel, d.django, d.elixir, d.node} {
		if err := rule(); err != nil {
			return Detection{}, err
		}
	}
	if err := d.applyCompose(composePath); err != nil {
		return Detection{}, err
	}
	if d.out.Frameworks == nil {
		d.out.Frameworks = []string{}
	}
	if d.out.Presets == nil {
		d.out.Presets = []Preset{}
	}
	if d.out.Skipped == nil {
		d.out.Skipped = []Skipped{}
	}
	return d.out, nil
}

type detector struct {
	root []string
	read ReadFunc
	out  Detection
	seen map[string]bool

	gemsRead bool
	gemList  []string
}

func (d *detector) has(name string) bool { return slices.Contains(d.root, name) }

// file reads a file, reporting false when it is missing.
func (d *detector) file(name string) ([]byte, bool, error) {
	content, err := d.read(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// rootFile reads a file of the root, without a request when it is absent.
func (d *detector) rootFile(name string) ([]byte, bool, error) {
	if !d.has(name) {
		return nil, false, nil
	}
	return d.file(name)
}

func (d *detector) framework(name string) { d.out.Frameworks = append(d.out.Frameworks, name) }

func (d *detector) detected(names ...string) bool {
	for _, n := range names {
		if slices.Contains(d.out.Frameworks, n) {
			return true
		}
	}
	return false
}

// add keeps the first preset of each key (the rules run in table order).
func (d *detector) add(p Preset) {
	if d.seen[p.Key] {
		return
	}
	d.seen[p.Key] = true
	d.out.Presets = append(d.out.Presets, p)
}

// gems lists the gems of the Gemfile, read once for both Ruby rules.
func (d *detector) gems() ([]string, error) {
	if d.gemsRead {
		return d.gemList, nil
	}
	content, ok, err := d.rootFile("Gemfile")
	if err != nil {
		return nil, err
	}
	d.gemsRead = true
	if ok {
		for _, m := range gemLine.FindAllSubmatch(content, -1) {
			d.gemList = append(d.gemList, string(m[1]))
		}
	}
	return d.gemList, nil
}

func (d *detector) rails() error {
	gems, err := d.gems()
	if err != nil {
		return err
	}
	gem := ""
	for _, name := range []string{"rails", "railties"} {
		if slices.Contains(gems, name) {
			gem = name
			break
		}
	}
	if gem == "" {
		return nil
	}
	d.framework(FrameworkRails)
	because := func(p Preset) Preset {
		p.Framework, p.File, p.Match = FrameworkRails, "Gemfile", gem
		return p
	}
	d.add(because(Preset{Key: "RAILS_ENV", Value: "production"}))
	d.add(because(Preset{Key: "RACK_ENV", Value: "production"}))
	d.add(because(Preset{Key: "SECRET_KEY_BASE", Generator: GenRailsSecretKeyBase}))

	// Only templates up to Rails 7.0 read these; later ones log to stdout
	// and serve static files by default.
	const production = "config/environments/production.rb"
	content, ok, err := d.file(production)
	if err != nil || !ok {
		return err
	}
	for _, key := range []string{"RAILS_LOG_TO_STDOUT", "RAILS_SERVE_STATIC_FILES"} {
		if strings.Contains(string(content), key) {
			d.add(Preset{Key: key, Value: "true", Framework: FrameworkRails, File: production, Match: key})
		}
	}
	return nil
}

func (d *detector) rack() error {
	if d.detected(FrameworkRails) {
		return nil
	}
	gems, err := d.gems()
	if err != nil {
		return err
	}
	file, match := "", ""
	for _, name := range []string{"sinatra", "roda", "rack"} {
		if slices.Contains(gems, name) {
			file, match = "Gemfile", name
			break
		}
	}
	if file == "" && d.has("config.ru") {
		file = "config.ru"
	}
	if file == "" {
		return nil
	}
	d.framework(FrameworkRack)
	d.add(Preset{Key: "RACK_ENV", Value: "production", Framework: FrameworkRack, File: file, Match: match})
	return nil
}

func (d *detector) laravel() error {
	if !d.has("artisan") {
		return nil
	}
	content, ok, err := d.rootFile("composer.json")
	if err != nil || !ok {
		return err
	}
	var manifest struct {
		Require map[string]json.RawMessage `json:"require"`
	}
	if json.Unmarshal(content, &manifest) != nil {
		return nil
	}
	const pkg = "laravel/framework"
	if _, ok := manifest.Require[pkg]; !ok {
		return nil
	}
	d.framework(FrameworkLaravel)
	because := func(p Preset) Preset {
		p.Framework, p.File, p.Match = FrameworkLaravel, "composer.json", pkg
		return p
	}
	d.add(because(Preset{Key: "APP_ENV", Value: "production"}))
	d.add(because(Preset{Key: "APP_DEBUG", Value: "false"}))
	d.add(because(Preset{Key: "APP_KEY", Generator: GenLaravelAppKey}))
	return nil
}

func (d *detector) django() error {
	content, ok, err := d.rootFile("manage.py")
	if err != nil || !ok || !strings.Contains(string(content), "django") {
		return err
	}
	d.framework(FrameworkDjango)
	// Django reads no variable itself: only a settings file that takes its
	// secret key from the environment gets one. DEBUG is never preset, as
	// settings files parse it in ways where "False" can turn it on.
	m := settingsModule.FindSubmatch(content)
	if m == nil {
		return nil
	}
	module := strings.ReplaceAll(string(m[1]), ".", "/")
	for _, name := range []string{module + ".py", module + "/__init__.py"} {
		settings, ok, err := d.file(name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, m := range djangoSecret.FindAllSubmatch(settings, -1) {
			key := string(m[1])
			d.add(Preset{Key: key, Generator: GenDjangoSecretKey, Framework: FrameworkDjango, File: name, Match: key})
		}
		return nil
	}
	return nil
}

func (d *detector) elixir() error {
	content, ok, err := d.rootFile("mix.exs")
	if err != nil || !ok {
		return err
	}
	if !phoenixDep.Match(content) {
		d.framework(FrameworkElixir)
		d.add(Preset{Key: "MIX_ENV", Value: "prod", Framework: FrameworkElixir, File: "mix.exs"})
		return nil
	}
	d.framework(FrameworkPhoenix)
	d.add(Preset{Key: "MIX_ENV", Value: "prod", Framework: FrameworkPhoenix, File: "mix.exs", Match: "phoenix"})
	d.add(Preset{Key: "SECRET_KEY_BASE", Generator: GenPhoenixSecretKeyBase, Framework: FrameworkPhoenix, File: "mix.exs", Match: "phoenix"})
	return nil
}

func (d *detector) node() error {
	// Next to another framework package.json only builds assets, and
	// NODE_ENV at run time changes nothing.
	if !d.has("package.json") || len(d.out.Frameworks) > 0 {
		return nil
	}
	d.framework(FrameworkNode)
	d.add(Preset{Key: "NODE_ENV", Value: "production", Framework: FrameworkNode, File: "package.json"})
	return nil
}

// applyCompose moves presets the Compose file fixes itself to Skipped.
func (d *detector) applyCompose(composePath string) error {
	if composePath == "" {
		for _, name := range compose.DefaultFiles() {
			if d.has(name) {
				composePath = name
				break
			}
		}
	}
	if composePath == "" {
		return nil
	}
	composePath = path.Clean(composePath)
	content, ok, err := d.file(composePath)
	if err != nil || !ok {
		return err
	}
	d.out.ComposePath = composePath
	if len(d.out.Presets) == 0 {
		return nil
	}
	fixed := map[string]string{}
	for _, hint := range compose.ScanEnvironment(content) {
		if _, seen := fixed[hint.Key]; hint.Literal && !seen {
			fixed[hint.Key] = hint.Service
		}
	}
	kept := d.out.Presets[:0]
	for _, p := range d.out.Presets {
		if service, ok := fixed[p.Key]; ok {
			d.out.Skipped = append(d.out.Skipped, Skipped{Key: p.Key, Service: service})
			continue
		}
		kept = append(kept, p)
	}
	d.out.Presets = kept
	return nil
}
