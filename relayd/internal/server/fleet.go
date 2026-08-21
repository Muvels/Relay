package server

import (
	"encoding/json"
	"sync"
	"time"

	relayv1 "github.com/matteomarolt/relay/relayd/internal/proto/relayv1"
)

// Fleet tracks live agent sessions. The Store knows every machine that ever
// joined; the Fleet knows who is connected right now and how to reach them.
type Fleet struct {
	mu       sync.RWMutex
	sessions map[string]*Session // machine id → live session
}

type Session struct {
	Machine   *Machine
	Inventory *relayv1.MachineInventory
	Send      chan *relayv1.ServerMessage

	mu            sync.Mutex
	cached        map[string]bool // image tags present on the machine
	lastHeartbeat time.Time
	lastUsage     *relayv1.Heartbeat
}

func (s *Session) MarkCached(tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached[tag] = true
}

// CachedCopy snapshots the cache set for the scheduler.
func (s *Session) CachedCopy() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.cached))
	for k := range s.cached {
		out[k] = true
	}
	return out
}

func NewFleet() *Fleet {
	return &Fleet{sessions: map[string]*Session{}}
}

func (f *Fleet) Attach(m *Machine, inv *relayv1.MachineInventory, cached []string) *Session {
	s := &Session{
		Machine:   m,
		Inventory: inv,
		cached:    map[string]bool{},
		Send:      make(chan *relayv1.ServerMessage, 64),
	}
	for _, tag := range cached {
		s.cached[tag] = true
	}
	s.mu.Lock()
	s.lastHeartbeat = time.Now()
	s.mu.Unlock()

	f.mu.Lock()
	if old, ok := f.sessions[m.ID]; ok {
		close(old.Send) // a reconnect replaces any stale session
	}
	f.sessions[m.ID] = s
	f.mu.Unlock()
	return s
}

// Detach removes the session if (and only if) it is still the current one.
// Returns whether this session was current. A superseded session must not
// trigger detach side effects that would clobber its replacement.
func (f *Fleet) Detach(machineID string, s *Session) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.sessions[machineID]; ok && cur == s {
		delete(f.sessions, machineID)
		close(s.Send)
		return true
	}
	return false
}

func (f *Fleet) Get(machineID string) (*Session, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, ok := f.sessions[machineID]
	return s, ok
}

func (f *Fleet) All() []*Session {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	return out
}

func (s *Session) Beat(hb *relayv1.Heartbeat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHeartbeat = time.Now()
	if hb != nil {
		s.lastUsage = hb
	}
}

func (s *Session) LastHeartbeat() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHeartbeat
}

func (s *Session) Usage() *relayv1.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsage
}

// TrySend queues a message without ever blocking the caller. A full or
// closed channel reports false so the caller can leave the run pending.
func (s *Session) TrySend(msg *relayv1.ServerMessage) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false // channel closed by a replacing session
		}
	}()
	select {
	case s.Send <- msg:
		return true
	default:
		return false
	}
}

// InventoryToMachine copies detected facts onto the Store row for listing.
func InventoryToMachine(m *Machine, inv *relayv1.MachineInventory) {
	m.OS = inv.GetOs()
	m.Arch = inv.GetArch()
	m.CPUCores = int(inv.GetCpuCores())
	m.MemoryMiB = int64(inv.GetMemoryMib())
	m.UnifiedMem = inv.GetUnifiedMemory()
	exec, _ := json.Marshal(inv.GetExecutors())
	m.Executors = string(exec)
	accels, _ := json.Marshal(inv.GetAccelerators())
	m.Accelerators = string(accels)
}
