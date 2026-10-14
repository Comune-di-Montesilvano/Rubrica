package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
)

// Area è un'area di primo livello (ParentKey "") o una sotto-area
// (ParentKey = key della madre). Un solo livello di annidamento. Key è
// stabile: usata in contacts.area, contact_sub_areas e negli URL
// (?group=, ?sub=, /carddav/<key>/). Sort = precedenza fra fratelli.
type Area struct {
	ID        int64
	Key       string
	Name      string
	ParentKey string
	Sort      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AreaNode: area di primo livello con le sue sotto-aree, per i template.
type AreaNode struct {
	*Area
	Children []*Area
}

// AreaRule è una regola salvata; AreaID 0 per le regole di perimetro.
type AreaRule struct {
	ID     int64
	AreaID int64
	arearules.Rule
	CreatedAt time.Time
}

var (
	ErrNestedSubArea  = errors.New("una sotto-area non può avere sotto-aree")
	ErrParentNotFound = errors.New("area madre inesistente")
)

const areaRulesMigratedKey = "area_rules_migrated"

// RulesOf estrae le regole pure da quelle salvate.
func RulesOf(rs []*AreaRule) []arearules.Rule {
	out := make([]arearules.Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Rule)
	}
	return out
}

// BuildAreaNodes raggruppa le aree (già ordinate) in primo livello + figli.
func BuildAreaNodes(areas []*Area) []*AreaNode {
	var nodes []*AreaNode
	idx := map[string]*AreaNode{}
	for _, a := range areas {
		if a.ParentKey == "" {
			n := &AreaNode{Area: a}
			nodes = append(nodes, n)
			idx[a.Key] = n
		}
	}
	for _, a := range areas {
		if n, ok := idx[a.ParentKey]; ok && a.ParentKey != "" {
			n.Children = append(n.Children, a)
		}
	}
	return nodes
}

const areaColumns = `id, key, name, parent_key, sort, created_at, updated_at`

