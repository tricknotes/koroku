package deploy

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envRawPattern = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]*$`)
)

// ValidEnvKey reports whether key is usable as an environment variable name.
func ValidEnvKey(key string) bool {
	return len(key) <= 255 && envKeyPattern.MatchString(key)
}

// ProvidedEnvPrefix starts the names of the variables koroku writes into
// every release's env file itself. Apps cannot save variables with it.
const ProvidedEnvPrefix = "KOROKU_"

// Provided is what the variables koroku provides describe (ProvidedEnv).
type Provided struct {
	// CommitSHA is the full commit of the release.
	CommitSHA string
	// AppURL is the app's primary URL when it is deployed; "" leaves out
	// KOROKU_APP_URL and KOROKU_APP_HOST.
	AppURL string
	// PRNumber is the pull request a review app runs (ARCHITECTURE 9.9);
	// 0 for other apps.
	PRNumber int
}

// ProvidedEnv returns the variables koroku adds to a release's env file:
// the commit it was built from, so that a Compose file can name images a
// CI built for that commit (`image: ...:sha-${KOROKU_COMMIT_SHORT}`) and
// an app can report its release; the app's URL and host, which a review
// app cannot write into its repository; and a review app's pull request.
// They take part in interpolation and are passed to every service like
// the app's own variables.
func ProvidedEnv(p Provided) map[string]string {
	short := p.CommitSHA
	if len(short) > 7 {
		short = short[:7]
	}
	out := map[string]string{
		"KOROKU_COMMIT_SHA":   p.CommitSHA,
		"KOROKU_COMMIT_SHORT": short,
	}
	if p.AppURL != "" {
		out["KOROKU_APP_URL"] = p.AppURL
		if u, err := url.Parse(p.AppURL); err == nil {
			out["KOROKU_APP_HOST"] = u.Host
		}
	}
	if p.PRNumber > 0 {
		out["KOROKU_PR_NUMBER"] = strconv.Itoa(p.PRNumber)
	}
	return out
}

// RenderEnvFile renders variables in the dotenv dialect Compose reads for
// both env_file and --env-file. Values are written so that Compose performs
// no interpolation on them:
//
//   - plain values are written as is;
//   - anything else is single-quoted, which Compose takes literally;
//   - values containing a single quote or a line break are double-quoted
//     with backslash escapes, including `\$` to keep dollars literal.
//
// The quoting rules were verified against Compose (see the integration tests).
func RenderEnvFile(vars map[string]string) (string, error) {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if !ValidEnvKey(k) {
			return "", fmt.Errorf("invalid environment variable name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		v := vars[k]
		if strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("environment variable %s contains a NUL byte", k)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(quoteEnvValue(v))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func quoteEnvValue(v string) string {
	if envRawPattern.MatchString(v) {
		return v
	}
	if !strings.ContainsAny(v, "'\n\r") {
		return "'" + v + "'"
	}
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"\n", `\n`,
		"\r", `\r`,
	)
	return `"` + replacer.Replace(v) + `"`
}

// ParseEnvFileValues reads back the values of an env file written by
// RenderEnvFile. It is not a general dotenv parser: it understands the
// three forms RenderEnvFile produces, and serves to mask the values a
// container receives (ARCHITECTURE 9.6).
func ParseEnvFileValues(content string) []string {
	entries := parseEnvFileEntries(content)
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry[1])
	}
	return values
}

// parseEnvFileEntries is ParseEnvFileValues with the keys: pairs of key
// and value in file order.
func parseEnvFileEntries(content string) [][2]string {
	var entries [][2]string
	for _, line := range strings.Split(content, "\n") {
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		var value string
		switch {
		case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'':
			value = raw[1 : len(raw)-1]
		case len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"':
			value = unquoteEnvValue(raw[1 : len(raw)-1])
		default:
			value = raw
		}
		entries = append(entries, [2]string{key, value})
	}
	return entries
}

func unquoteEnvValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory so that readers never observe a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp opens with O_EXCL, so it never writes through an existing
	// file or link, and the rename below replaces whatever is at path.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
