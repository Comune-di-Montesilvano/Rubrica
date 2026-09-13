# Rubrica LdavSync — redesign funzioni e rappresentazione dati Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sostituire la rubrica LdavSync (rotta a livello visivo e con Area
dedotta a runtime) con una vista unica a gruppi-reparto, un nuovo sistema
visivo coerente su tutte le pagine, e Area/description normalizzati a
sync-time invece che a ogni richiesta.

**Architecture:** Nessun nuovo servizio: si aggiunge una colonna `area` a
`contacts` (calcolata in `sync.go` dall'OU del DN LDAP), una funzione pura
di grouping in `internal/phonebook`, e si riscrivono CSS + template Go
esistenti. Tutta la logica nuova che non richiede LDAP/HTTP reale (area,
normalizzazione description, grouping) è testata con `go test`; template e
CSS si verificano visivamente con `docker compose up` (nessun test HTML
nel progetto, pattern coerente con l'esistente).

**Tech Stack:** Go 1.22, `gorilla/mux`, `mattn/go-sqlite3` (cgo), html/template,
HTMX 2.0, CSS custom (nessun framework) — nessuna libreria nuova.

**Spec:** `docs/superpowers/specs/2026-09-13-rubrica-redesign-design.md`

## Global Constraints

- CGO_ENABLED=1 richiesto per build/test (driver sqlite3) — vale per ogni task che tocca `internal/database`.
- Nessuna modifica a `internal/ldap` per la logica di *inclusione* sync (LDAP_ALLOWED_GROUPS, disabled-bit) — già verificata corretta.
- Nessuna modifica a `internal/carddav` — usa `cfg.LDAPOUFilters`, che resta intatto in `internal/config` (non è dead code: serve a CardDAV, fuori scope).
- `Title` resta in DB, sparisce solo dai template — nessuna migrazione di colonna per quello.
- Palette/token esatti dallo spec (sezione "Sistema visivo") — non improvvisare nuovi colori.
- Font: Archivo (500/600/700) intestazioni, Inter (400/500/600) corpo/numeri con `font-variant-numeric: tabular-nums`, caricati da Google Fonts come nei mockup.

---

## File Structure

- **Modify** `internal/database/sqlite.go` — colonna `area`, campo `Contact.Area`, tutte le query Contact (SELECT/Scan/UPSERT), nuovo `CountByArea()`.
- **Create** `internal/database/sqlite_test.go` — test per `Area` persistita e `CountByArea`.
- **Modify** `internal/ldap/sync.go` — `deriveArea`, `normalizeDescription`, uso nel loop di `SyncContacts`.
- **Create** `internal/ldap/sync_test.go` — test per `deriveArea`/`normalizeDescription` (funzioni pure, nessun LDAP reale).
- **Modify** `internal/phonebook/service.go` — `DepartmentGroup`, `GroupByDepartment`.
- **Create** `internal/phonebook/service_test.go` — test per `GroupByDepartment`.
- **Modify** `internal/i18n/messages.go` — nuove chiavi it/en per area, prefisso, azioni.
- **Modify** `web/static/css/style.css` — sistema visivo completo (sostituisce l'attuale).
- **Modify** `cmd/server/main.go` — `initials` in `funcMap`, `handleIndex` (AreaCounts/Total), `handleSearch` (filtro su `Contact.Area`, grouping).
- **Modify** `web/templates/phonebook.html` — rail di navigazione + ricerca + helper prefisso.
- **Modify** `web/templates/search_results.html` — vista a gruppi reparto invece di tabella.
- **Modify** `web/templates/contact_detail.html` — via CSS del progetto, non più Tailwind CDN.
- **Modify** `web/templates/login.html` — via CSS del progetto.
- **Modify** `web/templates/admin.html` — via CSS del progetto, sidebar admin.
- **Modify** `web/templates/admin_groups.html` — via CSS del progetto.
- **Modify** `web/templates/admin_group_members.html` — via CSS del progetto.

---

### Task 1: Colonna `area` in DB + `CountByArea`

**Files:**
- Modify: `internal/database/sqlite.go`
- Test: `internal/database/sqlite_test.go`

**Interfaces:**
- Produces: `Contact.Area string` (campo pubblico), `(*DB) CountByArea() (map[string]int, error)`.

- [ ] **Step 1: Scrivi i test (falliranno: `Area` non esiste ancora)**

Crea `internal/database/sqlite_test.go`:

```go
package database

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUpsertContactPersistsArea(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{
		UID:         "mario.rossi",
		DisplayName: "Mario Rossi",
		Area:        "interni",
		LastSync:    time.Now(),
	}
	if err := db.UpsertContact(c); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	got, err := db.GetContact("mario.rossi")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetContact returned nil")
	}
	if got.Area != "interni" {
		t.Errorf("Area = %q, want %q", got.Area, "interni")
	}
}

func TestUpsertContactRespectsManualOverrideForArea(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{UID: "u1", DisplayName: "U1", Area: "interni", LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatalf("initial UpsertContact failed: %v", err)
	}
	if err := db.UpdateContactOverride("u1", "u1@example.com", "12345"); err != nil {
		t.Fatalf("UpdateContactOverride failed: %v", err)
	}

	c2 := &Contact{UID: "u1", DisplayName: "U1", Area: "esterni", LastSync: time.Now()}
	if err := db.UpsertContact(c2); err != nil {
		t.Fatalf("second UpsertContact failed: %v", err)
	}

	got, err := db.GetContact("u1")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got.Area != "interni" {
		t.Errorf("Area = %q after override, want unchanged %q", got.Area, "interni")
	}
}

func TestCountByArea(t *testing.T) {
	db := newTestDB(t)
	contacts := []*Contact{
		{UID: "a", DisplayName: "A", Area: "interni", LastSync: time.Now()},
		{UID: "b", DisplayName: "B", Area: "interni", LastSync: time.Now()},
		{UID: "c", DisplayName: "C", Area: "esterni", LastSync: time.Now()},
		{UID: "d", DisplayName: "D", Area: "politica", LastSync: time.Now()},
	}
	for _, c := range contacts {
		if err := db.UpsertContact(c); err != nil {
			t.Fatalf("UpsertContact(%s) failed: %v", c.UID, err)
		}
	}

	counts, err := db.CountByArea()
	if err != nil {
		t.Fatalf("CountByArea failed: %v", err)
	}
	want := map[string]int{"interni": 2, "esterni": 1, "politica": 1}
	for area, wantCount := range want {
		if counts[area] != wantCount {
			t.Errorf("counts[%q] = %d, want %d", area, counts[area], wantCount)
		}
	}
}
```

- [ ] **Step 2: Esegui i test, verifica che falliscano**

Run: `CGO_ENABLED=1 go test ./internal/database/... -run TestUpsertContactPersistsArea -v`
Expected: FAIL con `unknown field Area in struct literal`.

- [ ] **Step 3: Aggiungi il campo `Area` e la migrazione**

In `internal/database/sqlite.go`, nello struct `Contact` aggiungi il campo
subito dopo `LDAPDN`:

```go
type Contact struct {
	ID             int64
	UID            string
	DisplayName    string
	Email          string
	LDAPExt        string
	PrimaryNumber  string
	Department     string
	Title          string
	Description    string
	LDAPGroups     string
	LDAPDN         string
	Area           string
	ManualOverride bool
	DeletedAt      *time.Time
	LastSync       time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
```

Nel metodo `migrate()`, aggiungi alla lista `alterStatements`:

```go
	alterStatements := []string{
		"ALTER TABLE contacts ADD COLUMN title TEXT",
		"ALTER TABLE contacts ADD COLUMN description TEXT",
		"ALTER TABLE contacts ADD COLUMN area TEXT",
	}
```

- [ ] **Step 4: Aggiorna `UpsertContact` per scrivere/rispettare `area`**

Sostituisci la query e la chiamata `Exec` in `UpsertContact`:

```go
	query := `
	INSERT INTO contacts (uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(uid) DO UPDATE SET
		display_name = CASE WHEN manual_override = 0 THEN excluded.display_name ELSE display_name END,
		email = CASE WHEN manual_override = 0 THEN excluded.email ELSE email END,
		ldap_ext = CASE WHEN manual_override = 0 THEN excluded.ldap_ext ELSE ldap_ext END,
		primary_number = CASE WHEN manual_override = 0 THEN excluded.primary_number ELSE primary_number END,
		department = CASE WHEN manual_override = 0 THEN excluded.department ELSE department END,
		title = CASE WHEN manual_override = 0 THEN excluded.title ELSE title END,
		description = CASE WHEN manual_override = 0 THEN excluded.description ELSE description END,
		ldap_groups = CASE WHEN manual_override = 0 THEN excluded.ldap_groups ELSE ldap_groups END,
		ldap_dn = CASE WHEN manual_override = 0 THEN excluded.ldap_dn ELSE ldap_dn END,
		area = CASE WHEN manual_override = 0 THEN excluded.area ELSE area END,
		deleted_at = NULL,
		last_sync = excluded.last_sync,
		updated_at = excluded.updated_at
	`

	if contact.CreatedAt.IsZero() {
		contact.CreatedAt = now
	}

	result, err := db.Exec(query, contact.UID, contact.DisplayName, contact.Email, contact.LDAPExt,
		contact.PrimaryNumber, contact.Department, contact.Title, contact.Description, contact.LDAPGroups, contact.LDAPDN, contact.Area, contact.ManualOverride, contact.DeletedAt,
		contact.LastSync, contact.CreatedAt, contact.UpdatedAt)
```

- [ ] **Step 5: Aggiorna SELECT/Scan in `GetContact`, `SearchContacts`, `ListContacts`, `ListAllContacts`, `GetGroupMembers`**

In ognuna di queste funzioni, aggiungi `area` alla lista colonne subito dopo
`ldap_dn` nella query SQL, e `&contact.Area` subito dopo `&contact.LDAPDN`
in ogni `Scan(...)`. Esempio per `GetContact` (stesso pattern per le altre
quattro):

```go
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE uid = ? AND deleted_at IS NULL
	`

	contact := &Contact{}
	err := db.QueryRow(query, uid).Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
		&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
		&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
```

- [ ] **Step 6: Aggiungi `CountByArea`**

Alla fine della sezione "Contact operations" in `sqlite.go`:

```go
// CountByArea returns the number of active (non-deleted) contacts per
// Area value ("interni"/"esterni"/"politica"). Contacts with no area yet
// assigned (not re-synced since this field was added) are counted under
// the empty string key.
func (db *DB) CountByArea() (map[string]int, error) {
	query := `SELECT COALESCE(area, '') AS area, COUNT(*) FROM contacts WHERE deleted_at IS NULL GROUP BY area`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to count by area: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var area string
		var count int
		if err := rows.Scan(&area, &count); err != nil {
			return nil, fmt.Errorf("failed to scan area count: %w", err)
		}
		counts[area] = count
	}
	return counts, nil
}
```

- [ ] **Step 7: Esegui i test, verifica che passino**

Run: `CGO_ENABLED=1 go test ./internal/database/... -v`
Expected: PASS su tutti e tre i nuovi test.

- [ ] **Step 8: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(database): persist contact Area, add CountByArea"
```

---

### Task 2: Derivazione Area + normalizzazione Description a sync-time

**Files:**
- Modify: `internal/ldap/sync.go`
- Test: `internal/ldap/sync_test.go`

**Interfaces:**
- Consumes: nessuna nuova dipendenza esterna.
- Produces: `deriveArea(dn string) string`, `normalizeDescription(raw string) string` (entrambe non esportate, usate solo dentro il package `ldap`); `Contact.Area` da Task 1.

- [ ] **Step 1: Scrivi i test (falliranno: le funzioni non esistono)**

Crea `internal/ldap/sync_test.go`:

```go
package ldap

import "testing"

func TestDeriveArea(t *testing.T) {
	cases := []struct {
		name string
		dn   string
		want string
	}{
		{"interni", "CN=Mario Rossi,OU=Users,OU=INTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "interni"},
		{"esterni", "CN=Anna Bianchi,OU=Users,OU=ESTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "esterni"},
		{"politica", "CN=Corinna Sandias,OU=Users,OU=AREA_POLITICA,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "politica"},
		{"ou sconosciuta", "CN=Guest,CN=Users,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", ""},
		{"case insensitive", "cn=Test,ou=Users,ou=interni,ou=comune-ms,dc=intranet,dc=comune,dc=montesilvano,dc=pe,dc=it", "interni"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveArea(c.dn)
			if got != c.want {
				t.Errorf("deriveArea(%q) = %q, want %q", c.dn, got, c.want)
			}
		})
	}
}

func TestNormalizeDescription(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Agente di Polizi Locale", "Agente di Polizia Locale"},
		{"agente di polizia locale", "Agente di Polizia Locale"},
		{"Edliizia", "Edilizia"},
		{"Istuttore", "Istruttore"},
		{"  Dirigente  ", "Dirigente"},
		{"", ""},
		{"Assessore", "Assessore"},
		{"Multi   spazi   interni", "Multi spazi interni"},
	}
	for _, c := range cases {
		got := normalizeDescription(c.in)
		if got != c.want {
			t.Errorf("normalizeDescription(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Esegui i test, verifica che falliscano**

Run: `go test ./internal/ldap/... -run TestDeriveArea -v`
Expected: FAIL con `undefined: deriveArea`.

- [ ] **Step 3: Implementa le funzioni in `sync.go`**

Aggiungi in `internal/ldap/sync.go`, vicino alle altre funzioni helper
(`extractCNFromDN` ecc.):

```go
// deriveArea maps an LDAP DN to the app's Area classification, based on
// the OU segment used by this AD structure (OU=INTERNI / OU=ESTERNI /
// OU=AREA_POLITICA sotto OU=COMUNE-MS). Ritorna "" se nessuna OU nota è
// trovata (account builtin/servizio come Guest, Administrator, krbtgt).
func deriveArea(dn string) string {
	d := strings.ToLower(dn)
	switch {
	case strings.Contains(d, "ou=interni"):
		return "interni"
	case strings.Contains(d, "ou=esterni"):
		return "esterni"
	case strings.Contains(d, "ou=area_politica"):
		return "politica"
	default:
		return ""
	}
}

// descriptionAliases raccoglie varianti note (typo, maiuscole incoerenti)
// del campo description di AD, osservate sui dati reali del Comune di
// Montesilvano. Le chiavi sono minuscole: normalizeDescription confronta
// dopo strings.ToLower, quindi qualsiasi variante di maiuscola collassa
// già a questo punto senza bisogno di duplicare le voci.
var descriptionAliases = map[string]string{
	"agente di polizi locale":  "Agente di Polizia Locale",
	"agente di polizia locale": "Agente di Polizia Locale",
	"edliizia":                 "Edilizia",
	"istuttore":                "Istruttore",
	"usciere":                  "Usciere",
}

// normalizeDescription collassa spazi multipli e riscrive typo/varianti
// note al valore canonico. Valori non mappati passano inalterati (solo
// trim). Non modifica LDAP, solo il valore scritto in contacts.description.
func normalizeDescription(raw string) string {
	trimmed := strings.Join(strings.Fields(raw), " ")
	if trimmed == "" {
		return ""
	}
	if canonical, ok := descriptionAliases[strings.ToLower(trimmed)]; ok {
		return canonical
	}
	return trimmed
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `go test ./internal/ldap/... -v`
Expected: PASS su `TestDeriveArea` e `TestNormalizeDescription`.

- [ ] **Step 5: Collega le funzioni dentro `SyncContacts`**

In `SyncContacts`, dove oggi c'è:

```go
		title := entry.GetAttributeValue("title")
		description := entry.GetAttributeValue("description")
```

sostituisci con:

```go
		title := entry.GetAttributeValue("title")
		description := normalizeDescription(entry.GetAttributeValue("description"))
```

e nella costruzione di `contact := &database.Contact{...}`, aggiungi il
campo `Area`:

```go
		contact := &database.Contact{
			UID:           uid,
			DisplayName:   displayName,
			Email:         email,
			LDAPExt:       telephoneNumber,
			PrimaryNumber: primaryNumber,
			Department:    department,
			Title:         title,
			Description:   description,
			LDAPGroups:    ldapGroupsStr,
			LDAPDN:        entry.DN,
			Area:          deriveArea(entry.DN),
			LastSync:      syncTime,
		}
```

- [ ] **Step 6: Verifica che il progetto compili**

Run: `CGO_ENABLED=1 go build ./...`
Expected: nessun errore.

- [ ] **Step 7: Commit**

```bash
git add internal/ldap/sync.go internal/ldap/sync_test.go
git commit -m "feat(ldap): derive Area from DN, normalize description at sync time"
```

---

### Task 3: Grouping per reparto (`phonebook.GroupByDepartment`)

**Files:**
- Modify: `internal/phonebook/service.go`
- Test: `internal/phonebook/service_test.go`

**Interfaces:**
- Consumes: `ContactWithGroups` (Task esistente), `database.Contact.Department`, `database.Contact.Area` (Task 1).
- Produces: `type DepartmentGroup struct { Name string; Contacts []*ContactWithGroups }`, `func GroupByDepartment(contacts []*ContactWithGroups) []*DepartmentGroup`.

- [ ] **Step 1: Scrivi il test (fallirà: `GroupByDepartment` non esiste)**

Crea `internal/phonebook/service_test.go`:

```go
package phonebook

import (
	"testing"

	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

func TestGroupByDepartment(t *testing.T) {
	contacts := []*ContactWithGroups{
		{Contact: &database.Contact{UID: "a", Department: "Polizia Locale"}},
		{Contact: &database.Contact{UID: "b", Department: "Polizia Locale"}},
		{Contact: &database.Contact{UID: "c", Department: "Urbanistica"}},
		{Contact: &database.Contact{UID: "d", Department: "", Area: "politica"}},
		{Contact: &database.Contact{UID: "e", Department: ""}},
	}

	groups := GroupByDepartment(contacts)

	if len(groups) != 4 {
		t.Fatalf("got %d groups, want 4", len(groups))
	}
	if groups[0].Name != "Polizia Locale" || len(groups[0].Contacts) != 2 {
		t.Errorf("groups[0] = %q with %d contacts, want Polizia Locale with 2", groups[0].Name, len(groups[0].Contacts))
	}

	names := map[string]int{}
	for _, g := range groups {
		names[g.Name] = len(g.Contacts)
	}
	if names["Amministrazione politica"] != 1 {
		t.Errorf("Amministrazione politica count = %d, want 1", names["Amministrazione politica"])
	}
	if names["Senza reparto"] != 1 {
		t.Errorf("Senza reparto count = %d, want 1", names["Senza reparto"])
	}
	if names["Urbanistica"] != 1 {
		t.Errorf("Urbanistica count = %d, want 1", names["Urbanistica"])
	}
}

func TestGroupByDepartmentEmptyInput(t *testing.T) {
	groups := GroupByDepartment(nil)
	if len(groups) != 0 {
		t.Errorf("got %d groups for nil input, want 0", len(groups))
	}
}
```

- [ ] **Step 2: Esegui il test, verifica che fallisca**

Run: `go test ./internal/phonebook/... -run TestGroupByDepartment -v`
Expected: FAIL con `undefined: GroupByDepartment`.

- [ ] **Step 3: Implementa in `service.go`**

Aggiungi `"sort"` agli import, poi alla fine del file:

```go
// DepartmentGroup è un'intestazione reparto con i contatti al suo interno,
// usata per la vista a gruppi collassabili (dept-head + righe dense in
// search_results.html).
type DepartmentGroup struct {
	Name     string
	Contacts []*ContactWithGroups
}

// GroupByDepartment raggruppa i contatti per Department (etichetta così
// com'è, non normalizzata qui — la sentence-case è responsabilità del
// template). I contatti senza reparto vengono raggruppati per Area invece:
// "politica" -> "Amministrazione politica", altrimenti -> "Senza reparto".
// I gruppi sono ordinati per numero di membri decrescente, poi
// alfabeticamente; i contatti mantengono l'ordine di arrivo (i chiamanti
// passano risultati già ordinati per display_name dalla query DB).
func GroupByDepartment(contacts []*ContactWithGroups) []*DepartmentGroup {
	index := make(map[string]int)
	groups := make([]*DepartmentGroup, 0)

	for _, c := range contacts {
		name := c.Contact.Department
		if name == "" {
			if c.Contact.Area == "politica" {
				name = "Amministrazione politica"
			} else {
				name = "Senza reparto"
			}
		}
		i, ok := index[name]
		if !ok {
			i = len(groups)
			index[name] = i
			groups = append(groups, &DepartmentGroup{Name: name})
		}
		groups[i].Contacts = append(groups[i].Contacts, c)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].Contacts) != len(groups[j].Contacts) {
			return len(groups[i].Contacts) > len(groups[j].Contacts)
		}
		return groups[i].Name < groups[j].Name
	})

	return groups
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `go test ./internal/phonebook/... -v`
Expected: PASS su entrambi i test.

- [ ] **Step 5: Commit**

```bash
git add internal/phonebook/service.go internal/phonebook/service_test.go
git commit -m "feat(phonebook): add GroupByDepartment for grouped index view"
```

---

### Task 4: Nuove chiavi i18n

**Files:**
- Modify: `internal/i18n/messages.go`

**Interfaces:**
- Consumes: mappa `messages["it"]`/`messages["en"]` esistente.
- Produces: chiavi `area_all`, `area_interni`, `area_esterni`, `area_politica`, `prefix_helper`, `extension`, `call_extension`, `search_placeholder_v2` non serve — riusa `search_placeholder` esistente.

- [ ] **Step 1: Aggiungi le chiavi alla mappa `it`**

In `internal/i18n/messages.go`, dentro `messages["it"]`, dopo
`"no_results": "Nessun risultato",` aggiungi:

```go
			"area_all":        "Tutti",
			"area_interni":    "Interni",
			"area_esterni":    "Esterni",
			"area_politica":   "Politica",
			"prefix_helper":   "Da fuori l'ente, componi %s prima dell'interno mostrato sotto",
			"extension":       "interno",
			"call_extension":  "Chiama interno",
```

- [ ] **Step 2: Aggiungi le stesse chiavi alla mappa `en`**

Dentro `messages["en"]`, dopo `"no_results": "No results found",`:

```go
			"area_all":        "All",
			"area_interni":    "Internal",
			"area_esterni":    "External",
			"area_politica":   "Political",
			"prefix_helper":   "From outside the office, dial %s before the extension shown below",
			"extension":       "ext.",
			"call_extension":  "Call extension",
```

- [ ] **Step 3: Verifica che compili**

Run: `go build ./...`
Expected: nessun errore.

- [ ] **Step 4: Commit**

```bash
git add internal/i18n/messages.go
git commit -m "feat(i18n): add area/prefix-helper message keys"
```

---

### Task 5: Sistema visivo — riscrittura `style.css`

**Files:**
- Modify: `web/static/css/style.css`

**Interfaces:**
- Produces: classi CSS consumate dai Task 7-10 (`.shell`, `.rail`, `.rail-item`, `.search`, `.prefix-helper`, `.dept-head`, `.row`, `.badge`, `.who`, `.name`, `.role`, `.phone`, `.detail`, `.field`, `.btn`/`.btn-primary`/`.btn-ghost`/`.btn-danger`, `.card`, `.grid2`, `.input`, tabelle admin, `.login-card`).

- [ ] **Step 1: Sostituisci l'intero contenuto di `web/static/css/style.css`**

```css
/* LdavSync — design system (verdigris/ink/paper), dark mode automatico */

@import url('https://fonts.googleapis.com/css2?family=Archivo:wght@500;600;700&family=Inter:wght@400;500;600&display=swap');

:root {
    --ink: #1B242E;
    --ink-soft: #5B6672;
    --paper: #F3F1EC;
    --paper-raised: #FFFFFF;
    --line: #DEDAD1;
    --accent: #3E6E63;
    --accent-tint: #E8EFED;
    --muted: #9B958A;
    --danger: #9B4A3F;
    --danger-tint: #F3E9E6;
}

@media (prefers-color-scheme: dark) {
    :root {
        --ink: #E7E4DC;
        --ink-soft: #9BA3AB;
        --paper: #14181C;
        --paper-raised: #1B2025;
        --line: #2A2F34;
        --accent: #6FAE9E;
        --accent-tint: #1D2723;
        --muted: #6B7178;
        --danger: #D48477;
        --danger-tint: #2A1E1B;
    }
}

* { box-sizing: border-box; }

body {
    margin: 0;
    background: var(--paper);
    color: var(--ink);
    font-family: 'Inter', system-ui, sans-serif;
    -webkit-font-smoothing: antialiased;
}

svg { display: block; }

a { color: inherit; }

/* --- App shell (pagine pubbliche) --- */

.shell { display: flex; min-height: 100vh; }

.rail {
    width: 212px;
    flex: 0 0 212px;
    background: var(--paper);
    border-right: 1px solid var(--line);
    padding: 22px 0;
    display: flex;
    flex-direction: column;
}

.rail-brand {
    font-family: 'Archivo', sans-serif;
    font-weight: 700;
    font-size: 16px;
    letter-spacing: -0.01em;
    padding: 0 20px 20px;
    color: var(--ink);
    text-decoration: none;
    display: block;
}

.rail-brand span { color: var(--accent); }

.rail-group {
    padding: 0 20px 6px;
    font-size: 11px;
    color: var(--muted);
    margin-top: 16px;
}

.rail-item {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 7px 20px;
    font-size: 13.5px;
    color: var(--ink-soft);
    cursor: pointer;
    border-left: 2px solid transparent;
    text-decoration: none;
    background: none;
    border-top: none; border-right: none; border-bottom: none;
    width: 100%;
    text-align: left;
}

.rail-item svg { width: 15px; height: 15px; stroke: var(--ink-soft); flex: 0 0 15px; }
.rail-item.active { color: var(--ink); border-left-color: var(--accent); background: var(--accent-tint); font-weight: 500; }
.rail-item.active svg { stroke: var(--accent); }
.rail-item .n { margin-left: auto; color: var(--muted); font-size: 11.5px; font-variant-numeric: tabular-nums; }

.rail-foot {
    margin-top: auto;
    padding: 14px 20px 0;
    border-top: 1px solid var(--line);
    font-size: 12px;
    color: var(--ink-soft);
}
.rail-foot a { color: var(--danger); text-decoration: none; }

.main { flex: 1; padding: 26px 40px 60px; max-width: 860px; }

.search {
    display: flex; align-items: center; gap: 10px;
    background: var(--paper-raised); border: 1px solid var(--line);
    border-radius: 8px; padding: 10px 14px; margin-bottom: 14px;
}
.search svg { width: 16px; height: 16px; stroke: var(--muted); flex: 0 0 16px; }
.search input { border: none; outline: none; background: none; font-family: 'Inter'; font-size: 14px; flex: 1; color: var(--ink); }
.search input::placeholder { color: var(--muted); }

/* --- Helper prefisso --- */

.prefix-helper {
    display: flex; align-items: center; gap: 9px; font-size: 12px; color: var(--ink-soft);
    background: var(--accent-tint); border: 1px solid var(--line); border-radius: 8px;
    padding: 9px 14px; margin-bottom: 24px;
}
.prefix-helper svg { width: 14px; height: 14px; stroke: var(--accent); flex: 0 0 14px; }
.prefix-helper b { color: var(--ink); font-weight: 600; font-variant-numeric: tabular-nums; }
.prefix-helper button { margin-left: auto; background: none; border: none; color: var(--muted); font-size: 13px; cursor: pointer; padding: 0; line-height: 1; }
.prefix-helper[hidden] { display: none; }

/* --- Gruppi reparto + righe contatto --- */

.dept { margin-top: 24px; }
.dept:first-child { margin-top: 0; }
.dept-head {
    display: flex; align-items: baseline; gap: 8px; cursor: pointer;
    padding: 8px 4px 7px; border-bottom: 1px solid var(--ink);
}
.dept-name { font-family: 'Archivo', sans-serif; font-weight: 600; font-size: 14.5px; letter-spacing: -0.003em; }
.dept-count { font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
.dept-tag { margin-left: auto; font-size: 10.5px; color: var(--muted); border: 1px solid var(--line); border-radius: 4px; padding: 1px 6px; }

.row {
    display: grid; grid-template-columns: 30px 1fr auto; align-items: center;
    gap: 13px; padding: 11px 4px; border-bottom: 1px solid var(--line);
}

.badge {
    width: 28px; height: 28px; border-radius: 5px; border: 1px solid var(--line);
    display: flex; align-items: center; justify-content: center;
    font-family: 'Archivo'; font-weight: 600; font-size: 11px; color: var(--ink-soft); flex: 0 0 28px;
}

.who a, .who { color: inherit; text-decoration: none; display: block; }
.name { font-family: 'Inter'; font-weight: 600; font-size: 14px; color: var(--ink); }
.role { font-size: 12px; color: var(--ink-soft); margin-top: 1px; }

.phone { display: flex; align-items: center; gap: 8px; justify-content: flex-end; }
.phone svg { width: 14px; height: 14px; stroke: var(--muted); flex: 0 0 14px; }
.phone a { color: var(--ink); text-decoration: none; font-family: 'Inter'; font-weight: 600; font-size: 16.5px; font-variant-numeric: tabular-nums; letter-spacing: -0.008em; white-space: nowrap; }
.phone .muted { color: var(--muted); font-weight: 400; font-size: 13px; }

.empty-state { text-align: center; padding: 60px 20px; color: var(--ink-soft); }
.empty-state-icon { font-size: 40px; margin-bottom: 12px; opacity: .6; }
.empty-state-text { font-size: 14px; }

/* --- Pagina dettaglio contatto --- */

.topbar {
    display: flex; align-items: center; justify-content: space-between;
    padding: 16px 32px; border-bottom: 1px solid var(--line);
}
.topbar-brand { font-family: 'Archivo'; font-weight: 700; font-size: 16px; text-decoration: none; color: var(--ink); }
.topbar-brand span { color: var(--accent); }
.topbar a.back { font-size: 13px; color: var(--ink-soft); text-decoration: none; }

.detail-page { max-width: 560px; margin: 32px auto; padding: 0 20px; }
.detail { background: var(--paper-raised); border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.detail-head { padding: 22px 22px 18px; border-bottom: 1px solid var(--line); }
.detail-badge { width: 44px; height: 44px; border-radius: 7px; border: 1px solid var(--line); display: flex; align-items: center; justify-content: center; font-family: 'Archivo'; font-weight: 600; font-size: 15px; color: var(--ink-soft); margin-bottom: 12px; }
.detail-name { font-family: 'Archivo'; font-weight: 700; font-size: 19px; letter-spacing: -0.01em; }
.detail-role { font-size: 13px; color: var(--ink-soft); margin-top: 2px; }
.detail-dept { display: inline-flex; align-items: center; gap: 5px; font-size: 11.5px; color: var(--accent); background: var(--accent-tint); border-radius: 5px; padding: 2px 8px; margin-top: 10px; }
.detail-body { padding: 16px 22px 22px; }
.field { display: flex; align-items: flex-start; gap: 11px; padding: 10px 0; border-bottom: 1px solid var(--line); }
.field:last-child { border-bottom: none; }
.field svg { width: 15px; height: 15px; stroke: var(--muted); flex: 0 0 15px; margin-top: 2px; }
.field-label { font-size: 10.5px; color: var(--muted); }
.field-value { font-size: 14.5px; color: var(--ink); font-weight: 500; margin-top: 1px; word-break: break-word; }
.detail-actions { display: flex; gap: 8px; margin-top: 18px; }

/* --- Bottoni (condivisi pagine pubbliche + admin) --- */

.btn { display: inline-flex; align-items: center; gap: 7px; border-radius: 7px; font-size: 13px; font-weight: 600; padding: 8px 14px; border: 1px solid transparent; cursor: pointer; text-decoration: none; font-family: 'Inter'; }
.btn svg { width: 13px; height: 13px; }
.btn-primary { background: var(--ink); color: var(--paper-raised); flex: 1; text-align: center; justify-content: center; }
.btn-ghost { background: none; border: 1px solid var(--line); color: var(--ink-soft); flex: 1; text-align: center; justify-content: center; }
.btn-danger { background: none; color: var(--danger); padding: 6px 10px; font-size: 12.5px; border: none; cursor: pointer; }

/* --- Admin --- */

.page-title { font-family: 'Archivo'; font-weight: 700; font-size: 20px; letter-spacing: -0.01em; margin: 0 0 22px; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 18px; margin-bottom: 28px; }
.card { background: var(--paper-raised); border: 1px solid var(--line); border-radius: 10px; padding: 18px 20px; }
.card h3 { font-family: 'Archivo'; font-size: 13px; font-weight: 600; margin: 0 0 4px; color: var(--ink-soft); }
.sync-time { font-family: 'Inter'; font-size: 22px; font-weight: 600; font-variant-numeric: tabular-nums; margin: 6px 0 14px; }
.sync-time small { display: block; font-size: 11.5px; color: var(--muted); font-weight: 400; margin-top: 2px; }

.field-row { display: flex; gap: 8px; margin-top: 10px; flex-wrap: wrap; }
.input { flex: 1; border: 1px solid var(--line); background: var(--paper); border-radius: 7px; padding: 8px 10px; font-size: 13.5px; color: var(--ink); font-family: 'Inter'; min-width: 120px; }
.input::placeholder { color: var(--muted); }
.helptext { font-size: 11px; color: var(--muted); margin-top: 6px; }

.section-head { display: flex; align-items: baseline; justify-content: space-between; border-bottom: 1px solid var(--ink); padding-bottom: 8px; margin-bottom: 2px; }
.section-head h3 { font-family: 'Archivo'; font-size: 14.5px; font-weight: 600; margin: 0; }

table { width: 100%; border-collapse: collapse; margin-top: 4px; }
th { text-align: left; font-size: 11px; color: var(--muted); font-weight: 500; padding: 10px 10px 8px; border-bottom: 1px solid var(--line); }
td { padding: 11px 10px; border-bottom: 1px solid var(--line); font-size: 13.5px; }
td.num { font-variant-numeric: tabular-nums; font-weight: 600; }
.row-actions { display: flex; gap: 4px; justify-content: flex-end; }
.row-actions .btn-ghost { padding: 5px 10px; font-size: 12px; flex: none; }

.add-group-form { display: none; background: var(--accent-tint); border: 1px solid var(--line); border-radius: 8px; padding: 14px 16px; margin: 12px 0; }
.add-group-form.open { display: block; }
.add-group-form .btn-primary { flex: none; }

.modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,.4); display: flex; align-items: center; justify-content: center; padding: 20px; }
.modal-overlay[hidden] { display: none; }
.modal-box { background: var(--paper-raised); border-radius: 10px; padding: 20px; max-width: 560px; width: 100%; max-height: 80vh; overflow-y: auto; }
.modal-box-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px; }
.modal-box-head button { background: none; border: none; color: var(--muted); font-size: 18px; cursor: pointer; }

/* --- Login --- */

.login-shell { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 20px; }
.login-card { background: var(--paper-raised); border: 1px solid var(--line); border-radius: 10px; padding: 32px; width: 100%; max-width: 360px; }
.login-title { font-family: 'Archivo'; font-weight: 700; font-size: 18px; text-align: center; margin: 0 0 24px; }
.login-field { margin-bottom: 14px; }
.login-field label { display: block; font-size: 12px; color: var(--ink-soft); margin-bottom: 5px; }

/* --- Responsive --- */

@media (max-width: 768px) {
    .shell { flex-direction: column; }
    .rail { width: auto; flex: none; flex-direction: row; overflow-x: auto; padding: 12px 0; border-right: none; border-bottom: 1px solid var(--line); align-items: center; }
    .rail-brand { padding: 0 14px; flex: 0 0 auto; }
    .rail-group { display: none; }
    .rail-item { flex: 0 0 auto; padding: 6px 12px; border-left: none; border-bottom: 2px solid transparent; white-space: nowrap; }
    .rail-item.active { border-left: none; border-bottom-color: var(--accent); }
    .rail-foot { display: none; }
    .main { padding: 18px 16px 40px; max-width: none; }
    .badge { width: 26px; height: 26px; flex: 0 0 26px; }
    .grid2 { grid-template-columns: 1fr; }
    .detail-page { margin: 16px auto; }
}
```

- [ ] **Step 2: Verifica visivamente (nessun test automatico per CSS)**

Run: `docker compose up -d --build` poi apri `http://localhost:<SERVER_PORT>/`
— la pagina esisterà ancora con il vecchio markup finché i Task 7-10 non lo
aggiornano; questo step serve solo a controllare che il file CSS non abbia
errori di sintassi (nessun crash del browser sul parsing, F12 console vuota
da errori CSS).

- [ ] **Step 3: Commit**

```bash
git add web/static/css/style.css
git commit -m "feat(ui): rewrite design system (ink/paper/verdigris, dark mode, Archivo+Inter)"
```

---

### Task 6: Handler — Area/Groups in `main.go`

**Files:**
- Modify: `cmd/server/main.go`

**Interfaces:**
- Consumes: `db.CountByArea()` (Task 1), `phonebook.GroupByDepartment` (Task 3), `i18n` chiavi (Task 4).
- Produces: template data `AreaCounts map[string]int` e `Total int` per `phonebook.html`; template data `Groups []*phonebook.DepartmentGroup` per `search_results.html` (sostituisce `Results`); funzione template `initials`.

- [ ] **Step 1: Aggiungi la funzione template `initials`**

Nel blocco `funcMap` in `main()`, accanto a `"substr"`:

```go
	funcMap := template.FuncMap{
		"substr": func(s string, start, length int) string {
			if start < 0 || start >= len(s) {
				return ""
			}
			end := start + length
			if end > len(s) {
				end = len(s)
			}
			return strings.ToUpper(s[start:end])
		},
		"initials": func(name string) string {
			parts := strings.Fields(name)
			if len(parts) == 0 {
				return ""
			}
			result := strings.ToUpper(string(parts[0][0]))
			if len(parts) > 1 {
				result += strings.ToUpper(string(parts[len(parts)-1][0]))
			}
			return result
		},
	}
```

- [ ] **Step 2: Riscrivi `handleIndex` per passare i conteggi Area**

```go
func handleIndex(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)

	counts, err := db.CountByArea()
	if err != nil {
		log.Printf("[INDEX] Failed to count by area: %v", err)
		counts = map[string]int{}
	}
	total := 0
	for _, n := range counts {
		total += n
	}

	data := map[string]interface{}{
		"Messages":   i18n.GetMessages(locale),
		"Locale":     locale,
		"AreaCounts": counts,
		"Total":      total,
	}
	templates.ExecuteTemplate(w, "phonebook.html", data)
}
```

- [ ] **Step 3: Riscrivi `handleSearch` per filtrare su `Contact.Area` e raggruppare**

Sostituisci l'intero corpo di `handleSearch` (rimuove il blocco di pattern
matching su `cfg.LDAPOUFilters`/DN, che restava comunque rotto per
`group=politica` — `LDAPOUFilters` resta intatto in `config.go` per
CardDAV, qui semplicemente non viene più usato):

```go
func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	groupFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))

	var (
		results []*phonebook.ContactWithGroups
		err     error
	)

	if query == "" {
		results, err = pbService.ListContactsWithGroups(500, 0)
	} else {
		results, err = pbService.SearchContactsWithGroups(query, 50)
	}

	if err != nil {
		log.Printf("[SEARCH] Failed: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if groupFilter == "interni" || groupFilter == "esterni" || groupFilter == "politica" {
		filtered := make([]*phonebook.ContactWithGroups, 0, len(results))
		for _, result := range results {
			if result.Contact.Area == groupFilter {
				filtered = append(filtered, result)
			}
		}
		results = filtered
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Groups":   phonebook.GroupByDepartment(results),
		"Messages": i18n.GetMessages(locale),
	}

	templates.ExecuteTemplate(w, "search_results.html", data)
}
```

- [ ] **Step 4: Verifica che compili**

Run: `CGO_ENABLED=1 go build ./...`
Expected: nessun errore. (I template non sono ancora aggiornati: l'app
compila comunque, i template verranno aggiornati nei Task 7-10 prima di
un test end-to-end.)

- [ ] **Step 5: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat(handlers): wire Area counts and department grouping into index/search"
```

---

### Task 7: Template — vista principale (`phonebook.html` + `search_results.html`)

**Files:**
- Modify: `web/templates/phonebook.html`
- Modify: `web/templates/search_results.html`

**Interfaces:**
- Consumes: `AreaCounts`, `Total` (Task 6, dati per `phonebook.html`); `Groups []*phonebook.DepartmentGroup` (Task 6, dati per `search_results.html`); classi CSS Task 5; func template `initials` (Task 6).

- [ ] **Step 1: Sostituisci `web/templates/phonebook.html`**

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        <nav class="rail">
            <a href="/" class="rail-brand">Ldav<span>Sync</span></a>
            <div class="rail-group">Area</div>
            <button class="rail-item active" data-area=""
                    hx-get="/search" hx-target="#search-results"
                    onclick="setActiveArea(this)">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18"/></svg>
                <span>{{index .Messages "area_all"}}</span><span class="n">{{.Total}}</span>
            </button>
            <button class="rail-item" data-area="interni"
                    hx-get="/search?group=interni" hx-target="#search-results"
                    onclick="setActiveArea(this)">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><circle cx="12" cy="8" r="3"/><path d="M5 20c0-3.9 3.1-7 7-7s7 3.1 7 7"/></svg>
                <span>{{index .Messages "area_interni"}}</span><span class="n">{{index .AreaCounts "interni"}}</span>
            </button>
            <button class="rail-item" data-area="esterni"
                    hx-get="/search?group=esterni" hx-target="#search-results"
                    onclick="setActiveArea(this)">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.2 2.4 3.4 5.6 3.4 9s-1.2 6.6-3.4 9c-2.2-2.4-3.4-5.6-3.4-9s1.2-6.6 3.4-9z"/></svg>
                <span>{{index .Messages "area_esterni"}}</span><span class="n">{{index .AreaCounts "esterni"}}</span>
            </button>
            <button class="rail-item" data-area="politica"
                    hx-get="/search?group=politica" hx-target="#search-results"
                    onclick="setActiveArea(this)">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><path d="M4 21V10l8-6 8 6v11"/><path d="M9 21v-6h6v6"/></svg>
                <span>{{index .Messages "area_politica"}}</span><span class="n">{{index .AreaCounts "politica"}}</span>
            </button>
            <div class="rail-foot">
                <a href="/login">{{index .Messages "admin_panel"}}</a>
            </div>
        </nav>

        <main class="main">
            <div class="search">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>
                <input type="search" name="q" placeholder="{{index .Messages "search_placeholder"}}"
                       hx-get="/search" hx-trigger="keyup changed delay:500ms"
                       hx-target="#search-results" hx-swap="innerHTML">
            </div>

            <div class="prefix-helper" id="prefix-helper">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
                <span>{{index .Messages "prefix_helper"}}</span>
                <button type="button" onclick="document.getElementById('prefix-helper').hidden = true; try { localStorage.setItem('ldavsync-prefix-helper-dismissed', '1'); } catch(e) {}" aria-label="Chiudi">&times;</button>
            </div>

            <div id="search-results" hx-get="/search" hx-trigger="load" hx-target="this" hx-swap="innerHTML">
                <div class="loading-spinner"><p>Caricamento contatti...</p></div>
            </div>
        </main>
    </div>

    <script>
        function setActiveArea(btn) {
            document.querySelectorAll('.rail-item').forEach(function (i) { i.classList.remove('active'); });
            btn.classList.add('active');
        }
        try {
            if (localStorage.getItem('ldavsync-prefix-helper-dismissed') === '1') {
                document.getElementById('prefix-helper').hidden = true;
            }
        } catch (e) {}
    </script>
</body>
</html>
```

Nota sul valore `%s` in `prefix_helper`: nello spec il prefisso mostrato è
lo stesso `PRIMARY_NUMBER_PREFIX_TEMPLATE` configurato (`0854481{ext}` →
prefisso `0854481`); per questa iterazione il testo resta statico
("Da fuori l'ente...") senza interpolare il prefisso reale — è annotato
come possibile follow-up, non blocca la vista.

- [ ] **Step 2: Sostituisci `web/templates/search_results.html`**

```html
{{if .Groups}}
{{range .Groups}}
<details class="dept" open>
    <summary class="dept-head">
        <span class="dept-name">{{.Name}}</span>
        <span class="dept-count">{{len .Contacts}}</span>
    </summary>
    {{range .Contacts}}
    <div class="row">
        <div class="badge">{{initials .Contact.DisplayName}}</div>
        <a class="who" href="/contacts/{{.Contact.UID}}">
            <div class="name">{{.Contact.DisplayName}}</div>
            {{if .Contact.Description}}<div class="role">{{.Contact.Description}}</div>{{end}}
        </a>
        <div class="phone">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 5c0-1 1-1.5 1.8-1.2l2.4 1c.6.2 1 .8 1 1.4v2c0 .5-.2 1-.6 1.3l-1 .9c1 2.3 2.8 4.1 5.1 5.1l.9-1c.3-.4.8-.6 1.3-.6h2c.6 0 1.2.4 1.4 1l1 2.4c.3.8-.2 1.8-1.2 1.8-8 0-14-6-14-14z"/></svg>
            {{if .Contact.PrimaryNumber}}
            <a href="tel:{{.Contact.PrimaryNumber}}">{{if .Contact.LDAPExt}}{{.Contact.LDAPExt}}{{else}}{{.Contact.PrimaryNumber}}{{end}}</a>
            {{else if .Contact.Email}}
            <a class="muted" href="mailto:{{.Contact.Email}}">{{.Contact.Email}}</a>
            {{else}}
            <span class="muted">&mdash;</span>
            {{end}}
        </div>
    </div>
    {{end}}
</details>
{{end}}
{{else}}
<div class="empty-state">
    <div class="empty-state-icon">&#128269;</div>
    <div class="empty-state-text">{{index .Messages "no_results"}}</div>
</div>
{{end}}
```

Nota: `<details open>` invece di un `<div>` statico — collassabile nativo,
nessun JS. I reparti senza contatti che matchano la ricerca non vengono
neppure resi (il grouping avviene sui risultati già filtrati da
`SearchContacts`), quindi "espandi solo i reparti con match" è già
soddisfatto dal filtro server-side, senza altra logica da scrivere.

- [ ] **Step 3: Verifica end-to-end con Docker**

Run: `docker compose up -d --build` poi `curl -s http://localhost:$SERVER_PORT/search | head -c 800` (usa la porta da `.env`).
Expected: HTML con `dept-head`/`row`, non più `contacts-table`; nessun
errore nei log (`docker compose logs --tail=30 ldavsync`).

- [ ] **Step 4: Commit**

```bash
git add web/templates/phonebook.html web/templates/search_results.html
git commit -m "feat(ui): grouped department view + prefix helper for main list"
```

---

### Task 8: Template — dettaglio contatto (`contact_detail.html`)

**Files:**
- Modify: `web/templates/contact_detail.html`

**Interfaces:**
- Consumes: `.Contact` (`database.Contact`), `.Groups` (`[]*database.GroupNumber`), classi `.topbar`/`.detail`/`.field`/`.btn` (Task 5), func `initials` (Task 6).

- [ ] **Step 1: Sostituisci il contenuto del file**

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Contact.DisplayName}} - {{index .Messages "app_title"}}</title>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="topbar">
        <a href="/" class="topbar-brand">Ldav<span>Sync</span></a>
        <a href="/" class="back">&larr; {{index .Messages "contacts"}}</a>
    </div>

    <div class="detail-page">
        <div class="detail">
            <div class="detail-head">
                <div class="detail-badge">{{initials .Contact.DisplayName}}</div>
                <div class="detail-name">{{.Contact.DisplayName}}</div>
                {{if .Contact.Description}}<div class="detail-role">{{.Contact.Description}}</div>{{end}}
                {{if .Contact.Department}}
                <div class="detail-dept">
                    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="4" width="18" height="16" rx="2"/></svg>
                    {{.Contact.Department}}
                </div>
                {{end}}
            </div>
            <div class="detail-body">
                {{if .Contact.LDAPExt}}
                <div class="field">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 5c0-1 1-1.5 1.8-1.2l2.4 1c.6.2 1 .8 1 1.4v2c0 .5-.2 1-.6 1.3l-1 .9c1 2.3 2.8 4.1 5.1 5.1l.9-1c.3-.4.8-.6 1.3-.6h2c.6 0 1.2.4 1.4 1l1 2.4c.3.8-.2 1.8-1.2 1.8-8 0-14-6-14-14z"/></svg>
                    <div><div class="field-label">{{index .Messages "extension"}}</div><div class="field-value">{{.Contact.LDAPExt}}</div></div>
                </div>
                {{end}}
                {{if .Contact.Email}}
                <div class="field">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 6l8 6 8-6M4 6h16v12H4V6z"/></svg>
                    <div><div class="field-label">{{index .Messages "email"}}</div><div class="field-value">{{.Contact.Email}}</div></div>
                </div>
                {{end}}
                {{range .Groups}}
                <div class="field">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0"/></svg>
                    <div><div class="field-label">{{.Name}}</div><div class="field-value">{{.Number}}</div></div>
                </div>
                {{end}}
                <div class="detail-actions">
                    {{if .Contact.PrimaryNumber}}<a href="tel:{{.Contact.PrimaryNumber}}" class="btn btn-primary">{{index .Messages "call_extension"}}</a>{{end}}
                    <a href="/contacts/{{.Contact.UID}}/export" class="btn btn-ghost">{{index .Messages "export_vcard"}}</a>
                </div>
            </div>
        </div>
    </div>
