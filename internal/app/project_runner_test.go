package app_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"flint/internal/app"
)

// projectMemStore extends filterMemStore with ProjectStore-like stubs
// so the RunCLI project subcommands can be exercised.
type projectMemStore struct {
	*filterMemStore
	projects        []app.Project
	active          app.Project
	setActiveCalled bool
	lastSetActiveID int64
	backupCalled    bool
	backupErr       error
}

func newProjectMemStore() *projectMemStore {
	active := app.Project{ID: 1, Name: "default"}
	return &projectMemStore{
		filterMemStore: newFilterMemStore(),
		projects:       []app.Project{active, {ID: 2, Name: "nt-cli"}},
		active:         active,
	}
}

func (p *projectMemStore) ListProjects() ([]app.Project, error) { return p.projects, nil }
func (p *projectMemStore) GetActive() (app.Project, error)      { return p.active, nil }
func (p *projectMemStore) SetActive(id int64) error {
	p.setActiveCalled = true
	p.lastSetActiveID = id
	for _, proj := range p.projects {
		if proj.ID == id {
			p.active = proj
		}
	}
	return nil
}
func (p *projectMemStore) CreateProject(in app.ProjectInput) (app.Project, error) {
	proj := app.Project{ID: 99, Name: in.Name}
	p.projects = append(p.projects, proj)
	return proj, nil
}
func (p *projectMemStore) FindByFingerprint(fp string) (*app.Project, error) { return nil, nil }
func (p *projectMemStore) Backup(dst string) error {
	p.backupCalled = true
	return p.backupErr
}
func (p *projectMemStore) Restore(src string) error { return nil }

func runCLIProject(t *testing.T, store *projectMemStore, args ...string) (int, string, string) {
	t.Helper()
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ---------------------------------------------------------------------------
// Task 2.7: project current
// ---------------------------------------------------------------------------

func TestRunCLI_ProjectCurrent_ShowsActiveName(t *testing.T) {
	store := newProjectMemStore()
	code, out, _ := runCLIProject(t, store, "project", "current")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("expected 'default' in output, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Task 2.7: project list
// ---------------------------------------------------------------------------

func TestRunCLI_ProjectList_ShowsAllProjects(t *testing.T) {
	store := newProjectMemStore()
	code, out, _ := runCLIProject(t, store, "project", "list")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("expected 'default' in output, got: %q", out)
	}
	if !strings.Contains(out, "nt-cli") {
		t.Errorf("expected 'nt-cli' in output, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Task 2.7: project switch + Task 2.8: pre-switch backup
// ---------------------------------------------------------------------------

func TestRunCLI_ProjectSwitch_CallsSetActiveAndBackup(t *testing.T) {
	store := newProjectMemStore()
	code, out, stderr := runCLIProject(t, store, "project", "switch", "2")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr)
	}
	if !store.setActiveCalled {
		t.Error("expected SetActive to be called")
	}
	if store.lastSetActiveID != 2 {
		t.Errorf("expected SetActive(2), got %d", store.lastSetActiveID)
	}
	// Pre-switch backup must fire (task 2.8)
	if !store.backupCalled {
		t.Error("expected pre-switch backup to be taken")
	}
	if !strings.Contains(out, "switched") {
		t.Errorf("expected 'switched' in output, got: %q", out)
	}
}

func TestRunCLI_ProjectSwitch_InvalidID_Fails(t *testing.T) {
	store := newProjectMemStore()
	code, _, stderr := runCLIProject(t, store, "project", "switch", "notanumber")
	if code == 0 {
		t.Error("expected non-zero exit for invalid id")
	}
	if !strings.Contains(stderr, "id") {
		t.Errorf("expected error mentioning id, got: %q", stderr)
	}
}

func TestRunCLI_ProjectSwitch_BackupFailureAbortsSwitch(t *testing.T) {
	store := newProjectMemStore()
	store.backupErr = errors.New("disk full")
	code, out, stderr := runCLIProject(t, store, "project", "switch", "2")
	if code == 0 {
		t.Fatalf("expected non-zero exit on backup failure")
	}
	if store.setActiveCalled {
		t.Fatalf("switch must not run when backup fails")
	}
	if !strings.Contains(strings.ToLower(stderr), "pre-switch backup") {
		t.Fatalf("expected pre-switch backup error in stderr, got %q", stderr)
	}
	if strings.Contains(strings.ToLower(out), "switched") {
		t.Fatalf("unexpected success output when backup fails: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Task 2.7: project detect
// ---------------------------------------------------------------------------

func TestRunCLI_ProjectDetect_OutputsStatus(t *testing.T) {
	store := newProjectMemStore()
	code, out, _ := runCLIProject(t, store, "project", "detect")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	// Must contain the status field
	if !strings.ContainsAny(out, "known new none ambiguous") {
		t.Errorf("expected probe status in output, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Task 2.7: unknown subcommand fails
// ---------------------------------------------------------------------------

func TestRunCLI_Project_UnknownSubcommand_Fails(t *testing.T) {
	store := newProjectMemStore()
	code, _, stderr := runCLIProject(t, store, "project", "bogus")
	if code == 0 {
		t.Error("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("expected subcommand name in error, got: %q", stderr)
	}
}

// No subcommand → usage
func TestRunCLI_Project_NoSubcommand_ShowsUsage(t *testing.T) {
	store := newProjectMemStore()
	code, _, stderr := runCLIProject(t, store, "project")
	if code == 0 {
		t.Error("expected non-zero exit for missing subcommand")
	}
	_ = stderr
}
