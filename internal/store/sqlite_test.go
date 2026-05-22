package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

func TestInit_MigrationV5ToV6_SchemaVersionStamped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v5.db")

	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL
		);
	`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(5, ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed schema_version=5: %v", err)
	}

	if err := s.Init(); err != nil {
		t.Fatalf("Init from v5 to v6: %v", err)
	}

	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 6 {
		t.Fatalf("expected schema_version=6, got %d", v)
	}
}

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestInit_MigrationFromOldSchema covers spec scenario:
// "Old DB upgraded in place" — pre-existing DB without updated_at must be migrated
// additively, existing rows must remain readable, and missing updated_at must
// default to created_at.
func TestInit_MigrationFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// Step 1: simulate the OLD schema (no updated_at) and seed a row.
	old, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	if _, err := old.db.Exec(`
		CREATE TABLE memory_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);
	`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := old.db.Exec(
		`INSERT INTO memory_items(content, created_at) VALUES(?, ?)`,
		"legacy note",
		createdAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old: %v", err)
	}

	// Step 2: open the SAME db file with new code and run Init() — should add
	// the column and backfill, not error out.
	upgraded, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer upgraded.Close()
	if err := upgraded.Init(); err != nil {
		t.Fatalf("Init() on legacy db should succeed, got: %v", err)
	}

	// Running Init() again on already-migrated schema must be idempotent.
	if err := upgraded.Init(); err != nil {
		t.Fatalf("Init() must be idempotent, got: %v", err)
	}

	// Step 3: legacy row must be readable AND its updated_at must equal created_at.
	got, err := upgraded.Get(1)
	if err != nil {
		t.Fatalf("Get legacy row: %v", err)
	}
	if got.Content != "legacy note" {
		t.Fatalf("expected legacy content preserved, got %q", got.Content)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("expected created_at preserved as %s, got %s", createdAt, got.CreatedAt)
	}
	if !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("expected backfilled updated_at == created_at, got created=%s updated=%s",
			got.CreatedAt, got.UpdatedAt)
	}
}

func TestSaveAndGet_RoundTripIncludesTimestamps(t *testing.T) {
	s := newTestStore(t)

	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	id, err := s.Save("first note", createdAt)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id {
		t.Fatalf("expected id %d, got %d", id, got.ID)
	}
	if got.Content != "first note" {
		t.Fatalf("expected content preserved, got %q", got.Content)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("expected created_at %s, got %s", createdAt, got.CreatedAt)
	}
	// On insert, updated_at must equal created_at per design.
	if !got.UpdatedAt.Equal(createdAt) {
		t.Fatalf("expected updated_at == created_at on insert, got created=%s updated=%s",
			got.CreatedAt, got.UpdatedAt)
	}
}

func TestGet_MissingIDReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get(9999)
	if err == nil {
		t.Fatalf("expected error for missing id, got nil")
	}
	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("expected app.ErrNotFound, got %v", err)
	}
}

func TestUpdate_ExistingRowChangesContentAndUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	id, err := s.Save("original", createdAt)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	updatedAt := time.Date(2024, 6, 2, 9, 30, 0, 0, time.UTC)
	ok, err := s.Update(id, "rewritten", updatedAt)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true when updating existing row")
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Content != "rewritten" {
		t.Fatalf("expected content updated to %q, got %q", "rewritten", got.Content)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("expected created_at unchanged %s, got %s", createdAt, got.CreatedAt)
	}
	if !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("expected updated_at %s, got %s", updatedAt, got.UpdatedAt)
	}
}

func TestUpdate_MissingIDReturnsFalseAndNoWrite(t *testing.T) {
	s := newTestStore(t)

	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	existingID, err := s.Save("untouched", createdAt)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	updatedAt := time.Date(2024, 6, 2, 9, 30, 0, 0, time.UTC)
	ok, err := s.Update(99999, "should not land", updatedAt)
	if err != nil {
		t.Fatalf("update missing: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for missing id")
	}

	// Confirm the existing row was not touched (no accidental UPDATE).
	got, err := s.Get(existingID)
	if err != nil {
		t.Fatalf("get existing: %v", err)
	}
	if got.Content != "untouched" {
		t.Fatalf("expected existing row content unchanged, got %q", got.Content)
	}
	if !got.UpdatedAt.Equal(createdAt) {
		t.Fatalf("expected existing updated_at unchanged %s, got %s", createdAt, got.UpdatedAt)
	}
}

func TestRecallAndList_ReturnUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	createdAt := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	id, err := s.Save("findable text", createdAt)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// After update, list/recall must surface the new updated_at.
	updatedAt := time.Date(2024, 6, 5, 8, 0, 0, 0, time.UTC)
	if _, err := s.Update(id, "findable text v2", updatedAt); err != nil {
		t.Fatalf("update: %v", err)
	}

	listed, err := s.List(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 row from list, got %d", len(listed))
	}
	if !listed[0].UpdatedAt.Equal(updatedAt) {
		t.Fatalf("list: expected updated_at %s, got %s", updatedAt, listed[0].UpdatedAt)
	}
	if !listed[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("list: expected created_at %s, got %s", createdAt, listed[0].CreatedAt)
	}

	recalled, err := s.Recall("findable", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) != 1 {
		t.Fatalf("expected 1 row from recall, got %d", len(recalled))
	}
	if !recalled[0].UpdatedAt.Equal(updatedAt) {
		t.Fatalf("recall: expected updated_at %s, got %s", updatedAt, recalled[0].UpdatedAt)
	}
	if !strings.Contains(recalled[0].Content, "v2") {
		t.Fatalf("expected updated content visible in recall, got %q", recalled[0].Content)
	}
}
