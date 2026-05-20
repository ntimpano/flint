package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flint/internal/model"
	"flint/internal/parity"
)

type Store interface {
	Init() error
	Save(content string, createdAt time.Time) (int64, error)
	Recall(query string, limit int) ([]model.MemoryItem, error)
	List(limit int) ([]model.MemoryItem, error)
	Get(id int64) (model.MemoryItem, error)
	Update(id int64, content string, updatedAt time.Time) (bool, error)
	Delete(id int64) (bool, error)
	Close() error
}

// GraphRecallStore extends Store with the PR4 graph-aware recall path.
// Optional capability — same defensive type-assert pattern as
// FilterStore / RelationStore. The service routes to RecallGraphAware
// only when (a) the env feature flag NTCLI_FF_GRAPH=1 is set AND (b)
// the underlying store implements this interface. Legacy fakes that
// don't implement it degrade to the plain Recall path so the FF stays
// safe to flip in mixed environments.
type GraphRecallStore interface {
	RecallGraphAware(opts model.RecallOptions) ([]model.MemoryItem, error)
}

// graphRecallEnabled reports whether the PR4 graph-aware recall path
// is opted in via env. Implemented as a function (not a constant) so
// tests can flip the env mid-run, mirroring the MCP-side helper.
func graphRecallEnabled() bool {
	return strings.TrimSpace(os.Getenv("NTCLI_FF_GRAPH")) == "1"
}

// MetadataStore extends Store with structured-memory operations introduced by
// the M1 milestone. Implementations of Store are not required to satisfy this
// interface; callers MUST type-assert and degrade gracefully when the backing
// store does not support metadata (e.g. lightweight in-memory test fakes).
type MetadataStore interface {
	SaveWithMeta(req model.SaveRequest) (int64, error)
}

// SessionStore extends Store with the lifecycle log introduced by M3.
// Implementations of Store are NOT required to satisfy this interface;
// the service layer type-asserts and fails fast for legacy fakes (same
// pattern as MetadataStore / FilterStore).
type SessionStore interface {
	SessionStart(id string, at time.Time) error
	SessionSummary(id, summary string, at time.Time) error
	SessionEnd(id string, at time.Time) error
	SessionEvents(id string) ([]model.SessionEvent, error)
	ActiveSessionID() (string, error)
}

// BehavioralStore extends Store with behavioral-learning operations.
// Optional capability; service methods type-assert defensively.
type BehavioralStore interface {
	RecordObservation(category, field, value string, confidence int, now time.Time) (int64, error)
	ListObservations(includeStatuses []string) ([]model.BehavioralObservation, error)
	GetObservation(id int64) (*model.BehavioralObservation, error)
	DismissObservation(id int64) error
	Candidates() ([]model.BehavioralObservation, error)
}

// ImportStore extends Store with the idempotent import path introduced
// by M3. Dedupe key is `(topic_key, sha256(content))`. Same defensive
// type-assert pattern as the other M3 capability interfaces.
type ImportStore interface {
	ImportRecords(rows []model.ImportRecord) (model.ImportResult, error)
}

// BackupStore extends Store with portable snapshot + restore for the
// M3 backup/restore feature. Implementations MUST produce a single-file
// artifact that can be moved across machines and restored losslessly.
// Defensive type-assert pattern: Service.Backup/Restore surface a clear
// capability error rather than a nil-method panic when the underlying
// store is a stub or doesn't support snapshots.
type BackupStore interface {
	Backup(dst string) error
	Restore(src string) error
}

// AutopilotSessionCloseThreshold is the spec-defined floor for the
// rolling close rate (capability: workflow-autopilot). Exposed as a
// package constant so callers and tests share one source of truth.
const AutopilotSessionCloseThreshold = 0.9

