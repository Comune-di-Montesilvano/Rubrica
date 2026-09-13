# Integrazione dati centralino PBX (ViVo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sincronizzare ogni ora, insieme al sync LDAP, i peers SIP e i call group del centralino Invidea "ViVo" non presenti nel dominio, arricchendo la rubrica esistente senza toccare i dati LDAP.

**Architecture:** Nuovo package `internal/pbx` (client HTTP/parsing + orchestrazione sync), mirror strutturale di `internal/ldap`. Estende lo schema esistente (`contacts.source='pbx'`, nuove colonne `group_numbers.source`/`name_override`) invece di introdurre tabelle nuove — riusa tutta la UI rubrica/admin già esistente (raggruppamento per `department`, editor gruppi in `/admin/groups`, override contatto). Nessuna env var: URL/utente/password del centralino vivono solo in `app_config`, editabili da una pagina admin dedicata `/admin/pbx`.

**Tech Stack:** Go stdlib (`net/http`, `regexp`, `encoding/json`), `mattn/go-sqlite3` (già in uso), nessuna nuova dipendenza.

**Spec:** `docs/superpowers/specs/2026-09-13-pbx-scraping-design.md`

## Global Constraints

- `go-sqlite3` richiede CGO: qualunque test che tocchi `internal/database` (direttamente o transitivamente, es. `internal/pbx`) va eseguito nel container Docker documentato in `CLAUDE.md`, non nativamente su Windows senza gcc:
  ```bash
  MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
    sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./... -v"
  ```
  Pacchetti senza dipendenza da `internal/database` (es. `internal/config`) si possono testare nativamente con `go test ./internal/config/...`.
- Nessuna nuova dipendenza esterna: parsing HTML/JSON con `regexp`+`encoding/json` della stdlib, come nel PoC già verificato (`cmd/pbxpoc`).
- `contacts.source` diventa `'ldap' | 'manual' | 'pbx'`; ogni nuova query/scan che tocca `contacts` deve continuare a passare stringhe vuote esplicite (mai `NULL`) per le colonne testuali non pertinenti, come già fa `CreateManualContact` — uno `Scan` in `string` semplice fallisce su `NULL`.
- Il sync PBX non deve mai toccare contatti `source='ldap'` o `source='manual'`, né gruppi `source='manual'` (salvo l'unico caso esplicito di migrazione-e-cancellazione descritto nello spec).
- Nessuna env var per le credenziali PBX: solo `app_config` (chiavi `pbx_url`/`pbx_user`/`pbx_pass`), editabile da `/admin/pbx`; subsystem no-op finché l'URL non è configurato da lì.

---

## Task 1: Schema `group_numbers` — colonne `source`/`name_override`

**Files:**
- Modify: `internal/database/sqlite.go`
- Modify: `internal/database/sqlite_test.go`

**Interfaces:**
- Consumes: nessuna (base per Task 3).
- Produces: `database.GroupNumber{ID, Number, Name, Description, Source, NameOverride, CreatedAt, UpdatedAt}`; `db.GetGroup`, `db.ListGroups`, `db.UpdateGroup` aggiornati per includere/gestire `Source`/`NameOverride`.

- [ ] **Step 1: Scrivi il test che fallisce (colonne non ancora esistenti)**

In `internal/database/sqlite_test.go`, aggiungi in fondo al file:

```go
func TestGroupNumberSourceDefaultsToManual(t *testing.T) {
	db := newTestDB(t)
	g := &GroupNumber{Number: "999", Name: "Test Manuale"}
	if err := db.CreateGroup(g); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	got, err := db.GetGroup(g.ID)
	if err != nil {
		t.Fatalf("GetGroup failed: %v", err)
	}
	if got.Source != "manual" {
		t.Errorf("Source = %q, want %q", got.Source, "manual")
	}
	if got.NameOverride {
		t.Error("NameOverride = true, want false di default")
	}
}

func TestUpdateGroupSetsNameOverride(t *testing.T) {
	db := newTestDB(t)
	g := &GroupNumber{Number: "998", Name: "Nome Iniziale"}
	if err := db.CreateGroup(g); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	g.Name = "Nome Modificato"
	if err := db.UpdateGroup(g); err != nil {
		t.Fatalf("UpdateGroup failed: %v", err)
	}

	got, err := db.GetGroup(g.ID)
	if err != nil {
		t.Fatalf("GetGroup failed: %v", err)
	}
	if !got.NameOverride {
		t.Error("NameOverride dovrebbe essere true dopo una modifica manuale via UpdateGroup")
	}
}
```

- [ ] **Step 2: Esegui i test e verifica che falliscano**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run 'TestGroupNumberSourceDefaultsToManual|TestUpdateGroupSetsNameOverride' -v"
```
Expected: FAIL — `GroupNumber` non ha ancora i campi `Source`/`NameOverride` (errore di compilazione).

- [ ] **Step 3: Aggiungi le colonne allo schema**

In `internal/database/sqlite.go`, nella slice `alterStatements` (dentro `migrate()`, circa riga 163-168):

```go
	alterStatements := []string{
		"ALTER TABLE contacts ADD COLUMN title TEXT",
		"ALTER TABLE contacts ADD COLUMN description TEXT",
		"ALTER TABLE contacts ADD COLUMN area TEXT",
		"ALTER TABLE contacts ADD COLUMN source TEXT DEFAULT 'ldap'",
		"ALTER TABLE group_numbers ADD COLUMN source TEXT DEFAULT 'manual'",
		"ALTER TABLE group_numbers ADD COLUMN name_override INTEGER DEFAULT 0",
	}
```

- [ ] **Step 4: Aggiungi i campi al struct `GroupNumber`**

```go
type GroupNumber struct {
	ID           int64
	Number       string
	Name         string
	Description  string
	Source       string // "manual" (default) o "pbx"
	NameOverride bool   // se true, il sync PBX non sovrascrive più Name
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
```

- [ ] **Step 5: Aggiorna `GetGroup` per leggere le nuove colonne**

```go
func (db *DB) GetGroup(id int64) (*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, created_at, updated_at FROM group_numbers WHERE id = ?`
	group := &GroupNumber{}
	err := db.QueryRow(query, id).Scan(&group.ID, &group.Number, &group.Name, &group.Description,
		&group.Source, &group.NameOverride, &group.CreatedAt, &group.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	return group, nil
}
```

- [ ] **Step 6: Aggiorna `ListGroups` allo stesso modo**

```go
func (db *DB) ListGroups() ([]*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, created_at, updated_at FROM group_numbers ORDER BY number`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		group := &GroupNumber{}
		err := rows.Scan(&group.ID, &group.Number, &group.Name, &group.Description,
			&group.Source, &group.NameOverride, &group.CreatedAt, &group.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, nil
}
```

- [ ] **Step 7: Aggiorna `UpdateGroup` per impostare `name_override=1`**

Una modifica manuale via l'admin UI esistente (`/admin/groups`) implica l'intento di fissare il nome — coerente con `UpdateContactOverride` che fa lo stesso su `contacts.manual_override`:

```go
func (db *DB) UpdateGroup(group *GroupNumber) error {
	group.UpdatedAt = time.Now()
	query := `
	UPDATE group_numbers
	SET number = ?, name = ?, description = ?, name_override = 1, updated_at = ?
	WHERE id = ?
	`
	_, err := db.Exec(query, group.Number, group.Name, group.Description, group.UpdatedAt, group.ID)
	return err
}
```

- [ ] **Step 8: Esegui i test e verifica che passino**

Run: stesso comando dello Step 2.
Expected: PASS.

- [ ] **Step 9: Esegui l'intera suite per verificare che nulla si sia rotto**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./... -v"
```
Expected: PASS su tutti i pacchetti.

- [ ] **Step 10: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): colonne source/name_override su group_numbers"
```

---

## Task 2: Operazioni DB per i contatti PBX

**Files:**
- Modify: `internal/database/sqlite.go`
- Modify: `internal/database/sqlite_test.go`

**Interfaces:**
- Consumes: `Contact` struct (esistente, invariato).
- Produces: `db.ListDomainExtensions() ([]string, error)`, `db.GetContactByExtension(ext string) (*Contact, error)`, `db.UpsertPBXContact(c *Contact) error`, `db.SoftDeleteStalePBXContacts(syncTime time.Time) (int64, error)` — usati da `internal/pbx.ApplyPeers` (Task 5).

- [ ] **Step 1: Scrivi i test che falliscono**

In fondo a `internal/database/sqlite_test.go`:

```go
func TestUpsertPBXContactCreatesWithSourcePBX(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{UID: "pbx-280", DisplayName: "FRATELLI DITALIA", LDAPExt: "280", PrimaryNumber: "280", Department: "Centralino - non mappato", LastSync: time.Now()}
	if err := db.UpsertPBXContact(c); err != nil {
		t.Fatalf("UpsertPBXContact failed: %v", err)
	}

	got, err := db.GetContact("pbx-280")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got == nil || got.Source != "pbx" {
		t.Fatalf("got = %+v, want source=pbx", got)
	}
}

func TestUpsertPBXContactRespectsManualOverride(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{UID: "pbx-281", DisplayName: "NOME ORIGINALE", LDAPExt: "281", PrimaryNumber: "281", LastSync: time.Now()}
	if err := db.UpsertPBXContact(c); err != nil {
		t.Fatalf("first UpsertPBXContact failed: %v", err)
	}
	if err := db.UpdateContactOverride("pbx-281", "", ""); err != nil {
		t.Fatalf("UpdateContactOverride failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE contacts SET display_name = ? WHERE uid = ?`, "Nome Personalizzato", "pbx-281"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	c2 := &Contact{UID: "pbx-281", DisplayName: "NOME CAMBIATO DAL CENTRALINO", LDAPExt: "281", PrimaryNumber: "281", LastSync: time.Now()}
	if err := db.UpsertPBXContact(c2); err != nil {
		t.Fatalf("second UpsertPBXContact failed: %v", err)
	}

	got, _ := db.GetContact("pbx-281")
	if got.DisplayName != "Nome Personalizzato" {
		t.Errorf("DisplayName = %q, want override preservato %q", got.DisplayName, "Nome Personalizzato")
	}
}

func TestListDomainExtensionsOnlyLDAP(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&Contact{UID: "u1", DisplayName: "U1", LDAPExt: "100", LastSync: time.Now()}); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}
	if err := db.UpsertPBXContact(&Contact{UID: "pbx-200", DisplayName: "P200", LDAPExt: "200", PrimaryNumber: "200", LastSync: time.Now()}); err != nil {
		t.Fatalf("UpsertPBXContact failed: %v", err)
	}

	exts, err := db.ListDomainExtensions()
	if err != nil {
		t.Fatalf("ListDomainExtensions failed: %v", err)
	}
	if len(exts) != 1 || exts[0] != "100" {
		t.Errorf("exts = %v, want [100] (solo source=ldap)", exts)
	}
}

func TestGetContactByExtension(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&Contact{UID: "u1", DisplayName: "U1", LDAPExt: "700", LastSync: time.Now()}); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	got, err := db.GetContactByExtension("700")
	if err != nil {
		t.Fatalf("GetContactByExtension failed: %v", err)
	}
	if got == nil || got.UID != "u1" {
		t.Fatalf("got = %+v, want uid=u1", got)
	}

	none, err := db.GetContactByExtension("nonexistent")
	if err != nil {
		t.Fatalf("GetContactByExtension failed: %v", err)
	}
	if none != nil {
		t.Errorf("got = %+v, want nil per interno inesistente", none)
	}
}

func TestSoftDeleteStalePBXContacts(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-2 * time.Hour)
	fresh := time.Now()

	if err := db.UpsertPBXContact(&Contact{UID: "pbx-300", DisplayName: "Stale", LDAPExt: "300", PrimaryNumber: "300", LastSync: old}); err != nil {
		t.Fatalf("UpsertPBXContact(stale) failed: %v", err)
	}
	if err := db.UpsertPBXContact(&Contact{UID: "pbx-301", DisplayName: "Kept", LDAPExt: "301", PrimaryNumber: "301", LastSync: fresh}); err != nil {
		t.Fatalf("UpsertPBXContact(kept) failed: %v", err)
	}
	// un contatto LDAP vecchio non deve mai essere toccato da questa funzione
	if err := db.UpsertContact(&Contact{UID: "ldap-old", DisplayName: "LDAP Old", LastSync: old}); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	n, err := db.SoftDeleteStalePBXContacts(fresh)
	if err != nil {
		t.Fatalf("SoftDeleteStalePBXContacts failed: %v", err)
	}
	if n != 1 {
		t.Errorf("SoftDeleteStalePBXContacts returned %d, want 1", n)
	}

	if got, _ := db.GetContact("pbx-300"); got != nil {
		t.Error("pbx-300 doveva essere soft-deleted")
	}
	if got, _ := db.GetContact("pbx-301"); got == nil {
		t.Error("pbx-301 doveva restare attivo")
	}
	if got, _ := db.GetContact("ldap-old"); got == nil {
		t.Error("ldap-old non deve essere toccato da SoftDeleteStalePBXContacts")
	}
}
```

- [ ] **Step 2: Esegui i test e verifica che falliscano**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run 'PBX|DomainExtensions|GetContactByExtension' -v"
```
Expected: FAIL — i metodi non esistono ancora (errore di compilazione).

