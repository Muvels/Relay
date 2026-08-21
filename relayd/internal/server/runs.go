package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
	"github.com/matteomarolt/relay/relayd/internal/server/scheduler"
)

// RunSpecJSON is the HTTP-facing mirror of relay.v1.RunSpec (minus run_id
// and the scheduler's decision fields, which the server assigns).
type RunSpecJSON struct {
	App             string            `json:"app"`
	Function        string            `json:"function"`
	EntryFile       string            `json:"entry_file"`
	CPUs            float64           `json:"cpus"`
	MemoryMiB       uint64            `json:"memory_mib"`
	TimeoutS        uint32            `json:"timeout_s"`
	Accelerators    []AccJSON         `json:"accelerators"`
	TargetNames     []string          `json:"target_names,omitempty"`
	Volumes         map[string]string `json:"volumes,omitempty"` // mount → name
	SecretNames     []string          `json:"secret_names,omitempty"`
	Kind            string            `json:"kind,omitempty"` // "job" | "service"
	ServicePort     uint32            `json:"service_port,omitempty"`
	Expose          string            `json:"expose,omitempty"` // private|public
	AuthNone        bool              `json:"auth_none,omitempty"`
	ServiceKey      string            `json:"service_key,omitempty"` // server-minted
	NativeEnv       *NativeEnvJSON    `json:"native_env,omitempty"`
	ImageTag        string            `json:"image_tag"`
	ImageDockerfile string            `json:"image_dockerfile"`
	BundleSha       string            `json:"bundle_sha256"`
	ArgsSha         string            `json:"args_sha256"`
	RuntimeProtocol uint32            `json:"runtime_protocol"`
	PythonMinor     string            `json:"python_minor"`
}

type AccJSON struct {
	Kind      string `json:"kind"`
	MemoryMiB uint64 `json:"memory_mib"`
	Count     uint32 `json:"count"`
}

