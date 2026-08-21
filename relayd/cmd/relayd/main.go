// relayd combines the Relay control plane and machine agent in one binary.
//
//	relayd server [--data-dir D] [--http :7460] [--grpc :7461]
//	relayd agent  --server HOST:PORT [--join TOKEN] [--name N] [--state-dir D]
//	relayd version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/matteomarolt/relay/relayd/internal/agent"
	"github.com/matteomarolt/relay/relayd/internal/server"
)

var version = "0.1.0-dev" // stamped via -ldflags "-X main.version=..."

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "version":
		fmt.Println("relayd", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `relayd: Relay control plane / machine agent

  relayd server [flags]   run the control plane
  relayd agent  [flags]   run the machine agent (dials out to the server)
  relayd version`)
}

func home(sub string) string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return sub
	}
	return filepath.Join(dir, sub)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dataDir := fs.String("data-dir", home(".relay/server"), "state directory")
	httpAddr := fs.String("http", "127.0.0.1:7460", "SDK/CLI HTTP listen address")
	grpcAddr := fs.String("grpc", "0.0.0.0:7461", "agent gRPC listen address")
	_ = fs.Parse(args)

	server.Version = version
	srv, err := server.Start(server.Config{
		DataDir: *dataDir, HTTPAddr: *httpAddr, GRPCAddr: *grpcAddr,
	})
	if err != nil {
		slog.Error("server start failed", "err", err)
		os.Exit(1)
	}
	fmt.Printf("relayd server ready\n  http  %s\n  grpc  %s\n  token %s\n",
		*httpAddr, *grpcAddr, srv.APIToken)

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down")
	srv.Shutdown(context.Background())
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	serverAddr := fs.String("server", "", "server gRPC address host:port (required)")
	join := fs.String("join", "", "one-time join token; accepts TOKEN or TOKEN@HOST:PORT#FINGERPRINT")
	joinOnly := fs.Bool("join-only", false, "register and store credentials, then exit (used by the installer)")
	name := fs.String("name", "", "requested machine name (default: hostname)")
	stateDir := fs.String("state-dir", home(".relay/agent"), "agent state directory")
	dockerSock := fs.String("docker-sock", "", "docker socket path override")
	_ = fs.Parse(args)

	joinToken := *join
	addr := *serverAddr
	fingerprint := ""
	// `relay connect` prints TOKEN@HOST:PORT#FP as one pasteable unit.
	if hash := strings.LastIndex(joinToken, "#"); hash > 0 {
		fingerprint = joinToken[hash+1:]
		joinToken = joinToken[:hash]
	}
	if at := strings.LastIndex(joinToken, "@"); at > 0 {
		if addr == "" {
			addr = joinToken[at+1:]
		}
		joinToken = joinToken[:at]
	}
	if addr == "" {
		fmt.Fprintln(os.Stderr, "relayd agent: --server HOST:PORT is required "+
			"(or pass --join TOKEN@HOST:PORT)")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := agent.Run(ctx, agent.Config{
		ServerAddr:        addr,
		JoinToken:         joinToken,
		ServerFingerprint: fingerprint,
		Name:              *name,
		StateDir:          *stateDir,
		Version:           version,
		DockerSock:        *dockerSock,
		JoinOnly:          *joinOnly,
	})
	if err != nil {
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}
