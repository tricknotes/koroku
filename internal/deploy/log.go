package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
)

// deployLog appends to the log file of one deployment. Everything written
// through it is masked line by line.
type deployLog struct {
	file   *os.File
	lines  *secrets.LineWriter
	masker *secrets.Masker
	now    func() time.Time
}

// Step keys written to the deploy log. The console splits the log into
// steps on these headlines (see docs/API.md).
const (
	StepFetch    = "fetch"
	StepPrepare  = "prepare"
	StepValidate = "validate"
	StepReuse    = "reuse"
	StepBuild    = "build"
	StepPull     = "pull"
	StepBackup   = "backup"
	StepStart    = "start"
	StepRoute    = "route"
	StepCheck    = "check"
	StepDone     = "done"
	StepFailed   = "failed"
	StepRollback = "rollback"
	StepCleanup  = "cleanup"
)

func openDeployLog(path string, masker *secrets.Masker, now func() time.Time) (*deployLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create deploy log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deploy log: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	limited := &cappedWriter{dst: f, remaining: maxDeployLogBytes}
	if info, err := f.Stat(); err == nil {
		limited.remaining -= info.Size()
	}
	return &deployLog{file: f, lines: secrets.NewLineWriter(limited, masker), masker: masker, now: now}, nil
}

// Writer returns the destination for command output.
func (l *deployLog) Writer() io.Writer { return l.lines }

// Step writes a headline that marks the start of a deploy step:
//
//	==> <RFC 3339 time> <key> <title>
//
// The time lets a reader derive how long each step took without a separate
// record of steps: the log file is the only record there is.
func (l *deployLog) Step(key, format string, args ...any) {
	_ = l.lines.Flush()
	title := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.lines, "==> %s %s %s\n", l.now().UTC().Format(time.RFC3339), key, title)
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
	l, err := openDeployLog(path, nil, nil)
	if err != nil {
		return
	}
	l.Printf("%s", line)
	l.Close()
}

// maxDeployLogBytes bounds one deployment's log file. A build that prints
// without end must not fill the disk every app shares.
const maxDeployLogBytes = 50 << 20

// cappedWriter stops writing once its budget is used up, leaving a note.
// It never reports an error for dropped output: the command whose output
// this is must not fail because of the log.
type cappedWriter struct {
	dst       io.Writer
	remaining int64
	noted     bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.remaining <= 0 {
		if !c.noted {
			c.noted = true
			_, _ = io.WriteString(c.dst, "[log truncated: the size limit of the log was reached]\n")
		}
		return len(p), nil
	}
	if int64(len(p)) > c.remaining {
		_, _ = c.dst.Write(p[:c.remaining])
		c.remaining = 0
		_, _ = c.Write(nil)
		return len(p), nil
	}
	c.remaining -= int64(len(p))
	return c.dst.Write(p)
}
