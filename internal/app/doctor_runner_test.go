package app_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"flint/internal/app"
)

// doctorRunnerStore wires a fake DoctorStore into the runner, embedding
// memStore so the rest of the Service surface stays satisfied.
type doctorRunnerStore struct {
	*memStore

	report app.DoctorReport
	err    error
}

func newDoctorRunnerStore() *doctorRunnerStore {
	return &doctorRunnerStore{memStore: newMemStore()}
}

func (d *doctorRunnerStore) Doctor() (app.DoctorReport, error) {
	if d.err != nil {
		return app.DoctorReport{}, d.err
	}
	return d.report, nil
}

var _ app.DoctorStore = (*doctorRunnerStore)(nil)

// TestRunner_Doctor_PrintsSummary proves the M3 doctor scenario:
// `nt-cli doctor` MUST print the store summary verbatim so users see
// every diagnostic axis (schema, fts, integrity, counts) at a glance.
func TestRunner_Doctor_PrintsSummary(t *testing.T) {
	store := newDoctorRunnerStore()
	store.report = app.DoctorReport{
		SchemaVersion:    3,
		FTSHealthy:       true,
		IntegrityOK:      true,
		MemoryItemsCount: 7,
		SessionsCount:    2,
		Summary:          "schema_version=3  fts=healthy  integrity=ok  memory_items=7  sessions=2",
	}
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, []string{"doctor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), store.report.Summary) {
		t.Fatalf("expected summary in stdout, got %q", stdout.String())
	}
}

func TestRunner_Doctor_PropagatesError(t *testing.T) {
	store := newDoctorRunnerStore()
	store.err = errors.New("integrity failed")
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, []string{"doctor"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit on doctor error")
	}
	if !strings.Contains(stderr.String(), "integrity failed") {
		t.Fatalf("expected error in stderr, got %q", stderr.String())
	}
}

// TestRunner_Doctor_NoArgs proves doctor takes no arguments. Extra args
// MUST surface a usage error so users don't silently ignore typos like
// `nt-cli doctor --json`.
func TestRunner_Doctor_NoArgs(t *testing.T) {
	svc := app.NewService(newDoctorRunnerStore(), nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, []string{"doctor", "extra"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit when extra args provided")
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "doctor") {
		t.Fatalf("expected usage error mentioning doctor, got %q", stderr.String())
	}
}
