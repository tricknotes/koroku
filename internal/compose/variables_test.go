package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// formatVariables renders variables one per line for golden files.
func formatVariables(vars []Variable) string {
	var b strings.Builder
	for _, v := range vars {
		level := "optional"
		if v.Required {
			level = "required"
		}
		def := "-"
		if v.Default != nil {
			def = fmt.Sprintf("%q", *v.Default)
		}
		fmt.Fprintf(&b, "%s %s mandatory=%t default=%s message=%q\n", v.Name, level, v.Mandatory, def, v.Message)
	}
	return b.String()
}

func TestVariablesGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "variables.compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "variables.txt", []byte(formatVariables(Variables(data))))
}

func TestVariablesCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"none", "services:\n  web:\n    image: nginx\n", ""},
		{"plain", "x: $FOO and ${BAR}\n", "BAR required mandatory=false default=- message=\"\"\nFOO required mandatory=false default=- message=\"\"\n"},
		{"escaped", "x: $$FOO $${BAR}\n", ""},
		{"default", "x: ${A:-1} ${B-2}\n", "A optional mandatory=false default=\"1\" message=\"\"\nB optional mandatory=false default=\"2\" message=\"\"\n"},
		{"alternate", "x: ${A:+on}\n", "A optional mandatory=false default=- message=\"\"\n"},
		{"per-commit tag with a fallback", "image: app:${A:+sha-}${A:-latest}\n", "A optional mandatory=false default=\"latest\" message=\"\"\n"},
		{"mandatory", "x: ${A:?need A}\n", "A required mandatory=true default=- message=\"need A\"\n"},
		{"nested is optional", "x: ${A:-${B}}\n", "A optional mandatory=false default=\"${B}\" message=\"\"\nB optional mandatory=false default=- message=\"\"\n"},
		{"required anywhere wins", "x: ${A:-1}\ny: ${A}\n", "A required mandatory=false default=\"1\" message=\"\"\n"},
		{"comment", "# ${A}\nx: 1 # ${B}\ny: a#${C}\n", "C required mandatory=false default=- message=\"\"\n"},
		{"quoted hash", "x: \"# ${A}\"\n", "A required mandatory=false default=- message=\"\"\n"},
		{"key", "${A}: 1\n  - ${B}: 2\n", ""},
		{"list item", "  - KEY=${A}\n  - ${B}:${C}\n", "A required mandatory=false default=- message=\"\"\nB required mandatory=false default=- message=\"\"\nC required mandatory=false default=- message=\"\"\n"},
		{"block scalar", "command: |\n  echo # ${A}\n  key: ${B}\nnext: # ${C}\n", "A required mandatory=false default=- message=\"\"\nB required mandatory=false default=- message=\"\"\n"},
		{"malformed", "x: ${ A} ${1A} ${A unclosed\n", ""},
		{"crlf", "x: ${A}\r\n", "A required mandatory=false default=- message=\"\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVariables(Variables([]byte(tc.in))); got != tc.want {
				t.Errorf("Variables(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}