- [ ] **Step 3: Implementa i quattro metodi**

In `internal/database/sqlite.go`, in fondo (dopo `SoftDeleteStale`, prima del commento "Manual contact operations"):

```go
// PBX operations (source='pbx': peers SIP del centralino non presenti nel
// dominio LDAP — vedi internal/pbx per l'orchestrazione del sync)

// ListDomainExtensions returns the ldap_ext of every active source='ldap'
// contact — usato dal sync PBX per escludere i peers già coperti da LDAP.
func (db *DB) ListDomainExtensions() ([]string, error) {
	rows, err := db.Query(`SELECT ldap_ext FROM contacts WHERE source = 'ldap' AND ldap_ext IS NOT NULL AND ldap_ext != ''`)
	if err != nil {
		return nil, fmt.Errorf("failed to list domain extensions: %w", err)
	}
	defer rows.Close()

	var exts []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			return nil, fmt.Errorf("failed to scan extension: %w", err)
		}
		exts = append(exts, ext)
	}
	return exts, rows.Err()
}

// GetContactByExtension returns the first active contact (qualunque
// source) con il ldap_ext dato — usato per risolvere i membri "SIP/xxx"
// di un call group PBX a un contact_id.
func (db *DB) GetContactByExtension(ext string) (*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE ldap_ext = ? AND deleted_at IS NULL
	LIMIT 1
	`
	contact := &Contact{}
	err := db.QueryRow(query, ext).Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
		&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.ManualOverride,
		&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact by extension: %w", err)
	}
	return contact, nil
}

// UpsertPBXContact crea o aggiorna un contatto source='pbx' (peer SIP del
// centralino non presente in dominio). Stesso pattern CASE-gated da
// manual_override di UpsertContact, ma imposta esplicitamente source='pbx'
// alla creazione (mai toccato in seguito, anche in ON CONFLICT).
func (db *DB) UpsertPBXContact(c *Contact) error {
	now := time.Now()
	c.UpdatedAt = now
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}

	query := `
	INSERT INTO contacts (uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, manual_override, deleted_at, last_sync, created_at, updated_at)
	VALUES (?, ?, '', ?, ?, ?, '', '', '', '', ?, 'pbx', ?, NULL, ?, ?, ?)
	ON CONFLICT(uid) DO UPDATE SET
		display_name = CASE WHEN manual_override = 0 THEN excluded.display_name ELSE display_name END,
		ldap_ext = CASE WHEN manual_override = 0 THEN excluded.ldap_ext ELSE ldap_ext END,
		primary_number = CASE WHEN manual_override = 0 THEN excluded.primary_number ELSE primary_number END,
		department = CASE WHEN manual_override = 0 THEN excluded.department ELSE department END,
		deleted_at = NULL,
		last_sync = excluded.last_sync,
		updated_at = excluded.updated_at
	`
	_, err := db.Exec(query, c.UID, c.DisplayName, c.LDAPExt, c.PrimaryNumber, c.Department, c.Area, c.ManualOverride, c.LastSync, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to upsert pbx contact: %w", err)
	}
	return nil
}

// SoftDeleteStalePBXContacts soft-delete i contatti source='pbx' il cui
// last_sync non è stato aggiornato in questo giro (il peer non compare più
// tra quelli del centralino, o è appena entrato in dominio LDAP e quindi
// non è stato riscritto da UpsertPBXContact). Mai tocca source='ldap' o
// source='manual'.
func (db *DB) SoftDeleteStalePBXContacts(syncTime time.Time) (int64, error) {
	result, err := db.Exec(
		`UPDATE contacts SET deleted_at = ?, updated_at = ? WHERE deleted_at IS NULL AND source = 'pbx' AND last_sync < ?`,
		syncTime, syncTime, syncTime,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to soft-delete stale pbx contacts: %w", err)
	}
	return result.RowsAffected()
}
```

- [ ] **Step 4: Esegui i test e verifica che passino**

Run: stesso comando dello Step 2.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): operazioni contatti source=pbx (upsert, lookup per interno, soft-delete stale)"
```

---

## Task 3: Operazioni DB per i call group PBX

**Files:**
- Modify: `internal/database/sqlite.go`
- Modify: `internal/database/sqlite_test.go`

**Interfaces:**
- Consumes: `GroupNumber` (Task 1), `db.DeleteGroup(id int64) error` (esistente), `db.GetGroupMembers` (esistente).
- Produces: `db.GetGroupByNumber(number string) (*GroupNumber, error)`, `db.ListGroupsBySource(source string) ([]*GroupNumber, error)`, `db.UpsertPBXGroup(number, name, description string, nameOverride bool) (*GroupNumber, error)`, `db.ReplaceGroupMembers(groupID int64, contactIDs []int64) error` — usati da `internal/pbx.ApplyCallGroups` (Task 5).

- [ ] **Step 1: Scrivi i test che falliscono**

In fondo a `internal/database/sqlite_test.go`:

