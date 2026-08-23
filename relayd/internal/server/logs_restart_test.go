package server

import (
	"os"
	"path/filepath"
	"testing"
)

// A settled run must not be followable after the process that ran it exited.
// The hub is rebuilt empty on every start, so "have I seen this run finish"
// has to come from the caller (the run row), never from hub memory.
func TestSubscribeToSettledRunAfterRestartReturnsNoLiveChannel(t *testing.T) {
	dir := t.TempDir()
	hub, err := NewLogHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub.Append("run_1", []string{"one", "two"})
	hub.Close("run_1")

	restarted, err := NewLogHub(dir) // a fresh process over the same data dir
	if err != nil {
		t.Fatal(err)
	}
	backlog, live, cancel := restarted.Subscribe("run_1", true)
	defer cancel()
	if live != nil {
		t.Fatal("settled run handed out a live channel that would never close")
	}
	if len(backlog) != 2 || backlog[0] != "one" || backlog[1] != "two" {
		t.Fatalf("backlog lost lines: %q", backlog)
	}
}

func TestSubscribeToLiveRunStillFollows(t *testing.T) {
	hub, err := NewLogHub(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, live, cancel := hub.Subscribe("run_1", false)
	defer cancel()
	if live == nil {
		t.Fatal("live run got no channel to follow")
	}
	hub.Append("run_1", []string{"hello"})
	if got := <-live; got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

func TestForgetDropsLogFileAndReopenedHandle(t *testing.T) {
	dir := t.TempDir()
	hub, err := NewLogHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub.Append("run_1", []string{"one"})
	hub.Close("run_1")
	// A straggler batch from an agent that reconnected after its run was
	// declared lost reopens the file; Forget is what reclaims it.
	hub.Append("run_1", []string{"late"})

	hub.Forget([]string{"run_1"})
	if _, err := os.Stat(filepath.Join(dir, "run_1.log")); !os.IsNotExist(err) {
		t.Fatalf("log file survived Forget: %v", err)
	}
	hub.mu.Lock()
	_, stillOpen := hub.files["run_1"]
	hub.mu.Unlock()
	if stillOpen {
		t.Fatal("file handle leaked past Forget")
	}
}
