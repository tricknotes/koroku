package compose

import (
	"regexp"
	"sort"
	"strings"
)

// Variable is one variable a Compose file interpolates (ARCHITECTURE 9.4).
type Variable struct {
	Name string
	// Required is koroku's judgement: the variable must be set for the
	// file to mean what it says. True for ${VAR:?msg} and ${VAR?msg}, and
	// for ${VAR} and $VAR without a default (Compose would substitute an
	// empty string). False when every reference has a default or an
	// alternate value, or the variable only appears nested inside the
	// default of another one.
	Required bool
	// Mandatory is set when a reference uses the ? form. It is what
	// `docker compose config --variables` reports as Required.
	Mandatory bool
	// Default is the text after :- or - of the first reference that has
	// one, nil when no reference has a default.
	Default *string
	// Message is the text after :? or ? of the first reference that has a
	// non-empty one.
	Message string
}

// varOccurrence is one reference found while scanning.
type varOccurrence struct {
	name   string
	op     string // "", ":-", "-", ":?", "?", ":+", "+"
	arg    string
	nested bool
}

var (
	nameStart = func(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
	nameChar  = func(c byte) bool { return nameStart(c) || (c >= '0' && c <= '9') }
	// plainKey matches a block mapping key written without quotes, with an
	// optional list marker in front: "  key: value", "- key:".
	plainKey = regexp.MustCompile(`^(\s*(?:-\s+)?)([^\s'"#][^:#]*?):(?:\s|$)`)
	// blockScalar matches a line whose value starts a literal or folded
	// block scalar ("command: |", "- >-", "key: |2").
	blockScalar = regexp.MustCompile(`(?:^|[\s:])[|>][-+]?[0-9]?[-+]?$`)
)

// Variables lists the variables a Compose file interpolates, sorted by
// name. It scans the text instead of parsing YAML (no YAML dependency, see
// ScanServices): comments are skipped, mapping keys are not interpolated
// by Compose and are skipped too, and block scalars are read as content.
// Files pulled in with include: or extends: are not read.
func Variables(content []byte) []Variable {
	var occurrences []varOccurrence
	for _, text := range interpolatedText(content) {
		occurrences = append(occurrences, scanInterpolation(text, false)...)
	}
	byName := map[string]*Variable{}
	var names []string
	for _, o := range occurrences {
		v, ok := byName[o.name]
		if !ok {
			v = &Variable{Name: o.name}
			byName[o.name] = v
			names = append(names, o.name)
		}
		switch o.op {
		case ":-", "-":
			if v.Default == nil {
				arg := o.arg
				v.Default = &arg
			}
		case ":?", "?":
			v.Mandatory = true
			if v.Message == "" {
				v.Message = strings.TrimSpace(o.arg)
			}
		}
		if o.nested {
			continue
		}
		if o.op == "" || o.op == ":?" || o.op == "?" {
			v.Required = true
		}
	}
	sort.Strings(names)
	out := make([]Variable, 0, len(names))
	for _, name := range names {
		out = append(out, *byName[name])
	}
	return out
}

// interpolatedText returns the parts of the file Compose interpolates:
// values, with comments and mapping keys removed. Block scalar lines are
// returned whole because '#' is content there.
func interpolatedText(content []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	var out []string
	blockIndent := -1 // indentation of the line that opened a block scalar
	for _, line := range lines {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if blockIndent >= 0 {
			if strings.TrimSpace(line) == "" || indent > blockIndent {
				out = append(out, line)
				continue
			}
			blockIndent = -1
		}
		text := stripYAMLComment(line)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if m := plainKey.FindStringSubmatchIndex(text); m != nil {
			// Keep the list marker's position but drop the key itself.
			text = text[m[3]:]
			if i := strings.IndexByte(text, ':'); i >= 0 {
				text = text[i+1:]
			}
		}
		if blockScalar.MatchString(strings.TrimRight(text, " \t")) {
			blockIndent = indent
		}
		out = append(out, text)
	}
	return out
}

// stripYAMLComment removes a YAML comment: '#' at the start of the line or
// after whitespace, outside quotes. Quotes only open at the start of a
// scalar, so the apostrophe in "it's" does not.
func stripYAMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
		case quote == '"':
			if c == '\\' {
				i++
			} else if c == '"' {
				quote = 0
			}
		case c == '#':
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		case c == '\'' || c == '"':
			if i == 0 || strings.IndexByte(" \t:-[{,", line[i-1]) >= 0 {
				quote = c
			}
		}
	}
	return line
}

// scanInterpolation finds the references in one value, following the
// syntax of Compose's interpolation: $$ escapes, $VAR, ${VAR} and the
// operators :- - :? ? :+ + whose argument may nest further references.
// A malformed reference is skipped.
func scanInterpolation(s string, nested bool) []varOccurrence {
	var out []varOccurrence
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			i++
		case nameStart(next):
			j := i + 1
			for j < len(s) && nameChar(s[j]) {
				j++
			}
			out = append(out, varOccurrence{name: s[i+1 : j], nested: nested})
			i = j - 1
		case next == '{':
			end := closingBrace(s, i+2)
			if end < 0 {
				continue
			}
			body := s[i+2 : end]
			if o, ok := parseBraced(body, nested); ok {
				out = append(out, o)
				if o.arg != "" {
					out = append(out, scanInterpolation(o.arg, true)...)
				}
			}
			i = end
		}
	}
	return out
}

// closingBrace returns the index of the '}' that closes a reference whose
// body starts at from, skipping nested ${...}; -1 when there is none.
func closingBrace(s string, from int) int {
	depth := 0
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '$':
			i++
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			depth++
			i++
		case s[i] == '}':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// parseBraced reads "NAME", "NAME:-arg", "NAME?arg" and so on.
func parseBraced(body string, nested bool) (varOccurrence, bool) {
	if body == "" || !nameStart(body[0]) {
		return varOccurrence{}, false
	}
	j := 1
	for j < len(body) && nameChar(body[j]) {
		j++
	}
	o := varOccurrence{name: body[:j], nested: nested}
	rest := body[j:]
	if rest == "" {
		return o, true
	}
	for _, op := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, op) {
			o.op = op
			o.arg = rest[len(op):]
			return o, true
		}
	}
	return varOccurrence{}, false
}