</body>
</html>
```

- [ ] **Step 2: Verifica end-to-end**

Run: `docker compose up -d --build` poi apri `http://localhost:$SERVER_PORT/contacts/<uid-reale>` (un uid dalla vista principale).
Expected: pagina coerente con lo stile del resto dell'app, nessun caricamento di `cdn.tailwindcss.com`.

- [ ] **Step 3: Commit**

```bash
git add web/templates/contact_detail.html
git commit -m "feat(ui): restyle contact detail page, drop Tailwind CDN"
```

---

### Task 9: Template — login (`login.html`)

**Files:**
- Modify: `web/templates/login.html`

**Interfaces:**
- Consumes: classi `.login-shell`/`.login-card`/`.input`/`.btn-primary` (Task 5).

- [ ] **Step 1: Sostituisci il contenuto del file**

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{index .Messages "login"}} - {{index .Messages "app_title"}}</title>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="login-shell">
        <div class="login-card">
            <h1 class="login-title">{{index .Messages "app_title"}}</h1>
            <form method="POST" action="/login">
                <div class="login-field">
                    <label for="username">{{index .Messages "username"}}</label>
                    <input class="input" style="width:100%;" id="username" name="username" type="text" required autofocus>
                </div>
                <div class="login-field">
                    <label for="password">{{index .Messages "password"}}</label>
                    <input class="input" style="width:100%;" id="password" name="password" type="password" required>
                </div>
                <button class="btn btn-primary" style="width:100%;" type="submit">{{index .Messages "login"}}</button>
            </form>
        </div>
    </div>
