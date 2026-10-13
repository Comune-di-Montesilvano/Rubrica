package ldapserver

import (
	"path/filepath"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLoadSettingsDefaults(t *testing.T) {
	st := LoadSettings(newTestDB(t))
	if st.BaseDN != DefaultBaseDN || st.BindDN != DefaultBindDN || st.BindPassword != "" {
		t.Fatalf("default inattesi: %+v", st)
	}
}

func TestSaveSettings(t *testing.T) {
	db := newTestDB(t)
	if err := SaveSettings(db, " dc=comune,dc=local ", "cn=phone,dc=comune,dc=local", "segreta"); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	st := LoadSettings(db)
	if st.BaseDN != "dc=comune,dc=local" || st.BindDN != "cn=phone,dc=comune,dc=local" || st.BindPassword != "segreta" {
		t.Fatalf("valori salvati inattesi: %+v", st)
	}

	// password vuota = invariata, DN vuoti = default
	if err := SaveSettings(db, "", "", ""); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	st = LoadSettings(db)
	if st.BaseDN != DefaultBaseDN || st.BindDN != DefaultBindDN || st.BindPassword != "segreta" {
		t.Fatalf("atteso default DN e password invariata: %+v", st)
	}
}

func TestSaveSettingsInvalidDN(t *testing.T) {
	db := newTestDB(t)
	if err := SaveSettings(db, "non-un-dn", "", "x"); err == nil {
		t.Fatal("atteso errore per base DN non valido")
	}
	if err := SaveSettings(db, "", "non-un-dn", "x"); err == nil {
		t.Fatal("atteso errore per bind DN non valido")
	}
	if st := LoadSettings(db); st.BindPassword != "" {
		t.Fatalf("con DN non valido non deve salvare nulla: %+v", st)
	}
}
