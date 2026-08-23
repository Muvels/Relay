package server

import (
	"context"
	"log/slog"
	"time"
)

// Retention bounds the three things that otherwise grow for as long as the
// server runs: settled run rows, their log files, and the blobs those runs
// were the last reference to. A control plane meant to stay up for months
// needs an upper bound on all three, not just a small constant factor.
//
// Deliberately NOT here: VACUUM. Reclaiming SQLite's freed pages would
// rewrite the whole database on a schedule, which costs far more flash
// writes than the free pages cost bytes. Freed pages are reused in place.
type RetentionConfig struct {
	// RunTTL is how long a settled run (and its logs) is kept after its last
	// state change. Zero keeps everything forever.
	RunTTL time.Duration
	// Interval between sweeps.
	Interval time.Duration
	// BlobGrace spares blobs written this recently even when nothing
	// references them yet, covering the SDK's upload-then-submit window.
	BlobGrace time.Duration
	// MaxDeletesPerSweep bounds one sweep so a first run against a long
	// history cannot hold the single writer connection for minutes.
	MaxDeletesPerSweep int
}

// DefaultRunTTL keeps a month of history: long enough that `relay ps` still
// answers "what did I run last month", short enough to bound the store.
const DefaultRunTTL = 30 * 24 * time.Hour

func (c RetentionConfig) withDefaults() RetentionConfig {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.BlobGrace <= 0 {
		c.BlobGrace = time.Hour
	}
	if c.MaxDeletesPerSweep <= 0 {
		c.MaxDeletesPerSweep = 5000
	}
	return c
}

func (c RetentionConfig) describe() string {
	if c.RunTTL <= 0 {
		return "off (runs, logs and blobs are kept forever)"
	}
	return "runs and logs older than " + c.RunTTL.String() +
		", then unreferenced blobs"
}

// StartRetention sweeps on an interval until ctx ends. The first sweep is
// delayed by one interval: a server that restarts often must not pay the
// sweep cost on every boot, and nothing is urgent at startup.
func StartRetention(ctx context.Context, store *Store, logs *LogHub, blobs *BlobStore, cfg RetentionConfig) {
	cfg = cfg.withDefaults()
	if cfg.RunTTL <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepRetention(store, logs, blobs, cfg, time.Now())
			}
		}
	}()
}

// sweepRetention deletes in dependency order: rows first, then the log files
// of exactly those rows, then blobs that lost their last reference. Reversing
// it would leave a run row pointing at a result blob that no longer exists.
func sweepRetention(store *Store, logs *LogHub, blobs *BlobStore, cfg RetentionConfig, now time.Time) (runs, blobsRemoved int) {
	cfg = cfg.withDefaults()
	// Honour "keep forever" here as well as in the scheduler: a zero TTL
	// reaching this far would otherwise read as "everything settled is
	// expired" and delete the entire history.
	if cfg.RunTTL <= 0 {
		return 0, 0
	}
	cutoff := now.Add(-cfg.RunTTL)

	const batch = 500
	for runs < cfg.MaxDeletesPerSweep {
		ids, err := store.TerminalRunsBefore(cutoff, min(batch, cfg.MaxDeletesPerSweep-runs))
		if err != nil {
			slog.Error("retention: list expired runs", "err", err)
			return runs, 0
		}
		if len(ids) == 0 {
			break
		}
		if err := store.DeleteRuns(ids); err != nil {
			slog.Error("retention: delete runs", "err", err)
			return runs, 0
		}
		logs.Forget(ids)
		runs += len(ids)
	}

	// Log files whose run row is gone: the ordinary path already removed
	// them, so anything found here survived a crash mid-delete or was
	// recreated afterwards by a straggler batch. Either way the run table
	// no longer names it, so only a directory scan can find it.
	if known, err := store.ExistingRunIDs(); err != nil {
		slog.Error("retention: run id scan", "err", err)
	} else if orphans, err := logs.SweepOrphans(known, now.Add(-cfg.BlobGrace)); err != nil {
		slog.Error("retention: orphan log sweep", "err", err)
	} else if orphans > 0 {
		slog.Info("retention removed orphaned logs", "files", orphans)
	}

	// Reachability is recomputed from scratch every sweep rather than
	// refcounted, because blobs are shared by construction: two runs of
	// unchanged code name the same bundle.
	keep, err := store.ReferencedBlobs()
	if err != nil {
		// Fails closed: an unreadable spec means an unknown reference set,
		// and deleting against an unknown set is how data disappears.
		slog.Error("retention: blob reference scan; skipping blob sweep", "err", err)
		return runs, 0
	}
	blobsRemoved, err = blobs.Sweep(keep, now.Add(-cfg.BlobGrace))
	if err != nil {
		slog.Error("retention: blob sweep", "err", err)
	}
	if runs > 0 || blobsRemoved > 0 {
		slog.Info("retention swept", "runs", runs, "blobs", blobsRemoved,
			"older_than", cfg.RunTTL.String())
	}
	return runs, blobsRemoved
}
