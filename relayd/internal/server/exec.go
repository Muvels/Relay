package server

import (
	"sync"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// ExecHub routes host-command output chunks from agent sessions to the
// HTTP request waiting on them.
type ExecHub struct {
	mu      sync.Mutex
	waiters map[string]chan *relayv1.ExecOutput
}

func NewExecHub() *ExecHub {
	return &ExecHub{waiters: map[string]chan *relayv1.ExecOutput{}}
}

func (h *ExecHub) Register(execID string) chan *relayv1.ExecOutput {
	ch := make(chan *relayv1.ExecOutput, 64)
	h.mu.Lock()
	h.waiters[execID] = ch
	h.mu.Unlock()
	return ch
}

func (h *ExecHub) Unregister(execID string) {
	h.mu.Lock()
	delete(h.waiters, execID)
	h.mu.Unlock()
}

func (h *ExecHub) Deliver(out *relayv1.ExecOutput) {
	h.mu.Lock()
	ch := h.waiters[out.GetExecId()]
	h.mu.Unlock()
	if ch != nil {
		select {
		case ch <- out:
		default: // waiter is gone or wedged; drop
		}
	}
}
