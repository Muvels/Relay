// Package scheduler implements Relay's three-phase placement:
//
//	filter: hard compatibility (online, explicit target, executor kind)
//	admission: capacity now, from the reservation ledger (never live
//	            utilization: time-slicing lies and GB10 NVML is broken)
//	score: best fit among survivors (image-cache locality dominates,
//	            then tightest accelerator fit, then fewest active runs)
//
// Place is a pure function: (request, snapshots) → decision | rejections.
// Every rejection carries a human reason that `relay ps` shows verbatim.
package scheduler

import (
	"fmt"
	"sort"
	"strings"
)

type AccelDevice struct {
	Kind      string // "cuda" | "mps"
	Index     int
	MemoryMiB uint64 // on unified machines this mirrors system memory
}

// Reservation is what the ledger says is already promised on a machine.
type Reservation struct {
	CPUs      float64
	MemoryMiB uint64
	// Per-device reserved accelerator memory, keyed by device index.
	DeviceMemMiB map[int]uint64
	ActiveRuns   int
}

type Machine struct {
	ID            string
	Name          string
	Online        bool
	OS            string
	Arch          string
	Executors     []string
	UnifiedMemory bool
	CPUCores      float64
	MemoryMiB     uint64
	Devices       []AccelDevice
	CachedImages  map[string]bool
	Reserved      Reservation
}

type AccelRequest struct {
	Kind      string
	MemoryMiB uint64
	Count     int
}

type Request struct {
	CPUs         float64
	MemoryMiB    uint64
	AccelOptions []AccelRequest // any-of, in user preference order
	ImageTag     string
	TargetNames  []string // empty = auto
	Kind         string   // "job" | "service"
	// NativeOK: the image is pip-only, so containerless (MPS) execution is
	// possible. Docker-only images can never be placed on native executors.
	NativeOK bool
}

type Decision struct {
	MachineID     string
	MachineName   string
	AccelKind     string
	DeviceIndices []int
	// Reserve* is what the ledger must record for this run.
	ReserveCPUs      float64
	ReserveMemoryMiB uint64
	ReserveDeviceMiB map[int]uint64
}

type Rejection struct {
	MachineName string
	Reason      string
}

func (r Rejection) String() string { return r.MachineName + " ✗ " + r.Reason }

// FormatRejections renders the queue explanation stored in run.detail.
func FormatRejections(rejections []Rejection) string {
	if len(rejections) == 0 {
		return "queued: no machines are connected. Run `relay connect` to add one"
	}
	parts := make([]string, 0, len(rejections))
	for _, r := range rejections {
		parts = append(parts, r.String())
	}
	return "queued: " + strings.Join(parts, "; ")
}

func executorFor(kind string) string {
	switch kind {
	case "cuda":
		return "docker-cuda"
	case "mps":
		return "native-mps"
	}
	return "docker"
}

func hasExecutor(m *Machine, name string) bool {
	for _, e := range m.Executors {
		if e == name {
			return true
		}
	}
	return false
}

// optionUsable gates one accel option on this machine (executor present and,
// for MPS, native-compatible image).
func optionUsable(req Request, m *Machine, opt AccelRequest) bool {
	if opt.Kind == "mps" && !req.NativeOK {
		return false
	}
	return hasExecutor(m, executorFor(opt.Kind))
}

func mib(v uint64) string {
	if v%1024 == 0 {
		return fmt.Sprintf("%dGB", v/1024)
	}
	return fmt.Sprintf("%dMB", v)
}

// Place picks the best machine, or explains per machine why it can't.
func Place(req Request, machines []*Machine) (*Decision, []Rejection) {
	var rejections []Rejection
	var candidates []*Decision
	scores := map[string]float64{}

	explicit := map[string]bool{}
	for _, name := range req.TargetNames {
		explicit[name] = true
	}

	for _, m := range machines {
		if len(explicit) > 0 && !explicit[m.Name] {
			continue // not addressed; no rejection noise
		}
		if reason := filter(req, m); reason != "" {
			rejections = append(rejections, Rejection{m.Name, reason})
			continue
		}
		decision, reason := admit(req, m)
		if decision == nil {
			rejections = append(rejections, Rejection{m.Name, reason})
			continue
		}
		candidates = append(candidates, decision)
		scores[m.ID] = score(req, m, decision)
	}

	if len(explicit) > 0 {
		known := map[string]bool{}
		for _, m := range machines {
			known[m.Name] = true
		}
		for name := range explicit {
			if !known[name] {
				rejections = append(rejections, Rejection{
					name, "no such machine. Check `relay fleet`"})
			}
		}
	}

	if len(candidates) == 0 {
		return nil, rejections
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return scores[candidates[i].MachineID] > scores[candidates[j].MachineID]
	})
	return candidates[0], rejections
}

// ---------------------------------------------------------------- phase 1

