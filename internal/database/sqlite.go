package database

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	*sql.DB
}

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
	Source         string // "ldap" (sincronizzato, default) o "manual" (creato da admin)
	Disabled       bool   // true = account AD disabilitato: mai in rubrica pubblica (zero value = false = attivo, così ogni altro punto che costruisce un Contact senza impostarlo resta corretto), solo per il matching nome centralino/dominio in /admin/pbx
	ManualOverride bool
	DeletedAt      *time.Time
	LastSync       time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Area rappresenta un'area organizzativa (Interni/Esterni/Politica di
// default, ma editabile: l'admin può crearne/rinominarne/eliminarne).
type Area struct {
	ID        int64
	Key       string
	Name      string
	// RangeStart/RangeEnd (opzionali, nil = nessuna regola) definiscono una
	// regola "per interno": un contatto/gruppo senza area già assegnata
	// (da OU mapping o esplicitamente) il cui interno numerico cade in
	// [RangeStart, RangeEnd] viene assegnato a quest'area — e la prende
	// anche come reparto/nome gruppo, per non restare senza etichetta.
	RangeStart *int
	RangeEnd   *int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type GroupNumber struct {
	ID           int64
	Number       string
	Name         string
	Description  string
	Source       string // "manual" (default) o "pbx"
	NameOverride bool   // se true, il sync PBX non sovrascrive più Name
	Area         string // area assegnata da una regola per range interno (vedi Area.RangeStart/RangeEnd), "" se nessuna
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GroupCategory raggruppa i gruppi di chiamata (group_numbers) in
// sezioni gerarchiche a display-time (nessuna colonna su group_numbers) —
// vedi docs/superpowers/specs/2026-09-15-group-categories-hierarchy-design.md.
// ParentID nil = categoria di primo livello (es. "Uffici"); non-nil =
// annidata sotto un'altra categoria (es. "Settore VI - Legale" dentro
// "Uffici"). RangeStart/RangeEnd nil = nessuna regola (categoria mai
// auto-assegnata, solo organizzativa se in futuro serve raggruppare a
// mano — oggi il matching è sempre su range).
type GroupCategory struct {
	ID         int64
	Key        string
	Name       string
	ParentID   *int64
	RangeStart *int
	RangeEnd   *int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type GroupMember struct {
	GroupID   int64
	ContactID int64
	CreatedAt time.Time
}

type AppConfig struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

func InitDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Enable WAL mode for better concurrency
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	dbWrapper := &DB{db}

	if err := dbWrapper.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	log.Printf("[DATABASE] Initialized at %s", path)
	return dbWrapper, nil
}

func (db *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS contacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		uid TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL,
		email TEXT,
		ldap_ext TEXT,
		primary_number TEXT,
		department TEXT,
		title TEXT,
		description TEXT,
		ldap_groups TEXT,
		ldap_dn TEXT,
		manual_override INTEGER DEFAULT 0,
		deleted_at DATETIME,
		last_sync DATETIME NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_contacts_uid ON contacts(uid);
	CREATE INDEX IF NOT EXISTS idx_contacts_last_sync ON contacts(last_sync);
	CREATE INDEX IF NOT EXISTS idx_contacts_deleted_at ON contacts(deleted_at);

	CREATE TABLE IF NOT EXISTS group_numbers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		number TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		description TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_group_numbers_number ON group_numbers(number);

	CREATE TABLE IF NOT EXISTS group_members (
		group_id INTEGER NOT NULL,
		contact_id INTEGER NOT NULL,
		created_at DATETIME NOT NULL,
		PRIMARY KEY (group_id, contact_id),
		FOREIGN KEY (group_id) REFERENCES group_numbers(id) ON DELETE CASCADE,
		FOREIGN KEY (contact_id) REFERENCES contacts(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_group_members_group_id ON group_members(group_id);
	CREATE INDEX IF NOT EXISTS idx_group_members_contact_id ON group_members(contact_id);

	CREATE TABLE IF NOT EXISTS app_config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS areas (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS group_categories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL DEFAULT '',
		parent_id INTEGER REFERENCES group_categories(id),
		range_start INTEGER,
		range_end INTEGER,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
	`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}

	// Add new columns if they don't exist (migration)
	alterStatements := []string{
		"ALTER TABLE contacts ADD COLUMN title TEXT",
		"ALTER TABLE contacts ADD COLUMN description TEXT",
		"ALTER TABLE contacts ADD COLUMN area TEXT",
		"ALTER TABLE contacts ADD COLUMN source TEXT DEFAULT 'ldap'",
		"ALTER TABLE contacts ADD COLUMN disabled INTEGER DEFAULT 0",
		"ALTER TABLE group_numbers ADD COLUMN source TEXT DEFAULT 'manual'",
		"ALTER TABLE group_numbers ADD COLUMN name_override INTEGER DEFAULT 0",
		"ALTER TABLE group_numbers ADD COLUMN area TEXT DEFAULT ''",
		"ALTER TABLE areas ADD COLUMN range_start INTEGER",
		"ALTER TABLE areas ADD COLUMN range_end INTEGER",
	}

	for _, stmt := range alterStatements {
		if _, err := db.Exec(stmt); err != nil {
			// Ignore "duplicate column name" errors (SQLite error: "duplicate column name")
			if !strings.Contains(err.Error(), "duplicate column name") {
				log.Printf("[DATABASE] Warning during migration: %v", err)
			}
		}
	}

	// Seed delle 3 aree storiche, solo se la tabella è vuota (prima
	// installazione o DB precedente all'introduzione del CRUD aree).
	// Da qui in poi le aree sono dati editabili da admin, non più fisse.
	var areaCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM areas`).Scan(&areaCount); err == nil && areaCount == 0 {
		now := time.Now()
		defaults := []struct{ key, name string }{
			{"interni", "Interni"},
			{"esterni", "Esterni"},
			{"politica", "Politica"},
		}
		for _, d := range defaults {
			if _, err := db.Exec(`INSERT INTO areas (key, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, d.key, d.name, now, now); err != nil {
				log.Printf("[DATABASE] Warning seeding default area %q: %v", d.key, err)
			}
		}
	}

	// Area "Uffici": non un'area OU-mappata come le altre, ma il posto dove
	// vivono le chiamate di gruppo nella rubrica pubblica (vedi handleSearch
	// in cmd/server) — key riservata "uffici", creata idempotentemente
	// (indipendente dal seed dei default sopra, che parte solo a tabella
	// vuota) così compare nel filtro Area anche su un DB già esistente.
	var ufficiCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM areas WHERE key = 'uffici'`).Scan(&ufficiCount); err == nil && ufficiCount == 0 {
		now := time.Now()
		if _, err := db.Exec(`INSERT INTO areas (key, name, created_at, updated_at) VALUES ('uffici', 'Uffici', ?, ?)`, now, now); err != nil {
			log.Printf("[DATABASE] Warning seeding area 'uffici': %v", err)
		}
	}

	return nil
}

// Contact operations

func (db *DB) UpsertContact(contact *Contact) error {
	now := time.Now()
	contact.UpdatedAt = now

	query := `
	INSERT INTO contacts (uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, disabled, manual_override, deleted_at, last_sync, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		disabled = excluded.disabled,
		deleted_at = NULL,
		last_sync = excluded.last_sync,
		updated_at = excluded.updated_at
	`

	if contact.CreatedAt.IsZero() {
		contact.CreatedAt = now
	}

	result, err := db.Exec(query, contact.UID, contact.DisplayName, contact.Email, contact.LDAPExt,
		contact.PrimaryNumber, contact.Department, contact.Title, contact.Description, contact.LDAPGroups, contact.LDAPDN, contact.Area, contact.Disabled, contact.ManualOverride, contact.DeletedAt,
		contact.LastSync, contact.CreatedAt, contact.UpdatedAt)

	if err != nil {
		return fmt.Errorf("failed to upsert contact: %w", err)
	}

	if contact.ID == 0 {
		id, _ := result.LastInsertId()
		contact.ID = id
	}

	return nil
}

func (db *DB) GetContact(uid string) (*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE uid = ? AND deleted_at IS NULL AND disabled = 0
	`

	contact := &Contact{}
	err := db.QueryRow(query, uid).Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
		&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
		&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact: %w", err)
	}

	return contact, nil
}

func (db *DB) SearchContacts(query string, limit int) ([]*Contact, error) {
	searchQuery := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL AND disabled = 0
	AND (email IS NOT NULL AND email != '' OR ldap_ext IS NOT NULL AND ldap_ext != '' OR primary_number IS NOT NULL AND primary_number != '')
	AND (display_name LIKE ? OR email LIKE ? OR ldap_ext LIKE ? OR primary_number LIKE ? OR department LIKE ? OR description LIKE ?)
	ORDER BY display_name
	LIMIT ?
	`

	pattern := "%" + query + "%"
	rows, err := db.Query(searchQuery, pattern, pattern, pattern, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search contacts: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}

	return contacts, nil
}

func (db *DB) ListContacts(limit, offset int) ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL AND disabled = 0
	AND (email IS NOT NULL AND email != '' OR ldap_ext IS NOT NULL AND ldap_ext != '' OR primary_number IS NOT NULL AND primary_number != '')
	ORDER BY display_name
	LIMIT ? OFFSET ?
	`

	rows, err := db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list contacts: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}

	return contacts, nil
}

// ListAllContacts returns all contacts without altri filtri di ricerca
// (per CardDAV) — esclude comunque i soft-deleted e i disabled (mai in
// rubrica pubblica, CardDAV incluso).
func (db *DB) ListAllContacts(limit, offset int) ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL AND disabled = 0
	ORDER BY display_name
	LIMIT ? OFFSET ?
	`

	rows, err := db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list all contacts: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}

	return contacts, nil
}

func (db *DB) SoftDeleteContact(uid string) error {
	query := `UPDATE contacts SET deleted_at = ?, updated_at = ? WHERE uid = ?`
	now := time.Now()
	_, err := db.Exec(query, now, now, uid)
	return err
}

func (db *DB) UpdateContactOverride(uid string, email, primaryNumber string) error {
	query := `
	UPDATE contacts
	SET email = ?, primary_number = ?, manual_override = 1, updated_at = ?
	WHERE uid = ?
	`
	_, err := db.Exec(query, email, primaryNumber, time.Now(), uid)
	return err
}

// CountByArea returns the number of active (non-deleted) contacts per
// Area value ("interni"/"esterni"/"politica"). Contacts with no area yet
// assigned (not re-synced since this field was added) are counted under
// the empty string key. Applica la stessa condizione "ha almeno un
// recapito" di ListContacts/SearchContacts — altrimenti il conteggio in
// sidebar non combacia con quanti contatti si vedono davvero (un
// consigliere ex-mandato senza email/telefono/interno in AD non compare
// mai in lista, ma veniva comunque contato).
func (db *DB) CountByArea() (map[string]int, error) {
	query := `
	SELECT COALESCE(area, '') AS area, COUNT(*)
	FROM contacts
	WHERE deleted_at IS NULL AND disabled = 0
	AND (email IS NOT NULL AND email != '' OR ldap_ext IS NOT NULL AND ldap_ext != '' OR primary_number IS NOT NULL AND primary_number != '')
	GROUP BY area
	`
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

// SoftDeleteStale marca come cancellato (deleted_at) qualsiasi contatto
// attivo di origine LDAP il cui last_sync sia precedente a syncTime —
// cioè non è stato toccato dal giro di sync corrente (disabilitato,
// spostato, rimosso da AD). I contatti source='manual' non sono mai
// toccati: non passano mai da un sync, quindi il loro last_sync
// resterebbe sempre "vecchio" e finirebbero cancellati al primo giro
// successivo alla creazione se non fossero esclusi qui. Ritorna quanti
// sono stati appena soft-deleted.
func (db *DB) SoftDeleteStale(syncTime time.Time) (int64, error) {
	result, err := db.Exec(
		`UPDATE contacts SET deleted_at = ?, updated_at = ? WHERE deleted_at IS NULL AND source = 'ldap' AND last_sync < ?`,
		syncTime, syncTime, syncTime,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to soft-delete stale contacts: %w", err)
	}
	return result.RowsAffected()
}

// PBX operations (source='pbx': peers SIP del centralino non presenti nel
// dominio LDAP — vedi internal/pbx per l'orchestrazione del sync)

// splitExtensions divide un valore ldap_ext su più interni separati da ";"
// (es. "700;701", un contatto può avere più interni mappati in AD — vedi
// una stessa persona), scartando token vuoti/spazi. Un valore con un solo
// interno torna comunque uno slice di un elemento.
func splitExtensions(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ListDomainExtensions returns the ldap_ext (ogni interno separatamente,
// un contatto può averne più di uno separati da ";") di ogni contatto
// source='ldap' attivo — usato dal sync PBX per escludere i peers già
// coperti da LDAP.
func (db *DB) ListDomainExtensions() ([]string, error) {
	rows, err := db.Query(`SELECT ldap_ext FROM contacts WHERE source = 'ldap' AND ldap_ext IS NOT NULL AND ldap_ext != ''`)
	if err != nil {
		return nil, fmt.Errorf("failed to list domain extensions: %w", err)
	}
	defer rows.Close()

	var exts []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan extension: %w", err)
		}
		exts = append(exts, splitExtensions(raw)...)
	}
	return exts, rows.Err()
}

// GetContactByExtension returns il contatto (qualunque source) il cui
// ldap_ext contiene l'interno dato — usato per risolvere i membri
// "SIP/xxx" di un call group PBX a un contact_id. Il confronto è per token
// (";"-separati), non uguaglianza esatta: un contatto con più interni
// ("700;701") deve risolvere su entrambi. Se l'interno è condiviso da più
// contatti (numero riassegnato senza ripulire il vecchio titolare in AD —
// vedi DuplicateExtension) preferisce quello attivo: "ORDER BY disabled"
// mette prima le righe disabled=0, così il gruppo si aggancia al
// nominativo giusto invece che a un ex titolare disabilitato.
func (db *DB) GetContactByExtension(ext string) (*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL
	AND (';' || ldap_ext || ';') LIKE ('%;' || ? || ';%')
	ORDER BY disabled ASC, id ASC
	LIMIT 1
	`
	contact := &Contact{}
	err := db.QueryRow(query, ext).Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
		&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
		&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get contact by extension: %w", err)
	}
	return contact, nil
}

// ListContactsBySource elenca i contatti attivi di un dato source
// ('ldap'/'pbx'/'manual'), in ordine alfabetico.
func (db *DB) ListContactsBySource(source string) ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE source = ? AND deleted_at IS NULL
	ORDER BY display_name
	`
	rows, err := db.Query(query, source)
	if err != nil {
		return nil, fmt.Errorf("failed to list contacts by source: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		if err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

// ListPBXContacts elenca i contatti source='pbx' attivi (interni sul
// centralino non coperti da nessun contatto source='ldap' con lo stesso
// interno) — usato dall'utility admin che aiuta a scoprire nominativi
// presenti sul centralino ma non ancora censiti nel dominio (es. un
// dipendente con interno telefonico ma senza account AD/LDAP).
func (db *DB) ListPBXContacts() ([]*Contact, error) {
	return db.ListContactsBySource("pbx")
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
		area = CASE WHEN manual_override = 0 THEN excluded.area ELSE area END,
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

// Manual contact operations (source='manual', contatti extra-dominio
// creati da admin, mai toccati dal sync/reconciliation LDAP)

// CreateManualContact inserisce un contatto con source='manual'. UID deve
// già essere univoco (generato dal chiamante, vedi slugify in main.go).
func (db *DB) CreateManualContact(c *Contact) error {
	now := time.Now()
	c.Source = "manual"
	c.LastSync = now
	c.CreatedAt = now
	c.UpdatedAt = now

	result, err := db.Exec(
		// ldap_ext/title/ldap_groups/ldap_dn non hanno senso per un contatto
		// manuale, ma vanno inseriti come stringa vuota (non NULL): lo Scan
		// condiviso con i contatti LDAP usa string semplici, non sql.NullString.
		`INSERT INTO contacts (uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, manual_override, last_sync, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, '', ?, '', '', ?, 'manual', 0, ?, ?, ?)`,
		c.UID, c.DisplayName, c.Email, c.PrimaryNumber, c.Department, c.Description, c.Area, c.LastSync, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create manual contact: %w", err)
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

// UpdateManualContact aggiorna i campi editabili di un contatto manuale.
// Ristretto a source='manual': non deve poter modificare un contatto
// sincronizzato da LDAP (quello passa dall'override esistente).
func (db *DB) UpdateManualContact(c *Contact) error {
	_, err := db.Exec(
		`UPDATE contacts SET display_name = ?, email = ?, primary_number = ?, department = ?, description = ?, area = ?, updated_at = ?
		WHERE uid = ? AND source = 'manual'`,
		c.DisplayName, c.Email, c.PrimaryNumber, c.Department, c.Description, c.Area, time.Now(), c.UID,
	)
	if err != nil {
		return fmt.Errorf("failed to update manual contact: %w", err)
	}
	return nil
}

// DeleteManualContact elimina definitivamente (non soft-delete: è un
// dato gestito a mano, non sincronizzato) un contatto manuale. Ristretto
// a source='manual' per non poter mai cancellare un contatto LDAP da qui.
func (db *DB) DeleteManualContact(uid string) error {
	_, err := db.Exec(`DELETE FROM contacts WHERE uid = ? AND source = 'manual'`, uid)
	if err != nil {
		return fmt.Errorf("failed to delete manual contact: %w", err)
	}
	return nil
}

// ListManualContacts returns all source='manual' contacts, alphabetically.
func (db *DB) ListManualContacts() ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE source = 'manual'
	ORDER BY display_name
	`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list manual contacts: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
}

// ListDistinctLDAPDNs returns the distinct ldap_dn of every contact ever
// synced (active or soft-deleted) — usato dal pannello admin per elencare
// le OU note su cui costruire il mapping Area.
func (db *DB) ListDistinctLDAPDNs() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT ldap_dn FROM contacts WHERE ldap_dn IS NOT NULL AND ldap_dn != ''`)
	if err != nil {
		return nil, fmt.Errorf("failed to list distinct DNs: %w", err)
	}
	defer rows.Close()

	var dns []string
	for rows.Next() {
		var dn string
		if err := rows.Scan(&dn); err != nil {
			return nil, fmt.Errorf("failed to scan DN: %w", err)
		}
		dns = append(dns, dn)
	}
	return dns, nil
}

// ListContactsWithNumber returns active contacts that have a phone number
// (primario o interno) — candidati di default nel picker "aggiungi
// membro" del pannello admin, prima ancora di digitare una ricerca.
func (db *DB) ListContactsWithNumber(limit int) ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, source, disabled, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL AND disabled = 0 AND ((primary_number IS NOT NULL AND primary_number != '') OR (ldap_ext IS NOT NULL AND ldap_ext != ''))
	ORDER BY display_name
	LIMIT ?
	`

	rows, err := db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list contacts with number: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
}

// listLDAPExtensionNames mappa ogni interno (ogni token ";"-separato di
// ldap_ext) dei contatti source='ldap' non soft-deleted, filtrati per
// disabled, al nome del contatto.
func (db *DB) listLDAPExtensionNames(disabled bool) (map[string]string, error) {
	disabledInt := 0
	if disabled {
		disabledInt = 1
	}
	rows, err := db.Query(`SELECT display_name, ldap_ext FROM contacts WHERE source = 'ldap' AND deleted_at IS NULL AND disabled = ? AND ldap_ext IS NOT NULL AND ldap_ext != ''`, disabledInt)
	if err != nil {
		return nil, fmt.Errorf("failed to list ldap extension names: %w", err)
	}
	defer rows.Close()

	names := make(map[string]string)
	for rows.Next() {
		var displayName, rawExt string
		if err := rows.Scan(&displayName, &rawExt); err != nil {
			return nil, fmt.Errorf("failed to scan extension name: %w", err)
		}
		for _, ext := range splitExtensions(rawExt) {
			names[ext] = displayName
		}
	}
	return names, rows.Err()
}

// ListActiveLDAPExtensionNames mappa ogni interno (ogni token ";"-separato
// di ldap_ext) dei contatti source='ldap' attivi (non disabled, non
// soft-deleted) al nome del contatto — usato per confrontare nome dominio
// e nome centralino sullo stesso interno (segnalare disallineamenti).
func (db *DB) ListActiveLDAPExtensionNames() (map[string]string, error) {
	return db.listLDAPExtensionNames(false)
}

// ListDisabledLDAPExtensionNames come ListActiveLDAPExtensionNames ma per
// i contatti disabled=1 — usato per trovare interni ancora attivi sul
// centralino ma il cui titolare in AD è disabilitato: numeri "riciclabili"
// (la persona non c'è più/non usa più quell'interno, il numero è libero
// per essere riassegnato).
func (db *DB) ListDisabledLDAPExtensionNames() (map[string]string, error) {
	return db.listLDAPExtensionNames(true)
}

// ListEmptyActiveGroups trova i gruppi di chiamata che HANNO membri
// (group_members non vuoto) ma NESSUNO di essi è un contatto attivo
// (disabled=0, non soft-deleted) — il gruppo è quindi invisibile nella
// rubrica pubblica (vedi phonebook.GroupWithMembers.ActiveMembers, filtrato
// anche lato handleSearch) pur avendo membri lato centralino: "da
// sistemare", non uno stato transitorio del sync.
func (db *DB) ListEmptyActiveGroups() ([]*GroupNumber, error) {
	rows, err := db.Query(`
	SELECT g.id, g.number, g.name, g.description, g.source, g.name_override, g.area, g.created_at, g.updated_at
	FROM group_numbers g
	WHERE EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = g.id)
	AND NOT EXISTS (
		SELECT 1 FROM group_members gm
		INNER JOIN contacts c ON c.id = gm.contact_id
		WHERE gm.group_id = g.id AND c.disabled = 0 AND c.deleted_at IS NULL
	)
	ORDER BY g.number
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list empty active groups: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		g := &GroupNumber{}
		if err := rows.Scan(&g.ID, &g.Number, &g.Name, &g.Description, &g.Source, &g.NameOverride, &g.Area, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan empty active group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// DuplicateExtension segnala un interno condiviso da più contatti attivi
// in dominio — probabile refuso in AD (stesso numero copiato su più
// schede) da controllare a mano.
type DuplicateExtension struct {
	Extension string
	Names     []string
}

// ListDuplicateExtensions trova gli interni (ogni token ";"-separato di
// ldap_ext) condivisi da 2+ contatti source='ldap' attivi (non disabled,
// non soft-deleted) — due persone abilitate non dovrebbero mai condividere
// lo stesso interno.
func (db *DB) ListDuplicateExtensions() ([]DuplicateExtension, error) {
	rows, err := db.Query(`SELECT display_name, ldap_ext FROM contacts WHERE source = 'ldap' AND deleted_at IS NULL AND disabled = 0 AND ldap_ext IS NOT NULL AND ldap_ext != '' ORDER BY display_name`)
	if err != nil {
		return nil, fmt.Errorf("failed to list contacts for duplicate extension check: %w", err)
	}
	defer rows.Close()

	byExt := make(map[string][]string)
	for rows.Next() {
		var displayName, rawExt string
		if err := rows.Scan(&displayName, &rawExt); err != nil {
			return nil, fmt.Errorf("failed to scan contact for duplicate check: %w", err)
		}
		for _, ext := range splitExtensions(rawExt) {
			byExt[ext] = append(byExt[ext], displayName)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var dups []DuplicateExtension
	for ext, names := range byExt {
		if len(names) > 1 {
			dups = append(dups, DuplicateExtension{Extension: ext, Names: names})
		}
	}
	sort.Slice(dups, func(i, j int) bool { return dups[i].Extension < dups[j].Extension })
	return dups, nil
}

// Area operations

// ListAreas returns all areas, alphabetically by name.
func (db *DB) ListAreas() ([]*Area, error) {
	rows, err := db.Query(`SELECT id, key, name, range_start, range_end, created_at, updated_at FROM areas ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("failed to list areas: %w", err)
	}
	defer rows.Close()

	var areas []*Area
	for rows.Next() {
		a := &Area{}
		if err := rows.Scan(&a.ID, &a.Key, &a.Name, &a.RangeStart, &a.RangeEnd, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan area: %w", err)
		}
		areas = append(areas, a)
	}
	return areas, nil
}

// SetAreaRange imposta (o rimuove, passando nil) la regola per range
// interno di un'area — vedi Area.RangeStart/RangeEnd.
func (db *DB) SetAreaRange(id int64, start, end *int) error {
	_, err := db.Exec(`UPDATE areas SET range_start = ?, range_end = ?, updated_at = ? WHERE id = ?`, start, end, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to set area range: %w", err)
	}
	return nil
}

// MatchExtensionRange trova la prima area con una regola di range che
// copre ext (interpretato come numero) — usato per assegnare un'area a un
// contatto/gruppo non altrimenti mappato (OU mapping vuoto per i
// contatti, o gruppo del centralino senza corrispondenza). Nil se ext non
// è numerico o nessuna regola lo copre.
func MatchExtensionRange(ext string, areas []*Area) *Area {
	n, err := strconv.Atoi(ext)
	if err != nil {
		return nil
	}
	for _, a := range areas {
		if a.RangeStart == nil || a.RangeEnd == nil {
			continue
		}
		if n >= *a.RangeStart && n <= *a.RangeEnd {
			return a
		}
	}
	return nil
}

// ListGroupCategories returns all group categories, ordinate per
// range_start crescente (NULL per ultimo) poi per nome — stesso criterio
// usato per l'ordinamento a display-time nell'albero pubblico.
func (db *DB) ListGroupCategories() ([]*GroupCategory, error) {
	rows, err := db.Query(`
	SELECT id, key, name, parent_id, range_start, range_end, created_at, updated_at
	FROM group_categories
	ORDER BY (range_start IS NULL), range_start, name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list group categories: %w", err)
	}
	defer rows.Close()

	var categories []*GroupCategory
	for rows.Next() {
		c := &GroupCategory{}
		if err := rows.Scan(&c.ID, &c.Key, &c.Name, &c.ParentID, &c.RangeStart, &c.RangeEnd, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan group category: %w", err)
		}
		categories = append(categories, c)
	}
	return categories, rows.Err()
}

// GetGroupCategory returns nil, nil se l'id non esiste.
func (db *DB) GetGroupCategory(id int64) (*GroupCategory, error) {
	c := &GroupCategory{}
	err := db.QueryRow(`
	SELECT id, key, name, parent_id, range_start, range_end, created_at, updated_at
	FROM group_categories WHERE id = ?
	`, id).Scan(&c.ID, &c.Key, &c.Name, &c.ParentID, &c.RangeStart, &c.RangeEnd, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group category: %w", err)
	}
	return c, nil
}

// CreateArea inserts a new area. Key must be unique (usato come valore di
// contacts.area e come chiave nel mapping OU->Area).
func (db *DB) CreateArea(a *Area) error {
	now := time.Now()
	a.CreatedAt = now
	a.UpdatedAt = now

	result, err := db.Exec(`INSERT INTO areas (key, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		a.Key, a.Name, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create area: %w", err)
	}
	id, _ := result.LastInsertId()
	a.ID = id
	return nil
}

// RenameArea updates only the display Name — Key resta stabile perché è
// referenziato da contacts.area e dal mapping OU->Area salvato altrove.
func (db *DB) RenameArea(id int64, name string) error {
	_, err := db.Exec(`UPDATE areas SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to rename area: %w", err)
	}
	return nil
}

// DeleteArea removes an area and clears it from any contact currently
// assigned to it (torna "nessuna area" invece di un riferimento pendente).
func (db *DB) DeleteArea(id int64) error {
	var key string
	if err := db.QueryRow(`SELECT key FROM areas WHERE id = ?`, id).Scan(&key); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("failed to look up area: %w", err)
	}

	if _, err := db.Exec(`DELETE FROM areas WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete area: %w", err)
	}
	if _, err := db.Exec(`UPDATE contacts SET area = '' WHERE area = ?`, key); err != nil {
		return fmt.Errorf("failed to clear area from contacts: %w", err)
	}
	return nil
}

// Group operations

func (db *DB) CreateGroup(group *GroupNumber) error {
	now := time.Now()
	group.CreatedAt = now
	group.UpdatedAt = now

	query := `
	INSERT INTO group_numbers (number, name, description, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?)
	`

	result, err := db.Exec(query, group.Number, group.Name, group.Description, group.CreatedAt, group.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create group: %w", err)
	}

	id, _ := result.LastInsertId()
	group.ID = id
	return nil
}

// UpdateGroup applica una modifica manuale (via admin UI) — imposta sempre
// name_override=1, coerente con UpdateContactOverride su contacts: una
// modifica esplicita fissa il nome, il sync PBX non lo sovrascrive più.
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

func (db *DB) DeleteGroup(id int64) error {
	_, err := db.Exec("DELETE FROM group_numbers WHERE id = ?", id)
	return err
}

func (db *DB) GetGroup(id int64) (*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, area, created_at, updated_at FROM group_numbers WHERE id = ?`
	group := &GroupNumber{}
	err := db.QueryRow(query, id).Scan(&group.ID, &group.Number, &group.Name, &group.Description,
		&group.Source, &group.NameOverride, &group.Area, &group.CreatedAt, &group.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	return group, nil
}

func (db *DB) ListGroups() ([]*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, area, created_at, updated_at FROM group_numbers ORDER BY number`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		group := &GroupNumber{}
		err := rows.Scan(&group.ID, &group.Number, &group.Name, &group.Description,
			&group.Source, &group.NameOverride, &group.Area, &group.CreatedAt, &group.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// GetGroupByNumber returns the group_numbers row for a given interno, o
// nil se non esiste.
func (db *DB) GetGroupByNumber(number string) (*GroupNumber, error) {
	query := `SELECT id, number, name, description, source, name_override, area, created_at, updated_at FROM group_numbers WHERE number = ?`
	group := &GroupNumber{}
	err := db.QueryRow(query, number).Scan(&group.ID, &group.Number, &group.Name, &group.Description,
		&group.Source, &group.NameOverride, &group.Area, &group.CreatedAt, &group.UpdatedAt)
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
	query := `SELECT id, number, name, description, source, name_override, area, created_at, updated_at FROM group_numbers WHERE source = ? ORDER BY number`
	rows, err := db.Query(query, source)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups by source: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		group := &GroupNumber{}
		if err := rows.Scan(&group.ID, &group.Number, &group.Name, &group.Description,
			&group.Source, &group.NameOverride, &group.Area, &group.CreatedAt, &group.UpdatedAt); err != nil {
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
func (db *DB) UpsertPBXGroup(number, name, description string, nameOverride bool, area string) (*GroupNumber, error) {
	now := time.Now()
	query := `
	INSERT INTO group_numbers (number, name, description, source, name_override, area, created_at, updated_at)
	VALUES (?, ?, ?, 'pbx', ?, ?, ?, ?)
	ON CONFLICT(number) DO UPDATE SET
		name = CASE WHEN name_override = 0 THEN excluded.name ELSE name END,
		description = excluded.description,
		source = 'pbx',
		area = excluded.area,
		updated_at = excluded.updated_at
	`
	if _, err := db.Exec(query, number, name, description, nameOverride, area, now, now); err != nil {
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

// Group member operations

func (db *DB) AddGroupMember(groupID, contactID int64) error {
	query := `INSERT OR IGNORE INTO group_members (group_id, contact_id, created_at) VALUES (?, ?, ?)`
	_, err := db.Exec(query, groupID, contactID, time.Now())
	return err
}

func (db *DB) RemoveGroupMember(groupID, contactID int64) error {
	query := `DELETE FROM group_members WHERE group_id = ? AND contact_id = ?`
	_, err := db.Exec(query, groupID, contactID)
	return err
}

func (db *DB) GetGroupMembers(groupID int64) ([]*Contact, error) {
	query := `
	SELECT c.id, c.uid, c.display_name, c.email, c.ldap_ext, c.primary_number, c.department, c.title, c.description, c.ldap_groups, c.ldap_dn, c.area, c.source, c.disabled,
	       c.manual_override, c.deleted_at, c.last_sync, c.created_at, c.updated_at
	FROM contacts c
	INNER JOIN group_members gm ON c.id = gm.contact_id
	WHERE gm.group_id = ? AND c.deleted_at IS NULL
	ORDER BY c.display_name
	`

	rows, err := db.Query(query, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group members: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.Source, &contact.Disabled, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
}

func (db *DB) GetContactGroups(contactID int64) ([]*GroupNumber, error) {
	query := `
	SELECT g.id, g.number, g.name, g.description, g.created_at, g.updated_at
	FROM group_numbers g
	INNER JOIN group_members gm ON g.id = gm.group_id
	WHERE gm.contact_id = ?
	ORDER BY g.number
	`

	rows, err := db.Query(query, contactID)
	if err != nil {
		return nil, fmt.Errorf("failed to get contact groups: %w", err)
	}
	defer rows.Close()

	var groups []*GroupNumber
	for rows.Next() {
		group := &GroupNumber{}
		err := rows.Scan(&group.ID, &group.Number, &group.Name, &group.Description,
			&group.CreatedAt, &group.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// Config operations

func (db *DB) SetConfig(key, value string) error {
	query := `
	INSERT INTO app_config (key, value, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`
	_, err := db.Exec(query, key, value, time.Now())
	return err
}

func (db *DB) GetConfig(key string) (string, error) {
	query := `SELECT value FROM app_config WHERE key = ?`
	var value string
	err := db.QueryRow(query, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get config: %w", err)
	}
	return value, nil
}
