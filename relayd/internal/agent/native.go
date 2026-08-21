package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// NativeExecutor runs jobs as host processes in uv-managed virtualenvs.
// the only way to reach Apple Silicon's GPU (Metal has no container
// passthrough, so MPS work cannot be containerized).
//
// Isolation note, deliberate and documented: native runs share the host
// (no container boundary). That is the same trust domain as everything
// else in a personal fleet; resource limits are scheduler reservations.
type NativeExecutor struct {
	workRoot string
}

func NewNativeExecutor(workRoot string) *NativeExecutor {
	return &NativeExecutor{workRoot: workRoot}
}

// NativeAvailable reports whether this machine can run native jobs (uv on
// PATH) and whether it offers MPS (Apple Silicon).
func NativeAvailable() (native bool, mps bool) {
	if _, err := exec.LookPath("uv"); err != nil {
		return false, false
	}
	return true, runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

func (e *NativeExecutor) venvDir(env *relayv1.NativeEnv) string {
	h := sha256.New()
	h.Write([]byte("py=" + env.GetPythonMinor() + "\n"))
	pkgs := append([]string(nil), env.GetPipPackages()...)
	sort.Strings(pkgs)
	for _, p := range pkgs {
		h.Write([]byte("pip=" + p + "\n"))
	}
	return filepath.Join(e.workRoot, "venvs", hex.EncodeToString(h.Sum(nil))[:16])
}

// venvLocks serializes builds per venv key within this agent process.
var venvLocks sync.Map // key string → *sync.Mutex

// ensureVenv builds (or reuses) the environment for a NativeEnv. Builds go
// into a temp dir and atomically rename into place under a per-key lock, so
// concurrent cold starts can never see (or corrupt) a half-built venv.
func (e *NativeExecutor) ensureVenv(ctx context.Context, env *relayv1.NativeEnv, runID string, ev Events) (string, error) {
	dir := e.venvDir(env)
	ready := filepath.Join(dir, ".ready")
	if _, err := os.Stat(ready); err == nil {
		return dir, nil
	}

	mu, _ := venvLocks.LoadOrStore(dir, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if _, err := os.Stat(ready); err == nil {
		return dir, nil // built while we waited
	}

	ev.Status(runID, relayv1.RunState_RUN_BUILDING,
		"creating native venv (python "+env.GetPythonMinor()+")", 0, "")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".build-*")
	if err != nil {
		return "", err
	}
	buildDir := filepath.Join(tmp, "venv")
	defer os.RemoveAll(tmp)

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "uv", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		out, err := cmd.CombinedOutput()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				ev.Log(runID, "[venv] "+line, true)
			}
		}
		if err != nil {
			return fmt.Errorf("uv %s: %w", args[0], err)
		}
		return nil
	}
	if err := run("venv", "--python", env.GetPythonMinor(), buildDir); err != nil {
		return "", err
	}
	if pkgs := env.GetPipPackages(); len(pkgs) > 0 {
		args := append([]string{"pip", "install", "--python",
			filepath.Join(buildDir, "bin", "python")}, pkgs...)
		if err := run(args...); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(buildDir, ".ready"), []byte("ok\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir) // stale half-build from a crashed agent
	if err := os.Rename(buildDir, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Run executes one job natively. Mirrors DockerExecutor.Run's contract.
func (e *NativeExecutor) Run(ctx context.Context, spec *relayv1.RunSpec, ev Events) {
	runID := spec.GetRunId()
	fail := func(format string, args ...any) {
		ev.Status(runID, relayv1.RunState_RUN_ERROR, fmt.Sprintf(format, args...), -1, "")
	}
	nenv := spec.GetNativeEnv()
	if nenv == nil || !nenv.GetSupported() {
		fail("this image uses Docker-only steps (apt/run/registry base) and " +
			"cannot run natively. The scheduler may have a bug or stale spec")
		return
	}
	// Canonicalize BEFORE the prefix check: /workspace/../../etc must never
	// pass, because the resolved path later feeds RemoveAll/Symlink.
	volumeMounts := map[string]string{}
	for mount, volName := range spec.GetVolumes() {
		clean := pathpkg.Clean(mount)
		if !strings.HasPrefix(clean, "/workspace/") || clean == "/workspace" {
			fail("native runs support volumes only under /workspace/ "+
				"(got %s). Containerless processes cannot bind arbitrary "+
				"absolute paths", mount)
			return
		}
		volumeMounts[clean] = volName
	}

	runDir := filepath.Join(e.workRoot, "runs", runID)
	workspace := filepath.Join(runDir, "workspace")
	ioDir := filepath.Join(runDir, "io")
	for _, dir := range []string{workspace, ioDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("prepare run dir: %v", err)
			return
		}
	}
	defer os.RemoveAll(runDir)

	ev.Status(runID, relayv1.RunState_RUN_BUILDING, "fetching code bundle", 0, "")
	bundlePath := filepath.Join(runDir, "bundle.tar.gz")
	if err := ev.FetchBlob(ctx, spec.GetBundleSha256(), bundlePath); err != nil {
		fail("fetch bundle: %v", err)
		return
	}
	if err := extractTarGz(bundlePath, workspace); err != nil {
		fail("extract bundle: %v", err)
		return
	}
	if err := ev.FetchBlob(ctx, spec.GetArgsSha256(),
		filepath.Join(ioDir, "call.bin")); err != nil {
		fail("fetch call payload: %v", err)
		return
	}

	venv, err := e.ensureVenv(ctx, nenv, runID, ev)
	if err != nil {
		fail("native env: %v", err)
		return
	}

	// Volumes: symlink workspace-relative mounts to persistent dirs. The
	// resolved link must land INSIDE the workspace (defense in depth on top
	// of the cleaned-prefix check above).
	wsAbs, _ := filepath.Abs(workspace)
	for mount, volName := range volumeMounts {
		if !validVolumeName(volName) {
			fail("invalid volume name %q", volName)
			return
		}
		volDir := filepath.Join(e.workRoot, "volumes", volName)
		if err := os.MkdirAll(volDir, 0o755); err != nil {
			fail("create volume %s: %v", volName, err)
			return
		}
		link := filepath.Join(wsAbs, strings.TrimPrefix(mount, "/workspace/"))
		if rel, err := filepath.Rel(wsAbs, link); err != nil ||
			strings.HasPrefix(rel, "..") {
			fail("volume mount %s escapes the workspace", mount)
			return
		}
		_ = os.MkdirAll(filepath.Dir(link), 0o755)
		_ = os.RemoveAll(link)
		if err := os.Symlink(volDir, link); err != nil {
			fail("link volume %s: %v", volName, err)
			return
		}
	}

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PYTHONPATH=" + filepath.Join(workspace, sdkBundleDir),
		"RELAY_RUN_ID=" + runID,
		"RELAY_WORKSPACE=" + workspace,
		"PYTHONUNBUFFERED=1",
	}
	for k, v := range nenv.GetEnv() {
		env = append(env, k+"="+v)
	}
	if names := spec.GetSecretNames(); len(names) > 0 {
		values, err := ev.Secrets(ctx, runID, names)
		if err != nil {
			fail("resolve secrets: %v", err)
			return
		}
		for name, value := range values {
			env = append(env, name+"="+value)
		}
	}

	cmd := exec.Command(filepath.Join(venv, "bin", "python"),
		"-m", "relay.runtime.invoke", ioDir)
	cmd.Dir = ioDir // shadowing guard, same as containers
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // kill the whole tree

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail("pipe: %v", err)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fail("start native process: %v", err)
		return
	}
	ev.Status(runID, relayv1.RunState_RUN_RUNNING, "native (mps)", 0, "")

	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		sc := newLineScanner(stdout)
		for sc.Scan() {
			ev.Log(runID, sc.Text(), false)
		}
	}()

	killTree := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	var timedOut atomic.Bool
	var timer *time.Timer
	if t := spec.GetResources().GetTimeoutS(); t > 0 {
		timer = time.AfterFunc(time.Duration(t)*time.Second, func() {
			timedOut.Store(true)
			killTree()
		})
		defer timer.Stop()
	}
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree()
		case <-cancelWatch:
		}
	}()

	err = cmd.Wait()
	close(cancelWatch)
	// Sweep detached descendants: the group id stays valid until reaped
	// children are gone; a normal parent exit must not leak background
	// processes it spawned.
	killTree()
	<-logsDone

	if timedOut.Load() {
		fail("run exceeded its %ds timeout and was killed",
			spec.GetResources().GetTimeoutS())
		return
	}
	if ctx.Err() != nil {
		ev.Status(runID, relayv1.RunState_RUN_CANCELED, "canceled", -1, "")
		return
	}
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			fail("wait: %v", err)
			return
		}
	}

	resultPath := filepath.Join(ioDir, "result.bin")
	if _, statErr := os.Stat(resultPath); statErr != nil ||
		(exitCode != 0 && exitCode != userErrorExit) {
		fail("native process exited %d without a result. See the run logs", exitCode)
		return
	}
	resultSha, err := uploadResultWithRetry(ctx, ev, runID, resultPath)
	if err != nil {
		fail("upload result: %v", err)
		return
	}
	if exitCode == userErrorExit {
		ev.Status(runID, relayv1.RunState_RUN_FAILED, "function raised", exitCode, resultSha)
	} else {
		ev.Status(runID, relayv1.RunState_RUN_SUCCEEDED, "", 0, resultSha)
	}
}
