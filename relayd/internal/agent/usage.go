package agent

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// usageCollector reads volatile kernel/driver counters only. Samples are sent
// to the server in memory and are never written to the agent state directory.
type usageCollector struct {
	mu       sync.Mutex
	previous *cpuSnapshot
}

type cpuSnapshot struct {
	total float64
	idle  float64
}

func newUsageCollector() *usageCollector { return &usageCollector{} }

// ResetCPU makes the first sample of a new telemetry lease establish a fresh
// baseline instead of averaging CPU use across the time the page was closed.
func (c *usageCollector) ResetCPU() {
	c.mu.Lock()
	c.previous = nil
	c.mu.Unlock()
}

func (c *usageCollector) Sample(ctx context.Context) *relayv1.Heartbeat {
	hb := &relayv1.Heartbeat{SampledAtUnixMs: time.Now().UnixMilli()}

	if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
		current := cpuSnapshotFromTimes(times[0])
		c.mu.Lock()
		previous := c.previous
		c.previous = &current
		c.mu.Unlock()
		if previous != nil {
			if used, ok := cpuUsedCores(*previous, current, runtime.NumCPU()); ok {
				hb.CpuUsedCores = used
				hb.CpuUsageAvailable = true
			}
		}
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		hb.MemoryUsedMib = vm.Used / (1 << 20)
		hb.MemoryUsageAvailable = true
	}

	// disk.Usage uses statfs: a metadata syscall, not a directory scan.
	if root, err := disk.UsageWithContext(ctx, "/"); err == nil {
		hb.DiskFreeMib = root.Free / (1 << 20)
		hb.DiskTotalMib = root.Total / (1 << 20)
		hb.DiskUsageAvailable = true
	}

	hb.Accelerators = sampleNVIDIA(ctx)
	return hb
}

func cpuSnapshotFromTimes(t cpu.TimesStat) cpuSnapshot {
	// guest/guest_nice are already included in Linux user/nice counters.
	total := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq +
		t.Softirq + t.Steal
	return cpuSnapshot{total: total, idle: t.Idle + t.Iowait}
}

func cpuUsedCores(previous, current cpuSnapshot, cores int) (float64, bool) {
	total := current.total - previous.total
	idle := current.idle - previous.idle
	if total <= 0 || idle < 0 || cores <= 0 {
		return 0, false
	}
	busy := total - idle
	if busy < 0 {
		busy = 0
	}
	if busy > total {
		busy = total
	}
	return busy / total * float64(cores), true
}

// sampleNVIDIA makes one bounded selective nvidia-smi query. It runs only
// during a telemetry lease, so machines without an open Machines page never
// spawn this process. MPS/unified-memory metrics remain represented by the
// host memory sample because Apple exposes no equivalent stable CLI counter.
func sampleNVIDIA(ctx context.Context) []*relayv1.AcceleratorUsage {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(queryCtx, "nvidia-smi",
		"--query-gpu=index,memory.used,utilization.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNVIDIAUsage(string(out))
}

func parseNVIDIAUsage(output string) []*relayv1.AcceleratorUsage {
	var usages []*relayv1.AcceleratorUsage
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		usage := &relayv1.AcceleratorUsage{Index: int32(index)}
		if memory, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64); err == nil {
			usage.MemoryUsedMib = memory
			usage.MemoryUsageAvailable = true
		}
		if utilization, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
			usage.Utilization = min(max(utilization/100, 0), 1)
			usage.UtilizationAvailable = true
		}
		usages = append(usages, usage)
	}
	return usages
}
