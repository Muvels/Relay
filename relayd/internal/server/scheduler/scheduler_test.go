package scheduler

import (
	"strings"
	"testing"
)

func rtx() *Machine {
	return &Machine{
		ID: "m_rtx", Name: "rtx-desktop", Online: true,
		OS: "linux", Arch: "amd64",
		Executors: []string{"docker", "docker-cuda"},
		CPUCores:  16, MemoryMiB: 64 * 1024,
		Devices:      []AccelDevice{{Kind: "cuda", Index: 0, MemoryMiB: 24 * 1024}},
		CachedImages: map[string]bool{},
		Reserved:     Reservation{DeviceMemMiB: map[int]uint64{}},
	}
}

func spark() *Machine {
	return &Machine{
		ID: "m_spark", Name: "dgx-spark", Online: true,
		OS: "linux", Arch: "arm64",
		Executors:     []string{"docker", "docker-cuda"},
		UnifiedMemory: true,
		CPUCores:      20, MemoryMiB: 128 * 1024,
		Devices:      []AccelDevice{{Kind: "cuda", Index: 0, MemoryMiB: 128 * 1024}},
		CachedImages: map[string]bool{},
		Reserved:     Reservation{DeviceMemMiB: map[int]uint64{}},
	}
}

func mac() *Machine {
	return &Machine{
		ID: "m_mac", Name: "macbook", Online: true,
		OS: "darwin", Arch: "arm64",
		Executors:     []string{"docker", "native-mps"},
		UnifiedMemory: true,
		CPUCores:      12, MemoryMiB: 48 * 1024,
		Devices:      []AccelDevice{{Kind: "mps", Index: 0, MemoryMiB: 48 * 1024}},
		CachedImages: map[string]bool{},
		Reserved:     Reservation{DeviceMemMiB: map[int]uint64{}},
	}
}

func cudaReq(memGiB uint64) Request {
	return Request{AccelOptions: []AccelRequest{{Kind: "cuda", MemoryMiB: memGiB * 1024, Count: 1}}}
}

func TestFilterOffline(t *testing.T) {
	m := rtx()
	m.Online = false
	d, rej := Place(cudaReq(8), []*Machine{m})
	if d != nil {
		t.Fatal("placed on offline machine")
	}
	if len(rej) != 1 || rej[0].Reason != "offline" {
		t.Fatalf("rejections: %v", rej)
	}
}

func TestCudaOnMpsMachineExplainsFix(t *testing.T) {
	_, rej := Place(cudaReq(8), []*Machine{mac()})
	if len(rej) != 1 || !strings.Contains(rej[0].Reason, "relay.MPS") {
		t.Fatalf("want MPS hint, got %v", rej)
	}
}

func TestAdmissionRespectsLedger(t *testing.T) {
	m := rtx()
	m.Reserved.DeviceMemMiB[0] = 20 * 1024 // 20 of 24GB reserved
	d, rej := Place(cudaReq(8), []*Machine{m})
	if d != nil {
		t.Fatal("should not fit: only 4GB free")
	}
	if !strings.Contains(rej[0].Reason, "8GB free") {
		t.Fatalf("reason should name the shortfall: %v", rej)
	}
	if d2, _ := Place(cudaReq(4), []*Machine{m}); d2 == nil {
		t.Fatal("4GB should still fit")
	}
}

func TestUnifiedMemorySingleBudget(t *testing.T) {
	m := spark()
	req := Request{
		MemoryMiB:    90 * 1024,
		AccelOptions: []AccelRequest{{Kind: "cuda", MemoryMiB: 70 * 1024, Count: 1}},
	}
	// 90 + 70 = 160GB > 128GB pool → must reject even though each half fits.
	d, rej := Place(req, []*Machine{m})
	if d != nil {
		t.Fatal("unified pool must be one budget, not two")
	}
	if !strings.Contains(rej[0].Reason, "unified memory") {
		t.Fatalf("reason: %v", rej)
	}
	req.MemoryMiB = 40 * 1024 // 40 + 70 = 110 fits
	if d, _ := Place(req, []*Machine{m}); d == nil {
		t.Fatal("110GB of 128GB should fit")
	}
}

