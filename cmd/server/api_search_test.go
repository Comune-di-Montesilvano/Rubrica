package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

func apiSearch(t *testing.T, q string) []apiContact {
	t.Helper()
	rec := httptest.NewRecorder()
	handleAPISearch(rec, httptest.NewRequest("GET", "/api/cerca?q="+q, nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type %q", ct)
	}
	var out []apiContact
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, rec.Body)
	}
	return out
}

func setupAPI(t *testing.T) {
	t.Helper()
	setupHandlerEnv(t)
	pbService = phonebook.NewService(db)
}

func addContact(t *testing.T, c database.Contact) {
	t.Helper()
	if err := db.UpsertContact(&c); err != nil {
		t.Fatal(err)
	}
}

func TestAPISearchShortQuery(t *testing.T) {
	setupAPI(t)
	addContact(t, database.Contact{UID: "arossi", DisplayName: "Anna Rossi", Email: "arossi@example.it"})
	if got := apiSearch(t, "a"); got == nil || len(got) != 0 {
		t.Fatalf("atteso [], ottenuto %#v", got)
	}
}

func TestAPISearchFields(t *testing.T) {
	setupAPI(t)
	addContact(t, database.Contact{UID: "mrossi", DisplayName: "Mario Rossi", Email: "mrossi@example.it", LDAPExt: "123;124", Department: "Tributi"})
	addContact(t, database.Contact{UID: "lbianchi", DisplayName: "Luca Bianchi", PrimaryNumber: "0850000000", Department: "Anagrafe"})
	got := apiSearch(t, "rossi")
	if len(got) != 1 || got[0] != (apiContact{UID: "mrossi", Nome: "Mario Rossi", Reparto: "Tributi", Interno: "123, 124", Mail: "mrossi@example.it"}) {
		t.Fatalf("rossi: %#v", got)
	}
	got = apiSearch(t, "bianchi")
	if len(got) != 1 || got[0].Interno != "0850000000" {
		t.Fatalf("bianchi: %#v", got)
	}
}

func TestAPISearchLimit(t *testing.T) {
	setupAPI(t)
	for i := 0; i < 10; i++ {
		addContact(t, database.Contact{UID: fmt.Sprintf("prova%d", i), DisplayName: fmt.Sprintf("Prova %d", i), Email: fmt.Sprintf("prova%d@example.it", i)})
	}
	if got := apiSearch(t, "prova"); len(got) != 8 {
		t.Fatalf("attese 8 voci, ottenute %d", len(got))
	}
}

func TestAPISearchDisabled(t *testing.T) {
	setupAPI(t)
	addContact(t, database.Contact{UID: "vverdi", DisplayName: "Vera Verdi", Email: "vverdi@example.it", Disabled: true})
	addContact(t, database.Contact{UID: "uverdi", DisplayName: "Ugo Verdi", Email: "uverdi@example.it"})
	if got := apiSearch(t, "verdi"); len(got) != 1 || got[0].UID != "uverdi" {
		t.Fatalf("disattivato in elenco: %#v", got)
	}
}
