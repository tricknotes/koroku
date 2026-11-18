package secrets

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces masked values in output.
const Placeholder = "[MASKED]"

const (
	// Values shorter than this are not masked: they are too short to be a
	// meaningful secret and would garble unrelated output.
	minMaskLen = 3
	// Values at least this long are masked wherever they appear. Shorter
	// ones (think "true" or "3000") are masked only as whole words.
	substringMaskLen = 8
)

// Masker removes secret values from text before it is logged.
type Masker struct {
	mu      sync.RWMutex
	secrets []string // sorted longest first so overlapping values mask fully
}

// NewMasker returns a Masker for the given secret values.
func NewMasker(values ...string) *Masker {
	m := &Masker{}
	m.Add(values...)
	return m
}

// Add registers more secret values. Multi-line values are masked line by line
// because log output is processed one line at a time.
func (m *Masker) Add(values ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool, len(m.secrets))
	for _, s := range m.secrets {
		seen[s] = true
	}
	for _, v := range values {
		for _, line := range strings.Split(v, "\n") {
			line = strings.TrimSpace(line)
			if len(line) < minMaskLen || seen[line] {
				continue
			}
			seen[line] = true
			m.secrets = append(m.secrets, line)
		}
	}
	sort.SliceStable(m.secrets, func(i, j int) bool { return len(m.secrets[i]) > len(m.secrets[j]) })
}

// Mask returns s with every registered secret replaced by Placeholder.
func (m *Masker) Mask(s string) string {
	if m == nil {
		return s
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, secret := range m.secrets {
		if !strings.Contains(s, secret) {
			continue
		}
		if len(secret) >= substringMaskLen {
			s = strings.ReplaceAll(s, secret, Placeholder)
		} else {
			s = replaceWholeWord(s, secret)
		}
	}
	return s
}

func replaceWholeWord(s, word string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, word)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		end := i + len(word)
		before := i == 0 || !isWordByte(s[i-1])
		after := end == len(s) || !isWordByte(s[end])
		if before && after {
			out.WriteString(s[:i])
			out.WriteString(Placeholder)
		} else {
			out.WriteString(s[:end])
		}
		s = s[end:]
	}
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// LineWriter masks secrets in a byte stream. It buffers until a newline so
// that a secret split across two writes is still caught, and forwards whole
// lines to the destination.
type LineWriter struct {
	mu     sync.Mutex
	dst    io.Writer
	masker *Masker
	buf    bytes.Buffer
}

// NewLineWriter wraps dst. Call Flush when the stream ends.
func NewLineWriter(dst io.Writer, masker *Masker) *LineWriter {
	return &LineWriter{dst: dst, masker: masker}
}

// MaxLineBytes bounds how much output without a line break is buffered.
// A longer run is cut into lines of this size, so a command that prints an
// endless line cannot make the writer grow without limit.
const MaxLineBytes = 64 << 10

// Write buffers p and emits every complete line, masked.
func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for w.buf.Len() > MaxLineBytes && bytes.IndexByte(w.buf.Bytes()[:MaxLineBytes], '\n') < 0 {
		chunk := string(w.buf.Next(MaxLineBytes))
		if _, err := io.WriteString(w.dst, w.masker.Mask(lastSegment(chunk))+"\n"); err != nil {
			return len(p), err
		}
	}
	for {
		data := w.buf.Bytes()
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := string(data[:i])
		w.buf.Next(i + 1)
		if _, err := io.WriteString(w.dst, w.masker.Mask(lastSegment(line))+"\n"); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// lastSegment drops everything up to the last interior carriage return.
// Progress output redraws a line with carriage returns; only its last state
// is kept, and no carriage return ever reaches the log file.
func lastSegment(line string) string {
	line = strings.TrimRight(line, "\r")
	if j := strings.LastIndexByte(line, '\r'); j >= 0 {
		line = line[j+1:]
	}
	return line
}

// Flush writes any buffered partial line.
func (w *LineWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		return nil
	}
	line := lastSegment(w.buf.String())
	w.buf.Reset()
	_, err := io.WriteString(w.dst, w.masker.Mask(line)+"\n")
	return err
}
