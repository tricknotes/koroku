package compose

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Compose reads more than the file it is given: `include` loads other
// Compose files, each with an env file of its own for interpolation, and
// `extends.file` copies a service from another file. All of that happens
// while the project is loaded, so nothing of it is left in the normalized
// project that CheckPolicy inspects: an include whose env_file is another
// app's env file puts that app's secrets into this app's containers, and an
// include from a Git or OCI address is fetched again by every later Compose
// command, so the content that is checked need not be the content that
// runs. ScanFiles therefore reads the files themselves, before Compose
// does.

// maxScannedFiles and maxScannedBytes bound what ScanFiles reads; Compose
// files are small and few.
const (
	maxScannedFiles = 100
	maxScannedBytes = 4 << 20
)

// remoteLocation matches what Compose loads from elsewhere: a URL (https,
// ssh, git, oci, ...) or the scp form of a Git address.
var remoteLocation = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://|[^/]+@[^/]+:)`)

// ScanFiles checks the Compose file of a release, and every file it pulls
// in with include or extends.file, against what koroku allows of those two:
// plain paths to files inside the checkout, without an env file or project
// directory of their own. composeFile is relative to releaseDir. Every
// finding is relaxable. A file that cannot be read as YAML is a finding
// too: what koroku cannot read, it cannot vouch for.
func ScanFiles(releaseDir, composeFile string) []Violation {
	s := fileScan{root: resolvePath(releaseDir), seen: map[string]bool{}}
	s.file(filepath.Join(s.root, composeFile))
	sort.SliceStable(s.found, func(i, j int) bool { return s.found[i].Message < s.found[j].Message })
	return s.found
}

type fileScan struct {
	root  string
	seen  map[string]bool
	found []Violation
}

func (s *fileScan) add(rule, format string, args ...any) {
	s.found = append(s.found, Violation{Rule: rule, Message: fmt.Sprintf(format, args...), Relaxable: true})
}

// name renders a path of the checkout for a message.
func (s *fileScan) name(path string) string {
	if rel, err := filepath.Rel(s.root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func (s *fileScan) file(path string) {
	if s.seen[path] {
		return
	}
	if len(s.seen) >= maxScannedFiles {
		s.add(RuleUnsupported, "the Compose files include more than %d files", maxScannedFiles)
		return
	}
	s.seen[path] = true

	f, err := os.Open(path)
	if err != nil {
		// A missing file is Compose's to report.
		return
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxScannedBytes+1))
	if err != nil || len(data) > maxScannedBytes {
		s.add(RuleUnsupported, "%s cannot be read as a Compose file (larger than %d MiB or unreadable)", s.name(path), maxScannedBytes>>20)
		return
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			// The error may quote the file, which is repository content
			// and may be anything; the name is enough.
			s.add(RuleUnsupported, "%s is not a YAML file koroku can read", s.name(path))
			return
		}
		s.document(path, doc)
	}
}

func (s *fileScan) document(path string, doc map[string]any) {
	dir := filepath.Dir(path)
	if include, ok := doc["include"]; ok && include != nil {
		entries, ok := include.([]any)
		if !ok {
			s.add(RuleUnsupported, "include in %s is not a list", s.name(path))
		}
		for _, entry := range entries {
			switch e := entry.(type) {
			case string:
				s.pulled("include", path, dir, e)
			case map[string]any:
				for _, key := range []string{"env_file", "project_directory"} {
					if v, ok := e[key]; ok && v != nil {
						s.add(RuleUnsupported, "include in %s sets %s, which is not allowed (an included file reads the app's variables like the file that includes it)", s.name(path), key)
					}
				}
				switch p := e["path"].(type) {
				case string:
					s.pulled("include", path, dir, p)
				case []any:
					for _, item := range p {
						if str, ok := item.(string); ok {
							s.pulled("include", path, dir, str)
						} else {
							s.add(RuleUnsupported, "include in %s has a path that is not a string", s.name(path))
						}
					}
				default:
					s.add(RuleUnsupported, "include in %s has an entry without a path", s.name(path))
				}
			default:
				s.add(RuleUnsupported, "include in %s has an entry koroku cannot read", s.name(path))
			}
		}
	}
	services, _ := doc["services"].(map[string]any)
	for _, service := range services {
		svc, _ := service.(map[string]any)
		extends, _ := svc["extends"].(map[string]any)
		if file, ok := extends["file"]; ok && file != nil {
			if str, ok := file.(string); ok {
				s.pulled("extends.file", path, dir, str)
			} else {
				s.add(RuleUnsupported, "extends.file in %s is not a string", s.name(path))
			}
		}
	}
}

// pulled checks one path that a file pulls in and scans the file it names.
func (s *fileScan) pulled(what, from, dir, target string) {
	switch {
	case target == "":
		return
	case strings.Contains(target, "$"):
		// Compose interpolates these paths, so the text says nothing about
		// the file that is read.
		s.add(RuleUnsupported, "%s in %s uses a variable (%s); write the path itself", what, s.name(from), target)
		return
	case remoteLocation.MatchString(target):
		s.add(RuleUnsupported, "%s in %s loads %s from outside the repository, which is not allowed", what, s.name(from), target)
		return
	}
	path := target
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	resolved := resolvePath(path)
	if !isInside(s.root, resolved) {
		s.add(RuleFileOutside, "%s in %s reads %s, which is outside the repository checkout", what, s.name(from), target)
		return
	}
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		s.add(RuleUnsupported, "%s in %s names the directory %s; name the Compose file", what, s.name(from), target)
		return
	}
	s.file(resolved)
}