// ComputeAutopilotSessionCloseRate returns the fraction of distinct
// session ids whose lifecycle log contains BOTH a non-empty `summary`
// row AND an `end` row. Empty input returns 0.
//
// This is the pure helper that the doctor surface uses to populate
// DoctorReport.AutopilotSessionCloseRate. Keeping it pure means tests
// don't need a live store and the formula is auditable from one place.
func ComputeAutopilotSessionCloseRate(events []model.SessionEvent) float64 {
	if len(events) == 0 {
		return 0
	}
	type sessState struct {
		hasSummary bool
		hasEnd     bool
	}
	by := map[string]*sessState{}
	for _, e := range events {
		st, ok := by[e.SessionID]
		if !ok {
			st = &sessState{}
			by[e.SessionID] = st
		}
		switch e.Kind {
		case "summary":
			if strings.TrimSpace(e.Summary) != "" {
				st.hasSummary = true
			}
		case "end":
			st.hasEnd = true
		}
	}
	if len(by) == 0 {
		return 0
	}
	closed := 0
	for _, st := range by {
		if st.hasSummary && st.hasEnd {
			closed++
		}
	}
	return float64(closed) / float64(len(by))
}

// DoctorStore extends Store with the M3 diagnostic surface. Doctor
// MUST be read-only — callers (CLI, MCP) rely on this guarantee to run
// it without backup safeguards.
type DoctorStore interface {
	Doctor() (model.DoctorReport, error)
}

// FilterStore extends Store with the structured read paths introduced by
// the M2 milestone (recall with metadata filters + recent-context view).
// Same defensive pattern as MetadataStore: callers MUST type-assert and
// fail fast rather than silently degrade — silent degradation would let
// filter-aware callers receive unfiltered rows and corrupt downstream
// invariants.
type FilterStore interface {
	RecallFiltered(opts model.RecallOptions) ([]model.MemoryItem, error)
	Context(n int, scope string) ([]model.MemoryItem, error)
	// ContextFiltered is the project-scoped extension of Context (PR2b).
	ContextFiltered(opts model.ContextOptions) ([]model.MemoryItem, error)
	// ListFiltered is the project-scoped extension of List (PR2b).
	ListFiltered(opts model.ListOptions) ([]model.MemoryItem, error)
}

// ErrNotFound is the stable sentinel returned when a note id does not exist.
var ErrNotFound = errors.New("note not found")

type Service struct {
	repo            Store
	activeProjectID int64 // resolved at boot, 0 = no project scoping
	// ProjectEngine handles project detection/switch/list/current.
	// Wired at boot when the store implements ProjectStore.
	// May be nil when running against legacy test fakes.
	ProjectEng ProjectEngine
}

// SetActiveProject injects the active project id into the service so all
// read/write paths are automatically scoped. Called once at boot from main
// after Init() resolves the active_project pointer.
func (s *Service) SetActiveProject(id int64) {
	s.activeProjectID = id
}

// ActiveProjectID returns the currently active project id (0 = unscoped).
func (s *Service) ActiveProjectID() int64 {
	return s.activeProjectID
}

func NewService(repo Store) *Service {
	svc := &Service{repo: repo}
	// Auto-wire the project engine when the store implements ProjectStore.
	// This covers both production (SQLiteStore) and test doubles that embed
	// ProjectStore-like capabilities.
	if ps, ok := repo.(ProjectStore); ok {
		svc.ProjectEng = newProjectEngine(ps, realGitResolver)
	}
	return svc
}

func (s *Service) Init() error {
	return s.repo.Init()
}

func (s *Service) Save(content string) (int64, error) {
	clean := strings.TrimSpace(content)
	if clean == "" {
		return 0, errors.New("content is empty")
	}
	// When a project is active and the store supports metadata, route through
	// SaveWithMeta so the row is stamped with the active project_id.
	// This satisfies the spec requirement: "default Save path scoped by active project".
	if s.activeProjectID > 0 {
		if meta, ok := s.repo.(MetadataStore); ok {
			return meta.SaveWithMeta(model.SaveRequest{
				Content:   clean,
				Type:      "manual",
				Scope:     "project",
				ProjectID: s.activeProjectID,
				CreatedAt: time.Now().UTC(),
			})
		}
	}
	return s.repo.Save(clean, time.Now().UTC())
}