</body>
</html>
```

- [ ] **Step 2: Verifica end-to-end**

Run: `docker compose up -d --build` poi apri `http://localhost:$SERVER_PORT/login`.
Expected: form coerente con il design system, nessun Tailwind CDN.

- [ ] **Step 3: Commit**

```bash
git add web/templates/login.html
git commit -m "feat(ui): restyle login page, drop Tailwind CDN"
```

---

### Task 10: Template — admin (`admin.html`, `admin_groups.html`, `admin_group_members.html`)

**Files:**
- Modify: `web/templates/admin.html`
- Modify: `web/templates/admin_groups.html`
- Modify: `web/templates/admin_group_members.html`

**Interfaces:**
- Consumes: `.Username`, `.LastSync` (esistenti, da `handleAdminDashboard`); `.Groups` (`[]*phonebook.GroupWithMembers`, da `handleAdminListGroups`); `.Group`/`.Members` (da `handleAdminGroupMembers`); classi `.rail`/`.card`/`.grid2`/`table`/`.btn-danger` (Task 5).

- [ ] **Step 1: Sostituisci `web/templates/admin.html`**

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{index .Messages "admin_panel"}} - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        <nav class="rail">
            <a href="/" class="rail-brand">Ldav<span>Sync</span> <span style="color:var(--ink-soft);font-weight:400;font-size:12px;">/ admin</span></a>
            <div class="rail-group">Gestione</div>
            <div class="rail-item active">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><rect x="3" y="4" width="7" height="7" rx="1"/><rect x="14" y="4" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></svg>
                <span>{{index .Messages "admin_panel"}}</span>
            </div>
            <a href="/" class="rail-item">
                <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18"/></svg>
                <span>{{index .Messages "contacts"}}</span>
            </a>
            <div class="rail-foot">
                {{.Username}}<br>
                <form method="POST" action="/logout" style="margin:0;">
                    <button type="submit" style="background:none;border:none;padding:0;color:var(--danger);font-size:12px;cursor:pointer;">{{index .Messages "logout"}}</button>
                </form>
            </div>
        </nav>

        <main class="main" style="max-width:920px;">
            <h1 class="page-title">{{index .Messages "admin_panel"}}</h1>

            <div class="grid2">
                <div class="card">
                    <h3>{{index .Messages "last_sync"}}</h3>
                    <div class="sync-time">{{.LastSync}}</div>
                    <button hx-post="/admin/sync" hx-swap="none" class="btn btn-primary" style="flex:none;">
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.5 9a9 9 0 0114.5-4.5L23 10M1 14l5 4.5A9 9 0 0020.5 15"/></svg>
                        {{index .Messages "sync_now"}}
                    </button>
                </div>
                <div class="card">
                    <h3>{{index .Messages "primary_number_prefix"}}</h3>
                    <form hx-post="/admin/config" hx-swap="none" class="field-row">
                        <input type="text" name="primary_number_prefix" placeholder="0854321{ext}" class="input">
                        <button type="submit" class="btn btn-ghost" style="flex:none;">{{index .Messages "save"}}</button>
                    </form>
                </div>
            </div>

            <div id="groups-content" hx-get="/admin/groups" hx-trigger="load" hx-swap="innerHTML">
                <p class="helptext">Caricamento...</p>
            </div>
        </main>
    </div>
