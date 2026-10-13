package ldapserver

import (
	"reflect"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

const testBase = "dc=rubrica,dc=local"

func findEntry(entries []*Entry, dn string) *Entry {
	for _, e := range entries {
		if e.DN == dn {
			return e
		}
	}
	return nil
}

func TestBuildEntriesContacts(t *testing.T) {
	now := time.Now()
	contacts := []*database.Contact{
		{UID: "mrossi", DisplayName: "Mario Rossi", LDAPExt: "700; 701 ;", Department: "Ragioneria", Title: "Istruttore", Email: "m.rossi@example.it"},
		{UID: "noext", DisplayName: "Senza Interno", LDAPExt: ""},
		{UID: "cher", DisplayName: "Cher", LDAPExt: "800"},
		{UID: "off", DisplayName: "Disattivo", LDAPExt: "900", Disabled: true},
		{UID: "del", DisplayName: "Cancellato", LDAPExt: "901", DeletedAt: &now},
		{UID: "a,b", DisplayName: "", LDAPExt: "902"},
	}
	entries := BuildEntries(testBase, contacts, nil)

	rossi := findEntry(entries, "uid=mrossi,ou=contatti,"+testBase)
	if rossi == nil {
		t.Fatal("entry mrossi mancante")
	}
	want := map[string][]string{
		"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
		"uid":             {"mrossi"},
		"cn":              {"Mario Rossi"},
		"displayName":     {"Mario Rossi"},
		"sn":              {"Rossi"},
		"givenName":       {"Mario"},
		"telephoneNumber": {"700", "701"},
		"ou":              {"Ragioneria"},
		"title":           {"Istruttore"},
		"mail":            {"m.rossi@example.it"},
	}
	if !reflect.DeepEqual(rossi.Attrs, want) {
		t.Errorf("attributi mrossi = %v\natteso %v", rossi.Attrs, want)
	}

	cher := findEntry(entries, "uid=cher,ou=contatti,"+testBase)
	if cher == nil {
		t.Fatal("entry cher mancante")
	}
	if got := cher.Get("sn"); !reflect.DeepEqual(got, []string{"Cher"}) {
		t.Errorf("sn di nome singolo = %v, atteso [Cher]", got)
	}
	if got := cher.Get("givenName"); got != nil {
		t.Errorf("givenName di nome singolo = %v, atteso assente", got)
	}
	if cher.Get("ou") != nil || cher.Get("mail") != nil || cher.Get("title") != nil {
		t.Error("attributi vuoti devono essere omessi")
	}

	for _, dn := range []string{
		"uid=noext,ou=contatti," + testBase,
		"uid=off,ou=contatti," + testBase,
		"uid=del,ou=contatti," + testBase,
	} {
		if findEntry(entries, dn) != nil {
			t.Errorf("entry %s non doveva esserci", dn)
		}
	}

	comma := findEntry(entries, `uid=a\,b,ou=contatti,`+testBase)
	if comma == nil {
		t.Fatal("entry con uid da escapare mancante")
	}
	if got := comma.Get("cn"); !reflect.DeepEqual(got, []string{"a,b"}) {
		t.Errorf("cn senza DisplayName = %v, atteso uid", got)
	}
}

func TestBuildEntriesGroups(t *testing.T) {
	active := &database.Contact{UID: "mrossi", DisplayName: "Mario Rossi"}
	disabled := &database.Contact{UID: "off", DisplayName: "Off", Disabled: true}
	groups := []*phonebook.GroupWithMembers{
		{Group: &database.GroupNumber{Number: "600", Name: "Reception"}, Members: []*database.Contact{active, disabled}},
		{Group: &database.GroupNumber{Number: "601", Name: "Tutti disattivi"}, Members: []*database.Contact{disabled}},
		{Group: &database.GroupNumber{Number: "602", Name: "Vuoto"}},
		{Group: &database.GroupNumber{Number: "603", Name: ""}, Members: []*database.Contact{active}},
	}
	entries := BuildEntries(testBase, nil, groups)

	rec := findEntry(entries, "uid=gruppo-600,ou=gruppi,"+testBase)
	if rec == nil {
		t.Fatal("entry gruppo 600 mancante")
	}
	want := map[string][]string{
		"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
		"uid":             {"gruppo-600"},
		"cn":              {"Reception"},
		"displayName":     {"Reception"},
		"sn":              {"Reception"},
		"telephoneNumber": {"600"},
	}
	if !reflect.DeepEqual(rec.Attrs, want) {
		t.Errorf("attributi gruppo = %v\natteso %v", rec.Attrs, want)
	}
	if findEntry(entries, "uid=gruppo-601,ou=gruppi,"+testBase) != nil {
		t.Error("gruppo con soli membri disattivi non doveva esserci")
	}
	if findEntry(entries, "uid=gruppo-602,ou=gruppi,"+testBase) != nil {
		t.Error("gruppo senza membri non doveva esserci")
	}
	noName := findEntry(entries, "uid=gruppo-603,ou=gruppi,"+testBase)
	if noName == nil || !reflect.DeepEqual(noName.Get("cn"), []string{"603"}) {
		t.Error("gruppo senza nome deve usare il numero come cn")
	}
}

func TestBuildEntriesStructureAndOrder(t *testing.T) {
	contacts := []*database.Contact{
		{UID: "z", DisplayName: "zeta Zeta", LDAPExt: "1"},
		{UID: "a", DisplayName: "Alfa Alfa", LDAPExt: "2"},
	}
	groups := []*phonebook.GroupWithMembers{
		{Group: &database.GroupNumber{Number: "600", Name: "Mensa"}, Members: []*database.Contact{{UID: "x"}}},
	}
	entries := BuildEntries(testBase, contacts, groups)

	var dns []string
	for _, e := range entries {
		dns = append(dns, e.DN)
	}
	want := []string{
		testBase,
		"ou=contatti," + testBase,
		"ou=gruppi," + testBase,
		"uid=a,ou=contatti," + testBase,
		"uid=gruppo-600,ou=gruppi," + testBase,
		"uid=z,ou=contatti," + testBase,
	}
	if !reflect.DeepEqual(dns, want) {
		t.Errorf("ordine DN = %v\natteso %v", dns, want)
	}
	if got := entries[0].Get("dc"); !reflect.DeepEqual(got, []string{"rubrica"}) {
		t.Errorf("attributo RDN della base = %v, atteso [rubrica]", got)
	}
	if got := entries[1].Get("objectClass"); !reflect.DeepEqual(got, []string{"top", "organizationalUnit"}) {
		t.Errorf("objectClass ou = %v", got)
	}
}
