package app

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// graphRecallFakeStore satisfies Store + FilterStore + GraphRecallStore.
// It records which path the service ultimately invoked so the routing
// tests can assert on call counters without booting SQLite.
//
// The plain `Recall` and `RecallFiltered` paths return distinguishable
// fixtures so tests can also verify the *result* came from the path
// they expected — not just the counter.
type graphRecallFakeStore struct {
	fakeStore

	// FilterStore plumbing
	recallFilteredCalls  int
	lastFilterOpts       RecallOptions
	recallFilteredResult []MemoryItem

	// GraphRecallStore plumbing
	graphAwareCalls   int
	lastGraphOpts     RecallOptions
	graphAwareResult  []MemoryItem
	graphAwareErr     error

	// plain Recall counter (override fakeStore.Recall)
	plainRecallCalls  int
	lastPlainQuery    string
	lastPlainLimit    int
	plainRecallResult []MemoryItem
}

func (g *graphRecallFakeStore) Recall(q string, l int) ([]MemoryItem, error) {
	g.plainRecallCalls++
	g.lastPlainQuery = q
	g.lastPlainLimit = l
	return g.plainRecallResult, nil
}

func (g *graphRecallFakeStore) RecallFiltered(opts RecallOptions) ([]MemoryItem, error) {
	g.recallFilteredCalls++
	g.lastFilterOpts = opts
	return g.recallFilteredResult, nil
}

// Context satisfies FilterStore so the type assertion succeeds. Tests
// don't exercise this path — return nil, nil.
func (g *graphRecallFakeStore) Context(n int, scope string) ([]MemoryItem, error) {
	return nil, nil
}

// ContextFiltered satisfies FilterStore (PR2b) — stub.
func (g *graphRecallFakeStore) ContextFiltered(opts ContextOptions) ([]MemoryItem, error) {
	return nil, nil
}

// ListFiltered satisfies FilterStore (PR2b) — stub.
func (g *graphRecallFakeStore) ListFiltered(opts ListOptions) ([]MemoryItem, error) {
	return nil, nil
}

func (g *graphRecallFakeStore) RecallGraphAware(opts RecallOptions) ([]MemoryItem, error) {
	g.graphAwareCalls++
	g.lastGraphOpts = opts
	return g.graphAwareResult, g.graphAwareErr
}

// withFFGraph toggles NTCLI_FF_GRAPH for one test and restores the
// previous value on cleanup. Mirrors the MCP-side helper but kept
// internal to the app package so the test files don't depend on each
// other.
func withFFGraph(t *testing.T, value string) {
	t.Helper()
	prev, had := os.LookupEnv("NTCLI_FF_GRAPH")
	if value == "" {
		_ = os.Unsetenv("NTCLI_FF_GRAPH")
	} else {
		_ = os.Setenv("NTCLI_FF_GRAPH", value)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NTCLI_FF_GRAPH", prev)
		} else {
			_ = os.Unsetenv("NTCLI_FF_GRAPH")
		}
	})
}

