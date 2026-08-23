package server

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogHub persists run logs to one file per run and fans live lines out to
// subscribers (SSE clients). Lines arrive from agents in batches.
//
// The hub deliberately keeps NO per-run memory of finished runs. Whether a
// run can still produce lines is a property of the run row, which outlives
// this process; callers pass it to Subscribe. An in-memory "done" set would
// both grow without bound and, worse, come back empty after a restart, so
// every pre-restart run would look live and its log stream would hang.
type LogHub struct {
	dir string

	mu    sync.Mutex
	files map[string]*os.File
	subs  map[string]map[chan string]struct{}
}

func NewLogHub(dir string) (*LogHub, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LogHub{
		dir:   dir,
		files: map[string]*os.File{},
		subs:  map[string]map[chan string]struct{}{},
	}, nil
}

func (h *LogHub) filePath(runID string) string {
	return filepath.Join(h.dir, runID+".log")
}

func (h *LogHub) Append(runID string, lines []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.files[runID]
	if !ok {
		var err error
		f, err = os.OpenFile(h.filePath(runID),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		h.files[runID] = f
	}
	// One write per incoming batch instead of one write per line. The agent
	// also coalesces bursty output, keeping log-heavy runs from generating a
	// stream of tiny filesystem writes without delaying live subscribers.
	var batch strings.Builder
	for _, line := range lines {
		batch.Grow(len(line) + 1)
		batch.WriteString(line)
		batch.WriteByte('\n')
	}
	if batch.Len() > 0 {
		_, _ = f.WriteString(batch.String())
	}
	for _, line := range lines {
		for ch := range h.subs[runID] {
			select {
			case ch <- line:
			default: // slow subscriber: drop rather than block the agent path
			}
		}
	}
}

// Close marks a run's log stream finished and releases the file handle.
func (h *LogHub) Close(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[runID]; ok {
		f.Close()
		delete(h.files, runID)
	}
	for ch := range h.subs[runID] {
		close(ch)
	}
	delete(h.subs, runID)
}

// Subscribe returns the backlog read so far plus a live channel. finished
// (the caller's terminal-state check on the run row) makes the channel nil:
// no more lines are coming, so a follower must not wait for them.
//
// A run whose row is terminal has normally had every line appended already:
// the agent drains its entire log queue before sending a terminal status,
// and the server writes the row before closing the stream. A session that
// dropped mid-flush can still deliver stragglers afterwards; those land in
// the file, so a later read shows them even though no follower was waiting.
func (h *LogHub) Subscribe(runID string, finished bool) ([]string, chan string, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	backlog := h.readBacklog(runID)
	if finished {
		return backlog, nil, func() {}
	}
	ch := make(chan string, 256)
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan string]struct{}{}
	}
	h.subs[runID][ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set, ok := h.subs[runID]; ok {
			if _, live := set[ch]; live {
				delete(set, ch)
				close(ch)
			}
		}
	}
	return backlog, ch, cancel
}

// Forget drops a run's log file and any handle still open on it. Called by
// retention once the run row is gone; a late Append (from an agent that
// reconnected after its run was declared lost) can reopen the file, and
// this is what reclaims that descriptor.
func (h *LogHub) Forget(runIDs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, runID := range runIDs {
		if f, ok := h.files[runID]; ok {
			f.Close()
			delete(h.files, runID)
		}
		_ = os.Remove(h.filePath(runID))
	}
}

// IsOpen reports whether this process is already streaming the run. The
// session loop uses it to decide when a validity check is worth doing: a
// closed file means this is the first batch seen for that run, which is the
// only moment the check costs anything.
func (h *LogHub) IsOpen(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, open := h.files[runID]
	return open
}

// SweepOrphans deletes log files whose run no longer exists. Forget covers
// the ordinary path, but not a crash between deleting the row and deleting
// the file, nor a late Append that recreates a file after Forget already
// ran. Neither is rediscoverable from the run table, so the directory
// itself has to be the source of truth here.
func (h *LogHub) SweepOrphans(known map[string]bool, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		runID, isLog := strings.CutSuffix(name, ".log")
		if entry.IsDir() || !isLog || known[runID] {
			continue
		}
		h.mu.Lock()
		// A run this process is actively streaming is not an orphan, however
		// the caller's snapshot of the run table looked.
		_, open := h.files[runID]
		h.mu.Unlock()
		if open {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(h.dir, name)) == nil {
			removed++
		}
	}
	return removed, nil
}

func (h *LogHub) readBacklog(runID string) []string {
	f, err := os.Open(h.filePath(runID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}
