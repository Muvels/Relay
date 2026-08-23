package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

const blobChunkSize = 1 << 20 // 1 MiB

// AgentSvc implements relay.v1.AgentService, including everything an agent can do,
// always over a connection the agent initiated.
type AgentSvc struct {
	relayv1.UnimplementedAgentServiceServer
	store   *Store
	fleet   *Fleet
	runs    *Runs
	logs    *LogHub
	blobs   *BlobStore
	secrets *SecretStore
	execHub *ExecHub
}

func NewAgentSvc(store *Store, fleet *Fleet, runs *Runs, logs *LogHub, blobs *BlobStore, secrets *SecretStore, execHub *ExecHub) *AgentSvc {
	return &AgentSvc{store: store, fleet: fleet, runs: runs, logs: logs,
		blobs: blobs, secrets: secrets, execHub: execHub}
}

// GetSecrets releases values only for names the calling machine's OWN
// active run references. A stolen agent token cannot dump the vault.
func (s *AgentSvc) GetSecrets(ctx context.Context, req *relayv1.SecretsRequest) (*relayv1.SecretsResponse, error) {
	m, err := s.authMachine(ctx)
	if err != nil {
		return nil, err
	}
	if _, connected := s.fleet.Get(m.ID); !connected {
		return nil, status.Error(codes.PermissionDenied,
			"no live fleet session for this machine")
	}
	run, err := s.store.GetRun(req.GetRunId())
	if err != nil || run.MachineID != m.ID || IsTerminal(run.State) {
		return nil, status.Error(codes.PermissionDenied,
			"run is not active on this machine")
	}
	var spec RunSpecJSON
	if err := json.Unmarshal([]byte(run.SpecJSON), &spec); err != nil {
		return nil, status.Error(codes.Internal, "corrupt run spec")
	}
	allowed := map[string]bool{}
	for _, n := range spec.SecretNames {
		allowed[n] = true
	}
	out := map[string]string{}
	for _, name := range req.GetNames() {
		if !allowed[name] {
			return nil, status.Errorf(codes.PermissionDenied,
				"run %s does not declare secret %s", run.ID, name)
		}
		value, err := s.secrets.Get(name)
		if err != nil {
			return nil, status.Errorf(codes.NotFound,
				"secret %s is not set. Run `relay secret set %s` on your dev machine",
				name, name)
		}
		out[name] = value
	}
	return &relayv1.SecretsResponse{Values: out}, nil
}

