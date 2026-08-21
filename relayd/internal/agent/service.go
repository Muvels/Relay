package agent

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// RunService supervises a long-running service container.
//
// Exposure model: the auth proxy IS the only listener in both modes; the
// container itself always binds loopback:
//
//	private: proxy listens on the LAN (0.0.0.0:<stable port>), key-gated
//	          by default; auth="none" waives the key for LAN callers
//	public: proxy listens on loopback and a cloudflared quick tunnel
//	        fronts it (interim provider; see PLAN §7/§11; named tunnels
//	          under an owned CF account need account credentials)
//
// The proxy port is agent-owned and stable across container restarts, so
// endpoints survive restarts; the proxy retargets to each new host port.
func (e *DockerExecutor) RunService(ctx context.Context, spec *relayv1.RunSpec, ev Events) {
	runID := spec.GetRunId()
	fail := func(format string, args ...any) {
		ev.Status(runID, relayv1.RunState_RUN_ERROR, fmt.Sprintf(format, args...), -1, "")
	}
	if spec.GetServicePort() == 0 {
		fail("service has no port declared")
		return
	}
	public := spec.GetExpose() == "public"
	funnel := spec.GetExpose() == "funnel"
	if spec.GetServiceKey() == "" && !spec.GetServiceAuthNone() {
		fail("service arrived without an auth key. This is a server bug")
		return
	}
	if public {
		if _, err := exec.LookPath("cloudflared"); err != nil {
			fail("expose=\"public\" needs cloudflared on this machine. " +
				"install it (brew install cloudflared / apt install cloudflared) " +
				"and redeploy, or use expose=\"private\" / \"funnel\"")
			return
		}
	}
	if funnel {
		if tailscaleBin() == "" {
			fail("expose=\"funnel\" needs the tailscale CLI on this machine")
			return
		}
		if tailscaleSelf(ctx) == "" {
			fail("expose=\"funnel\" needs a running tailnet on this machine " +
				"(tailscale up). Note: Funnel requires Tailscale's hosted " +
				"control plane and a policy that allows it. Headscale " +
				"tailnets can use expose=\"private\" instead (endpoints are " +
				"tailnet-reachable automatically)")
			return
		}
		// Tailscale serves one Funnel root route per machine. A second
		// funnel service would silently shadow or misroute the first.
		if owner, ok := claimFunnel(runID); !ok {
			fail("this machine's Funnel is already serving run %s. A "+
				"Tailscale node has one public https root. Use "+
				"expose=\"private\" for this service, or place it on "+
				"another machine (target=)", owner)
			return
		}
		defer releaseFunnel(runID)
	}

	runDir := filepath.Join(e.workRoot, "runs", runID)
	defer os.RemoveAll(runDir)

	// Fetch inputs once; the workspace re-extracts fresh per attempt so a
	// crashing service cannot poison its own next start.
	bundlePath := filepath.Join(runDir, "bundle.tar.gz")
	callPath := filepath.Join(runDir, "call.bin")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fail("prepare run dir: %v", err)
		return
	}
	ev.Status(runID, relayv1.RunState_RUN_BUILDING, "fetching code bundle", 0, "")
	if err := ev.FetchBlob(ctx, spec.GetBundleSha256(), bundlePath); err != nil {
		fail("fetch bundle: %v", err)
		return
	}
	if err := ev.FetchBlob(ctx, spec.GetArgsSha256(), callPath); err != nil {
		fail("fetch call payload: %v", err)
		return
	}
	if present, err := e.docker.ImageExists(ctx, spec.GetImageTag()); err != nil {
		fail("docker unavailable: %v", err)
		return
	} else if !present {
		ev.Status(runID, relayv1.RunState_RUN_BUILDING,
			"building image "+spec.GetImageTag(), 0, "")
		if err := e.buildImage(ctx, spec, ev); err != nil {
			fail("image build failed: %v", err)
			return
		}
	}

	// Tunneled modes keep the proxy loopback-only; private publishes it.
	proxy, err := newAuthProxy(public || funnel, spec.GetServiceKey(), spec.GetServiceAuthNone())
	if err != nil {
		fail("auth proxy: %v", err)
		return
	}
	defer proxy.Close()

	var tunnel *tunnelSupervisor
	defer func() {
		if tunnel != nil {
			tunnel.Close()
		}
	}()

	// Private endpoints prefer the tailnet name when one is up, making them reachable
	// from all the user's devices, anywhere, Tailscale or Headscale alike.
	// Recomputed per healthy attempt so joining/leaving a tailnet is picked
	// up on the next restart or redeploy.
	endpoint := func() string {
		if tunnel != nil {
			return tunnel.URL()
		}
		host := lanIP()
		if ts := tailscaleSelf(ctx); ts != "" {
			host = ts
		}
		return host + ":" + proxy.Port()
	}

	backoff := time.Second
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			ev.Status(runID, relayv1.RunState_RUN_CANCELED, "service stopped", -1, "")
			return
		}

		// Fresh workspace per attempt.
		p := runPaths{runDir: runDir,
			workspace: filepath.Join(runDir, fmt.Sprintf("ws-%d", attempt)),
			ioDir:     filepath.Join(runDir, fmt.Sprintf("io-%d", attempt))}
		if err := os.MkdirAll(p.ioDir, 0o755); err != nil {
			fail("prepare attempt dir: %v", err)
			return
		}
		if err := extractTarGz(bundlePath, p.workspace); err != nil {
			fail("extract bundle: %v", err)
			return
		}
		if err := copyFile(callPath, filepath.Join(p.ioDir, "call.bin")); err != nil {
			fail("stage call payload: %v", err)
			return
		}
		if attempt > 0 { // drop the previous attempt's tree
			_ = os.RemoveAll(filepath.Join(runDir, fmt.Sprintf("ws-%d", attempt-1)))
			_ = os.RemoveAll(filepath.Join(runDir, fmt.Sprintf("io-%d", attempt-1)))
		}

		cfg, err := e.baseConfig(ctx, spec, p, ev)
		if err != nil {
			fail("%v", err)
			return
		}
		portKey := fmt.Sprintf("%d/tcp", spec.GetServicePort())
		cfg.ExposedPorts = map[string]struct{}{portKey: {}}
		// The container always binds loopback. The auth proxy is the only
		// externally reachable listener (PLAN §7: public ≠ open, and
		// private is key-gated by default too).
		cfg.HostConfig.PortBindings = map[string][]portBinding{
			portKey: {{HostIP: "127.0.0.1", HostPort: ""}},
		}

		name := fmt.Sprintf("relay-%s-%d", runID, attempt)
		containerID, err := e.docker.ContainerCreate(ctx, name, cfg)
		if err != nil {
			fail("create container: %v", err)
			return
		}
		if err := e.docker.ContainerStart(ctx, containerID); err != nil {
			e.cleanupContainer(ctx, runID, containerID, ev)
			fail("start container: %v", err)
			return
		}

		logCtx, logCancel := context.WithCancel(context.WithoutCancel(ctx))
		logsDone := make(chan struct{})
		go func() {
			defer close(logsDone)
			_ = e.docker.ContainerLogs(logCtx, containerID, func(line string, stderr bool) {
				ev.Log(runID, line, stderr)
			})
		}()
		joinLogs := func() {
			select {
			case <-logsDone:
			case <-time.After(30 * time.Second):
				logCancel()
				<-logsDone
			}
			logCancel()
		}

		hostPort, healthy := e.awaitHealthy(ctx, containerID, spec.GetServicePort())
		var healthyAt time.Time
		if healthy && ctx.Err() == nil {
			proxy.SetTarget(hostPort)
			if (public || funnel) && tunnel == nil {
				argv := []string{"cloudflared", "tunnel",
					"--url", "http://" + proxy.Addr(), "--no-autoupdate"}
				urlRe := tunnelURLRe
				var health func(context.Context) bool
				if funnel {
					argv = []string{tailscaleBin(), "funnel", proxy.Port()}
					urlRe = funnelURLRe
					// A foreground funnel can outlive its route across
					// `tailscale down/up`. Kill it when the tailnet drops
					// so the supervisor re-establishes a live one.
					health = func(hctx context.Context) bool {
						return tailscaleSelf(hctx) != ""
					}
				}
				tunnel, err = startTunnel(ctx, argv, urlRe, health, runID, ev)
				if err != nil {
					e.cleanupContainer(ctx, runID, containerID, ev)
					joinLogs()
					if funnel {
						fail("funnel: %v. See the [tunnel] lines in `relay "+
							"logs %s`. Funnel needs Tailscale's hosted control "+
							"plane (Headscale cannot Funnel) and the `funnel` "+
							"node attribute in your tailnet policy", err, runID)
					} else {
						fail("tunnel: %v", err)
					}
					return
				}
				tunnel.OnEndpointChange(func(url string) {
					ev.StatusEndpoint(runID, relayv1.RunState_RUN_RUNNING,
						"serving (tunnel reconnected)", url)
				})
			}
			healthyAt = time.Now()
			ev.StatusEndpoint(runID, relayv1.RunState_RUN_RUNNING, "serving", endpoint())
		} else if ctx.Err() == nil {
			// The process is alive but never opened its port. Kill it because waiting forever on
			// a wedged process would freeze the supervisor.
			ev.Status(runID, relayv1.RunState_RUN_BUILDING,
				"service did not open its port within 120s; restarting", 0, "")
			e.cleanupContainer(ctx, runID, containerID, ev)
		}

		var exitCode int
		if ctx.Err() == nil && healthy {
			exitCode, _, err = e.docker.WaitWithTimeout(ctx, containerID, 0)
		}
		// The old host port is dead and may be reused by anything. Return 503
		// through the proxy until the next attempt is healthy.
		proxy.SetTarget("")
		e.cleanupContainer(ctx, runID, containerID, ev)
		joinLogs()
		if ctx.Err() != nil {
			ev.Status(runID, relayv1.RunState_RUN_CANCELED, "service stopped", -1, "")
			return
		}

		// Backoff resets only after a stable healthy run. A service that
		// binds its port and immediately crashes must still back off.
		if healthy && time.Since(healthyAt) > 30*time.Second {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		detail := fmt.Sprintf("service exited (code %d); restarting in %s",
			exitCode, backoff)
		if err != nil {
			detail = fmt.Sprintf("service wait failed (%v); restarting in %s",
				err, backoff)
		}
		ev.Status(runID, relayv1.RunState_RUN_BUILDING, detail, exitCode, "")
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
}

