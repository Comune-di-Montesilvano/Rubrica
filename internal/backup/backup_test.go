package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newTestSQLDB(t *testing.T) *database.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateBackupScheduled(t *testing.T) {
	db := newTestSQLDB(t)
	dir := t.TempDir()

	b, err := CreateBackup(db.DB, dir, false)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if b.Manual {
		t.Error("Manual = true, want false")
	}
	if !strings.HasPrefix(b.Name, "scheduled-") {
		t.Errorf("Name = %q, want scheduled- prefix", b.Name)
	}
	if b.SizeBytes == 0 {
		t.Error("SizeBytes = 0, want > 0 (backup file should have content)")
	}
}

func TestCreateBackupManual(t *testing.T) {
	db := newTestSQLDB(t)
	dir := t.TempDir()

	b, err := CreateBackup(db.DB, dir, true)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if !b.Manual {
		t.Error("Manual = false, want true")
	}
	if !strings.HasPrefix(b.Name, "manual-") {
		t.Errorf("Name = %q, want manual- prefix", b.Name)
	}
}

func TestListBackupsEmptyDir(t *testing.T) {
	backups, err := ListBackups(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("ListBackups on missing dir should not error, got: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("got %d backups, want 0", len(backups))
	}
}

func TestListBackupsOrderedNewestFirst(t *testing.T) {
	db := newTestSQLDB(t)
	dir := t.TempDir()

	b1, err := CreateBackup(db.DB, dir, false)
	if err != nil {
		t.Fatalf("CreateBackup 1 failed: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	b2, err := CreateBackup(db.DB, dir, false)
	if err != nil {
		t.Fatalf("CreateBackup 2 failed: %v", err)
	}

	backups, err := ListBackups(dir)
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("got %d backups, want 2", len(backups))
	}
	if backups[0].Name != b2.Name || backups[1].Name != b1.Name {
		t.Errorf("order = [%s, %s], want [%s, %s] (newest first)", backups[0].Name, backups[1].Name, b2.Name, b1.Name)
	}
}

func TestListBackupsIgnoresUnrelatedFiles(t *testing.T) {
	db := newTestSQLDB(t)
	dir := t.TempDir()

	if _, err := CreateBackup(db.DB, dir, false); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-backup.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("failed to write unrelated file: %v", err)
	}

	backups, err := ListBackups(dir)
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("got %d backups, want 1 (unrelated file should be ignored)", len(backups))
	}
}