// SaveWithMeta persists a note with optional structured metadata. Empty
// `Type` defaults to "manual" and empty `Scope` defaults to "project" per
// design.md §metadata-defaults — the spec calls these out explicitly so
// downstream filters can rely on the defaults being non-empty.
//
// If the underlying store does not implement MetadataStore, the call fails
// fast rather than silently dropping metadata: silent drops would let
// downstream consumers see partially-populated rows and corrupt the
// upsert-by-topic invariant.
func (s *Service) SaveWithMeta(req model.SaveRequest) (int64, error) {
	clean := strings.TrimSpace(req.Content)
	if clean == "" {
		return 0, errors.New("content is empty")
	}
	meta, ok := s.repo.(MetadataStore)
	if !ok {
		return 0, errors.New("store does not support metadata operations")
	}

	if strings.TrimSpace(req.Type) == "" {
		req.Type = "manual"
	}
	if strings.TrimSpace(req.Scope) == "" {
		req.Scope = "project"
	}
	req.Content = clean
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	// Stamp active project if not explicitly set by the caller
	if req.ProjectID == 0 && s.activeProjectID > 0 {
		req.ProjectID = s.activeProjectID
	}
	return meta.SaveWithMeta(req)
}

func (s *Service) Recall(query string, limit int) ([]model.MemoryItem, error) {
	clean := strings.TrimSpace(query)
	if clean == "" {
		return nil, errors.New("query is empty")
	}
	if limit <= 0 {
		limit = 10
	}
	// When an active project is set, route through RecallWithOptions so the
	// project_id filter is applied. This satisfies the spec requirement:
	// "default Recall path scoped by active project".
	if s.activeProjectID > 0 {
		if _, ok := s.repo.(FilterStore); ok {
			return s.RecallWithOptions(model.RecallOptions{Query: clean, Limit: limit})
		}
	}
	// PR4: when NTCLI_FF_GRAPH=1 AND the store supports graph-aware
	// recall, route to RecallGraphAware so the same surface gets
	// supersedes-suppression and bounded boost. Capability missing →
	// silent fall-through to plain Recall (legacy fakes still work).
	if graphRecallEnabled() {
		if gs, ok := s.repo.(GraphRecallStore); ok {
			return gs.RecallGraphAware(model.RecallOptions{
				Query:      clean,
				Limit:      limit,
				GraphAware: true,
			})
		}
	}
	return s.repo.Recall(clean, limit)
}

// RecallWithOptions runs a recall with optional metadata + date-range
// filters. Validation mirrors Recall(): empty/whitespace-only queries
// are rejected before the store is touched. Limit ≤0 defaults to 10.
//
// If the underlying store does not implement FilterStore, the call
// fails fast with an explicit error rather than degrading to plain
// Recall — silent degradation would let filter-aware callers see
// unfiltered rows.
func (s *Service) RecallWithOptions(opts model.RecallOptions) ([]model.MemoryItem, error) {
	clean := strings.TrimSpace(opts.Query)
	hasTopicKey := strings.TrimSpace(opts.TopicKey) != ""
	if clean == "" && !hasTopicKey {
		return nil, errors.New("query is empty")
	}
	opts.Query = clean
	opts.Type = strings.TrimSpace(opts.Type)
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	// PR4: same routing rule as Recall(). When the FF is ON AND the
	// store implements GraphRecallStore, dispatch the graph-aware
	// path — it honors IncludeSuperseded for free. When the FF is
	// OFF, behavior is byte-identical to PR2b.
	if graphRecallEnabled() {
		if gs, ok := s.repo.(GraphRecallStore); ok {
			opts.GraphAware = true
			return gs.RecallGraphAware(opts)
		}
	}
	filt, ok := s.repo.(FilterStore)
	if !ok {
		return nil, errors.New("store does not support filter operations")
	}
	// Inject active project scoping when not explicitly set
	if opts.ProjectID == 0 && s.activeProjectID > 0 {
		opts.ProjectID = s.activeProjectID
	}
	return filt.RecallFiltered(opts)
}

