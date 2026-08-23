package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/matteomarolt/relay/relayd/internal/pin"
	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

type Config struct {
	ServerAddr        string // host:port of the gRPC listener
	JoinToken         string // set on first join only
	ServerFingerprint string // cert pin from the join string (#fp suffix)
	Name              string // requested machine name ("" = hostname)
	StateDir          string
	Version           string
	DockerSock        string
	JoinOnly          bool // register, persist credentials, exit
}

type credentials struct {
	MachineID     string `json:"machine_id"`
	AgentToken    string `json:"agent_token"`
	ServerAddr    string `json:"server_addr"`
	Name          string `json:"machine_name"`
	ServerCertPEM string `json:"server_cert_pem,omitempty"` // pinned after join
}

// Agent owns the outbound connection and every local run.
type Agent struct {
	cfg    Config
	creds  credentials
	docker *dockerClient
	exec   *DockerExecutor
	native *NativeExecutor
	usage  *usageCollector

	mu     sync.Mutex
	active map[string]context.CancelFunc
	// statusCh is RELIABLE: sends block rather than drop because a lost terminal
	// status would strand a run forever. logCh is lossy by design.
	statusCh chan *relayv1.AgentMessage
	logCh    chan *relayv1.AgentMessage
	client   relayv1.AgentServiceClient

	seenCertDER []byte // guarded by mu; captured during TLS verification

	// pendingStatus holds statuses whose Send failed. They retry on the
	// next session. unreported counts queued-but-undelivered statuses per
	// run, so those runs stay in ActiveRunIds and a reconnect's reconcile
	// cannot mark a finished-but-unreported run lost.
	pendingStatus []*relayv1.AgentMessage // guarded by mu
	unreported    map[string]int          // guarded by mu; runID → queued statuses
}

func credsPath(stateDir string) string { return filepath.Join(stateDir, "credentials.json") }

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	a := &Agent{
		cfg:        cfg,
		docker:     newDockerClient(cfg.DockerSock),
		usage:      newUsageCollector(),
		active:     map[string]context.CancelFunc{},
		statusCh:   make(chan *relayv1.AgentMessage, 1024),
		logCh:      make(chan *relayv1.AgentMessage, 1024),
		unreported: map[string]int{},
	}
	a.exec = NewDockerExecutor(a.docker, cfg.StateDir)
	a.native = NewNativeExecutor(cfg.StateDir)
	a.loadCreds() // pre-load so TLS pinning knows a stored cert

	conn, err := grpc.NewClient(cfg.ServerAddr,
		grpc.WithTransportCredentials(a.tlsCreds()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(8<<20), grpc.MaxCallSendMsgSize(8<<20)),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	a.client = relayv1.NewAgentServiceClient(conn)

	// Clean up containers orphaned by a previous agent death before any
	// new work arrives (nothing is active yet, so everything labeled ours
	// from a prior life goes).
	a.exec.SweepOrphans(ctx, func(runID string) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, active := a.active[runID]
		return active
	})
	if err := cleanupStaleRunDirs(cfg.StateDir); err != nil {
		slog.Warn("could not clean stale run scratch", "err", err)
	}

	if err := a.join(ctx); err != nil {
		return err
	}
	if cfg.JoinOnly {
		slog.Info("joined; credentials stored", "machine", a.creds.Name,
			"state_dir", cfg.StateDir)
		return nil
	}
	slog.Info("agent ready", "machine", a.creds.Name, "server", cfg.ServerAddr)

	// The reconnect loop uses exponential backoff, jitter, and a cap, which T3's
	// connector supervisor lacked.
	backoff := 500 * time.Millisecond
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		sleep := backoff + time.Duration(rand.Int64N(int64(backoff/2)))
		slog.Warn("session ended; reconnecting", "err", err, "in", sleep.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	return nil
}

func (a *Agent) loadCreds() {
	data, err := os.ReadFile(credsPath(a.cfg.StateDir))
	if err == nil {
		_ = json.Unmarshal(data, &a.creds)
	}
}

func (a *Agent) saveCreds() error {
	blob, _ := json.MarshalIndent(a.creds, "", "  ")
	return os.WriteFile(credsPath(a.cfg.StateDir), blob, 0o600)
}