func filter(req Request, m *Machine) string {
	if !m.Online {
		return "offline"
	}
	// Services run supervised containers (port publish, restart, exposure);
	// MPS/native cannot host them.
	if req.Kind == "service" {
		if !hasExecutor(m, "docker") {
			return "services need Docker on the machine"
		}
		for _, opt := range req.AccelOptions {
			if opt.Kind == "mps" {
				return "services cannot use MPS (no container path to Metal)"
			}
		}
	}
	if len(req.AccelOptions) == 0 {
		if hasExecutor(m, "docker") {
			return ""
		}
		// Docker-less machines can still run pip-only jobs natively.
		if hasExecutor(m, "native") && req.NativeOK {
			return ""
		}
		if hasExecutor(m, "native") {
			return "no Docker; native execution needs a pip-only image"
		}
		return "no usable executor (is Docker running there?)"
	}
	var missing []string
	for _, opt := range req.AccelOptions {
		if opt.Kind == "mps" && !req.NativeOK {
			missing = append(missing, "mps (image has Docker-only steps. "+
				"MPS runs natively and needs a pip-only image)")
			continue
		}
		if hasExecutor(m, executorFor(opt.Kind)) {
			return ""
		}
		missing = append(missing, opt.Kind)
	}
	needs := strings.Join(missing, "/")
	if len(req.AccelOptions) == 1 && req.AccelOptions[0].Kind == "cuda" {
		for _, d := range m.Devices {
			if d.Kind == "mps" {
				return "requires CUDA; machine has MPS → add relay.MPS(...) to accelerator="
			}
		}
	}
	return fmt.Sprintf("requires %s; machine has no matching executor", needs)
}

// ---------------------------------------------------------------- phase 2

func admit(req Request, m *Machine) (*Decision, string) {
	if req.CPUs > 0 {
		free := m.CPUCores - m.Reserved.CPUs
		if req.CPUs > free {
			return nil, fmt.Sprintf("%.0f of %.0f CPUs free, needs %.0f",
				free, m.CPUCores, req.CPUs)
		}
	}

	// Unified memory (DGX Spark, Apple Silicon): ONE pool serves system
	// memory and accelerator working set, so admit against the sum.
	if m.UnifiedMemory {
		return admitUnified(req, m)
	}

	if req.MemoryMiB > 0 {
		free := m.MemoryMiB - m.Reserved.MemoryMiB
		if req.MemoryMiB > free {
			return nil, fmt.Sprintf("%s of %s RAM free, needs %s",
				mib(free), mib(m.MemoryMiB), mib(req.MemoryMiB))
		}
	}

	if len(req.AccelOptions) == 0 {
		return &Decision{
			MachineID: m.ID, MachineName: m.Name,
			ReserveCPUs: req.CPUs, ReserveMemoryMiB: req.MemoryMiB,
			ReserveDeviceMiB: map[int]uint64{},
		}, ""
	}

	var lastReason string
	for _, opt := range req.AccelOptions {
		if !optionUsable(req, m, opt) {
			continue
		}
		indices, reason := pickDevices(opt, m)
		if indices == nil {
			lastReason = reason
			continue
		}
		reserve := map[int]uint64{}
		perDevice := opt.MemoryMiB
		for _, idx := range indices {
			amount := perDevice
			if amount == 0 { // "any GPU" reserves the device whole
				amount = deviceMem(m, idx)
			}
			reserve[idx] = amount
		}
		return &Decision{
			MachineID: m.ID, MachineName: m.Name,
			AccelKind: opt.Kind, DeviceIndices: indices,
			ReserveCPUs: req.CPUs, ReserveMemoryMiB: req.MemoryMiB,
			ReserveDeviceMiB: reserve,
		}, ""
	}
	if lastReason == "" {
		lastReason = "no accelerator option fits"
	}
	return nil, lastReason
}

