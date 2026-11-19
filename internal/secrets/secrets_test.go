package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{0x42}, KeySize) }

func TestBoxRoundTrip(t *testing.T) {
	box, err := NewBox(testKey())
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	sealed, err := box.Seal([]byte("postgres://user:pw@db/app"), "app:1:DATABASE_URL")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("postgres")) {
		t.Error("ciphertext contains the plaintext")
	}
	plain, err := box.Open(sealed, "app:1:DATABASE_URL")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plain) != "postgres://user:pw@db/app" {
		t.Errorf("plaintext = %q", plain)
	}

	again, _ := box.Seal([]byte("postgres://user:pw@db/app"), "app:1:DATABASE_URL")
	if bytes.Equal(sealed, again) {
		t.Error("two encryptions of the same value must differ (random nonce)")
	}
}

func TestBoxRejectsTampering(t *testing.T) {
	box, _ := NewBox(testKey())
	sealed, _ := box.Seal([]byte("value"), "app:1:KEY")

	if _, err := box.Open(sealed, "app:2:KEY"); err == nil {
		t.Error("a value moved to another row must not decrypt")
	}
	corrupted := append([]byte(nil), sealed...)
	corrupted[len(corrupted)-1] ^= 1
	if _, err := box.Open(corrupted, "app:1:KEY"); err == nil {
		t.Error("a corrupted value must not decrypt")
	}
	if _, err := box.Open([]byte("short"), "app:1:KEY"); err == nil {
		t.Error("a truncated value must not decrypt")
	}
	other, _ := NewBox(bytes.Repeat([]byte{0x01}, KeySize))
	if _, err := other.Open(sealed, "app:1:KEY"); err == nil {
		t.Error("a different key must not decrypt")
	}
	if _, err := NewBox([]byte("too short")); err == nil {
		t.Error("NewBox must reject a short key")
	}
}

func TestBoxFingerprint(t *testing.T) {
	box, _ := NewBox(testKey())
	a := box.Fingerprint("env:1:A", []byte("value"))
	if a != box.Fingerprint("env:1:A", []byte("value")) {
		t.Error("the fingerprint is not stable")
	}
	if len(a) != 32 || strings.Contains(a, "value") {
		t.Errorf("fingerprint = %q", a)
	}
	for name, other := range map[string]string{
		"value":        box.Fingerprint("env:1:A", []byte("value2")),
		"key":          box.Fingerprint("env:1:B", []byte("value")),
		"app":          box.Fingerprint("env:2:A", []byte("value")),
		"split":        box.Fingerprint("env:1:Av", []byte("alue")),
		"empty value":  box.Fingerprint("env:1:A", nil),
		"other master": mustBox(t, bytes.Repeat([]byte{0x01}, KeySize)).Fingerprint("env:1:A", []byte("value")),
	} {
		if other == a {
			t.Errorf("a different %s gives the same fingerprint", name)
		}
	}
}

func mustBox(t *testing.T, key []byte) *Box {
	t.Helper()
	box, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "master.key")

	key, created, err := LoadOrCreateKey(path)
	if err != nil || !created || len(key) != KeySize {
		t.Fatalf("first call: key=%d bytes created=%v err=%v", len(key), created, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}

	again, created, err := LoadOrCreateKey(path)
	if err != nil || created || !bytes.Equal(key, again) {
		t.Fatalf("second call must load the same key: created=%v err=%v", created, err)
	}
}

func TestLoadKeyRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()

	open := filepath.Join(dir, "open.key")
	if err := os.WriteFile(open, []byte(strings.Repeat("ab", KeySize)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(open); err == nil || !strings.Contains(err.Error(), "too open") {
		t.Errorf("world-readable key: err = %v", err)
	}

	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKey(bad)
	if err == nil {
		t.Fatal("malformed key must be rejected")
	}
	if strings.Contains(err.Error(), "not-hex") {
		t.Error("the error must not echo the key file content")
	}
	// A malformed existing file must never be silently replaced.
	if _, _, err := LoadOrCreateKey(bad); err == nil {
		t.Error("LoadOrCreateKey must not overwrite a malformed key")
	}
}

func TestMaskerMask(t *testing.T) {
	m := NewMasker("s3cr3t-password", "true", "3000", "ab", "", "line-one-secret\nline-two-secret")
	cases := []struct{ in, want string }{
		{"connecting with s3cr3t-password now", "connecting with [MASKED] now"},
		{"url=postgres://u:s3cr3t-password@db", "url=postgres://u:[MASKED]@db"},
		{"s3cr3t-passwords3cr3t-password", "[MASKED][MASKED]"},
		// Short values are masked only as whole words.
		{"DEBUG=true", "DEBUG=[MASKED]"},
		{"construed as truely fine", "construed as truely fine"},
		{"listening on :3000", "listening on :[MASKED]"},
		{"took 30000ms", "took 30000ms"},
		// Values under three characters are never masked.
		{"ab cab", "ab cab"},
		{"first line-one-secret then line-two-secret", "first [MASKED] then [MASKED]"},
		{"nothing to hide", "nothing to hide"},
	}
	for _, tc := range cases {
		if got := m.Mask(tc.in); got != tc.want {
			t.Errorf("Mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMaskerOverlappingSecrets(t *testing.T) {
	m := NewMasker("tokenvalue", "tokenvalue-extended")
	if got := m.Mask("x tokenvalue-extended y"); got != "x [MASKED] y" {
		t.Errorf("longest secret must win, got %q", got)
	}
}

func TestMaskerAddAndNil(t *testing.T) {
	m := NewMasker()
	if got := m.Mask("ghs_abcdefghij"); got != "ghs_abcdefghij" {
		t.Errorf("unexpected masking: %q", got)
	}
	m.Add("ghs_abcdefghij")
	if got := m.Mask("token ghs_abcdefghij"); got != "token [MASKED]" {
		t.Errorf("got %q", got)
	}
	var nilMasker *Masker
	if got := nilMasker.Mask("plain"); got != "plain" {
		t.Errorf("nil masker changed the text: %q", got)
	}
}

func TestLineWriterMasksAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := NewLineWriter(&out, NewMasker("s3cr3t-password"))

	for _, chunk := range []string{"start s3cr3t-", "password end\nsecond ", "line\npartial s3cr3t-password"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := out.String(); got != "start [MASKED] end\nsecond line\n" {
		t.Errorf("before flush: %q", got)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want := "start [MASKED] end\nsecond line\npartial [MASKED]\n"
	if got := out.String(); got != want {
		t.Errorf("after flush: %q, want %q", got, want)
	}
	if err := w.Flush(); err != nil || out.String() != want {
		t.Errorf("a second flush must be a no-op")
	}
}

func TestLineWriterCollapsesCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	w := NewLineWriter(&out, NewMasker())
	_, _ = w.Write([]byte("pulling 10%\rpulling 50%\rpulling 100%\r\nwindows line\r\n"))
	if got := out.String(); got != "pulling 100%\nwindows line\n" {
		t.Errorf("got %q", got)
	}
}

// TestLineWriterNeverEmitsCarriageReturns: the deploy log is streamed as
// Server-Sent Events, where a carriage return ends a line. Neither a complete
// nor a flushed partial line may carry one.
func TestLineWriterNeverEmitsCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	w := NewLineWriter(&out, NewMasker())
	_, _ = w.Write([]byte("harmless\rid: 1\revent: end\nunterminated\rid: 2\revent: end\rdata: succeeded"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\r") {
		t.Errorf("carriage return in the output: %q", out.String())
	}
	if got := out.String(); got != "event: end\ndata: succeeded\n" {
		t.Errorf("output = %q, want only the last state of each line", got)
	}
}

// TestLineWriterBoundsUnterminatedOutput: output without any line break must
// not be buffered without limit.
func TestLineWriterBoundsUnterminatedOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewLineWriter(&out, NewMasker("s3cr3t-password"))
	chunk := bytes.Repeat([]byte("a"), 10<<10)
	for i := 0; i < 100; i++ { // 1000 KiB, never a newline
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() < 900<<10 {
		t.Errorf("only %d bytes were passed on; the rest is buffered", out.Len())
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if len(line) > MaxLineBytes {
			t.Fatalf("a line of %d bytes was emitted", len(line))
		}
	}
	_ = w.Flush()
	if strings.Count(out.String(), "a") != 1000<<10 {
		t.Errorf("output lost or duplicated bytes: %d", strings.Count(out.String(), "a"))
	}
}