func TestAnyOfPrefersFirstOptionButFallsBack(t *testing.T) {
	req := Request{NativeOK: true, AccelOptions: []AccelRequest{
		{Kind: "cuda", MemoryMiB: 8 * 1024, Count: 1},
		{Kind: "mps", MemoryMiB: 8 * 1024},
	}}
	d, _ := Place(req, []*Machine{mac()})
	if d == nil || d.AccelKind != "mps" {
		t.Fatalf("mac should serve the MPS option, got %+v", d)
	}
	d, _ = Place(req, []*Machine{mac(), rtx()})
	if d == nil || d.AccelKind != "cuda" || d.MachineName != "rtx-desktop" {
		t.Fatalf("with both, CUDA (first option) should win: %+v", d)
	}
}

func TestImageCacheLocalityDominates(t *testing.T) {
	warm, cold := spark(), rtx()
	warm.CachedImages["relay-img:abc"] = true
	req := cudaReq(8)
	req.ImageTag = "relay-img:abc"
	d, _ := Place(req, []*Machine{cold, warm})
	if d == nil || d.MachineName != "dgx-spark" {
		t.Fatalf("warm cache should win: %+v", d)
	}
}

func TestBestFitKeepsBigSlotsOpen(t *testing.T) {
	small, big := rtx(), spark()
	d, _ := Place(cudaReq(8), []*Machine{big, small})
	if d == nil || d.MachineName != "rtx-desktop" {
		t.Fatalf("8GB job should pack onto the 24GB card, not the 128GB Spark: %+v", d)
	}
}

func TestExplicitTargetOnlyConsidersNamed(t *testing.T) {
	req := cudaReq(8)
	req.TargetNames = []string{"dgx-spark"}
	d, _ := Place(req, []*Machine{rtx(), spark()})
	if d == nil || d.MachineName != "dgx-spark" {
		t.Fatalf("explicit target ignored: %+v", d)
	}
}

func TestUnknownTargetExplains(t *testing.T) {
	req := cudaReq(8)
	req.TargetNames = []string{"nonexistent"}
	d, rej := Place(req, []*Machine{rtx()})
	if d != nil {
		t.Fatal("placed on unknown target")
	}
	if !strings.Contains(FormatRejections(rej), "no such machine") {
		t.Fatalf("rejections: %v", rej)
	}
}

func TestMultiGpuCount(t *testing.T) {
	m := rtx()
	m.Devices = append(m.Devices, AccelDevice{Kind: "cuda", Index: 1, MemoryMiB: 24 * 1024})
	req := Request{AccelOptions: []AccelRequest{{Kind: "cuda", MemoryMiB: 8 * 1024, Count: 2}}}
	d, _ := Place(req, []*Machine{m})
	if d == nil || len(d.DeviceIndices) != 2 {
		t.Fatalf("want 2 devices: %+v", d)
	}
	m.Reserved.DeviceMemMiB[1] = 20 * 1024
	d, rej := Place(req, []*Machine{m})
	if d != nil {
		t.Fatalf("only one device has 8GB free, got %+v", d)
	}
	if !strings.Contains(rej[0].Reason, "1 available") {
		t.Fatalf("reason: %v", rej)
	}
}

func TestWholeDeviceReservationWhenNoMemoryGiven(t *testing.T) {
	m := rtx()
	req := Request{AccelOptions: []AccelRequest{{Kind: "cuda", Count: 1}}}
	d, _ := Place(req, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] != 24*1024 {
		t.Fatalf("gpu='any' must reserve the whole device: %+v", d)
	}
	// Second any-GPU job must now queue.
	m.Reserved.DeviceMemMiB[0] = d.ReserveDeviceMiB[0]
	if d2, _ := Place(req, []*Machine{m}); d2 != nil {
		t.Fatal("device is fully reserved; second job must queue")
	}
}

func TestExplicitSharedReservationAllowsPacking(t *testing.T) {
	m := rtx()
	req := Request{AccelOptions: []AccelRequest{{
		Kind: "cuda", MemoryMiB: 8 * 1024, Count: 1, Exclusive: false,
	}}}
	d, _ := Place(req, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] != 8*1024 {
		t.Fatalf("shared request must reserve only its VRAM budget: %+v", d)
	}
	m.Reserved.DeviceMemMiB[0] = d.ReserveDeviceMiB[0]
	if d2, _ := Place(req, []*Machine{m}); d2 == nil || d2.DeviceIndices[0] != 0 {
		t.Fatalf("second shared request should pack onto the same GPU: %+v", d2)
	}
}

