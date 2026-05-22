package app_test

import (
	"bytes"
	"strings"
	"testing"

	"flint/internal/app"
)

// autoswitchProjectStore is a test double that extends projectMemStore
// with a controllable Probe result so we can simulate all autoswitch scenarios
// without touching the real filesystem or git.
type autoswitchProjectStore struct {
	*projectMemStore
	probeResult app.ProbeResult
}

func newAutoswitchStore(probeResult app.ProbeResult) *autoswitchProjectStore {
	return &autoswitchProjectStore{
		projectMemStore: newProjectMemStore(),
		probeResult:     probeResult,
	}
}

// autoswitchEngine wraps a projectEngineImpl-like stub where Probe returns
// the injected result.
type autoswitchEngine struct {
	store       *autoswitchProjectStore
	probeResult app.ProbeResult
}

func (e *autoswitchEngine) Probe(_ string) (app.ProbeResult, error) { return e.probeResult, nil }
func (e *autoswitchEngine) List() ([]app.Project, error)            { return e.store.ListProjects() }
func (e *autoswitchEngine) Current() (app.Project, error)           { return e.store.GetActive() }
func (e *autoswitchEngine) Switch(id int64) error                   { return e.store.SetActive(id) }
func (e *autoswitchEngine) Confirm(candidate string) error {
	// Create new project and switch to it.
	p, err := e.store.CreateProject(app.ProjectInput{Name: candidate})
	if err != nil {
		return err
	}
	return e.store.SetActive(p.ID)
}

// buildAutoswitchSvc builds a Service with the injected engine.
func buildAutoswitchSvc(store *autoswitchProjectStore, eng app.ProjectEngine) *app.Service {
	svc := app.NewService(store, nil)
	svc.ProjectEng = eng
	return svc
}

// ---------------------------------------------------------------------------
// Scenario 1: known + high-confidence → silent autoswitch
// ---------------------------------------------------------------------------