// TestRecall_FFOff_UsesPlainPath proves that with NTCLI_FF_GRAPH unset,
// Service.Recall calls the legacy Recall path verbatim — graph-aware
// re-ranking is invisible to clients that have not opted in.
func TestRecall_FFOff_UsesPlainPath(t *testing.T) {
	withFFGraph(t, "")
	store := &graphRecallFakeStore{
		plainRecallResult: []MemoryItem{{ID: 1, Content: "plain"}},
		graphAwareResult:  []MemoryItem{{ID: 99, Content: "graph"}},
	}
	svc := NewService(store, nil)

	got, err := svc.Recall("alpha", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.graphAwareCalls != 0 {
		t.Fatalf("FF off: graph path must not be invoked, got %d calls", store.graphAwareCalls)
	}
	if store.plainRecallCalls != 1 {
		t.Fatalf("FF off: expected 1 plain Recall call, got %d", store.plainRecallCalls)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("FF off: expected plain result, got %+v", got)
	}
}

// TestRecall_FFOn_RoutesToGraphAware proves that with NTCLI_FF_GRAPH=1,
// Service.Recall routes to RecallGraphAware (when the store implements
// GraphRecallStore) and forwards a sane RecallOptions value.
func TestRecall_FFOn_RoutesToGraphAware(t *testing.T) {
	withFFGraph(t, "1")
	store := &graphRecallFakeStore{
		plainRecallResult: []MemoryItem{{ID: 1, Content: "plain"}},
		graphAwareResult:  []MemoryItem{{ID: 99, Content: "graph"}},
	}
	svc := NewService(store, nil)

	got, err := svc.Recall("  alpha  ", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.plainRecallCalls != 0 {
		t.Fatalf("FF on with graph capability: plain path must not run, got %d calls", store.plainRecallCalls)
	}
	if store.graphAwareCalls != 1 {
		t.Fatalf("FF on: expected 1 graph-aware call, got %d", store.graphAwareCalls)
	}
	if store.lastGraphOpts.Query != "alpha" {
		t.Fatalf("FF on: expected trimmed query forwarded, got %q", store.lastGraphOpts.Query)
	}
	if store.lastGraphOpts.Limit != 7 {
		t.Fatalf("FF on: expected limit forwarded, got %d", store.lastGraphOpts.Limit)
	}
	if len(got) != 1 || got[0].ID != 99 {
		t.Fatalf("FF on: expected graph-aware result, got %+v", got)
	}
}

// TestRecall_FFOn_LegacyStore_FallsBackToPlain proves the routing
// degrades gracefully: a store that DOES NOT implement GraphRecallStore
// (e.g. a tiny test fake) must still receive the plain Recall call,
// not crash. Capability missing != error.
func TestRecall_FFOn_LegacyStore_FallsBackToPlain(t *testing.T) {
	withFFGraph(t, "1")
	// fakeStore (the legacy fake from service_test.go) implements only
	// Store — no GraphRecallStore — so the FF on branch must fall back.
	fake := &fakeStore{}
	svc := NewService(fake, nil)

	_, err := svc.Recall("alpha", 5)
	if err != nil {
		t.Fatalf("legacy store with FF on must not error, got %v", err)
	}
}

// TestRecallWithOptions_FFOff_UsesFilterPath proves the filtered surface
// (RecallWithOptions) keeps its existing behaviour with the flag off.
// Symmetry with TestRecall_FFOff_UsesPlainPath above.
func TestRecallWithOptions_FFOff_UsesFilterPath(t *testing.T) {
	withFFGraph(t, "")
	store := &graphRecallFakeStore{
		recallFilteredResult: []MemoryItem{{ID: 2, Content: "filter"}},
		graphAwareResult:     []MemoryItem{{ID: 99, Content: "graph"}},
	}
	svc := NewService(store, nil)

	got, err := svc.RecallWithOptions(RecallOptions{Query: "alpha", Type: "decision"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.graphAwareCalls != 0 {
		t.Fatalf("FF off: graph path must not run, got %d", store.graphAwareCalls)
	}
	if store.recallFilteredCalls != 1 {
		t.Fatalf("FF off: expected 1 RecallFiltered call, got %d", store.recallFilteredCalls)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("FF off: expected filtered result, got %+v", got)
	}
}

// TestRecallWithOptions_FFOn_RoutesToGraphAware proves the filtered
// surface ALSO routes to graph-aware when FF is ON. The GraphAware
// option flag MUST be set on the forwarded RecallOptions so the store
// (or any future delegate) can introspect why it was called.
func TestRecallWithOptions_FFOn_RoutesToGraphAware(t *testing.T) {
	withFFGraph(t, "1")
	store := &graphRecallFakeStore{
		recallFilteredResult: []MemoryItem{{ID: 2, Content: "filter"}},
		graphAwareResult:     []MemoryItem{{ID: 99, Content: "graph"}},
	}
	svc := NewService(store, nil)

	got, err := svc.RecallWithOptions(RecallOptions{
		Query:             "alpha",
		Limit:             3,
		IncludeSuperseded: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.recallFilteredCalls != 0 {
		t.Fatalf("FF on: filtered path must not run, got %d", store.recallFilteredCalls)
	}
	if store.graphAwareCalls != 1 {
		t.Fatalf("FF on: expected 1 graph-aware call, got %d", store.graphAwareCalls)
	}
	if !store.lastGraphOpts.IncludeSuperseded {
		t.Fatalf("FF on: IncludeSuperseded must be forwarded, got %+v", store.lastGraphOpts)
	}
	if !store.lastGraphOpts.GraphAware {
		t.Fatalf("FF on: GraphAware option must be set on forwarded opts, got %+v", store.lastGraphOpts)
	}
	if len(got) != 1 || got[0].ID != 99 {
		t.Fatalf("FF on: expected graph-aware result, got %+v", got)
	}
}

// TestRecall_FFOn_GraphErrorPropagates: when the graph path returns an
// error the service surfaces it verbatim (no silent swallow).
func TestRecall_FFOn_GraphErrorPropagates(t *testing.T) {
	withFFGraph(t, "1")
	store := &graphRecallFakeStore{
		graphAwareErr: errors.New("graph boom"),
	}
	svc := NewService(store, nil)

	_, err := svc.Recall("alpha", 5)
	if err == nil {
		t.Fatalf("expected error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "graph boom") {
		t.Fatalf("expected wrapped error to contain %q, got %q", "graph boom", err.Error())
	}
}