// Context returns the most recent N notes (newest-first), optionally
// scoped to a single Scope value. n ≤0 defaults to 10. Scope is
// trimmed; empty string disables the scope filter. Same defensive
// FilterStore type-assert as RecallWithOptions.
func (s *Service) Context(n int, scope string) ([]model.MemoryItem, error) {
	return s.ContextOpts(n, scope, false)
}

// ContextOpts is the full version of Context that exposes the AllProjects
// bypass flag. When allProjects is true, the active project filter is skipped
// so callers can read across all project contexts.
func (s *Service) ContextOpts(n int, scope string, allProjects bool) ([]model.MemoryItem, error) {
	filt, ok := s.repo.(FilterStore)
	if !ok {
		return nil, errors.New("store does not support context operations")
	}
	if n <= 0 {
		n = 10
	}
	scope = strings.TrimSpace(scope)
	return filt.ContextFiltered(model.ContextOptions{
		N:           n,
		Scope:       scope,
		ProjectID:   s.activeProjectID,
		AllProjects: allProjects,
	})
}

func (s *Service) List(limit int) ([]model.MemoryItem, error) {
	return s.ListOpts(limit, false)
}

// ListOpts is the full list surface with optional all-project bypass.
// When an active project is set and the store supports FilterStore, list is
// scoped to that project by default. allProjects=true bypasses the scope.
func (s *Service) ListOpts(limit int, allProjects bool) ([]model.MemoryItem, error) {
	if limit <= 0 {
		limit = 10
	}
	if s.activeProjectID > 0 {
		if filt, ok := s.repo.(FilterStore); ok {
			return filt.ListFiltered(model.ListOptions{
				Limit:       limit,
				ProjectID:   s.activeProjectID,
				AllProjects: allProjects,
			})
		}
	}
	return s.repo.List(limit)
}

func (s *Service) Get(id int64) (model.MemoryItem, error) {
	if id <= 0 {
		return model.MemoryItem{}, errors.New("id must be positive")
	}
	return s.repo.Get(id)
}

func (s *Service) Update(id int64, content string) (bool, error) {
	if id <= 0 {
		return false, errors.New("id must be positive")
	}
	clean := strings.TrimSpace(content)
	if clean == "" {
		return false, errors.New("content is empty")
	}
	return s.repo.Update(id, clean, time.Now().UTC())
}

func (s *Service) Delete(id int64) (bool, error) {
	if id <= 0 {
		return false, errors.New("id must be positive")
	}
	return s.repo.Delete(id)
}

// SessionStart appends a "start" lifecycle row tagged with id. The
// service trims the id and stamps the current UTC time; empty/
// whitespace-only ids are rejected so unrelated sessions cannot be
// silently merged. Fails fast if the store does not implement
// SessionStore (same defensive pattern as MetadataStore/FilterStore).
func (s *Service) SessionStart(id string) error {
	clean, err := s.validateSessionID(id)
	if err != nil {
		return err
	}
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return errors.New("store does not support session operations")
	}
	return sess.SessionStart(clean, time.Now().UTC())
}

// SessionEnd appends an "end" lifecycle row. Multiple ends are tolerated
// at the store layer — interpretation is the reader's responsibility.
func (s *Service) SessionEnd(id string) error {
	clean, err := s.validateSessionID(id)
	if err != nil {
		return err
	}
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return errors.New("store does not support session operations")
	}
	return sess.SessionEnd(clean, time.Now().UTC())
}

// SessionEndStrict implements the workflow-autopilot spec scenario
// "Missing summary blocks clean end". When invoked without --force,
// the session id MUST have at least one `summary` lifecycle row;
// otherwise the call returns an error containing `summary_required`
// and the underlying SessionEnd is NOT invoked (no end-row written).
//
// Callers that intentionally want to close without a summary call
// SessionEnd directly (the --force path).
func (s *Service) SessionEndStrict(id string) error {
	clean, err := s.validateSessionID(id)
	if err != nil {
		return err
	}
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return errors.New("store does not support session operations")
	}
	events, err := sess.SessionEvents(clean)
	if err != nil {
		return err
	}
	hasSummary := false
	for _, e := range events {
		if e.Kind == "summary" && strings.TrimSpace(e.Summary) != "" {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		return errors.New("summary_required: session has no summary row; call SessionSummary first or use --force")
	}
	return sess.SessionEnd(clean, time.Now().UTC())
}

