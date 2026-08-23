package server

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LogHub persists run logs to one file per run and fans live lines out to
// subscribers (SSE clients). Lines arrive from agents in batches.
type LogHub struct {
	dir string

	mu    sync.Mutex
	files map[string]*os.File
	subs  map[string]map[chan string]struct{}
	done  map[string]bool
}

func NewLogHub(dir string) (*LogHub, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LogHub{
		dir:   dir,
		files: map[string]*os.File{},
		subs:  map[string]map[chan string]struct{}{},
		done:  map[string]bool{},
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
	h.done[runID] = true
	for ch := range h.subs[runID] {
		close(ch)
	}
	delete(h.subs, runID)
}

// Subscribe returns the backlog read so far plus a live channel (nil when
// the run has already finished).
func (h *LogHub) Subscribe(runID string) ([]string, chan string, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	backlog := h.readBacklog(runID)
	if h.done[runID] {
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
