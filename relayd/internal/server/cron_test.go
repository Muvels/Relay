package server

import (
	"testing"
	"time"
)

func at(min, hour, day int, month time.Month, year int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

func TestCronBasics(t *testing.T) {
	cases := []struct {
		expr  string
		time  time.Time
		match bool
	}{
		{"* * * * *", at(17, 9, 16, time.August, 2026), true},
		{"*/15 * * * *", at(30, 9, 16, time.August, 2026), true},
		{"*/15 * * * *", at(31, 9, 16, time.August, 2026), false},
		{"0 3 * * *", at(0, 3, 16, time.August, 2026), true},
		{"0 3 * * *", at(0, 4, 16, time.August, 2026), false},
		{"30 6 1 * *", at(30, 6, 1, time.September, 2026), true},
		{"30 6 1 * *", at(30, 6, 2, time.September, 2026), false},
		// 2026-08-16 is a Sunday (weekday 0).
		{"0 12 * * 0", at(0, 12, 16, time.August, 2026), true},
		{"0 12 * * 1-5", at(0, 12, 16, time.August, 2026), false},
		{"0 0 * 12 *", at(0, 0, 25, time.December, 2026), true},
		{"5,35 * * * *", at(35, 14, 16, time.August, 2026), true},
		{"5,35 * * * *", at(36, 14, 16, time.August, 2026), false},
		// Steps count from the range's lower bound: dom */2 = 1,3,5,…
		{"0 0 */2 * *", at(0, 0, 3, time.August, 2026), true},
		{"0 0 */2 * *", at(0, 0, 4, time.August, 2026), false},
		{"10-30/10 * * * *", at(20, 8, 16, time.August, 2026), true},
		{"10-30/10 * * * *", at(25, 8, 16, time.August, 2026), false},
		// dow 7 ≡ Sunday (2026-08-16 is a Sunday).
		{"0 12 * * 7", at(0, 12, 16, time.August, 2026), true},
		// Vixie: restricted dom AND dow → either fires.
		{"0 0 1 * 0", at(0, 0, 16, time.August, 2026), true},  // Sunday, not the 1st
		{"0 0 1 * 3", at(0, 0, 16, time.August, 2026), false}, // Sunday, not the 1st, not Wed
	}
	for _, c := range cases {
		expr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		if got := expr.Matches(c.time); got != c.match {
			t.Errorf("%q at %s: got %v want %v", c.expr, c.time, got, c.match)
		}
	}
}

func TestFireDueSubmitsOncePerMinute(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ensureScheduleTable(); err != nil {
		t.Fatal(err)
	}
	logs, _ := NewLogHub(dir + "/logs")
	runs := NewRuns(store, NewFleet(), logs)

	spec := `{"app":"a","function":"f","entry_file":"a","image_tag":"t",` +
		`"image_dockerfile":"FROM x","bundle_sha256":"b","args_sha256":"c"}`
	if _, err := store.UpsertSchedule("a", "f", "*/5 * * * *", spec); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 16, 10, 15, 30, 0, time.UTC) // :15 matches */5
	fireDue(store, runs, now)
	fireDue(store, runs, now.Add(10*time.Second)) // same minute → no double fire

	pending, err := store.ListRuns(10, []string{"pending"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("want exactly 1 fired run, got %d", len(pending))
	}
	fireDue(store, runs, now.Add(5*time.Minute)) // :20 → fires again
	pending, _ = store.ListRuns(10, []string{"pending"})
	if len(pending) != 2 {
		t.Fatalf("want 2 after next window, got %d", len(pending))
	}
	fireDue(store, runs, now.Add(6*time.Minute)) // :21 → no match
	pending, _ = store.ListRuns(10, []string{"pending"})
	if len(pending) != 2 {
		t.Fatalf("want still 2, got %d", len(pending))
	}
}

func TestCronRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "61 * * * *", "* 25 * * *",
		"a * * * *", "*/0 * * * *", "9-3 * * * *", "* * 32 * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}