func (s *AgentSvc) Register(ctx context.Context, req *relayv1.RegisterRequest) (*relayv1.RegisterResponse, error) {
	if err := s.store.ConsumeJoinToken(req.GetJoinToken()); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	name := req.GetRequestedName()
	if name == "" {
		name = req.GetInventory().GetHostname()
	}
	if name == "" {
		name = "machine"
	}
	m, err := s.store.CreateMachine(name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if inv := req.GetInventory(); inv != nil {
		InventoryToMachine(m, inv)
		_ = s.store.UpdateMachineInventory(m)
	}
	slog.Info("machine joined", "name", m.Name, "id", m.ID)
	return &relayv1.RegisterResponse{
		MachineId:   m.ID,
		MachineName: m.Name,
		AgentToken:  m.AgentToken,
	}, nil
}

func (s *AgentSvc) authMachine(ctx context.Context) (*Machine, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	auth := md.Get("authorization")
	if len(auth) == 0 || !strings.HasPrefix(auth[0], "Bearer ") {
		return nil, status.Error(codes.Unauthenticated, "missing agent token")
	}
	m, err := s.store.MachineByToken(strings.TrimPrefix(auth[0], "Bearer "))
	if err != nil {
		return nil, status.Error(codes.Unauthenticated,
			"unknown agent token. Rejoin with `relay connect`")
	}
	return m, nil
}

func (s *AgentSvc) Session(stream grpc.BidiStreamingServer[relayv1.AgentMessage, relayv1.ServerMessage]) error {
	m, err := s.authMachine(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first session message must be Hello")
	}
	if inv := hello.GetInventory(); inv != nil {
		InventoryToMachine(m, inv)
		_ = s.store.UpdateMachineInventory(m)
	}

	session := s.fleet.Attach(m, hello.GetInventory(), hello.GetCachedImageTags())
	defer func() {
		if s.fleet.Detach(m.ID, session) {
			// Persist once at disconnect. While connected, last-seen liveness is
			// held in Session memory so idle agents do not churn SQLite's WAL.
			_ = s.store.TouchMachine(m.ID)
			// Only the current session requeues on exit. A superseded one
			// must not undo assignments sent through its replacement.
			s.runs.OnAgentDetach(m.ID)
		}
	}()
	slog.Info("agent connected", "machine", m.Name)

	s.runs.Reconcile(m.ID, hello.GetActiveRunIds())

	if err := stream.Send(&relayv1.ServerMessage{Msg: &relayv1.ServerMessage_Hello{
		Hello: &relayv1.ServerHello{ServerVersion: Version, HeartbeatIntervalS: 10},
	}}); err != nil {
		return err
	}

	// Writer: everything queued for this machine.
	writerDone := make(chan error, 1)
	go func() {
		for msg := range session.Send {
			if err := stream.Send(msg); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	s.runs.DispatchPending()

	// Reader: agent events until disconnect.
	readerDone := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				readerDone <- err
				return
			}
			// Any inbound message proves liveness. A status flood must not
			// let the janitor mistake a busy machine for a dead one.
			session.Beat(nil)
			switch payload := msg.Msg.(type) {
			case *relayv1.AgentMessage_Heartbeat:
				session.Beat(payload.Heartbeat)
			case *relayv1.AgentMessage_RunStatus:
				s.runs.HandleStatus(m.ID, payload.RunStatus)
				if IsTerminal(protoStateNames[payload.RunStatus.GetState()]) {
					s.runs.DispatchPending() // capacity freed
				}
			case *relayv1.AgentMessage_Logs:
				runID := payload.Logs.GetRunId()
				// Only checked when this process is not already streaming the
				// run, so the cost is one lookup per run per session rather
				// than one per batch. A run whose row is gone would otherwise
				// keep recreating its log file after retention deleted it,
				// forever, so the agent is told to stop it at the source.
				if !s.logs.IsOpen(runID) {
					if _, err := s.store.GetRun(runID); errors.Is(err, ErrNotFound) {
						slog.Warn("logs for a run this server no longer has; "+
							"canceling it", "run", runID, "machine", m.Name)
						session.TrySend(&relayv1.ServerMessage{
							Msg: &relayv1.ServerMessage_Cancel{
								Cancel: &relayv1.CancelRun{RunId: runID}}})
						continue
					}
				}
				lines := make([]string, 0, len(payload.Logs.GetLines()))
				for _, l := range payload.Logs.GetLines() {
					lines = append(lines, l.GetLine())
				}
				s.logs.Append(runID, lines)
			case *relayv1.AgentMessage_ExecOutput:
				s.execHub.Deliver(payload.ExecOutput)
			}
		}
	}()

	select {
	case err = <-readerDone:
	case err = <-writerDone:
	}
	slog.Info("agent disconnected", "machine", m.Name, "err", err)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// ------------------------------------------------------------ blobs

func (s *AgentSvc) HasBlob(ctx context.Context, req *relayv1.BlobRequest) (*relayv1.BlobStat, error) {
	if _, err := s.authMachine(ctx); err != nil {
		return nil, err
	}
	stat := &relayv1.BlobStat{Sha256: req.GetSha256()}
	if s.blobs.Has(req.GetSha256()) {
		stat.Exists = true
		if size, err := s.blobs.Size(req.GetSha256()); err == nil {
			stat.Size = uint64(size)
		}
	}
	return stat, nil
}

func (s *AgentSvc) DownloadBlob(req *relayv1.BlobRequest, stream grpc.ServerStreamingServer[relayv1.BlobChunk]) error {
	if _, err := s.authMachine(stream.Context()); err != nil {
		return err
	}
	r, err := s.blobs.Open(req.GetSha256())
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	defer r.Close()
	buf := make([]byte, blobChunkSize)
	first := true
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := &relayv1.BlobChunk{Data: buf[:n]}
			if first {
				chunk.Sha256 = req.GetSha256()
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *AgentSvc) UploadBlob(stream grpc.ClientStreamingServer[relayv1.BlobChunk, relayv1.BlobStat]) error {
	if _, err := s.authMachine(stream.Context()); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var declared string
	done := make(chan struct{})
	var sha string
	var size int64
	var werr error
	go func() {
		defer close(done)
		sha, size, werr = s.blobs.Write(pr, "")
	}()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-done
			return err
		}
		if chunk.GetSha256() != "" {
			declared = chunk.GetSha256()
		}
		if _, err := pw.Write(chunk.GetData()); err != nil {
			<-done
			return err
		}
	}
	pw.Close()
	<-done
	if werr != nil {
		return status.Error(codes.Internal, werr.Error())
	}
	if declared != "" && declared != sha {
		return status.Errorf(codes.InvalidArgument,
			"blob hash mismatch: declared %s got %s", declared, sha)
	}
	_ = stream.SendAndClose(&relayv1.BlobStat{Sha256: sha, Exists: true, Size: uint64(size)})
	return nil
}

// Version is stamped by main at startup.
var Version = "dev"

// StartJanitor runs the lost-run sweeper until ctx ends.
func StartJanitor(ctx context.Context, runs *Runs) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runs.Janitor(60 * time.Second)
			}
		}
	}()
}