func scanArea(row interface{ Scan(...any) error }) (*Area, error) {
	a := &Area{}
	if err := row.Scan(&a.ID, &a.Key, &a.Name, &a.ParentKey, &a.Sort, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return a, nil
}

// ListAreas: tutte le aree, in ordine di precedenza (sort, poi nome).
func (db *DB) ListAreas() ([]*Area, error) {
	rows, err := db.Query(`SELECT ` + areaColumns + ` FROM areas ORDER BY sort, name`)
	if err != nil {
		return nil, fmt.Errorf("failed to list areas: %w", err)
	}
	defer rows.Close()
	var out []*Area
	for rows.Next() {
		a, err := scanArea(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan area: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) getAreaWhere(where string, arg any) (*Area, error) {
	a, err := scanArea(db.QueryRow(`SELECT `+areaColumns+` FROM areas WHERE `+where, arg))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get area: %w", err)
	}
	return a, nil
}

func (db *DB) GetArea(id int64) (*Area, error)        { return db.getAreaWhere("id = ?", id) }
func (db *DB) GetAreaByKey(key string) (*Area, error) { return db.getAreaWhere("key = ?", key) }

// CreateArea inserisce un'area in coda ai fratelli. Con ParentKey la madre
// deve esistere ed essere di primo livello.
func (db *DB) CreateArea(a *Area) error {
	if a.ParentKey != "" {
		parent, err := db.GetAreaByKey(a.ParentKey)
		if err != nil {
			return err
		}
		if parent == nil {
			return ErrParentNotFound
		}
		if parent.ParentKey != "" {
			return ErrNestedSubArea
		}
	}
	var maxSort int
	if err := db.QueryRow(`SELECT COALESCE(MAX(sort), -1) FROM areas WHERE parent_key = ?`, a.ParentKey).Scan(&maxSort); err != nil {
		return fmt.Errorf("failed to compute sort: %w", err)
	}
	now := time.Now()
	a.Sort, a.CreatedAt, a.UpdatedAt = maxSort+1, now, now
	res, err := db.Exec(`INSERT INTO areas (key, name, parent_key, sort, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.Key, a.Name, a.ParentKey, a.Sort, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create area: %w", err)
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// RenameArea cambia solo il nome: la key resta stabile.
func (db *DB) RenameArea(id int64, name string) error {
	if _, err := db.Exec(`UPDATE areas SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now(), id); err != nil {
		return fmt.Errorf("failed to rename area: %w", err)
	}
	return nil
}

// MoveArea sposta l'area di una posizione fra i fratelli e rinumera sort
// 0..n-1 (normalizza eventuali sort duplicati).
func (db *DB) MoveArea(id int64, up bool) error {
	a, err := db.GetArea(id)
	if err != nil || a == nil {
		return err
	}
	rows, err := db.Query(`SELECT id FROM areas WHERE parent_key = ? ORDER BY sort, name`, a.ParentKey)
	if err != nil {
		return fmt.Errorf("failed to list siblings: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, sid)
	}
	rows.Close()
	pos := -1
	for i, sid := range ids {
		if sid == id {
			pos = i
		}
	}
	other := pos + 1
	if up {
		other = pos - 1
	}
	if pos < 0 || other < 0 || other >= len(ids) {
		return nil
	}
	ids[pos], ids[other] = ids[other], ids[pos]
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i, sid := range ids {
		if _, err := tx.Exec(`UPDATE areas SET sort = ?, updated_at = ? WHERE id = ?`, i, time.Now(), sid); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to reorder areas: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteArea elimina l'area, le sue sotto-aree e le loro regole; azzera
// contacts.area / group_numbers.area che le usavano e rimuove le righe
// contact_sub_areas corrispondenti.
func (db *DB) DeleteArea(id int64) error {
	a, err := db.GetArea(id)
	if err != nil || a == nil {
		return err
	}
	ids, keys := []int64{a.ID}, []string{a.Key}
	if a.ParentKey == "" {
		rows, err := db.Query(`SELECT id, key FROM areas WHERE parent_key = ?`, a.Key)
		if err != nil {
			return fmt.Errorf("failed to list sub-areas: %w", err)
		}
		for rows.Next() {
			var cid int64
			var ckey string
			if err := rows.Scan(&cid, &ckey); err != nil {
				rows.Close()
				return err
			}
			ids, keys = append(ids, cid), append(keys, ckey)
		}
		rows.Close()
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	exec := func(q string, arg any) error {
		if _, err := tx.Exec(q, arg); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to delete area: %w", err)
		}
		return nil
	}
	for _, i := range ids {
		if err := exec(`DELETE FROM area_rules WHERE area_id = ?`, i); err != nil {
			return err
		}
		if err := exec(`DELETE FROM areas WHERE id = ?`, i); err != nil {
			return err
		}
	}
	for _, k := range keys {
		for _, q := range []string{
			`DELETE FROM contact_sub_areas WHERE sub_area_key = ?`,
			`UPDATE contacts SET area = '' WHERE area = ?`,
			`UPDATE group_numbers SET area = '' WHERE area = ?`,
		} {
			if err := exec(q, k); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (db *DB) listRules(table, where string, args ...any) ([]*AreaRule, error) {
	cols := `id, 0, kind, attr, value, created_at`
	if table == "area_rules" {
		cols = `id, area_id, kind, attr, value, created_at`
	}
	rows, err := db.Query(`SELECT `+cols+` FROM `+table+` `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list rules: %w", err)
	}
	defer rows.Close()
	var out []*AreaRule
	for rows.Next() {
		r := &AreaRule{}
		if err := rows.Scan(&r.ID, &r.AreaID, &r.Kind, &r.Attr, &r.Value, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) ListAreaRules(areaID int64) ([]*AreaRule, error) {
	return db.listRules("area_rules", "WHERE area_id = ?", areaID)
}

func (db *DB) AddAreaRule(areaID int64, r arearules.Rule) error {
	_, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, ?, ?, ?, ?)`,
		areaID, r.Kind, r.Attr, r.Value, time.Now())
	if err != nil {
		return fmt.Errorf("failed to add area rule: %w", err)
	}
	return nil
}

func (db *DB) DeleteAreaRule(areaID, ruleID int64) error {
	if _, err := db.Exec(`DELETE FROM area_rules WHERE id = ? AND area_id = ?`, ruleID, areaID); err != nil {
		return fmt.Errorf("failed to delete area rule: %w", err)
	}
	return nil
}

func (db *DB) ListScopeRules() ([]*AreaRule, error) { return db.listRules("scope_rules", "") }

func (db *DB) AddScopeRule(r arearules.Rule) error {
	_, err := db.Exec(`INSERT INTO scope_rules (kind, attr, value, created_at) VALUES (?, ?, ?, ?)`, r.Kind, r.Attr, r.Value, time.Now())
	if err != nil {
		return fmt.Errorf("failed to add scope rule: %w", err)
	}
	return nil
}

func (db *DB) DeleteScopeRule(ruleID int64) error {
	if _, err := db.Exec(`DELETE FROM scope_rules WHERE id = ?`, ruleID); err != nil {
		return fmt.Errorf("failed to delete scope rule: %w", err)
	}
	return nil
}

// AreaTree: albero di valutazione (precedenza = ordine) con le regole.
func (db *DB) AreaTree() ([]arearules.Node, error) {
	areas, err := db.ListAreas()
	if err != nil {
		return nil, err
	}
	all, err := db.listRules("area_rules", "")
	if err != nil {
		return nil, err
	}
	byArea := map[int64][]arearules.Rule{}
	for _, r := range all {
		byArea[r.AreaID] = append(byArea[r.AreaID], r.Rule)
	}
	var tree []arearules.Node
	for _, n := range BuildAreaNodes(areas) {
		node := arearules.Node{Key: n.Key, Name: n.Name, Rules: byArea[n.ID]}
		for _, c := range n.Children {
			node.Children = append(node.Children, arearules.Node{Key: c.Key, Name: c.Name, Rules: byArea[c.ID]})
		}
		tree = append(tree, node)
	}
	return tree, nil
}

// migrateAreaRules converte, una sola volta, il vecchio mapping OU->Area
// (app_config "ou_area_mapping", o il default storico) e i range delle aree
// (areas.range_start/range_end) in regole, e assegna sort alfabetico.
func (db *DB) migrateAreaRules() error {
	if v, _ := db.GetConfig(areaRulesMigratedKey); v == "1" {
		return nil
	}
	mapping := map[string]string{"INTERNI": "interni", "ESTERNI": "esterni", "AREA_POLITICA": "politica"}
	if raw, _ := db.GetConfig("ou_area_mapping"); raw != "" {
		var stored map[string]string
		if err := json.Unmarshal([]byte(raw), &stored); err == nil && len(stored) > 0 {
			mapping = stored
		}
	}
	now := time.Now()
	for ou, key := range mapping {
		if key == "" {
			continue
		}
		a, err := db.GetAreaByKey(key)
		if err != nil {
			return err
		}
		if a == nil {
			continue
		}
		if _, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, 'ou', '', ?, ?)`,
			a.ID, strings.ToUpper(strings.TrimSpace(ou)), now); err != nil {
			return err
		}
	}

	type rng struct {
		id     int64
		lo, hi int
	}
	var ranges []rng
	rows, err := db.Query(`SELECT id, range_start, range_end FROM areas WHERE range_start IS NOT NULL AND range_end IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r rng
		if err := rows.Scan(&r.id, &r.lo, &r.hi); err != nil {
			rows.Close()
			return err
		}
		ranges = append(ranges, r)
	}
	rows.Close()
	for _, r := range ranges {
		if _, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, 'range', '', ?, ?)`,
			r.id, fmt.Sprintf("%d-%d", r.lo, r.hi), now); err != nil {
			return err
		}
	}

	// Precedenza: prima le aree con regole ou (prima della migrazione l'OU
	// mapping vinceva sempre sul range), poi le altre; alfabetico dentro
	// ciascun gruppo.
	var ids []int64
	rows, err = db.Query(`SELECT id FROM areas ORDER BY
		(SELECT COUNT(*) FROM area_rules r WHERE r.area_id = areas.id AND r.kind = 'ou') = 0, name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if _, err := db.Exec(`UPDATE areas SET sort = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return db.SetConfig(areaRulesMigratedKey, "1")
}
