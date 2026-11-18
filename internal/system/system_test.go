package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
)

const meminfo = `MemTotal:        4018456 kB
MemFree:          201232 kB
MemAvailable:    2513204 kB
Buffers:          102312 kB
`

func writeMeminfo(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInfo(t *testing.T) {
	fake := &runner.Fake{Handler: func(context.Context, runner.Cmd) (string, error) {
		return "29.8.0\n", nil
	}}
	h := Host{DataDir: t.TempDir(), Runner: fake, MeminfoPath: writeMeminfo(t, meminfo)}

	info := h.Info(context.Background())
	if info.Disk == nil || info.Disk.TotalBytes == 0 {
		t.Errorf("Disk = %+v", info.Disk)
	} else if info.Disk.UsedPercent < 0 || info.Disk.UsedPercent > 100 {
		t.Errorf("UsedPercent = %v", info.Disk.UsedPercent)
	}
	if info.Memory == nil || info.Memory.TotalBytes != 4018456*1024 || info.Memory.AvailableBytes != 2513204*1024 {
		t.Errorf("Memory = %+v", info.Memory)
	}
	if info.DockerVersion != "29.8.0" {
		t.Errorf("DockerVersion = %q", info.DockerVersion)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].String() != "docker version --format {{.Server.Version}}" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestInfoToleratesFailures(t *testing.T) {
	fake := &runner.Fake{Handler: func(context.Context, runner.Cmd) (string, error) {
		return "", errors.New("docker not running")
	}}
	h := Host{
		DataDir:     filepath.Join(t.TempDir(), "missing"),
		Runner:      fake,
		MeminfoPath: writeMeminfo(t, "garbage\n"),
	}
	info := h.Info(context.Background())
	if info.Disk != nil || info.Memory != nil || info.DockerVersion != "" {
		t.Errorf("expected empty info, got %+v", info)
	}
}

func TestDiskUsageError(t *testing.T) {
	if _, err := (Host{DataDir: "/nonexistent/paas"}).DiskUsage(); err == nil {
		t.Error("expected an error for a missing directory")
	}
}
