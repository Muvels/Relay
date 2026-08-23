package server

import (
	"strings"
	"testing"
)

func TestToSchedRequestPreservesExplicitGpuExclusivity(t *testing.T) {
	exclusive := true
	spec := &RunSpecJSON{Accelerators: []AccJSON{{
		Kind: "cuda", MemoryMiB: 24 * 1024, Count: 1, Exclusive: &exclusive,
	}}}
	req := toSchedRequest(spec)
	if len(req.AccelOptions) != 1 || !req.AccelOptions[0].Exclusive {
		t.Fatalf("exclusive flag lost at scheduler boundary: %+v", req)
	}
}

func TestToSchedRequestTreatsLegacyZeroMemoryAsExclusive(t *testing.T) {
	spec := &RunSpecJSON{Accelerators: []AccJSON{{Kind: "cuda", Count: 1}}}
	req := toSchedRequest(spec)
	if len(req.AccelOptions) != 1 || !req.AccelOptions[0].Exclusive {
		t.Fatalf("legacy whole-device request became shared: %+v", req)
	}
}

func TestToSchedRequestPreservesLegacyNumericMemoryAsShared(t *testing.T) {
	spec := &RunSpecJSON{Accelerators: []AccJSON{{
		Kind: "cuda", MemoryMiB: 24 * 1024, Count: 1,
	}}}
	req := toSchedRequest(spec)
	if len(req.AccelOptions) != 1 || req.AccelOptions[0].Exclusive {
		t.Fatalf("legacy numeric VRAM request lost shared semantics: %+v", req)
	}
}

func TestResourceChipNamesGpuSharingMode(t *testing.T) {
	shared := resourceChip(`{"accelerators":[{"kind":"cuda","memory_mib":24576,"count":1,"exclusive":false}]}`)
	if !strings.Contains(shared, "shared") {
		t.Fatalf("shared mode missing from chip: %q", shared)
	}
	exclusive := resourceChip(`{"accelerators":[{"kind":"cuda","memory_mib":24576,"count":1,"exclusive":true}]}`)
	if !strings.Contains(exclusive, "exclusive") {
		t.Fatalf("exclusive mode missing from chip: %q", exclusive)
	}
}

func TestSharedGpuValidationRequiresReservation(t *testing.T) {
	shared := false
	spec := &RunSpecJSON{Accelerators: []AccJSON{{
		Kind: "cuda", Exclusive: &shared,
	}}}
	if err := spec.validateAccelerators(); err == nil {
		t.Fatal("shared GPU without a VRAM reservation was accepted")
	}
	spec.Accelerators[0].MemoryMiB = 8 * 1024
	if err := spec.validateAccelerators(); err != nil {
		t.Fatalf("valid shared GPU rejected: %v", err)
	}
}
