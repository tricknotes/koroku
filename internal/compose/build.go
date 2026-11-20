package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// BuildDiff is the result of comparing the build sections of one service
// in two releases (ARCHITECTURE 9.5). It holds key names only, never values.
type BuildDiff struct {
	// DifferingArgs are the build.args keys whose interpolated values
	// differ, or that only one side has, in sorted order.
	DifferingArgs []string
	// OtherDiffers is true when anything besides the args differs
	// (context, Dockerfile, target, additional contexts, ...).
	OtherDiffers bool
}

// Same reports whether both definitions build the same image from the same
// source.
func (d BuildDiff) Same() bool { return len(d.DifferingArgs) == 0 && !d.OtherDiffers }

// CompareBuilds compares two normalized build sections. Each was produced
// by `docker compose config` for a checkout of the same commit in its own
// release directory (aDir, bDir); paths inside those directories are
// compared relative to them, so two checkouts of one commit compare equal.
// Arg values are compared by their SHA-256 so that the values of the apps'
// variables do not need to be kept or shown.
func CompareBuilds(a *Build, aDir string, b *Build, bDir string) (BuildDiff, error) {
	if a == nil || b == nil {
		return BuildDiff{}, errors.New("compare builds: a service has no build section")
	}
	aArgs, aRest, err := normalizeBuild(a.raw, aDir)
	if err != nil {
		return BuildDiff{}, err
	}
	bArgs, bRest, err := normalizeBuild(b.raw, bDir)
	if err != nil {
		return BuildDiff{}, err
	}
	var diff BuildDiff
	keys := map[string]bool{}
	for k := range aArgs {
		keys[k] = true
	}
	for k := range bArgs {
		keys[k] = true
	}
	for k := range keys {
		av, aok := aArgs[k]
		bv, bok := bArgs[k]
		if aok != bok || av != bv {
			diff.DifferingArgs = append(diff.DifferingArgs, k)
		}
	}
	sort.Strings(diff.DifferingArgs)
	diff.OtherDiffers = !bytes.Equal(aRest, bRest)
	return diff, nil
}

// normalizeBuild splits a raw build section into hashed args and the
// canonical JSON of everything else, with paths under dir made relative.
func normalizeBuild(raw json.RawMessage, dir string) (map[string][sha256.Size]byte, []byte, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("compare builds: the build section was not read from Compose output")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var section map[string]any
	if err := dec.Decode(&section); err != nil {
		// The section may hold values of the app's variables: no excerpt.
		return nil, nil, errors.New("compare builds: build section is not a JSON object")
	}
	args := map[string][sha256.Size]byte{}
	if rawArgs, ok := section["args"].(map[string]any); ok {
		for k, v := range rawArgs {
			data, err := json.Marshal(v)
			if err != nil {
				return nil, nil, errors.New("compare builds: unreadable build arg")
			}
			args[k] = sha256.Sum256(data)
		}
	}
	delete(section, "args")
	rest, err := json.Marshal(relativize(section, filepath.Clean(dir)))
	if err != nil {
		return nil, nil, errors.New("compare builds: unreadable build section")
	}
	return args, rest, nil
}

// relativize replaces every string that is dir or a path below it with a
// path relative to dir. encoding/json sorts map keys, so the result
// marshals canonically.
func relativize(v any, dir string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			t[k] = relativize(inner, dir)
		}
		return t
	case []any:
		for i, inner := range t {
			t[i] = relativize(inner, dir)
		}
		return t
	case string:
		if t == dir {
			return "."
		}
		if strings.HasPrefix(t, dir+string(filepath.Separator)) {
			return "./" + filepath.ToSlash(strings.TrimPrefix(t, dir+string(filepath.Separator)))
		}
		return t
	default:
		return v
	}
}
