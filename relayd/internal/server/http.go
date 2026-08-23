package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// httpAPI is the SDK/CLI-facing surface: JSON over HTTP, log streaming as
// chunked text. This is deliberately not gRPC; see PLAN.md §3.
type httpAPI struct {
	store    *Store
	fleet    *Fleet
	runs     *Runs
	logs     *LogHub
	blobs    *BlobStore
	secrets  *SecretStore
	execHub  *ExecHub
	apiToken string
	dataDir  string
	grpcFP   string

	deployMu sync.Mutex // serializes service replacement
}

func NewHTTPHandler(store *Store, fleet *Fleet, runs *Runs, logs *LogHub, blobs *BlobStore, secrets *SecretStore, execHub *ExecHub, apiToken, dataDir, grpcFP string) http.Handler {
	api := &httpAPI{store: store, fleet: fleet, runs: runs, logs: logs,
		blobs: blobs, secrets: secrets, execHub: execHub, apiToken: apiToken,
		dataDir: dataDir, grpcFP: grpcFP}
	mux := http.NewServeMux()

	dash := dashboardHandler()
	mux.Handle("GET /{$}", dash)
	mux.Handle("GET /assets/", dash)
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "version": Version,
			"grpc_fingerprint": api.grpcFP, // public; pins the agent channel
		})
	})
	mux.HandleFunc("GET /install.sh", api.installScript)
	mux.HandleFunc("GET /v1/install/{asset}", api.installAsset)

	mux.HandleFunc("POST /v1/runs", api.auth(api.submitRun))
	mux.HandleFunc("GET /v1/runs", api.auth(api.listRuns))
	mux.HandleFunc("GET /v1/runs/{id}", api.auth(api.getRun))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", api.auth(api.cancelRun))
	mux.HandleFunc("GET /v1/runs/{id}/logs", api.auth(api.runLogs))
	mux.HandleFunc("GET /v1/machines", api.auth(api.listMachines))
	mux.HandleFunc("POST /v1/machines/{name}/exec", api.auth(api.execOnMachine))
	mux.HandleFunc("POST /v1/join-tokens", api.auth(api.createJoinToken))

	mux.HandleFunc("POST /v1/services", api.auth(api.deployService))
	mux.HandleFunc("GET /v1/services", api.auth(api.listServices))

	mux.HandleFunc("POST /v1/schedules", api.auth(api.upsertSchedule))
	mux.HandleFunc("GET /v1/schedules", api.auth(api.listSchedules))
	mux.HandleFunc("DELETE /v1/schedules/{app}/{function}", api.auth(api.deleteSchedule))

	mux.HandleFunc("PUT /v1/secrets/{name}", api.auth(api.setSecret))
	mux.HandleFunc("GET /v1/secrets", api.auth(api.listSecrets))
	mux.HandleFunc("DELETE /v1/secrets/{name}", api.auth(api.deleteSecret))

	mux.HandleFunc("HEAD /v1/blobs/{sha}", api.auth(api.headBlob))
	mux.HandleFunc("GET /v1/blobs/{sha}", api.auth(api.getBlob))
	mux.HandleFunc("PUT /v1/blobs/{sha}", api.auth(api.putBlob))

	return mux
}

func (a *httpAPI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.apiToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "invalid or missing API token. The token is printed " +
					"when `relayd server` starts and stored in its data dir " +
					"as api_token; the CLI reads ~/.relay/config.toml or RELAY_TOKEN.",
			})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]any{"error": fmt.Sprintf(format, args...)})
}

func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ------------------------------------------------------------ runs