// awaitHealthy resolves the mapped host port and TCP-dials it until the
// service accepts connections (or 120s passes / the ctx dies).
//
// TCP-accept is deliberately the v1 readiness contract (an app-level HTTP
// health option is a documented follow-up); the 30s stability window in the
// restart loop covers bind-then-crash processes.
func (e *DockerExecutor) awaitHealthy(ctx context.Context, containerID string, port uint32) (string, bool) {
	deadline := time.Now().Add(120 * time.Second)
	var hostPort string
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if hostPort == "" {
			if hp, err := e.docker.ContainerHostPort(ctx, containerID, port); err == nil {
				hostPort = hp
			}
		}
		if hostPort != "" {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+hostPort, time.Second)
			if err == nil {
				conn.Close()
				return hostPort, true
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return hostPort, false
}

// SweepOrphans removes containers labeled dev.relay.run that no live run
// owns, which indicates leftovers from an abrupt agent death.
func (e *DockerExecutor) SweepOrphans(ctx context.Context, isActive func(runID string) bool) {
	ids, err := e.docker.ListRelayContainers(ctx)
	if err != nil {
		return
	}
	for id, runID := range ids {
		if !isActive(runID) {
			slog.Info("removing orphaned container", "run", runID)
			_ = e.docker.ContainerRemove(ctx, id)
		}
	}
}

// funnelMu guards the one-Funnel-per-machine invariant.
var (
	funnelMu    sync.Mutex
	funnelOwner string
)

func claimFunnel(runID string) (string, bool) {
	funnelMu.Lock()
	defer funnelMu.Unlock()
	if funnelOwner != "" && funnelOwner != runID {
		return funnelOwner, false
	}
	funnelOwner = runID
	return runID, true
}

func releaseFunnel(runID string) {
	funnelMu.Lock()
	defer funnelMu.Unlock()
	if funnelOwner == runID {
		funnelOwner = ""
	}
}

// ---------------------------------------------------------------- proxy

// authProxy fronts every exposed service. The Relay credential is checked
// in constant time, stripped before forwarding, and application-level
// Authorization survives when the key came via X-Relay-Key.
type authProxy struct {
	server *http.Server
	lis    net.Listener
	proxy  *httputil.ReverseProxy
	target atomic.Value // string host port
	key    string
	open   bool
}

func newAuthProxy(loopbackOnly bool, key string, authNone bool) (*authProxy, error) {
	bind := "0.0.0.0:0"
	if loopbackOnly {
		bind = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	ap := &authProxy{lis: lis, key: key, open: authNone}
	ap.target.Store("")
	ap.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http",
				Host: "127.0.0.1:" + ap.target.Load().(string)})
			r.Out.Host = r.In.Host
			// Go strips X-Forwarded-* before Rewrite. Tunnel front-ends
			// (cloudflared, tailscale funnel) set real client IP/proto on
			// their loopback hop. Preserve those so apps see HTTPS and the
			// true caller; otherwise synthesize from the direct connection.
			forwarded := false
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto",
				"X-Forwarded-Host"} {
				if v := r.In.Header.Values(h); len(v) > 0 {
					r.Out.Header[h] = v
					forwarded = true
				}
			}
			if !forwarded {
				r.SetXForwarded()
			}
		},
	}
	ap.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ap.open {
			viaDedicated := r.Header.Get("X-Relay-Key")
			viaBearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			ok := ap.keyMatches(viaDedicated) || ap.keyMatches(viaBearer)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "relay: missing or invalid service key",
					http.StatusUnauthorized)
				return
			}
			// Never forward the Relay credential upstream; keep the app's
			// own Authorization only when the key came via X-Relay-Key.
			r.Header.Del("X-Relay-Key")
			if !ap.keyMatches(viaDedicated) {
				r.Header.Del("Authorization")
			}
		}
		if ap.target.Load().(string) == "" {
			http.Error(w, "relay: service is starting", http.StatusServiceUnavailable)
			return
		}
		ap.proxy.ServeHTTP(w, r)
	})}
	go func() { _ = ap.server.Serve(lis) }()
	return ap, nil
}

