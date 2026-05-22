package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"flint/internal/app"
	"flint/internal/store"
)

// TestBackup_RestoreRoundTrip proves the M3 spec scenario "portable
// backup artifact + lossless round-trip": a backup taken from a populated
// store can be restored into an empty store and produce identical rows.
func TestBackup_RestoreRoundTrip(t *testing.T) {
	src := openTempStoreT(t)
	srcSvc := app.NewService(src, nil)
	if err := srcSvc.Init(); err != nil {
		t.Fatalf("init src: %v", err)
	}

	// Seed some realistic content across the M1/M2/M3 surface so the
	// round-trip exercises columns added by every prior migration.
	if _, err := srcSvc.SaveWithMeta(app.SaveRequest{
		Content: "alpha", Title: "A", Type: "decision", TopicKey: "k1", Scope: "project",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := srcSvc.SaveWithMeta(app.SaveRequest{
		Content: "beta", Title: "B", Type: "manual", TopicKey: "k2", Scope: "personal",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := src.Backup(backupPath); err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := os.Stat(backupPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("backup file missing or empty: %v size=%d", err, info.Size())
	}

	// Open a brand-new empty store, then restore from the artifact.
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := store.NewSQLiteStore(dstPath)
	if err != nil {
		t.Fatalf("new dst: %v", err)
	}
	defer dst.Close()
	if err := app.NewService(dst, nil).Init(); err != nil {
		t.Fatalf("init dst: %v", err)
	}
	if err := dst.Restore(backupPath); err != nil {
		t.Fatalf("restore: %v", err)
	}

	rows, err := dst.List(10)
	if err != nil {
		t.Fatalf("list after restore: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows after restore, got %d", len(rows))
	}
	// Match by content since order is created_at DESC.
	contents := map[string]bool{rows[0].Content: true, rows[1].Content: true}
	if !contents["alpha"] || !contents["beta"] {
		t.Fatalf("expected alpha+beta after restore, got %+v", rows)
	}
	// Spot-check metadata round-tripped.
	for _, r := range rows {
		if r.Content == "alpha" && (r.Title != "A" || r.TopicKey != "k1") {
			t.Fatalf("alpha metadata lost: %+v", r)
		}
	}

	// Spec scenario: "Round-trip preserves recall output" — identical
	// recall queries on S1 and S2 MUST return identical ordered results.
	// We compare by (id, content, title, topic_key) tuple-by-tuple so any
	// drift in row identity, ordering, or metadata fails loudly.
	for _, q := range []string{"alpha", "beta"} {
		srcHits, err := src.Recall(q, 10)
		if err != nil {
			t.Fatalf("src recall %q: %v", q, err)
		}
		dstHits, err := dst.Recall(q, 10)
		if err != nil {
			t.Fatalf("dst recall %q: %v", q, err)
		}
		if len(srcHits) != len(dstHits) {
			t.Fatalf("recall %q length drift: src=%d dst=%d", q, len(srcHits), len(dstHits))
		}
		for i := range srcHits {
			if srcHits[i].ID != dstHits[i].ID ||
				srcHits[i].Content != dstHits[i].Content ||
				srcHits[i].Title != dstHits[i].Title ||
				srcHits[i].TopicKey != dstHits[i].TopicKey {
				t.Fatalf("recall %q row %d differs:\n src=%+v\n dst=%+v",
					q, i, srcHits[i], dstHits[i])
			}
		}
		if len(srcHits) == 0 {
			t.Fatalf("recall %q returned no hits on src; expected at least one match", q)
		}
	}
}

// TestBackup_RejectsMissingDir guards a clear error when the destination
// directory doesn't exist — backup MUST NOT silently no-op.
func TestBackup_RejectsMissingDir(t *testing.T) {
	src := openTempStoreT(t)
	if err := app.NewService(src, nil).Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	err := src.Backup("/nope/does/not/exist/backup.db")
	if err == nil {
		t.Fatalf("expected error for missing parent dir")
	}
}

// TestRestore_RejectsMissingFile: restoring from a nonexistent file
// must error before touching the live DB.
func TestBackup_Restore_RejectsMissingFile(t *testing.T) {
	src := openTempStoreT(t)
	if err := app.NewService(src, nil).Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := src.Restore("/nope/missing.db"); err == nil {
		t.Fatalf("expected error for missing backup file")
	}
}

// openTempStoreT returns a fresh SQLiteStore on a temp file, closed by
// t.Cleanup. Mirrors helpers used elsewhere in the package.
func openTempStoreT(t *testing.T) *store.SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewSQLiteStore(filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
