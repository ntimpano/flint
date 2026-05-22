package app

import (
	"strings"
	"testing"
)

// importFakeStore extends fakeStore with ImportStore so service-level
// import tests can run without booting SQLite.
type importFakeStore struct {
	fakeStore

	calls   int
	lastIn  []ImportRecord
	result  ImportResult
	failErr error
}

func (f *importFakeStore) ImportRecords(rows []ImportRecord) (ImportResult, error) {
	f.calls++
	f.lastIn = rows
	if f.failErr != nil {
		return ImportResult{}, f.failErr
	}
	return f.result, nil
}

// TestService_ImportJSON_ParsesAndForwards proves the JSON path: a JSON
// array of records reaches the store as []ImportRecord, with the result
// surfaced verbatim.
func TestService_ImportJSON_ParsesAndForwards(t *testing.T) {
	fake := &importFakeStore{result: ImportResult{Inserted: 2}}
	svc := NewService(fake, nil)

	payload := `[
		{"content": "alpha", "title": "A", "type": "decision", "topic_key": "arch/a", "scope": "project"},
		{"content": "beta", "topic_key": "arch/b"}
	]`
	res, err := svc.ImportJSON([]byte(payload), false)
	if err != nil {
		t.Fatalf("ImportJSON: %v", err)
	}
	if res.Inserted != 2 {
		t.Fatalf("expected Inserted=2 from store, got %+v", res)
	}
	if fake.calls != 1 || len(fake.lastIn) != 2 {
		t.Fatalf("expected 1 store call with 2 rows, got calls=%d rows=%d", fake.calls, len(fake.lastIn))
	}
	if fake.lastIn[0].Title != "A" || fake.lastIn[0].TopicKey != "arch/a" {
		t.Fatalf("first record metadata not forwarded: %+v", fake.lastIn[0])
	}
}

// TestService_ImportJSON_DryRunSkipsStore proves spec scenario "Dry-run
// reports without writing": dry-run MUST produce planned counts WITHOUT
// invoking ImportRecords on the store.
func TestService_ImportJSON_DryRunSkipsStore(t *testing.T) {
	fake := &importFakeStore{}
	svc := NewService(fake, nil)

	payload := `[{"content": "x"}, {"content": "y"}]`
	res, err := svc.ImportJSON([]byte(payload), true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("dry-run must not touch store, got %d calls", fake.calls)
	}
	// In dry-run, Inserted is the number of NEW records (we don't dedupe
	// against the store, so all valid records count as planned inserts).
	if res.Inserted != 2 {
		t.Fatalf("dry-run plan expected 2 inserts, got %+v", res)
	}
}

// TestService_ImportJSON_RejectsInvalidJSON: clearly wrong input must
// error out without touching the store.
func TestService_ImportJSON_RejectsInvalidJSON(t *testing.T) {
	fake := &importFakeStore{}
	svc := NewService(fake, nil)

	if _, err := svc.ImportJSON([]byte("not json"), false); err == nil {
		t.Fatalf("expected JSON parse error")
	}
	if fake.calls != 0 {
		t.Fatalf("invalid JSON must not touch store")
	}
}

// TestService_ImportJSON_RequiresImportStore covers the defensive
// type-assert path: a Store that doesn't implement ImportStore returns
// a clear error rather than degrading silently.
func TestService_ImportJSON_RequiresImportStore(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	if _, err := svc.ImportJSON([]byte(`[{"content":"x"}]`), false); err == nil {
		t.Fatalf("expected capability error")
	} else if !strings.Contains(strings.ToLower(err.Error()), "import") {
		t.Fatalf("expected import-capability error, got %q", err)
	}
}

// TestService_ImportJSON_SkipsEmptyContent proves validation: rows with
// empty/whitespace content are silently dropped (not forwarded), so
// imports of partially malformed files still succeed for valid rows.
func TestService_ImportJSON_SkipsEmptyContent(t *testing.T) {
	fake := &importFakeStore{result: ImportResult{Inserted: 1}}
	svc := NewService(fake, nil)

	payload := `[{"content": ""}, {"content": "valid"}, {"content": "   "}]`
	if _, err := svc.ImportJSON([]byte(payload), false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.lastIn) != 1 || fake.lastIn[0].Content != "valid" {
		t.Fatalf("expected only the valid row forwarded, got %+v", fake.lastIn)
	}
}