// tlsCreds builds the pinned transport:
//   - stored cert (post-join): only that exact certificate is accepted
//   - join fingerprint: trust-on-first-use against the #fp in the join string
//   - neither: TOFU with a loud warning (manual --server without a join)
//
// Hostname verification is irrelevant by design because the pin is the identity.
func (a *Agent) tlsCreds() grpccreds.TransportCredentials {
	cfg := &tls.Config{
		InsecureSkipVerify: true, // manual pin verification below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificate")
			}
			der := rawCerts[0]
			if a.creds.ServerCertPEM != "" {
				block, _ := pem.Decode([]byte(a.creds.ServerCertPEM))
				if block == nil || !bytes.Equal(block.Bytes, der) {
					return errors.New(
						"server certificate changed. This may indicate a MITM, or the " +
							"server regenerated its TLS cert. If the latter, " +
							"remove server_cert_pem from the agent " +
							"credentials.json and re-verify the fingerprint.")
				}
				return nil
			}
			fp := pin.Fingerprint(der)
			if want := a.cfg.ServerFingerprint; want != "" {
				if fp != want {
					return fmt.Errorf(
						"server fingerprint %s does not match join string %s",
						fp, want)
				}
			} else {
				slog.Warn("pinning server certificate on first use "+
					"(no fingerprint given)", "fingerprint", fp)
			}
			a.mu.Lock()
			a.seenCertDER = der
			a.mu.Unlock()
			return nil
		},
	}
	return grpccreds.NewTLS(cfg)
}

