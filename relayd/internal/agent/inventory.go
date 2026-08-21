package agent

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// DetectInventory probes the machine. It must never fail because a machine with
// nothing detected is still a valid (CPU, docker-less) fleet member.
func DetectInventory(ctx context.Context, docker *dockerClient, version string) *relayv1.MachineInventory {
	hostname, _ := os.Hostname()
	inv := &relayv1.MachineInventory{
		Hostname:     strings.TrimSuffix(hostname, ".local"),
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CpuCores:     uint32(runtime.NumCPU()),
		MemoryMib:    detectMemoryMiB(),
		AgentVersion: version,
	}

	dockerUp := docker != nil && docker.Ping(ctx) == nil
	if dockerUp {
		inv.Executors = append(inv.Executors, "docker")
	}

	gpus, unified := detectNvidia(ctx, inv.MemoryMib)
	inv.Accelerators = append(inv.Accelerators, gpus...)
	if unified {
		inv.UnifiedMemory = true
	}
	if len(gpus) > 0 && dockerUp && runtime.GOOS == "linux" {
		inv.Executors = append(inv.Executors, "docker-cuda")
	}

	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		inv.UnifiedMemory = true
		inv.Accelerators = append(inv.Accelerators, &relayv1.DetectedAccelerator{
			Kind:             "mps",
			Name:             appleChipName(),
			MemoryMib:        inv.MemoryMib, // unified pool
			MemoryUnreliable: true,
		})
		if native, mps := NativeAvailable(); native {
			inv.Executors = append(inv.Executors, "native")
			if mps {
				inv.Executors = append(inv.Executors, "native-mps")
			}
		}
	}
	return inv
}

func detectMemoryMiB() uint64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			if b, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return b / (1 << 20)
			}
		}
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if kib, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
					fields := strings.Fields(kib)
					if len(fields) >= 1 {
						if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
							return v / 1024
						}
					}
				}
			}
		}
	}
	return 0
}

// detectNvidia shells out to nvidia-smi. On unified-memory parts (DGX Spark
// GB10) memory.total reports "[N/A]"/"Not Supported"; we then fall back to
// system memory and flag it unreliable. The scheduler budgets one pool.
func detectNvidia(ctx context.Context, systemMemMiB uint64) ([]*relayv1.DetectedAccelerator, bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, false
	}
	var gpus []*relayv1.DetectedAccelerator
	unified := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		name := strings.TrimSpace(parts[1])
		memRaw := strings.TrimSpace(parts[2])
		gpu := &relayv1.DetectedAccelerator{Kind: "cuda", Name: name, Index: int32(idx)}
		if mem, err := strconv.ParseUint(memRaw, 10, 64); err == nil && mem > 0 {
			gpu.MemoryMib = mem
		} else {
			// "[N/A]" / "Not Supported": unified-memory silicon.
			gpu.MemoryMib = systemMemMiB
			gpu.MemoryUnreliable = true
			unified = true
		}
		gpus = append(gpus, gpu)
	}
	return gpus, unified
}

func appleChipName() string {
	out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	if err != nil {
		return "Apple Silicon"
	}
	return strings.TrimSpace(string(out))
}
