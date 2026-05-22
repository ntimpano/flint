package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"flint/internal/app"
	"flint/internal/store"

	_ "modernc.org/sqlite"
)

// TestDoctor_HealthyStore proves the M3 doctor scenario "diagnostic
// surface": after Init, doctor MUST report schema_version=3, FTS healthy,
// integrity ok, and accurate row counts.
func TestDoctor_HealthyStore(t *testing.T) {
	src := openTempStoreT(t)
	svc := app.NewService(src, nil)
	if err := svc.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := svc.Save("note 1"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := svc.Save("note 2"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := svc.SessionStart("sess-1"); err != nil {
		t.Fatalf("session start: %v", err)
	}

	report, err := src.Doctor()
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("schema_version: want %d got %d",
			store.CurrentSchemaVersion, report.SchemaVersion)
	}
	if !report.FTSHealthy {
		t.Fatalf("expected FTS healthy after Init")
	}
	if !report.IntegrityOK {
		t.Fatalf("expected integrity_check=ok, got messages=%v", report.IntegrityMessages)
	}
	if report.MemoryItemsCount != 2 {
		t.Fatalf("memory_items count: want 2 got %d", report.MemoryItemsCount)
	}
	if report.SessionsCount != 1 {
		t.Fatalf("sessions count: want 1 got %d", report.SessionsCount)
	}
}

// TestDoctor_FreshStoreCounts: an empty store after Init reports zero
// counts and stays healthy.
func TestDoctor_FreshStoreCounts(t *testing.T) {
	src := openTempStoreT(t)
	if err := app.NewService(src, nil).Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	report, err := src.Doctor()
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.MemoryItemsCount != 0 || report.SessionsCount != 0 {
		t.Fatalf("expected zero counts on fresh store, got %+v", report)
	}
	if !report.FTSHealthy || !report.IntegrityOK {
		t.Fatalf("fresh store must be healthy: %+v", report)
	}
}

// TestDoctor_ReportsContainSummaryLine sanity-checks the human-readable
// Summary field (used by CLI/MCP presentation) — must mention each
// reported axis so users see all signals at a glance.
func TestDoctor_ReportsContainSummaryLine(t *testing.T) {
	src := openTempStoreT(t)
	if err := app.NewService(src, nil).Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	report, err := src.Doctor()
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	summary := strings.ToLower(report.Summary)
	for _, axis := range []string{"schema", "fts", "integrity", "memory_items", "sessions"} {
		if !strings.Contains(summary, axis) {
			t.Fatalf("summary missing %q axis: %q", axis, report.Summary)
		}
	}
}

// TestDoctor_CorruptFTS_ReportsDegradedAndRecallStillWorks proves M3 spec
// scenario "Corrupt FTS reported, CRUD still works": when memory_fts is
// dropped/corrupted, doctor MUST flag FTS as not-healthy AND recall MUST
// continue to return results via the LIKE fallback path.
//
// This closes the verify-report partial: prior tests covered the recall
// fallback OR the doctor summary in isolation, but never asserted that
// the doctor surface AND the recall fallback agree on the same broken DB.
func TestDoctor_CorruptFTS_ReportsDegradedAndRecallStillWorks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	src, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	svc := app.NewService(src, nil)
	if err := svc.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Seed content so recall has something to find via LIKE fallback.
	if _, err := svc.Save("delta needle in haystack"); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Force FTS corruption by dropping the virtual table via a side
	// connection — same path, no exported helper needed. modernc/sqlite
	// allows a second handle on the same file in this in-process test.
	side, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("side open: %v", err)
	}
	if _, err := side.Exec(`DROP TABLE memory_fts`); err != nil {
		_ = side.Close()
		t.Fatalf("drop fts: %v", err)
	}
	_ = side.Close()

	// 1. Doctor surface: FTSHealthy MUST be false; summary MUST surface
	//    a non-healthy FTS marker so CLI/MCP renders show degraded state.
	report, err := src.Doctor()
	if err != nil {
		t.Fatalf("doctor on corrupt fts: %v", err)
	}
	if report.FTSHealthy {
		t.Fatalf("expected FTSHealthy=false after dropping memory_fts; got true")
	}
	summary := strings.ToLower(report.Summary)
	if !strings.Contains(summary, "fts=") {
		t.Fatalf("summary missing fts= axis: %q", report.Summary)
	}
	if strings.Contains(summary, "fts=healthy") {
		t.Fatalf("summary still reports fts=healthy on corrupt store: %q", report.Summary)
	}
	// Other axes MUST stay consistent (integrity untouched, counts intact).
	if !report.IntegrityOK {
		t.Fatalf("integrity_check should still pass on dropped-FTS store: %+v", report)
	}
	if report.MemoryItemsCount != 1 {
		t.Fatalf("memory_items count unaffected by FTS drop: want 1 got %d", report.MemoryItemsCount)
	}

	// 2. Recall MUST fall back to LIKE and still find the seeded row.
	hits, err := svc.Recall("needle", 10)
	if err != nil {
		t.Fatalf("recall after fts drop must not error, got: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected LIKE fallback to find seeded row, got %d hits", len(hits))
	}
	if hits[0].Content != "delta needle in haystack" {
		t.Fatalf("unexpected fallback hit: %+v", hits[0])
	}
}
