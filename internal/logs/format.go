package logs

import (
	"bytes"
	"hash/fnv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// entryTimeLayout is RFC 3339 in UTC with a fixed nine fractional
	// digits, so entries have a constant-width prefix.
	entryTimeLayout = "2006-01-02T15:04:05.000000000Z"
	dateLayout      = "2006-01-02"

	truncatedMark = " ...[truncated]"

	// readLineCap bounds a line read back from a file. Entries are written
	// well below it; anything longer is damage and is cut.
	readLineCap = MaxLineBytesLimit + 1024
)

// appendEntry appends the file representation of one entry to buf.
func appendEntry(buf []byte, ts time.Time, text string) []byte {
	buf = ts.UTC().AppendFormat(buf, entryTimeLayout)
	buf = append(buf, ' ')
	buf = append(buf, text...)
	return append(buf, '\n')
}

// parseEntry splits one file line (without the newline) into its timestamp
// and text. The text aliases line.
func parseEntry(line []byte) (ts time.Time, text []byte, ok bool) {
	stamp := line
	if i := bytes.IndexByte(line, ' '); i >= 0 {
		stamp, text = line[:i], line[i+1:]
	}
	ts, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return time.Time{}, nil, false
	}
	return ts.UTC(), text, true
}

// eraseLine is what Compose prints in front of its own status messages.
var eraseLine = []byte("\r\x1b[K")

// parseComposeLine splits one line of
// `docker compose logs --timestamps --no-color` into the container name
// (without the project prefix), the timestamp and the text:
//
//	web-1           | 2026-11-18T11:19:59.807681149Z hello
//
// The name is padded with spaces to align the bars. Status messages of
// Compose itself ("web-1 has been recreated") have no bar and timestamp and
// are rejected. The text aliases line.
func parseComposeLine(line []byte) (name string, ts time.Time, text []byte, ok bool) {
	for bytes.HasPrefix(line, eraseLine) {
		line = line[len(eraseLine):]
	}
	bar := bytes.IndexByte(line, '|')
	if bar <= 0 {
		return "", time.Time{}, nil, false
	}
	rawName := bytes.TrimRight(line[:bar], " ")
	if len(rawName) == 0 || bytes.ContainsAny(rawName, " \t") {
		return "", time.Time{}, nil, false
	}
	rest := line[bar+1:]
	if len(rest) == 0 || rest[0] != ' ' {
		return "", time.Time{}, nil, false
	}
	rest = rest[1:]
	stamp := rest
	if i := bytes.IndexByte(rest, ' '); i >= 0 {
		stamp, text = rest[:i], rest[i+1:]
	}
	ts, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return "", time.Time{}, nil, false
	}
	return string(rawName), ts.UTC(), text, true
}

// serviceOfContainer derives the service name from the container name that
// Compose prints. Compose names containers <project>-<service>-<number> and
// prints them without the project, so the service is everything before the
// last "-<number>"; that holds for service names that themselves contain
// dashes or end in digits. A service with a custom container_name is printed
// under that name, which is then used as it is. It reports false when the
// result is not usable as a directory name.
func serviceOfContainer(name string) (string, bool) {
	service := name
	if i := strings.LastIndexByte(name, '-'); i > 0 && i < len(name)-1 && allDigits(name[i+1:]) {
		service = name[:i]
	}
	if strings.HasPrefix(service, "_") || !servicePattern.MatchString(service) {
		return "", false
	}
	return service, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// cleanText turns raw line content into the text of an entry: valid UTF-8,
// on one line, at most max bytes plus the truncation mark. Control
// characters other than tab and escape (colour sequences are common) and
// invalid bytes become U+FFFD. The result only depends on the input, so the
// same line always compares equal after a restart.
func cleanText(raw []byte, max int, truncated bool) string {
	raw = bytes.TrimRight(raw, "\r")
	clean := true
	for _, b := range raw {
		if b >= utf8.RuneSelf || (b < 0x20 && b != '\t' && b != 0x1b) || b == 0x7f {
			clean = false
			break
		}
	}
	if clean && len(raw) <= max {
		if truncated {
			return string(raw) + truncatedMark
		}
		return string(raw)
	}

	var b strings.Builder
	b.Grow(min(len(raw), max) + len(truncatedMark))
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		if (r == utf8.RuneError && size <= 1) || (r < 0x20 && r != '\t' && r != 0x1b) || r == 0x7f {
			r = utf8.RuneError
		}
		if b.Len()+utf8.RuneLen(r) > max {
			truncated = true
			break
		}
		b.WriteRune(r)
		raw = raw[size:]
	}
	if truncated {
		b.WriteString(truncatedMark)
	}
	return b.String()
}

// entryKey identifies an entry for duplicate detection.
func entryKey(service string, ts time.Time, text string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(service))
	var stamp [9]byte
	n := ts.UnixNano()
	for i := 1; i < len(stamp); i++ {
		stamp[i] = byte(n >> (8 * (i - 1)))
	}
	_, _ = h.Write(stamp[:])
	_, _ = h.Write([]byte(text))
	return h.Sum64()
}

// splitter cuts a byte stream into lines of bounded length. A line longer
// than max is emitted with its first max bytes and truncated set; the rest
// of it is discarded. Memory use is bounded by max whatever the input.
type splitter struct {
	max int
	// emit receives one line without its newline. The slice is only valid
	// during the call.
	emit func(line []byte, truncated bool)

	buf  []byte
	over bool
}

// Write consumes p. It never fails.
func (s *splitter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p)
			break
		}
		if len(s.buf) == 0 && !s.over && i <= s.max {
			s.emit(p[:i], false)
		} else {
			s.add(p[:i])
			s.emit(s.buf, s.over)
			s.buf, s.over = s.buf[:0], false
		}
		p = p[i+1:]
	}
	return n, nil
}

func (s *splitter) add(p []byte) {
	if s.over {
		return
	}
	if room := s.max - len(s.buf); len(p) > room {
		s.buf = append(s.buf, p[:room]...)
		s.over = true
		return
	}
	s.buf = append(s.buf, p...)
}

// reset drops an unfinished line.
func (s *splitter) reset() { s.buf, s.over = s.buf[:0], false }