// persistSeenCert pins the server certificate after a successful RPC.
func (a *Agent) persistSeenCert() {
	a.mu.Lock()
	der := a.seenCertDER
	a.mu.Unlock()
	if der == nil || a.creds.ServerCertPEM != "" {
		return
	}
	a.creds.ServerCertPEM = string(pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err := a.saveCreds(); err != nil {
		slog.Warn("could not persist pinned certificate", "err", err)
	}
}

func (a *Agent) join(ctx context.Context) error {
	if a.creds.AgentToken != "" {
		return nil
	}
	if a.cfg.JoinToken == "" {
		return errors.New(
			"this machine has not joined a fleet. Run with --join <token> " +
				"(mint one with `relay connect` on your dev machine)")
	}
	inv := DetectInventory(ctx, a.docker, a.cfg.Version)
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := a.client.Register(jctx, &relayv1.RegisterRequest{
		JoinToken:     a.cfg.JoinToken,
		RequestedName: a.cfg.Name,
		Inventory:     inv,
	})
	if err != nil {
		return fmt.Errorf("join failed: %w", err)
	}
	a.creds = credentials{
		MachineID:  resp.GetMachineId(),
		AgentToken: resp.GetAgentToken(),
		ServerAddr: a.cfg.ServerAddr,
		Name:       resp.GetMachineName(),
	}
	// Pin the certificate IN the same write as the credentials: a bearer
	// token must never exist on disk without its pin (a crash between two
	// writes would downgrade every later connect to TOFU).
	a.mu.Lock()
	if der := a.seenCertDER; der != nil {
		a.creds.ServerCertPEM = string(pem.EncodeToMemory(
			&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	a.mu.Unlock()
	if err := a.saveCreds(); err != nil {
		return err
	}
	slog.Info("joined fleet", "machine", a.creds.Name)
	return nil
}

func (a *Agent) authCtx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+a.creds.AgentToken)
}

func (a *Agent) session(ctx context.Context) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := a.client.Session(a.authCtx(sctx))
	if err != nil {
		return err
	}

	inv := DetectInventory(sctx, a.docker, a.cfg.Version)
	cached, _ := a.docker.ListImageTags(sctx, "relay-img:")
	if err := stream.Send(&relayv1.AgentMessage{Msg: &relayv1.AgentMessage_Hello{
		Hello: &relayv1.AgentHello{
			MachineId:       a.creds.MachineID,
			Inventory:       inv,
			CachedImageTags: cached,
			ActiveRunIds:    a.activeRunIDs(),
		},
	}}); err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("server did not open with Hello")
	}
	heartbeatEvery := time.Duration(hello.GetHeartbeatIntervalS()) * time.Second
	if heartbeatEvery <= 0 {
		heartbeatEvery = 10 * time.Second
	}
	a.persistSeenCert()
	slog.Info("connected", "server_version", hello.GetServerVersion())

	type telemetryLease struct {
		duration time.Duration
		interval time.Duration
	}
	telemetryConfig := make(chan telemetryLease, 1)
	telemetryRequests := make(chan struct{}, 1)
	telemetrySamples := make(chan *relayv1.Heartbeat, 1)
	go func() {
		for {
			select {
			case <-sctx.Done():
				return
			case <-telemetryRequests:
				sample := a.usage.Sample(sctx)
				select {
				case telemetrySamples <- sample:
				default: // keep at most one fresh sample queued for the writer
				}
			}
		}
	}()

	// Writer: heartbeats + executor events. Status messages take priority
	// over logs; a failed status Send is requeued, never lost.
	writerDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		telemetryTimer := time.NewTimer(time.Hour)
		if !telemetryTimer.Stop() {
			<-telemetryTimer.C
		}
		defer telemetryTimer.Stop()
		var telemetryTimerC <-chan time.Time
		var telemetryUntil time.Time
		telemetryEvery := 3 * time.Second
		resetTelemetryTimer := func(after time.Duration) {
			if !telemetryTimer.Stop() {
				select {
				case <-telemetryTimer.C:
				default:
				}
			}
			telemetryTimer.Reset(after)
			telemetryTimerC = telemetryTimer.C
		}
		requestTelemetrySample := func() {
			select {
			case telemetryRequests <- struct{}{}:
			default:
			}
		}
		var sendLogs func(*relayv1.AgentMessage) error
		sendStatus := func(msg *relayv1.AgentMessage) error {
			// Executors join their log follower before reporting a terminal state.
			// Flush those already-queued lines first so clients never observe a
			// finished run whose final log batch is still behind the status.
			if st := msg.GetRunStatus(); st != nil {
				switch st.GetState() {
				case relayv1.RunState_RUN_SUCCEEDED, relayv1.RunState_RUN_FAILED,
					relayv1.RunState_RUN_ERROR, relayv1.RunState_RUN_CANCELED,
					relayv1.RunState_RUN_LOST:
					select {
					case pendingLogs := <-a.logCh:
						if err := sendLogs(pendingLogs); err != nil {
							a.mu.Lock()
							a.pendingStatus = append(a.pendingStatus, msg)
							a.mu.Unlock()
							return err
						}
					default:
					}
				}
			}
			if err := stream.Send(msg); err != nil {
				a.mu.Lock()
				a.pendingStatus = append(a.pendingStatus, msg)
				a.mu.Unlock()
				return err
			}
			if st := msg.GetRunStatus(); st != nil {
				a.mu.Lock()
				if a.unreported[st.GetRunId()] <= 1 {
					delete(a.unreported, st.GetRunId())
				} else {
					a.unreported[st.GetRunId()]--
				}
				a.mu.Unlock()
			}
			return nil
		}
		// Coalesce log lines that are already queued into one message per run.
		// This adds no timer or steady-state work, while turning output bursts
		// into far fewer network and filesystem writes on the server.
		sendLogs = func(first *relayv1.AgentMessage) error {
			const maxLines = 128
			batches := map[string][]*relayv1.LogLine{}
			var runIDs []string
			add := func(msg *relayv1.AgentMessage) {
				logs := msg.GetLogs()
				if logs == nil || len(logs.GetLines()) == 0 {
					return
				}
				runID := logs.GetRunId()
				if _, exists := batches[runID]; !exists {
					runIDs = append(runIDs, runID)
				}
				batches[runID] = append(batches[runID], logs.GetLines()...)
			}
			add(first)
			count := len(first.GetLogs().GetLines())
			for count < maxLines {
				select {
				case msg := <-a.logCh:
					if logs := msg.GetLogs(); logs != nil {
						count += len(logs.GetLines())
					}
					add(msg)
				default:
					count = maxLines
				}
			}
			for _, runID := range runIDs {
				if err := stream.Send(&relayv1.AgentMessage{
					Msg: &relayv1.AgentMessage_Logs{Logs: &relayv1.LogBatch{
						RunId: runID, Lines: batches[runID],
					}},
				}); err != nil {
					return err
				}
			}
			return nil
		}
		// Retries from the previous session go first.
		a.mu.Lock()
		retries := a.pendingStatus
		a.pendingStatus = nil
		a.mu.Unlock()
		for _, msg := range retries {
			if err := sendStatus(msg); err != nil {
				writerDone <- err
				return
			}
		}
		for {
			// Drain any pending status first.
			select {
			case msg := <-a.statusCh:
				if err := sendStatus(msg); err != nil {
					writerDone <- err
					return
				}
				continue
			default:
			}
			select {
			case <-sctx.Done():
				writerDone <- nil
				return
			case <-ticker.C:
				if err := stream.Send(&relayv1.AgentMessage{
					Msg: &relayv1.AgentMessage_Heartbeat{Heartbeat: &relayv1.Heartbeat{}},
				}); err != nil {
					writerDone <- err
					return
				}
			case lease := <-telemetryConfig:
				wasActive := telemetryTimerC != nil && time.Now().Before(telemetryUntil)
				telemetryUntil = time.Now().Add(lease.duration)
				telemetryEvery = lease.interval
				if !wasActive {
					a.usage.ResetCPU()
					requestTelemetrySample()
					resetTelemetryTimer(telemetryEvery)
				}
			case <-telemetryTimerC:
				if !time.Now().Before(telemetryUntil) {
					telemetryTimerC = nil
					continue
				}
				requestTelemetrySample()
				resetTelemetryTimer(telemetryEvery)
			case sample := <-telemetrySamples:
				if err := stream.Send(&relayv1.AgentMessage{
					Msg: &relayv1.AgentMessage_Heartbeat{Heartbeat: sample},
				}); err != nil {
					writerDone <- err
					return
				}
			case msg := <-a.statusCh:
				if err := sendStatus(msg); err != nil {
					writerDone <- err
					return
				}
			case msg := <-a.logCh:
				if err := sendLogs(msg); err != nil {
					writerDone <- err
					return
				}
			}
		}
	}()

	// A dead writer must also unblock the reader's Recv.
	go func() {
		select {
		case <-sctx.Done():
		case err := <-writerDone:
			writerDone <- err // put it back for the main path
			cancel()
		}
	}()

	// Reader: assignments and cancels.
	for {
		msg, err := stream.Recv()
		if err != nil {
			cancel()
			select {
			case werr := <-writerDone:
				if werr != nil {
					return werr
				}
			case <-time.After(5 * time.Second):
			}
			return err
		}
		switch payload := msg.Msg.(type) {
		case *relayv1.ServerMessage_Assign:
			a.startRun(ctx, payload.Assign.GetSpec())
		case *relayv1.ServerMessage_Cancel:
			a.cancelRun(payload.Cancel.GetRunId())
		case *relayv1.ServerMessage_Exec:
			go a.runExec(ctx, payload.Exec)
		case *relayv1.ServerMessage_Telemetry:
			duration := time.Duration(payload.Telemetry.GetDurationS()) * time.Second
			interval := time.Duration(payload.Telemetry.GetIntervalS()) * time.Second
			if duration < 3*time.Second || duration > 30*time.Second {
				duration = 8 * time.Second
			}
			if interval < 2*time.Second || interval > 10*time.Second {
				interval = 3 * time.Second
			}
			lease := telemetryLease{duration: duration, interval: interval}
			select {
			case telemetryConfig <- lease:
			default:
				// Coalesce repeated dashboard renewals without blocking assignments.
				select {
				case <-telemetryConfig:
				default:
				}
				select {
				case telemetryConfig <- lease:
				default:
				}
			}
		}
	}
}

