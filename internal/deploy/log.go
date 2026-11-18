package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tricknotes/koroku/internal/secrets"
)

// deployLog appends to the log file of one deployment. Everything written
// through it is masked line by line.
type deployLog struct {
	file   *os.File
	lines  *secrets.LineWriter
	masker *secrets.Masker
}

func openDeployLog(path string, masker *secrets.Masker) (*deployLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create deploy log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deploy log: %w", err)
	}
	return &deployLog{file: f, lines: secrets.NewLineWriter(f, masker), masker: masker}, nil
}

// Writer returns the destination for command output.
func (l *deployLog) Writer() io.Writer { return l.lines }

// Step writes a headline that marks the start of a deploy step.
func (l *deployLog) Step(format string, args ...any) {
	_ = l.lines.Flush()
	_, _ = fmt.Fprintf(l.lines, "==> "+format+"\n", args...)
}

// Printf writes one informational line.
func (l *deployLog) Printf(format string, args ...any) {
	_ = l.lines.Flush()
	_, _ = fmt.Fprintf(l.lines, format+"\n", args...)
}

func (l *deployLog) Close() {
	_ = l.lines.Flush()
	_ = l.file.Close()
}

// appendLogLine adds a single line to a deployment log outside of a running
// job, for example when a queued deployment is canceled.
func appendLogLine(path, line string) {
	l, err := openDeployLog(path, nil)
	if err != nil {
		return
	}
	l.Printf("%s", line)
	l.Close()
}