type NativeEnvJSON struct {
	Supported   bool              `json:"supported"`
	PythonMinor string            `json:"python_minor,omitempty"`
	PipPackages []string          `json:"pip_packages,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// reservationJSON persists a scheduler decision on the run row; the ledger
// is derived from these, so restarts lose nothing.
type reservationJSON struct {
	CPUs         float64           `json:"cpus,omitempty"`
	MemoryMiB    uint64            `json:"memory_mib,omitempty"`
	DeviceMemMiB map[string]uint64 `json:"device_mem_mib,omitempty"`
	AccelKind    string            `json:"accel_kind,omitempty"`
	DeviceIdx    []int32           `json:"device_indices,omitempty"`
}

func (spec *RunSpecJSON) toProto(runID string, d *scheduler.Decision) *relayv1.RunSpec {
	accs := make([]*relayv1.Accelerator, 0, len(spec.Accelerators))
	for _, a := range spec.Accelerators {
		accs = append(accs, &relayv1.Accelerator{
			Kind: a.Kind, MemoryMib: a.MemoryMiB, Count: a.Count,
		})
	}
	out := &relayv1.RunSpec{
		RunId:     runID,
		App:       spec.App,
		Function:  spec.Function,
		EntryFile: spec.EntryFile,
		Resources: &relayv1.Resources{
			Cpus:               spec.CPUs,
			MemoryMib:          spec.MemoryMiB,
			TimeoutS:           spec.TimeoutS,
			AcceleratorOptions: accs,
		},
		ImageTag:        spec.ImageTag,
		ImageDockerfile: spec.ImageDockerfile,
		BundleSha256:    spec.BundleSha,
		ArgsSha256:      spec.ArgsSha,
		RuntimeProtocol: spec.RuntimeProtocol,
		PythonMinor:     spec.PythonMinor,
		Volumes:         spec.Volumes,
		SecretNames:     spec.SecretNames,
		Kind:            spec.Kind,
		ServicePort:     spec.ServicePort,
		Expose:          spec.Expose,
		ServiceKey:      spec.ServiceKey,
		ServiceAuthNone: spec.AuthNone,
	}
	if n := spec.NativeEnv; n != nil {
		out.NativeEnv = &relayv1.NativeEnv{
			Supported:   n.Supported,
			PythonMinor: n.PythonMinor,
			PipPackages: n.PipPackages,
			Env:         n.Env,
		}
	}
	if d != nil {
		out.AcceleratorKind = d.AccelKind
		for _, idx := range d.DeviceIndices {
			out.DeviceIndices = append(out.DeviceIndices, int32(idx))
		}
	}
	return out
}

// Runs owns the run state machine and placement.
type Runs struct {
	store *Store
	fleet *Fleet
	logs  *LogHub

	startedAt time.Time  // server boot; gates the janitor's lost-marking
	mu        sync.Mutex // serializes dispatch decisions
}

func NewRuns(store *Store, fleet *Fleet, logs *LogHub) *Runs {
	return &Runs{store: store, fleet: fleet, logs: logs, startedAt: time.Now()}
}

func (r *Runs) Submit(spec *RunSpecJSON) (*Run, error) {
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	run, err := r.store.CreateRun(spec.App, spec.Function, spec.Kind, string(specBytes))
	if err != nil {
		return nil, err
	}
	r.DispatchPending()
	return r.store.GetRun(run.ID)
}

// liveRunsCap bounds ledger/reconcile/janitor queries. Far beyond any
// personal fleet; hitting it means the accounting is no longer complete.
const liveRunsCap = 5000

// ledger sums persisted reservations of live runs, per machine.
func (r *Runs) ledger() map[string]scheduler.Reservation {
	out := map[string]scheduler.Reservation{}
	runs, err := r.store.ListRuns(liveRunsCap, []string{"assigned", "building", "running"})
	if err != nil {
		slog.Error("ledger query", "err", err)
		return out
	}
	if len(runs) == liveRunsCap {
		slog.Warn("live-run cap reached; reservation accounting may be incomplete")
	}
	for _, run := range runs {
		if run.MachineID == "" || run.ReservationJSON == "" {
			continue
		}
		var res reservationJSON
		if json.Unmarshal([]byte(run.ReservationJSON), &res) != nil {
			continue
		}
		entry := out[run.MachineID]
		entry.CPUs += res.CPUs
		entry.MemoryMiB += res.MemoryMiB
		if entry.DeviceMemMiB == nil {
			entry.DeviceMemMiB = map[int]uint64{}
		}
		for key, amount := range res.DeviceMemMiB {
			if idx, err := strconv.Atoi(key); err == nil {
				entry.DeviceMemMiB[idx] += amount
			}
		}
		entry.ActiveRuns++
		out[run.MachineID] = entry
	}
	return out
}

// snapshots renders the connected fleet for the scheduler.
func (r *Runs) snapshots() []*scheduler.Machine {
	ledger := r.ledger()
	var out []*scheduler.Machine
	for _, s := range r.fleet.All() {
		inv := s.Inventory
		m := &scheduler.Machine{
			ID:            s.Machine.ID,
			Name:          s.Machine.Name,
			Online:        true,
			OS:            inv.GetOs(),
			Arch:          inv.GetArch(),
			Executors:     inv.GetExecutors(),
			UnifiedMemory: inv.GetUnifiedMemory(),
			CPUCores:      float64(inv.GetCpuCores()),
			MemoryMiB:     inv.GetMemoryMib(),
			CachedImages:  s.CachedCopy(),
			Reserved:      ledger[s.Machine.ID],
		}
		if m.Reserved.DeviceMemMiB == nil {
			m.Reserved.DeviceMemMiB = map[int]uint64{}
		}
		for _, acc := range inv.GetAccelerators() {
			m.Devices = append(m.Devices, scheduler.AccelDevice{
				Kind:      acc.GetKind(),
				Index:     int(acc.GetIndex()),
				MemoryMiB: acc.GetMemoryMib(),
			})
		}
		out = append(out, m)
	}
	return out
}

func toSchedRequest(spec *RunSpecJSON) scheduler.Request {
	req := scheduler.Request{
		CPUs:        spec.CPUs,
		MemoryMiB:   spec.MemoryMiB,
		ImageTag:    spec.ImageTag,
		TargetNames: spec.TargetNames,
		Kind:        spec.Kind,
		NativeOK:    spec.NativeEnv != nil && spec.NativeEnv.Supported,
	}
	for _, a := range spec.Accelerators {
		req.AccelOptions = append(req.AccelOptions, scheduler.AccelRequest{
			Kind: a.Kind, MemoryMiB: a.MemoryMiB, Count: int(a.Count),
		})
	}
	return req
}

func applyDecisionToSnapshot(machines []*scheduler.Machine, d *scheduler.Decision) {
	for _, m := range machines {
		if m.ID != d.MachineID {
			continue
		}
		m.Reserved.CPUs += d.ReserveCPUs
		m.Reserved.MemoryMiB += d.ReserveMemoryMiB
		for idx, amount := range d.ReserveDeviceMiB {
			m.Reserved.DeviceMemMiB[idx] += amount
		}
		m.Reserved.ActiveRuns++
	}
}

// DispatchPending places every pending run it can. Called on submit, on
// agent attach, on run completion, and by the janitor.
func (r *Runs) DispatchPending() {
	r.mu.Lock()
	defer r.mu.Unlock()

	pending, err := r.store.ListRuns(liveRunsCap, []string{"pending"})
	if err != nil {
		slog.Error("list pending runs", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	machines := r.snapshots()

	for i := len(pending) - 1; i >= 0; i-- { // oldest first
		run := pending[i]
		var spec RunSpecJSON
		if err := json.Unmarshal([]byte(run.SpecJSON), &spec); err != nil {
			_, _ = r.store.TransitionRun(run.ID, nil, "error",
				"corrupt spec: "+err.Error(), "", "", 0)
			continue
		}
		decision, rejections := scheduler.Place(toSchedRequest(&spec), machines)
		if decision == nil {
			_ = r.store.SetRunDetail(run.ID, scheduler.FormatRejections(rejections))
			continue
		}
		session, ok := r.fleet.Get(decision.MachineID)
		if !ok {
			continue // vanished between snapshot and dispatch; next pass
		}

		// Persist FIRST, send second: a fast agent status update must find
		// the assignment recorded, and a failed send must be undoable.
		res := reservationJSON{
			CPUs:         decision.ReserveCPUs,
			MemoryMiB:    decision.ReserveMemoryMiB,
			AccelKind:    decision.AccelKind,
			DeviceMemMiB: map[string]uint64{},
		}
		for _, idx := range decision.DeviceIndices {
			res.DeviceIdx = append(res.DeviceIdx, int32(idx))
		}
		for idx, amount := range decision.ReserveDeviceMiB {
			res.DeviceMemMiB[strconv.Itoa(idx)] = amount
		}
		resBytes, _ := json.Marshal(res)
		assigned, err := r.store.SetRunAssigned(run.ID, decision.MachineID,
			"assigned to "+decision.MachineName, string(resBytes))
		if err != nil || !assigned {
			continue // Leave it alone because it was canceled or storage failed.
		}
		msg := &relayv1.ServerMessage{
			Msg: &relayv1.ServerMessage_Assign{
				Assign: &relayv1.RunAssignment{Spec: spec.toProto(run.ID, decision)},
			},
		}
		if !session.TrySend(msg) {
			_, _ = r.store.RevertRunToPending(run.ID, decision.MachineID)
			continue
		}
		applyDecisionToSnapshot(machines, decision)
		slog.Info("run assigned", "run", run.ID, "machine", decision.MachineName,
			"devices", decision.DeviceIndices)
	}
}

var protoStateNames = map[relayv1.RunState]string{
	relayv1.RunState_RUN_PENDING:   "pending",
	relayv1.RunState_RUN_ASSIGNED:  "assigned",
	relayv1.RunState_RUN_BUILDING:  "building",
	relayv1.RunState_RUN_RUNNING:   "running",
	relayv1.RunState_RUN_SUCCEEDED: "succeeded",
	relayv1.RunState_RUN_FAILED:    "failed",
	relayv1.RunState_RUN_ERROR:     "error",
	relayv1.RunState_RUN_CANCELED:  "canceled",
	relayv1.RunState_RUN_LOST:      "lost",
}

func IsTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "error", "canceled", "lost":
		return true
	}
	return false
}

func (r *Runs) HandleStatus(machineID string, upd *relayv1.RunStatusUpdate) {
	state, ok := protoStateNames[upd.GetState()]
	if !ok {
		return
	}
	run, err := r.store.GetRun(upd.GetRunId())
	if err != nil || run.MachineID != machineID {
		return // Ignore stale updates for unknown runs or runs owned by another machine.
	}
	// Conditional write: terminal states settle exactly once, and ownership
	// is rechecked in the WHERE clause. A machine that lost the run
	// between our read and this write cannot land a stale update.
	changed, err := r.store.TransitionRunOwned(upd.GetRunId(), nil, machineID,
		state, upd.GetDetail(), upd.GetResultSha256(), int(upd.GetExitCode()))
	if err != nil {
		slog.Error("update run status", "run", upd.GetRunId(), "err", err)
		return
	}
	if changed && upd.GetEndpoint() != "" {
		_ = r.store.SetRunEndpoint(upd.GetRunId(), upd.GetEndpoint())
	}
	if changed && IsTerminal(state) {
		r.logs.Close(upd.GetRunId())
		// The image is now warm on that machine (built or pulled on the way
		// to running), so feed the locality score.
		if state == "succeeded" || state == "failed" {
			var spec RunSpecJSON
			if json.Unmarshal([]byte(run.SpecJSON), &spec) == nil && spec.ImageTag != "" {
				if s, ok := r.fleet.Get(machineID); ok {
					s.MarkCached(spec.ImageTag)
				}
			}
		}
	}
}

// Reconcile aligns server state with what a (re)connecting agent reports.
// A run this machine owns but no longer runs died with the agent.
func (r *Runs) Reconcile(machineID string, activeRunIDs []string) {
	active := map[string]bool{}
	for _, id := range activeRunIDs {
		active[id] = true
	}
	for _, state := range []string{"assigned", "building", "running"} {
		runs, err := r.store.ListRuns(liveRunsCap, []string{state})
		if err != nil {
			continue
		}
		for _, run := range runs {
			if run.MachineID == machineID && !active[run.ID] {
				if changed, _ := r.store.TransitionRun(run.ID, []string{state},
					"lost", "agent restarted while the run was live",
					"", "", 0); changed {
					r.logs.Close(run.ID)
				}
			}
		}
	}
}

// OnAgentDetach requeues assignments the departed agent may never have
// received; anything it acknowledged (building/running) stays for the
// janitor/reconcile to settle.
func (r *Runs) OnAgentDetach(machineID string) {
	runs, err := r.store.ListRuns(liveRunsCap, []string{"assigned"})
	if err != nil {
		return
	}
	requeued := false
	for _, run := range runs {
		if run.MachineID != machineID {
			continue
		}
		if changed, _ := r.store.RevertRunToPending(run.ID, machineID); changed {
			requeued = true
			slog.Info("run requeued after agent detach", "run", run.ID)
		}
	}
	if requeued {
		r.DispatchPending()
	}
}

// Cancel asks the owning agent to kill the run (or settles it directly if
// it never left the queue).
func (r *Runs) Cancel(runID string) error {
	run, err := r.store.GetRun(runID)
	if err != nil {
		return err
	}
	if IsTerminal(run.State) {
		return fmt.Errorf("run %s already %s", runID, run.State)
	}
	if run.State == "pending" || run.MachineID == "" {
		changed, err := r.store.TransitionRun(runID, []string{"pending"},
			"canceled", "canceled before assignment", "", "", 0)
		if err != nil {
			return err
		}
		if changed {
			r.logs.Close(runID)
			return nil
		}
		// The cancellation raced with dispatch. Read again and use the agent path.
		if run, err = r.store.GetRun(runID); err != nil {
			return err
		}
		if IsTerminal(run.State) {
			return nil
		}
	}
	if s, ok := r.fleet.Get(run.MachineID); ok {
		delivered := s.TrySend(&relayv1.ServerMessage{
			Msg: &relayv1.ServerMessage_Cancel{Cancel: &relayv1.CancelRun{RunId: runID}},
		})
		if !delivered {
			return fmt.Errorf(
				"cancel for %s could not be delivered to %s right now. Retry the request",
				runID, run.MachineID)
		}
		return nil // agent confirms via a CANCELED status update
	}
	if changed, err := r.store.TransitionRun(runID,
		[]string{"assigned", "building", "running"}, "lost",
		"machine offline at cancel", "", "", 0); err == nil && changed {
		r.logs.Close(runID)
	}
	return nil
}

// Janitor marks runs LOST when their machine has been silent too long.
// A freshly (re)started server gives every agent a reconnect grace window
// first. The store's LastSeenAt predates the restart by construction, and
// declaring a healthy machine's runs lost seconds after boot would reject
// their real (possibly terminal) updates forever.
func (r *Runs) Janitor(offlineAfter time.Duration) {
	if time.Since(r.startedAt) < 2*offlineAfter {
		r.DispatchPending()
		return
	}
	for _, state := range []string{"assigned", "building", "running"} {
		runs, err := r.store.ListRuns(liveRunsCap, []string{state})
		if err != nil {
			continue
		}
		for _, run := range runs {
			s, connected := r.fleet.Get(run.MachineID)
			if connected && time.Since(s.LastHeartbeat()) < offlineAfter {
				continue
			}
			if !connected {
				m, err := r.store.MachineByID(run.MachineID)
				if err == nil && time.Since(m.LastSeenAt) < offlineAfter {
					continue // recently seen; give it time to reconnect
				}
			}
			if changed, _ := r.store.TransitionRun(run.ID, []string{state},
				"lost", "machine went offline during the run", "", "", 0); changed {
				r.logs.Close(run.ID)
				slog.Warn("run lost", "run", run.ID, "machine", run.MachineID)
			}
		}
	}
	r.DispatchPending()
}