func TestExplicitExclusiveMinimumNeedsEmptyDevice(t *testing.T) {
	m := rtx()
	req := Request{AccelOptions: []AccelRequest{{
		Kind: "cuda", MemoryMiB: 8 * 1024, Count: 1, Exclusive: true,
	}}}
	d, _ := Place(req, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] != 24*1024 {
		t.Fatalf("exclusive request must reserve the whole selected device: %+v", d)
	}
	m.Reserved.DeviceMemMiB[0] = 1
	if d2, _ := Place(req, []*Machine{m}); d2 != nil {
		t.Fatalf("exclusive request landed on a shared device: %+v", d2)
	}
	m.Reserved.DeviceMemMiB[0] = 0
	m.Devices[0].MemoryMiB = 4 * 1024
	if d2, _ := Place(req, []*Machine{m}); d2 != nil {
		t.Fatalf("exclusive request ignored its minimum VRAM: %+v", d2)
	}
}

func TestCpuAdmission(t *testing.T) {
	m := rtx()
	m.Reserved.CPUs = 12
	req := Request{CPUs: 8}
	d, rej := Place(req, []*Machine{m})
	if d != nil {
		t.Fatal("12 of 16 reserved; 8 must not fit")
	}
	if !strings.Contains(rej[0].Reason, "CPUs free") {
		t.Fatalf("reason: %v", rej)
	}
}

func TestUnifiedWholeDeviceIsExclusive(t *testing.T) {
	m := spark()
	req := Request{AccelOptions: []AccelRequest{{Kind: "cuda", Count: 1}}} // "any GPU"
	d, _ := Place(req, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] == 0 {
		t.Fatalf("memory-less GPU on unified must reserve the pool, got %+v", d)
	}
	m.Reserved.DeviceMemMiB[0] = d.ReserveDeviceMiB[0]
	if d2, _ := Place(req, []*Machine{m}); d2 != nil {
		t.Fatal("second any-GPU job must queue on unified silicon")
	}
}

func TestUnifiedExplicitExclusiveReservesRemainingPool(t *testing.T) {
	m := spark()
	req := Request{AccelOptions: []AccelRequest{{
		Kind: "cuda", MemoryMiB: 24 * 1024, Count: 1, Exclusive: true,
	}}}
	d, _ := Place(req, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] != 128*1024 {
		t.Fatalf("exclusive unified request must reserve the remaining pool: %+v", d)
	}

	shared := req
	shared.AccelOptions[0].Exclusive = false
	d, _ = Place(shared, []*Machine{m})
	if d == nil || d.ReserveDeviceMiB[0] != 24*1024 {
		t.Fatalf("shared unified request must reserve only its budget: %+v", d)
	}
}

func TestUnifiedKindFiltering(t *testing.T) {
	m := mac() // has ONE mps device, zero cuda
	req := Request{NativeOK: true,
		AccelOptions: []AccelRequest{{Kind: "cuda", MemoryMiB: 1024, Count: 1}}}
	if d, _ := Place(req, []*Machine{m}); d != nil {
		t.Fatal("cuda request must not count mps devices")
	}
}

func TestMpsRequiresNativeCompatibleImage(t *testing.T) {
	req := Request{AccelOptions: []AccelRequest{{Kind: "mps", MemoryMiB: 8 * 1024}}}
	d, rej := Place(req, []*Machine{mac()}) // NativeOK false
	if d != nil {
		t.Fatal("docker-only image must not land on native MPS")
	}
	if !strings.Contains(rej[0].Reason, "pip-only") {
		t.Fatalf("reason should explain the image constraint: %v", rej)
	}
	req.NativeOK = true
	if d, _ := Place(req, []*Machine{mac()}); d == nil || d.AccelKind != "mps" {
		t.Fatalf("pip-only image should place on MPS: %+v", d)
	}
}

func TestServicesNeedDockerNotMps(t *testing.T) {
	req := Request{Kind: "service", NativeOK: true,
		AccelOptions: []AccelRequest{{Kind: "mps", MemoryMiB: 1024}}}
	d, rej := Place(req, []*Machine{mac()})
	if d != nil {
		t.Fatal("MPS services are impossible (no container path to Metal)")
	}
	if !strings.Contains(rej[0].Reason, "MPS") {
		t.Fatalf("reason: %v", rej)
	}
}

func TestNoMachines(t *testing.T) {
	d, rej := Place(cudaReq(1), nil)
	if d != nil {
		t.Fatal("no machines")
	}
	if !strings.Contains(FormatRejections(rej), "relay connect") {
		t.Fatalf("empty-fleet message should point at relay connect: %q",
			FormatRejections(rej))
	}
}