```go
func TestUpsertPBXGroupCreatesAndUpdates(t *testing.T) {
	db := newTestDB(t)

	g, err := db.UpsertPBXGroup("500", "Gruppo Test", "", false)
	if err != nil {
		t.Fatalf("UpsertPBXGroup failed: %v", err)
	}
	if g.Source != "pbx" || g.Name != "Gruppo Test" {
		t.Fatalf("g = %+v, unexpected", g)
	}

	g2, err := db.UpsertPBXGroup("500", "Gruppo Rinominato Dal Centralino", "", false)
	if err != nil {
		t.Fatalf("second UpsertPBXGroup failed: %v", err)
	}
	if g2.ID != g.ID {
		t.Errorf("ID cambiato tra due upsert sullo stesso number: %d vs %d", g.ID, g2.ID)
	}
	if g2.Name != "Gruppo Rinominato Dal Centralino" {
		t.Errorf("Name = %q, want aggiornato dal centralino", g2.Name)
	}
}

func TestUpsertPBXGroupRespectsNameOverride(t *testing.T) {
	db := newTestDB(t)
	g, err := db.UpsertPBXGroup("501", "Nome Migrato", "", true)
	if err != nil {
		t.Fatalf("UpsertPBXGroup failed: %v", err)
	}
	if !g.NameOverride {
		t.Fatal("NameOverride dovrebbe essere true come passato alla creazione")
	}

	g2, err := db.UpsertPBXGroup("501", "Nome Nuovo Dal Centralino", "", false)
	if err != nil {
		t.Fatalf("second UpsertPBXGroup failed: %v", err)
	}
	if g2.Name != "Nome Migrato" {
		t.Errorf("Name = %q, want invariato %q (name_override attivo)", g2.Name, "Nome Migrato")
	}
}

func TestGetGroupByNumberNotFound(t *testing.T) {
	db := newTestDB(t)
	got, err := db.GetGroupByNumber("nonexistent")
	if err != nil {
		t.Fatalf("GetGroupByNumber failed: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestListGroupsBySource(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateGroup(&GroupNumber{Number: "1", Name: "Manuale"}); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if _, err := db.UpsertPBXGroup("2", "PBX", "", false); err != nil {
		t.Fatalf("UpsertPBXGroup failed: %v", err)
	}

	pbxGroups, err := db.ListGroupsBySource("pbx")
	if err != nil {
		t.Fatalf("ListGroupsBySource failed: %v", err)
	}
	if len(pbxGroups) != 1 || pbxGroups[0].Number != "2" {
		t.Errorf("pbxGroups = %+v, want solo il gruppo 2", pbxGroups)
	}
}

func TestReplaceGroupMembers(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&Contact{UID: "u1", DisplayName: "U1", LastSync: time.Now()}); err != nil {
		t.Fatalf("UpsertContact(u1) failed: %v", err)
	}
	if err := db.UpsertContact(&Contact{UID: "u2", DisplayName: "U2", LastSync: time.Now()}); err != nil {
		t.Fatalf("UpsertContact(u2) failed: %v", err)
	}
	u1, _ := db.GetContact("u1")
	u2, _ := db.GetContact("u2")

	g := &GroupNumber{Number: "600", Name: "G"}
	if err := db.CreateGroup(g); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if err := db.AddGroupMember(g.ID, u1.ID); err != nil {
		t.Fatalf("AddGroupMember failed: %v", err)
	}

	// ReplaceGroupMembers deve sostituire integralmente: u1 fuori, u2 dentro
	if err := db.ReplaceGroupMembers(g.ID, []int64{u2.ID}); err != nil {
		t.Fatalf("ReplaceGroupMembers failed: %v", err)
	}

	members, err := db.GetGroupMembers(g.ID)
	if err != nil {
		t.Fatalf("GetGroupMembers failed: %v", err)
	}
	if len(members) != 1 || members[0].UID != "u2" {
		t.Errorf("members = %+v, want solo u2", members)
	}
}
```

