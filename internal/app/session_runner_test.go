package app_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"flint/internal/app"
)

// sessionMemStore extends memStore so RunCLI can dispatch session commands
// against an in-memory backing store. Mirrors metaMemStore / filterMemStore.
type sessionMemStore struct {
	*memStore

	startCalls   int
	endCalls     int
	summaryCalls int

	lastID      string
	lastSummary string
}

func newSessionMemStore() *sessionMemStore {
	return &sessionMemStore{memStore: newMemStore()}
}

func (s *sessionMemStore) SessionStart(id string, at time.Time) error {
	s.startCalls++
	s.lastID = id
	return nil
}

func (s *sessionMemStore) SessionEnd(id string, at time.Time) error {
	s.endCalls++
	s.lastID = id
	return nil
}

func (s *sessionMemStore) SessionSummary(id, summary string, at time.Time) error {
	s.summaryCalls++
	s.lastID = id
	s.lastSummary = summary
	return nil
}

func (s *sessionMemStore) SessionEvents(id string) ([]app.SessionEvent, error) {
	s.lastID = id
	return nil, nil
}

func (s *sessionMemStore) ActiveSessionID() (string, error) {
	if strings.TrimSpace(s.lastID) == "" {
		return "", nil
	}
	return s.lastID, nil
}

// Compile-time guard: sessionMemStore satisfies Store and SessionStore.
var _ app.Store = (*sessionMemStore)(nil)
var _ app.SessionStore = (*sessionMemStore)(nil)

func runCLISession(t *testing.T, store *sessionMemStore, args ...string) (int, string, string) {
	t.Helper()
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestRunCLI_SessionStart_DispatchesAndPrintsConfirmation covers the happy
// path: `nt-cli session start <id>` should reach the store and print
// "session started <id>".
func TestRunCLI_SessionStart_DispatchesAndPrintsConfirmation(t *testing.T) {
	store := newSessionMemStore()
	code, stdout, stderr := runCLISession(t, store, "session", "start", "sess-1")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.startCalls != 1 || store.lastID != "sess-1" {
		t.Fatalf("expected 1 start call for sess-1, got calls=%d id=%q", store.startCalls, store.lastID)
	}
	if !strings.Contains(stdout, "sess-1") {
		t.Fatalf("expected stdout to mention id, got %q", stdout)
	}
}

// TestRunCLI_SessionEnd_Dispatches mirrors the start case for `end`.
func TestRunCLI_SessionEnd_Dispatches(t *testing.T) {
	store := newSessionMemStore()
	code, stdout, stderr := runCLISession(t, store, "session", "end", "sess-2")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.endCalls != 1 || store.lastID != "sess-2" {
		t.Fatalf("expected 1 end call for sess-2, got calls=%d id=%q", store.endCalls, store.lastID)
	}
	if !strings.Contains(stdout, "sess-2") {
		t.Fatalf("stdout missing id: %q", stdout)
	}
}

// TestRunCLI_SessionSummary_JoinsTrailingArgs proves the summary text is
// joined from positional args (mirrors `update`/`save` behaviour).
func TestRunCLI_SessionSummary_JoinsTrailingArgs(t *testing.T) {
	store := newSessionMemStore()
	code, _, stderr := runCLISession(t, store, "session", "summary", "sess-3", "closed", "the", "deal")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.summaryCalls != 1 {
		t.Fatalf("expected 1 summary call, got %d", store.summaryCalls)
	}
	if store.lastSummary != "closed the deal" {
		t.Fatalf("expected joined summary, got %q", store.lastSummary)
	}
}

// TestRunCLI_SessionUsageErrors covers missing/invalid args; each must
// print to stderr and exit non-zero without touching the store.
func TestRunCLI_SessionUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no subcommand", []string{"session"}},
		{"unknown subcommand", []string{"session", "bogus", "id"}},
		{"start without id", []string{"session", "start"}},
		{"end without id", []string{"session", "end"}},
		{"summary without id", []string{"session", "summary"}},
		{"summary without text", []string{"session", "summary", "sess-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newSessionMemStore()
			code, _, stderr := runCLISession(t, store, tc.args...)
			if code == 0 {
				t.Fatalf("expected non-zero exit, got 0")
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("expected error message on stderr")
			}
			if store.startCalls+store.endCalls+store.summaryCalls != 0 {
				t.Fatalf("store should not be touched on usage errors")
			}
		})
	}
}

// TestRunCLI_UsageMentionsSession ensures the top-level usage banner
// advertises the new command — keeps discoverability honest.
func TestRunCLI_UsageMentionsSession(t *testing.T) {
	store := newSessionMemStore()
	_, stdout, _ := runCLISession(t, store, "bogus-command-xyz")
	if !strings.Contains(stdout, "session") {
		t.Fatalf("usage banner should mention 'session', got:\n%s", stdout)
	}
}
