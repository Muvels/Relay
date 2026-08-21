package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Minimal 5-field cron: minute hour day-of-month month day-of-week.
// Each field: "*", value, range "A-B", step "*/N" or "A-B/N", comma lists.
// Steps count from the range's lower bound. For example, dom "*/2" = 1,3,5,… because the
// standard semantics for 1-based fields). Weekday accepts 7 as Sunday.
// When BOTH dom and dow are restricted, either matching fires (Vixie).
//
// Delivery is best-effort at-most-once per matching minute: no catch-up
// after server downtime, server-local timezone (documented in docs/).

type cronField struct {
	any   bool
	allow map[int]bool
}

type CronExpr struct {
	fields [5]cronField
	src    string
}

var cronRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

func ParseCron(expr string) (*CronExpr, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf(
			"cron %q needs 5 fields (minute hour day month weekday)", expr)
	}
	out := &CronExpr{src: expr}
	for i, part := range parts {
		f, err := parseCronField(part, cronRanges[i][0], cronRanges[i][1], i == 4)
		if err != nil {
			return nil, fmt.Errorf("cron %q field %d: %w", expr, i+1, err)
		}
		out.fields[i] = f
	}
	return out, nil
}

func parseCronField(s string, lo, hi int, isDow bool) (cronField, error) {
	if s == "*" {
		return cronField{any: true}, nil
	}
	max := hi
	if isDow {
		max = 7 // 7 ≡ Sunday
	}
	allow := map[int]bool{}
	for _, tok := range strings.Split(s, ",") {
		rangePart, stepPart, hasStep := strings.Cut(tok, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepPart)
			if err != nil || n <= 0 {
				return cronField{}, fmt.Errorf("bad step in %q", tok)
			}
			step = n
		}
		var start, end int
		switch {
		case rangePart == "*":
			start, end = lo, hi
		default:
			if a, b, isRange := strings.Cut(rangePart, "-"); isRange {
				var err1, err2 error
				start, err1 = strconv.Atoi(a)
				end, err2 = strconv.Atoi(b)
				if err1 != nil || err2 != nil {
					return cronField{}, fmt.Errorf("bad range %q", tok)
				}
			} else {
				v, err := strconv.Atoi(rangePart)
				if err != nil {
					return cronField{}, fmt.Errorf("bad value %q", tok)
				}
				start, end = v, v
				if hasStep { // "N/step" means N..hi per Vixie
					end = hi
				}
			}
		}
		if start > end || start < lo || end > max {
			return cronField{}, fmt.Errorf(
				"bad range %q (allowed %d-%d)", tok, lo, max)
		}
		for v := start; v <= end; v += step {
			val := v
			if isDow && val == 7 {
				val = 0
			}
			allow[val] = true
		}
	}
	return cronField{allow: allow}, nil
}

func (f cronField) matches(v int) bool {
	return f.any || f.allow[v]
}

func (f cronField) restricted() bool { return !f.any }

// Matches reports whether the minute containing t fires.
func (c *CronExpr) Matches(t time.Time) bool {
	min, hour := t.Minute(), t.Hour()
	dom, mon, dow := t.Day(), int(t.Month()), int(t.Weekday())
	if !c.fields[0].matches(min) || !c.fields[1].matches(hour) ||
		!c.fields[3].matches(mon) {
		return false
	}
	domF, dowF := c.fields[2], c.fields[4]
	if domF.restricted() && dowF.restricted() {
		return domF.matches(dom) || dowF.matches(dow) // Vixie either-matches
	}
	return domF.matches(dom) && dowF.matches(dow)
}

func (c *CronExpr) String() string { return c.src }

// ---------------------------------------------------------------- storage

type Schedule struct {
	ID        string
	App       string
	Function  string
	Cron      string
	SpecJSON  string
	LastRunAt time.Time
}

func (s *Store) ensureScheduleTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schedules (
		id TEXT PRIMARY KEY,
		app TEXT NOT NULL, function TEXT NOT NULL,
		cron TEXT NOT NULL, spec_json TEXT NOT NULL,
		last_run_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		UNIQUE(app, function)
	)`)
	return err
}

func (s *Store) UpsertSchedule(app, function, cron, specJSON string) (*Schedule, error) {
	sc := &Schedule{ID: newID("sch"), App: app, Function: function,
		Cron: cron, SpecJSON: specJSON}
	_, err := s.db.Exec(
		`INSERT INTO schedules(id, app, function, cron, spec_json, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(app, function) DO UPDATE SET
		   cron=excluded.cron, spec_json=excluded.spec_json`,
		sc.ID, app, function, cron, specJSON, time.Now().Unix())
	return sc, err
}

func (s *Store) DeleteSchedule(app, function string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE app=? AND function=?`,
		app, function)
	return err
}

func (s *Store) ListSchedules() ([]*Schedule, error) {
	rows, err := s.db.Query(
		`SELECT id, app, function, cron, spec_json, last_run_at
		 FROM schedules ORDER BY app, function`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		sc := &Schedule{}
		var last int64
		if err := rows.Scan(&sc.ID, &sc.App, &sc.Function, &sc.Cron,
			&sc.SpecJSON, &last); err != nil {
			return nil, err
		}
		sc.LastRunAt = time.Unix(last, 0)
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) MarkScheduleRun(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_run_at=? WHERE id=?`,
		at.Unix(), id)
	return err
}

// ---------------------------------------------------------------- ticker

// StartCron fires due schedules once per matching minute.
func StartCron(ctx context.Context, store *Store, runs *Runs) {
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				fireDue(store, runs, now)
			}
		}
	}()
}

func fireDue(store *Store, runs *Runs, now time.Time) {
	schedules, err := store.ListSchedules()
	if err != nil {
		return
	}
	minute := now.Truncate(time.Minute)
	for _, sc := range schedules {
		expr, err := ParseCron(sc.Cron)
		if err != nil || !expr.Matches(minute) {
			continue
		}
		if !sc.LastRunAt.Before(minute) {
			continue // already fired this minute
		}
		var spec RunSpecJSON
		if json.Unmarshal([]byte(sc.SpecJSON), &spec) != nil {
			continue
		}
		if _, err := runs.Submit(&spec); err != nil {
			slog.Error("cron submit failed", "schedule", sc.App+"."+sc.Function, "err", err)
			continue
		}
		if err := store.MarkScheduleRun(sc.ID, minute); err != nil {
			// A failed mark may cause a duplicate on the next tick, so log it loudly.
			slog.Error("cron mark failed. The next tick may fire this "+
				"schedule again", "schedule", sc.App+"."+sc.Function, "err", err)
		}
		slog.Info("cron fired", "schedule", sc.App+"."+sc.Function, "cron", sc.Cron)
	}
}