// runExec executes one bounded host command (relay shell) and streams
// combined output back through the reliable status channel.
func (a *Agent) runExec(ctx context.Context, req *relayv1.ExecRequest) {
	send := func(out *relayv1.ExecOutput) {
		out.ExecId = req.GetExecId()
		a.statusCh <- &relayv1.AgentMessage{
			Msg: &relayv1.AgentMessage_ExecOutput{ExecOutput: out}}
	}
	timeout := time.Duration(req.GetTimeoutS()) * time.Second
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = 2 * time.Minute
	}
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := req.GetArgv()
	slog.Info("exec", "argv", argv)
	cmd := exec.CommandContext(ectx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		send(&relayv1.ExecOutput{Done: true, Error: err.Error()})
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		send(&relayv1.ExecOutput{Done: true, Error: err.Error()})
		return
	}
	buf := make([]byte, 16*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			send(&relayv1.ExecOutput{Chunk: chunk})
		}
		if readErr != nil {
			break
		}
	}
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			send(&relayv1.ExecOutput{Done: true, Error: err.Error()})
			return
		}
	}
	if ectx.Err() != nil {
		send(&relayv1.ExecOutput{Done: true, Error: "command timed out"})
		return
	}
	send(&relayv1.ExecOutput{Done: true, ExitCode: int32(exitCode)})
}

func (a *Agent) activeRunIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := map[string]bool{}
	ids := make([]string, 0, len(a.active))
	for id := range a.active {
		ids = append(ids, id)
		seen[id] = true
	}
	// Runs with undelivered statuses are still "ours": reconcile must not
	// mark them lost before the queued (possibly terminal) status lands.
	for id := range a.unreported {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids
}

