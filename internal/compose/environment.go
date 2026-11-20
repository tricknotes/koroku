package compose

import "strings"

// EnvironmentHint is one entry of a service's `environment:` as
// ScanEnvironment could read it. Values are not kept: callers only need to
// know whether the file fixes the value itself.
type EnvironmentHint struct {
	Service string
	Key     string
	// Literal is true when the entry has a value without interpolation
	// ("RAILS_ENV: production"). Entries without a value (passed through
	// from the environment) and values with a `$` are not literal: the
	// variables koroku injects still decide them.
	Literal bool
}

// ScanEnvironment reads the `environment:` entries of every service, in the
// map form (KEY: value) and the list form (- KEY=value), without a YAML
// parser. Like ScanServices it only serves the console's drafts and skips
// what it cannot read (flow style, anchors, merges).
func ScanEnvironment(content []byte) []EnvironmentHint {
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")

	var (
		hints      []EnvironmentHint
		inServices bool
		serviceInd = -1 // indentation of service names
		service    string
		keyInd     = -1 // indentation of the keys of a service
		envIndent  = -1 // indentation of the environment key being read
		entryInd   = -1 // indentation of its entries
	)
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
			serviceInd, service, envIndent, entryInd = -1, "", -1, -1
			continue
		}
		if !inServices {
			continue
		}
		if serviceInd == -1 {
			serviceInd = indent
		}
		if indent == serviceInd {
			service, keyInd, envIndent, entryInd = "", -1, -1, -1
			if m := keyLine.FindStringSubmatch(line); m != nil {
				service = firstNonEmpty(m[2], m[3], m[4])
			}
			continue
		}
		if service == "" || indent < serviceInd {
			continue
		}

		if envIndent >= 0 {
			// List items may sit at the same indentation as their key.
			isItem := listItem.MatchString(line)
			if indent > envIndent || (isItem && indent == envIndent) {
				if entryInd == -1 {
					entryInd = indent
				}
				if indent == entryInd {
					if hint, ok := environmentEntry(service, line, isItem); ok {
						hints = append(hints, hint)
					}
				}
				continue
			}
			envIndent, entryInd = -1, -1
		}
		if keyInd == -1 {
			keyInd = indent
		}
		// Only the service's own key, not one nested in another key.
		if m := keyLine.FindStringSubmatch(line); m != nil && indent == keyInd &&
			firstNonEmpty(m[2], m[3], m[4]) == "environment" && strings.TrimSpace(m[5]) == "" {
			envIndent = indent
		}
	}
	return hints
}

// environmentEntry reads one line of an environment block.
func environmentEntry(service, line string, isItem bool) (EnvironmentHint, bool) {
	if isItem {
		item := unquote(strings.TrimSpace(listItem.FindStringSubmatch(line)[2]))
		key, value, hasValue := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !isEnvName(key) {
			return EnvironmentHint{}, false
		}
		return EnvironmentHint{Service: service, Key: key, Literal: hasValue && !strings.Contains(value, "$")}, true
	}
	m := keyLine.FindStringSubmatch(line)
	if m == nil {
		return EnvironmentHint{}, false
	}
	key := firstNonEmpty(m[2], m[3], m[4])
	if !isEnvName(key) {
		return EnvironmentHint{}, false
	}
	value := strings.TrimSpace(m[5])
	// "KEY:", "KEY: null" and "KEY: ~" pass the variable through.
	literal := value != "" && value != "null" && value != "~" && !strings.Contains(value, "$")
	return EnvironmentHint{Service: service, Key: key, Literal: literal}, true
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