- [ ] **Step 2: Esegui i test e verifica che falliscano**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run 'PBXGroup|GetGroupByNumber|ListGroupsBySource|ReplaceGroupMembers' -v"
```
Expected: FAIL — i metodi non esistono ancora.

- [ ] **Step 3: Implementa i quattro metodi**

In `internal/database/sqlite.go`, dopo `ListGroups` (Task 1, Step 6):

```go
// GetGroupByNumber returns the group_numbers row for a given interno, o
// nil se non esiste.
func (db *DB) GetGroupByNumber(number string) (*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, created_at, updated_at FROM group_numbers WHERE number = ?`
	group := &GroupNumber{}
	err := db.QueryRow(query, number).Scan(&group.ID, &group.Number, &group.Name, &group.Description,
		&group.Source, &group.NameOverride, &group.CreatedAt, &group.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group by number: %w", err)
	}
	return group, nil
}

// ListGroupsBySource filtra group_numbers per source ("manual" o "pbx").
func (db *DB) ListGroupsBySource(source string) ([]*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, created_at, updated_at FROM group_numbers WHERE source = ? ORDER BY number`
	rows, err := db.Query(query, source)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups by source: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		group := &GroupNumber{}
		if err := rows.Scan(&group.ID, &group.Number, &group.Name, &group.Description,
			&group.Source, &group.NameOverride, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// UpsertPBXGroup crea o aggiorna una riga group_numbers con source='pbx'.
// Il nome è sovrascritto dal centralino solo se name_override=0 sulla riga
// già esistente (il valore di name_override passato qui conta solo alla
// PRIMA creazione — vedi il caso di migrazione da gruppo manuale in
// internal/pbx.ApplyCallGroups). Presuppone che un'eventuale riga
// preesistente con lo stesso number sia già source='pbx' (il conflitto con
// un gruppo manuale va risolto dal chiamante PRIMA di invocare questo
// metodo, cancellando la riga manuale).
func (db *DB) UpsertPBXGroup(number, name, description string, nameOverride bool) (*GroupNumber, error) {
	now := time.Now()
	query := `
	INSERT INTO group_numbers (number, name, description, source, name_override, created_at, updated_at)
	VALUES (?, ?, ?, 'pbx', ?, ?, ?)
	ON CONFLICT(number) DO UPDATE SET
		name = CASE WHEN name_override = 0 THEN excluded.name ELSE name END,
		description = excluded.description,
		source = 'pbx',
		updated_at = excluded.updated_at
	`
	if _, err := db.Exec(query, number, name, description, nameOverride, now, now); err != nil {
		return nil, fmt.Errorf("failed to upsert pbx group: %w", err)
	}
	return db.GetGroupByNumber(number)
}

// ReplaceGroupMembers sostituisce integralmente i membri di un gruppo in
// una transazione: usato dal sync PBX, per cui il mapping peers<->gruppo è
// fonte di verità assoluta lato centralino (mai un merge additivo).
func (db *DB) ReplaceGroupMembers(groupID int64, contactIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_id = ?`, groupID); err != nil {
		return fmt.Errorf("failed to clear group members: %w", err)
	}
	now := time.Now()
	for _, cid := range contactIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO group_members (group_id, contact_id, created_at) VALUES (?, ?, ?)`, groupID, cid, now); err != nil {
			return fmt.Errorf("failed to insert group member: %w", err)
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Esegui i test e verifica che passino**

Run: stesso comando dello Step 2.
Expected: PASS.

- [ ] **Step 5: Esegui l'intera suite database**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -v"
```
Expected: PASS su tutti i test del pacchetto.

- [ ] **Step 6: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): operazioni call group PBX (upsert per interno, sostituzione membri)"
```

---

## Task 4: `internal/pbx` — client HTTP e parsing

**Files:**
- Create: `internal/pbx/client.go`
- Create: `internal/pbx/client_test.go`
- Create: `internal/pbx/testdata/peers.html`
- Create: `internal/pbx/testdata/callgroups.html`

**Interfaces:**
- Produces: `pbx.Peer{Extension, CallerID, Status string}`, `pbx.CallGroup{Name, Extension string, Members []string, Strategy, Timeout string, Enabled bool}`, `pbx.NewClient(baseURL string) *Client`, `(*Client).Login(user, pass string) error`, `(*Client).FetchPeers() ([]Peer, error)`, `(*Client).FetchCallGroups() ([]CallGroup, error)`, `pbx.ParsePeers(html string) ([]Peer, error)`, `pbx.ParseCallGroups(html string) []CallGroup` — usati da `internal/pbx/sync.go` (Task 5) e da `cmd/pbxpoc` (Task 8).

- [ ] **Step 1: Crea le fixture HTML (dati sintetici, nessun dato reale)**

`internal/pbx/testdata/peers.html`:

```html
<html><body>
<script type="text/javascript">
  var infopeers = [{"defaultuser":"100","callerid":"MARIO ROSSI","status":""},{"defaultuser":"200","callerid":"GRUPPO TEST","status":"1"}];
</script>
</body></html>
```

`internal/pbx/testdata/callgroups.html`:

```html
<html><body>
<table>
<tr class="vivo_rowColor1" onMouseOver="selectionTableRow(this,'pointer','#BACEDF','','bold');" onMouseOut="selectionTableRow(this,'','','','');">
  <td align="center"><input type="checkbox" id="cgid[]" name="cgid[]" value="17"></td>
  <td onclick="editCallGroup('17')">Ufficio Test</td>
  <td onclick="editCallGroup('17')">495</td>
  <td onclick="editCallGroup('17')">SIP/740<br />SIP/741</td>
  <td onclick="editCallGroup('17')">ringall</td>
  <td onclick="editCallGroup('17')">20</td>
  <td align="center" onclick="editCallGroup('17')"><img src="https://10.0.90.253/vivo_images/on.gif" /></td>
</tr>
<tr class="vivo_rowColor2" onMouseOver="selectionTableRow(this,'pointer','#BACEDF','','bold');" onMouseOut="selectionTableRow(this,'','','','');">
  <td align="center"><input type="checkbox" id="cgid[]" name="cgid[]" value="18"></td>
  <td onclick="editCallGroup('18')">Ufficio Disattivo</td>
  <td onclick="editCallGroup('18')">496</td>
  <td onclick="editCallGroup('18')">SIP/742</td>
  <td onclick="editCallGroup('18')">ringall</td>
  <td onclick="editCallGroup('18')">30</td>
  <td align="center" onclick="editCallGroup('18')"><img src="https://10.0.90.253/vivo_images/off.gif" /></td>
</tr>
</table>
</body></html>
```

- [ ] **Step 2: Scrivi i test che falliscono**

`internal/pbx/client_test.go`:

```go
package pbx

import (
	"os"
	"testing"
)

func TestParsePeers(t *testing.T) {
	html, err := os.ReadFile("testdata/peers.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	peers, err := ParsePeers(string(html))
	if err != nil {
		t.Fatalf("ParsePeers failed: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	if peers[0].Extension != "100" || peers[0].CallerID != "MARIO ROSSI" {
		t.Errorf("peers[0] = %+v, unexpected", peers[0])
	}
	if peers[1].Extension != "200" || peers[1].Status != "1" {
		t.Errorf("peers[1] = %+v, unexpected", peers[1])
	}
}

func TestParsePeersMissingVar(t *testing.T) {
	_, err := ParsePeers("<html><body>nothing here</body></html>")
	if err == nil {
		t.Fatal("expected error when infopeers var is missing")
	}
}

func TestParseCallGroups(t *testing.T) {
	html, err := os.ReadFile("testdata/callgroups.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	groups := ParseCallGroups(string(html))
	if len(groups) != 2 {
		t.Fatalf("got %d call groups, want 2", len(groups))
	}

	g0 := groups[0]
	if g0.Name != "Ufficio Test" || g0.Extension != "495" {
		t.Errorf("groups[0] = %+v, unexpected", g0)
	}
	if len(g0.Members) != 2 || g0.Members[0] != "740" || g0.Members[1] != "741" {
		t.Errorf("groups[0].Members = %v, want [740 741]", g0.Members)
	}
	if !g0.Enabled {
		t.Error("groups[0].Enabled = false, want true")
	}

	g1 := groups[1]
	if g1.Enabled {
		t.Error("groups[1].Enabled = true, want false (off.gif)")
	}
	if len(g1.Members) != 1 || g1.Members[0] != "742" {
		t.Errorf("groups[1].Members = %v, want [742]", g1.Members)
	}
}
```

- [ ] **Step 3: Esegui i test e verifica che falliscano**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/pbx/... -v"
```
Expected: FAIL — package `pbx` non esiste ancora.

- [ ] **Step 4: Implementa `client.go`**

`internal/pbx/client.go`:

```go
// Package pbx implementa lo screen-scraping della web console del
// centralino Invidea "ViVo" (nessuna API ufficiale) per estrarre peers SIP
// e call group non presenti nel dominio LDAP. Vedi
// docs/superpowers/specs/2026-09-13-pbx-scraping-design.md per il design
// completo, gli endpoint usati e le regole di merge/override.
package pbx

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// Peer è un interno SIP come esposto da
// vivo.index.php?module=monitor&mode=peers (var JS "infopeers").
type Peer struct {
	Extension string `json:"defaultuser"`
	CallerID  string `json:"callerid"`
	Status    string `json:"status"`
}

// CallGroup è una riga della tabella
// vivo.index.php?module=extensions&mode=callgroups.
type CallGroup struct {
	Name      string
	Extension string
	Members   []string // interni destinatari, estratti da "SIP/xxx"
	Strategy  string
	Timeout   string
	Enabled   bool
}

// Client parla con la web console del centralino su una sessione autenticata.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient crea un client per il centralino a baseURL (es.
// "https://10.0.90.253"). Il certificato TLS è tipicamente self-signed
// (dispositivo su IP privato) — la verifica è disabilitata di proposito.
func NewClient(baseURL string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Jar: jar,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// Login autentica la sessione. Va chiamato prima di FetchPeers/FetchCallGroups.
func (c *Client) Login(user, pass string) error {
	form := url.Values{
		"redirect2": {"5"},
		"username":  {user},
		"password":  {pass},
		"Submit3":   {"Accedi"},
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/verifica__login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `window.open("header.php"`) {
		return fmt.Errorf("login fallito (status %d) — credenziali errate?", resp.StatusCode)
	}
	return nil
}

func (c *Client) get(path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", c.baseURL+"/vivo.php")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s -> HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

var infopeersRe = regexp.MustCompile(`var infopeers\s*=\s*(\[.*?\]);`)

// FetchPeers scarica e parsa lo stato dei peers SIP.
func (c *Client) FetchPeers() ([]Peer, error) {
	html, err := c.get("vivo.index.php?module=monitor&mode=peers")
	if err != nil {
		return nil, err
	}
	return ParsePeers(html)
}

// ParsePeers estrae l'array JSON dalla var JS "infopeers" incorporata
// nella pagina. Esportata per i test (fixture HTML statiche).
func ParsePeers(html string) ([]Peer, error) {
	m := infopeersRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("var infopeers non trovata nella risposta (sessione scaduta o pagina cambiata?)")
	}
	var peers []Peer
	if err := json.Unmarshal([]byte(m[1]), &peers); err != nil {
		return nil, fmt.Errorf("parse infopeers: %w", err)
	}
	return peers, nil
}

// FetchCallGroups scarica e parsa la tabella dei call group.
func (c *Client) FetchCallGroups() ([]CallGroup, error) {
	html, err := c.get("vivo.index.php?module=extensions&mode=callgroups")
	if err != nil {
		return nil, err
	}
	return ParseCallGroups(html), nil
}

// Regex sulla struttura HTML della tabella callgroups. Fragile per natura
// (screen-scraping): se il centralino cambia versione/markup va rivista.
var (
	rowRe     = regexp.MustCompile(`(?s)<tr class="vivo_rowColor\d".*?</tr>`)
	cgidRe    = regexp.MustCompile(`name="cgid\[\]"\s*\n?\s*value="(\d+)"`)
	cellsRe   = regexp.MustCompile(`editCallGroup\('\d+'\)">([^<]*)</td>`)
	sipRe     = regexp.MustCompile(`SIP/(\d+)`)
	enabledRe = regexp.MustCompile(`/(on|off)\.gif`)
)

// ParseCallGroups estrae la tabella dei call group dall'HTML della pagina.
// Righe non riconosciute (niente checkbox cgid, meno di 5 colonne testuali)
// vengono ignorate silenziosamente. Esportata per i test.
func ParseCallGroups(html string) []CallGroup {
	var groups []CallGroup
	for _, row := range rowRe.FindAllString(html, -1) {
		if cgidRe.FindStringSubmatch(row) == nil {
			continue
		}
		cells := cellsRe.FindAllStringSubmatch(row, -1)
		// colonne attese dopo la checkbox: nome, interno, peers, strategy, timeout
		if len(cells) < 5 {
			continue
		}
		var members []string
		for _, sm := range sipRe.FindAllStringSubmatch(cells[2][1], -1) {
			members = append(members, sm[1])
		}
		enabled := false
		if em := enabledRe.FindStringSubmatch(row); em != nil {
			enabled = em[1] == "on"
		}
		groups = append(groups, CallGroup{
			Name:      strings.TrimSpace(cells[0][1]),
			Extension: strings.TrimSpace(cells[1][1]),
			Members:   members,
			Strategy:  strings.TrimSpace(cells[3][1]),
			Timeout:   strings.TrimSpace(cells[4][1]),
			Enabled:   enabled,
		})
	}
	return groups
}
```

- [ ] **Step 5: Esegui i test e verifica che passino**

Run: stesso comando dello Step 3.
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/pbx/client.go internal/pbx/client_test.go internal/pbx/testdata
git commit -m "feat(pbx): client HTTP + parsing peers/call group centralino"
```

---

## Task 5: `internal/pbx` — orchestrazione sync

**Files:**
- Create: `internal/pbx/sync.go`
- Create: `internal/pbx/sync_test.go`

**Interfaces:**
- Consumes: `Peer`, `CallGroup` (Task 4); `database.DB` con i metodi dei Task 1-3.
- Produces: `pbx.PBXURLConfigKey/PBXUserConfigKey/PBXPassConfigKey` (chiavi `app_config`), `pbx.LoadPBXConfig(db *database.DB) (url, user, pass string)`, `pbx.Filters{ExcludeUnnamed, ExcludeInactiveGroups, ExcludeEmptyGroups bool}`, `pbx.LoadFilters(db *database.DB) Filters`, `pbx.SaveFilters(db *database.DB, f Filters) error`, `pbx.FilterPeers(peers []Peer, f Filters) []Peer`, `pbx.FilterCallGroups(groups []CallGroup, f Filters) []CallGroup`, `pbx.SyncPBX(db *database.DB) error`, `pbx.ApplyPeers(db *database.DB, peers []Peer, syncTime time.Time) (int, error)`, `pbx.ApplyCallGroups(db *database.DB, groups []CallGroup) error` — usati da `cmd/server/main.go` (Task 6, sia per il sync automatico che per la pagina `/admin/pbx`, Task 7).

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/pbx/sync_test.go`:

```go
package pbx

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

func newTestDB(t *testing.T) *database.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestApplyPeersExcludesDomainExtensions(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&database.Contact{UID: "mario.rossi", DisplayName: "Mario Rossi", LDAPExt: "100", LastSync: time.Now()}); err != nil {
		t.Fatalf("seed ldap contact: %v", err)
	}

	peers := []Peer{
		{Extension: "100", CallerID: "MARIO ROSSI (PBX)"},
		{Extension: "200", CallerID: "GRUPPO TEST"},
	}
	applied, err := ApplyPeers(db, peers, time.Now())
	if err != nil {
		t.Fatalf("ApplyPeers failed: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1 (solo il peer non in dominio)", applied)
	}

	if got, _ := db.GetContact("pbx-100"); got != nil {
		t.Error("peer 100 è già in dominio, non deve diventare un contatto pbx")
	}
	got, err := db.GetContact("pbx-200")
	if err != nil || got == nil {
		t.Fatalf("peer 200 doveva diventare un contatto pbx: %v", err)
	}
	if got.DisplayName != "GRUPPO TEST" || got.Source != "pbx" {
		t.Errorf("contatto pbx-200 = %+v, unexpected", got)
	}
}

func TestApplyPeersSoftDeletesDisappearedPeers(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-2 * time.Hour)
	if _, err := ApplyPeers(db, []Peer{{Extension: "300", CallerID: "SPARISCE"}}, old); err != nil {
		t.Fatalf("first ApplyPeers failed: %v", err)
	}
	if got, _ := db.GetContact("pbx-300"); got == nil {
		t.Fatal("pbx-300 dovrebbe esistere dopo il primo giro")
	}

	if _, err := ApplyPeers(db, []Peer{}, time.Now()); err != nil {
		t.Fatalf("second ApplyPeers failed: %v", err)
	}
	if got, _ := db.GetContact("pbx-300"); got != nil {
		t.Error("pbx-300 doveva essere soft-deleted, non compare più tra i peer del centralino")
	}
}

func TestApplyCallGroupsCreatesAndLinksMembers(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&database.Contact{UID: "u1", DisplayName: "U1", LDAPExt: "700", LastSync: time.Now()}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := ApplyPeers(db, []Peer{{Extension: "701", CallerID: "PEER 701"}}, time.Now()); err != nil {
		t.Fatalf("seed peer: %v", err)
	}

	groups := []CallGroup{
		{Name: "Gruppo Test", Extension: "500", Members: []string{"700", "701"}, Strategy: "ringall", Timeout: "20", Enabled: true},
	}
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("ApplyCallGroups failed: %v", err)
	}

	group, err := db.GetGroupByNumber("500")
	if err != nil || group == nil {
		t.Fatalf("gruppo 500 non trovato: %v", err)
	}
	if group.Name != "Gruppo Test" || group.Source != "pbx" {
		t.Errorf("group = %+v, unexpected", group)
	}

	members, err := db.GetGroupMembers(group.ID)
	if err != nil {
		t.Fatalf("GetGroupMembers failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
}

func TestApplyCallGroupsMigratesManualNameAsOverride(t *testing.T) {
	db := newTestDB(t)
	manual := &database.GroupNumber{Number: "9", Name: "Centralino Comunale"}
	if err := db.CreateGroup(manual); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	groups := []CallGroup{
		{Name: "Centralino", Extension: "9", Members: []string{}, Strategy: "ringall", Timeout: "20", Enabled: true},
	}
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("ApplyCallGroups failed: %v", err)
	}

	got, err := db.GetGroupByNumber("9")
	if err != nil || got == nil {
		t.Fatalf("gruppo 9 non trovato dopo la migrazione: %v", err)
	}
	if got.Source != "pbx" {
		t.Errorf("Source = %q, want pbx", got.Source)
	}
	if got.Name != "Centralino Comunale" {
		t.Errorf("Name = %q, want il nome manuale migrato %q", got.Name, "Centralino Comunale")
	}
	if !got.NameOverride {
		t.Error("NameOverride dovrebbe essere true dopo la migrazione")
	}

	groups[0].Name = "Centralino V2"
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("second ApplyCallGroups failed: %v", err)
	}
	got2, _ := db.GetGroupByNumber("9")
	if got2.Name != "Centralino Comunale" {
		t.Errorf("Name = %q dopo secondo giro, want invariato %q", got2.Name, "Centralino Comunale")
	}
}

func TestLoadPBXConfigEmptyWhenNotSet(t *testing.T) {
	db := newTestDB(t)

	url, user, pass := LoadPBXConfig(db)
	if url != "" || user != "" || pass != "" {
		t.Errorf("LoadPBXConfig senza config salvata = (%q,%q,%q), want tutto vuoto", url, user, pass)
	}
}

func TestLoadPBXConfigReadsFromDB(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetConfig(PBXURLConfigKey, "https://10.0.90.253"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if err := db.SetConfig(PBXUserConfigKey, "admin"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if err := db.SetConfig(PBXPassConfigKey, "secret"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}

	url, user, pass := LoadPBXConfig(db)
	if url != "https://10.0.90.253" || user != "admin" || pass != "secret" {
		t.Errorf("LoadPBXConfig = (%q,%q,%q), want i valori salvati", url, user, pass)
	}
}

func TestFilterPeersExcludesPlaceholderNames(t *testing.T) {
	peers := []Peer{
		{Extension: "521", CallerID: " <521>"},
		{Extension: "100", CallerID: "MARIO ROSSI"},
		{Extension: "534", CallerID: "<534>"},
	}
	got := FilterPeers(peers, Filters{ExcludeUnnamed: true})
	if len(got) != 1 || got[0].Extension != "100" {
		t.Errorf("FilterPeers(ExcludeUnnamed=true) = %+v, want solo il peer 100", got)
	}

	got2 := FilterPeers(peers, Filters{ExcludeUnnamed: false})
	if len(got2) != 3 {
		t.Errorf("FilterPeers(ExcludeUnnamed=false) = %d peers, want 3 (nessun filtro)", len(got2))
	}
}

func TestFilterCallGroupsExcludesInactiveAndEmpty(t *testing.T) {
	groups := []CallGroup{
		{Name: "Attivo con membri", Extension: "1", Members: []string{"700"}, Enabled: true},
		{Name: "Disattivo", Extension: "2", Members: []string{"700"}, Enabled: false},
		{Name: "Senza membri", Extension: "3", Members: []string{}, Enabled: true},
	}

	got := FilterCallGroups(groups, Filters{ExcludeInactiveGroups: true, ExcludeEmptyGroups: true})
	if len(got) != 1 || got[0].Extension != "1" {
		t.Errorf("FilterCallGroups(entrambi i filtri) = %+v, want solo il gruppo 1", got)
	}

	got2 := FilterCallGroups(groups, Filters{})
	if len(got2) != 3 {
		t.Errorf("FilterCallGroups(nessun filtro) = %d gruppi, want 3", len(got2))
	}
}

func TestLoadFiltersDefaultsToTrue(t *testing.T) {
	db := newTestDB(t)
	f := LoadFilters(db)
	if !f.ExcludeUnnamed || !f.ExcludeInactiveGroups || !f.ExcludeEmptyGroups {
		t.Errorf("LoadFilters senza config salvata = %+v, want tutto true di default", f)
	}
}

func TestSaveAndLoadFilters(t *testing.T) {
	db := newTestDB(t)
	want := Filters{ExcludeUnnamed: false, ExcludeInactiveGroups: true, ExcludeEmptyGroups: false}
	if err := SaveFilters(db, want); err != nil {
		t.Fatalf("SaveFilters failed: %v", err)
	}

	got := LoadFilters(db)
	if got != want {
		t.Errorf("LoadFilters dopo SaveFilters = %+v, want %+v", got, want)
	}
}

func TestApplyCallGroupsDeletesDisappearedGroups(t *testing.T) {
	db := newTestDB(t)
	if err := ApplyCallGroups(db, []CallGroup{{Name: "Sparisce", Extension: "111", Members: []string{}}}); err != nil {
		t.Fatalf("first ApplyCallGroups failed: %v", err)
	}
	if got, _ := db.GetGroupByNumber("111"); got == nil {
		t.Fatal("gruppo 111 dovrebbe esistere dopo il primo giro")
	}

	if err := ApplyCallGroups(db, []CallGroup{}); err != nil {
		t.Fatalf("second ApplyCallGroups failed: %v", err)
	}
	if got, _ := db.GetGroupByNumber("111"); got != nil {
		t.Error("gruppo 111 doveva essere cancellato, non compare più tra i call group del centralino")
	}
}
```

- [ ] **Step 2: Esegui i test e verifica che falliscano**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/pbx/... -run 'ApplyPeers|ApplyCallGroups|LoadPBXConfig|Filter' -v"
```
Expected: FAIL — `ApplyPeers`/`ApplyCallGroups`/`SyncPBX`/`LoadPBXConfig`/`Filters`/`LoadFilters`/`SaveFilters`/`FilterPeers`/`FilterCallGroups` non esistono ancora.

- [ ] **Step 3: Implementa `sync.go`**

`internal/pbx/sync.go`:

```go
package pbx

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

// Chiavi app_config per la configurazione PBX, editabile da /admin/pbx.
// Nessuna env var: questa è l'unica fonte di verità.
const (
	PBXURLConfigKey  = "pbx_url"
	PBXUserConfigKey = "pbx_user"
	PBXPassConfigKey = "pbx_pass"

	filterExcludeUnnamedConfigKey        = "pbx_exclude_unnamed"
	filterExcludeInactiveGroupsConfigKey = "pbx_exclude_inactive_groups"
	filterExcludeEmptyGroupsConfigKey    = "pbx_exclude_empty_groups"
)

// LoadPBXConfig legge url/utente/password da app_config (stringa vuota se
// non ancora configurato). Usata sia dal sync automatico che dalla pagina
// admin (per precompilare url/utente, mai la password — vedi Task 7).
func LoadPBXConfig(db *database.DB) (url, user, pass string) {
	url, _ = db.GetConfig(PBXURLConfigKey)
	user, _ = db.GetConfig(PBXUserConfigKey)
	pass, _ = db.GetConfig(PBXPassConfigKey)
	return url, user, pass
}

// Filters sono i filtri di esclusione configurabili da /admin/pbx: dati
// "spazzatura" del centralino che, di default, non vogliamo veder comparire
// in rubrica. Tutti true di default (vedi LoadFilters) — un admin può
// disattivarli singolarmente se preferisce vedere anche quei dati.
type Filters struct {
	ExcludeUnnamed        bool // contatti con nome placeholder tipo "<534>"
	ExcludeInactiveGroups bool // call group disattivi sul centralino
	ExcludeEmptyGroups    bool // call group senza destinatari
}

func boolConfig(db *database.DB, key string, fallback bool) bool {
	v, _ := db.GetConfig(key)
	if v == "" {
		return fallback
	}
	return v == "true"
}

func setBoolConfig(db *database.DB, key string, value bool) error {
	v := "false"
	if value {
		v = "true"
	}
	return db.SetConfig(key, v)
}

// LoadFilters legge i filtri da app_config. Ogni filtro non ancora
// impostato torna true (default "prudente": nascondi la spazzatura finché
// l'admin non chiede esplicitamente di vederla).
func LoadFilters(db *database.DB) Filters {
	return Filters{
		ExcludeUnnamed:        boolConfig(db, filterExcludeUnnamedConfigKey, true),
		ExcludeInactiveGroups: boolConfig(db, filterExcludeInactiveGroupsConfigKey, true),
		ExcludeEmptyGroups:    boolConfig(db, filterExcludeEmptyGroupsConfigKey, true),
	}
}

// SaveFilters salva i tre filtri in app_config.
func SaveFilters(db *database.DB, f Filters) error {
	if err := setBoolConfig(db, filterExcludeUnnamedConfigKey, f.ExcludeUnnamed); err != nil {
		return err
	}
	if err := setBoolConfig(db, filterExcludeInactiveGroupsConfigKey, f.ExcludeInactiveGroups); err != nil {
		return err
	}
	return setBoolConfig(db, filterExcludeEmptyGroupsConfigKey, f.ExcludeEmptyGroups)
}

// placeholderNameRe riconosce i nomi "segnaposto" che il centralino usa per
// gli interni senza un nome configurato, es. " <521>" o "<534>".
var placeholderNameRe = regexp.MustCompile(`^<\d+>$`)

func isPlaceholderName(callerID string) bool {
	return placeholderNameRe.MatchString(strings.TrimSpace(callerID))
}

// FilterPeers applica Filters.ExcludeUnnamed alla lista di peer.
func FilterPeers(peers []Peer, f Filters) []Peer {
	if !f.ExcludeUnnamed {
		return peers
	}
	var out []Peer
	for _, p := range peers {
		if isPlaceholderName(p.CallerID) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FilterCallGroups applica Filters.ExcludeInactiveGroups/ExcludeEmptyGroups
// alla lista di call group.
func FilterCallGroups(groups []CallGroup, f Filters) []CallGroup {
	var out []CallGroup
	for _, g := range groups {
		if f.ExcludeInactiveGroups && !g.Enabled {
			continue
		}
		if f.ExcludeEmptyGroups && len(g.Members) == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}

// SyncPBX esegue un giro completo di sync (login, fetch, filtra, applica).
// No-op silenzioso se l'URL non è ancora configurato da /admin/pbx —
// subsystem disattivo. Un errore di rete/login/parsing salta l'intero giro
// senza toccare i dati esistenti.
func SyncPBX(db *database.DB) error {
	url, user, pass := LoadPBXConfig(db)
	if url == "" {
		return nil
	}

	log.Printf("[PBX] Starting PBX sync...")

	client := NewClient(url)
	if err := client.Login(user, pass); err != nil {
		return fmt.Errorf("pbx login failed: %w", err)
	}

	peers, err := client.FetchPeers()
	if err != nil {
		return fmt.Errorf("pbx fetch peers failed: %w", err)
	}

	groups, err := client.FetchCallGroups()
	if err != nil {
		return fmt.Errorf("pbx fetch call groups failed: %w", err)
	}

	filters := LoadFilters(db)
	peers = FilterPeers(peers, filters)
	groups = FilterCallGroups(groups, filters)

	applied, err := ApplyPeers(db, peers, time.Now())
	if err != nil {
		return fmt.Errorf("pbx apply peers failed: %w", err)
	}

	if err := ApplyCallGroups(db, groups); err != nil {
		return fmt.Errorf("pbx apply call groups failed: %w", err)
	}

	log.Printf("[PBX] Sync completed: %d peers applied, %d call groups", applied, len(groups))
	return nil
}

// ApplyPeers upserta come contacts source='pbx' i peer non già coperti da
// un contatto source='ldap' (stesso ldap_ext), e soft-delete quelli
// scomparsi dal centralino in questo giro. Esportata per i test.
func ApplyPeers(db *database.DB, peers []Peer, syncTime time.Time) (int, error) {
	domainExts, err := db.ListDomainExtensions()
	if err != nil {
		return 0, err
	}
	inDomain := make(map[string]struct{}, len(domainExts))
	for _, e := range domainExts {
		inDomain[e] = struct{}{}
	}

	applied := 0
	for _, p := range peers {
		if _, ok := inDomain[p.Extension]; ok {
			continue
		}
		c := &database.Contact{
			UID:           "pbx-" + p.Extension,
			DisplayName:   p.CallerID,
			LDAPExt:       p.Extension,
			PrimaryNumber: p.Extension,
			Department:    "Centralino - non mappato",
			LastSync:      syncTime,
		}
		if err := db.UpsertPBXContact(c); err != nil {
			log.Printf("[PBX] skip peer %s: %v", p.Extension, err)
			continue
		}
		applied++
	}

	if _, err := db.SoftDeleteStalePBXContacts(syncTime); err != nil {
		return applied, err
	}
	return applied, nil
}

// ApplyCallGroups upserta i call group come group_numbers source='pbx' e
// ne sovrascrive sempre i membri (fonte di verità assoluta lato PBX). Un
// gruppo manuale preesistente con lo stesso interno viene sostituito,
// migrandone il nome come override se diverso da quello del centralino.
// Esportata per i test.
func ApplyCallGroups(db *database.DB, groups []CallGroup) error {
	seen := make(map[string]struct{}, len(groups))

	for _, g := range groups {
		seen[g.Extension] = struct{}{}

		name := g.Name
		nameOverride := false

		existing, err := db.GetGroupByNumber(g.Extension)
		if err != nil {
			return err
		}
		if existing != nil && existing.Source == "manual" {
			if existing.Name != g.Name {
				name = existing.Name
				nameOverride = true
			}
			if err := db.DeleteGroup(existing.ID); err != nil {
				return err
			}
		}

		row, err := db.UpsertPBXGroup(g.Extension, name, "", nameOverride)
		if err != nil {
			log.Printf("[PBX] skip call group %s: %v", g.Extension, err)
			continue
		}

		var memberIDs []int64
		for _, ext := range g.Members {
			contact, err := db.GetContactByExtension(ext)
			if err != nil {
				return err
			}
			if contact == nil {
				log.Printf("[PBX] call group %s: interno membro %s non trovato tra i contatti, ignorato", g.Extension, ext)
				continue
			}
			memberIDs = append(memberIDs, contact.ID)
		}
		if err := db.ReplaceGroupMembers(row.ID, memberIDs); err != nil {
			return err
		}
	}

	stale, err := db.ListGroupsBySource("pbx")
	if err != nil {
		return err
	}
	for _, g := range stale {
		if _, ok := seen[g.Number]; ok {
			continue
		}
		if err := db.DeleteGroup(g.ID); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Esegui i test e verifica che passino**

Run: stesso comando dello Step 2.
Expected: PASS.

- [ ] **Step 5: Esegui l'intera suite del progetto**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go vet ./... && CGO_ENABLED=1 go test ./... -v"
```
Expected: PASS su tutti i pacchetti, nessun warning `go vet`.

- [ ] **Step 6: Commit**

```bash
git add internal/pbx/sync.go internal/pbx/sync_test.go
git commit -m "feat(pbx): orchestrazione sync (peers + call group, override, stale cleanup)"
```

---

## Task 6: Wiring sync automatico in `cmd/server/main.go`

Solo il giro automatico (startup + ticker orario). Il sync manuale ha un
pulsante dedicato sulla nuova pagina `/admin/pbx` (Task 7), non condivide
più il pulsante "Sync Now" di LDAP.

**Files:**
- Modify: `cmd/server/main.go`

**Interfaces:**
- Consumes: `pbx.SyncPBX(db) error` (Task 5).
- Produces: package var `lastPBXSync time.Time` — usato dalla pagina `/admin/pbx` (Task 7).

- [ ] **Step 1: Aggiungi l'import**

In `cmd/server/main.go`, nel blocco import (dopo `"github.com/mirkochipdotcom/ldavsync/internal/ldap"`):

```go
	"github.com/mirkochipdotcom/ldavsync/internal/ldap"
	"github.com/mirkochipdotcom/ldavsync/internal/pbx"
	"github.com/mirkochipdotcom/ldavsync/internal/phonebook"
```

- [ ] **Step 2: Aggiungi la var di stato accanto a `lastSync`**

Cerca la dichiarazione di `lastSync` nel blocco `var (...)` in cima al file e aggiungi accanto:

```go
	lastSync    time.Time
	lastPBXSync time.Time
```

- [ ] **Step 3: Aggiungi la chiamata nel sync iniziale allo startup**

Nel blocco `// Perform initial sync` (circa riga 123-130):

```go
	// Perform initial sync
	go func() {
		if err := ldap.SyncContacts(db, cfg); err != nil {
			log.Printf("[SYNC] Initial sync failed: %v", err)
		} else {
			lastSync = time.Now()
		}
		if err := pbx.SyncPBX(db); err != nil {
			log.Printf("[PBX] Initial sync failed: %v", err)
		} else {
			lastPBXSync = time.Now()
		}
	}()
```

- [ ] **Step 4: Aggiungi la chiamata nel ticker orario**

In `ldapSyncWorker()` (circa riga 195-207):

```go
func ldapSyncWorker() {
	ticker := time.NewTicker(time.Duration(cfg.SyncIntervalHours) * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("[SYNC] Starting scheduled sync...")
		if err := ldap.SyncContacts(db, cfg); err != nil {
			log.Printf("[SYNC] Failed: %v", err)
		} else {
			lastSync = time.Now()
		}
		if err := pbx.SyncPBX(db); err != nil {
			log.Printf("[PBX] Failed: %v", err)
		} else {
			lastPBXSync = time.Now()
		}
	}
}
```

- [ ] **Step 5: Verifica che l'intero progetto compili**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go build ./..."
```
Expected: build ok (`lastPBXSync` non ancora letto da nessuno finché non c'è la pagina admin del Task 7 — nessun errore "declared and not used" perché è una var di package, non locale).

- [ ] **Step 6: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat: wiring sync PBX automatico nel giro orario e allo startup"
```

---

## Task 7: Pagina admin `/admin/pbx` (config + sync manuale)

**Files:**
- Create: `web/templates/admin_page_pbx.html`
- Create: `web/templates/admin_pbx.html`
- Modify: `web/templates/rail.html`
- Modify: `cmd/server/main.go`

**Interfaces:**
- Consumes: `pbx.PBXURLConfigKey/PBXUserConfigKey/PBXPassConfigKey`, `pbx.LoadPBXConfig`, `pbx.SyncPBX` (Task 5); `db.SetConfig`/`db.GetConfig` (esistenti); `railData()` (esistente).

- [ ] **Step 1: Aggiungi il fragment del form**

`web/templates/admin_pbx.html` (fragment, target dello swap htmx — mostra form config + stato ultimo sync):

```html
<div class="admin-card">
    <h2>Configurazione centralino</h2>
    <form hx-post="/admin/pbx" hx-target="#pbx-content" hx-swap="innerHTML" class="field-row" style="flex-direction:column;align-items:stretch;gap:10px;">
        <label>
            URL centralino
            <input type="text" name="pbx_url" value="{{.PBXURL}}" placeholder="https://10.0.90.253" class="input">
        </label>
        <label>
            Utente
            <input type="text" name="pbx_user" value="{{.PBXUser}}" class="input">
        </label>
        <label>
            Password
            <input type="password" name="pbx_pass" value="" placeholder="{{if .PBXHasPassword}}(lasciare vuoto per non modificare){{else}}nessuna password salvata{{end}}" class="input">
        </label>
        <button type="submit" class="btn btn-primary" style="flex:none;">{{index .Messages "save"}}</button>
    </form>
</div>

<div class="admin-card" style="margin-top:16px;">
    <h2>Filtri di esclusione</h2>
    <form hx-post="/admin/pbx" hx-target="#pbx-content" hx-swap="innerHTML" style="display:flex;flex-direction:column;gap:8px;">
        <input type="hidden" name="pbx_url" value="{{.PBXURL}}">
        <input type="hidden" name="pbx_user" value="{{.PBXUser}}">
        <label style="display:flex;align-items:center;gap:8px;">
            <input type="checkbox" name="exclude_unnamed" {{if .PBXExcludeUnnamed}}checked{{end}}>
            Escludi contatti senza nome (es. interno mostrato come "&lt;534&gt;")
        </label>
        <label style="display:flex;align-items:center;gap:8px;">
            <input type="checkbox" name="exclude_inactive_groups" {{if .PBXExcludeInactiveGroups}}checked{{end}}>
            Escludi gruppi di chiamata disattivi sul centralino
        </label>
        <label style="display:flex;align-items:center;gap:8px;">
            <input type="checkbox" name="exclude_empty_groups" {{if .PBXExcludeEmptyGroups}}checked{{end}}>
            Escludi gruppi di chiamata senza destinatari
        </label>
        <button type="submit" class="btn btn-ghost" style="flex:none;align-self:flex-start;">{{index .Messages "save"}}</button>
    </form>
</div>

<div class="admin-card" style="margin-top:16px;">
    <h2>Sincronizzazione</h2>
    <p>Ultimo sync riuscito: {{.LastPBXSync}}</p>
    <button hx-post="/admin/pbx/sync" hx-target="#pbx-content" hx-swap="innerHTML" class="btn btn-primary" style="flex:none;">Sincronizza ora</button>
</div>
```

- [ ] **Step 2: Aggiungi la pagina completa**

`web/templates/admin_page_pbx.html`:

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Centralino - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        {{template "rail.html" .}}
        <main class="main" style="max-width:920px;">
            <h1 class="page-title">Centralino</h1>
            <div id="pbx-content">{{template "admin_pbx.html" .}}</div>
        </main>
    </div>
</body>
</html>
```

- [ ] **Step 3: Aggiungi la voce di menu in `rail.html`**

Dopo la voce `/admin/local-contacts` (circa riga 51-54 di `web/templates/rail.html`):

```html
    <a href="/admin/local-contacts" class="rail-item{{if eq .Section "admin-contacts"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><circle cx="12" cy="8" r="3"/><path d="M5 20c0-3.9 3.1-7 7-7s7 3.1 7 7"/><path d="M19 8v4M21 10h-4"/></svg>
        <span>Contatti locali</span>
    </a>
    <a href="/admin/pbx" class="rail-item{{if eq .Section "admin-pbx"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M22 16.92v3a2 2 0 01-2.18 2 19.79 19.79 0 01-8.63-3.07 19.5 19.5 0 01-6-6 19.79 19.79 0 01-3.07-8.67A2 2 0 014.11 2h3a2 2 0 012 1.72c.127.96.362 1.903.7 2.81a2 2 0 01-.45 2.11L8.09 9.91a16 16 0 006 6l1.27-1.27a2 2 0 012.11-.45c.907.338 1.85.573 2.81.7A2 2 0 0122 16.92z"/></svg>
        <span>Centralino</span>
    </a>
```

- [ ] **Step 4: Aggiungi gli handler**

In fondo al blocco "Admin handlers" (dopo `handleAdminContactOverride`, o vicino a `handleAdminConfig`):

```go
func renderPBX(w http.ResponseWriter, r *http.Request) {
	url, user, pass := pbx.LoadPBXConfig(db)
	filters := pbx.LoadFilters(db)

	data := railData()
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-pbx"
	data["PBXURL"] = url
	data["PBXUser"] = user
	data["PBXHasPassword"] = pass != ""
	data["PBXExcludeUnnamed"] = filters.ExcludeUnnamed
	data["PBXExcludeInactiveGroups"] = filters.ExcludeInactiveGroups
	data["PBXExcludeEmptyGroups"] = filters.ExcludeEmptyGroups
	if lastPBXSync.IsZero() {
		data["LastPBXSync"] = "mai"
	} else {
		data["LastPBXSync"] = lastPBXSync.Format("2006-01-02 15:04:05")
	}
	templates.ExecuteTemplate(w, "admin_pbx.html", data)
}

func handleAdminPBX(w http.ResponseWriter, r *http.Request) {
	url, user, pass := pbx.LoadPBXConfig(db)
	filters := pbx.LoadFilters(db)

	data := railData()
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-pbx"
	data["PBXURL"] = url
	data["PBXUser"] = user
	data["PBXHasPassword"] = pass != ""
	data["PBXExcludeUnnamed"] = filters.ExcludeUnnamed
	data["PBXExcludeInactiveGroups"] = filters.ExcludeInactiveGroups
	data["PBXExcludeEmptyGroups"] = filters.ExcludeEmptyGroups
	if lastPBXSync.IsZero() {
		data["LastPBXSync"] = "mai"
	} else {
		data["LastPBXSync"] = lastPBXSync.Format("2006-01-02 15:04:05")
	}
	templates.ExecuteTemplate(w, "admin_page_pbx.html", data)
}

// handleAdminSavePBXConfig salva url/utente/password/filtri del centralino.
// La password inviata vuota lascia invariata quella già salvata (non viene
// mai ri-mostrata in chiaro nel form). Le checkbox dei filtri non compaiono
// nel form POST quando deselezionate (comportamento standard HTML) — la
// loro assenza va quindi letta come "false", non come "campo mancante".
func handleAdminSavePBXConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	if err := db.SetConfig(pbx.PBXURLConfigKey, strings.TrimSpace(r.FormValue("pbx_url"))); err != nil {
		http.Error(w, "Failed to save PBX URL", http.StatusInternalServerError)
		return
	}
	if err := db.SetConfig(pbx.PBXUserConfigKey, strings.TrimSpace(r.FormValue("pbx_user"))); err != nil {
		http.Error(w, "Failed to save PBX user", http.StatusInternalServerError)
		return
	}
	if newPass := r.FormValue("pbx_pass"); newPass != "" {
		if err := db.SetConfig(pbx.PBXPassConfigKey, newPass); err != nil {
			http.Error(w, "Failed to save PBX password", http.StatusInternalServerError)
			return
		}
	}

	filters := pbx.Filters{
		ExcludeUnnamed:        r.FormValue("exclude_unnamed") != "",
		ExcludeInactiveGroups: r.FormValue("exclude_inactive_groups") != "",
		ExcludeEmptyGroups:    r.FormValue("exclude_empty_groups") != "",
	}
	if err := pbx.SaveFilters(db, filters); err != nil {
		http.Error(w, "Failed to save PBX filters", http.StatusInternalServerError)
		return
	}

	renderPBX(w, r)
}

func handleAdminSyncPBX(w http.ResponseWriter, r *http.Request) {
	if err := pbx.SyncPBX(db); err != nil {
		log.Printf("[PBX] Manual sync failed: %v", err)
	} else {
		lastPBXSync = time.Now()
		log.Printf("[PBX] Manual sync completed")
	}
	renderPBX(w, r)
}
```

- [ ] **Step 5: Registra le route**

Nel blocco `admin := r.PathPrefix("/admin").Subrouter()` (dopo `admin.HandleFunc("/contacts/{uid}/override", ...)`):

```go
	admin.HandleFunc("/pbx", handleAdminPBX).Methods("GET")
	admin.HandleFunc("/pbx", handleAdminSavePBXConfig).Methods("POST")
	admin.HandleFunc("/pbx/sync", handleAdminSyncPBX).Methods("POST")
```

- [ ] **Step 6: Verifica che l'intero progetto compili e la suite passi**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go build ./... && CGO_ENABLED=1 go vet ./... && CGO_ENABLED=1 go test ./... -v"
```
Expected: build ok, nessun warning `go vet`, tutti i test PASS.

- [ ] **Step 7: Verifica manuale (opzionale ma consigliata)**

Run: `docker compose up -d --build`, poi apri `/admin/pbx` da browser autenticato come admin, salva URL/utente/password di test, premi "Sincronizza ora" e verifica che la pagina mostri un errore leggibile se le credenziali sono sbagliate (non un 500 bianco) e l'orario aggiornato se riuscito.

- [ ] **Step 8: Commit**

```bash
git add web/templates/admin_page_pbx.html web/templates/admin_pbx.html web/templates/rail.html cmd/server/main.go
git commit -m "feat(admin): pagina /admin/pbx per config centralino, filtri esclusione e sync manuale"
```

---

## Task 8: Rifattorizza `cmd/pbxpoc` per usare `internal/pbx`

**Files:**
- Modify: `cmd/pbxpoc/main.go`

**Interfaces:**
- Consumes: `pbx.NewClient`, `(*pbx.Client).Login`, `(*pbx.Client).FetchPeers`, `(*pbx.Client).FetchCallGroups` (Task 4), `database.InitDB` (esistente), `db.ListDomainExtensions` (Task 2).

Il PoC duplicava login/parsing che ora vive in `internal/pbx` (Task 4) — questo task lo riduce a un thin wrapper CLI utile per debug manuale, senza duplicare logica (DRY).

- [ ] **Step 1: Riscrivi `cmd/pbxpoc/main.go`**

```go
// Comando pbxpoc: strumento di debug manuale per il sync PBX. Fa login,
// scarica peers + call group con internal/pbx, e stampa a schermo cosa
// verrebbe applicato (peers esclusi quelli già in dominio LDAP) — utile
// per verificare credenziali/endpoint prima che il sync orario reale
// (cmd/server, vedi internal/pbx.SyncPBX) tocchi il database.
//
// Uso:
//
//	PBX_URL=https://10.0.90.253 PBX_USER=admin PBX_PASS=... go run ./cmd/pbxpoc
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/mirkochipdotcom/ldavsync/internal/config"
	"github.com/mirkochipdotcom/ldavsync/internal/database"
	"github.com/mirkochipdotcom/ldavsync/internal/pbx"
)

func main() {
	pbxURL := mustEnv("PBX_URL")
	pbxUser := mustEnv("PBX_USER")
	pbxPass := mustEnv("PBX_PASS")

	cfg := config.Load()
	db, err := database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("apertura db: %v", err)
	}
	defer db.Close()

	domainExts, err := db.ListDomainExtensions()
	if err != nil {
		log.Fatalf("lettura interni dominio: %v", err)
	}
	inDomain := make(map[string]struct{}, len(domainExts))
	for _, e := range domainExts {
		inDomain[e] = struct{}{}
	}
	log.Printf("interni già in dominio (esclusi dai peers PBX): %d", len(inDomain))

	client := pbx.NewClient(pbxURL)
	if err := client.Login(pbxUser, pbxPass); err != nil {
		log.Fatalf("login PBX: %v", err)
	}

	peers, err := client.FetchPeers()
	if err != nil {
		log.Fatalf("fetch peers: %v", err)
	}

	fmt.Println("\n=== Peers SOLO centralino (esclusi quelli già sincronizzati da LDAP) ===")
	for _, p := range peers {
		if _, ok := inDomain[p.Extension]; ok {
			continue
		}
		fmt.Printf("%-6s %-40s status=%q\n", p.Extension, p.CallerID, p.Status)
	}

	groups, err := client.FetchCallGroups()
	if err != nil {
		log.Fatalf("fetch call groups: %v", err)
	}

	fmt.Println("\n=== Gruppi di chiamata ===")
	for _, g := range groups {
		fmt.Printf("%-35s interno=%-6s strategy=%-10s timeout=%-4s abilitato=%v membri=%v\n",
			g.Name, g.Extension, g.Strategy, g.Timeout, g.Enabled, g.Members)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("variabile env %s mancante", key)
	}
	return v
}
```

- [ ] **Step 2: Verifica che compili**

Run:
```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go build ./..."
```
Expected: build ok.

- [ ] **Step 3: Commit**

```bash
git add cmd/pbxpoc/main.go
git commit -m "refactor(pbxpoc): usa internal/pbx invece di duplicare login/parsing"
```

---

## Task 9: Documentazione — CLAUDE.md

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Aggiungi una voce all'Architecture**

Nella sezione `## Architecture` di `CLAUDE.md`, dopo il bullet su `internal/phonebook`:

```markdown
- **`internal/pbx`**: screen-scraping (nessuna API ufficiale) della web console del centralino Invidea "ViVo" — sync orario opzionale, stesso giro di `ldap.SyncContacts`, attivo solo se configurato da `/admin/pbx` (nessuna env var: URL/utente/password vivono solo in `app_config`). Peers SIP non coperti da un contatto `source='ldap'` (stesso `ldap_ext`) diventano `contacts` `source='pbx'` (`department="Centralino - non mappato"`, nome/reparto/area overridabili come i manuali via `manual_override`). Call group del centralino estendono `group_numbers` (colonne `source`/`name_override`): il nome è overridabile in locale, ma il mapping membri è sempre sovrascritto dal centralino (fonte di verità assoluta) — un gruppo manuale con lo stesso interno viene sostituito, migrandone il nome come override. Tre filtri di esclusione configurabili da `/admin/pbx` (default tutti attivi): contatti con nome placeholder (`<numero>`), call group disattivi, call group senza destinatari. Vedi `docs/superpowers/specs/2026-09-13-pbx-scraping-design.md`.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: documenta internal/pbx in CLAUDE.md"
```
