// Package server implements the relayd control plane: SQLite state, blob
// store, run lifecycle, the agent gRPC service, and the SDK HTTP API.
package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS machines (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    agent_token   TEXT NOT NULL,
    os            TEXT NOT NULL DEFAULT '',
    arch          TEXT NOT NULL DEFAULT '',
    cpu_cores     INTEGER NOT NULL DEFAULT 0,
    memory_mib    INTEGER NOT NULL DEFAULT 0,
    unified_mem   INTEGER NOT NULL DEFAULT 0,
    executors     TEXT NOT NULL DEFAULT '',   -- comma-separated
    accelerators  TEXT NOT NULL DEFAULT '',   -- json
    created_at    INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    app           TEXT NOT NULL,
    function      TEXT NOT NULL,
    state         TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    machine_id    TEXT NOT NULL DEFAULT '',
    spec_json     TEXT NOT NULL,
    result_sha    TEXT NOT NULL DEFAULT '',
    exit_code     INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_state ON runs(state);
CREATE TABLE IF NOT EXISTS join_tokens (
    token       TEXT PRIMARY KEY,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Columns added after the initial schema; applied idempotently.
var migrations = []string{
	`ALTER TABLE runs ADD COLUMN reservation_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'job'`,
	`ALTER TABLE runs ADD COLUMN endpoint TEXT NOT NULL DEFAULT ''`,
	// Newline-delimited {"state":…,"ts":…} entries, appended per transition
	// and feeds the dashboard's execution timeline.
	`ALTER TABLE runs ADD COLUMN events_json TEXT NOT NULL DEFAULT ''`,
}

func OpenStore(path string) (*Store, error) {
	// WAL + NORMAL avoids a physical sync for every small state transition.
	// SQLite still keeps the database consistent across crashes; only the most
	// recent transactions can roll back after a sudden OS crash or power loss.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc sqlite is safest with a single writer connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migration %q: %w", m, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%x%s", prefix, time.Now().Unix(), hex.EncodeToString(b))
}

func NewSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "rly_" + hex.EncodeToString(b)
}

// ------------------------------------------------------------ settings

// EnsureSetting returns the stored value, creating it with generate() once.
func (s *Store) EnsureSetting(key string, generate func() string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	v = generate()
	_, err = s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)`, key, v)
	return v, err
}

// ------------------------------------------------------------ join tokens

func (s *Store) CreateJoinToken(ttl time.Duration) (string, error) {
	token := NewSecret()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO join_tokens(token, created_at, expires_at) VALUES(?,?,?)`,
		token, now, now+int64(ttl.Seconds()),
	)
	return token, err
}

// ConsumeJoinToken atomically validates and burns a one-time join token.
func (s *Store) ConsumeJoinToken(token string) error {
	res, err := s.db.Exec(
		`UPDATE join_tokens SET used = 1
		 WHERE token = ? AND used = 0 AND expires_at > ?`,
		token, time.Now().Unix(),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("join token invalid, expired, or already used. Mint a fresh one with `relay connect`")
	}
	return nil
}

// ------------------------------------------------------------ machines

type Machine struct {
	ID           string
	Name         string
	AgentToken   string
	OS           string
	Arch         string
	CPUCores     int
	MemoryMiB    int64
	UnifiedMem   bool
	Executors    string
	Accelerators string // json blob, opaque to the store
	LastSeenAt   time.Time
}

func (s *Store) CreateMachine(name string) (*Machine, error) {
	m := &Machine{ID: newID("m"), Name: name, AgentToken: NewSecret()}
	// Suffix the name on collision rather than failing the join.
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", name, i+1)
		}
		_, err := s.db.Exec(
			`INSERT INTO machines(id, name, agent_token, created_at) VALUES(?,?,?,?)`,
			m.ID, candidate, m.AgentToken, time.Now().Unix(),
		)
		if err == nil {
			m.Name = candidate
			return m, nil
		}
		if i > 50 {
			return nil, fmt.Errorf("create machine: %w", err)
		}
	}
}

func (s *Store) MachineByToken(token string) (*Machine, error) {
	return s.machineWhere(`agent_token = ?`, token)
}

