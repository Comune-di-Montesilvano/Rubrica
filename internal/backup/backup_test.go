package backup

import (
	"fmt"
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

func makeBackup(dir, name string, manual bool, createdAt time.Time) *BackupFile {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("fake-db"), 0o644); err != nil {
		panic(err)
	}
	return &BackupFile{Name: name, Path: path, Manual: manual, CreatedAt: createdAt}
}

func TestPruneScheduledKeepsAllWithinWeek(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	var backups []*BackupFile
	for i := 0; i < 7; i++ {
		ts := now.AddDate(0, 0, -i)
		backups = append(backups, makeBackup(dir, fmt.Sprintf("scheduled-%s.db", ts.Format(timestampLayout)), false, ts))
	}

	deleted, err := PruneScheduled(backups, now)
	if err != nil {
		t.Fatalf("PruneScheduled failed: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %d backups within the 7-day window, want 0", len(deleted))
	}
}

func TestPruneScheduledKeepsOnePerWeekBeyond7Days(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	older := now.AddDate(0, 0, -10)
	newer := now.AddDate(0, 0, -9)
	b1 := makeBackup(dir, fmt.Sprintf("scheduled-%s.db", older.Format(timestampLayout)), false, older)
	b2 := makeBackup(dir, fmt.Sprintf("scheduled-%s.db", newer.Format(timestampLayout)), false, newer)

	deleted, err := PruneScheduled([]*BackupFile{b2, b1}, now)
	if err != nil {
		t.Fatalf("PruneScheduled failed: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Name != b1.Name {
		t.Fatalf("deleted = %v, want [%s] (the older of the two same-week backups)", deleted, b1.Name)
	}
	if _, err := os.Stat(b2.Path); err != nil {
		t.Errorf("newer backup should survive, but Stat failed: %v", err)
	}
	if _, err := os.Stat(b1.Path); !os.IsNotExist(err) {
		t.Errorf("older backup should be deleted from disk, Stat err = %v", err)
	}
}

func TestPruneScheduledKeepsOnePerMonthBeyond35Days(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	older := now.AddDate(0, -2, -1)
	newer := now.AddDate(0, -2, 0)
	b1 := makeBackup(dir, fmt.Sprintf("scheduled-%s.db", older.Format(timestampLayout)), false, older)
	b2 := makeBackup(dir, fmt.Sprintf("scheduled-%s.db", newer.Format(timestampLayout)), false, newer)

	deleted, err := PruneScheduled([]*BackupFile{b2, b1}, now)
	if err != nil {
		t.Fatalf("PruneScheduled failed: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Name != b1.Name {
		t.Fatalf("deleted = %v, want [%s]", deleted, b1.Name)
	}
}

func TestPruneScheduledDeletesBeyondOneYear(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(-2, 0, 0)
	b := makeBackup(dir, fmt.Sprintf("scheduled-%s.db", old.Format(timestampLayout)), false, old)

	deleted, err := PruneScheduled([]*BackupFile{b}, now)
	if err != nil {
		t.Fatalf("PruneScheduled failed: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("got %d deleted, want 1 (backup older than 1 year)", len(deleted))
	}
}

func TestPruneScheduledNeverTouchesManual(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(-2, 0, 0)
	b := makeBackup(dir, fmt.Sprintf("manual-%s.db", old.Format(timestampLayout)), true, old)

	deleted, err := PruneScheduled([]*BackupFile{b}, now)
	if err != nil {
		t.Fatalf("PruneScheduled failed: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("got %d deleted, want 0 (manual backups are never pruned)", len(deleted))
	}
	if _, err := os.Stat(b.Path); err != nil {
		t.Errorf("manual backup file should still exist: %v", err)
	}
}

func TestValidateSQLiteHeaderAcceptsRealDB(t *testing.T) {
	db := newTestSQLDB(t)
	dir := t.TempDir()
	b, err := CreateBackup(db.DB, dir, false)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	if err := ValidateSQLiteHeader(b.Path); err != nil {
		t.Errorf("ValidateSQLiteHeader on a real backup file failed: %v", err)
	}
}

func TestValidateSQLiteHeaderRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-db.db")
	if err := os.WriteFile(path, []byte("this is definitely not a sqlite file"), 0o644); err != nil {
		t.Fatalf("failed to write garbage file: %v", err)
	}

	if err := ValidateSQLiteHeader(path); err == nil {
		t.Error("ValidateSQLiteHeader should reject a non-SQLite file")
	}
}

func TestValidateSQLiteHeaderRejectsMissingFile(t *testing.T) {
	if err := ValidateSQLiteHeader(filepath.Join(t.TempDir(), "nonexistent.db")); err == nil {
		t.Error("ValidateSQLiteHeader should error on a missing file")
	}
}