type runJSON struct {
	ID        string     `json:"id"`
	App       string     `json:"app"`
	Function  string     `json:"function"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	Detail    string     `json:"detail"`
	Machine   string     `json:"machine"`
	Endpoint  string     `json:"endpoint,omitempty"`
	Resources string     `json:"resources,omitempty"` // chip text, e.g. "CUDA 24GB"
	Image     string     `json:"image,omitempty"`
	Events    []RunEvent `json:"events,omitempty"`
	ResultSha string     `json:"result_sha256,omitempty"`
	ExitCode  int        `json:"exit_code"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

func (a *httpAPI) runToJSON(r *Run) runJSON {
	machine := r.MachineID
	if m, err := a.store.MachineByID(r.MachineID); err == nil {
		machine = m.Name
	}
	var spec RunSpecJSON
	_ = json.Unmarshal([]byte(r.SpecJSON), &spec)
	return runJSON{
		ID: r.ID, App: r.App, Function: r.Function, Kind: r.Kind,
		State: r.State, Detail: r.Detail, Machine: machine,
		Endpoint: r.Endpoint, Resources: resourceChip(r.SpecJSON),
		Image:     spec.ImageTag,
		Events:    ParseRunEvents(r.EventsJSON),
		ResultSha: r.ResultSha,
		ExitCode:  r.ExitCode,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// resourceChip renders a run's accelerator ask as dashboard chip text.
func resourceChip(specJSON string) string {
	var spec RunSpecJSON
	if json.Unmarshal([]byte(specJSON), &spec) != nil {
		return ""
	}
	if len(spec.Accelerators) == 0 {
		return "CPU"
	}
	parts := make([]string, 0, len(spec.Accelerators))
	for _, a := range spec.Accelerators {
		s := strings.ToUpper(a.Kind)
		if a.MemoryMiB > 0 {
			s += " " + mibLabel(a.MemoryMiB)
		}
		if a.Count > 1 {
			s += fmt.Sprintf(" ×%d", a.Count)
		}
		if a.isExclusive() {
			s += " exclusive"
		} else {
			s += " shared"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

func mibLabel(mib uint64) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGB", mib/1024)
	}
	return fmt.Sprintf("%dMB", mib)
}

func (a *httpAPI) submitRun(w http.ResponseWriter, r *http.Request) {
	var spec RunSpecJSON
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "bad run spec: %v", err)
		return
	}
	if spec.Function == "" || spec.EntryFile == "" || spec.BundleSha == "" || spec.ArgsSha == "" {
		writeErr(w, http.StatusBadRequest,
			"run spec needs function, entry_file, bundle_sha256, args_sha256")
		return
	}
	if spec.Kind == "service" {
		writeErr(w, http.StatusBadRequest,
			"services deploy via POST /v1/services (relay deploy), not /v1/runs")
		return
	}
	if err := spec.validateAccelerators(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad accelerator spec: %v", err)
		return
	}
	for _, sha := range []string{spec.BundleSha, spec.ArgsSha} {
		if !a.blobs.Has(sha) {
			writeErr(w, http.StatusPreconditionFailed,
				"blob %s not uploaded yet (or invalid id)", shortSha(sha))
			return
		}
	}
	run, err := a.runs.Submit(&spec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "submit: %v", err)
		return
	}
	writeJSON(w, http.StatusCreated, a.runToJSON(run))
}

func (a *httpAPI) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.store.GetRun(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no run %s", r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, a.runToJSON(run))
}

func (a *httpAPI) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	var states []string
	if s := r.URL.Query().Get("state"); s != "" {
		states = strings.Split(s, ",")
	}
	runs, err := a.store.ListRuns(limit, states)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, a.runToJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (a *httpAPI) cancelRun(w http.ResponseWriter, r *http.Request) {
	if err := a.runs.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runLogs streams "text/plain; charset=utf-8" lines: full backlog, then live
// lines until the run finishes (or immediately when follow=0).
func (a *httpAPI) runLogs(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := a.store.GetRun(runID)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no run %s", runID)
		return
	}
	// A settled run gets its backlog and nothing else, even under --follow.
	// The run row is the only durable record of that, so a server restart
	// must not turn `relay logs <old-run>` into a hang.
	finished := err == nil && IsTerminal(run.State)
	follow := r.URL.Query().Get("follow") != "0"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	backlog, live, cancel := a.logs.Subscribe(runID, finished)
	if live != nil {
		// Re-read after subscribing. If the run settled in between, its
		// Close already ran and will never close the channel we just took,
		// so this follower would wait forever. Re-subscribing as finished
		// also re-reads the backlog, which now holds the final lines.
		if settled, gerr := a.store.GetRun(runID); gerr == nil && IsTerminal(settled.State) {
			cancel()
			backlog, live, cancel = a.logs.Subscribe(runID, true)
		}
	}
	defer cancel()
	for _, line := range backlog {
		fmt.Fprintln(w, line)
	}
	if flusher != nil {
		flusher.Flush()
	}
	if !follow || live == nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-live:
			if !ok {
				return
			}
			fmt.Fprintln(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// ------------------------------------------------------------ services

// deployService replaces any live generation of app+function with a new
// service run. The minted service key returns ONCE, in this response.
func (a *httpAPI) deployService(w http.ResponseWriter, r *http.Request) {
	var spec RunSpecJSON
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "bad service spec: %v", err)
		return
	}
	if spec.Kind != "service" || spec.ServicePort == 0 ||
		spec.Function == "" || spec.BundleSha == "" || spec.ArgsSha == "" {
		writeErr(w, http.StatusBadRequest,
			"service spec needs kind=service, service_port, function, and blobs")
		return
	}
	switch spec.Expose {
	case "", "private":
		spec.Expose = "private"
	case "public", "funnel":
	default:
		writeErr(w, http.StatusBadRequest,
			`expose must be "private", "funnel", or "public"`)
		return
	}
	if err := spec.validateAccelerators(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad accelerator spec: %v", err)
		return
	}
	for _, sha := range []string{spec.BundleSha, spec.ArgsSha} {
		if !a.blobs.Has(sha) {
			writeErr(w, http.StatusPreconditionFailed,
				"blob %s not uploaded yet (or invalid id)", shortSha(sha))
			return
		}
	}
	// Public ≠ open: every exposed service gets a capability key unless
	// auth was explicitly opted out (PLAN §7 "public ≠ open"; private mode
	// is key-gated by default too).
	serviceKey := ""
	if !spec.AuthNone {
		serviceKey = NewSecret()
	}
	spec.ServiceKey = serviceKey

	// Serialize deploys and submit the NEW generation before touching the
	// old one because a failed submit must never cause an outage. (Random ports
	// let generations coexist briefly; full health-gated switchover is a
	// documented follow-up.)
	a.deployMu.Lock()
	defer a.deployMu.Unlock()
	previous, _ := a.store.ActiveServiceRuns(spec.App, spec.Function)
	run, err := a.runs.Submit(&spec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			"deploy: %v (previous generation left untouched)", err)
		return
	}
	// Readiness-gated switchover: the old generation is canceled only once
	// the new one is RUNNING (random ports let them coexist). If the new
	// one dies, the old keeps serving. Escape hatch: if the new stays
	// pending >2min (the old may hold its capacity), hand over anyway.
	oldIDs := make([]string, 0, len(previous))
	for _, old := range previous {
		oldIDs = append(oldIDs, old.ID)
	}
	go a.superviseReplacement(run.ID, oldIDs)

	writeJSON(w, http.StatusCreated, map[string]any{
		"run":         a.runToJSON(run),
		"service_key": serviceKey, // shown once; not retrievable later
		"replaced":    len(previous),
	})
}