func (ap *authProxy) keyMatches(candidate string) bool {
	if candidate == "" || ap.key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(ap.key)) == 1
}

func (ap *authProxy) Addr() string { return ap.lis.Addr().String() }
func (ap *authProxy) Port() string {
	_, port, _ := net.SplitHostPort(ap.lis.Addr().String())
	return port
}
func (ap *authProxy) SetTarget(port string) { ap.target.Store(port) }
func (ap *authProxy) Close()                { _ = ap.server.Close() }

// ---------------------------------------------------------------- tunnel

// tunnelURLRe is anchored to cloudflared's registration banner line.
var tunnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// tunnelSupervisor keeps a cloudflared quick tunnel alive for the life of
// its ctx: restarts it on unexpected exit (with backoff) and reports fresh
// URLs. The whole cloudflared process group dies with Close().
type tunnelSupervisor struct {
	url      atomic.Value // string
	cancel   context.CancelFunc
	done     chan struct{}
	onChange atomic.Value // func(string)
}

func (t *tunnelSupervisor) URL() string { return t.url.Load().(string) }

func (t *tunnelSupervisor) OnEndpointChange(fn func(string)) {
	t.onChange.Store(fn)
}

func (t *tunnelSupervisor) Close() {
	t.cancel()
	<-t.done
}

