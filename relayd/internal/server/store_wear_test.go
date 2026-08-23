package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openWearTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func totalChanges(t *testing.T, store *Store) int64 {
	t.Helper()
	var n int64
	if err := store.db.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoreUsesWALNormalSynchronous(t *testing.T) {
	store := openWearTestStore(t)
	var journal string
	var synchronous int
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || synchronous != 1 {
		t.Fatalf("got journal=%s synchronous=%d, want wal/1", journal, synchronous)
	}
}

func TestUnchangedPendingDetailDoesNotWrite(t *testing.T) {
	store := openWearTestStore(t)
	run, err := store.CreateRun("app", "fn", "job", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunDetail(run.ID, "waiting for capacity"); err != nil {
		t.Fatal(err)
	}
	afterFirst := totalChanges(t, store)
	if err := store.SetRunDetail(run.ID, "waiting for capacity"); err != nil {
		t.Fatal(err)
	}
	if got := totalChanges(t, store); got != afterFirst {
		t.Fatalf("unchanged detail wrote a row: changes %d -> %d", afterFirst, got)
	}
}

func TestRunTransitionAppendsEventInSameUpdate(t *testing.T) {
	store := openWearTestStore(t)
	run, err := store.CreateRun("app", "fn", "job", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	before := totalChanges(t, store)
	changed, err := store.TransitionRun(run.ID, []string{"pending"},
		"building", "", "machine", "", 0)
	if err != nil || !changed {
		t.Fatalf("transition changed=%v err=%v", changed, err)
	}
	if delta := totalChanges(t, store) - before; delta != 1 {
		t.Fatalf("transition changed %d rows, want one combined update", delta)
	}
	got, err := store.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := ParseRunEvents(got.EventsJSON)
	if len(events) != 2 || events[0].State != "pending" || events[1].State != "building" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestWriteFileIfChangedLeavesMtimeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	data := []byte("secret\n")
	if err := writeFileIfChanged(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_700_000_000, 123_000_000)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeFileIfChanged(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("unchanged file mtime changed: got %s want %s", info.ModTime(), old)
	}
}
