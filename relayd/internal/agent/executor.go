package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// Events is how a running job reports back into the session loop.
type Events interface {
	Status(runID string, state relayv1.RunState, detail string, exitCode int, resultSha string)
	StatusEndpoint(runID string, state relayv1.RunState, detail, endpoint string)
	Log(runID, line string, stderr bool)
	// FetchBlob writes the blob to path; UploadFile returns its sha256.
	FetchBlob(ctx context.Context, sha, path string) error
	UploadFile(ctx context.Context, path string) (string, error)
	// Secrets resolves declared secret names to values at run start.
	Secrets(ctx context.Context, runID string, names []string) (map[string]string, error)
}

// userErrorExit mirrors relay.runtime.invoke.USER_ERROR_EXIT.
const userErrorExit = 13

// sdkBundleDir is where the SDK embeds its own source inside every code
// bundle so containers need no Relay preinstalled. Mirrored in the Python
// client (bundle.py).
const sdkBundleDir = "_relay_sdk"

var volumeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)

func validVolumeName(name string) bool {
	return volumeNameRe.MatchString(name)
}

// reservedMountConflict returns a reason when a CLEANED absolute mount path
// would shadow Relay's runtime paths ("" = fine). Blocks equality, parents
// of reserved paths, and anything inside the io/runtime trees; nested
// mounts inside /workspace are allowed (that's what volumes are for).
func reservedMountConflict(clean string) string {
	reserved := []string{"/relay/io", "/workspace", "/opt/relay"}
	if clean == "/" {
		return "would shadow the filesystem root"
	}
	for _, r := range reserved {
		if clean == r {
			return "collides with the Relay-reserved path " + r
		}
		if strings.HasPrefix(r+"/", clean+"/") { // clean is a parent of r
			return "contains the Relay-reserved path " + r
		}
	}
	for _, tree := range []string{"/relay", "/opt/relay"} {
		if strings.HasPrefix(clean+"/", tree+"/") {
			return "is inside the Relay-reserved tree " + tree
		}
	}
	return ""
}

// DockerExecutor runs RunSpecs in containers. The contract matches the M0
// local executor exactly (io dir + invoke protocol); only the transport
// around it changed. Jobs run once; services (service.go) are supervised.
type DockerExecutor struct {
	docker   *dockerClient
	workRoot string
}

func NewDockerExecutor(docker *dockerClient, workRoot string) *DockerExecutor {
	return &DockerExecutor{docker: docker, workRoot: workRoot}
}

type runPaths struct {
	runDir    string
	workspace string
	ioDir     string
}

// prepare materializes bundle + call payload and ensures the image exists,
// reporting failures itself. ok=false means a status was already sent.
func (e *DockerExecutor) prepare(ctx context.Context, spec *relayv1.RunSpec, ev Events) (runPaths, bool) {
	runID := spec.GetRunId()
	fail := func(format string, args ...any) {
		ev.Status(runID, relayv1.RunState_RUN_ERROR, fmt.Sprintf(format, args...), -1, "")
	}
	p := runPaths{runDir: filepath.Join(e.workRoot, "runs", runID)}
	p.workspace = filepath.Join(p.runDir, "workspace")
	p.ioDir = filepath.Join(p.runDir, "io")
	for _, dir := range []string{p.workspace, p.ioDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("prepare run dir: %v", err)
			return p, false
		}
	}

	ev.Status(runID, relayv1.RunState_RUN_BUILDING, "fetching code bundle", 0, "")
	bundlePath := filepath.Join(p.runDir, "bundle.tar.gz")
	if err := ev.FetchBlob(ctx, spec.GetBundleSha256(), bundlePath); err != nil {
		fail("fetch bundle: %v", err)
		return p, false
	}
	if err := extractTarGz(bundlePath, p.workspace); err != nil {
		fail("extract bundle: %v", err)
		return p, false
	}
	if err := ev.FetchBlob(ctx, spec.GetArgsSha256(),
		filepath.Join(p.ioDir, "call.bin")); err != nil {
		fail("fetch call payload: %v", err)
		return p, false
	}

	present, err := e.docker.ImageExists(ctx, spec.GetImageTag())
	if err != nil {
		fail("docker unavailable: %v", err)
		return p, false
	}
	if !present {
		ev.Status(runID, relayv1.RunState_RUN_BUILDING,
			"building image "+spec.GetImageTag(), 0, "")
		if err := e.buildImage(ctx, spec, ev); err != nil {
			fail("image build failed: %v", err)
			return p, false
		}
	}
	return p, true
}

