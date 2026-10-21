package database

import (
	"errors"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
)

func areaByKey(t *testing.T, db *DB, key string) *Area {
	t.Helper()
	a, err := db.GetAreaByKey(key)
	if err != nil || a == nil {
		t.Fatalf("GetAreaByKey(%q) = %v, %v", key, a, err)
	}
	return a
}

func TestMigrationCreatesDefaultOURules(t *testing.T) {
	db := newTestDB(t)
	tree, err := db.AreaTree()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"interni": "INTERNI", "esterni": "ESTERNI", "politica": "AREA_POLITICA"}
	for key, ou := range want {
		n := arearules.FindNode(tree, key)
		if n == nil || len(n.Rules) != 1 || n.Rules[0] != (arearules.Rule{Kind: "ou", Value: ou}) {
			t.Errorf("area %s: regole = %+v, want una regola ou %s", key, n, ou)
		}
	}
}

func TestMigrationFromSavedMappingAndRanges(t *testing.T) {
	db := newTestDB(t)
	// Simula un DB precedente: flag assente, mapping personalizzato, range.
	if _, err := db.Exec(`DELETE FROM area_rules`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConfig("area_rules_migrated", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConfig("ou_area_mapping", `{"UNDEF":"esterni","INTERNI":"interni","FANTASMA":"inesistente"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE areas SET range_start = 700, range_end = 799 WHERE key = 'uffici'`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateAreaRules(); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateAreaRules(); err != nil { // idempotente
		t.Fatal(err)
	}
	rules, _ := db.ListAreaRules(areaByKey(t, db, "esterni").ID)
	if len(rules) != 1 || rules[0].Value != "UNDEF" {
		t.Errorf("esterni = %+v, want solo ou UNDEF", rules)
	}
	rules, _ = db.ListAreaRules(areaByKey(t, db, "uffici").ID)
	if len(rules) != 1 || rules[0].Kind != "range" || rules[0].Value != "700-799" {
		t.Errorf("uffici = %+v, want range 700-799", rules)
	}
	rules, _ = db.ListAreaRules(areaByKey(t, db, "politica").ID)
	if len(rules) != 0 {
		t.Errorf("politica = %+v, want nessuna regola (non nel mapping salvato)", rules)
	}
}

func TestCreateSubAreaAndNesting(t *testing.T) {
	db := newTestDB(t)
	sub := &Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}
	if err := db.CreateArea(sub); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateArea(&Area{Key: "x", Name: "X", ParentKey: "dirigenti"}); !errors.Is(err, ErrNestedSubArea) {
		t.Errorf("err = %v, want ErrNestedSubArea", err)
	}
	if err := db.CreateArea(&Area{Key: "y", Name: "Y", ParentKey: "nessuna"}); !errors.Is(err, ErrParentNotFound) {
		t.Errorf("err = %v, want ErrParentNotFound", err)
	}
	if err := db.CreateArea(&Area{Key: "collab", Name: "Collaboratori", ParentKey: "interni"}); err != nil {
		t.Fatal(err)
	}
	tree, _ := db.AreaTree()
	n := arearules.FindNode(tree, "interni")
	if n == nil || len(n.Children) != 2 || n.Children[0].Key != "dirigenti" || n.Children[1].Key != "collab" {
		t.Fatalf("figli di interni = %+v", n)
	}
	if err := db.MoveArea(areaByKey(t, db, "collab").ID, true); err != nil {
		t.Fatal(err)
	}
	tree, _ = db.AreaTree()
	if n := arearules.FindNode(tree, "interni"); n.Children[0].Key != "collab" {
		t.Errorf("dopo MoveArea su: primo figlio = %q, want collab", n.Children[0].Key)
	}
}

func TestDeleteAreaCascades(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateArea(&Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}); err != nil {
		t.Fatal(err)
	}
	sub := areaByKey(t, db, "dirigenti")
	if err := db.AddAreaRule(sub.ID, arearules.Rule{Kind: "user", Value: "u1"}); err != nil {
		t.Fatal(err)
	}
	c := &Contact{UID: "u1", DisplayName: "U1", Area: "interni", LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO contact_sub_areas (contact_id, sub_area_key) VALUES (?, 'dirigenti')`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteArea(areaByKey(t, db, "interni").ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAreaByKey("dirigenti"); a != nil {
		t.Error("sotto-area non eliminata con la madre")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM area_rules WHERE area_id = ?`, sub.ID).Scan(&n)
	if n != 0 {
		t.Errorf("regole residue = %d", n)
	}
	db.QueryRow(`SELECT COUNT(*) FROM contact_sub_areas`).Scan(&n)
	if n != 0 {
		t.Errorf("contact_sub_areas residue = %d", n)
	}
	got, _ := db.GetContact("u1")
	if got.Area != "" {
		t.Errorf("area contatto = %q, want vuota", got.Area)
	}
}

func TestScopeRulesCRUD(t *testing.T) {
	db := newTestDB(t)
	if err := db.AddScopeRule(arearules.Rule{Kind: "ou", Value: "INTERNI"}); err != nil {
		t.Fatal(err)
	}
	rs, _ := db.ListScopeRules()
	if len(rs) != 1 || rs[0].Value != "INTERNI" || rs[0].AreaID != 0 {
		t.Fatalf("scope = %+v", rs)
	}
	if err := db.DeleteScopeRule(rs[0].ID); err != nil {
		t.Fatal(err)
	}
	if rs, _ := db.ListScopeRules(); len(rs) != 0 {
		t.Errorf("scope dopo delete = %+v", rs)
	}
}

// Review: prima l'OU mapping vinceva sempre sul range; la migrazione deve
// mettere le aree con regole ou prima di quelle con solo range.
func TestMigrationKeepsOUBeforeRange(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	if _, err := db.Exec(`DELETE FROM area_rules`); err != nil {
		t.Fatal(err)
	}
	db.SetConfig("area_rules_migrated", "")
	if _, err := db.Exec(`INSERT INTO areas (key, name, range_start, range_end, created_at, updated_at) VALUES ('azienda_speciale', 'Azienda Speciale', 500, 599, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateAreaRules(); err != nil {
		t.Fatal(err)
	}
	tree, _ := db.AreaTree()
	interno := arearules.Profile{DN: "CN=x,OU=Users,OU=INTERNI,DC=x", Extensions: []string{"550"}}
	if area, _ := arearules.Resolve(interno, tree); area != "interni" {
		t.Errorf("dipendente OU=INTERNI con interno 550 = %q, want interni (OU vince sul range come prima)", area)
	}
	pbx := arearules.Profile{Extensions: []string{"550"}}
	if area, _ := arearules.Resolve(pbx, tree); area != "azienda_speciale" {
		t.Errorf("peer 550 = %q, want azienda_speciale", area)
	}
}
