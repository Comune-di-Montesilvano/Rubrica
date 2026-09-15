# Backup and Restore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Backup/ripristino della DB SQLite di Rubrica — snapshot automatici schedulati con retention GFS, backup manuale on-demand, ripristino via upload da `/admin/backup`.

**Architecture:** Nuovo package `internal/backup` (snapshot via `VACUUM INTO`, lista/retention su filesystem — nessuna tabella DB nuova). Wiring in `cmd/server/main.go`: una goroutine `backupWorker()` (stesso pattern di `ldapSyncWorker`) per gli snapshot schedulati, handler admin per trigger manuale/lista/download/elimina/ripristino, riuso dello `syncStatus`/`sync_status.html` già esistenti per il feedback async.

**Tech Stack:** Go 1.25, `mattn/go-sqlite3` (`VACUUM INTO` nativo SQLite ≥3.27), `html/template`, `gorilla/mux`, HTMX (upload multipart + polling, nessun JS nuovo).

**Spec:** `docs/superpowers/specs/2026-09-15-backup-restore-design.md`

## Global Constraints

- Nessuna tabella DB nuova — i backup sono file in `/data/backups/`, letti dal filesystem.
- Naming file: `scheduled-YYYYMMDDTHHMMSS.db` (schedulati) / `manual-YYYYMMDDTHHMMSS.db` (manuali).
- Retention GFS solo su `scheduled-*`: tutti quelli ≤7 giorni, poi 1/settimana fino a 35 giorni, poi 1/mese fino a 365 giorni, oltre eliminati. `manual-*` mai toccati dalla pulizia automatica.
- Snapshot sempre via `VACUUM INTO`, mai copia file a caldo (rischio di leggere un file dietro un inode scollegato — vedi CLAUDE.md, gotcha incontrato in questa stessa sessione con `docker cp`).
- Il ripristino, dopo aver sostituito `/data/ldavsync.db`, deve rimuovere anche `-wal`/`-shm` residui prima di riavviare (stesso motivo: file "gemelli" disallineati con la nuova DB causano errori "readonly database"/corruzione, incontrato manualmente in sessione).

---

### Task 1: Package `internal/backup` — creazione e lista snapshot

**Files:**
- Create: `internal/backup/backup.go`
- Test: `internal/backup/backup_test.go`

**Interfaces:**
- Produces:
  ```go
  type BackupFile struct {
      Name      string
      Path      string
      Manual    bool
      CreatedAt time.Time
      SizeBytes int64
  }
  func CreateBackup(sqlDB *sql.DB, dir string, manual bool) (*BackupFile, error)
  func ListBackups(dir string) ([]*BackupFile, error) // più recenti prima
  ```

- [ ] **Step 1: Scrivi il test (fallisce: package non esiste)**

Crea `internal/backup/backup_test.go`:

```go
package backup

import (
	"path/filepath"
	"testing"

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
	if !strings_HasPrefix(b.Name, "scheduled-") {
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
	if !strings_HasPrefix(b.Name, "manual-") {
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
	// Timestamp nel nome file ha risoluzione al secondo: forziamo un
	// secondo di differenza così i due backup hanno nomi diversi invece
	// di collidere (VACUUM INTO fallisce se il file di destinazione
	// esiste già).
	time_Sleep1s()
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
	if err := os_WriteFile(filepath.Join(dir, "not-a-backup.txt"), []byte("x")); err != nil {
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
```

*Nota*: i placeholder `strings_HasPrefix`, `time_Sleep1s`, `os_WriteFile` nel test sopra vanno sostituiti con le chiamate dirette agli stdlib package nel passo di implementazione (`strings.HasPrefix`, `time.Sleep(1100 * time.Millisecond)`, `os.WriteFile(path, data, 0o644)`) insieme ai relativi import (`strings`, `time`, `os`) — scritti così solo per non far compilare il file prima del passo 3 senza gli import corretti; il file finale del passo 3 li sostituisce integralmente.

- [ ] **Step 2: Esegui il test, verifica il fallimento**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -v"`
Expected: FAIL — il package `internal/backup` non esiste ancora (`no Go files`), oppure una volta creato lo scheletro, `undefined: CreateBackup`.