// baseConfig assembles the shared container configuration (env incl.
// secrets, mounts incl. volumes, resource limits, GPU pinning).
func (e *DockerExecutor) baseConfig(ctx context.Context, spec *relayv1.RunSpec, p runPaths, ev Events) (*containerConfig, error) {
	runID := spec.GetRunId()
	env := []string{
		"PYTHONPATH=/workspace/" + sdkBundleDir,
		"RELAY_RUN_ID=" + runID,
	}
	if names := spec.GetSecretNames(); len(names) > 0 {
		values, err := ev.Secrets(ctx, runID, names)
		if err != nil {
			return nil, fmt.Errorf("resolve secrets: %w", err)
		}
		for name, value := range values {
			env = append(env, name+"="+value)
		}
	}

	cfg := &containerConfig{
		Image:      spec.GetImageTag(),
		Entrypoint: []string{"python3"}, // registry images keep their own otherwise
		Cmd:        []string{"-m", "relay.runtime.invoke", "/relay/io"},
		// Start OUTSIDE /workspace: `python3 -m` prepends the cwd to
		// sys.path, and a project shipping its own relay.py must not shadow
		// the embedded runtime. invoke chdirs to /workspace before user code.
		WorkingDir: "/relay/io",
		Env:        env,
		Labels:     map[string]string{"dev.relay.run": runID},
	}
	// The workspace is a per-run scratch copy, so it mounts read-write.
	// unlike M0 local mode, which mounts the user's real project read-only.
	cfg.HostConfig.Binds = []string{
		p.ioDir + ":/relay/io",
		p.workspace + ":/workspace",
	}
	for mountPath, volName := range spec.GetVolumes() {
		clean := pathpkg.Clean(mountPath)
		if !validVolumeName(volName) || !pathpkg.IsAbs(clean) {
			return nil, fmt.Errorf("invalid volume mapping %q → %q", mountPath, volName)
		}
		// Canonicalized reserved-path guard: no mount may equal, contain,
		// or live inside the runtime's own paths.
		if reason := reservedMountConflict(clean); reason != "" {
			return nil, fmt.Errorf("volume mount %q %s", mountPath, reason)
		}
		volDir := filepath.Join(e.workRoot, "volumes", volName)
		if err := os.MkdirAll(volDir, 0o755); err != nil {
			return nil, fmt.Errorf("create volume %s: %w", volName, err)
		}
		cfg.HostConfig.Binds = append(cfg.HostConfig.Binds, volDir+":"+clean)
	}
	if res := spec.GetResources(); res != nil {
		if res.GetCpus() > 0 {
			cfg.HostConfig.NanoCpus = int64(res.GetCpus() * 1e9)
		}
		if res.GetMemoryMib() > 0 {
			cfg.HostConfig.Memory = int64(res.GetMemoryMib()) << 20
		}
	}
	// Pin to exactly the devices the scheduler granted, never "all".
	if spec.GetAcceleratorKind() == "cuda" && len(spec.GetDeviceIndices()) > 0 {
		ids := make([]string, 0, len(spec.GetDeviceIndices()))
		for _, idx := range spec.GetDeviceIndices() {
			ids = append(ids, fmt.Sprintf("%d", idx))
		}
		cfg.HostConfig.DeviceRequests = []deviceRequest{{
			Driver: "nvidia", DeviceIDs: ids, Capabilities: [][]string{{"gpu"}},
		}}
	}
	return cfg, nil
}

// cleanupContainer force-removes with a bound so a wedged daemon can't pin
// the run goroutine forever.
func (e *DockerExecutor) cleanupContainer(ctx context.Context, runID, containerID string, ev Events) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := e.docker.ContainerRemove(cctx, containerID); err != nil {
		ev.Log(runID, "[relay] container cleanup failed: "+err.Error(), true)
	}
}

