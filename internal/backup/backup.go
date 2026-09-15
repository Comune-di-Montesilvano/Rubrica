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

// PruneScheduled applica la retention GFS (grandfather/father/son) ai
// soli backup con Manual=false — i manuali restano finché un admin non
// li elimina esplicitamente. Funziona meglio se backups è già ordinato
// più recenti prima (come ritornato da ListBackups): per ogni bucket
// settimana/mese tiene il primo che incontra, che così è il più recente.
// Ritorna i file eliminati (già rimossi dal filesystem al momento del
// ritorno).
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