func admitUnified(req Request, m *Machine) (*Decision, string) {
	var reservedPool uint64 = m.Reserved.MemoryMiB
	for _, v := range m.Reserved.DeviceMemMiB {
		reservedPool += v
	}
	free := m.MemoryMiB - min(reservedPool, m.MemoryMiB)

	if len(req.AccelOptions) == 0 {
		if req.MemoryMiB > free {
			return nil, fmt.Sprintf("%s of %s unified memory free, needs %s",
				mib(free), mib(m.MemoryMiB), mib(req.MemoryMiB))
		}
		return &Decision{
			MachineID: m.ID, MachineName: m.Name,
			ReserveCPUs: req.CPUs, ReserveMemoryMiB: req.MemoryMiB,
			ReserveDeviceMiB: map[int]uint64{},
		}, ""
	}

	// Try each usable option fully (devices AND memory) before moving on.
	var lastReason string
	for i := range req.AccelOptions {
		opt := &req.AccelOptions[i]
		if !optionUsable(req, m, *opt) {
			continue
		}
		var kindDevices []AccelDevice
		for _, d := range m.Devices {
			if d.Kind == opt.Kind {
				kindDevices = append(kindDevices, d)
			}
		}
		if len(kindDevices) == 0 {
			lastReason = fmt.Sprintf("no %s devices", opt.Kind)
			continue
		}
		count := max(opt.Count, 1)
		if count > len(kindDevices) {
			lastReason = fmt.Sprintf("needs %d %s devices, machine has %d",
				count, opt.Kind, len(kindDevices))
			continue
		}
		// A memory-less GPU request on unified silicon reserves the whole
		// remaining pool for the device. "Any GPU" means exclusive use,
		// exactly like whole-device reservation on discrete cards.
		perDevice := opt.MemoryMiB
		wholeDevice := perDevice == 0
		// Skip devices already carrying reservations when exclusive.
		var picked []AccelDevice
		for _, d := range kindDevices {
			if wholeDevice && m.Reserved.DeviceMemMiB[d.Index] > 0 {
				continue
			}
			picked = append(picked, d)
			if len(picked) == count {
				break
			}
		}
		if len(picked) < count {
			lastReason = fmt.Sprintf("%d free %s device(s) needed, %d available",
				count, strings.ToUpper(opt.Kind), len(picked))
			continue
		}
		accelNeed := perDevice * uint64(count)
		if wholeDevice {
			accelNeed = free - min(req.MemoryMiB, free) // the rest of the pool
		}
		need := req.MemoryMiB + accelNeed
		if need > free {
			lastReason = fmt.Sprintf(
				"%s of %s unified memory free, needs %s (RAM %s + accelerator %s)",
				mib(free), mib(m.MemoryMiB), mib(need),
				mib(req.MemoryMiB), mib(accelNeed))
			continue
		}
		d := &Decision{
			MachineID: m.ID, MachineName: m.Name,
			AccelKind:   opt.Kind,
			ReserveCPUs: req.CPUs, ReserveMemoryMiB: req.MemoryMiB,
			ReserveDeviceMiB: map[int]uint64{},
		}
		reservePer := perDevice
		if wholeDevice {
			reservePer = max64(accelNeed/uint64(count), 1)
		}
		for _, dev := range picked {
			d.DeviceIndices = append(d.DeviceIndices, dev.Index)
			d.ReserveDeviceMiB[dev.Index] = reservePer
		}
		return d, ""
	}
	if lastReason == "" {
		lastReason = "no accelerator option fits"
	}
	return nil, lastReason
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func deviceMem(m *Machine, index int) uint64 {
	for _, d := range m.Devices {
		if d.Index == index {
			return d.MemoryMiB
		}
	}
	return 0
}

// pickDevices chooses Count devices of the requested kind with enough free
// memory, best-fit (least leftover) first.
func pickDevices(opt AccelRequest, m *Machine) ([]int, string) {
	count := max(opt.Count, 1)
	type fit struct {
		index    int
		free     uint64
		leftover uint64
	}
	var fits []fit
	var kindDevices int
	for _, d := range m.Devices {
		if d.Kind != opt.Kind {
			continue
		}
		kindDevices++
		free := d.MemoryMiB - min(m.Reserved.DeviceMemMiB[d.Index], d.MemoryMiB)
		need := opt.MemoryMiB
		if need == 0 {
			need = d.MemoryMiB // whole-device reservation
		}
		if free >= need {
			fits = append(fits, fit{d.Index, free, free - need})
		}
	}
	if kindDevices == 0 {
		return nil, fmt.Sprintf("no %s devices", opt.Kind)
	}
	if len(fits) < count {
		if opt.MemoryMiB > 0 {
			return nil, fmt.Sprintf(
				"%d %s device(s) with %s free needed, %d available",
				count, strings.ToUpper(opt.Kind), mib(opt.MemoryMiB), len(fits))
		}
		return nil, fmt.Sprintf("%d free %s device(s) needed, %d available",
			count, strings.ToUpper(opt.Kind), len(fits))
	}
	sort.Slice(fits, func(i, j int) bool { return fits[i].leftover < fits[j].leftover })
	indices := make([]int, count)
	for i := 0; i < count; i++ {
		indices[i] = fits[i].index
	}
	sort.Ints(indices)
	return indices, ""
}

// ---------------------------------------------------------------- phase 3

func score(req Request, m *Machine, d *Decision) float64 {
	s := 0.0
	if req.ImageTag != "" && m.CachedImages[req.ImageTag] {
		s += 100 // warm image beats everything else
	}
	// Tightest accelerator fit: keep big slots open for big jobs.
	var leftover uint64
	for idx, reserve := range d.ReserveDeviceMiB {
		free := deviceMem(m, idx) - min(m.Reserved.DeviceMemMiB[idx], deviceMem(m, idx))
		if free > reserve {
			leftover += free - reserve
		}
	}
	s -= float64(leftover) / 1024.0 * 0.1 // soft: must never outweigh cache
	// Small jobs shouldn't camp on scarce unified-memory big iron.
	if m.UnifiedMemory && len(req.AccelOptions) == 0 {
		s -= 5
	}
	s -= float64(m.Reserved.ActiveRuns) // spread when otherwise equal
	return s
}
