package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaskedValue replaces the values the configuration view hides.
const MaskedValue = "[MASKED]"

// DisplayConfig turns the output of `docker compose config --format json
// --no-interpolate --no-env-resolution` into YAML for the console's
// configuration view (ARCHITECTURE 9.10), with every value that can carry
// a secret replaced by MaskedValue: the values of each service's
// environment and build args, and the content of top-level secrets. Keys
// stay visible. The order of keys is the order Compose printed them in.
// mask, when set, is applied to every remaining string (keys included)
// before it is written, so that a masked value is still quoted correctly.
func DisplayConfig(data []byte, mask func(string) string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	root, err := readNode(dec)
	if err != nil {
		// The input may hold literal values from the repository, so the
		// error carries no excerpt.
		return "", errors.New("parse compose config: output is not the expected JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("parse compose config: unexpected data after the configuration")
	}
	if root.kind != kindObject {
		return "", errors.New("parse compose config: the configuration is not an object")
	}
	maskConfig(root)
	if mask != nil {
		maskStrings(root, mask)
	}
	var b strings.Builder
	writeObject(&b, root, 0)
	return b.String(), nil
}

type nodeKind int

const (
	kindNull nodeKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

// node is a JSON value that keeps the order of object keys.
type node struct {
	kind   nodeKind
	scalar string // bool, number or string
	keys   []string
	values []*node // object values (by keys) or array items
}

func (n *node) get(key string) *node {
	if n == nil || n.kind != kindObject {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.values[i]
		}
	}
	return nil
}

func readNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return &node{kind: kindNull}, nil
	case bool:
		return &node{kind: kindBool, scalar: strconv.FormatBool(t)}, nil
	case json.Number:
		return &node{kind: kindNumber, scalar: t.String()}, nil
	case string:
		return &node{kind: kindString, scalar: t}, nil
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: kindObject}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				value, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, key)
				n.values = append(n.values, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &node{kind: kindArray}
			for dec.More() {
				value, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.values = append(n.values, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// maskConfig hides the values that may be secrets. koroku cannot tell a
// secret from a harmless literal, so every value in these places goes,
// including the ones written in the repository.
func maskConfig(root *node) {
	if services := root.get("services"); services != nil && services.kind == kindObject {
		for _, svc := range services.values {
			maskMapping(svc.get("environment"))
			if build := svc.get("build"); build != nil {
				maskMapping(build.get("args"))
			}
		}
	}
	if secrets := root.get("secrets"); secrets != nil && secrets.kind == kindObject {
		for _, secret := range secrets.values {
			if content := secret.get("content"); content != nil && content.kind != kindNull {
				*content = node{kind: kindString, scalar: MaskedValue}
			}
		}
	}
}

func maskStrings(n *node, mask func(string) string) {
	if n.kind == kindString {
		n.scalar = mask(n.scalar)
	}
	for i := range n.keys {
		n.keys[i] = mask(n.keys[i])
	}
	for _, v := range n.values {
		maskStrings(v, mask)
	}
}

// maskMapping masks the values of an environment-like section: an object
// of KEY: value, or a list of "KEY=value". A key without a value (null, or
// "KEY" in a list) has nothing to hide and stays as it is.
func maskMapping(n *node) {
	if n == nil {
		return
	}
	switch n.kind {
	case kindObject:
		for i, v := range n.values {
			if v.kind != kindNull {
				n.values[i] = &node{kind: kindString, scalar: MaskedValue}
			}
		}
	case kindArray:
		for _, item := range n.values {
			if item.kind != kindString {
				*item = node{kind: kindString, scalar: MaskedValue}
				continue
			}
			if key, _, ok := strings.Cut(item.scalar, "="); ok {
				item.scalar = key + "=" + MaskedValue
			}
		}
	default:
		*n = node{kind: kindString, scalar: MaskedValue}
	}
}

func writeIndent(b *strings.Builder, indent int) {
	b.WriteString(strings.Repeat("  ", indent))
}

// writeObject writes the entries of an object, each on its own line.
func writeObject(b *strings.Builder, n *node, indent int) {
	for i, key := range n.keys {
		if i > 0 {
			writeIndent(b, indent)
		}
		b.WriteString(yamlString(key))
		b.WriteString(":")
		writeValue(b, n.values[i], indent)
	}
}

// writeValue writes what follows "key:" or "-": a scalar on the same line,
// or a nested block on the next lines.
func writeValue(b *strings.Builder, v *node, indent int) {
	switch {
	case v.kind == kindObject && len(v.keys) > 0:
		b.WriteString("\n")
		writeIndent(b, indent+1)
		writeObject(b, v, indent+1)
	case v.kind == kindArray && len(v.values) > 0:
		b.WriteString("\n")
		for _, item := range v.values {
			writeIndent(b, indent+1)
			b.WriteString("-")
			if item.kind == kindObject && len(item.keys) > 0 {
				b.WriteString(" ")
				writeObject(b, item, indent+2)
				continue
			}
			writeValue(b, item, indent+1)
		}
	default:
		b.WriteString(" ")
		b.WriteString(yamlScalar(v))
		b.WriteString("\n")
	}
}

func yamlScalar(v *node) string {
	switch v.kind {
	case kindNull:
		return "null"
	case kindBool, kindNumber:
		return v.scalar
	case kindString:
		return yamlString(v.scalar)
	case kindArray:
		return "[]"
	default:
		return "{}"
	}
}

// yamlString writes s plain when YAML reads it back as the same string,
// and double-quoted otherwise (a JSON string is a valid YAML double-quoted
// scalar).
func yamlString(s string) string {
	if plainSafe(s) {
		return s
	}
	return quote(s)
}

func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null", "~":
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	onlyDigits := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == '/', c == '@', c == '+', c == '-' && i > 0:
			onlyDigits = false
		case c >= '0' && c <= '9', c == '.', c == ':':
		default:
			return false
		}
	}
	// "1:30" or "1.2.3" could read as a number in some YAML parsers.
	return !onlyDigits
}