// SessionSummary appends a "summary" lifecycle row. Empty/whitespace-only
// summaries are rejected — a summary row with no content has no value
// for downstream consumers and would just clutter the log.
func (s *Service) SessionSummary(id, summary string) error {
	clean, err := s.validateSessionID(id)
	if err != nil {
		return err
	}
	cleanSummary := strings.TrimSpace(summary)
	if cleanSummary == "" {
		return errors.New("summary is empty")
	}
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return errors.New("store does not support session operations")
	}
	return sess.SessionSummary(clean, cleanSummary, time.Now().UTC())
}

// SessionEvents returns every lifecycle row tagged to id, in insertion
// order. Validation mirrors the write paths.
func (s *Service) SessionEvents(id string) ([]model.SessionEvent, error) {
	clean, err := s.validateSessionID(id)
	if err != nil {
		return nil, err
	}
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return nil, errors.New("store does not support session operations")
	}
	return sess.SessionEvents(clean)
}

// SessionStore exposes the optional session-capable store adapter.
func (s *Service) SessionStore() SessionStore {
	sess, ok := s.repo.(SessionStore)
	if !ok {
		return nil
	}
	return sess
}

// BehavioralStore exposes the optional behavioral-capable store adapter.
func (s *Service) BehavioralStore() BehavioralStore {
	bs, ok := s.repo.(BehavioralStore)
	if !ok {
		return nil
	}
	return bs
}

// RecordObservationFromMarker parses a behavioral marker and persists it.
func (s *Service) RecordObservationFromMarker(marker string) (int64, error) {
	category, field, value, confidence, err := parseObservationMarker(marker)
	if err != nil {
		return 0, err
	}
	bs := s.BehavioralStore()
	if bs == nil {
		return 0, errors.New("store does not support behavioral operations")
	}
	return bs.RecordObservation(category, field, value, confidence, time.Now().UTC())
}

func (s *Service) validateSessionID(id string) (string, error) {
	clean := strings.TrimSpace(id)
	if clean == "" {
		return "", errors.New("session id is empty")
	}
	return clean, nil
}

// importJSONRecord mirrors ImportRecord with JSON tags. Kept private —
// callers send raw bytes and the service handles the parse so we can
// add MD/CSV later without exposing parser details.
type importJSONRecord struct {
	Content  string `json:"content"`
	Title    string `json:"title,omitempty"`
	Type     string `json:"type,omitempty"`
	TopicKey string `json:"topic_key,omitempty"`
	Scope    string `json:"scope,omitempty"`
}

// ImportJSON parses a JSON array of records and either writes them via
// ImportRecords (idempotent dedupe at the store) or, in dry-run mode,
// returns the planned insert count without touching the store.
//
// Empty/whitespace-only `content` is dropped client-side so partially
// malformed files still surface valid rows. The store does its own
// dedupe by `(topic_key, sha256(content))` — this method only handles
// parsing + dry-run accounting.
func (s *Service) ImportJSON(data []byte, dryRun bool) (model.ImportResult, error) {
	var records []importJSONRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return model.ImportResult{}, err
	}
	rows := make([]model.ImportRecord, 0, len(records))
	for _, r := range records {
		if strings.TrimSpace(r.Content) == "" {
			continue
		}
		rows = append(rows, model.ImportRecord{
			Content:  r.Content,
			Title:    r.Title,
			Type:     r.Type,
			TopicKey: r.TopicKey,
			Scope:    r.Scope,
		})
	}
	if dryRun {
		// In dry-run we report the local plan: every valid row is a
		// planned insert. The store's dedupe pass would refine this,
		// but we can't peek at the store without writing — that's the
		// honest contract per spec scenario "Dry-run reports without
		// writing".
		return model.ImportResult{Inserted: len(rows)}, nil
	}
	imp, ok := s.repo.(ImportStore)
	if !ok {
		return model.ImportResult{}, errors.New("store does not support import operations")
	}
	return imp.ImportRecords(rows)
}

