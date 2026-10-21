package carddav

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func setup(t *testing.T) (*Server, []*database.Contact) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.CreateArea(&database.Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"})
	for _, c := range []*database.Contact{
		{UID: "a", DisplayName: "A", Email: "a@x.it", Area: "interni", LastSync: time.Now()},
		{UID: "b", DisplayName: "B", Email: "b@x.it", Area: "esterni", LastSync: time.Now()},
	} {
		db.UpsertContact(c)
	}
	id, _, _ := db.ContactIDAndArea("a")
	db.SetContactSubAreas(id, []string{"dirigenti"})
	all, _ := db.ListAllContacts(1000, 0)
	return NewServer(db, &config.Config{}), all
}

func uids(cs []*database.Contact) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.UID] = true
	}
	return m
}

func TestContactsForBook(t *testing.T) {
	s, all := setup(t)
	cases := []struct {
		book string
		ok   bool
		want []string
	}{
		{"all", true, []string{"a", "b"}},
		{"interni", true, []string{"a"}},
		{"dirigenti", true, []string{"a"}},
		{"esterni", true, []string{"b"}},
		{"fantasma", false, nil},
	}
	for _, c := range cases {
		got, ok, err := s.contactsForBook(c.book, all)
		if err != nil || ok != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.book, ok, err)
			continue
		}
		if !ok {
			continue
		}
		m := uids(got)
		if len(m) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.book, m, c.want)
		}
		for _, u := range c.want {
			if !m[u] {
				t.Errorf("%s: manca %s", c.book, u)
			}
		}
	}
	if name := s.bookDisplayName("dirigenti"); name != "Rubrica Dirigenti" {
		t.Errorf("bookDisplayName = %q", name)
	}
}