// startTunnel launches a supervised tunnel process (cloudflared quick
// tunnel or tailscale funnel, meaning anything that prints its public URL and
// stays alive) and blocks until the first URL is known. Lifecycle derives
// from the SERVICE ctx: cancel kills the tunnel.
func startTunnel(serviceCtx context.Context, argv []string, urlRe *regexp.Regexp, health func(context.Context) bool, runID string, ev Events) (*tunnelSupervisor, error) {
	tctx, cancel := context.WithCancel(serviceCtx)
	t := &tunnelSupervisor{cancel: cancel, done: make(chan struct{})}
	t.url.Store("")

	firstURL := make(chan string, 1)
	firstErr := make(chan error, 1)
	go func() {
		defer close(t.done)
		backoff := time.Second
		sentFirst := false
		for tctx.Err() == nil {
			started := time.Now()
			// runTunnelOnce blocks for the lifetime of one tunnel process.
			// the URL must be signaled from the callback, never after
			// return, or a healthy tunnel would block startup forever.
			_, err := runTunnelOnce(tctx, argv, urlRe, health, runID, ev, func(u string) {
				t.url.Store(u)
				if !sentFirst {
					sentFirst = true
					firstURL <- u
					return
				}
				if fn, ok := t.onChange.Load().(func(string)); ok && fn != nil {
					fn(u)
				}
			})
			if !sentFirst {
				sentFirst = true
				if err == nil {
					err = fmt.Errorf("%s ended without a URL", argv[0])
				}
				firstErr <- err
				return
			}
			if tctx.Err() != nil {
				return
			}
			if time.Since(started) > 5*time.Minute {
				backoff = time.Second // long stable tunnel resets the backoff
			}
			ev.Log(runID, "[tunnel] "+argv[0]+" exited; restarting in "+backoff.String(), true)
			select {
			case <-tctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()

	select {
	case u := <-firstURL:
		t.url.Store(u)
		return t, nil
	case err := <-firstErr:
		cancel()
		<-t.done
		return nil, err
	case <-serviceCtx.Done():
		cancel()
		<-t.done
		return nil, serviceCtx.Err()
	}
}

// runTunnelOnce runs one tunnel process to completion. onURL fires as soon
// as the public URL is printed. Returns the URL (or an error when none
// appeared within 45s).
func runTunnelOnce(ctx context.Context, argv []string, urlRe *regexp.Regexp, health func(context.Context) bool, runID string, ev Events, onURL func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { // kill the whole tree, not just the parent
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}

	urlCh := make(chan string, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := newLineScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if m := urlRe.FindString(line); m != "" {
				select {
				case urlCh <- m:
				default:
				}
			}
			ev.Log(runID, "[tunnel] "+line, true)
		}
	}()

	waitErr := make(chan error, 1)
	go func() {
		<-scanDone
		waitErr <- cmd.Wait()
	}()

	// Liveness watchdog: some tunnels (foreground funnel) can outlive their
	// route; two consecutive failed health checks kill the process so the
	// supervisor rebuilds a live tunnel.
	if health != nil {
		go func() {
			misses := 0
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if health(ctx) {
						misses = 0
						continue
					}
					misses++
					if misses >= 2 {
						ev.Log(runID, "[tunnel] backend unhealthy; recycling tunnel", true)
						_ = cmd.Cancel()
						return
					}
				}
			}
		}()
	}

	select {
	case url := <-urlCh:
		onURL(url) // signals readiness upward IMMEDIATELY
		<-waitErr  // then supervise this instance until it ends
		return url, nil
	case err := <-waitErr:
		return "", fmt.Errorf("%s exited before reporting a URL: %v", argv[0], err)
	case <-time.After(45 * time.Second):
		_ = cmd.Cancel()
		<-waitErr
		return "", fmt.Errorf("%s did not report a tunnel URL within 45s", argv[0])
	case <-ctx.Done():
		<-waitErr
		return "", ctx.Err()
	}
}

// lanIP finds this machine's primary outbound interface address without
// sending anything (UDP "connect" only sets the route).
func lanIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET, never sent
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return "127.0.0.1"
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
