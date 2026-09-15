package pbx

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newTestDB(t *testing.T) *database.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestApplyPeersExcludesDomainExtensions(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&database.Contact{UID: "mario.rossi", DisplayName: "Mario Rossi", LDAPExt: "100", LastSync: time.Now()}); err != nil {
		t.Fatalf("seed ldap contact: %v", err)
	}

	peers := []Peer{
		{Extension: "100", CallerID: "MARIO ROSSI (PBX)"},
		{Extension: "200", CallerID: "GRUPPO TEST"},
	}
	applied, err := ApplyPeers(db, peers, time.Now())
	if err != nil {
		t.Fatalf("ApplyPeers failed: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1 (solo il peer non in dominio)", applied)
	}

	if got, _ := db.GetContact("pbx-100"); got != nil {
		t.Error("peer 100 è già in dominio, non deve diventare un contatto pbx")
	}
	got, err := db.GetContact("pbx-200")
	if err != nil || got == nil {
		t.Fatalf("peer 200 doveva diventare un contatto pbx: %v", err)
	}
	if got.DisplayName != "GRUPPO TEST" || got.Source != "pbx" {
		t.Errorf("contatto pbx-200 = %+v, unexpected", got)
	}
}

func TestApplyPeersSoftDeletesDisappearedPeers(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-2 * time.Hour)
	if _, err := ApplyPeers(db, []Peer{{Extension: "300", CallerID: "SPARISCE"}}, old); err != nil {
		t.Fatalf("first ApplyPeers failed: %v", err)
	}
	if got, _ := db.GetContact("pbx-300"); got == nil {
		t.Fatal("pbx-300 dovrebbe esistere dopo il primo giro")
	}

	if _, err := ApplyPeers(db, []Peer{}, time.Now()); err != nil {
		t.Fatalf("second ApplyPeers failed: %v", err)
	}
	if got, _ := db.GetContact("pbx-300"); got != nil {
		t.Error("pbx-300 doveva essere soft-deleted, non compare più tra i peer del centralino")
	}
}

func TestApplyPeersFallsBackToGroupCategoryDepartment(t *testing.T) {
	db := newTestDB(t)
	start, end := 250, 299
	if err := db.CreateGroupCategory(&database.GroupCategory{Key: "politica", Name: "Politica", RangeStart: &start, RangeEnd: &end}); err != nil {
		t.Fatalf("CreateGroupCategory failed: %v", err)
	}

	if _, err := ApplyPeers(db, []Peer{{Extension: "282", CallerID: "RICCARDO ROSSI"}}, time.Now()); err != nil {
		t.Fatalf("ApplyPeers failed: %v", err)
	}

	got, err := db.GetContact("pbx-282")
	if err != nil || got == nil {
		t.Fatalf("peer 282 doveva diventare un contatto pbx: %v", err)
	}
	if got.Department != "Politica" {
		t.Errorf("Department = %q, want %q (dalla categoria chiamate, non piu' \"Centralino - non mappato\")", got.Department, "Politica")
	}
	if got.Area != "" {
		t.Errorf("Area = %q, want vuota — la categoria chiamate non assegna mai contacts.area", got.Area)
	}
}

