# Backup e ripristino del database

Data: 2026-09-15

## Problema

Rubrica non ha nessun meccanismo di backup: l'unico dato persistente (`/data/ldavsync.db`, SQLite) vive in un named volume Docker (`rubrica-data`) senza alcuna copia di sicurezza. Se il volume viene perso o corrotto, si perde tutto lo stato locale non ri-derivabile da LDAP/PBX (config admin, contatti manuali, aree, gruppi, categorie, mapping OU) — lo stesso scenario già capitato una volta in questa sessione durante il passaggio a named volume (vedi CLAUDE.md, sezione Docker image).

## Non-obiettivi

- Nessuna redondanza esterna (storage remoto, S3, ecc.) — i backup restano nello stesso volume Docker della DB, per scelta esplicita (più semplice, nessun setup infrastrutturale aggiuntivo). Un volume perso porta via anche i backup: limite noto e accettato per questa prima versione.
- Nessuna crittografia dei backup — stesso livello di protezione del file DB originale (permessi filesystem del volume).
- Nessun backup incrementale — ogni backup è uno snapshot completo del DB (dimensione attesa contenuta, poche migliaia di contatti).

## Decisioni

- **Meccanismo di snapshot**: `VACUUM INTO 'path'` (SQL nativo, supportato da SQLite ≥3.27, disponibile in `mattn/go-sqlite3`) invece di una copia file mentre il processo ha il DB aperto. Una copia file a caldo rischia uno snapshot incoerente (letto a metà scrittura) o, se il file viene sostituito via `docker cp` mentre il processo lo tiene aperto, un file descriptor che punta a un inode ormai scollegato — esattamente il problema incontrato manualmente in questa sessione verificando la feature del raggruppamento gerarchico. `VACUUM INTO` produce uno snapshot transazionalmente coerente in un'unica istruzione SQL, senza bloccare le scritture concorrenti più del necessario.
- **Percorso**: `/data/backups/`, stesso volume Docker della DB (`rubrica-data`) — nessuna nuova env var per il path, coerente con la scelta di non introdurre storage esterno in questa prima versione.
- **Naming**: `scheduled-YYYYMMDDTHHMMSS.db` per i backup automatici, `manual-YYYYMMDDTHHMMSS.db` per quelli avviati da `/admin/backup` — il prefisso distingue le due categorie per la retention (vedi sotto) senza bisogno di uno stato separato (DB o file di metadata): la lista dei backup si ottiene semplicemente leggendo la directory.
- **Schedulazione**: nuova variabile d'ambiente `BACKUP_INTERVAL_HOURS` (default 24), stesso pattern di `SYNC_INTERVAL_HOURS`/`ldapSyncWorker` — una goroutine con `time.NewTicker` che esegue lo snapshot e poi la pulizia (retention).
- **Retention (GFS - grandfather/father/son)**: applicata solo ai backup `scheduled-*` dopo ogni backup schedulato riuscito, mai a quelli `manual-*` (un backup manuale resta finché un admin non lo elimina esplicitamente):
  - Ultimi 7 giorni: tenuti tutti (un backup al giorno con l'intervallo di default, quindi fino a 7 file).
  - Giorni 8-35 (4 settimane successive): tenuto un solo backup per settimana di calendario (il più recente della settimana), gli altri eliminati.
  - Oltre 35 giorni fino a 12 mesi: tenuto un solo backup per mese di calendario (il più recente del mese).
  - Oltre 12 mesi: eliminato anche il backup mensile.
- **Backup manuale**: bottone "Backup ora" in `/admin/backup`, stesso pattern async di `handleAdminSyncPBX` (stato in memoria, polling via HTMX per il risultato) — crea un file `manual-*`, mai toccato dalla pulizia automatica.
- **Lista e download**: `/admin/backup` elenca il contenuto di `/data/backups` (nome file, dimensione, data — letti dal filesystem, non da una tabella DB) con un link di download per ciascuno e un pulsante elimina per i manuali (i file `scheduled-*` restano eliminabili manualmente allo stesso modo, per semplicità — non serve impedirlo).
- **Ripristino**: upload di un file `.db` da `/admin/backup`. Validazione minima: i primi 16 byte del file devono essere l'header SQLite (`SQLite format 3\0`) — rifiuta immediatamente file non-SQLite senza tentare di aprirli. Il file caricato sovrascrive `/data/ldavsync.db`; l'applicazione chiude la connessione DB corrente e termina il processo (`os.Exit(0)`) — `restart: unless-stopped` in `docker-compose.yml` fa ripartire il container, che riapre il file appena scritto. Nessun passaggio manuale oltre l'upload.
- **Nessun backup automatico prima del ripristino in questa versione**: un ripristino è distruttivo per definizione (sostituisce lo stato corrente) — l'admin è responsabile di aver eventualmente scaricato lo stato corrente prima di procedere. Il form di upload mostra un avviso esplicito ("Sostituirà tutti i dati correnti, operazione irreversibile").

## Modello dati

Nessuna tabella nuova — i backup sono file sul filesystem, non righe DB. Motivazione: la lista si ottiene leggendo la directory (`os.ReadDir`), il nome file stesso codifica tipo (scheduled/manual) e timestamp; niente stato da tenere sincronizzato con il filesystem, niente rischio di disallineamento tra "quello che il DB dice che esiste" e "quello che esiste davvero su disco".

```go
// internal/backup (nuovo package)
type BackupFile struct {
    Name      string    // es. "scheduled-20260915T140000.db"
    Path      string    // path assoluto completo
    Manual    bool      // prefisso "manual-"
    CreatedAt time.Time // parsata dal nome file
    SizeBytes int64
}
```

## Funzioni principali (`internal/backup`)

```go
// CreateBackup esegue VACUUM INTO su un nuovo file in dir, con prefisso
// "scheduled-" o "manual-" e timestamp corrente. Ritorna il BackupFile
// creato.
func CreateBackup(db *sql.DB, dir string, manual bool) (*BackupFile, error)

// ListBackups legge dir e ritorna i BackupFile trovati (solo file che
// matchano il pattern "scheduled-*.db"/"manual-*.db"), più recenti prima.
func ListBackups(dir string) ([]*BackupFile, error)

// PruneScheduled applica la retention GFS ai soli file "scheduled-*" in
// backups (già ordinati più recenti prima) e elimina dal filesystem
// quelli fuori policy. Ritorna i file eliminati.
func PruneScheduled(backups []*BackupFile, now time.Time) ([]*BackupFile, error)

// ValidateSQLiteHeader legge i primi 16 byte di path e verifica l'header
// SQLite — usata prima di accettare un upload come ripristino.
func ValidateSQLiteHeader(path string) error
```

## Admin UI

Nuova pagina `/admin/backup` (stesso pattern di `/admin/pbx`: config/azioni + stato sync):

- Card "Backup manuale": bottone "Backup ora" (async, polling stato).
- Card "Backup disponibili": tabella (nome, tipo, dimensione, data) con download + elimina per riga.
- Card "Ripristina da backup": form upload file, avviso irreversibilità, conferma esplicita (`hx-confirm` lato client, come già usato per le eliminazioni in `/admin/groups`/`/admin/areas`).

## Rischi / punti aperti per il piano di implementazione

- `VACUUM INTO` fallisce se il file di destinazione esiste già — il naming con timestamp al secondo rende una collisione praticamente impossibile in uso normale, ma il piano deve decidere se gestire l'errore (retry con suffisso) o lasciarlo fallire e loggare (probabile scelta: loggare, è un caso limite non realistico).
- Il riavvio dopo ripristino (`os.Exit(0)`) presuppone `restart: unless-stopped` configurato — vero in `docker-compose.yml` di produzione, ma un `go run cmd/server/main.go` in locale (dev flow, vedi CLAUDE.md) NON riparte da solo: il piano deve documentare che il test locale del ripristino richiede `docker compose up` (non `go run`), o un riavvio manuale.
- Dimensione massima upload per il ripristino: il piano deve verificare/impostare un limite ragionevole lato `http.MaxBytesReader` per l'handler di upload, coerente con la dimensione tipica del DB (poche decine di MB).
