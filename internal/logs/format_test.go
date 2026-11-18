package logs

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// realFollowOutput was captured from Docker 29.8 / Compose v5.8 with
// `docker compose logs --follow --timestamps --no-color` while a service was
// recreated and the project was removed (control characters as printed). It
// has two replicas of a service named job-worker-2, a service db1 with
// container_name set, a line that is only a newline, an escape sequence and
// a carriage return from the app, and Compose's own status messages.
const realFollowOutput = "web-1  | 2026-11-18T11:19:59.807655438Z err\n" +
	"web-1  | 2026-11-18T11:19:59.807681149Z hello\n" +
	"web-1  | 2026-11-18T11:19:59.807750868Z   indented | pipe\n" +
	"web-1  | 2026-11-18T11:19:59.807756148Z \n" +
	"web-1  | 2026-11-18T11:20:01.807925020Z no newline\x1b[31mred\x1b[0m\n" +
	"web-1  | 2026-11-18T11:20:01.807961478Z cr\rline\r\n" +
	"job-worker-2-2  | 2026-11-18T11:20:00.987236727Z w1\n" +
	"custom-name-logfmt  | 2026-11-18T11:19:59.255176405Z named\n" +
	"job-worker-2-1      | 2026-11-18T11:20:00.363608105Z w1\n" +
	"\r\x1b[Kweb-1 has been recreated\n" +
	"web-1               | 2026-11-18T11:23:45.013550899Z err\n" +
	"\r\x1b[Kjob-worker-2-1 exited with code 137\n" +
	"\n"

func TestParseRealComposeOutput(t *testing.T) {
	type entry struct{ service, time, text string }
	var got []entry
	for _, line := range strings.Split(strings.TrimSuffix(realFollowOutput, "\n"), "\n") {
		name, ts, text, ok := parseComposeLine([]byte(line))
		if !ok {
			continue
		}
		service, ok := serviceOfContainer(name)
		if !ok {
			t.Errorf("no service for container %q", name)
			continue
		}
		got = append(got, entry{service, ts.Format(time.RFC3339Nano), cleanText(text, 1024, false)})
	}
	want := []entry{
		{"web", "2026-11-18T11:19:59.807655438Z", "err"},
		{"web", "2026-11-18T11:19:59.807681149Z", "hello"},
		{"web", "2026-11-18T11:19:59.807750868Z", "  indented | pipe"},
		{"web", "2026-11-18T11:19:59.807756148Z", ""},
		{"web", "2026-11-18T11:20:01.80792502Z", "no newline\x1b[31mred\x1b[0m"},
		{"web", "2026-11-18T11:20:01.807961478Z", "cr�line"},
		{"job-worker-2", "2026-11-18T11:20:00.987236727Z", "w1"},
		{"custom-name-logfmt", "2026-11-18T11:19:59.255176405Z", "named"},
		{"job-worker-2", "2026-11-18T11:20:00.363608105Z", "w1"},
		{"web", "2026-11-18T11:23:45.013550899Z", "err"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseComposeLineRejects(t *testing.T) {
	for _, line := range []string{
		"",
		"web-1 has been recreated",
		"\r\x1b[Kweb-1 exited with code 137",
		"| 2026-11-18T11:19:59Z no name",
		"web-1  | not-a-time text",
		"web-1  |2026-11-18T11:19:59Z no space after the bar",
		"two words  | 2026-11-18T11:19:59Z text",
		"time=\"2026-11-18T11:19:59Z\" level=warning msg=\"a | b\"",
	} {
		if _, _, _, ok := parseComposeLine([]byte(line)); ok {
			t.Errorf("parseComposeLine(%q) accepted", line)
		}
	}
	// A timestamp without text and without the trailing space is an entry.
	if _, _, text, ok := parseComposeLine([]byte("web-1  | 2026-11-18T11:19:59Z")); !ok || len(text) != 0 {
		t.Errorf("bare timestamp: ok=%v text=%q", ok, text)
	}
}

func TestServiceOfContainer(t *testing.T) {
	for name, want := range map[string]string{
		"web-1":          "web",
		"web-12":         "web",
		"job-worker-2-1": "job-worker-2",
		"db1-1":          "db1",
		"redis7-3":       "redis7",
		"my.svc_x-1":     "my.svc_x",
		"custom-name":    "custom-name",
		"web":            "web",
		"-1":             "",
		"_access-1":      "",
		"..-1":           "",
		"a/b-1":          "",
	} {
		got, ok := serviceOfContainer(name)
		if got != want || ok != (want != "") {
			t.Errorf("serviceOfContainer(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		max       int
		truncated bool
		want      string
	}{
		{"plain", "hello", 10, false, "hello"},
		{"tab and colour kept", "a\tb\x1b[31m", 20, false, "a\tb\x1b[31m"},
		{"trailing carriage return", "hello\r", 10, false, "hello"},
		{"control characters", "a\x00b\x07c\x7f", 20, false, "a�b�c�"},
		{"invalid utf-8", "ok\xff\xfe", 20, false, "ok��"},
		{"multibyte", "ログ", 20, false, "ログ"},
		{"cut", "abcdefghij", 4, false, "abcd" + truncatedMark},
		{"cut by the splitter", "abcd", 10, true, "abcd" + truncatedMark},
		{"cut between runes", "ログログ", 7, false, "ログ" + truncatedMark},
		{"replacement counts towards the limit", "\xff\xff\xff", 6, false, "��" + truncatedMark},
	}
	for _, tc := range cases {
		got := cleanText([]byte(tc.raw), tc.max, tc.truncated)
		if got != tc.want {
			t.Errorf("%s: cleanText = %q, want %q", tc.name, got, tc.want)
		}
		if !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r") {
			t.Errorf("%s: %q is not a clean single line", tc.name, got)
		}
	}
}

func TestEntryRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 18, 3, 4, 5, 120, time.UTC)
	line := appendEntry(nil, ts.In(jst), "text with  spaces ")
	if want := "2026-11-18T03:04:05.000000120Z text with  spaces \n"; string(line) != want {
		t.Fatalf("appendEntry = %q, want %q", line, want)
	}
	got, text, ok := parseEntry(bytes.TrimSuffix(line, []byte("\n")))
	if !ok || !got.Equal(ts) || string(text) != "text with  spaces " {
		t.Errorf("parseEntry = %v, %q, %v", got, text, ok)
	}
	if _, _, ok := parseEntry([]byte("garbage without a timestamp")); ok {
		t.Error("parseEntry accepted garbage")
	}
}

func TestSplitterBoundsLines(t *testing.T) {
	type out struct {
		line      string
		truncated bool
	}
	var got []out
	sp := splitter{max: 8, emit: func(line []byte, truncated bool) {
		got = append(got, out{string(line), truncated})
	}}
	// Lines arrive in arbitrary pieces; the long one spans many writes.
	for _, piece := range []string{"ab", "c\nde", "\n", strings.Repeat("x", 5), strings.Repeat("y", 5000), "z\nlast\nunfinished"} {
		if n, err := sp.Write([]byte(piece)); n != len(piece) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if cap(sp.buf) > 64 {
			t.Fatalf("splitter buffer grew to %d bytes", cap(sp.buf))
		}
	}
	want := []out{{"abc", false}, {"de", false}, {"xxxxxyyy", true}, {"last", false}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	sp.reset()
	_, _ = sp.Write([]byte("fresh\n"))
	if last := got[len(got)-1]; last.line != "fresh" {
		t.Errorf("after reset got %q, want the unfinished line dropped", last.line)
	}
}