// Run executes a one-shot job.
func (e *DockerExecutor) Run(ctx context.Context, spec *relayv1.RunSpec, ev Events) {
	runID := spec.GetRunId()
	fail := func(format string, args ...any) {
		ev.Status(runID, relayv1.RunState_RUN_ERROR, fmt.Sprintf(format, args...), -1, "")
	}

	p, ok := e.prepare(ctx, spec, ev)
	if !ok {
		return
	}
	defer os.RemoveAll(p.runDir)

	cfg, err := e.baseConfig(ctx, spec, p, ev)
	if err != nil {
		fail("%v", err)
		return
	}
	containerID, err := e.docker.ContainerCreate(ctx, "relay-"+runID, cfg)
	if err != nil {
		fail("create container: %v", err)
		return
	}
	defer e.cleanupContainer(ctx, runID, containerID, ev)

	if err := e.docker.ContainerStart(ctx, containerID); err != nil {
		fail("start container: %v", err)
		return
	}
	ev.Status(runID, relayv1.RunState_RUN_RUNNING, "", 0, "")

	// The log follower gets its own cancelable context so no failure mode
	// (wedged daemon, failed removal) can strand this goroutine forever.
	logCtx, logCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer logCancel()
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
	}

	timeout := time.Duration(spec.GetResources().GetTimeoutS()) * time.Second
	exitCode, timedOut, err := e.docker.WaitWithTimeout(ctx, containerID, timeout)
	if timedOut || err != nil {
		// Kill the container first because the log follower only ends when the
		// container does, so joining logs before removal deadlocks.
		e.cleanupContainer(ctx, runID, containerID, ev)
		joinLogs()
		if timedOut {
			fail("run exceeded its %s timeout and was killed", timeout)
			return
		}
		if ctx.Err() != nil { // canceled from the server
			ev.Status(runID, relayv1.RunState_RUN_CANCELED, "canceled", -1, "")
			return
		}
		fail("wait: %v", err)
		return
	}
	joinLogs()

	// Collect the result.
	resultPath := filepath.Join(p.ioDir, "result.bin")
	if _, statErr := os.Stat(resultPath); statErr != nil ||
		(exitCode != 0 && exitCode != userErrorExit) {
		fail("container exited %d without a result. This is an infrastructure failure; "+
			"see run logs", exitCode)
		return
	}
	resultSha, err := uploadResultWithRetry(ctx, ev, runID, resultPath)
	if err != nil {
		fail("upload result: %v", err)
		return
	}
	if exitCode == userErrorExit {
		ev.Status(runID, relayv1.RunState_RUN_FAILED,
			"function raised", exitCode, resultSha)
	} else {
		ev.Status(runID, relayv1.RunState_RUN_SUCCEEDED, "", 0, resultSha)
	}
}

// uploadResultWithRetry is PATIENT: a result that finished while the
// control plane was offline (dev laptop asleep, server rebooting) must
// survive until the server returns. The run stays active agent-side, so
// reconcile keeps it alive and the terminal status flushes after upload.
func uploadResultWithRetry(ctx context.Context, ev Events, runID, path string) (string, error) {
	backoff := 5 * time.Second
	deadline := time.Now().Add(24 * time.Hour)
	for attempt := 0; ; attempt++ {
		sha, err := ev.UploadFile(ctx, path)
		if err == nil {
			if attempt > 0 {
				ev.Log(runID, "[relay] result uploaded after reconnect", true)
			}
			return sha, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return "", err
		}
		if attempt == 0 {
			ev.Log(runID, "[relay] result ready but the server is "+
				"unreachable. Retrying until it returns", true)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// buildImage shells out to the docker CLI: BuildKit access without linking
// BuildKit. Build output streams into the run's logs.
func (e *DockerExecutor) buildImage(ctx context.Context, spec *relayv1.RunSpec, ev Events) error {
	cmd := exec.CommandContext(ctx, "docker", "build",
		"--progress=plain", "-t", spec.GetImageTag(), "-")
	cmd.Stdin = strings.NewReader(spec.GetImageDockerfile())
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker CLI not available for image build: %w", err)
	}
	sc := newLineScanner(out)
	for sc.Scan() {
		ev.Log(spec.GetRunId(), "[build] "+sc.Text(), true)
	}
	return cmd.Wait()
}

// extractTarGz unpacks a bundle, refusing entries that escape the target.
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destAbs, hdr.Name)
		if !strings.HasPrefix(target, destAbs+string(os.PathSeparator)) && target != destAbs {
			return fmt.Errorf("bundle entry %q escapes the workspace", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			w.Close()
		case tar.TypeSymlink, tar.TypeLink:
			// Links could point outside the workspace; bundles never
			// legitimately contain them (the SDK skips them at pack time).
			return fmt.Errorf("bundle entry %q: links are not allowed", hdr.Name)
		}
	}
}
