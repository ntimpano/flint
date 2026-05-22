package app

import (
	"strings"
	"testing"
	"time"
)

// filterFakeStore is a fakeStore extension that also implements
// FilterStore so service-level filter tests can assert delegation
// without dragging in the SQLite layer.
type filterFakeStore struct {
	fakeStore

	recallFilteredCalls  int
	lastFilterOpts       RecallOptions
	recallFilteredResult []MemoryItem

	contextCalls     int
	lastContextN     int
	lastContextScope string
	contextResult    []MemoryItem

	listFilteredCalls  int
	lastListOpts       ListOptions
	listFilteredResult []MemoryItem
}

func (f *filterFakeStore) RecallFiltered(opts RecallOptions) ([]MemoryItem, error) {
	f.recallFilteredCalls++
	f.lastFilterOpts = opts
	return f.recallFilteredResult, nil
}

func (f *filterFakeStore) Context(n int, scope string) ([]MemoryItem, error) {
	f.contextCalls++
	f.lastContextN = n
	f.lastContextScope = scope
	return f.contextResult, nil
}

// ContextFiltered is the project-scoped extension stub.
func (f *filterFakeStore) ContextFiltered(opts ContextOptions) ([]MemoryItem, error) {
	return f.Context(opts.N, opts.Scope)
}

// ListFiltered is the project-scoped extension stub.
func (f *filterFakeStore) ListFiltered(opts ListOptions) ([]MemoryItem, error) {
	f.listFilteredCalls++
	f.lastListOpts = opts
	return f.listFilteredResult, nil
}

// TestRecallWithOptions_Validation rejects empty query before touching the
// store, mirroring Recall()'s contract.
func TestRecallWithOptions_Validation(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"empty", ""},
		{"only spaces", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &filterFakeStore{}
			svc := NewService(fake, nil)
			_, err := svc.RecallWithOptions(RecallOptions{Query: tc.query})
			if err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if fake.recallFilteredCalls != 0 {
				t.Fatalf("expected no store call on invalid query, got %d", fake.recallFilteredCalls)
			}
		})
	}
}

// TestRecallWithOptions_DefaultsAndForwardsFilters proves the service
// trims the query, defaults limit to 10 when ≤0, and forwards the
// metadata filters verbatim to the FilterStore.
func TestRecallWithOptions_DefaultsAndForwardsFilters(t *testing.T) {
	fake := &filterFakeStore{
		recallFilteredResult: []MemoryItem{{ID: 1, Content: "alpha", Type: "decision"}},
	}
	svc := NewService(fake, nil)

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)
	got, err := svc.RecallWithOptions(RecallOptions{
		Query: "  alpha  ",
		Type:  "decision",
		Since: since,
		Until: until,
		Limit: 0, // service must default to 10
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected forwarded result, got %d items", len(got))
	}
	if fake.recallFilteredCalls != 1 {
		t.Fatalf("expected exactly 1 RecallFiltered call, got %d", fake.recallFilteredCalls)
	}
	if fake.lastFilterOpts.Query != "alpha" {
		t.Fatalf("expected trimmed query %q, got %q", "alpha", fake.lastFilterOpts.Query)
	}
	if fake.lastFilterOpts.Type != "decision" {
		t.Fatalf("expected type forwarded, got %q", fake.lastFilterOpts.Type)
	}
	if !fake.lastFilterOpts.Since.Equal(since) || !fake.lastFilterOpts.Until.Equal(until) {
		t.Fatalf("expected date range forwarded, got [%s, %s]", fake.lastFilterOpts.Since, fake.lastFilterOpts.Until)
	}
	if fake.lastFilterOpts.Limit != 10 {
		t.Fatalf("expected default limit=10 when caller passes 0, got %d", fake.lastFilterOpts.Limit)
	}
}

// TestRecallWithOptions_StoreWithoutFilterCapability proves the service
// fails fast (rather than silently dropping filters) when the underlying
// store doesn't implement FilterStore. Mirrors SaveWithMeta's defensive
// check for MetadataStore.
func TestRecallWithOptions_StoreWithoutFilterCapability(t *testing.T) {
	fake := &fakeStore{} // no RecallFiltered/Context
	svc := NewService(fake, nil)

	_, err := svc.RecallWithOptions(RecallOptions{Query: "alpha", Type: "decision"})
	if err == nil {
		t.Fatalf("expected error when store doesn't support filters, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "filter") &&
		!strings.Contains(strings.ToLower(err.Error()), "metadata") {
		t.Fatalf("expected filter/metadata error message, got %q", err.Error())
	}
}

// TestContext_ValidationAndDefaults: n ≤ 0 defaults to 10; scope is
// trimmed before forwarding.
func TestContext_ValidationAndDefaults(t *testing.T) {
	fake := &filterFakeStore{
		contextResult: []MemoryItem{{ID: 1, Content: "x"}},
	}
	svc := NewService(fake, nil)

	got, err := svc.Context(0, "  project  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected forwarded result, got %d", len(got))
	}
	if fake.contextCalls != 1 {
		t.Fatalf("expected exactly 1 Context call, got %d", fake.contextCalls)
	}
	if fake.lastContextN != 10 {
		t.Fatalf("expected default n=10 when caller passes 0, got %d", fake.lastContextN)
	}
	if fake.lastContextScope != "project" {
		t.Fatalf("expected trimmed scope %q, got %q", "project", fake.lastContextScope)
	}
}

// TestContext_StoreWithoutContextCapability: same defensive contract as
// RecallWithOptions when the underlying store is a legacy fake.
func TestContext_StoreWithoutContextCapability(t *testing.T) {
	fake := &fakeStore{}
	svc := NewService(fake, nil)

	_, err := svc.Context(5, "")
	if err == nil {
		t.Fatalf("expected error when store doesn't support Context, got nil")
	}
}

func TestServiceList_DefaultScopesByActiveProject(t *testing.T) {
	fake := &filterFakeStore{listFilteredResult: []MemoryItem{{ID: 2, Content: "scoped"}}}
	svc := NewService(fake, nil)
	svc.SetActiveProject(7)

	items, err := svc.List(3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].Content != "scoped" {
		t.Fatalf("unexpected items: %+v", items)
	}
	if fake.listFilteredCalls != 1 {
		t.Fatalf("expected ListFiltered call, got %d", fake.listFilteredCalls)
	}
	if fake.lastListOpts.ProjectID != 7 || fake.lastListOpts.AllProjects {
		t.Fatalf("expected scoped opts (project_id=7, all_projects=false), got %+v", fake.lastListOpts)
	}
}

func TestServiceListOpts_AllProjectsBypass(t *testing.T) {
	fake := &filterFakeStore{listFilteredResult: []MemoryItem{{ID: 1}, {ID: 2}}}
	svc := NewService(fake, nil)
	svc.SetActiveProject(9)

	items, err := svc.ListOpts(10, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if fake.listFilteredCalls != 1 {
		t.Fatalf("expected ListFiltered call, got %d", fake.listFilteredCalls)
	}
	if !fake.lastListOpts.AllProjects || fake.lastListOpts.ProjectID != 9 {
		t.Fatalf("expected bypass opts (all_projects=true, project_id=9), got %+v", fake.lastListOpts)
	}
}
