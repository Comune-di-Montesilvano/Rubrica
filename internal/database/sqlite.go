package database

import (
	"database/sql"
	"fmt"
	"log"
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
	ManualOverride bool
	DeletedAt      *time.Time
	LastSync       time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type GroupNumber struct {
	ID          int64
	Number      string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
	`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}

	// Add new columns if they don't exist (migration)
	alterStatements := []string{
		"ALTER TABLE contacts ADD COLUMN title TEXT",
		"ALTER TABLE contacts ADD COLUMN description TEXT",
		"ALTER TABLE contacts ADD COLUMN area TEXT",
	}

	for _, stmt := range alterStatements {
		if _, err := db.Exec(stmt); err != nil {
			// Ignore "duplicate column name" errors (SQLite error: "duplicate column name")
			if !strings.Contains(err.Error(), "duplicate column name") {
				log.Printf("[DATABASE] Warning during migration: %v", err)
			}
		}
	}

	return nil
}

// Contact operations

func (db *DB) UpsertContact(contact *Contact) error {
	now := time.Now()
	contact.UpdatedAt = now

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
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE uid = ? AND deleted_at IS NULL
	`

	contact := &Contact{}
	err := db.QueryRow(query, uid).Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
		&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
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
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL
	AND (email IS NOT NULL AND email != '' OR ldap_ext IS NOT NULL AND ldap_ext != '' OR primary_number IS NOT NULL AND primary_number != '')
	AND (display_name LIKE ? OR email LIKE ? OR ldap_ext LIKE ? OR primary_number LIKE ? OR department LIKE ?)
	ORDER BY display_name
	LIMIT ?
	`

	pattern := "%" + query + "%"
	rows, err := db.Query(searchQuery, pattern, pattern, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search contacts: %w", err)
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		err := rows.Scan(&contact.ID, &contact.UID, &contact.DisplayName, &contact.Email,
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
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
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL
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
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}

	return contacts, nil
}

// ListAllContacts returns all contacts without filtering (for CardDAV)
func (db *DB) ListAllContacts(limit, offset int) ([]*Contact, error) {
	query := `
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL
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
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
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

// SoftDeleteStale marca come cancellato (deleted_at) qualsiasi contatto
// attivo il cui last_sync sia precedente a syncTime — cioè non è stato
// toccato dal giro di sync corrente (disabilitato, spostato, rimosso da
// AD). Ritorna quanti sono stati appena soft-deleted.
func (db *DB) SoftDeleteStale(syncTime time.Time) (int64, error) {
	result, err := db.Exec(
		`UPDATE contacts SET deleted_at = ?, updated_at = ? WHERE deleted_at IS NULL AND last_sync < ?`,
		syncTime, syncTime, syncTime,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to soft-delete stale contacts: %w", err)
	}
	return result.RowsAffected()
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
	SELECT id, uid, display_name, email, ldap_ext, primary_number, department, title, description, ldap_groups, ldap_dn, area, manual_override, deleted_at, last_sync, created_at, updated_at
	FROM contacts
	WHERE deleted_at IS NULL AND ((primary_number IS NOT NULL AND primary_number != '') OR (ldap_ext IS NOT NULL AND ldap_ext != ''))
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
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
			&contact.DeletedAt, &contact.LastSync, &contact.CreatedAt, &contact.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
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

func (db *DB) UpdateGroup(group *GroupNumber) error {
	group.UpdatedAt = time.Now()
	query := `
	UPDATE group_numbers
	SET number = ?, name = ?, description = ?, updated_at = ?
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
	query := `SELECT id, number, name, description, created_at, updated_at FROM group_numbers WHERE id = ?`
	group := &GroupNumber{}
	err := db.QueryRow(query, id).Scan(&group.ID, &group.Number, &group.Name, &group.Description,
		&group.CreatedAt, &group.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	return group, nil
}

func (db *DB) ListGroups() ([]*GroupNumber, error) {
	query := `SELECT id, number, name, description, created_at, updated_at FROM group_numbers ORDER BY number`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
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
	SELECT c.id, c.uid, c.display_name, c.email, c.ldap_ext, c.primary_number, c.department, c.title, c.description, c.ldap_groups, c.ldap_dn, c.area,
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
			&contact.LDAPExt, &contact.PrimaryNumber, &contact.Department, &contact.Title, &contact.Description, &contact.LDAPGroups, &contact.LDAPDN, &contact.Area, &contact.ManualOverride,
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