// startRun launches the executor under a per-run context so cancel works.
// Run contexts descend from the agent root, not the session: a dropped
// connection must not kill live jobs.
func (a *Agent) startRun(root context.Context, spec *relayv1.RunSpec) {
	runID := spec.GetRunId()
	a.mu.Lock()
	if _, exists := a.active[runID]; exists {
		a.mu.Unlock()
		return
	}
	rctx, cancel := context.WithCancel(root)
	a.active[runID] = cancel
	a.mu.Unlock()

	slog.Info("run starting", "run", runID,
		"function", spec.GetFunction(), "kind", spec.GetKind())
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.active, runID)
			a.mu.Unlock()
			cancel()
		}()
		switch {
		case spec.GetKind() == "service":
			a.exec.RunService(rctx, spec, a)
		case spec.GetAcceleratorKind() == "mps":
			a.native.Run(rctx, spec, a) // Metal is unreachable from containers
		case a.docker.Ping(rctx) != nil && spec.GetNativeEnv().GetSupported():
			// Docker-less machine, pip-only image: run natively (mirrors
			// the scheduler's no-accelerator gating).
			a.native.Run(rctx, spec, a)
		default:
			a.exec.Run(rctx, spec, a)
		}
	}()
}

func (a *Agent) cancelRun(runID string) {
	a.mu.Lock()
	cancel, ok := a.active[runID]
	a.mu.Unlock()
	if ok {
		slog.Info("run canceled by server", "run", runID)
		cancel()
	}
}

// ------------------------------------------------------------ Events

// Status blocks until queued because terminal statuses must never be dropped
// (deliberate backpressure: a 1024-deep queue full for one machine means
// something is very wrong anyway). The queue and the retry buffer survive
// session reconnects, so updates queued during an outage flush on connect.
func (a *Agent) Status(runID string, state relayv1.RunState, detail string, exitCode int, resultSha string) {
	a.enqueueStatus(&relayv1.RunStatusUpdate{
		RunId: runID, State: state, Detail: detail,
		ExitCode: int32(exitCode), ResultSha256: resultSha,
	})
}

func (a *Agent) StatusEndpoint(runID string, state relayv1.RunState, detail, endpoint string) {
	a.enqueueStatus(&relayv1.RunStatusUpdate{
		RunId: runID, State: state, Detail: detail, Endpoint: endpoint,
	})
}

func (a *Agent) enqueueStatus(st *relayv1.RunStatusUpdate) {
	a.mu.Lock()
	a.unreported[st.GetRunId()]++
	a.mu.Unlock()
	a.statusCh <- &relayv1.AgentMessage{
		Msg: &relayv1.AgentMessage_RunStatus{RunStatus: st}}
}

// Log drops on backpressure because losing log lines is better than stalling runs.
func (a *Agent) Log(runID, line string, stderr bool) {
	msg := &relayv1.AgentMessage{Msg: &relayv1.AgentMessage_Logs{
		Logs: &relayv1.LogBatch{RunId: runID, Lines: []*relayv1.LogLine{{
			TsUnixMs: time.Now().UnixMilli(), Line: line, Stderr: stderr,
		}}},
	}}
	select {
	case a.logCh <- msg:
	default:
	}
}

func (a *Agent) Secrets(ctx context.Context, runID string, names []string) (map[string]string, error) {
	resp, err := a.client.GetSecrets(a.authCtx(ctx), &relayv1.SecretsRequest{
		RunId: runID, Names: names,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetValues(), nil
}

func (a *Agent) FetchBlob(ctx context.Context, sha, path string) error {
	stream, err := a.client.DownloadBlob(a.authCtx(ctx), &relayv1.BlobRequest{Sha256: sha})
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(chunk.GetData()); err != nil {
			return err
		}
		h.Write(chunk.GetData())
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("blob %s corrupted in transit (got %s)", sha[:12], got[:12])
	}
	return nil
}

func (a *Agent) UploadFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stream, err := a.client.UploadBlob(a.authCtx(ctx))
	if err != nil {
		return "", err
	}
	buf := make([]byte, 1<<20)
	first := true
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := &relayv1.BlobChunk{Data: buf[:n]}
			_ = first
			first = false
			if sendErr := stream.Send(chunk); sendErr != nil {
				break
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	stat, err := stream.CloseAndRecv()
	if err != nil {
		return "", err
	}
	return stat.GetSha256(), nil
}

// newLineScanner wraps a reader with a generous line buffer (build output
// can contain very long lines).
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return sc
}
