package app_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// runCLIFilter drives RunCLI with a filterMemStore so the service sees a
// store that satisfies both Store and FilterStore.
func runCLIFilter(t *testing.T, store *filterMemStore, args ...string) (int, string, string) {
	t.Helper()
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// filterMemStore extends memStore with FilterStore so the CLI runner can
// drive the new context/recall-filter surfaces under test.
type filterMemStore struct {
	*memStore
}

func newFilterMemStore() *filterMemStore {
	return &filterMemStore{memStore: newMemStore()}
}

func (f *filterMemStore) RecallFiltered(opts app.RecallOptions) ([]app.MemoryItem, error) {
	var out []app.MemoryItem
	for _, it := range f.items {
		if opts.Query != "" && !strings.Contains(it.Content, opts.Query) {
			continue
		}
		if opts.Type != "" && it.Type != opts.Type {
			continue
		}
		if !opts.Since.IsZero() && it.CreatedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && it.CreatedAt.After(opts.Until) {
			continue
		}
		out = append(out, it)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

func (f *filterMemStore) Context(n int, scope string) ([]app.MemoryItem, error) {
	all := make([]app.MemoryItem, 0, len(f.items))
	for _, it := range f.items {
		if scope != "" && it.Scope != scope {
			continue
		}
		all = append(all, it)
	}
	// Newest-first by CreatedAt.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].CreatedAt.After(all[i].CreatedAt) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all, nil
}

// ContextFiltered is the project-scoped extension (PR2b stub — fakes ignore ProjectID).
func (f *filterMemStore) ContextFiltered(opts app.ContextOptions) ([]app.MemoryItem, error) {
	return f.Context(opts.N, opts.Scope)
}

// ListFiltered is the project-scoped extension (PR2b stub — fakes ignore ProjectID).
func (f *filterMemStore) ListFiltered(opts app.ListOptions) ([]app.MemoryItem, error) {
	return f.memStore.List(opts.Limit)
}

// SaveWithMeta lets CLI tests seed metadata fields the runner needs to
// filter on (type, scope). It implements app.MetadataStore.
func (f *filterMemStore) SaveWithMeta(req app.SaveRequest) (int64, error) {
	id := f.nextID
	f.nextID++
	stamp := req.CreatedAt
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}
	f.items[id] = app.MemoryItem{
		ID:        id,
		Content:   req.Content,
		CreatedAt: stamp,
		UpdatedAt: stamp,
		Title:     req.Title,
		Type:      req.Type,
		TopicKey:  req.TopicKey,
		Scope:     req.Scope,
	}
	return id, nil
}

// TestRunCLI_ContextReturnsRecentN proves the CLI exposes the new
// `context` subcommand and prints the most recent N items newest-first.
func TestRunCLI_ContextReturnsRecentN(t *testing.T) {
	store := newFilterMemStore()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		if _, err := store.SaveWithMeta(app.SaveRequest{
			Content:   "row-" + string(rune('a'+i)),
			Scope:     "project",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	code, stdout, stderr := runCLIFilter(t, store, "context", "--n=3")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	// Newest 3 are row-f, row-e, row-d in that order.
	wantOrder := []string{"row-f", "row-e", "row-d"}
	for _, w := range wantOrder {
		if !strings.Contains(stdout, w) {
			t.Fatalf("expected stdout to contain %q, got %q", w, stdout)
		}
	}
	// Ordering: row-f appears before row-d.
	if strings.Index(stdout, "row-f") > strings.Index(stdout, "row-d") {
		t.Fatalf("expected newest-first order, got %q", stdout)
	}
	// Older rows must NOT appear.
	if strings.Contains(stdout, "row-a") || strings.Contains(stdout, "row-b") {
		t.Fatalf("expected only top-3 rows, got %q", stdout)
	}
}

// TestRunCLI_ContextDefaultLimit10 proves the default n is 10 when no flag
// is provided (spec scenario "default N=10").
func TestRunCLI_ContextDefaultLimit10(t *testing.T) {
	store := newFilterMemStore()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		if _, err := store.SaveWithMeta(app.SaveRequest{
			Content:   "ctx-" + string(rune('A'+i)),
			Scope:     "project",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	code, stdout, _ := runCLIFilter(t, store, "context")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	// 12 seeded rows, default 10 expected → 2 oldest (ctx-A, ctx-B) excluded.
	if strings.Contains(stdout, "ctx-A") {
		t.Fatalf("expected oldest row ctx-A to be excluded by default n=10, got %q", stdout)
	}
	if !strings.Contains(stdout, "ctx-L") {
		t.Fatalf("expected newest row ctx-L to be present, got %q", stdout)
	}
}

// TestRunCLI_ContextScopeFilter proves --scope narrows results.
func TestRunCLI_ContextScopeFilter(t *testing.T) {
	store := newFilterMemStore()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []struct {
		scope, content string
	}{
		{"project", "proj-1"},
		{"personal", "pers-1"},
		{"project", "proj-2"},
		{"personal", "pers-2"},
	}
	for i, p := range pairs {
		if _, err := store.SaveWithMeta(app.SaveRequest{
			Content:   p.content,
			Scope:     p.scope,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	code, stdout, stderr := runCLIFilter(t, store, "context", "--scope=personal")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if strings.Contains(stdout, "proj-1") || strings.Contains(stdout, "proj-2") {
		t.Fatalf("expected project rows excluded, got %q", stdout)
	}
	if !strings.Contains(stdout, "pers-1") || !strings.Contains(stdout, "pers-2") {
		t.Fatalf("expected both personal rows present, got %q", stdout)
	}
}

// TestRunCLI_RecallTypeFlagNarrowsResults proves --type forwards to
// RecallFiltered and narrows by metadata.
func TestRunCLI_RecallTypeFlagNarrowsResults(t *testing.T) {
	store := newFilterMemStore()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha decision row", Type: "decision", CreatedAt: base,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.SaveWithMeta(app.SaveRequest{
		Content: "alpha manual row", Type: "manual", CreatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code, stdout, stderr := runCLIFilter(t, store, "recall", "--type=decision", "alpha")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "alpha decision row") {
		t.Fatalf("expected decision row in output, got %q", stdout)
	}
	if strings.Contains(stdout, "alpha manual row") {
		t.Fatalf("expected manual row excluded, got %q", stdout)
	}
}
