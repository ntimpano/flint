package app

import (
	"strings"
	"testing"
	"time"
)

// autopilotSessionStore extends sessionFakeStore with replayable events
// so SessionEndStrict can decide whether a summary row exists.
type autopilotSessionStore struct {
	sessionFakeStore
}

// TestSessionEndStrict_BlocksWhenNoSummary — spec scenario:
// "Missing summary blocks clean end". GIVEN a session with start but no
// summary, WHEN session end runs without --force, THEN exit is non-zero
// with `summary_required` and no end-row is written.
func TestSessionEndStrict_BlocksWhenNoSummary(t *testing.T) {
	fake := &autopilotSessionStore{}
	fake.events = []SessionEvent{
		{SessionID: "s-1", Kind: "start", CreatedAt: time.Now().UTC()},
	}
	svc := NewService(fake, nil)

	err := svc.SessionEndStrict("s-1")
	if err == nil {
		t.Fatalf("expected summary_required error; got nil")
	}
	if !strings.Contains(err.Error(), "summary_required") {
		t.Fatalf("expected error containing %q; got %q", "summary_required", err.Error())
	}
	if fake.endCalls != 0 {
		t.Fatalf("end-row MUST NOT be written when summary is missing; got %d end calls", fake.endCalls)
	}
}

// TestSessionEndStrict_PassesWhenSummaryPresent — triangulation with the
// happy path. A summary row makes the strict end legal.
func TestSessionEndStrict_PassesWhenSummaryPresent(t *testing.T) {
	fake := &autopilotSessionStore{}
	fake.events = []SessionEvent{
		{SessionID: "s-2", Kind: "start", CreatedAt: time.Now().UTC()},
		{SessionID: "s-2", Kind: "summary", Summary: "done", CreatedAt: time.Now().UTC()},
	}
	svc := NewService(fake, nil)

	if err := svc.SessionEndStrict("s-2"); err != nil {
		t.Fatalf("SessionEndStrict: %v", err)
	}
	if fake.endCalls != 1 {
		t.Fatalf("end-row MUST be written when summary is present; got %d", fake.endCalls)
	}
}

// TestSessionEndStrict_RejectsEmptyID guards against silent merging on
// an empty session id (matches the SessionStart/End validation rules).
func TestSessionEndStrict_RejectsEmptyID(t *testing.T) {
	svc := NewService(&autopilotSessionStore{}, nil)
	if err := svc.SessionEndStrict("   "); err == nil {
		t.Fatalf("expected error for empty session id")
	}
}
