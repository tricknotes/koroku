// Package system reports host resource usage for the system screen and for
// the deploy-time disk check.
package system

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// Disk describes the filesystem holding the data directory.
type Disk struct {
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// Memory describes host memory in bytes.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// Swap describes host swap space in bytes.
type Swap struct {
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// Info is the payload of GET /api/system. Sections that could not be read
// are nil; Swap is also nil on a host without swap.
type Info struct {
	Disk          *Disk   `json:"disk"`
	Memory        *Memory `json:"memory"`
	Swap          *Swap   `json:"swap"`
	DockerVersion string  `json:"docker_version"`
}

// Prober reads host information.
type Prober interface {
	// Info gathers everything, tolerating partial failures.
	Info(ctx context.Context) Info
	// DiskUsage reports usage of the data directory filesystem.
	DiskUsage() (Disk, error)
}

// Host is the real Prober.
type Host struct {
	// DataDir is the directory whose filesystem is measured.
	DataDir string
	// Runner executes `docker version`.
	Runner runner.Runner
	// MeminfoPath overrides /proc/meminfo in tests.
	MeminfoPath string
}

// Info implements Prober.
func (h Host) Info(ctx context.Context) Info {
	var info Info
	if d, err := h.DiskUsage(); err == nil {
		info.Disk = &d
	}
	if m, swap, err := h.memory(); err == nil {
		info.Memory = &m
		info.Swap = swap
	}
	info.DockerVersion = h.dockerVersion(ctx)
	return info
}

// DiskUsage implements Prober. Usage is computed the way df does: space
// reserved for root counts as used.
func (h Host) DiskUsage() (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(h.DataDir, &st); err != nil {
		return Disk{}, fmt.Errorf("statfs %s: %w", h.DataDir, err)
	}
	blockSize := uint64(st.Bsize)
	total := st.Blocks * blockSize
	used := (st.Blocks - st.Bfree) * blockSize
	usable := used + st.Bavail*blockSize
	d := Disk{TotalBytes: total, UsedBytes: used}
	if usable > 0 {
		d.UsedPercent = float64(used) / float64(usable) * 100
	}
	return d, nil
}

// memory reads memory and swap usage from meminfo. The swap is nil when the
// host has none or the file does not report it.
func (h Host) memory() (Memory, *Swap, error) {
	path := h.MeminfoPath
	if path == "" {
		path = "/proc/meminfo"
	}
	f, err := os.Open(path)
	if err != nil {
		return Memory{}, nil, fmt.Errorf("open meminfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	var m Memory
	var swapTotal, swapFree uint64
	found, swapFound := 0, 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.TotalBytes = kb * 1024
			found++
		case "MemAvailable":
			m.AvailableBytes = kb * 1024
			found++
		case "SwapTotal":
			swapTotal = kb * 1024
			swapFound++
		case "SwapFree":
			swapFree = kb * 1024
			swapFound++
		}
	}
	if err := scanner.Err(); err != nil {
		return Memory{}, nil, fmt.Errorf("read meminfo: %w", err)
	}
	if found < 2 {
		return Memory{}, nil, fmt.Errorf("meminfo: MemTotal or MemAvailable missing")
	}
	var swap *Swap
	if swapFound == 2 && swapTotal > 0 && swapFree <= swapTotal {
		swap = &Swap{TotalBytes: swapTotal, UsedBytes: swapTotal - swapFree}
	}
	return m, swap, nil
}

func (h Host) dockerVersion(ctx context.Context) string {
	if h.Runner == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runner.Output(ctx, h.Runner, runner.Cmd{
		Name: "docker",
		Args: []string{"version", "--format", "{{.Server.Version}}"},
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
