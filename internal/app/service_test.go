package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore is a minimal in-memory Store implementation for service tests.
// It records calls so we can assert that validation prevents store access.
type fakeStore struct {
	getCalls    int
	updateCalls int
	saveCalls   int
	listCalls   int

	getResult    MemoryItem
	getErr       error
	updateResult bool
	updateErr    error
	listResult   []MemoryItem
	listErr      error

	lastUpdateID      int64
	lastUpdateContent string
	lastUpdateAt      time.Time
}

func (f *fakeStore) Init() error                                  { return nil }
func (f *fakeStore) Save(c string, t time.Time) (int64, error)    { f.saveCalls++; return 1, nil }
func (f *fakeStore) Recall(q string, l int) ([]MemoryItem, error) { return nil, nil }
func (f *fakeStore) List(l int) ([]MemoryItem, error) {
	f.listCalls++
	return f.listResult, f.listErr
}
func (f *fakeStore) Delete(id int64) (bool, error) { return true, nil }
func (f *fakeStore) Close() error                  { return nil }

func (f *fakeStore) Get(id int64) (MemoryItem, error) {
	f.getCalls++
	return f.getResult, f.getErr
}

func (f *fakeStore) Update(id int64, content string, updatedAt time.Time) (bool, error) {
	f.updateCalls++
	f.lastUpdateID = id
	f.lastUpdateContent = content
	f.lastUpdateAt = updatedAt
	return f.updateResult, f.updateErr
}

type fakeBehavioralStore struct{}

func (f *fakeBehavioralStore) RecordObservation(category, field, value string, confidence int, now time.Time) (int64, error) {
	return 123, nil
}
func (f *fakeBehavioralStore) ListObservations(includeStatuses []string) ([]BehavioralObservation, error) {
	return nil, nil
}
func (f *fakeBehavioralStore) GetObservation(id int64) (*BehavioralObservation, error) {
	return nil, nil
}
func (f *fakeBehavioralStore) DismissObservation(id int64) error { return nil }
func (f *fakeBehavioralStore) Candidates() ([]BehavioralObservation, error) {
	return nil, nil
}

func TestServiceBehavioralStore_ReturnsInjectedRepo(t *testing.T) {
	fake := &fakeStore{}
	b := &fakeBehavioralStore{}
	svc := NewService(fake, b)
	if svc.BehavioralStore() != b {
		t.Fatalf("expected injected behavioral store to be returned")
	}
}

func TestServiceGet_ValidationRejectsBadIDs(t *testing.T) {
	cases := []struct {
		name string
		id   int64
	}{
		{"zero id", 0},
		{"negative id", -1},
		{"large negative", -999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeStore{}
			svc := NewService(fake, nil)

			_, err := svc.Get(tc.id)
			if err == nil {
				t.Fatalf("expected validation error for id=%d, got nil", tc.id)
			}
			if fake.getCalls != 0 {
				t.Fatalf("expected store.Get NOT to be called on invalid id, got %d calls", fake.getCalls)
			}
		})
	}
}

func TestServiceGet_ValidIDDelegatesToStore(t *testing.T) {
	fake := &fakeStore{
		getResult: MemoryItem{
			ID:        7,
			Content:   "hello",
			CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
			UpdatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}
	svc := NewService(fake, nil)

	got, err := svc.Get(7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getCalls != 1 {
		t.Fatalf("expected exactly 1 store.Get call, got %d", fake.getCalls)
	}
	if got.ID != 7 || got.Content != "hello" {
		t.Fatalf("unexpected item returned: %+v", got)
	}
	if !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("expected UpdatedAt==CreatedAt for fresh record, got created=%s updated=%s", got.CreatedAt, got.UpdatedAt)
	}
}

func TestServiceGet_PropagatesNotFound(t *testing.T) {
	notFound := errors.New("not found")
	fake := &fakeStore{getErr: notFound}
	svc := NewService(fake, nil)

	_, err := svc.Get(42)
	if err == nil {
		t.Fatalf("expected error from store, got nil")
	}
	if !errors.Is(err, notFound) && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected wrapped not-found error, got %v", err)
	}
}

func TestServiceUpdate_ValidationRejectsBadIDs(t *testing.T) {
	cases := []struct {
		name string
		id   int64
	}{
		{"zero id", 0},
		{"negative id", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeStore{}
			svc := NewService(fake, nil)

			_, err := svc.Update(tc.id, "valid content")
			if err == nil {
				t.Fatalf("expected validation error for id=%d, got nil", tc.id)
			}
			if fake.updateCalls != 0 {
				t.Fatalf("expected store.Update NOT to be called on invalid id, got %d calls", fake.updateCalls)
			}
		})
	}
}

func TestServiceUpdate_ValidationRejectsEmptyContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty string", ""},
		{"only spaces", "   "},
		{"only tabs and newlines", "\t\n  \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeStore{}
			svc := NewService(fake, nil)

			_, err := svc.Update(1, tc.content)
			if err == nil {
				t.Fatalf("expected validation error for content=%q, got nil", tc.content)
			}
			if fake.updateCalls != 0 {
				t.Fatalf("expected store.Update NOT to be called on empty content, got %d calls", fake.updateCalls)
			}
		})
	}
}

func TestServiceUpdate_ValidInputUpdatesWithUTCTimestamp(t *testing.T) {
	fake := &fakeStore{updateResult: true}
	svc := NewService(fake, nil)

	before := time.Now().UTC()
	ok, err := svc.Update(5, "new content")
	after := time.Now().UTC()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true when store reports update success")
	}
	if fake.updateCalls != 1 {
		t.Fatalf("expected exactly 1 store.Update call, got %d", fake.updateCalls)
	}
	if fake.lastUpdateID != 5 {
		t.Fatalf("expected id=5 forwarded to store, got %d", fake.lastUpdateID)
	}
	if fake.lastUpdateContent != "new content" {
		t.Fatalf("expected content forwarded verbatim, got %q", fake.lastUpdateContent)
	}
	if fake.lastUpdateAt.Location() != time.UTC {
		t.Fatalf("expected updatedAt to be UTC, got %s", fake.lastUpdateAt.Location())
	}
	if fake.lastUpdateAt.Before(before) || fake.lastUpdateAt.After(after) {
		t.Fatalf("expected updatedAt within [%s, %s], got %s", before, after, fake.lastUpdateAt)
	}
}

func TestServiceUpdate_TrimsContentBeforeWrite(t *testing.T) {
	fake := &fakeStore{updateResult: true}
	svc := NewService(fake, nil)

	if _, err := svc.Update(5, "  padded  "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastUpdateContent != "padded" {
		t.Fatalf("expected content to be trimmed to %q, got %q", "padded", fake.lastUpdateContent)
	}
}

func TestServiceUpdate_NotFoundReturnsFalse(t *testing.T) {
	fake := &fakeStore{updateResult: false}
	svc := NewService(fake, nil)

	ok, err := svc.Update(99, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when store reports no rows affected")
	}
}

func TestServiceList_GlobalFallbackWhenNoActiveProject(t *testing.T) {
	fake := &fakeStore{listResult: []MemoryItem{{ID: 1, Content: "global"}}}
	svc := NewService(fake, nil)

	items, err := svc.List(5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].Content != "global" {
		t.Fatalf("unexpected list result: %+v", items)
	}
	if fake.listCalls != 1 {
		t.Fatalf("expected fallback to Store.List exactly once, got %d", fake.listCalls)
	}
}
