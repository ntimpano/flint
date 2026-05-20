package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flint/internal/app"
	"flint/internal/model"

	_ "modernc.org/sqlite"
)

// CurrentSchemaVersion is the migration target enforced by Init. Increment
// this constant only when adding a new forward-only migration step.
//
// v1: M1 structured-memory columns (title/type/topic_key/scope) + indexes.
// v2: M2 ranked-recall — memory_fts virtual table + sync triggers.
// v3: M3 session lifecycle log — `sessions` table for session_workflow.
// v4: M4 memory graph — `memory_relations` typed-edge table + indexes.
// v5: project context — `projects` table + `active_project` singleton +
//
//	nullable `project_id` column on memory_items with backfill to a
//	`default` project. Enables host-mediated project autoswitch.
//
// v6: behavioral learning — `behavioral_observations` table + indexes.
const CurrentSchemaVersion = 6

var ErrNoActiveSession = errors.New("no active session")

type SQLiteStore struct {
	db        *sql.DB
	dbPath    string
	backupDir string
	// useFTS is set at Init() time to true when the FTS5 module is
	// available AND the memory_fts table is healthy. Recall() checks this
	// flag once per call and downgrades to LIKE on the fly if a query
	// against memory_fts fails (e.g. corruption, manual DROP).
	useFTS bool
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// modernc.org/sqlite defaults foreign_keys=OFF per connection. Keep the
	// DSN-level pragma so every pooled connection starts with FK enforcement.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := applyPragmas(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{
		db:        db,
		dbPath:    path,
		backupDir: defaultBackupDir(),
	}, nil
}

func applyPragmas(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("apply pragma foreign_keys: %w", err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("apply pragma journal_mode WAL: %w", err)
	}
	return nil
}

// SetBackupDir overrides the directory used for pre-migration snapshots.
// Tests use this to keep snapshots inside a t.TempDir; the production
// default is `~/.flint/backups/`.
func (s *SQLiteStore) SetBackupDir(dir string) {
	s.backupDir = dir
}

// defaultBackupDir resolves `~/.flint/backups` lazily so tests that never
// hit migration do not need a writable HOME.
func defaultBackupDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
			home = h
		} else {
			home = os.TempDir()
		}
	}
	return filepath.Join(home, ".flint", "backups")
}

