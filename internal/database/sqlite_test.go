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
	if len(areas) != 3 {
		t.Fatalf("got %d seeded areas, want 3 (interni/esterni/politica)", len(areas))
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
