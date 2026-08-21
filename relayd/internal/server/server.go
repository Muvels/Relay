package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

type Config struct {
	DataDir  string
	GRPCAddr string // agents dial this
	HTTPAddr string // SDK/CLI dial this
}

type Server struct {
	cfg         Config
	store       *Store
	grpcSrv     *grpc.Server
	httpSrv     *http.Server
	APIToken    string
	Fingerprint string
	cancel      context.CancelFunc
}

func Start(cfg Config) (*Server, error) {
	// 0700 throughout: the data dir holds every credential in the system.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(cfg.DataDir, 0o700)
	store, err := OpenStore(filepath.Join(cfg.DataDir, "relay.db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(filepath.Join(cfg.DataDir, "relay.db"+suffix), 0o600)
	}
	blobs, err := NewBlobStore(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return nil, err
	}
	logs, err := NewLogHub(filepath.Join(cfg.DataDir, "logs"))
	if err != nil {
		return nil, err
	}
	apiToken, err := store.EnsureSetting("api_token", NewSecret)
	if err != nil {
		return nil, err
	}
	// Convenience for same-machine CLI: readable only by the owner.
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "api_token"), []byte(apiToken+"\n"), 0o600)

	secrets, err := NewSecretStore(store, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}

	if err := store.ensureScheduleTable(); err != nil {
		return nil, err
	}

	fleet := NewFleet()
	runs := NewRuns(store, fleet, logs)

	ctx, cancel := context.WithCancel(context.Background())
	StartJanitor(ctx, runs)
	StartCron(ctx, store, runs)

	cert, fingerprint, err := EnsureCert(cfg.DataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("tls cert: %w", err)
	}
	grpcSrv := grpc.NewServer(
		grpc.Creds(credentials.NewServerTLSFromCert(&cert)),
		grpc.MaxRecvMsgSize(8<<20),
		grpc.MaxSendMsgSize(8<<20),
	)
	execHub := NewExecHub()
	relayv1.RegisterAgentServiceServer(grpcSrv,
		NewAgentSvc(store, fleet, runs, logs, blobs, secrets, execHub))

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("grpc listen %s: %w", cfg.GRPCAddr, err)
	}
	go func() {
		if err := grpcSrv.Serve(grpcLis); err != nil {
			slog.Error("grpc server stopped", "err", err)
		}
	}()

	httpSrv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: NewHTTPHandler(store, fleet, runs, logs, blobs, secrets,
			execHub, apiToken, cfg.DataDir, fingerprint),
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server stopped", "err", err)
		}
	}()

	slog.Info("relayd server up",
		"http", cfg.HTTPAddr, "grpc", cfg.GRPCAddr, "data", cfg.DataDir)
	return &Server{
		cfg: cfg, store: store, grpcSrv: grpcSrv, httpSrv: httpSrv,
		APIToken: apiToken, Fingerprint: fingerprint, cancel: cancel,
	}, nil
}

// Shutdown is BOUNDED: agents hold their session streams open forever by
// design, so a purely graceful gRPC stop would never return. After a short
// grace, streams are cut (agents reconnect with backoff; that's normal).
func (s *Server) Shutdown(ctx context.Context) {
	s.cancel()
	hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
	defer hcancel()
	_ = s.httpSrv.Shutdown(hctx)

	done := make(chan struct{})
	go func() {
		s.grpcSrv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.grpcSrv.Stop()
		<-done
	}
	_ = s.store.Close()
}