func TestApplyCallGroupsCreatesAndLinksMembers(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertContact(&database.Contact{UID: "u1", DisplayName: "U1", LDAPExt: "700", LastSync: time.Now()}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := ApplyPeers(db, []Peer{{Extension: "701", CallerID: "PEER 701"}}, time.Now()); err != nil {
		t.Fatalf("seed peer: %v", err)
	}

	groups := []CallGroup{
		{Name: "Gruppo Test", Extension: "500", Members: []string{"700", "701"}, Strategy: "ringall", Timeout: "20", Enabled: true},
	}
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("ApplyCallGroups failed: %v", err)
	}

	group, err := db.GetGroupByNumber("500")
	if err != nil || group == nil {
		t.Fatalf("gruppo 500 non trovato: %v", err)
	}
	if group.Name != "Gruppo Test" || group.Source != "pbx" {
		t.Errorf("group = %+v, unexpected", group)
	}

	members, err := db.GetGroupMembers(group.ID)
	if err != nil {
		t.Fatalf("GetGroupMembers failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
}

func TestApplyCallGroupsMigratesManualNameAsOverride(t *testing.T) {
	db := newTestDB(t)
	manual := &database.GroupNumber{Number: "9", Name: "Centralino Comunale"}
	if err := db.CreateGroup(manual); err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	groups := []CallGroup{
		{Name: "Centralino", Extension: "9", Members: []string{}, Strategy: "ringall", Timeout: "20", Enabled: true},
	}
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("ApplyCallGroups failed: %v", err)
	}

	got, err := db.GetGroupByNumber("9")
	if err != nil || got == nil {
		t.Fatalf("gruppo 9 non trovato dopo la migrazione: %v", err)
	}
	if got.Source != "pbx" {
		t.Errorf("Source = %q, want pbx", got.Source)
	}
	if got.Name != "Centralino Comunale" {
		t.Errorf("Name = %q, want il nome manuale migrato %q", got.Name, "Centralino Comunale")
	}
	if !got.NameOverride {
		t.Error("NameOverride dovrebbe essere true dopo la migrazione")
	}

	groups[0].Name = "Centralino V2"
	if err := ApplyCallGroups(db, groups); err != nil {
		t.Fatalf("second ApplyCallGroups failed: %v", err)
	}
	got2, _ := db.GetGroupByNumber("9")
	if got2.Name != "Centralino Comunale" {
		t.Errorf("Name = %q dopo secondo giro, want invariato %q", got2.Name, "Centralino Comunale")
	}
}

func TestLoadPBXConfigEmptyWhenNotSet(t *testing.T) {
	db := newTestDB(t)

	url, user, pass := LoadPBXConfig(db)
	if url != "" || user != "" || pass != "" {
		t.Errorf("LoadPBXConfig senza config salvata = (%q,%q,%q), want tutto vuoto", url, user, pass)
	}
}

func TestLoadPBXConfigReadsFromDB(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetConfig(PBXURLConfigKey, "https://192.0.2.10"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if err := db.SetConfig(PBXUserConfigKey, "admin"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if err := db.SetConfig(PBXPassConfigKey, "secret"); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}

	url, user, pass := LoadPBXConfig(db)
	if url != "https://192.0.2.10" || user != "admin" || pass != "secret" {
		t.Errorf("LoadPBXConfig = (%q,%q,%q), want i valori salvati", url, user, pass)
	}
}

func TestFilterPeersExcludesPlaceholderNames(t *testing.T) {
	peers := []Peer{
		{Extension: "521", CallerID: " <521>"},
		{Extension: "100", CallerID: "MARIO ROSSI"},
		{Extension: "534", CallerID: "<534>"},
	}
	got := FilterPeers(peers, Filters{ExcludeUnnamed: true})
	if len(got) != 1 || got[0].Extension != "100" {
		t.Errorf("FilterPeers(ExcludeUnnamed=true) = %+v, want solo il peer 100", got)
	}

	got2 := FilterPeers(peers, Filters{ExcludeUnnamed: false})
	if len(got2) != 3 {
		t.Errorf("FilterPeers(ExcludeUnnamed=false) = %d peers, want 3 (nessun filtro)", len(got2))
	}
}

func TestFilterCallGroupsExcludesInactiveAndEmpty(t *testing.T) {
	groups := []CallGroup{
		{Name: "Attivo con membri", Extension: "1", Members: []string{"700"}, Enabled: true},
		{Name: "Disattivo", Extension: "2", Members: []string{"700"}, Enabled: false},
		{Name: "Senza membri", Extension: "3", Members: []string{}, Enabled: true},
	}

	got := FilterCallGroups(groups, Filters{ExcludeInactiveGroups: true, ExcludeEmptyGroups: true})
	if len(got) != 1 || got[0].Extension != "1" {
		t.Errorf("FilterCallGroups(entrambi i filtri) = %+v, want solo il gruppo 1", got)
	}

	got2 := FilterCallGroups(groups, Filters{})
	if len(got2) != 3 {
		t.Errorf("FilterCallGroups(nessun filtro) = %d gruppi, want 3", len(got2))
	}
}

func TestLoadFiltersDefaultsToTrue(t *testing.T) {
	db := newTestDB(t)
	f := LoadFilters(db)
	if !f.ExcludeUnnamed || !f.ExcludeInactiveGroups || !f.ExcludeEmptyGroups {
		t.Errorf("LoadFilters senza config salvata = %+v, want tutto true di default", f)
	}
}

func TestSaveAndLoadFilters(t *testing.T) {
	db := newTestDB(t)
	want := Filters{ExcludeUnnamed: false, ExcludeInactiveGroups: true, ExcludeEmptyGroups: false}
	if err := SaveFilters(db, want); err != nil {
		t.Fatalf("SaveFilters failed: %v", err)
	}

	got := LoadFilters(db)
	if got != want {
		t.Errorf("LoadFilters dopo SaveFilters = %+v, want %+v", got, want)
	}
}

func TestApplyCallGroupsDeletesDisappearedGroups(t *testing.T) {
	db := newTestDB(t)
	if err := ApplyCallGroups(db, []CallGroup{{Name: "Sparisce", Extension: "111", Members: []string{}}}); err != nil {
		t.Fatalf("first ApplyCallGroups failed: %v", err)
	}
	if got, _ := db.GetGroupByNumber("111"); got == nil {
		t.Fatal("gruppo 111 dovrebbe esistere dopo il primo giro")
	}

	if err := ApplyCallGroups(db, []CallGroup{}); err != nil {
		t.Fatalf("second ApplyCallGroups failed: %v", err)
	}
	if got, _ := db.GetGroupByNumber("111"); got != nil {
		t.Error("gruppo 111 doveva essere cancellato, non compare più tra i call group del centralino")
	}
}
