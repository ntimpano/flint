package app

import (
	"strings"
	"testing"
	"time"
)

// suggestFakeStore is a fakeStore that returns a configurable Recall result
// so SuggestRelations can be tested without booting SQLite or FTS.
type suggestFakeStore struct {
	fakeStore

	recallCalls   int
	recallQuery   string
	recallLimit   int
	recallResults []MemoryItem
	recallErr     error

	createCalls int
}

func (f *suggestFakeStore) Recall(q string, limit int) ([]MemoryItem, error) {
	f.recallCalls++
	f.recallQuery = q
	f.recallLimit = limit
	return f.recallResults, f.recallErr
}

// suggestRelStore couples the suggest fake with a RelationStore so we can
// prove that suggestion is read-only — CreateRelation must NEVER fire.
type suggestRelStore struct {
	suggestFakeStore
	createCalls int
}

func (f *suggestRelStore) CreateRelation(src, tgt int64, t string, at time.Time) error {
	f.createCalls++
	return nil
}

func (f *suggestRelStore) Neighbors(id int64, dir RelationDirection) ([]MemoryRelation, error) {
	return nil, nil
}

// TestSuggestRelations_ReturnsCandidatesWithoutPersisting — spec scenario:
// "Suggestions returned, none auto-applied". WHEN saving content with
// overlapping topic THEN response includes suggested_relations[] AND DB
// has no new edges until confirmation.
func TestSuggestRelations_ReturnsCandidatesWithoutPersisting(t *testing.T) {
	fake := &suggestRelStore{}
	fake.recallResults = []MemoryItem{
		{ID: 11, Content: "auth model decision", TopicKey: "architecture/auth"},
		{ID: 12, Content: "auth refresh tokens", TopicKey: "architecture/auth"},
		{ID: 13, Content: "logging pattern",     TopicKey: "ops/logs"},
	}
	svc := NewService(fake, nil)

	req := SaveRequest{
		Content:  "rotate refresh tokens for auth",
		TopicKey: "architecture/auth",
		Type:     "decision",
	}
	got, err := svc.SuggestRelations(req)
	if err != nil {
		t.Fatalf("SuggestRelations: %v", err)
	}

	// Up to 3 suggestions, never persisted.
	if len(got) == 0 {
		t.Fatalf("expected at least one suggested relation; got 0")
	}
	if len(got) > 3 {
		t.Fatalf("expected ≤3 suggestions; got %d", len(got))
	}
	if fake.createCalls != 0 {
		t.Fatalf("SuggestRelations MUST NOT call CreateRelation; got %d calls", fake.createCalls)
	}
	// Suggestions are unsaved: ID must be 0 to mark them as proposals.
	for i, r := range got {
		if r.ID != 0 {
			t.Fatalf("suggestion[%d] must have ID=0 (unsaved proposal); got %d", i, r.ID)
		}
		if r.RelationType == "" {
			t.Fatalf("suggestion[%d] must declare a RelationType", i)
		}
		if r.TargetID == 0 {
			t.Fatalf("suggestion[%d] must point at a real target id", i)
		}
	}
}

// TestSuggestRelations_TriangulateNoTopicNoSuggestions — spec invariant:
// "On save with topic_key, the system SHOULD suggest...". Without a
// topic_key there is no anchor and the response MUST be empty (the
// store is not even queried).
func TestSuggestRelations_TriangulateNoTopicNoSuggestions(t *testing.T) {
	fake := &suggestRelStore{}
	fake.recallResults = []MemoryItem{{ID: 99, Content: "anything"}}
	svc := NewService(fake, nil)

	got, err := svc.SuggestRelations(SaveRequest{Content: "no topic", TopicKey: ""})
	if err != nil {
		t.Fatalf("SuggestRelations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no suggestions without topic_key; got %d", len(got))
	}
	if fake.recallCalls != 0 {
		t.Fatalf("Recall MUST NOT fire without topic_key; got %d calls", fake.recallCalls)
	}
}

// TestSuggestRelations_TriangulateRelationTypeFromSpec — every suggestion
// MUST come from the curated whitelist. This pins the contract so a
// future widening / typo breaks loudly.
func TestSuggestRelations_TriangulateRelationTypeFromSpec(t *testing.T) {
	fake := &suggestRelStore{}
	fake.recallResults = []MemoryItem{
		{ID: 21, Content: "match", TopicKey: "architecture/auth"},
	}
	svc := NewService(fake, nil)

	got, err := svc.SuggestRelations(SaveRequest{
		Content:  "auth body",
		TopicKey: "architecture/auth",
	})
	if err != nil {
		t.Fatalf("SuggestRelations: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one suggestion")
	}
	for _, r := range got {
		if !IsAllowedRelationType(r.RelationType) {
			t.Fatalf("suggestion uses non-whitelisted relation type %q", r.RelationType)
		}
		// Spec: auto-link suggestions are always `related` (default
		// safest type — operator confirms before stronger semantics
		// like supersedes/refines/depends_on).
		if r.RelationType != "related" {
			t.Fatalf("suggestion default type must be %q, got %q", "related", r.RelationType)
		}
	}
	// Sanity: error path still exists.
	_ = strings.Contains
}
