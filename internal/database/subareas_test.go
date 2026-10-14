package database

import (
	"reflect"
	"testing"
	"time"
)

func seedSubAreas(t *testing.T, db *DB) {
	t.Helper()
	for _, a := range []*Area{
		{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"},
		{Key: "protciv", Name: "Protezione civile", ParentKey: "interni"},
	} {
		if err := db.CreateArea(a); err != nil {
			t.Fatal(err)
		}
	}
}

func addContact(t *testing.T, db *DB, uid, area string) *Contact {
	t.Helper()
	c := &Contact{UID: uid, DisplayName: uid, Email: uid + "@x.it", Area: area, LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	id, _, err := db.ContactIDAndArea(uid)
	if err != nil || id == 0 {
		t.Fatalf("ContactIDAndArea(%s) = %d, %v", uid, id, err)
	}
	c.ID = id
	return c
}

func TestSetAndListContactSubAreas(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	a := addContact(t, db, "a", "interni")
	b := addContact(t, db, "b", "interni")
	if err := db.SetContactSubAreas(a.ID, []string{"protciv", "dirigenti", "dirigenti"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(b.ID, []string{"protciv"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(b.ID, nil); err != nil { // sostituzione in blocco
		t.Fatal(err)
	}
	got, err := db.ListSubAreasForContacts([]int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dirigenti", "protciv"}; !reflect.DeepEqual(got[a.ID], want) {
		t.Errorf("sub di a = %v, want %v (ordine di sort, senza duplicati)", got[a.ID], want)
	}
	if len(got[b.ID]) != 0 {
		t.Errorf("sub di b = %v, want vuoto", got[b.ID])
	}
	if m, _ := db.ListSubAreasForContacts(nil); len(m) != 0 {
		t.Errorf("ids vuoti = %v", m)
	}
}

func TestCountsAndMembers(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	a := addContact(t, db, "a", "interni")
	addContact(t, db, "b", "interni")
	db.SetContactSubAreas(a.ID, []string{"dirigenti", "protciv"})

	counts, err := db.CountBySubArea()
	if err != nil {
		t.Fatal(err)
	}
	if counts["dirigenti"] != 1 || counts["protciv"] != 1 {
		t.Errorf("counts = %v", counts)
	}
	members, total, _ := db.ListSubAreaMembers("dirigenti", 30)
	if total != 1 || len(members) != 1 || members[0].UID != "a" {
		t.Errorf("membri dirigenti = %d %+v", total, members)
	}
	without, total, _ := db.ListAreaMembersWithoutSubArea("interni", 30)
	if total != 1 || without[0].UID != "b" {
		t.Errorf("senza sottogruppo = %d %+v", total, without)
	}
	all, total, _ := db.ListAreaMembers("interni", 1)
	if total != 2 || len(all) != 1 {
		t.Errorf("membri interni = total %d len %d, want 2 e 1 (limit)", total, len(all))
	}
	ids, _ := db.ContactIDsBySubArea("protciv")
	if !ids[a.ID] || len(ids) != 1 {
		t.Errorf("ContactIDsBySubArea = %v", ids)
	}
}

func TestManualContactDeleteCascadesSubAreas(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	c := &Contact{UID: "manual-x", DisplayName: "X", Area: "interni"}
	if err := db.CreateManualContact(c); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(c.ID, []string{"dirigenti"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteManualContact("manual-x"); err != nil {
		t.Fatalf("DeleteManualContact con sotto-aree: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM contact_sub_areas`).Scan(&n)
	if n != 0 {
		t.Errorf("righe residue = %d", n)
	}
}

func TestStaleCounts(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-time.Hour)
	for _, uid := range []string{"a", "b", "c"} {
		db.UpsertContact(&Contact{UID: uid, DisplayName: uid, LastSync: old})
	}
	now := time.Now()
	db.UpsertContact(&Contact{UID: "a", DisplayName: "a", LastSync: now})
	stale, _ := db.CountStaleLDAP(now)
	active, _ := db.CountActiveLDAP()
	if stale != 2 || active != 3 {
		t.Errorf("stale=%d active=%d, want 2 e 3", stale, active)
	}
	names, _ := db.ListActiveLDAPNames()
	if len(names) != 3 || names["a"] != "a" {
		t.Errorf("names = %v", names)
	}
}

func TestSetContactAreaRespectsOverride(t *testing.T) {
	db := newTestDB(t)
	c := addContact(t, db, "u", "esterni")
	db.UpdateContactOverride("u", "u@x.it", "1")
	if err := db.SetContactArea(c.ID, "interni"); err != nil {
		t.Fatal(err)
	}
	if _, area, _ := db.ContactIDAndArea("u"); area != "esterni" {
		t.Errorf("area = %q, want esterni (manual_override)", area)
	}
}