// Backup creates a portable snapshot of the live store at dst. Returns
// a capability error if the underlying Store does not implement
// BackupStore (defensive type-assert mirrors session/import).
func (s *Service) Backup(dst string) error {
	clean := strings.TrimSpace(dst)
	if clean == "" {
		return errors.New("backup destination is empty")
	}
	bs, ok := s.repo.(BackupStore)
	if !ok {
		return errors.New("store does not support backup operations")
	}
	return bs.Backup(clean)
}

// Restore replaces the live store with the contents of src. Same
// capability check + trim semantics as Backup. A pre-restore safety
// snapshot is taken automatically into the default backup directory
// before the restore runs (task 4.4). If the snapshot fails, the
// restore is aborted to avoid data loss.
// Callers are expected to re-Init the service afterwards if the
// artifact predates the current schema version — Init is forward-only
// and idempotent.
func (s *Service) Restore(src string) error {
	clean := strings.TrimSpace(src)
	if clean == "" {
		return errors.New("restore source is empty")
	}
	bs, ok := s.repo.(BackupStore)
	if !ok {
		return errors.New("store does not support restore operations")
	}
	doSnapshot := func() error {
		snapPath, err := AutoBackupPath()
		if err != nil {
			return err
		}
		return bs.Backup(snapPath)
	}
	doRestore := func() error {
		return bs.Restore(clean)
	}
	return SafeRestore(doSnapshot, doRestore)
}

// Doctor returns the read-only diagnostic snapshot from the underlying
// store. Returns a capability error if the Store doesn't implement
// DoctorStore (defensive type-assert mirrors session/import/backup).
func (s *Service) Doctor() (model.DoctorReport, error) {
	ds, ok := s.repo.(DoctorStore)
	if !ok {
		return model.DoctorReport{}, errors.New("store does not support doctor diagnostics")
	}
	return ds.Doctor()
}

// systemClock satisfies parity.Clock with wall-clock time.Now in UTC.
// Kept private — production callers don't need to construct it.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// recallAdapter bridges the Service.Recall surface (returning
// MemoryItem slices) to the parity.Recaller interface (which only
// needs content strings). Keeping the adapter local to the app
// package avoids leaking parity types into the Store interface and
// keeps the store layer ignorant of the harness contract.
type recallAdapter struct {
	svc *Service
}

func (r *recallAdapter) Recall(query string, limit int) ([]string, error) {
	items, err := r.svc.Recall(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Content)
	}
	return out, nil
}

// RunContinuityHarness loads the fixture suite at fixturePath, replays
// every query through the live Service.Recall path, and writes the
// resulting baseline.json to outPath. The returned baseline mirrors
// the file content (same struct, indented JSON for runbook diffing).
//
// The harness is read-only — it never writes to the store. This is
// the surface consumed by `nt-cli parity continuity` and by PR5's
// post-feature replay step (which compares against this baseline to
// assert delta_pct ≤ -35).
func (s *Service) RunContinuityHarness(fixturePath, outPath string) (model.ContinuityBaseline, error) {
	queries, err := parity.LoadQueries(fixturePath)
	if err != nil {
		return model.ContinuityBaseline{}, err
	}
	baseline, err := parity.ComputeContinuity(queries, &recallAdapter{svc: s}, systemClock{})
	if err != nil {
		return model.ContinuityBaseline{}, err
	}
	// Indent the file output so runbook reviewers can diff baselines
	// across releases without a separate jq step.
	body, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return model.ContinuityBaseline{}, err
	}
	if outPath != "" {
		if err := os.WriteFile(outPath, body, 0o644); err != nil {
			return model.ContinuityBaseline{}, err
		}
	}
	return baseline, nil
}

func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
			home = h
		} else {
			home = "/tmp"
		}
	}
	dir := filepath.Join(home, ".flint")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "flint.db"), nil
}
