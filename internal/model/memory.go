package model

import "time"

// DoctorReport mirrors the store-layer diagnostic snapshot. Re-declared
// at the service layer (instead of re-exporting the store type) so the
// `app` package stays independent of `store` — keeping the same
// dependency direction the rest of the package follows.
type DoctorReport struct {
	SchemaVersion     int
	FTSHealthy        bool
	IntegrityOK       bool
	IntegrityMessages []string
	MemoryItemsCount  int
	SessionsCount     int
	Summary           string

	// AutopilotSessionCloseRate is the rolling fraction (∈ [0,1]) of
	// sessions in the active window that closed cleanly (both a
	// `summary` row and an `end` row). Spec capability:
	// workflow-autopilot — "Doctor surfaces autopilot rate".
	// Pairs with AutopilotThreshold for the doctor surface.
	AutopilotSessionCloseRate float64
	// AutopilotThreshold is the spec floor (0.9 = 90% of sessions
	// must close cleanly over the rolling window). Surfaced verbatim
	// so doctor JSON consumers can compute pass/fail without hard-
	// coding the constant on their side.
	AutopilotThreshold float64
}

// ContextOptions carries parameters for the Context surface when
// project-scoped filtering is required.
type ContextOptions struct {
	N           int
	Scope       string
	ProjectID   int64 // 0 = no filter; >0 = filter by project_id
	AllProjects bool  // true = bypass ProjectID filter
}

// ListOptions carries parameters for the List surface when project-scoped
// filtering is required.
type ListOptions struct {
	Limit       int
	ProjectID   int64 // 0 = no filter; >0 = filter by project_id
	AllProjects bool  // true = bypass ProjectID filter
}

// RecallOptions carries the optional filter dimensions accepted by the
// M2 recall surface. Zero-value fields are treated as "unbounded":
//   - Query: required, non-empty after trim (validated by Service).
//   - Type:  empty string = no type filter.
//   - Since/Until: zero time = no lower/upper bound on created_at.
//   - Limit: ≤0 defaults to 10 at the service layer.
//   - ProjectID: 0 = no project filter; >0 = only rows for that project.
//   - AllProjects: true = bypass ProjectID filter (cross-project read).
type RecallOptions struct {
	Query string
	Type  string
	Since time.Time
	Until time.Time
	Limit int
	// TopicKey filters recall rows by exact topic_key when non-empty.
	TopicKey string

	// ProjectID scopes the recall to a specific project. 0 = no filter.
	ProjectID int64
	// AllProjects bypasses the ProjectID filter when true.
	AllProjects bool

	// IncludeSuperseded opts back into rows that have been superseded
	// by another row (predecessors of a `supersedes` edge). When false
	// (default) RecallGraphAware suppresses them so the surface only
	// shows current revisions. The plain Recall / RecallFiltered paths
	// ignore this field — supersedes-aware filtering is exclusive to
	// the graph-aware path.
	IncludeSuperseded bool

	// GraphAware requests the graph-aware ranking path. Wired by the
	// service layer based on the NTCLI_FF_GRAPH feature flag — the
	// store reads it directly so legacy fakes that don't implement
	// graph capability never see this option engaged.
	GraphAware bool
}

// SaveRequest carries the optional metadata fields accepted by the M1 save
// surface. CreatedAt MUST be set by the caller; the service is responsible
// for stamping the current UTC time when invoked through the public API.
type SaveRequest struct {
	Content   string
	Title     string
	Type      string
	TopicKey  string
	Scope     string
	CreatedAt time.Time
	// ProjectID stamps the active project on the saved row.
	// 0 means "no project" (legacy rows); >0 scopes the row.
	ProjectID int64
}

type MemoryItem struct {
	ID        int64
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
	// M1 metadata fields. Empty string is the documented "unset" marker so
	// the schema additions stay backward-compatible with existing callers.
	Title    string
	Type     string
	TopicKey string
	Scope    string
}

// ImportRecord is a single row queued for the M3 import bridge. Empty
// metadata fields default to type=manual / scope=project at the store
// layer (mirrors SaveWithMeta behaviour). CreatedAt zero = stamp now.
type ImportRecord struct {
	Content  string
	Title    string
	Type     string
	TopicKey string
	Scope    string
}

// ImportResult is the count summary returned by ImportRecords. Inserted +
// Skipped MUST equal len(input). Skipped covers both dedupe hits and
// validation drops (empty content) so callers can render a single
// "no-op" status from a single field.
type ImportResult struct {
	Inserted int
	Skipped  int
}
