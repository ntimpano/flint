package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"flint/internal/model"

	_ "modernc.org/sqlite"
)

type BehaviorSQLiteStore struct {
	db *sql.DB
}

func NewBehaviorSQLiteStore(dbPath string) (*BehaviorSQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
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

	if _, err := db.Exec(`
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
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_behavioral_obs_unique
			ON behavioral_observations(category, field, value)
	`); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &BehaviorSQLiteStore{db: db}, nil
}

func (s *BehaviorSQLiteStore) Close() error {
	return s.db.Close()
}

func (s *BehaviorSQLiteStore) RecordObservation(category, field, value string, confidence int, now time.Time) (int64, error) {
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

func (s *BehaviorSQLiteStore) ListObservations(includeStatuses []string) ([]model.BehavioralObservation, error) {
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

func (s *BehaviorSQLiteStore) GetObservation(id int64) (*model.BehavioralObservation, error) {
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

func (s *BehaviorSQLiteStore) DismissObservation(id int64) error {
	_, err := s.db.Exec(
		`UPDATE behavioral_observations SET status = 'dismissed' WHERE id = ?`,
		id,
	)
	return err
}

func (s *BehaviorSQLiteStore) Candidates() ([]model.BehavioralObservation, error) {
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