</body>
</html>
```

- [ ] **Step 2: Sostituisci `web/templates/admin_groups.html`**

```html
<div class="section-head">
    <h3>{{index .Messages "group_numbers"}}</h3>
    <button class="btn btn-ghost" style="flex:none;" onclick="document.getElementById('addform').classList.toggle('open')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 5v14M5 12h14"/></svg>
        {{index .Messages "add_group"}}
    </button>
</div>

<div class="add-group-form" id="addform">
    <form hx-post="/admin/groups" hx-target="#groups-content" hx-swap="outerHTML">
        <div class="field-row">
            <input type="text" name="number" placeholder="{{index .Messages "number"}}" required class="input">
            <input type="text" name="name" placeholder="{{index .Messages "name"}}" required class="input">
            <input type="text" name="description" placeholder="{{index .Messages "description"}}" class="input">
        </div>
        <button type="submit" class="btn btn-primary" style="flex:none;">{{index .Messages "save"}}</button>
    </form>
</div>

<table>
    <thead>
        <tr>
            <th>{{index .Messages "number"}}</th>
            <th>{{index .Messages "name"}}</th>
            <th>{{index .Messages "description"}}</th>
            <th>{{index .Messages "members"}}</th>
            <th></th>
        </tr>
    </thead>
    <tbody>
        {{range .Groups}}
        <tr>
            <td class="num">{{.Group.Number}}</td>
            <td>{{.Group.Name}}</td>
            <td class="member-count">{{if .Group.Description}}{{.Group.Description}}{{else}}&mdash;{{end}}</td>
            <td class="member-count">{{len .Members}}</td>
            <td>
                <div class="row-actions">
                    <button hx-get="/admin/groups/{{.Group.ID}}/members" hx-target="#modal-content"
                            onclick="document.getElementById('modal').removeAttribute('hidden')"
                            class="btn btn-ghost">{{index $.Messages "members"}}</button>
                    <button hx-post="/admin/groups/{{.Group.ID}}/delete" hx-confirm="Sei sicuro?"
                            hx-target="#groups-content" hx-swap="outerHTML"
                            class="btn-danger">{{index $.Messages "delete"}}</button>
                </div>
            </td>
        </tr>
        {{end}}
    </tbody>
