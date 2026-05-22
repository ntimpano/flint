package app_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flint/internal/app"
)

// importMemStore extends memStore with ImportStore so RunCLI can dispatch
// the import subcommand. Captures the rows received for assertions.
type importMemStore struct {
	*memStore

	calls  int
	lastIn []app.ImportRecord
	result app.ImportResult
}

func newImportMemStore() *importMemStore {
	return &importMemStore{memStore: newMemStore()}
}

func (m *importMemStore) ImportRecords(rows []app.ImportRecord) (app.ImportResult, error) {
	m.calls++
	m.lastIn = rows
	if m.result == (app.ImportResult{}) {
		// Default behaviour: every row counts as inserted.
		m.result = app.ImportResult{Inserted: len(rows)}
	}
	return m.result, nil
}

var _ app.ImportStore = (*importMemStore)(nil)

func runCLIImport(t *testing.T, store *importMemStore, args ...string) (int, string, string) {
	t.Helper()
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestRunCLI_Import_HappyPath proves the JSON import flow: file is
// parsed, store is invoked, summary is printed.
func TestRunCLI_Import_HappyPath(t *testing.T) {
	store := newImportMemStore()
	path := writeFixture(t, "in.json", `[
		{"content":"a","topic_key":"k1"},
		{"content":"b","topic_key":"k2"}
	]`)
	code, stdout, stderr := runCLIImport(t, store, "import", path)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.calls != 1 || len(store.lastIn) != 2 {
		t.Fatalf("expected 1 store call with 2 rows, got calls=%d rows=%d", store.calls, len(store.lastIn))
	}
	if !strings.Contains(stdout, "inserted") || !strings.Contains(stdout, "2") {
		t.Fatalf("stdout missing summary: %q", stdout)
	}
}

// TestRunCLI_Import_DryRun proves spec scenario: --dry-run reports plan
// without touching the store.
func TestRunCLI_Import_DryRun(t *testing.T) {
	store := newImportMemStore()
	path := writeFixture(t, "in.json", `[{"content":"x"}]`)
	code, stdout, _ := runCLIImport(t, store, "import", "--dry-run", path)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if store.calls != 0 {
		t.Fatalf("dry-run must not touch store, got %d calls", store.calls)
	}
	if !strings.Contains(strings.ToLower(stdout), "dry") {
		t.Fatalf("stdout should advertise dry-run, got %q", stdout)
	}
}

// TestRunCLI_Import_UsageErrors covers missing path / nonexistent file.
func TestRunCLI_Import_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing path", []string{"import"}},
		{"nonexistent file", []string{"import", "/nope/does-not-exist.json"}},
		{"unknown flag", []string{"import", "--bogus", "ignored.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newImportMemStore()
			code, _, stderr := runCLIImport(t, store, tc.args...)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("expected stderr message")
			}
			if store.calls != 0 {
				t.Fatalf("store should not be called on usage error")
			}
		})
	}
}

// TestRunCLI_UsageMentionsImport guards discoverability.
func TestRunCLI_UsageMentionsImport(t *testing.T) {
	store := newImportMemStore()
	_, stdout, _ := runCLIImport(t, store, "totally-bogus-cmd")
	if !strings.Contains(stdout, "import") {
		t.Fatalf("usage banner must mention 'import', got:\n%s", stdout)
	}
}
