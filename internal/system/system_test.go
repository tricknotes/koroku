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

func TestInfoSwap(t *testing.T) {
	cases := []struct {
		name    string
		meminfo string
		want    *Swap
	}{
		{"in use", meminfo + "SwapTotal:       2097148 kB\nSwapFree:        1994748 kB\n", &Swap{TotalBytes: 2097148 * 1024, UsedBytes: 102400 * 1024}},
		{"unused", meminfo + "SwapTotal:       2097148 kB\nSwapFree:        2097148 kB\n", &Swap{TotalBytes: 2097148 * 1024}},
		{"no swap on the host", meminfo + "SwapTotal:             0 kB\nSwapFree:              0 kB\n", nil},
		{"not reported", meminfo, nil},
		{"only the total", meminfo + "SwapTotal:       2097148 kB\n", nil},
	}
	for _, tc := range cases {
		h := Host{DataDir: t.TempDir(), MeminfoPath: writeMeminfo(t, tc.meminfo)}
		got := h.Info(context.Background()).Swap
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: Swap = %+v, want nil", tc.name, got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: Swap = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestDiskUsageError(t *testing.T) {
	if _, err := (Host{DataDir: "/nonexistent/koroku"}).DiskUsage(); err == nil {
		t.Error("expected an error for a missing directory")
	}
}

func TestAvailableMemory(t *testing.T) {
	h := Host{MeminfoPath: writeMeminfo(t, meminfo+"SwapTotal:       2097148 kB\nSwapFree:        1048576 kB\n")}
	got, err := h.AvailableMemory()
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(2513204+1048576) * 1024; got != want {
		t.Errorf("AvailableMemory = %d, want %d", got, want)
	}
	h = Host{MeminfoPath: writeMeminfo(t, meminfo)}
	if got, err := h.AvailableMemory(); err != nil || got != 2513204*1024 {
		t.Errorf("without swap: %d, %v", got, err)
	}
}