</table>

<div id="modal" class="modal-overlay" hidden>
    <div class="modal-box">
        <div class="modal-box-head">
            <h3>{{index .Messages "members"}}</h3>
            <button onclick="document.getElementById('modal').setAttribute('hidden','')">&times;</button>
        </div>
        <div id="modal-content"></div>
    </div>
</div>
```

- [ ] **Step 3: Sostituisci `web/templates/admin_group_members.html`**

```html
<div>
    <h3 class="page-title" style="font-size:15px;margin-bottom:14px;">{{.Group.Number}} &mdash; {{.Group.Name}}</h3>

    <div style="margin-bottom:18px;">
        <div class="helptext" style="margin-bottom:6px;">{{index .Messages "add_member"}}</div>
        <form hx-post="/admin/groups/{{.Group.ID}}/members" hx-target="#members-list" hx-swap="outerHTML" class="field-row">
            <input type="number" name="contact_id" placeholder="Contact ID" required class="input">
            <button type="submit" class="btn btn-primary" style="flex:none;">{{index .Messages "add_member"}}</button>
        </form>
    </div>

    <div id="members-list">
        {{if .Members}}
        <table>
            <thead>
                <tr>
                    <th>{{index .Messages "name"}}</th>
                    <th>{{index .Messages "email"}}</th>
                    <th>{{index .Messages "phone"}}</th>
                    <th></th>
                </tr>
            </thead>
            <tbody>
                {{range .Members}}
                <tr>
                    <td>{{.DisplayName}}</td>
                    <td class="member-count">{{.Email}}</td>
                    <td class="member-count">{{.PrimaryNumber}}</td>
                    <td>
                        <button hx-post="/admin/groups/{{$.Group.ID}}/members/{{.ID}}/delete"
                                hx-target="#members-list" hx-swap="outerHTML" class="btn-danger">
                            {{index $.Messages "delete"}}
                        </button>
                    </td>
                </tr>
                {{end}}
            </tbody>
        </table>
        {{else}}
        <p class="helptext" style="text-align:center;padding:16px 0;">Nessun membro</p>
        {{end}}
    </div>
</div>
```

- [ ] **Step 4: Verifica end-to-end**

Run: `docker compose up -d --build`, login su `http://localhost:$SERVER_PORT/login` con credenziali admin LDAP, poi apri `/admin`.
Expected: dashboard con card scure/chiare coerenti, tabella etichette
numero funzionante (crea/elimina/membri), nessun Tailwind CDN caricato.

- [ ] **Step 5: Commit**

```bash
git add web/templates/admin.html web/templates/admin_groups.html web/templates/admin_group_members.html
git commit -m "feat(ui): restyle admin panel, drop Tailwind CDN"
```

---

## Verifica finale

- [ ] `CGO_ENABLED=1 go vet ./...` — nessun warning.
- [ ] `CGO_ENABLED=1 go test ./... -v` — tutti i test passano (Task 1-3).
- [ ] `docker compose up -d --build` — container parte, `/health` risponde 200.
- [ ] Verifica manuale: vista principale raggruppata per reparto, contatori
  sidebar reali (non più "..."), helper prefisso dismissibile, dettaglio
  contatto/login/admin coerenti nello stesso stile, dark mode del sistema
  operativo cambia la palette su tutte le pagine.
