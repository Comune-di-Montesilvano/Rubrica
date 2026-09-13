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

func TestManualContactCRUD(t *testing.T) {
	db := newTestDB(t)

	c := &Contact{UID: "manual-mario-esterno", DisplayName: "Mario Esterno", Email: "mario@fornitore.it", Area: "esterni"}
	if err := db.CreateManualContact(c); err != nil {
		t.Fatalf("CreateManualContact failed: %v", err)
	}
	if c.ID == 0 {
		t.Error("CreateManualContact did not set ID")
	}
	if c.Source != "manual" {
		t.Errorf("Source = %q, want manual", c.Source)
	}

	got, err := db.GetContact("manual-mario-esterno")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got == nil || got.Source != "manual" {
		t.Fatalf("GetContact did not return the manual contact correctly: %+v", got)
	}

	manuals, err := db.ListManualContacts()
	if err != nil {
		t.Fatalf("ListManualContacts failed: %v", err)
	}
	if len(manuals) != 1 {
		t.Fatalf("got %d manual contacts, want 1", len(manuals))
	}

	c.DisplayName = "Mario Esterno (rinominato)"
	c.PrimaryNumber = "3331234567"
	if err := db.UpdateManualContact(c); err != nil {
		t.Fatalf("UpdateManualContact failed: %v", err)
	}
	got, _ = db.GetContact("manual-mario-esterno")
	if got.DisplayName != "Mario Esterno (rinominato)" || got.PrimaryNumber != "3331234567" {
		t.Errorf("UpdateManualContact did not persist changes: %+v", got)
	}

	if err := db.DeleteManualContact("manual-mario-esterno"); err != nil {
		t.Fatalf("DeleteManualContact failed: %v", err)
	}
	got, _ = db.GetContact("manual-mario-esterno")
	if got != nil {
		t.Error("contact should be gone after DeleteManualContact")
	}
}

func TestSoftDeleteStaleExcludesManualContacts(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-2 * time.Hour)

	ldapContact := &Contact{UID: "ldap1", DisplayName: "LDAP1", Area: "interni", LastSync: old}
	if err := db.UpsertContact(ldapContact); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	manual := &Contact{UID: "manual-x", DisplayName: "Manual X", Area: "esterni"}
	if err := db.CreateManualContact(manual); err != nil {
		t.Fatalf("CreateManualContact failed: %v", err)
	}
	// Il manuale ha last_sync = now (impostato da CreateManualContact),
	// ma un sync futuro potrebbe avvenire molto dopo: verifichiamo che
	// anche con last_sync artificialmente vecchio non venga toccato.
	if _, err := db.Exec(`UPDATE contacts SET last_sync = ? WHERE uid = ?`, old, manual.UID); err != nil {
		t.Fatalf("failed to backdate manual contact: %v", err)
	}

	n, err := db.SoftDeleteStale(time.Now())
	if err != nil {
		t.Fatalf("SoftDeleteStale failed: %v", err)
	}
	if n != 1 {
		t.Errorf("SoftDeleteStale removed %d contacts, want 1 (only the LDAP one)", n)
	}

	got, _ := db.GetContact("manual-x")
	if got == nil {
		t.Error("manual contact should survive SoftDeleteStale even with an old last_sync")
	}
}

func TestSoftDeleteStale(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-2 * time.Hour)
	fresh := time.Now()

	stale := &Contact{UID: "stale1", DisplayName: "Stale", Area: "interni", LastSync: old}
	kept := &Contact{UID: "kept1", DisplayName: "Kept", Area: "interni", LastSync: fresh}
	if err := db.UpsertContact(stale); err != nil {
		t.Fatalf("UpsertContact(stale1) failed: %v", err)
	}
	if err := db.UpsertContact(kept); err != nil {
		t.Fatalf("UpsertContact(kept1) failed: %v", err)
	}

	n, err := db.SoftDeleteStale(fresh)
	if err != nil {
		t.Fatalf("SoftDeleteStale failed: %v", err)
	}
	if n != 1 {
		t.Errorf("SoftDeleteStale returned %d, want 1", n)
	}

	got, err := db.GetContact("stale1")
	if err != nil {
		t.Fatalf("GetContact(stale1) failed: %v", err)
	}
	if got != nil {
		t.Errorf("stale1 should be soft-deleted (GetContact should return nil), got %+v", got)
	}

	got2, err := db.GetContact("kept1")
	if err != nil {
		t.Fatalf("GetContact(kept1) failed: %v", err)
	}
	if got2 == nil {
		t.Error("kept1 should still be active")
	}
}