func TestAutoswitch_KnownHighConfidence_SilentSwitch(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "known",
		Candidate:  "nt-cli", // project #2 in store
		Confidence: "high",
		Reason:     "fingerprint matched",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	// Active is #1 "default"; probe says "nt-cli" (#2).
	var stdout bytes.Buffer
	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return false }, // should not matter for silent switch
		Stdin:         strings.NewReader(""),
		Stderr:        &stdout,
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if !switched {
		t.Fatal("expected switch to happen for known/high-confidence different project")
	}
	if svc.ActiveProjectID() != 2 {
		t.Errorf("expected active project 2, got %d", svc.ActiveProjectID())
	}
	// No prompt should have been printed.
	if strings.Contains(stdout.String(), "autoswitch:") {
		t.Errorf("did not expect a prompt for silent switch, got: %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: same active project → no switch
// ---------------------------------------------------------------------------

func TestAutoswitch_KnownHighConfidence_SameProject_NoSwitch(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "known",
		Candidate:  "default", // already active
		Confidence: "high",
		Reason:     "fingerprint matched",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return false },
		Stdin:         strings.NewReader(""),
		Stderr:        &bytes.Buffer{},
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if switched {
		t.Error("expected no switch when probe matches active project")
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: new project + interactive → prompt, user accepts
// ---------------------------------------------------------------------------

func TestAutoswitch_New_Interactive_UserAccepts(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "new",
		Candidate:  "brand-new",
		Confidence: "high",
		Reason:     "no matching fingerprint",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	var stdout bytes.Buffer
	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return true },
		Stdin:         strings.NewReader("y\n"),
		Stderr:        &stdout,
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if !switched {
		t.Fatal("expected switch when user accepts in interactive mode")
	}
	if !strings.Contains(stdout.String(), "autoswitch:") {
		t.Errorf("expected prompt in interactive mode, got: %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: new project + interactive → prompt, user declines
// ---------------------------------------------------------------------------

func TestAutoswitch_New_Interactive_UserDeclines(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "new",
		Candidate:  "brand-new",
		Confidence: "high",
		Reason:     "no matching fingerprint",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	originalID := svc.ActiveProjectID()
	var stdout bytes.Buffer
	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return true },
		Stdin:         strings.NewReader("n\n"),
		Stderr:        &stdout,
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if switched {
		t.Error("expected no switch when user declines")
	}
	if svc.ActiveProjectID() != originalID {
		t.Errorf("expected active project to remain %d, got %d", originalID, svc.ActiveProjectID())
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: new project + non-interactive → no prompt, no switch
// ---------------------------------------------------------------------------

func TestAutoswitch_New_NonInteractive_NoSwitchNoPrompt(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "new",
		Candidate:  "brand-new",
		Confidence: "high",
		Reason:     "no matching fingerprint",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	var stdout bytes.Buffer
	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return false },
		Stdin:         strings.NewReader(""),
		Stderr:        &stdout,
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if switched {
		t.Error("expected no switch in non-interactive mode for uncertain state")
	}
	if strings.Contains(stdout.String(), "autoswitch:") {
		t.Errorf("expected no prompt in non-interactive mode, got: %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: ambiguous + non-interactive → no switch
// ---------------------------------------------------------------------------

func TestAutoswitch_Ambiguous_NonInteractive_NoSwitch(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "ambiguous",
		Candidates: []app.Project{{ID: 1, Name: "default"}, {ID: 2, Name: "nt-cli"}},
		Confidence: "low",
		Reason:     "multiple root paths matched",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	var stdout bytes.Buffer
	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return false },
		Stdin:         strings.NewReader(""),
		Stderr:        &stdout,
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if switched {
		t.Error("expected no switch for ambiguous state in non-interactive mode")
	}
}

// ---------------------------------------------------------------------------
// Scenario 7: non-memory command → RunCLIWithStdin does NOT call autoswitch
// ---------------------------------------------------------------------------

func TestRunCLIWithStdin_NonMemoryCommand_NoAutoswitch(t *testing.T) {
	// Use projectMemStore so ProjectEng gets wired, but probe would return
	// "known/high/nt-cli" (project #2). If autoswitch fires for "project"
	// command, the active project would change.
	store := newProjectMemStore() // active = default (#1)
	svc := app.NewService(store, nil)

	// Inject a controllable engine that would switch if called.
	switchCalled := false
	type countingEngine struct {
		app.ProjectEngine
	}
	// We can't intercept at the engine level easily, so instead we check
	// the active project ID doesn't change after running a non-memory command.
	originalID := svc.ActiveProjectID()

	var stdout, stderr bytes.Buffer
	// "project current" is a non-memory command.
	code := app.RunCLIWithStdin(svc, []string{"project", "current"}, strings.NewReader(""), &stdout, &stderr)
	_ = switchCalled
	if code != 0 {
		t.Errorf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	if svc.ActiveProjectID() != originalID {
		t.Errorf("expected active project unchanged for non-memory command, got %d", svc.ActiveProjectID())
	}
}

// ---------------------------------------------------------------------------
// Scenario 8: IsMemoryCommand coverage
// ---------------------------------------------------------------------------

func TestIsMemoryCommand(t *testing.T) {
	memCmds := []string{"save", "recall", "context", "list", "get", "update", "delete"}
	for _, cmd := range memCmds {
		if !app.IsMemoryCommand(cmd) {
			t.Errorf("expected %q to be a memory command", cmd)
		}
	}
	nonMemCmds := []string{"init", "session", "import", "backup", "restore", "doctor", "parity", "project", "mcp"}
	for _, cmd := range nonMemCmds {
		if app.IsMemoryCommand(cmd) {
			t.Errorf("expected %q to NOT be a memory command", cmd)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 9: none status → no switch
// ---------------------------------------------------------------------------

func TestAutoswitch_None_NoSwitch(t *testing.T) {
	store := newAutoswitchStore(app.ProbeResult{
		Status:     "none",
		Confidence: "low",
		Reason:     "not inside a git repo",
	})
	eng := &autoswitchEngine{store: store, probeResult: store.probeResult}
	svc := buildAutoswitchSvc(store, eng)

	policy := app.AutoswitchPolicy{
		GetCwd:        func() (string, error) { return "/fake/cwd", nil },
		IsInteractive: func() bool { return true },
		Stdin:         strings.NewReader("y\n"),
		Stderr:        &bytes.Buffer{},
	}

	switched := app.ApplyAutoswitch(svc, policy)
	if switched {
		t.Error("expected no switch for 'none' probe status")
	}
}
