package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The submit path proves a blob exists and writes the referencing row a
// moment later. A sweep that scanned reachability before that row existed
// must not delete the blob in between. Mtime cannot carry this: a
// content-addressed store does not rewrite content it already has, so a
// reused bundle keeps its original, arbitrarily old timestamp.
func TestSweepSparesABlobASubmitIsAboutToReference(t *testing.T) {
	_, _, blobs := newRetentionFixture(t)
	sha := putBlobForTest(t, blobs, "reused bundle", 90*24*time.Hour)

	// What submitRun/deployService/upsertSchedule all do first.
	if !blobs.Has(sha) {
		t.Fatal("blob should be present")
	}

	removed, err := blobs.Sweep(map[string]bool{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || !blobs.Has(sha) {
		t.Fatal("swept a blob whose reference was being created")
	}
}

// The lease is a grace window, not a permanent pin: once it ages out, an
// unreferenced blob is collectable again.
func TestExpiredLeaseStopsSparingABlob(t *testing.T) {
	_, _, blobs := newRetentionFixture(t)
	sha := putBlobForTest(t, blobs, "abandoned", 90*24*time.Hour)
	if !blobs.Has(sha) {
		t.Fatal("blob should be present")
	}

	// A sweep whose cutoff is in the future: every lease has aged out.
	removed, err := blobs.Sweep(map[string]bool{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want the unreferenced blob collected", removed)
	}
}

// Destructive GC must fail closed: a spec it cannot read is a spec whose
// blobs it cannot enumerate.
func TestUnreadableSpecAbortsTheBlobSweep(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	good := putBlobForTest(t, blobs, "still needed", 90*24*time.Hour)

	run, err := store.CreateRun("app", "fn", "job", specWithBlobs(t, good, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`UPDATE runs SET spec_json='{not json' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ReferencedBlobs(); err == nil {
		t.Fatal("reference scan accepted an unreadable spec")
	}
	if _, removed := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour}, time.Now()); removed != 0 {
		t.Fatalf("deleted %d blobs despite an unreadable reference set", removed)
	}
	if !blobs.Has(good) {
		t.Fatal("a blob was deleted while reachability was unknown")
	}
}

// Forget covers the ordinary path. A crash between deleting the row and
// deleting the file, or a late Append that recreates one, leaves a file the
// run table can never point at again.
func TestOrphanedLogFilesAreReclaimed(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)

	orphan := filepath.Join(logs.dir, "run_vanished.log")
	if err := os.WriteFile(orphan, []byte("stranded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	kept, err := store.CreateRun("app", "fn", "job", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	logs.Append(kept.ID, []string{"live"})
	logs.Close(kept.ID)
	keptPath := logs.filePath(kept.ID)
	if err := os.Chtimes(keptPath, old, old); err != nil {
		t.Fatal(err)
	}

	sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour}, time.Now())

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned log survived: %v", err)
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Fatalf("log of a run that still exists was deleted: %v", err)
	}
}

// A run this process is streaming right now is not an orphan, whatever the
// caller's snapshot of the run table said.
func TestOrphanSweepSkipsAnOpenLogStream(t *testing.T) {
	_, logs, _ := newRetentionFixture(t)
	logs.Append("run_streaming", []string{"first line"})
	old := time.Now().Add(-24 * time.Hour)
	path := logs.filePath("run_streaming")
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := logs.SweepOrphans(map[string]bool{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatal("swept the log of a run being streamed right now")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("open stream's file was deleted: %v", err)
	}
}

// Every terminal transition must close the log stream; a follower on a run
// that settles is otherwise stranded forever. The corrupt-spec path in
// DispatchPending was the one that did not.
func TestCorruptSpecTransitionClosesFollowers(t *testing.T) {
	store, logs, _ := newRetentionFixture(t)
	run, err := store.CreateRun("app", "fn", "job", `{"bundle_sha256": TRUNCATED`)
	if err != nil {
		t.Fatal(err)
	}
	_, live, cancel := logs.Subscribe(run.ID, false)
	defer cancel()
	if live == nil {
		t.Fatal("expected a live channel for a pending run")
	}

	NewRuns(store, NewFleet(), logs).DispatchPending()

	settled, err := store.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "error" {
		t.Fatalf("state is %q, want error", settled.State)
	}
	if !strings.Contains(settled.Detail, "corrupt spec") {
		t.Fatalf("detail is %q", settled.Detail)
	}
	select {
	case _, open := <-live:
		if open {
			t.Fatal("channel delivered a line instead of closing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower was never closed: it would hang forever")
	}
}