func (a *httpAPI) superviseReplacement(newID string, oldIDs []string) {
	if len(oldIDs) == 0 {
		return
	}
	cancelOld := func(reason string) {
		for _, id := range oldIDs {
			if err := a.runs.Cancel(id); err != nil {
				slog.Warn("replacing service: cancel failed",
					"run", id, "err", err)
			}
		}
		slog.Info("service generation replaced", "new", newID, "reason", reason)
	}
	deadline := time.Now().Add(15 * time.Minute)
	pendingSince := time.Now()
	for time.Now().Before(deadline) {
		run, err := a.store.GetRun(newID)
		if err != nil {
			return
		}
		switch {
		case run.State == "running":
			cancelOld("new generation healthy")
			return
		case IsTerminal(run.State):
			slog.Warn("replacement generation died; keeping the old one",
				"new", newID, "state", run.State, "detail", run.Detail)
			return
		case run.State == "pending" && time.Since(pendingSince) > 2*time.Minute:
			cancelOld("capacity handoff (new generation waiting on resources)")
			return
		}
		time.Sleep(2 * time.Second)
	}
	slog.Warn("replacement supervision timed out; old generation kept", "new", newID)
}

func (a *httpAPI) listServices(w http.ResponseWriter, _ *http.Request) {
	runs, err := a.store.ListRuns(200, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := []runJSON{}
	for _, r := range runs {
		if r.Kind == "service" && !IsTerminal(r.State) {
			out = append(out, a.runToJSON(r))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// ------------------------------------------------------------ machines

func (a *httpAPI) listMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := a.store.ListMachines()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	// Capacity is intentionally derived from the same reservation ledger the
	// scheduler uses for admission. It answers "what can Relay schedule now?"
	// without presenting best-effort host/GPU telemetry as a hard guarantee.
	ledger := a.runs.ledger()
	type reservationJSON struct {
		CPUCores             float64        `json:"cpu_cores"`
		MemoryMiB            uint64         `json:"memory_mib"`
		AcceleratorMemoryMiB map[int]uint64 `json:"accelerator_memory_mib"`
		ActiveRuns           int            `json:"active_runs"`
	}
	type acceleratorUsageJSON struct {
		Index                int32   `json:"index"`
		MemoryUsedMiB        uint64  `json:"memory_used_mib"`
		Utilization          float64 `json:"utilization"`
		MemoryUsageAvailable bool    `json:"memory_usage_available"`
		UtilizationAvailable bool    `json:"utilization_available"`
	}
	type usageJSON struct {
		SampledAt            string                 `json:"sampled_at"`
		CPUUsedCores         float64                `json:"cpu_used_cores"`
		MemoryUsedMiB        uint64                 `json:"memory_used_mib"`
		DiskFreeMiB          uint64                 `json:"disk_free_mib"`
		DiskTotalMiB         uint64                 `json:"disk_total_mib"`
		CPUUsageAvailable    bool                   `json:"cpu_usage_available"`
		MemoryUsageAvailable bool                   `json:"memory_usage_available"`
		DiskUsageAvailable   bool                   `json:"disk_usage_available"`
		Accelerators         []acceleratorUsageJSON `json:"accelerators"`
	}
	type machineJSON struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Online       bool            `json:"online"`
		OS           string          `json:"os"`
		Arch         string          `json:"arch"`
		CPUCores     int             `json:"cpu_cores"`
		MemoryMiB    int64           `json:"memory_mib"`
		Unified      bool            `json:"unified_memory"`
		Executors    json.RawMessage `json:"executors"`
		Accelerators json.RawMessage `json:"accelerators"`
		Reserved     reservationJSON `json:"reserved"`
		Usage        *usageJSON      `json:"usage,omitempty"`
		LastSeen     string          `json:"last_seen"`
	}
	wantTelemetry := r.URL.Query().Get("telemetry") == "1"
	out := make([]machineJSON, 0, len(machines))
	for _, m := range machines {
		session, online := a.fleet.Get(m.ID)
		if online && wantTelemetry {
			// A short renewable lease ensures collection stops shortly after the
			// Machines page closes. The request and samples stay in memory only.
			session.TrySend(&relayv1.ServerMessage{Msg: &relayv1.ServerMessage_Telemetry{
				Telemetry: &relayv1.TelemetryRequest{DurationS: 8, IntervalS: 3},
			}})
		}
		reserved := ledger[m.ID]
		if reserved.DeviceMemMiB == nil {
			reserved.DeviceMemMiB = map[int]uint64{}
		}
		rawOr := func(s string) json.RawMessage {
			if s == "" || !json.Valid([]byte(s)) {
				return json.RawMessage("[]")
			}
			return json.RawMessage(s)
		}
		var usage *usageJSON
		if online {
			if hb, receivedAt := session.Usage(); hb != nil && time.Since(receivedAt) < 12*time.Second {
				usage = &usageJSON{
					SampledAt:            receivedAt.UTC().Format(time.RFC3339),
					CPUUsedCores:         hb.GetCpuUsedCores(),
					MemoryUsedMiB:        hb.GetMemoryUsedMib(),
					DiskFreeMiB:          hb.GetDiskFreeMib(),
					DiskTotalMiB:         hb.GetDiskTotalMib(),
					CPUUsageAvailable:    hb.GetCpuUsageAvailable(),
					MemoryUsageAvailable: hb.GetMemoryUsageAvailable(),
					DiskUsageAvailable:   hb.GetDiskUsageAvailable(),
				}
				for _, accelerator := range hb.GetAccelerators() {
					usage.Accelerators = append(usage.Accelerators, acceleratorUsageJSON{
						Index: accelerator.GetIndex(), MemoryUsedMiB: accelerator.GetMemoryUsedMib(),
						Utilization:          accelerator.GetUtilization(),
						MemoryUsageAvailable: accelerator.GetMemoryUsageAvailable(),
						UtilizationAvailable: accelerator.GetUtilizationAvailable(),
					})
				}
			}
		}
		lastSeen := m.LastSeenAt
		if online {
			lastSeen = session.LastHeartbeat()
		}
		out = append(out, machineJSON{
			ID: m.ID, Name: m.Name, Online: online, OS: m.OS, Arch: m.Arch,
			CPUCores: m.CPUCores, MemoryMiB: m.MemoryMiB, Unified: m.UnifiedMem,
			Executors:    rawOr(m.Executors),
			Accelerators: rawOr(m.Accelerators),
			Reserved: reservationJSON{
				CPUCores:             reserved.CPUs,
				MemoryMiB:            reserved.MemoryMiB,
				AcceleratorMemoryMiB: reserved.DeviceMemMiB,
				ActiveRuns:           reserved.ActiveRuns,
			},
			Usage:    usage,
			LastSeen: lastSeen.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

// execOnMachine runs one bounded host command on a fleet machine and
// streams combined output back. The final line is "\x00exit <code>".
func (a *httpAPI) execOnMachine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Argv     []string `json:"argv"`
		TimeoutS uint32   `json:"timeout_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, `body must be {"argv": ["cmd", ...]}`)
		return
	}
	if body.TimeoutS == 0 || body.TimeoutS > 300 {
		body.TimeoutS = 120
	}
	name := r.PathValue("name")
	machines, err := a.store.ListMachines()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var machineID string
	for _, m := range machines {
		if m.Name == name {
			machineID = m.ID
		}
	}
	if machineID == "" {
		writeErr(w, http.StatusNotFound, "no machine %q. Check `relay fleet`", name)
		return
	}
	session, ok := a.fleet.Get(machineID)
	if !ok {
		writeErr(w, http.StatusConflict, "machine %q is offline", name)
		return
	}

	execID := newID("x")
	ch := a.execHub.Register(execID)
	defer a.execHub.Unregister(execID)
	if !session.TrySend(&relayv1.ServerMessage{Msg: &relayv1.ServerMessage_Exec{
		Exec: &relayv1.ExecRequest{ExecId: execID, Argv: body.Argv,
			TimeoutS: body.TimeoutS},
	}}) {
		writeErr(w, http.StatusConflict, "machine %q is busy. Retry the request", name)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)
	deadline := time.After(time.Duration(body.TimeoutS+15) * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			fmt.Fprintf(w, "\x00exit -1 (server-side wait timed out)\n")
			return
		case out := <-ch:
			if len(out.GetChunk()) > 0 {
				_, _ = w.Write(out.GetChunk())
				if flusher != nil {
					flusher.Flush()
				}
			}
			if out.GetDone() {
				if out.GetError() != "" {
					fmt.Fprintf(w, "\x00exit -1 (%s)\n", out.GetError())
				} else {
					fmt.Fprintf(w, "\x00exit %d\n", out.GetExitCode())
				}
				return
			}
		}
	}
}

func (a *httpAPI) createJoinToken(w http.ResponseWriter, _ *http.Request) {
	token, err := a.store.CreateJoinToken(30 * time.Minute)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_in": "30m",
	})
}

// ------------------------------------------------------------ schedules

type scheduleRequest struct {
	Cron string      `json:"cron"`
	Spec RunSpecJSON `json:"spec"`
}

func (a *httpAPI) upsertSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad schedule: %v", err)
		return
	}
	if _, err := ParseCron(req.Cron); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Spec.Function == "" || req.Spec.BundleSha == "" || req.Spec.Kind == "service" {
		writeErr(w, http.StatusBadRequest,
			"schedule spec needs a job function with uploaded blobs")
		return
	}
	for _, sha := range []string{req.Spec.BundleSha, req.Spec.ArgsSha} {
		if !a.blobs.Has(sha) {
			writeErr(w, http.StatusPreconditionFailed,
				"blob %s not uploaded. A schedule must reference real blobs "+
					"or every firing would fail", shortSha(sha))
			return
		}
	}
	specBytes, _ := json.Marshal(req.Spec)
	if _, err := a.store.UpsertSchedule(req.Spec.App, req.Spec.Function,
		req.Cron, string(specBytes)); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (a *httpAPI) listSchedules(w http.ResponseWriter, _ *http.Request) {
	schedules, err := a.store.ListSchedules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	type schedJSON struct {
		App      string `json:"app"`
		Function string `json:"function"`
		Cron     string `json:"cron"`
		LastRun  string `json:"last_run,omitempty"`
	}
	out := []schedJSON{}
	for _, s := range schedules {
		sj := schedJSON{App: s.App, Function: s.Function, Cron: s.Cron}
		if s.LastRunAt.Unix() > 0 {
			sj.LastRun = s.LastRunAt.UTC().Format(time.RFC3339)
		}
		out = append(out, sj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (a *httpAPI) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteSchedule(r.PathValue("app"), r.PathValue("function")); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------ secrets
// Write-only over HTTP: values go in, only names ever come out.

func (a *httpAPI) setSecret(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Value == "" {
		writeErr(w, http.StatusBadRequest, `body must be {"value": "..."}`)
		return
	}
	if err := a.secrets.Set(r.PathValue("name"), body.Value); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *httpAPI) listSecrets(w http.ResponseWriter, _ *http.Request) {
	names, err := a.secrets.Names()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

func (a *httpAPI) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := a.secrets.Delete(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------ blobs

func (a *httpAPI) headBlob(w http.ResponseWriter, r *http.Request) {
	if a.blobs.Has(r.PathValue("sha")) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (a *httpAPI) getBlob(w http.ResponseWriter, r *http.Request) {
	rc, err := a.blobs.Open(r.PathValue("sha"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}

func (a *httpAPI) putBlob(w http.ResponseWriter, r *http.Request) {
	expected := r.PathValue("sha")
	// The SDK already uses HEAD before PUT, but close the race server-side too:
	// an existing content-addressed blob never needs to be rewritten.
	if size, err := a.blobs.Size(expected); err == nil {
		writeJSON(w, http.StatusCreated, map[string]any{
			"sha256": expected, "size": size,
		})
		return
	}
	sha, size, err := a.blobs.Write(r.Body, expected)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": sha, "size": size})
}

// ------------------------------------------------------------ install

func (a *httpAPI) installScript(w http.ResponseWriter, _ *http.Request) {
	p := filepath.Join(a.dataDir, "binaries", "install.sh")
	data, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusNotFound,
			"no install script staged. Run `make release` in the relay repo "+
				"and copy relayd/bin/* plus installer/install.sh into %s",
			filepath.Join(a.dataDir, "binaries"))
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = w.Write(data)
}

func (a *httpAPI) installAsset(w http.ResponseWriter, r *http.Request) {
	asset := r.PathValue("asset")
	if strings.Contains(asset, "/") || strings.Contains(asset, "..") {
		writeErr(w, http.StatusBadRequest, "bad asset name")
		return
	}
	http.ServeFile(w, r, filepath.Join(a.dataDir, "binaries", asset))
}