func TestAreaCRUD(t *testing.T) {
	db := newTestDB(t)

	areas, err := db.ListAreas()
	if err != nil {
		t.Fatalf("ListAreas failed: %v", err)
	}
	if len(areas) != 4 {
		t.Fatalf("got %d seeded areas, want 4 (interni/esterni/politica/uffici)", len(areas))
	}

	a := &Area{Key: "estero", Name: "Estero"}
	if err := db.CreateArea(a); err != nil {
		t.Fatalf("CreateArea failed: %v", err)
	}
	if a.ID == 0 {
		t.Error("CreateArea did not set ID")
	}

	if err := db.RenameArea(a.ID, "Estero (nuovo)"); err != nil {
		t.Fatalf("RenameArea failed: %v", err)
	}
	areas, _ = db.ListAreas()
	found := false
	for _, x := range areas {
		if x.ID == a.ID && x.Name == "Estero (nuovo)" {
			found = true
		}
	}
	if !found {
		t.Error("RenameArea did not persist new name")
	}

	c := &Contact{UID: "ext1", DisplayName: "Ext1", Area: "estero", LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	if err := db.DeleteArea(a.ID); err != nil {
		t.Fatalf("DeleteArea failed: %v", err)
	}
	areas, _ = db.ListAreas()
	for _, x := range areas {
		if x.ID == a.ID {
			t.Error("area should be gone after DeleteArea")
		}
	}
	got, _ := db.GetContact("ext1")
	if got == nil {
		t.Fatal("contact should still exist after its area is deleted")
	}
	if got.Area != "" {
		t.Errorf("contact.Area = %q after DeleteArea, want empty", got.Area)
	}
}

func TestCountByArea(t *testing.T) {
	db := newTestDB(t)
	contacts := []*Contact{
		{UID: "a", DisplayName: "A", Area: "interni", PrimaryNumber: "1", LastSync: time.Now()},
		{UID: "b", DisplayName: "B", Area: "interni", PrimaryNumber: "2", LastSync: time.Now()},
		{UID: "c", DisplayName: "C", Area: "esterni", Email: "c@example.com", LastSync: time.Now()},
		{UID: "d", DisplayName: "D", Area: "politica", LDAPExt: "100", LastSync: time.Now()},
		{UID: "e", DisplayName: "E (senza recapiti)", Area: "politica", LastSync: time.Now()},
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
	// "e" ha area=politica ma nessun recapito: non deve essere contata,
	// altrimenti il conteggio sidebar non combacia con ListContacts.
	want := map[string]int{"interni": 2, "esterni": 1, "politica": 1}
	for area, wantCount := range want {
		if counts[area] != wantCount {
			t.Errorf("counts[%q] = %d, want %d", area, counts[area], wantCount)
		}
	}
}

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

func TestUpsertPBXGroupCreatesAndUpdates(t *testing.T) {
	db := newTestDB(t)

	g, err := db.UpsertPBXGroup("500", "Gruppo Test", "", false, "")
	if err != nil {
		t.Fatalf("UpsertPBXGroup failed: %v", err)
	}
	if g.Source != "pbx" || g.Name != "Gruppo Test" {
		t.Fatalf("g = %+v, unexpected", g)
	}

	g2, err := db.UpsertPBXGroup("500", "Gruppo Rinominato Dal Centralino", "", false, "")
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
	g, err := db.UpsertPBXGroup("501", "Nome Migrato", "", true, "")
	if err != nil {
		t.Fatalf("UpsertPBXGroup failed: %v", err)
	}
	if !g.NameOverride {
		t.Fatal("NameOverride dovrebbe essere true come passato alla creazione")
	}

	g2, err := db.UpsertPBXGroup("501", "Nome Nuovo Dal Centralino", "", false, "")
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
	if _, err := db.UpsertPBXGroup("2", "PBX", "", false, ""); err != nil {
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
