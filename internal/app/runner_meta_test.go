package app_test

import (
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// metaMemStore extends the existing memStore (defined in runner_test.go)
// with MetadataStore so the CLI runner can exercise the metadata-aware
// save path under test. It is intentionally minimal: only the fields
// observed by the metadata save tests are persisted.
type metaMemStore struct {
	*memStore
	lastReq      app.SaveRequest
	saveMetaHits int
}

func newMetaMemStore() *metaMemStore {
	return &metaMemStore{memStore: newMemStore()}
}

func (m *metaMemStore) SaveWithMeta(req app.SaveRequest) (int64, error) {
	m.saveMetaHits++
	m.lastReq = req
	id := m.memStore.nextID
	m.memStore.nextID++
	m.memStore.items[id] = app.MemoryItem{
		ID:        id,
		Content:   req.Content,
		CreatedAt: req.CreatedAt,
		UpdatedAt: req.CreatedAt,
		Title:     req.Title,
		Type:      req.Type,
		TopicKey:  req.TopicKey,
		Scope:     req.Scope,
	}
	return id, nil
}

// TestRunCLI_SaveWithMetadataFlags proves task 1.6: the CLI accepts
// `--title --type --topic-key --scope` as additive flags and forwards
// them through the service layer onto the store.
func TestRunCLI_SaveWithMetadataFlags(t *testing.T) {
	store := newMetaMemStore()
	svc := app.NewService(store, nil)
	var stdout2, stderr2 strings.Builder
	exit := app.RunCLI(svc, []string{"save",
		"--title=Auth Model",
		"--type=decision",
		"--topic-key=arch/auth",
		"--scope=personal",
		"decision body",
	}, &stdout2, &stderr2)
	if exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", exit, stderr2.String())
	}
	if store.saveMetaHits != 1 {
		t.Fatalf("expected SaveWithMeta hit, got %d", store.saveMetaHits)
	}
	if store.lastReq.Title != "Auth Model" {
		t.Fatalf("title: want %q got %q", "Auth Model", store.lastReq.Title)
	}
	if store.lastReq.Type != "decision" {
		t.Fatalf("type: want %q got %q", "decision", store.lastReq.Type)
	}
	if store.lastReq.TopicKey != "arch/auth" {
		t.Fatalf("topic_key: want %q got %q", "arch/auth", store.lastReq.TopicKey)
	}
	if store.lastReq.Scope != "personal" {
		t.Fatalf("scope: want %q got %q", "personal", store.lastReq.Scope)
	}
	if store.lastReq.Content != "decision body" {
		t.Fatalf("content: want %q got %q", "decision body", store.lastReq.Content)
	}
	if !strings.Contains(stdout2.String(), "saved #") {
		t.Fatalf("expected confirmation on stdout, got %q", stdout2.String())
	}
}

// TestRunCLI_SaveWithoutFlagsStaysBackwardCompatible proves task 1.6 second
// half: when no metadata flags are present, the CLI MUST still drive the
// legacy Save() path so existing scripts and test fakes that do not
// implement MetadataStore keep working.
func TestRunCLI_SaveWithoutFlagsStaysBackwardCompatible(t *testing.T) {
	store := newMemStore() // does NOT implement MetadataStore

	code, stdout, stderr := runCLI(t, store, "save", "plain note")
	if code != 0 {
		t.Fatalf("expected exit 0 with legacy save, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "saved #") {
		t.Fatalf("expected save confirmation, got %q", stdout)
	}
	// The note must be findable through the legacy Recall path.
	items, _ := store.Recall("plain", 10)
	if len(items) != 1 {
		t.Fatalf("expected 1 saved item, got %d", len(items))
	}
	if items[0].Content != "plain note" {
		t.Fatalf("content drift: %q", items[0].Content)
	}
}

// TestRunCLI_SaveOnlyContentArgIsRequired proves the parser still rejects
// flag-only invocations without any content body so the existing usage
// contract (`nt-cli save "note"`) is preserved.
func TestRunCLI_SaveOnlyContentArgIsRequired(t *testing.T) {
	store := newMetaMemStore()
	svc := app.NewService(store, nil)
	var stdout, stderr strings.Builder
	code := app.RunCLI(svc, []string{"save", "--type=decision"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit when content arg omitted, stdout=%q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("expected error on stderr")
	}
	if store.saveMetaHits != 0 {
		t.Fatalf("store must not be touched when content is missing, hits=%d", store.saveMetaHits)
	}
}

// TestRunCLI_SaveContentSurvivesEmbeddedFlagLikeText proves the parser
// stops consuming flags as soon as a non-flag argument appears, so users
// can save content that itself contains `--`-prefixed words.
func TestRunCLI_SaveContentSurvivesEmbeddedFlagLikeText(t *testing.T) {
	store := newMetaMemStore()
	svc := app.NewService(store, nil)
	var stdout, stderr strings.Builder
	code := app.RunCLI(svc, []string{
		"save", "--type=decision", "use --foo flag for X",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	if store.lastReq.Content != "use --foo flag for X" {
		t.Fatalf("content drift: %q", store.lastReq.Content)
	}
}

// TestRunCLI_SaveDefaultsAppliedWhenOnlyTopicKeyGiven proves the service
// defaults flow through CLI when only `--topic-key` is provided: type
// becomes `manual` and scope becomes `project` automatically.
func TestRunCLI_SaveDefaultsAppliedWhenOnlyTopicKeyGiven(t *testing.T) {
	store := newMetaMemStore()
	svc := app.NewService(store, nil)
	var stdout, stderr strings.Builder
	code := app.RunCLI(svc, []string{"save", "--topic-key=arch/db", "body"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit: %d stderr=%q", code, stderr.String())
	}
	if store.lastReq.Type != "manual" {
		t.Fatalf("default type: want %q got %q", "manual", store.lastReq.Type)
	}
	if store.lastReq.Scope != "project" {
		t.Fatalf("default scope: want %q got %q", "project", store.lastReq.Scope)
	}
	if store.lastReq.TopicKey != "arch/db" {
		t.Fatalf("topic_key: %q", store.lastReq.TopicKey)
	}
}

// Compile-time guard: metaMemStore satisfies both Store and MetadataStore.
var _ app.Store = (*metaMemStore)(nil)
var _ app.MetadataStore = (*metaMemStore)(nil)

func init() {
	// Ensure clock-dependent fields stay deterministic in case the runner
	// tests get reordered. Touching time here is a no-op but keeps the
	// import alive when any of the above tests is removed.
	_ = time.Now
}
