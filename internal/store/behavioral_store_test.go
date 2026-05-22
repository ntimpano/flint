package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newBehaviorTestStore(t *testing.T) *BehaviorSQLiteStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "behavior.db")
	s, err := NewBehaviorSQLiteStore(path)
	if err != nil {
		t.Fatalf("open behavior store: %v", err)
	}
	return s
}

func TestBehavioralStore_RecordInsertAndUpsertPromotion(t *testing.T) {
	s := newBehaviorTestStore(t)
	defer func() { _ = s.Close() }()
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	id, err := s.RecordObservation("tone", "language", "es", 90, now)
	if err != nil {
		t.Fatalf("RecordObservation insert: %v", err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation insert: %v", err)
	}
	if obs.OccurrenceCount != 1 || obs.Status != "observed" {
		t.Fatalf("insert state mismatch: count=%d status=%q", obs.OccurrenceCount, obs.Status)
	}

	if _, err := s.RecordObservation("tone", "language", "es", 90, now.Add(time.Minute)); err != nil {
		t.Fatalf("RecordObservation upsert #2: %v", err)
	}
	if _, err := s.RecordObservation("tone", "language", "es", 90, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordObservation upsert #3: %v", err)
	}
	obs, err = s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation after upsert: %v", err)
	}
	if obs.OccurrenceCount != 3 {
		t.Fatalf("expected occurrence_count=3, got %d", obs.OccurrenceCount)
	}
	if obs.Status != "candidate" {
		t.Fatalf("expected status=candidate after threshold, got %q", obs.Status)
	}
}

func TestBehavioralStore_NoPromotionAtLowConfidence(t *testing.T) {
	s := newBehaviorTestStore(t)
	defer func() { _ = s.Close() }()
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	id, err := s.RecordObservation("format", "response_length", "short", 60, now)
	if err != nil {
		t.Fatalf("RecordObservation insert: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.RecordObservation("format", "response_length", "short", 60, now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatalf("RecordObservation upsert #%d: %v", i+2, err)
		}
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs.OccurrenceCount != 5 {
		t.Fatalf("expected occurrence_count=5, got %d", obs.OccurrenceCount)
	}
	if obs.Status != "observed" {
		t.Fatalf("expected status=observed at confidence<=60, got %q", obs.Status)
	}
}

func TestBehavioralStore_DismissIdempotentAndFilters(t *testing.T) {
	s := newBehaviorTestStore(t)
	defer func() { _ = s.Close() }()
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	observedID, _ := s.RecordObservation("process", "ask_before_mutation", "true", 80, now)
	candidateID, _ := s.RecordObservation("tone", "language", "es", 90, now)
	for i := 0; i < 2; i++ {
		_, _ = s.RecordObservation("tone", "language", "es", 90, now.Add(time.Duration(i+1)*time.Minute))
	}

	if err := s.DismissObservation(observedID); err != nil {
		t.Fatalf("DismissObservation #1: %v", err)
	}
	if err := s.DismissObservation(observedID); err != nil {
		t.Fatalf("DismissObservation #2: %v", err)
	}

	cands, err := s.Candidates()
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != candidateID {
		t.Fatalf("expected only candidate id %d, got %+v", candidateID, cands)
	}

	list, err := s.ListObservations([]string{"observed", "candidate"})
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	for _, obs := range list {
		if obs.Status == "dismissed" {
			t.Fatalf("dismissed observation leaked into list: %+v", obs)
		}
	}
}
