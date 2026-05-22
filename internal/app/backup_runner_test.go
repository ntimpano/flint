package app_test

import (
	"bytes"
	"strings"
	"testing"

	"flint/internal/app"
)

// backupMemStore extends memStore with BackupStore so RunCLI can dispatch
// the backup/restore subcommands without booting SQLite.
type backupMemStore struct {
	*memStore

	backupCalls  int
	restoreCalls int
	lastBackup   string
	lastRestore  string
}

func newBackupMemStore() *backupMemStore {
	return &backupMemStore{memStore: newMemStore()}
}

func (b *backupMemStore) Backup(dst string) error {
	b.backupCalls++
	b.lastBackup = dst
	return nil
}

func (b *backupMemStore) Restore(src string) error {
	b.restoreCalls++
	b.lastRestore = src
	return nil
}

var _ app.BackupStore = (*backupMemStore)(nil)

func runCLIBackup(t *testing.T, store *backupMemStore, args ...string) (int, string, string) {
	t.Helper()
	svc := app.NewService(store, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunCLI(svc, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunCLI_Backup_HappyPath(t *testing.T) {
	store := newBackupMemStore()
	code, stdout, stderr := runCLIBackup(t, store, "backup", "/tmp/snap.db")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.backupCalls != 1 || store.lastBackup != "/tmp/snap.db" {
		t.Fatalf("expected 1 call to /tmp/snap.db, got calls=%d path=%q",
			store.backupCalls, store.lastBackup)
	}
	if !strings.Contains(stdout, "/tmp/snap.db") {
		t.Fatalf("stdout missing path: %q", stdout)
	}
}

func TestRunCLI_Restore_HappyPath(t *testing.T) {
	store := newBackupMemStore()
	code, stdout, stderr := runCLIBackup(t, store, "restore", "/tmp/in.db")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if store.restoreCalls != 1 || store.lastRestore != "/tmp/in.db" {
		t.Fatalf("expected 1 call to /tmp/in.db, got calls=%d path=%q",
			store.restoreCalls, store.lastRestore)
	}
	if !strings.Contains(stdout, "/tmp/in.db") {
		t.Fatalf("stdout missing path: %q", stdout)
	}
}

// TestRunCLI_Backup_UsageErrors covers missing path argument.
func TestRunCLI_Backup_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"backup missing path", []string{"backup"}},
		{"restore missing path", []string{"restore"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newBackupMemStore()
			code, _, stderr := runCLIBackup(t, store, tc.args...)
			if code == 0 {
				t.Fatalf("expected non-zero exit")
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("expected stderr message")
			}
			if store.backupCalls != 0 || store.restoreCalls != 0 {
				t.Fatalf("store must not be called on usage error")
			}
		})
	}
}

func TestRunCLI_UsageMentionsBackupRestore(t *testing.T) {
	store := newBackupMemStore()
	_, stdout, _ := runCLIBackup(t, store, "totally-bogus")
	if !strings.Contains(stdout, "backup") || !strings.Contains(stdout, "restore") {
		t.Fatalf("usage banner must mention backup + restore, got:\n%s", stdout)
	}
}
