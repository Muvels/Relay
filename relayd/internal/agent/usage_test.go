package agent

import (
	"math"
	"testing"
)

func TestCPUUsedCores(t *testing.T) {
	used, ok := cpuUsedCores(
		cpuSnapshot{total: 100, idle: 60},
		cpuSnapshot{total: 120, idle: 65},
		8,
	)
	if !ok {
		t.Fatal("expected valid CPU sample")
	}
	if math.Abs(used-6) > 0.001 {
		t.Fatalf("used cores = %v, want 6", used)
	}
}

func TestCPUUsedCoresRejectsInvalidDelta(t *testing.T) {
	if _, ok := cpuUsedCores(
		cpuSnapshot{total: 100, idle: 60},
		cpuSnapshot{total: 100, idle: 60},
		8,
	); ok {
		t.Fatal("zero-length sample should be invalid")
	}
}

func TestParseNVIDIAUsage(t *testing.T) {
	got := parseNVIDIAUsage("0, 8192, 37\n1, N/A, N/A\n")
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2", len(got))
	}
	if got[0].GetIndex() != 0 || got[0].GetMemoryUsedMib() != 8192 ||
		!got[0].GetMemoryUsageAvailable() ||
		math.Abs(got[0].GetUtilization()-0.37) > 0.001 ||
		!got[0].GetUtilizationAvailable() {
		t.Fatalf("unexpected first device: %+v", got[0])
	}
	if got[1].GetMemoryUsageAvailable() || got[1].GetUtilizationAvailable() {
		t.Fatalf("N/A metrics should stay unavailable: %+v", got[1])
	}
}
