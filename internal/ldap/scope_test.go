package ldap

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateScopeRules(t *testing.T) {
	db := newDB(t)
	find := func(cn string) (string, error) {
		if cn == "all_users-servizi_interni" {
			return "CN=ALL_USERS-SERVIZI_INTERNI,OU=COMUNE-MS,DC=x", nil
		}
		return "", nil
	}
	if err := migrateScopeRules(db, []string{"all_users-servizi_interni", "all_users-servizi_esterni"}, find); err != nil {
		t.Fatal(err)
	}
	if err := migrateScopeRules(db, []string{"all_users-servizi_interni"}, find); err != nil {
		t.Fatal(err)
	}
	rs, _ := db.ListScopeRules()
	if len(rs) != 1 || rs[0].Kind != "adgroup" || rs[0].Value != "cn=all_users-servizi_interni,ou=comune-ms,dc=x" {
		t.Errorf("scope = %+v, want una sola regola (CN inesistente scartato, seconda chiamata no-op)", rs)
	}
}

func TestMigrateScopeRulesNothingFound(t *testing.T) {
	db := newDB(t)
	find := func(string) (string, error) { return "", errors.New("non trovato") }
	if err := migrateScopeRules(db, []string{"fantasma"}, find); err == nil {
		t.Fatal("atteso errore: nessun gruppo trovato")
	}
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v == "1" {
		t.Error("il flag non deve essere scritto se la migrazione fallisce")
	}
}

// Review Focus 1: con manual_override l'upsert conserva l'area vecchia; le
// sotto-aree salvate devono essere quelle dell'area salvata.
func TestApplySubAreasFollowsStoredArea(t *testing.T) {
	db := newDB(t)
	for _, a := range []*database.Area{
		{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"},
		{Key: "fornitori", Name: "Fornitori", ParentKey: "esterni"},
	} {
		if err := db.CreateArea(a); err != nil {
			t.Fatal(err)
		}
	}
	db.UpsertContact(&database.Contact{UID: "u", DisplayName: "U", Area: "esterni", LastSync: time.Now()})
	db.UpdateContactOverride("u", "u@x.it", "1")

	tree := []arearules.Node{
		{Key: "interni", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}, Children: []arearules.Node{
			{Key: "dirigenti", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}},
		}},
		{Key: "esterni", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}, Children: []arearules.Node{
			{Key: "fornitori", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}},
		}},
	}
	ev := evaluated{
		Contact:  &database.Contact{UID: "u", DisplayName: "U", Area: "interni", LastSync: time.Now()},
		Profile:  arearules.Profile{Username: "u"},
		SubAreas: []string{"dirigenti"},
	}
	if err := db.UpsertContact(ev.Contact); err != nil {
		t.Fatal(err)
	}
	applySubAreas(db, ev, tree)
	id, area, _ := db.ContactIDAndArea("u")
	got, _ := db.ListSubAreasForContacts([]int64{id})
	if area != "esterni" || len(got[id]) != 1 || got[id][0] != "fornitori" {
		t.Errorf("area=%q sub=%v, want esterni e [fornitori]", area, got[id])
	}
}

func TestMigrateScopeRulesEmptyEnv(t *testing.T) {
	db := newDB(t)
	if err := migrateScopeRules(db, nil, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v != "1" {
		t.Error("env vuota: flag atteso")
	}
}

// Review: un errore di ricerca (timeout) non è "gruppo inesistente": la
// migrazione deve fermarsi senza scrivere il flag, non scartare il gruppo.
func TestMigrateScopeRulesSearchErrorAborts(t *testing.T) {
	db := newDB(t)
	find := func(cn string) (string, error) {
		if cn == "a" {
			return "", errors.New("timeout")
		}
		return "CN=B,DC=x", nil
	}
	if err := migrateScopeRules(db, []string{"a", "b"}, find); err == nil {
		t.Fatal("atteso errore su ricerca fallita")
	}
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v == "1" {
		t.Error("flag scritto nonostante l'errore")
	}
	if rs, _ := db.ListScopeRules(); len(rs) != 0 {
		t.Errorf("regole create nonostante l'errore: %+v", rs)
	}
}
