package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// contactCols: colonne di contacts con alias c, nello stesso ordine dello
// Scan usato da GetContact.
const contactCols = `c.id, c.uid, c.display_name, c.email, c.ldap_ext, c.primary_number, c.department, c.title, c.description, c.ldap_groups, c.ldap_dn, c.area, c.source, c.disabled, c.manual_override, c.deleted_at, c.last_sync, c.created_at, c.updated_at`

// hasContactInfoSQL: stessa condizione "ha almeno un recapito" di CountByArea.
const hasContactInfoSQL = `(c.email IS NOT NULL AND c.email != '' OR c.ldap_ext IS NOT NULL AND c.ldap_ext != '' OR c.primary_number IS NOT NULL AND c.primary_number != '')`

func scanContactRows(rows *sql.Rows) ([]*Contact, error) {
	defer rows.Close()
	var out []*Contact
	for rows.Next() {
		c := &Contact{}
		if err := rows.Scan(&c.ID, &c.UID, &c.DisplayName, &c.Email, &c.LDAPExt, &c.PrimaryNumber, &c.Department, &c.Title,
			&c.Description, &c.LDAPGroups, &c.LDAPDN, &c.Area, &c.Source, &c.Disabled, &c.ManualOverride,
			&c.DeletedAt, &c.LastSync, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetContactSubAreas sostituisce in blocco le sotto-aree del contatto.
func (db *DB) SetContactSubAreas(contactID int64, keys []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM contact_sub_areas WHERE contact_id = ?`, contactID); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to clear sub-areas: %w", err)
	}
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO contact_sub_areas (contact_id, sub_area_key) VALUES (?, ?)`, contactID, k); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to set sub-area: %w", err)
		}
	}
	return tx.Commit()
}

// ListSubAreasForContacts: id contatto → key sotto-aree in ordine di
// visualizzazione. Una sola query per tutti i risultati di una ricerca.
func (db *DB) ListSubAreasForContacts(ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(`
	SELECT csa.contact_id, csa.sub_area_key
	FROM contact_sub_areas csa LEFT JOIN areas a ON a.key = csa.sub_area_key
	WHERE csa.contact_id IN (`+ph+`)
	ORDER BY csa.contact_id, COALESCE(a.sort, 0), COALESCE(a.name, csa.sub_area_key)`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list contact sub-areas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = append(out[id], key)
	}
	return out, rows.Err()
}

// CountBySubArea: contatti attivi con almeno un recapito per sotto-area.
func (db *DB) CountBySubArea() (map[string]int, error) {
	rows, err := db.Query(`
	SELECT csa.sub_area_key, COUNT(*)
	FROM contact_sub_areas csa JOIN contacts c ON c.id = csa.contact_id
	WHERE c.deleted_at IS NULL AND c.disabled = 0 AND ` + hasContactInfoSQL + `
	GROUP BY csa.sub_area_key`)
	if err != nil {
		return nil, fmt.Errorf("failed to count by sub-area: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ContactIDsBySubArea: id dei contatti con quella sotto-area (CardDAV).
func (db *DB) ContactIDsBySubArea(key string) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT contact_id FROM contact_sub_areas WHERE sub_area_key = ?`, key)
	if err != nil {
		return nil, fmt.Errorf("failed to list sub-area contacts: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (db *DB) listMembers(where string, limit int, args ...any) ([]*Contact, int, error) {
	base := ` FROM contacts c WHERE c.deleted_at IS NULL AND c.disabled = 0 AND ` + where
	var total int
	if err := db.QueryRow(`SELECT COUNT(*)`+base, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count members: %w", err)
	}
	rows, err := db.Query(`SELECT `+contactCols+base+` ORDER BY c.display_name LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list members: %w", err)
	}
	list, err := scanContactRows(rows)
	return list, total, err
}

func (db *DB) ListAreaMembers(areaKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.area = ?`, limit, areaKey)
}

func (db *DB) ListSubAreaMembers(subKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.id IN (SELECT contact_id FROM contact_sub_areas WHERE sub_area_key = ?)`, limit, subKey)
}

func (db *DB) ListAreaMembersWithoutSubArea(areaKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.area = ? AND NOT EXISTS (SELECT 1 FROM contact_sub_areas WHERE contact_id = c.id)`, limit, areaKey)
}

// ContactIDAndArea: id e area salvata di un contatto in qualsiasi stato
// (anche disabled/cancellato). 0, "", nil se non esiste. Dopo un upsert
// LastInsertId non è affidabile sul ramo UPDATE: usare questa.
func (db *DB) ContactIDAndArea(uid string) (int64, string, error) {
	var id int64
	var area sql.NullString
	err := db.QueryRow(`SELECT id, area FROM contacts WHERE uid = ?`, uid).Scan(&id, &area)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("failed to get contact id: %w", err)
	}
	return id, area.String, nil
}

// SetContactArea aggiorna l'area senza toccare chi ha manual_override.
func (db *DB) SetContactArea(id int64, area string) error {
	if _, err := db.Exec(`UPDATE contacts SET area = ?, updated_at = ? WHERE id = ? AND manual_override = 0`, area, time.Now(), id); err != nil {
		return fmt.Errorf("failed to set contact area: %w", err)
	}
	return nil
}

func (db *DB) SetGroupArea(id int64, area string) error {
	if _, err := db.Exec(`UPDATE group_numbers SET area = ?, updated_at = ? WHERE id = ?`, area, time.Now(), id); err != nil {
		return fmt.Errorf("failed to set group area: %w", err)
	}
	return nil
}

// CountStaleLDAP: contatti LDAP non cancellati che SoftDeleteStale
// cancellerebbe con questo syncTime.
func (db *DB) CountStaleLDAP(syncTime time.Time) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE deleted_at IS NULL AND source = 'ldap' AND last_sync < ?`, syncTime).Scan(&n)
	return n, err
}

// CountActiveLDAP: contatti LDAP non cancellati (disabled compresi).
func (db *DB) CountActiveLDAP() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE deleted_at IS NULL AND source = 'ldap'`).Scan(&n)
	return n, err
}

// ListActiveLDAPNames: uid → nome dei contatti LDAP oggi in rubrica, per
// l'anteprima del perimetro.
func (db *DB) ListActiveLDAPNames() (map[string]string, error) {
	rows, err := db.Query(`SELECT uid, display_name FROM contacts WHERE deleted_at IS NULL AND disabled = 0 AND source = 'ldap'`)
	if err != nil {
		return nil, fmt.Errorf("failed to list ldap contacts: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, name string
		if err := rows.Scan(&uid, &name); err != nil {
			return nil, err
		}
		out[uid] = name
	}
	return out, rows.Err()
}
