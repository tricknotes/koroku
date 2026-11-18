package runner

import (
	"context"
	"io"
	"sync"
)

// Fake records commands instead of executing them. Tests set Handler to
// script the output and result of each call.
type Fake struct {
	// Handler decides the outcome of a command. A nil Handler succeeds silently.
	Handler func(ctx context.Context, cmd Cmd) (stdout string, err error)

	mu    sync.Mutex
	calls []Cmd
}

// Run records cmd, invokes the handler and writes its stdout to cmd.Stdout.
func (f *Fake) Run(ctx context.Context, cmd Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	handler := f.Handler
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if handler == nil {
		return nil
	}
	out, err := handler(ctx, cmd)
	if out != "" && cmd.Stdout != nil {
		if _, werr := io.WriteString(cmd.Stdout, out); werr != nil {
			return werr
		}
	}
	return err
}

// Calls returns a copy of every command seen so far.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.calls...)
}

// Reset forgets recorded calls.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}