func (s *SQLiteStore) Init() error {
	// Bootstrap: ensure schema_version exists so migration state is queryable
	// regardless of whether this is a fresh install or a legacy DB.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}

	// Detect whether this DB pre-existed M1 (legacy memory_items present but
	// schema_version still 0). Snapshot the file BEFORE applying migrations
	// so a failed run is recoverable from disk.
	legacy, err := s.hasLegacyTable()
	if err != nil {
		return err
	}
	needsMigration := current < CurrentSchemaVersion && legacy
	if needsMigration {
		if err := s.snapshotForMigration(); err != nil {
			return fmt.Errorf("pre-migration snapshot: %w", err)
		}
	}

	// Apply M1 schema in a transaction so a mid-migration failure leaves
	// schema_version unchanged. The CREATE TABLE IF NOT EXISTS keeps fresh
	// installs additive too.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS memory_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME,
			title TEXT,
			type TEXT,
			topic_key TEXT,
			scope TEXT
		);
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_items_created_at ON memory_items(created_at DESC)`,
	); err != nil {
		return err
	}

	// Drop any stale FTS sync triggers BEFORE running the column backfill
	// UPDATE below. If a previous process left triggers behind but
	// memory_fts was dropped (corruption, manual recovery, downgrade),
	// the backfill UPDATE would fire those triggers and crash with
	// "no such table: memory_fts". Doing this here keeps the migration
	// reentrant in the face of partially broken installs.
	for _, drop := range []string{
		`DROP TRIGGER IF EXISTS memory_items_ai`,
		`DROP TRIGGER IF EXISTS memory_items_ad`,
		`DROP TRIGGER IF EXISTS memory_items_au`,
	} {
		if _, err := tx.Exec(drop); err != nil {
			return fmt.Errorf("clean stale fts triggers: %w", err)
		}
	}

	// Backwards-compat ALTERs: SQLite has no IF NOT EXISTS for ADD COLUMN,
	// so we tolerate the duplicate-column error per migration step.
	for _, alter := range []string{
		`ALTER TABLE memory_items ADD COLUMN updated_at DATETIME`,
		`ALTER TABLE memory_items ADD COLUMN title TEXT`,
		`ALTER TABLE memory_items ADD COLUMN type TEXT`,
		`ALTER TABLE memory_items ADD COLUMN topic_key TEXT`,
		`ALTER TABLE memory_items ADD COLUMN scope TEXT`,
		`ALTER TABLE memory_items ADD COLUMN content_hash TEXT`,
	} {
		if _, err := tx.Exec(alter); err != nil {
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("migrate columns: %w", err)
			}
		}
	}

	if _, err := tx.Exec(
		`UPDATE memory_items SET updated_at = created_at WHERE updated_at IS NULL`,
	); err != nil {
		return fmt.Errorf("backfill updated_at: %w", err)
	}

	// Topic-key lookup index — used by the application-level upsert path.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_items_topic ON memory_items(topic_key, scope)`,
	); err != nil {
		return err
	}

	// M3 — session lifecycle log. Each row is one of "start" / "summary"
	// / "end" tagged with a session_id. Kept as an append-only log
	// (no UPDATE) so re-running session start/end doesn't mutate
	// historical rows. Indexed by session_id for fast SessionEvents().
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			summary TEXT,
			created_at DATETIME NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create sessions: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id, id)`,
	); err != nil {
		return err
	}

	// M3 — content_hash column on memory_items powers the import dedupe path.
	// Base index is created on legacy key columns; M5 upgrades it to include
	// project_id once that column exists.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_items_import_dedupe
		 ON memory_items(topic_key, content_hash)`,
	); err != nil {
		return err
	}

	// M4 — typed memory graph. memory_relations is an additive directed-edge
	// table linking two memory_items rows by relation_type. Constraints:
	//   * source_id != target_id            — no self-loops (CHECK).
	//   * relation_type IN whitelist        — keep semantics curated (CHECK).
	//   * ON DELETE CASCADE on both FKs     — neighbor traversal never sees
	//                                         dangling edges; relies on the
	//                                         DSN-level foreign_keys pragma
	//                                         set in NewSQLiteStore.
	// Indexed in both directions so Neighbors(id, dir) is O(log n) for either
	// outbound (source_id) or inbound (target_id) lookups, ordered by
	// created_at ASC for deterministic traversal.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS memory_relations (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id     INTEGER NOT NULL,
			target_id     INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			created_at    DATETIME NOT NULL,
			CHECK (source_id <> target_id),
			CHECK (relation_type IN (
				'related','supersedes','conflicts_with','refines','depends_on'
			)),
			FOREIGN KEY(source_id) REFERENCES memory_items(id) ON DELETE CASCADE,
			FOREIGN KEY(target_id) REFERENCES memory_items(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create memory_relations: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_relations_source
		 ON memory_relations(source_id, created_at, id)`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_relations_target
		 ON memory_relations(target_id, created_at, id)`,
	); err != nil {
		return err
	}

	// M5 — project context. The `projects` table holds named project
	// records identified by a stable fingerprint; `active_project` is a
	// singleton (id = 1) pointer at the currently active project. The
	// `memory_items.project_id` column is added nullable for additivity:
	// fresh installs and legacy DBs both run the same backfill that maps
	// every existing row to a "default" project, then the read path
	// (PR2) starts filtering by active_project. Keeping project_id
	// nullable is deliberate — a NOT NULL CHECK can be promoted in a
	// later migration once all install bases are observed clean.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT NOT NULL UNIQUE,
			root_path    TEXT NOT NULL DEFAULT '',
			fingerprint  TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create projects: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_projects_fingerprint
		 ON projects(fingerprint)`,
	); err != nil {
		return err
	}
	// active_project is a SINGLE-row pointer table. id is fixed at 1 via
	// CHECK so callers can UPSERT the active context without having to
	// pre-clean prior rows. project_id is a foreign key into projects.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS active_project (
			id          INTEGER PRIMARY KEY CHECK (id = 1),
			project_id  INTEGER NOT NULL,
			updated_at  DATETIME NOT NULL,
			FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE RESTRICT
		);
	`); err != nil {
		return fmt.Errorf("create active_project: %w", err)
	}
	// Add nullable project_id on memory_items (additive, idempotent).
	if _, err := tx.Exec(
		`ALTER TABLE memory_items ADD COLUMN project_id INTEGER`,
	); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("add memory_items.project_id: %w", err)
		}
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_items_project_id
		 ON memory_items(project_id)`,
	); err != nil {
		return err
	}
	// Upgrade import dedupe index to include project_id (PR2b semantics).
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_memory_items_import_dedupe`); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_memory_items_import_dedupe
		 ON memory_items(project_id, topic_key, content_hash)`,
	); err != nil {
		return err
	}
	// Backfill: ensure a "default" project exists, then map every legacy
	// row to it. INSERT OR IGNORE keeps this idempotent across re-runs
	// (the UNIQUE constraint on name is the dedupe key).
	//
	// Rollback SQL (operational, pre-UNIQUE-hardening):
	//   UPDATE memory_items
	//   SET project_id = NULL
	//   WHERE project_id = (SELECT id FROM projects WHERE name = 'default');
	//
	//   DELETE FROM active_project WHERE id = 1;
	//   DELETE FROM projects WHERE name = 'default';
	//
	// In practice, restore from the pre-migration snapshot is preferred for
	// full reversibility because it guarantees exact data restoration.
	nowStamp := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO projects(name, root_path, fingerprint, created_at)
		 VALUES('default', '', '', ?)`,
		nowStamp,
	); err != nil {
		return fmt.Errorf("seed default project: %w", err)
	}
	var defaultID int64
	if err := tx.QueryRow(
		`SELECT id FROM projects WHERE name = 'default'`,
	).Scan(&defaultID); err != nil {
		return fmt.Errorf("read default project id: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE memory_items SET project_id = ? WHERE project_id IS NULL`,
		defaultID,
	); err != nil {
		return fmt.Errorf("backfill memory_items.project_id: %w", err)
	}
	// Initialize the active_project pointer to default if not yet set.
	// INSERT OR IGNORE protects an existing pointer from being overwritten
	// during re-runs of Init() on already-migrated DBs.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO active_project(id, project_id, updated_at)
		 VALUES(1, ?, ?)`,
		defaultID, nowStamp,
	); err != nil {
		return fmt.Errorf("seed active_project: %w", err)
	}

	// M6 — behavioral learning observation capture table.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS behavioral_observations (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			category         TEXT    NOT NULL,
			field            TEXT    NOT NULL,
			value            TEXT    NOT NULL,
			confidence       INTEGER NOT NULL CHECK (confidence BETWEEN 0 AND 100),
			occurrence_count INTEGER NOT NULL DEFAULT 1,
			status           TEXT    NOT NULL DEFAULT 'observed'
			                 CHECK (status IN ('observed','candidate','dismissed')),
			last_seen        DATETIME NOT NULL,
			created_at       DATETIME NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create behavioral_observations: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_behavioral_obs_unique
		 ON behavioral_observations(category, field, value)`,
	); err != nil {
		return fmt.Errorf("create behavioral_observations unique index: %w", err)
	}

	// M2 — FTS5 ranked recall. The virtual table mirrors (content, title)
	// and is kept in sync by AFTER INSERT/UPDATE/DELETE triggers. Triggers
	// reference the rowid as `memory_items.id` so bm25 ranks map cleanly
	// back to the canonical row id without an extra join.
	//
	// We tolerate FTS5 not being compiled into the SQLite build: a CREATE
	// VIRTUAL TABLE failure flips useFTS=false and Recall() degrades to
	// LIKE for the lifetime of the process. Other migration steps must
	// still succeed so the rest of the M1 surface keeps working.
	ftsHealthy := true
	if _, err := tx.Exec(
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
			content,
			title,
			content='memory_items',
			content_rowid='id',
			tokenize='unicode61'
		)`,
	); err != nil {
		ftsHealthy = false
	}
	if ftsHealthy {
		// Sync triggers. content_rowid='id' makes memory_fts a contentless
		// (external-content) FTS table; we use the documented insert/delete
		// dance for UPDATEs to keep the index aligned with row revisions.
		for _, stmt := range []string{
			`CREATE TRIGGER IF NOT EXISTS memory_items_ai AFTER INSERT ON memory_items BEGIN
				INSERT INTO memory_fts(rowid, content, title) VALUES (new.id, new.content, COALESCE(new.title, ''));
			END`,
			`CREATE TRIGGER IF NOT EXISTS memory_items_ad AFTER DELETE ON memory_items BEGIN
				INSERT INTO memory_fts(memory_fts, rowid, content, title) VALUES('delete', old.id, old.content, COALESCE(old.title, ''));
			END`,
			`CREATE TRIGGER IF NOT EXISTS memory_items_au AFTER UPDATE ON memory_items BEGIN
				INSERT INTO memory_fts(memory_fts, rowid, content, title) VALUES('delete', old.id, old.content, COALESCE(old.title, ''));
				INSERT INTO memory_fts(rowid, content, title) VALUES (new.id, new.content, COALESCE(new.title, ''));
			END`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				ftsHealthy = false
				break
			}
		}
	}
	if ftsHealthy {
		// Backfill: rebuild the FTS index from memory_items so any rows
		// inserted under v1 (or imported via DB swap) become searchable
		// without requiring callers to re-save them.
		if _, err := tx.Exec(
			`INSERT INTO memory_fts(memory_fts) VALUES('rebuild')`,
		); err != nil {
			ftsHealthy = false
		}
	}
	s.useFTS = ftsHealthy

	// Stamp schema_version exactly once per upgrade so re-running Init is
	// idempotent (no duplicate snapshot, no row churn).
	if current < CurrentSchemaVersion {
		if _, err := tx.Exec(
			`INSERT INTO schema_version(version, applied_at) VALUES(?, ?)`,
			CurrentSchemaVersion, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("record schema_version: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	commit = true
	return nil
}

// SchemaVersion returns the highest applied schema version, or 0 if no
// migrations have ever been recorded.
func (s *SQLiteStore) SchemaVersion() (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// hasLegacyTable reports whether memory_items already exists, which signals
// "this is a pre-M1 install" when combined with schema_version = 0.
func (s *SQLiteStore) hasLegacyTable() (bool, error) {
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memory_items'`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name == "memory_items", nil
}

