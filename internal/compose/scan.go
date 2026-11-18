package compose

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ServiceHint is what ScanServices could read about a service.
type ServiceHint struct {
	Name string
	// Ports are the container-side ports from `ports` and `expose`.
	Ports []int
}

var (
	keyLine   = regexp.MustCompile(`^( *)(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9._-]+)):(?:\s+(.*))?$`)
	listItem  = regexp.MustCompile(`^( *)-\s*(.*)$`)
	portDigit = regexp.MustCompile(`^[0-9]+$`)
)

// ScanServices reads service names and container ports from a Compose file
// without a YAML parser. It only has to prefill a form (the user can correct
// it, and the deploy itself relies on `docker compose config`), so it
// understands the common block layout and ignores what it cannot read:
// anchors, merges, includes and flow-style service maps yield no hints.
func ScanServices(content []byte) []ServiceHint {
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")

	var (
		hints       []*ServiceHint
		inServices  bool
		serviceInd  = -1 // indentation of service names
		current     *ServiceHint
		portsIndent = -1 // indentation of the ports/expose key being read
		pendingLong bool // inside a long-syntax port item waiting for target:
	)
	addPort := func(raw string) {
		if current == nil {
			return
		}
		if port, ok := containerPort(raw); ok {
			for _, existing := range current.Ports {
				if existing == port {
					return
				}
			}
			current.Ports = append(current.Ports, port)
		}
	}

	for _, raw := range lines {
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.ContainsRune(leadingWhitespace(line), '\t') {
			continue // tabs are not valid YAML indentation
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if indent == 0 {
			m := keyLine.FindStringSubmatch(line)
			inServices = m != nil && firstNonEmpty(m[2], m[3], m[4]) == "services" && strings.TrimSpace(m[5]) == ""
			serviceInd, current, portsIndent = -1, nil, -1
			continue
		}
		if !inServices {
			continue
		}
		if serviceInd == -1 {
			serviceInd = indent
		}

		// A service name.
		if indent == serviceInd {
			portsIndent, pendingLong = -1, false
			m := keyLine.FindStringSubmatch(line)
			if m == nil {
				current = nil
				continue
			}
			current = &ServiceHint{Name: firstNonEmpty(m[2], m[3], m[4])}
			hints = append(hints, current)
			continue
		}
		if current == nil || indent < serviceInd {
			continue
		}

		// Inside a ports/expose list.
		if portsIndent >= 0 && indent >= portsIndent {
			if m := listItem.FindStringSubmatch(line); m != nil {
				item := strings.TrimSpace(m[2])
				pendingLong = false
				if key, value, ok := strings.Cut(item, ":"); ok && isWord(key) && !portDigit.MatchString(key) {
					// Long syntax: "- target: 80" or "- published: ...".
					pendingLong = true
					if key == "target" {
						addPort(value)
					}
				} else {
					addPort(item)
				}
				continue
			}
			if pendingLong && indent > portsIndent {
				if key, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && key == "target" {
					addPort(value)
				}
				continue
			}
			portsIndent, pendingLong = -1, false
		}

		// A key of the service.
		if m := keyLine.FindStringSubmatch(line); m != nil {
			key, value := firstNonEmpty(m[2], m[3], m[4]), strings.TrimSpace(m[5])
			if key != "ports" && key != "expose" {
				continue
			}
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				// Flow sequence on one line: ports: ["3000:3000", 8080]
				for _, item := range strings.Split(value[1:len(value)-1], ",") {
					addPort(item)
				}
				continue
			}
			if value == "" {
				portsIndent, pendingLong = indent, false
			}
		}
	}

	out := make([]ServiceHint, 0, len(hints))
	for _, h := range hints {
		sort.Ints(h.Ports)
		out = append(out, *h)
	}
	return out
}

// containerPort extracts the container-side port of a short-syntax port
// entry such as 3000, "8080:80", "127.0.0.1:8080:80/tcp" or "9000-9002".
func containerPort(raw string) (int, bool) {
	s := strings.Trim(strings.TrimSpace(raw), `"'`)
	s, _, _ = strings.Cut(s, "/")
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s, _, _ = strings.Cut(s, "-")
	if !portDigit.MatchString(s) {
		return 0, false // for example ${PORT}
	}
	port, err := strconv.Atoi(s)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// stripComment removes a trailing comment. A '#' starts a comment only at
// the start of the line or after whitespace, and not inside quotes.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return strings.TrimRight(line, " \t")
}

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
