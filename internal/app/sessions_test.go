package app

import (
	"strings"
	"testing"
	"time"
)

// sessionFakeStore extends fakeStore with SessionStore so service-level
// session tests assert delegation without booting SQLite.
type sessionFakeStore struct {
	fakeStore

	startCalls   int
	endCalls     int
	summaryCalls int

	lastID      string
	lastSummary string
	lastAt      time.Time

	events []SessionEvent
	failOn string // kind that should return error
}

func (f *sessionFakeStore) SessionStart(id string, at time.Time) error {
	f.startCalls++
	f.lastID = id
	f.lastAt = at
	if f.failOn == "start" {
		return errStubFail
	}
	return nil
}

func (f *sessionFakeStore) SessionSummary(id, summary string, at time.Time) error {
	f.summaryCalls++
	f.lastID = id
	f.lastSummary = summary
	f.lastAt = at
	if f.failOn == "summary" {
		return errStubFail
	}
	return nil
}

func (f *sessionFakeStore) SessionEnd(id string, at time.Time) error {
	f.endCalls++
	f.lastID = id
	f.lastAt = at
	if f.failOn == "end" {
		return errStubFail
	}
	return nil
}

func (f *sessionFakeStore) SessionEvents(id string) ([]SessionEvent, error) {
	f.lastID = id
	return f.events, nil
}

func (f *sessionFakeStore) ActiveSessionID() (string, error) {
	return strings.TrimSpace(f.lastID), nil
}

var errStubFail = stubErr("stub failure")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestService_SessionStart_TrimAndForward proves the service trims the id
// and forwards a non-zero timestamp (using its own clock) to the store.
func TestService_SessionStart_TrimAndForward(t *testing.T) {
	fake := &sessionFakeStore{}
	svc := NewService(fake, nil)

	if err := svc.SessionStart("  sess-1  "); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	if fake.startCalls != 1 {
		t.Fatalf("expected 1 start call, got %d", fake.startCalls)
	}
	if fake.lastID != "sess-1" {
		t.Fatalf("expected trimmed id %q, got %q", "sess-1", fake.lastID)
	}
	if fake.lastAt.IsZero() {
		t.Fatalf("expected non-zero timestamp")
	}
}

// TestService_SessionEnd_TrimAndForward parallels SessionStart.
func TestService_SessionEnd_TrimAndForward(t *testing.T) {
	fake := &sessionFakeStore{}
	svc := NewService(fake, nil)

	if err := svc.SessionEnd("sess-2"); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	if fake.endCalls != 1 || fake.lastID != "sess-2" {
		t.Fatalf("unexpected delegation: calls=%d lastID=%q", fake.endCalls, fake.lastID)
	}
}

// TestService_SessionSummary_RejectsEmptySummary: the lifecycle log
// shouldn't accept empty summaries — they would make the row useless.
func TestService_SessionSummary_RejectsEmptySummary(t *testing.T) {
	fake := &sessionFakeStore{}
	svc := NewService(fake, nil)

	if err := svc.SessionSummary("sess-3", "   "); err == nil {
		t.Fatalf("expected error on empty summary")
	}
	if fake.summaryCalls != 0 {
		t.Fatalf("expected no store call on validation failure, got %d", fake.summaryCalls)
	}
}

// TestService_SessionSummary_ForwardsContent proves the trimmed summary
// reaches the store verbatim.
func TestService_SessionSummary_ForwardsContent(t *testing.T) {
	fake := &sessionFakeStore{}
	svc := NewService(fake, nil)

	if err := svc.SessionSummary("sess-3", "  closed deal  "); err != nil {
		t.Fatalf("SessionSummary: %v", err)
	}
	if fake.lastSummary != "closed deal" {
		t.Fatalf("expected trimmed summary forwarded, got %q", fake.lastSummary)
	}
}

// TestService_SessionStart_RejectsEmptyID guards against silent merging.
func TestService_SessionStart_RejectsEmptyID(t *testing.T) {
	cases := []string{"", "   "}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			fake := &sessionFakeStore{}
			svc := NewService(fake, nil)
			if err := svc.SessionStart(id); err == nil {
				t.Fatalf("expected error for id=%q", id)
			}
			if fake.startCalls != 0 {
				t.Fatalf("store should not be called on invalid id")
			}
		})
	}
}

// TestService_Session_StoreWithoutSessionCapability proves the defensive
// type-assert: legacy fakes that don't implement SessionStore must get a
// clear error rather than a silent no-op.
func TestService_Session_StoreWithoutSessionCapability(t *testing.T) {
	fake := &fakeStore{}
	svc := NewService(fake, nil)

	if err := svc.SessionStart("x"); err == nil {
		t.Fatalf("expected capability error from SessionStart")
	} else if !strings.Contains(strings.ToLower(err.Error()), "session") {
		t.Fatalf("expected session capability error, got %q", err)
	}
}