// snapshotForMigration copies the current DB file into backupDir under a
// timestamped name BEFORE the M1 migration runs. The copy uses a streaming
// io.Copy so large databases are handled without loading them into memory.
func (s *SQLiteStore) snapshotForMigration() error {
	if err := os.MkdirAll(s.backupDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(s.backupDir, fmt.Sprintf("pre-migration-%s.db", stamp))

	// Flush WAL/journal state so the copied file is a complete snapshot.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(FULL)`); err != nil {
		// Non-fatal: not all journal modes support checkpoint. Continue.
		_ = err
	}

	src, err := os.Open(s.dbPath)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return out.Sync()
}

// isDuplicateColumnErr matches SQLite's "duplicate column name" error so
// repeated ALTER TABLE calls remain idempotent across Init() runs.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}

func (s *SQLiteStore) Save(content string, createdAt time.Time) (int64, error) {
	stamp := createdAt.Format(time.RFC3339)
	res, err := s.db.Exec(
		"INSERT INTO memory_items(content, created_at, updated_at) VALUES(?, ?, ?)",
		content,
		stamp,
		stamp,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SaveWithMeta persists a row with optional structured metadata. When
// req.TopicKey is non-empty the call performs an application-level upsert:
// the latest matching (scope, topic_key, project_id) row is UPDATEd in place;
// otherwise a new row is INSERTed. The application-level path was chosen over a unique
// DB constraint to keep future history/versioning flexibility open per
// design.md §FTS+Topic strategy.
func (s *SQLiteStore) SaveWithMeta(req model.SaveRequest) (int64, error) {
	stamp := req.CreatedAt.Format(time.RFC3339)

	if strings.TrimSpace(req.TopicKey) != "" {
		var existingID int64
		err := s.db.QueryRow(
			`SELECT id FROM memory_items
			 WHERE topic_key = ?
			   AND COALESCE(scope, '') = COALESCE(?, '')
			   AND COALESCE(project_id, 0) = COALESCE(?, 0)
			 ORDER BY datetime(updated_at) DESC, id DESC
			 LIMIT 1`,
			req.TopicKey, req.Scope, req.ProjectID,
		).Scan(&existingID)
		if err == nil {
			if _, err := s.db.Exec(
				`UPDATE memory_items
				 SET content = ?, updated_at = ?, title = ?, type = ?
				 WHERE id = ?`,
				req.Content, stamp, req.Title, req.Type, existingID,
			); err != nil {
				return 0, err
			}
			return existingID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	// Stamp project_id when provided (v5+). Zero means unscoped (legacy).
	var projectIDVal any
	if req.ProjectID > 0 {
		projectIDVal = req.ProjectID
	}
	res, err := s.db.Exec(
		`INSERT INTO memory_items(content, created_at, updated_at, title, type, topic_key, scope, project_id)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Content, stamp, stamp,
		req.Title, req.Type, req.TopicKey, req.Scope, projectIDVal,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// scanRow scans a single row in
// (id, content, created_at, updated_at, title, type, topic_key, scope) order.
func scanRow(scanner interface {
	Scan(dest ...any) error
}) (model.MemoryItem, error) {
	var (
		it         model.MemoryItem
		createdRaw string
		updatedRaw sql.NullString
		title      sql.NullString
		typ        sql.NullString
		topicKey   sql.NullString
		scope      sql.NullString
	)
	if err := scanner.Scan(
		&it.ID, &it.Content, &createdRaw, &updatedRaw,
		&title, &typ, &topicKey, &scope,
	); err != nil {
		return model.MemoryItem{}, err
	}
	created, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return model.MemoryItem{}, fmt.Errorf("invalid created_at in db: %w", err)
	}
	it.CreatedAt = created

	if updatedRaw.Valid && updatedRaw.String != "" {
		updated, err := time.Parse(time.RFC3339, updatedRaw.String)
		if err != nil {
			return model.MemoryItem{}, fmt.Errorf("invalid updated_at in db: %w", err)
		}
		it.UpdatedAt = updated
	} else {
		it.UpdatedAt = created
	}
	it.Title = title.String
	it.Type = typ.String
	it.TopicKey = topicKey.String
	it.Scope = scope.String
	return it, nil
}

const selectColumns = `id, content, created_at, updated_at, title, type, topic_key, scope`

func (s *SQLiteStore) Get(id int64) (model.MemoryItem, error) {
	row := s.db.QueryRow(
		`SELECT `+selectColumns+`
		 FROM memory_items
		 WHERE id = ?`,
		id,
	)
	it, err := scanRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.MemoryItem{}, app.ErrNotFound
		}
		return model.MemoryItem{}, err
	}
	return it, nil
}

func (s *SQLiteStore) Update(id int64, content string, updatedAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE memory_items SET content = ?, updated_at = ? WHERE id = ?`,
		content,
		updatedAt.Format(time.RFC3339),
		id,
	)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *SQLiteStore) Recall(query string, limit int) ([]model.MemoryItem, error) {
	if s.useFTS {
		items, err := s.recallFTS(query, limit)
		if err == nil {
			return items, nil
		}
		// FTS query failed mid-flight (e.g. memory_fts dropped or
		// corrupted after Init). Mark FTS unhealthy for the rest of the
		// process and fall through to LIKE so the caller still gets a
		// result instead of an error per spec scenario "FTS unavailable
		// falls back to LIKE".
		s.useFTS = false
	}
	return s.recallLIKE(query, limit)
}

// RecallFiltered runs a recall with optional metadata + date-range
// filters. Empty/zero-valued filter fields are treated as unbounded so the
// same code path supports plain queries and fully-faceted queries without
// branching at the call site.
//
// Filter semantics (all AND-combined):
//   - Type: exact match on memory_items.type when non-empty.
//   - Since/Until: inclusive bounds on created_at; zero-time = unbounded.
//   - Limit: SQL LIMIT clause; the caller is responsible for defaults.
//
// FTS path is preferred when healthy; on FTS failure we transparently
// downgrade to LIKE and disable useFTS for the rest of the process —
// same resilience contract as Recall().
func (s *SQLiteStore) RecallFiltered(opts model.RecallOptions) ([]model.MemoryItem, error) {
	if s.useFTS {
		items, err := s.recallFTSFiltered(opts)
		if err == nil {
			return items, nil
		}
		s.useFTS = false
	}
	return s.recallLIKEFiltered(opts)
}

// Context returns the most recent N rows newest-first, optionally narrowed
// by scope. Empty scope disables the scope filter. Ordering is
// created_at DESC, falling back to id DESC for ties — matching the legacy
// List() ordering so callers see consistent recency semantics.
func (s *SQLiteStore) Context(n int, scope string) ([]model.MemoryItem, error) {
	scope = strings.TrimSpace(scope)
	var (
		rows *sql.Rows
		err  error
	)
	if scope == "" {
		rows, err = s.db.Query(
			`SELECT `+selectColumns+`
			 FROM memory_items
			 ORDER BY datetime(created_at) DESC, id DESC
			 LIMIT ?`,
			n,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT `+selectColumns+`
			 FROM memory_items
			 WHERE COALESCE(scope, '') = ?
			 ORDER BY datetime(created_at) DESC, id DESC
			 LIMIT ?`,
			scope, n,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// ContextFiltered returns the most recent N rows with optional scope and
// project_id filtering. When opts.AllProjects is true, the project filter
// is bypassed (--all flag behavior).
func (s *SQLiteStore) ContextFiltered(opts model.ContextOptions) ([]model.MemoryItem, error) {
	clauses := []string{}
	args := []any{}
	scope := strings.TrimSpace(opts.Scope)
	if scope != "" {
		clauses = append(clauses, "COALESCE(scope, '') = ?")
		args = append(args, scope)
	}
	if opts.ProjectID > 0 && !opts.AllProjects {
		clauses = append(clauses, "project_id = ?")
		args = append(args, opts.ProjectID)
	}

	q := `SELECT ` + selectColumns + ` FROM memory_items`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY datetime(created_at) DESC, id DESC LIMIT ?"
	args = append(args, opts.N)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// ListFiltered returns up to limit rows in created_at DESC order, optionally
// filtered by project_id. AllProjects bypasses the filter.
func (s *SQLiteStore) ListFiltered(opts model.ListOptions) ([]model.MemoryItem, error) {
	clauses := []string{}
	args := []any{}
	if opts.ProjectID > 0 && !opts.AllProjects {
		clauses = append(clauses, "project_id = ?")
		args = append(args, opts.ProjectID)
	}

	q := `SELECT ` + selectColumns + ` FROM memory_items`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY datetime(created_at) DESC LIMIT ?"
	args = append(args, opts.Limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// UseFTS reports whether ranked recall is currently active. It returns
// false when the FTS5 module was unavailable at Init() OR when an earlier
// recall failed and the store downgraded itself to LIKE for the rest of
// the process. Doctor/health surfaces (M3) read this flag.
func (s *SQLiteStore) UseFTS() bool {
	return s.useFTS
}

// recallFTS executes a bm25-ranked query against memory_fts and joins back
// to memory_items for full row reconstruction. The MATCH expression is
// rebuilt from the user query as a prefix-OR clause so casual keyword input
// (e.g. "fts5 ranking") still matches without forcing callers to learn
// FTS5 query syntax.
func (s *SQLiteStore) recallFTS(query string, limit int) ([]model.MemoryItem, error) {
	match := buildFTSMatch(query)
	if match == "" {
		// Nothing tokenizable — defer to LIKE so the caller still gets a
		// meaningful behavior on punctuation-only input.
		return s.recallLIKE(query, limit)
	}
	rows, err := s.db.Query(
		`SELECT mi.id, mi.content, mi.created_at, mi.updated_at,
		        mi.title, mi.type, mi.topic_key, mi.scope
		 FROM memory_fts
		 JOIN memory_items mi ON mi.id = memory_fts.rowid
		 WHERE memory_fts MATCH ?
		 ORDER BY bm25(memory_fts)
		 LIMIT ?`,
		match, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// recallLIKE is the resilient fallback path used when FTS is unavailable
// or returns an error. It preserves the legacy created_at-DESC ordering
// because LIKE has no relevance signal — newest-first is the least
// surprising default.
func (s *SQLiteStore) recallLIKE(query string, limit int) ([]model.MemoryItem, error) {
	like := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.Query(
		`SELECT `+selectColumns+`
		 FROM memory_items
		 WHERE LOWER(content) LIKE ?
		 ORDER BY datetime(created_at) DESC
		 LIMIT ?`,
		like,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// recallFTSFiltered is recallFTS plus optional metadata + date filters
// applied as additional WHERE clauses against memory_items. The MATCH
// stays inside memory_fts so bm25 ranking is preserved; type/since/until
// are joined-table predicates.
func (s *SQLiteStore) recallFTSFiltered(opts model.RecallOptions) ([]model.MemoryItem, error) {
	match := buildFTSMatch(opts.Query)
	if match == "" {
		return s.recallLIKEFiltered(opts)
	}
	clauses := []string{"memory_fts MATCH ?"}
	args := []any{match}
	clauses, args = appendMetadataFilters(clauses, args, opts)

	q := `SELECT mi.id, mi.content, mi.created_at, mi.updated_at,
	             mi.title, mi.type, mi.topic_key, mi.scope
	      FROM memory_fts
	      JOIN memory_items mi ON mi.id = memory_fts.rowid
	      WHERE ` + strings.Join(clauses, " AND ") + `
	      ORDER BY bm25(memory_fts)
	      LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// recallLIKEFiltered is the resilient fallback for filter-aware recall.
// Uses LOWER(content) LIKE plus the same metadata predicates. Order is
// created_at DESC so newest-first matches the unfiltered LIKE path.
func (s *SQLiteStore) recallLIKEFiltered(opts model.RecallOptions) ([]model.MemoryItem, error) {
	like := "%" + strings.ToLower(opts.Query) + "%"
	clauses := []string{"LOWER(content) LIKE ?"}
	args := []any{like}
	clauses, args = appendMetadataFilters(clauses, args, opts)

	q := `SELECT ` + selectColumns + `
	      FROM memory_items
	      WHERE ` + strings.Join(clauses, " AND ") + `
	      ORDER BY datetime(created_at) DESC
	      LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// appendMetadataFilters appends type/since/until/project predicates to the
// given WHERE clause list. The column references are unqualified so callers
// MUST ensure memory_items is the only table where those columns exist
// in the surrounding query (true for both filtered recall paths).
func appendMetadataFilters(clauses []string, args []any, opts model.RecallOptions) ([]string, []any) {
	if t := strings.TrimSpace(opts.Type); t != "" {
		clauses = append(clauses, "COALESCE(type, '') = ?")
		args = append(args, t)
	}
	if tk := strings.TrimSpace(opts.TopicKey); tk != "" {
		clauses = append(clauses, "COALESCE(topic_key, '') = ?")
		args = append(args, tk)
	}
	if !opts.Since.IsZero() {
		clauses = append(clauses, "datetime(created_at) >= datetime(?)")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		clauses = append(clauses, "datetime(created_at) <= datetime(?)")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if opts.ProjectID > 0 && !opts.AllProjects {
		clauses = append(clauses, "project_id = ?")
		args = append(args, opts.ProjectID)
	}
	return clauses, args
}

// buildFTSMatch sanitizes free-form user input into a safe FTS5 MATCH
// expression. Each whitespace-separated token is wrapped in double quotes
// and given a `*` prefix-match suffix so partial words still hit. Quotes
// in the input are stripped — FTS5 treats `"` as a phrase delimiter and
// passing it raw would let user input break the parser.
func buildFTSMatch(query string) string {
	fields := strings.Fields(strings.ReplaceAll(query, `"`, ""))
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf(`"%s"*`, f))
	}
	return strings.Join(parts, " OR ")
}

func (s *SQLiteStore) List(limit int) ([]model.MemoryItem, error) {
	rows, err := s.db.Query(
		`SELECT `+selectColumns+`
		 FROM memory_items
		 ORDER BY datetime(created_at) DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

func scanAll(rows *sql.Rows) ([]model.MemoryItem, error) {
	var items []model.MemoryItem
	for rows.Next() {
		it, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *SQLiteStore) Delete(id int64) (bool, error) {
	res, err := s.db.Exec("DELETE FROM memory_items WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// SessionStart appends a "start" lifecycle row tagged with id. Empty/
// whitespace-only ids are rejected so unrelated sessions cannot be
// silently merged under an empty key.
func (s *SQLiteStore) SessionStart(id string, at time.Time) error {
	return s.appendSessionEvent(id, "start", "", at)
}

// SessionSummary appends a "summary" lifecycle row carrying free-form
// text. Multiple summaries per session are allowed (append-only log).
func (s *SQLiteStore) SessionSummary(id, summary string, at time.Time) error {
	return s.appendSessionEvent(id, "summary", summary, at)
}

// SessionEnd appends an "end" lifecycle row. Multiple ends are tolerated;
// the log captures whatever the caller wrote — interpretation is the
// reader's responsibility.
func (s *SQLiteStore) SessionEnd(id string, at time.Time) error {
	return s.appendSessionEvent(id, "end", "", at)
}

func (s *SQLiteStore) appendSessionEvent(id, kind, summary string, at time.Time) error {
	clean := strings.TrimSpace(id)
	if clean == "" {
		return errors.New("session id is empty")
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(session_id, kind, summary, created_at) VALUES(?, ?, ?, ?)`,
		clean, kind, summary, at.UTC().Format(time.RFC3339),
	)
	return err
}

// SessionEvents returns every lifecycle row tagged to id, ordered by id
// ASC (insertion order). Empty result is not an error — callers see an
// empty slice for unknown session ids.
func (s *SQLiteStore) SessionEvents(id string) ([]model.SessionEvent, error) {
	clean := strings.TrimSpace(id)
	if clean == "" {
		return nil, errors.New("session id is empty")
	}
	rows, err := s.db.Query(
		`SELECT session_id, kind, COALESCE(summary, ''), created_at
		 FROM sessions
		 WHERE session_id = ?
		 ORDER BY id ASC`,
		clean,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SessionEvent
	for rows.Next() {
		var ev model.SessionEvent
		var createdRaw string
		if err := rows.Scan(&ev.SessionID, &ev.Kind, &ev.Summary, &createdRaw); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, createdRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid sessions.created_at: %w", err)
		}
		ev.CreatedAt = t
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ActiveSessionID() (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT st.session_id
		 FROM sessions st
		 WHERE st.kind = 'start'
		   AND NOT EXISTS (
			   SELECT 1
			   FROM sessions en
			   WHERE en.session_id = st.session_id AND en.kind = 'end'
		   )
		 ORDER BY datetime(st.created_at) DESC, st.id DESC
		 LIMIT 1`,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoActiveSession
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *SQLiteStore) RecordObservation(category, field, value string, confidence int, now time.Time) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO behavioral_observations
		 (category, field, value, confidence, occurrence_count, status, last_seen, created_at)
		 VALUES(?, ?, ?, ?, 1, 'observed', ?, ?)`,
		category, field, value, confidence, stamp, stamp,
	)
	if err != nil {
		return 0, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		if _, err := s.db.Exec(
			`UPDATE behavioral_observations
			 SET occurrence_count = occurrence_count + 1,
			     last_seen = ?
			 WHERE category = ? AND field = ? AND value = ?`,
			stamp, category, field, value,
		); err != nil {
			return 0, err
		}
	}
	if _, err := s.db.Exec(
		`UPDATE behavioral_observations
		 SET status = 'candidate'
		 WHERE category = ? AND field = ? AND value = ?
		   AND occurrence_count >= 3 AND confidence > 60`,
		category, field, value,
	); err != nil {
		return 0, err
	}
	var id int64
	if err := s.db.QueryRow(
		`SELECT id FROM behavioral_observations
		 WHERE category = ? AND field = ? AND value = ?`,
		category, field, value,
	).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *SQLiteStore) ListObservations(includeStatuses []string) ([]model.BehavioralObservation, error) {
	if len(includeStatuses) == 0 {
		return []model.BehavioralObservation{}, nil
	}
	placeholders := make([]string, 0, len(includeStatuses))
	args := make([]any, 0, len(includeStatuses))
	for _, st := range includeStatuses {
		placeholders = append(placeholders, "?")
		args = append(args, st)
	}
	q := `SELECT id, category, field, value, confidence, occurrence_count, status, last_seen, created_at
		FROM behavioral_observations
		WHERE status IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY datetime(last_seen) DESC, id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BehavioralObservation
	for rows.Next() {
		obs, err := scanBehavioralObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetObservation(id int64) (*model.BehavioralObservation, error) {
	row := s.db.QueryRow(
		`SELECT id, category, field, value, confidence, occurrence_count, status, last_seen, created_at
		 FROM behavioral_observations WHERE id = ?`,
		id,
	)
	obs, err := scanBehavioralObservation(row)
	if err != nil {
		return nil, err
	}
	return &obs, nil
}

func (s *SQLiteStore) DismissObservation(id int64) error {
	_, err := s.db.Exec(
		`UPDATE behavioral_observations SET status = 'dismissed' WHERE id = ?`,
		id,
	)
	return err
}

func (s *SQLiteStore) Candidates() ([]model.BehavioralObservation, error) {
	return s.ListObservations([]string{"candidate"})
}

func scanBehavioralObservation(scanner interface{ Scan(dest ...any) error }) (model.BehavioralObservation, error) {
	var (
		obs                       model.BehavioralObservation
		lastSeenRaw, createdAtRaw string
	)
	if err := scanner.Scan(
		&obs.ID,
		&obs.Category,
		&obs.Field,
		&obs.Value,
		&obs.Confidence,
		&obs.OccurrenceCount,
		&obs.Status,
		&lastSeenRaw,
		&createdAtRaw,
	); err != nil {
		return model.BehavioralObservation{}, err
	}
	lastSeen, err := time.Parse(time.RFC3339, lastSeenRaw)
	if err != nil {
		return model.BehavioralObservation{}, fmt.Errorf("invalid behavioral_observations.last_seen: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339, createdAtRaw)
	if err != nil {
		return model.BehavioralObservation{}, fmt.Errorf("invalid behavioral_observations.created_at: %w", err)
	}
	obs.LastSeen = lastSeen
	obs.CreatedAt = createdAt
	return obs, nil
}

// ImportRecords inserts a batch of records, deduping each row on the
// composite key `(project_id, topic_key, sha256(content))`. Rows that match an
// existing key are counted as Skipped and not written. Empty content
// is also skipped (defensive — input parsers should already filter).
//
// The whole batch runs in a single transaction so a mid-batch failure
// rolls back cleanly. CreatedAt for new rows is stamped with time.Now
// at the store layer to avoid clock skew between caller and DB.
func (s *SQLiteStore) ImportRecords(rows []model.ImportRecord) (model.ImportResult, error) {
	res := model.ImportResult{}
	if len(rows) == 0 {
		return res, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	active, err := s.GetActive()
	if err != nil {
		return res, err
	}
	activeProjectID := active.ID
	for _, r := range rows {
		content := r.Content
		if strings.TrimSpace(content) == "" {
			res.Skipped++
			continue
		}
		hash := contentHash(content)
		// Dedupe lookup. COALESCE so empty/null topic_key both compare
		// against the literal empty string, keeping pure-content imports
		// idempotent (spec scenario "Re-import produces no duplicates").
		var existing int64
		err := tx.QueryRow(
			`SELECT id FROM memory_items
			 WHERE project_id = ?
			   AND COALESCE(topic_key, '') = COALESCE(?, '')
			   AND content_hash = ?
			 LIMIT 1`,
			activeProjectID, r.TopicKey, hash,
		).Scan(&existing)
		if err == nil {
			res.Skipped++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}

		typ := r.Type
		if strings.TrimSpace(typ) == "" {
			typ = "manual"
		}
		scope := r.Scope
		if strings.TrimSpace(scope) == "" {
			scope = "project"
		}
		if _, err := tx.Exec(
			`INSERT INTO memory_items(content, created_at, updated_at, title, type, topic_key, scope, content_hash, project_id)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			content, now, now, r.Title, typ, r.TopicKey, scope, hash, activeProjectID,
		); err != nil {
			return res, err
		}
		res.Inserted++
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// contentHash returns the lowercase hex sha256 of the input. Used as
// the second component of the import dedupe key.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Backup writes a portable, self-contained snapshot of the live database
// to dst using SQLite's `VACUUM INTO` — atomic, schema-aware, and safe
// to run while connections are open. The destination file MUST NOT exist
// (VACUUM INTO refuses to overwrite); callers should pick a fresh path
// with a timestamp suffix per `pre-migration-<ts>.db` convention.
//
// Errors:
//   - parent directory missing  -> bubble up the OS error verbatim
//   - destination already exists -> SQLite error (caller should rename)
//   - any I/O fault              -> rollback is automatic; live DB untouched
func (s *SQLiteStore) Backup(dst string) error {
	if strings.TrimSpace(dst) == "" {
		return errors.New("backup destination is empty")
	}
	// Validate parent dir exists so we surface a clear error instead of
	// SQLite's terse "unable to open database file".
	if _, err := os.Stat(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("backup destination dir: %w", err)
	}
	// VACUUM INTO requires a string literal; use parameter substitution
	// at the Go layer (path can contain quotes only on weird filesystems
	// — we escape defensively).
	escaped := strings.ReplaceAll(dst, "'", "''")
	if _, err := s.db.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// Restore replaces the live database file with the contents of src by
// closing the active connection, copying bytes, and reopening. The
// caller is responsible for re-running Init() afterwards if the schema
// version of the artifact is older than CurrentSchemaVersion — Init is
// forward-only and will migrate transparently.
//
// Errors:
//   - src missing      -> os.Stat error before mutation
//   - copy failure     -> live DB is restored from a temp side-copy taken
//     before the swap, so a half-failed restore does
//     not leave the user with no DB at all
func (s *SQLiteStore) Restore(src string) error {
	if strings.TrimSpace(src) == "" {
		return errors.New("restore source is empty")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("restore source: %w", err)
	}
	live := s.dbPath
	if strings.TrimSpace(live) == "" {
		return errors.New("store has no on-disk path; cannot restore")
	}
	// Take a side-copy of the live DB so we can roll back if the restore
	// copy fails mid-way. Same dir to avoid cross-FS rename.
	side := live + ".restore-bak"
	if err := copyFile(live, side); err != nil {
		// If the live file doesn't exist yet, that's OK — we'll create
		// it fresh from the artifact below.
		if !os.IsNotExist(err) {
			return fmt.Errorf("snapshot live db: %w", err)
		}
	}
	// Close the active connection so the file handle releases the OS
	// lock before we overwrite it (Windows would refuse otherwise; UNIX
	// is more lenient but locks can still cause readback weirdness).
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close live db: %w", err)
	}
	if err := copyFile(src, live); err != nil {
		// Roll back: put the side-copy back in place so the user isn't
		// left with a broken DB. Best-effort — log via err return only.
		_ = copyFile(side, live)
		_ = reopenStore(s)
		return fmt.Errorf("copy artifact: %w", err)
	}
	_ = os.Remove(side)
	return reopenStore(s)
}

// copyFile copies src to dst using a buffered io.Copy. dst is created
// with 0o644; the caller is responsible for ensuring the parent dir
// exists.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// reopenStore re-opens the underlying *sql.DB on s.dbPath so the store
// is usable after a Restore. Kept private — Backup/Restore are the only
// callers.
func reopenStore(s *SQLiteStore) error {
	db, err := sql.Open("sqlite", s.dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		return fmt.Errorf("reopen sqlite: %w", err)
	}
	if err := applyPragmas(db); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("reopen sqlite ping: %w", err)
	}
	s.db = db
	return nil
}

// DoctorReport is an alias for model.DoctorReport — the diagnostic
// snapshot type lives in `model` as the shared domain contract. Each
// axis maps 1:1 to a check the M3 doctor surface
// advertises:
//   - SchemaVersion: numeric migration target reached
//   - FTSHealthy:    memory_fts table queryable + sync triggers present
//   - IntegrityOK:   PRAGMA integrity_check returned 'ok' on every row
//   - MemoryItemsCount / SessionsCount: row counts for the M1+M3 tables
//   - Summary: short human-readable line for CLI/MCP presentation,
//     containing every axis name so callers can render a one-shot status
type DoctorReport = model.DoctorReport

// Doctor runs the full diagnostic suite and returns a DoctorReport. All
// checks are read-only — Doctor MUST never mutate the database (callers
// rely on this to run it from MCP without backup safeguards).
func (s *SQLiteStore) Doctor() (model.DoctorReport, error) {
	report := model.DoctorReport{}

	// 1. Schema version. Read from the schema_version table that Init
	// stamps on every successful migration; max(version) is current.
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`,
	).Scan(&report.SchemaVersion); err != nil {
		return report, fmt.Errorf("read schema version: %w", err)
	}

	// 2. FTS health: a SELECT against memory_fts that succeeds means the
	// virtual table exists and the FTS5 module is loaded. We don't check
	// triggers explicitly — a healthy SELECT is the user-facing contract.
	var ftsCount int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_fts WHERE 1=0`,
	).Scan(&ftsCount); err == nil {
		report.FTSHealthy = true
	}

	// 3. Integrity check. SQLite returns 'ok' on a single row when clean,
	// otherwise one row per problem. We collect every non-'ok' message
	// so CLI surface can show them verbatim.
	rows, err := s.db.Query("PRAGMA integrity_check")
	if err != nil {
		return report, fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	report.IntegrityOK = true
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return report, err
		}
		if strings.TrimSpace(strings.ToLower(msg)) != "ok" {
			report.IntegrityOK = false
			report.IntegrityMessages = append(report.IntegrityMessages, msg)
		}
	}

	// 4. Row counts. Best-effort — missing tables (older schema) count as 0.
	_ = s.db.QueryRow("SELECT count(*) FROM memory_items").Scan(&report.MemoryItemsCount)
	_ = s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&report.SessionsCount)

	// 5. Render a single-line summary that names every axis. Order is
	// stable so tests + screen readers can rely on it.
	integrity := "ok"
	if !report.IntegrityOK {
		integrity = fmt.Sprintf("FAIL (%d issues)", len(report.IntegrityMessages))
	}
	fts := "healthy"
	if !report.FTSHealthy {
		fts = "unavailable"
	}
	report.Summary = fmt.Sprintf(
		"schema_version=%d  fts=%s  integrity=%s  memory_items=%d  sessions=%d",
		report.SchemaVersion, fts, integrity,
		report.MemoryItemsCount, report.SessionsCount,
	)
	return report, nil
}
