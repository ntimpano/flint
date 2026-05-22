package app

import (
	"strings"
	"testing"
)

// backupFakeStore extends fakeStore with the BackupStore capability so
// service-level backup/restore tests can run without booting SQLite.
type backupFakeStore struct {
	fakeStore

	backupCalls  int
	restoreCalls int
	lastBackup   string
	lastRestore  string
	failErr      error
}

func (f *backupFakeStore) Backup(dst string) error {
	f.backupCalls++
	f.lastBackup = dst
	return f.failErr
}

func (f *backupFakeStore) Restore(src string) error {
	f.restoreCalls++
	f.lastRestore = src
	return f.failErr
}

// TestService_Backup_ForwardsPath: happy path forwards the trimmed path
// to the store and returns no error.
func TestService_Backup_ForwardsPath(t *testing.T) {
	fake := &backupFakeStore{}
	svc := NewService(fake, nil)
	if err := svc.Backup("  /tmp/snap.db  "); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if fake.backupCalls != 1 || fake.lastBackup != "/tmp/snap.db" {
		t.Fatalf("expected trimmed path forwarded, got calls=%d path=%q",
			fake.backupCalls, fake.lastBackup)
	}
}

// TestService_Backup_RejectsEmpty: empty path errors at the service
// layer without calling the store.
func TestService_Backup_RejectsEmpty(t *testing.T) {
	fake := &backupFakeStore{}
	svc := NewService(fake, nil)
	if err := svc.Backup("   "); err == nil {
		t.Fatalf("expected error for empty path")
	}
	if fake.backupCalls != 0 {
		t.Fatalf("store must not be called on empty path")
	}
}

// TestService_Backup_CapabilityError: a Store without BackupStore returns
// a clear capability error (defensive type-assert pattern).
func TestService_Backup_CapabilityError(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	err := svc.Backup("/tmp/x.db")
	if err == nil {
		t.Fatalf("expected capability error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "backup") {
		t.Fatalf("expected backup-capability error, got %q", err)
	}
}

// TestService_Restore_ForwardsPath mirrors Backup.
func TestService_Restore_ForwardsPath(t *testing.T) {
	fake := &backupFakeStore{}
	svc := NewService(fake, nil)
	if err := svc.Restore("/tmp/in.db"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if fake.restoreCalls != 1 || fake.lastRestore != "/tmp/in.db" {
		t.Fatalf("expected restore call, got calls=%d path=%q",
			fake.restoreCalls, fake.lastRestore)
	}
}

func TestService_Restore_RejectsEmpty(t *testing.T) {
	fake := &backupFakeStore{}
	svc := NewService(fake, nil)
	if err := svc.Restore(""); err == nil {
		t.Fatalf("expected error for empty path")
	}
	if fake.restoreCalls != 0 {
		t.Fatalf("store must not be called on empty path")
	}
}

func TestService_Restore_CapabilityError(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	err := svc.Restore("/tmp/in.db")
	if err == nil {
		t.Fatalf("expected capability error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "restore") {
		t.Fatalf("expected restore-capability error, got %q", err)
	}
}