- [ ] **Step 3: Riscrivi il test con gli import corretti e implementa il package**

Sostituisci `internal/backup/backup_test.go` con la versione finale (stesso contenuto del passo 1, ma con import stdlib diretti invece dei placeholder):

```go
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
```

Crea `internal/backup/backup.go`:

```go
// Package backup gestisce gli snapshot della DB SQLite di Rubrica —
// creazione (VACUUM INTO, mai copia file a caldo), lista e retention.
// Nessuna tabella DB dedicata: i backup sono file sul filesystem,
// letti/elencati direttamente da lì (vedi
// docs/superpowers/specs/2026-09-15-backup-restore-design.md).
package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const timestampLayout = "20060102T150405"

// BackupFile descrive uno snapshot esistente sul filesystem.
type BackupFile struct {
	Name      string
	Path      string
	Manual    bool
	CreatedAt time.Time
	SizeBytes int64
}

var filenamePattern = regexp.MustCompile(`^(scheduled|manual)-(\d{8}T\d{6})\.db$`)

// CreateBackup crea uno snapshot coerente del DB in dir via VACUUM INTO —
// mai una copia del file mentre il processo lo tiene aperto (rischio di
// snapshot incoerente, o di un file descriptor che punta a un inode
// scollegato se il file viene sostituito da fuori mentre è aperto).
func CreateBackup(sqlDB *sql.DB, dir string, manual bool) (*BackupFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	prefix := "scheduled"
	if manual {
		prefix = "manual"
	}
	now := time.Now()
	name := fmt.Sprintf("%s-%s.db", prefix, now.Format(timestampLayout))
	path := filepath.Join(dir, name)

	if _, err := sqlDB.Exec("VACUUM INTO ?", path); err != nil {
		return nil, fmt.Errorf("failed to vacuum into %s: %w", path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat new backup file: %w", err)
	}

	return &BackupFile{
		Name:      name,
		Path:      path,
		Manual:    manual,
		CreatedAt: now,
		SizeBytes: info.Size(),
	}, nil
}

// ListBackups legge dir e ritorna i BackupFile trovati (solo file che
// matchano "scheduled-*.db"/"manual-*.db"), più recenti prima. Una dir
// mancante non è un errore — ritorna una lista vuota (caso "nessun
// backup ancora fatto").
func ListBackups(dir string) ([]*BackupFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	var backups []*BackupFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := filenamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		ts, err := time.Parse(timestampLayout, m[2])
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		backups = append(backups, &BackupFile{
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			Manual:    m[1] == "manual",
			CreatedAt: ts,
			SizeBytes: info.Size(),
		})
	}

	sort.Slice(backups, func(i, j int) bool { return backups[i].CreatedAt.After(backups[j].CreatedAt) })
	return backups, nil
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -v"`
Expected: PASS su tutti e 5 i test. Se `VACUUM INTO ?` fallisce con un errore di sintassi/binding dal driver (alcuni driver SQLite non accettano parametri bindati su `VACUUM INTO`), sostituisci con `fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(path, "'", "''"))` (escaping dell'apice singolo, standard SQL) passato a `sqlDB.Exec` senza argomenti — il path è generato internamente da `CreateBackup` stesso (timestamp + prefisso fisso), non input utente, quindi l'escaping è una precauzione, non una mitigazione di un rischio reale di injection.

- [ ] **Step 5: Commit**

```bash
git add internal/backup/backup.go internal/backup/backup_test.go
git commit -m "feat(backup): creazione e lista snapshot DB (VACUUM INTO)"
```

---

### Task 2: Retention GFS (`PruneScheduled`)

**Files:**
- Modify: `internal/backup/backup.go`
- Test: `internal/backup/backup_test.go`

**Interfaces:**
- Consumes: `BackupFile` da Task 1
- Produces:
  ```go
  // PruneScheduled applica la retention GFS ai soli BackupFile con
  // Manual=false: tutti quelli con età <= 7 giorni, poi il più recente
  // per settimana di calendario fino a 35 giorni, poi il più recente per
  // mese di calendario fino a 365 giorni, oltre eliminati. Ritorna i file
  // effettivamente eliminati (già rimossi dal filesystem).
  func PruneScheduled(backups []*BackupFile, now time.Time) ([]*BackupFile, error)
  ```

- [ ] **Step 1: Scrivi il test (fallisce: funzione non esiste)**

Aggiungi in `internal/backup/backup_test.go`:

```go
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

	// Due backup nella stessa settimana di calendario, entrambi tra 8 e
	// 35 giorni fa: solo il più recente dei due deve sopravvivere.
	older := now.AddDate(0, 0, -10)
	newer := now.AddDate(0, 0, -9)
	// Stessa settimana ISO: se AddDate(-10)/-9 dovessero cadere a
	// cavallo di due settimane diverse il test andrebbe rivisto, ma con
	// now fisso al 2026-09-15 (martedì) -9/-10 giorni restano entrambi
	// nella settimana del 2026-09-07..13.
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

	older := now.AddDate(0, -2, -1) // ~2 mesi e un giorno fa, stesso mese di "newer"
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
```

Aggiungi `"fmt"` se non già importato in cima al file di test (serve per `fmt.Sprintf` nei nuovi test) — verifica l'import esistente dal passo precedente, `fmt` non era ancora presente nel file di test.

- [ ] **Step 2: Esegui i test, verifica il fallimento**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -run TestPruneScheduled -v"`
Expected: FAIL con `undefined: PruneScheduled`.

- [ ] **Step 3: Implementa `PruneScheduled`**

Aggiungi in `internal/backup/backup.go`, dopo `ListBackups`:

```go
// PruneScheduled applica la retention GFS (grandfather/father/son) ai
// soli backup con Manual=false — i manuali restano finché un admin non
// li elimina esplicitamente. backups non deve necessariamente essere
// ordinato: la funzione elabora ogni entry in base alla propria età, e
// per settimana/mese tiene il più recente scoperto finora in quel
// bucket (quindi FUNZIONA MEGLIO se la lista è già ordinata più recenti
// prima, come ritornata da ListBackups — con ordine diverso il "più
// recente" per bucket non è garantito). Ritorna i file eliminati (già
// rimossi dal filesystem al momento del ritorno).
func PruneScheduled(backups []*BackupFile, now time.Time) ([]*BackupFile, error) {
	const day = 24 * time.Hour
	var deleted []*BackupFile
	seenWeek := map[string]bool{}
	seenMonth := map[string]bool{}

	for _, b := range backups {
		if b.Manual {
			continue
		}
		age := now.Sub(b.CreatedAt)

		var bucketKey string
		var seen map[string]bool
		switch {
		case age <= 7*day:
			continue // livello giornaliero: tenuto sempre
		case age <= 35*day:
			year, week := b.CreatedAt.ISOWeek()
			bucketKey = fmt.Sprintf("%d-W%02d", year, week)
			seen = seenWeek
		case age <= 365*day:
			bucketKey = b.CreatedAt.Format("2006-01")
			seen = seenMonth
		default:
			if err := os.Remove(b.Path); err != nil {
				return deleted, fmt.Errorf("failed to remove %s: %w", b.Path, err)
			}
			deleted = append(deleted, b)
			continue
		}

		if seen[bucketKey] {
			if err := os.Remove(b.Path); err != nil {
				return deleted, fmt.Errorf("failed to remove %s: %w", b.Path, err)
			}
			deleted = append(deleted, b)
		} else {
			seen[bucketKey] = true
		}
	}

	return deleted, nil
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -v"`
Expected: PASS su tutti i test del package (Task 1 + Task 2).

- [ ] **Step 5: Commit**

```bash
git add internal/backup/backup.go internal/backup/backup_test.go
git commit -m "feat(backup): retention GFS (7 giornalieri/4 settimanali/12 mensili)"
```

---

### Task 3: Validazione header SQLite (`ValidateSQLiteHeader`)

**Files:**
- Modify: `internal/backup/backup.go`
- Test: `internal/backup/backup_test.go`

**Interfaces:**
- Produces:
  ```go
  func ValidateSQLiteHeader(path string) error // nil se path è un file SQLite valido
  ```

- [ ] **Step 1: Scrivi il test (fallisce: funzione non esiste)**

Aggiungi in `internal/backup/backup_test.go`:

```go
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
```

- [ ] **Step 2: Esegui il test, verifica il fallimento**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -run TestValidateSQLiteHeader -v"`
Expected: FAIL con `undefined: ValidateSQLiteHeader`.

- [ ] **Step 3: Implementa `ValidateSQLiteHeader`**

Aggiungi in `internal/backup/backup.go` (aggiungi `"io"` agli import):

```go
// sqliteHeaderMagic sono i primi 16 byte di ogni file SQLite valido
// (https://www.sqlite.org/fileformat.html#the_database_header).
var sqliteHeaderMagic = []byte("SQLite format 3\x00")

// ValidateSQLiteHeader legge i primi 16 byte di path e verifica l'header
// SQLite — usata prima di accettare un file caricato come ripristino,
// per rifiutare subito un file non-SQLite senza tentare di aprirlo come
// DB (che con go-sqlite3 potrebbe anche "riuscire" silenziosamente su un
// file vuoto/malformato, creando confusione).
func ValidateSQLiteHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	header := make([]byte, len(sqliteHeaderMagic))
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("failed to read file header: %w", err)
	}
	for i, b := range sqliteHeaderMagic {
		if header[i] != b {
			return fmt.Errorf("file does not have a valid SQLite header")
		}
	}
	return nil
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/backup/... -v"`
Expected: PASS su tutti i test del package `internal/backup`.

- [ ] **Step 5: Commit**

```bash
git add internal/backup/backup.go internal/backup/backup_test.go
git commit -m "feat(backup): validazione header SQLite per gli upload di ripristino"
```

---

### Task 4: Config + scheduler + admin UI (lista/manuale/download/elimina)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/server/main.go`
- Create: `web/templates/admin_backup.html`
- Create: `web/templates/admin_page_backup.html`
- Modify: `web/templates/rail.html`
- Modify: `docker-compose.yml`

**Interfaces:**
- Consumes: `backup.CreateBackup`, `backup.ListBackups`, `backup.PruneScheduled` da Task 1-2; `syncStatus`/`sync_status.html` esistenti

- [ ] **Step 1: Aggiungi `BackupIntervalHours` alla config**

In `internal/config/config.go`, nel blocco `type Config struct`, aggiungi dopo `SyncIntervalHours int`:

```go
	BackupIntervalHours int
```

E in `Load()`, dopo `SyncIntervalHours: getEnvInt("SYNC_INTERVAL_HOURS", 1),`:

```go
		BackupIntervalHours: getEnvInt("BACKUP_INTERVAL_HOURS", 24),
```

- [ ] **Step 2: Variabili globali e directory backup in `main.go`**

In `cmd/server/main.go`, nel blocco `var (...)` dopo `pbxManualSync = &syncStatus{}`, aggiungi:

```go
	backupManualSync = &syncStatus{}
```

Aggiungi `"path/filepath"` e `"github.com/Comune-di-Montesilvano/Rubrica/internal/backup"` agli import.

Dopo il caricamento di `cfg = config.Load()` in `main()`, aggiungi una funzione helper (fuori da `main()`, vicino a `railData()`):

```go
// backupDir è la directory dei backup, derivata da DatabasePath (stesso
// volume Docker della DB — vedi spec, nessuna env var dedicata in questa
// prima versione).
func backupDir() string {
	return filepath.Join(filepath.Dir(cfg.DatabasePath), "backups")
}
```

- [ ] **Step 3: Goroutine schedulata**

In `cmd/server/main.go`, dopo la funzione `ldapSyncWorker()`, aggiungi:

```go
// backupWorker esegue uno snapshot schedulato ogni BACKUP_INTERVAL_HOURS
// e applica subito dopo la retention GFS — stesso pattern di
// ldapSyncWorker, ma con un ticker indipendente (la cadenza di backup
// non ha motivo di essere legata a quella del sync LDAP/PBX).
func backupWorker() {
	ticker := time.NewTicker(time.Duration(cfg.BackupIntervalHours) * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("[BACKUP] Starting scheduled backup...")
		b, err := backup.CreateBackup(db.DB, backupDir(), false)
		if err != nil {
			log.Printf("[BACKUP] Scheduled backup failed: %v", err)
			continue
		}
		log.Printf("[BACKUP] Scheduled backup created: %s", b.Name)

		backups, err := backup.ListBackups(backupDir())
		if err != nil {
			log.Printf("[BACKUP] Failed to list backups for pruning: %v", err)
			continue
		}
		deleted, err := backup.PruneScheduled(backups, time.Now())
		if err != nil {
			log.Printf("[BACKUP] Pruning failed: %v", err)
			continue
		}
		if len(deleted) > 0 {
			log.Printf("[BACKUP] Pruned %d old scheduled backups", len(deleted))
		}
	}
}
```

In `main()`, subito dopo `go ldapSyncWorker()`, aggiungi:

```go
	go backupWorker()
```

- [ ] **Step 4: Route admin**

In `cmd/server/main.go`, nel blocco delle route admin, dopo le route `/pbx/...`, aggiungi:

```go
	admin.HandleFunc("/backup", handleAdminBackup).Methods("GET")
	admin.HandleFunc("/backup/create", handleAdminCreateBackup).Methods("POST")
	admin.HandleFunc("/backup/status", handleAdminBackupStatus).Methods("GET")
	admin.HandleFunc("/backup/download/{name}", handleAdminDownloadBackup).Methods("GET")
	admin.HandleFunc("/backup/{name}/delete", handleAdminDeleteBackup).Methods("POST")
```

- [ ] **Step 5: Handler pagina, trigger manuale, download, elimina**

Aggiungi in `cmd/server/main.go`, dopo `handleAdminSyncPBXStatus`/`renderSyncStatusPBX` (fine del blocco PBX):

```go
// backupPageData raccoglie i dati comuni alla pagina /admin/backup e al
// suo frammento — la lista backup va ricaricata ad ogni render perché
// riflette lo stato del filesystem, non della DB.
func backupPageData(r *http.Request) map[string]interface{} {
	backups, err := backup.ListBackups(backupDir())
	if err != nil {
		log.Printf("[ADMIN] Failed to list backups: %v", err)
	}
	return map[string]interface{}{
		"Backups":  backups,
		"Messages": i18n.GetMessages(i18n.ResolveLocale(r)),
	}
}

func handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	data := railData()
	for k, v := range backupPageData(r) {
		data[k] = v
	}
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-backup"
	templates.ExecuteTemplate(w, "admin_page_backup.html", data)
}

func renderAdminBackup(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "admin_backup.html", backupPageData(r))
}

// handleAdminCreateBackup avvia un backup manuale in background e
// ritorna subito il frammento di stato "in corso" — stesso pattern
// async di handleAdminSyncPBX, riusa lo stesso sync_status.html.
func handleAdminCreateBackup(w http.ResponseWriter, r *http.Request) {
	backupManualSync.start()
	go func() {
		_, err := backup.CreateBackup(db.DB, backupDir(), true)
		if err != nil {
			log.Printf("[BACKUP] Manual backup failed: %v", err)
		} else {
			log.Printf("[BACKUP] Manual backup completed")
		}
		backupManualSync.finish(err)
	}()
	renderBackupSyncStatus(w, r)
}

func handleAdminBackupStatus(w http.ResponseWriter, r *http.Request) {
	renderBackupSyncStatus(w, r)
}

func renderBackupSyncStatus(w http.ResponseWriter, r *http.Request) {
	st := backupManualSync.snapshot()
	data := map[string]interface{}{
		"Running":   st.Running,
		"Phase":     st.Phase,
		"Message":   st.Message,
		"IsError":   st.IsError,
		"StatusURL": "/admin/backup/status",
	}
	if !st.Running {
		// Il backup manuale appena creato deve comparire nella lista
		// senza bisogno di un reload — swap-out-of-band sullo stesso
		// contenitore usato dal caricamento pagina.
		data["OOBBackupList"] = true
	}
	templates.ExecuteTemplate(w, "backup_sync_status.html", data)
}

// backupNamePattern valida {name} dagli URL di download/elimina — deve
// combaciare esattamente con lo schema di naming di backup.CreateBackup,
// altrimenti rifiuta (previene path traversal tipo "../../etc/passwd").
var backupNamePattern = regexp.MustCompile(`^(scheduled|manual)-\d{8}T\d{6}\.db$`)

func handleAdminDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !backupNamePattern.MatchString(name) {
		http.Error(w, "Nome file non valido", http.StatusBadRequest)
		return
	}
	path := filepath.Join(backupDir(), name)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeFile(w, r, path)
}

func handleAdminDeleteBackup(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !backupNamePattern.MatchString(name) {
		http.Error(w, "Nome file non valido", http.StatusBadRequest)
		return
	}
	if err := os.Remove(filepath.Join(backupDir(), name)); err != nil && !os.IsNotExist(err) {
		log.Printf("[ADMIN] Failed to delete backup %s: %v", name, err)
	}
	renderAdminBackup(w, r)
}
```

Aggiungi `"os"` e `"regexp"` agli import di `cmd/server/main.go` se non già presenti (verifica il blocco import esistente prima di aggiungerli, per non duplicarli).

- [ ] **Step 6: Template frammento stato (riuso di sync_status.html con variante OOB)**

Il `sync_status.html` esistente non ha un modo per triggerare il refresh della lista backup dopo il completamento. Crea `web/templates/backup_sync_status.html` come variante dedicata (non modificare `sync_status.html`, usato anche da LDAP/PBX):

```html
{{if .Running}}
<div class="sync-status running" hx-get="{{.StatusURL}}" hx-trigger="load delay:1200ms" hx-target="this" hx-swap="outerHTML">
    <svg class="spin" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.5 9a9 9 0 0114.5-4.5L23 10M1 14l5 4.5A9 9 0 0020.5 15"/></svg>
    {{.Phase}}
</div>
{{else}}
<div class="sync-status {{if .IsError}}error{{else}}success{{end}}">{{.Message}}</div>
{{if .OOBBackupList}}<div id="backup-list" hx-get="/admin/backup" hx-trigger="load" hx-target="this" hx-swap="innerHTML" hx-select="#backup-list-inner" hx-swap-oob="true"></div>{{end}}
{{end}}
```

*Nota*: questo pattern (`hx-select` per estrarre solo `#backup-list-inner` dalla risposta HTML completa di `/admin/backup`) è più semplice di aggiungere un endpoint dedicato solo-lista — richiede che `admin_backup.html` (Step 7) avvolga la tabella in un elemento `id="backup-list-inner"`.

- [ ] **Step 7: Template frammento pagina backup**

Crea `web/templates/admin_backup.html`:

```html
<div id="backup-list-inner">
<div class="card">
    <h3>Backup manuale</h3>
    <p class="helptext" style="margin-bottom:10px;">Crea uno snapshot immediato del database, mai eliminato automaticamente.</p>
    <button hx-post="/admin/backup/create" hx-target="#backup-sync-status" hx-swap="innerHTML" class="btn btn-primary" style="flex:none;">Backup ora</button>
    <div id="backup-sync-status"></div>
</div>

<div class="card" style="margin-top:16px;">
    <h3>Backup disponibili</h3>
    {{if .Backups}}
    <div class="modal-scroll-x">
    <table>
        <thead>
            <tr>
                <th>Nome</th>
                <th>Tipo</th>
                <th>Data</th>
                <th>Dimensione</th>
                <th></th>
            </tr>
        </thead>
        <tbody>
            {{range .Backups}}
            <tr>
                <td class="num">{{.Name}}</td>
                <td>{{if .Manual}}Manuale{{else}}Schedulato{{end}}</td>
                <td>{{.CreatedAt.Format "2006-01-02 15:04:05"}}</td>
                <td>{{.SizeBytes}} byte</td>
                <td>
                    <div class="row-actions">
                        <a href="/admin/backup/download/{{.Name}}" class="btn btn-ghost">Scarica</a>
                        <button hx-post="/admin/backup/{{.Name}}/delete" hx-confirm="Eliminare il backup {{.Name}}?" hx-target="#backup-list-inner" hx-swap="outerHTML" class="btn-danger">Elimina</button>
                    </div>
                </td>
            </tr>
            {{end}}
        </tbody>
    </table>
    </div>
    {{else}}
    <p class="helptext" style="text-align:center;padding:16px 0;">Nessun backup ancora disponibile.</p>
    {{end}}
</div>
</div>
```

- [ ] **Step 8: Shell pagina + voce di navigazione**

Crea `web/templates/admin_page_backup.html`:

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Backup - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        {{template "rail.html" .}}
        <main class="main" style="max-width:920px;">
            <h1 class="page-title">Backup</h1>
            {{template "admin_backup.html" .}}
        </main>
    </div>
</body>
</html>
```

In `web/templates/rail.html`, trova la voce `/admin/group-categories` (aggiunta in una feature precedente) e aggiungi subito dopo:

```html
    <a href="/admin/backup" class="rail-item{{if eq .Section "admin-backup"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M21 8v13H3V8"/><path d="M1 3h22v5H1z"/><path d="M10 12h4"/></svg>
        <span>Backup</span>
    </a>
```

- [ ] **Step 9: `docker-compose.yml`**

In `docker-compose.yml`, nel blocco `environment:`, dopo `- SYNC_INTERVAL_HOURS=${SYNC_INTERVAL_HOURS:-1}`, aggiungi:

```yaml
      - BACKUP_INTERVAL_HOURS=${BACKUP_INTERVAL_HOURS:-24}
```

- [ ] **Step 10: Build, vet, rebuild locale, verifica manuale**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && go build ./... && go vet ./..."`
Expected: nessun errore.

Run: `docker compose up -d --build && sleep 3 && docker logs rubrica --tail 15`
Expected: nessun panic di parsing template.

Poi, da browser loggato come admin: naviga `/admin/backup`, clicca "Backup ora", verifica che appaia lo stato "in corso" seguito da successo e che il file compaia nella tabella con link "Scarica" funzionante (scarica un file `.db` non vuoto).

- [ ] **Step 11: Commit**

```bash
git add internal/config/config.go cmd/server/main.go web/templates/admin_backup.html web/templates/admin_page_backup.html web/templates/backup_sync_status.html web/templates/rail.html docker-compose.yml
git commit -m "feat(admin): pagina /admin/backup, backup manuale/schedulato, download/elimina"
```

---

### Task 5: Ripristino da upload

**Files:**
- Modify: `cmd/server/main.go`
- Modify: `web/templates/admin_backup.html`

**Interfaces:**
- Consumes: `backup.ValidateSQLiteHeader` da Task 3

- [ ] **Step 1: Route e handler**

In `cmd/server/main.go`, aggiungi la route dopo quelle di backup:

```go
	admin.HandleFunc("/backup/restore", handleAdminRestoreBackup).Methods("POST")
```

Aggiungi l'handler dopo `handleAdminDeleteBackup`:

```go
// handleAdminRestoreBackup sostituisce il DB corrente con il file
// caricato e riavvia il processo — restart: unless-stopped in
// docker-compose.yml lo rialza da solo (vedi spec). Il file va scritto
// prima su un path temporaneo e validato PRIMA di chiudere la
// connessione DB corrente: se la validazione fallisce, l'app continua a
// girare normalmente invece di essere già a metà di uno swap.
func handleAdminRestoreBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20) // 200MB, ampio margine sulla dimensione tipica del DB
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "File troppo grande o richiesta non valida", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("backup_file")
	if err != nil {
		http.Error(w, "File mancante", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tmpPath := cfg.DatabasePath + ".restore-tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		http.Error(w, "Impossibile scrivere il file temporaneo", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(tmpPath)
		http.Error(w, "Errore durante la scrittura del file", http.StatusInternalServerError)
		return
	}
	out.Close()

	if err := backup.ValidateSQLiteHeader(tmpPath); err != nil {
		os.Remove(tmpPath)
		http.Error(w, "Il file caricato non e' un database SQLite valido", http.StatusBadRequest)
		return
	}

	log.Printf("[BACKUP] Ripristino richiesto da %s, riavvio in corso...", sessionAdminUsername(r))
	db.Close()

	if err := os.Rename(tmpPath, cfg.DatabasePath); err != nil {
		log.Fatalf("[BACKUP] Failed to replace database file during restore: %v", err)
	}
	// File "gemelli" del vecchio DB (WAL/SHM) non corrispondono più al
	// file appena scritto — lasciarli porta a "attempt to write a
	// readonly database" al riavvio (vedi CLAUDE.md, gotcha incontrato
	// manualmente in questa stessa sessione con un docker cp).
	os.Remove(cfg.DatabasePath + "-wal")
	os.Remove(cfg.DatabasePath + "-shm")

	w.Write([]byte("Ripristino completato, il servizio si sta riavviando..."))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	os.Exit(0)
}
```

Aggiungi `"io"` agli import di `cmd/server/main.go` se non già presente.

- [ ] **Step 2: Form di upload nel template**

In `web/templates/admin_backup.html`, aggiungi una terza card prima della chiusura di `</div>` finale (dopo la card "Backup disponibili"):

```html
<div class="card" style="margin-top:16px;">
    <h3>Ripristina da backup</h3>
    <p class="helptext" style="margin-bottom:10px;color:var(--danger);">Sostituisce tutti i dati correnti con quelli del file caricato. Operazione irreversibile: il servizio si riavvia subito dopo.</p>
    <form method="POST" action="/admin/backup/restore" enctype="multipart/form-data" onsubmit="return confirm('Sostituire TUTTI i dati correnti con questo backup? Operazione irreversibile.');" style="display:flex;gap:10px;align-items:center;">
        <input type="file" name="backup_file" accept=".db" required class="input">
        <button type="submit" class="btn btn-danger" style="flex:none;">Ripristina</button>
    </form>
</div>
```

*Nota*: form HTML nativo (non HTMX) perché la risposta è testo semplice seguito dal riavvio del processo — non c'è un frammento HTML sensato da fare swap in-place, e `onsubmit="return confirm(...)"` è coerente con `hx-confirm` già usato altrove nella stessa pagina.

- [ ] **Step 3: Build, vet, verifica manuale**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && go build ./... && go vet ./..."`
Expected: nessun errore.

Run: `docker compose up -d --build && sleep 3`

Verifica manuale (**solo con `docker compose up`, non `go run` — vedi Rischi nello spec: `go run` locale non riparte da solo dopo `os.Exit`, serve `restart: unless-stopped`**):
1. Scarica un backup esistente da `/admin/backup` (Task 4).
2. Ricarica `/admin/backup`, carica lo stesso file nel form "Ripristina da backup", conferma.
3. Verifica con `docker logs rubrica --tail 20` che il container si sia riavviato da solo (nuova riga `[MAIN] Starting Rubrica ...`) e che `/` risponda di nuovo dopo pochi secondi.
4. Prova a caricare un file non-SQLite (es. un `.txt` rinominato `.db`): verifica che l'app risponda con errore 400 e **non** si riavvii (i log non devono mostrare un nuovo `[MAIN] Starting`).

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go web/templates/admin_backup.html
git commit -m "feat(admin): ripristino DB da upload backup con restart automatico"
```

## Self-Review

- **Copertura spec**: meccanismo `VACUUM INTO` (Task 1), retention GFS (Task 2), validazione upload (Task 3), scheduler + config + admin UI lista/manuale/download/elimina (Task 4), ripristino con pulizia WAL/SHM e restart (Task 5) — tutte le sezioni della spec hanno un task corrispondente. Il punto aperto "dimensione massima upload" è risolto in Task 5 Step 1 (`http.MaxBytesReader`, 200MB); il punto aperto "collisione filename" è risolto nel commento a `CreateBackup` (Task 1) rimandando a un errore loggato, coerente con la valutazione della spec ("caso limite non realistico").
- **Placeholder**: nessun TBD/TODO residuo nel codice finale (i placeholder nel test di Task 1 Step 1 sono deliberati e sostituiti interamente al Step 3 dello stesso task, con nota esplicita).
- **Coerenza tipi**: `BackupFile`, `CreateBackup`, `ListBackups`, `PruneScheduled`, `ValidateSQLiteHeader` usano firme identiche in tutti i task che li consumano (Task 4 importa da `internal/backup` esattamente i nomi definiti in Task 1-3; Task 5 usa `backup.ValidateSQLiteHeader` con la firma di Task 3).