func (s *Store) MachineByID(id string) (*Machine, error) {
	return s.machineWhere(`id = ?`, id)
}

func (s *Store) machineWhere(cond string, arg any) (*Machine, error) {
	m := &Machine{}
	var unified int
	var lastSeen int64
	err := s.db.QueryRow(
		`SELECT id, name, agent_token, os, arch, cpu_cores, memory_mib,
		        unified_mem, executors, accelerators, last_seen_at
		 FROM machines WHERE `+cond, arg,
	).Scan(&m.ID, &m.Name, &m.AgentToken, &m.OS, &m.Arch, &m.CPUCores,
		&m.MemoryMiB, &unified, &m.Executors, &m.Accelerators, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.UnifiedMem = unified == 1
	m.LastSeenAt = time.Unix(lastSeen, 0)
	return m, nil
}

func (s *Store) UpdateMachineInventory(m *Machine) error {
	unified := 0
	if m.UnifiedMem {
		unified = 1
	}
	_, err := s.db.Exec(
		`UPDATE machines SET os=?, arch=?, cpu_cores=?, memory_mib=?,
		        unified_mem=?, executors=?, accelerators=?, last_seen_at=?
		 WHERE id=?`,
		m.OS, m.Arch, m.CPUCores, m.MemoryMiB, unified, m.Executors,
		m.Accelerators, time.Now().Unix(), m.ID,
	)
	return err
}

func (s *Store) TouchMachine(id string) error {
	_, err := s.db.Exec(
		`UPDATE machines SET last_seen_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) ListMachines() ([]*Machine, error) {
	rows, err := s.db.Query(
		`SELECT id, name, agent_token, os, arch, cpu_cores, memory_mib,
		        unified_mem, executors, accelerators, last_seen_at
		 FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Machine
	for rows.Next() {
		m := &Machine{}
		var unified int
		var lastSeen int64
		if err := rows.Scan(&m.ID, &m.Name, &m.AgentToken, &m.OS, &m.Arch,
			&m.CPUCores, &m.MemoryMiB, &unified, &m.Executors,
			&m.Accelerators, &lastSeen); err != nil {
			return nil, err
		}
		m.UnifiedMem = unified == 1
		m.LastSeenAt = time.Unix(lastSeen, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ runs

type Run struct {
	ID              string
	App             string
	Function        string
	Kind            string
	State           string
	Detail          string
	MachineID       string
	Endpoint        string
	SpecJSON        string
	ReservationJSON string
	EventsJSON      string
	ResultSha       string
	ExitCode        int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type RunEvent struct {
	State string `json:"state"`
	TsMs  int64  `json:"ts"`
}

// runEvent returns one append-only timeline entry. Callers include it in the
// same SQL statement as the state change so one transition needs one commit.
func runEvent(state string) string {
	return fmt.Sprintf(`{"state":%q,"ts":%d}`+"\n", state, time.Now().UnixMilli())
}

// ParseRunEvents decodes the newline-delimited event log.
func ParseRunEvents(eventsJSON string) []RunEvent {
	var out []RunEvent
	for _, line := range strings.Split(eventsJSON, "\n") {
		if line == "" {
			continue
		}
		var ev RunEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.State != "" {
			out = append(out, ev)
		}
	}
	return out
}

// SetRunEndpoint records where a service is reachable.
func (s *Store) SetRunEndpoint(id, endpoint string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET endpoint=?, updated_at=? WHERE id=? AND endpoint<>?`,
		endpoint, time.Now().Unix(), id, endpoint)
	return err
}

// ActiveServiceRuns finds live service runs for an app+function (used by
// deploy to replace the previous generation).
func (s *Store) ActiveServiceRuns(app, function string) ([]*Run, error) {
	runs, err := s.ListRuns(5000, []string{"pending", "assigned", "building", "running"})
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, r := range runs {
		if r.Kind == "service" && r.App == app && r.Function == function {
			out = append(out, r)
		}
	}
	return out, nil
}

// SetRunAssigned atomically records a placement decision; false when the
// run left `pending` in the meantime (e.g. canceled).
func (s *Store) SetRunAssigned(id, machineID, detail, reservationJSON string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE runs SET state='assigned', detail=?, machine_id=?,
		        reservation_json=?, events_json=events_json || ?, updated_at=?
		 WHERE id=? AND state='pending'`,
		detail, machineID, reservationJSON, runEvent("assigned"), time.Now().Unix(), id,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetRunDetail updates only the human-readable detail (queue explanations).
func (s *Store) SetRunDetail(id, detail string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET detail=?, updated_at=?
		 WHERE id=? AND state='pending' AND detail<>?`,
		detail, time.Now().Unix(), id, detail)
	return err
}

func (s *Store) CreateRun(app, function, kind, specJSON string) (*Run, error) {
	if kind == "" {
		kind = "job"
	}
	r := &Run{ID: newID("run"), App: app, Function: function, Kind: kind,
		State: "pending", SpecJSON: specJSON}
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO runs(id, app, function, kind, state, spec_json, events_json,
		                  created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, app, function, kind, r.State, specJSON, runEvent("pending"), now, now,
	)
	return r, err
}

func (s *Store) GetRun(id string) (*Run, error) {
	r := &Run{}
	var created, updated int64
	err := s.db.QueryRow(
		`SELECT id, app, function, kind, endpoint, state, detail, machine_id,
		        spec_json, reservation_json, events_json, result_sha,
		        exit_code, created_at, updated_at
		 FROM runs WHERE id = ?`, id,
	).Scan(&r.ID, &r.App, &r.Function, &r.Kind, &r.Endpoint, &r.State,
		&r.Detail, &r.MachineID, &r.SpecJSON, &r.ReservationJSON,
		&r.EventsJSON, &r.ResultSha, &r.ExitCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return r, nil
}

var terminalStates = []string{"succeeded", "failed", "error", "canceled", "lost"}

// TransitionRun conditionally moves a run between states. `from` restricts
// which current states permit the transition (nil = any NON-terminal state;
// terminal states are never overwritable). When ownerMachineID is set, the
// row must still belong to that machine at write time. A stale update from
// a machine that lost the run cannot land. Returns whether a row changed,
// callers treat false as "someone else settled it first", not an error.
func (s *Store) TransitionRun(id string, from []string, state, detail, machineID, resultSha string, exitCode int) (bool, error) {
	return s.transitionRun(id, from, "", state, detail, machineID, resultSha, exitCode)
}

func (s *Store) TransitionRunOwned(id string, from []string, ownerMachineID, state, detail, resultSha string, exitCode int) (bool, error) {
	return s.transitionRun(id, from, ownerMachineID, state, detail, ownerMachineID, resultSha, exitCode)
}

func (s *Store) transitionRun(id string, from []string, ownerMachineID, state, detail, machineID, resultSha string, exitCode int) (bool, error) {
	var cond string
	args := []any{state, detail, machineID, machineID, resultSha, resultSha,
		exitCode, runEvent(state), time.Now().Unix(), id}
	if from == nil {
		cond = `state NOT IN (` + placeholders(len(terminalStates)) + `)`
		for _, t := range terminalStates {
			args = append(args, t)
		}
	} else {
		cond = `state IN (` + placeholders(len(from)) + `)`
		for _, f := range from {
			args = append(args, f)
		}
	}
	if ownerMachineID != "" {
		cond += ` AND machine_id = ?`
		args = append(args, ownerMachineID)
	}
	res, err := s.db.Exec(
		`UPDATE runs SET state=?, detail=?,
		        machine_id = CASE WHEN ? != '' THEN ? ELSE machine_id END,
		        result_sha = CASE WHEN ? != '' THEN ? ELSE result_sha END,
		        exit_code=?, events_json=events_json || ?, updated_at=?
		 WHERE id=? AND `+cond,
		args...,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevertRunToPending undoes an assignment that never reached the machine.
func (s *Store) RevertRunToPending(id, machineID string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE runs SET state='pending', detail='requeued: machine dropped',
		        machine_id='', reservation_json='',
		        events_json=events_json || ?, updated_at=?
		 WHERE id=? AND state='assigned' AND machine_id=?`,
		runEvent("pending"), time.Now().Unix(), id, machineID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) ListRuns(limit int, states []string) ([]*Run, error) {
	q := `SELECT id, app, function, kind, endpoint, state, detail, machine_id,
	             spec_json, reservation_json, events_json, result_sha,
	             exit_code, created_at, updated_at
	      FROM runs`
	args := []any{}
	if len(states) > 0 {
		q += ` WHERE state IN (` + placeholders(len(states)) + `)`
		for _, st := range states {
			args = append(args, st)
		}
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r := &Run{}
		var created, updated int64
		if err := rows.Scan(&r.ID, &r.App, &r.Function, &r.Kind, &r.Endpoint,
			&r.State, &r.Detail, &r.MachineID, &r.SpecJSON,
			&r.ReservationJSON, &r.EventsJSON, &r.ResultSha, &r.ExitCode,
			&created, &updated); err != nil {
			return nil, err
		}
		r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ retention

// TerminalRunsBefore lists settled runs last touched before cutoff, oldest
// first, so a sweep makes progress from the far end of history. Live runs
// are never returned: a run still on a machine outlives any TTL.
func (s *Store) TerminalRunsBefore(cutoff time.Time, limit int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM runs
		 WHERE state IN (`+placeholders(len(terminalStates))+`) AND updated_at < ?
		 ORDER BY updated_at LIMIT ?`,
		append(terminalArgs(), cutoff.Unix(), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func terminalArgs() []any {
	args := make([]any, 0, len(terminalStates))
	for _, t := range terminalStates {
		args = append(args, t)
	}
	return args
}

// DeleteRuns removes run rows by id. One statement per call keeps the WAL
// churn of a sweep proportional to sweeps, not to rows.
func (s *Store) DeleteRuns(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(
		`DELETE FROM runs WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	return err
}

// ReferencedBlobs collects every blob id still reachable from stored state:
// the code bundle and call payload of each surviving run and schedule, plus
// each stored result. Anything not in this set is unreachable by any API
// the SDK can call. Errors are returned rather than swallowed, because a
// partial answer here would authorize deleting live data.
func (s *Store) ReferencedBlobs() (map[string]bool, error) {
	keep := map[string]bool{}
	// A spec that will not parse is a spec whose blobs cannot be enumerated.
	// Skipping it would quietly authorize deleting inputs something still
	// needs, so the whole scan fails instead: destructive GC fails closed.
	addSpec := func(owner, specJSON string) error {
		var spec struct {
			BundleSha string `json:"bundle_sha256"`
			ArgsSha   string `json:"args_sha256"`
		}
		if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
			return fmt.Errorf("unreadable spec on %s: %w", owner, err)
		}
		if spec.BundleSha != "" {
			keep[spec.BundleSha] = true
		}
		if spec.ArgsSha != "" {
			keep[spec.ArgsSha] = true
		}
		return nil
	}
	runRows, err := s.db.Query(`SELECT id, spec_json, result_sha FROM runs`)
	if err != nil {
		return nil, err
	}
	defer runRows.Close()
	for runRows.Next() {
		var id, specJSON, resultSha string
		if err := runRows.Scan(&id, &specJSON, &resultSha); err != nil {
			return nil, err
		}
		if err := addSpec("run "+id, specJSON); err != nil {
			return nil, err
		}
		if resultSha != "" {
			keep[resultSha] = true
		}
	}
	if err := runRows.Err(); err != nil {
		return nil, err
	}
	// Schedules hold a spec that has not run yet; its bundle must survive
	// however long the cron sits idle between firings.
	schedRows, err := s.db.Query(`SELECT id, spec_json FROM schedules`)
	if err != nil {
		return nil, err
	}
	defer schedRows.Close()
	for schedRows.Next() {
		var id, specJSON string
		if err := schedRows.Scan(&id, &specJSON); err != nil {
			return nil, err
		}
		if err := addSpec("schedule "+id, specJSON); err != nil {
			return nil, err
		}
	}
	return keep, schedRows.Err()
}

// ExistingRunIDs is the id set of every stored run. Retention uses it to
// find log files whose run is gone, which is the only way an orphaned log
// can ever be rediscovered: nothing else records that the file exists.
func (s *Store) ExistingRunIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += "?"
	}
	return s
}
