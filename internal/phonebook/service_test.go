package phonebook

import (
	"testing"

	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

func TestGroupByDepartment(t *testing.T) {
	contacts := []*ContactWithGroups{
		{Contact: &database.Contact{UID: "a", Department: "Polizia Locale"}},
		{Contact: &database.Contact{UID: "b", Department: "Polizia Locale"}},
		{Contact: &database.Contact{UID: "c", Department: "Urbanistica"}},
		{Contact: &database.Contact{UID: "d", Department: "", Area: "politica"}},
		{Contact: &database.Contact{UID: "e", Department: ""}},
	}

	groups := GroupByDepartment(contacts)

	if len(groups) != 4 {
		t.Fatalf("got %d groups, want 4", len(groups))
	}
	if groups[0].Name != "Amministrazione politica" {
		t.Errorf("groups[0].Name = %q, want alphabetically-first \"Amministrazione politica\"", groups[0].Name)
	}
	for i := 1; i < len(groups); i++ {
		if groups[i-1].Name > groups[i].Name {
			t.Errorf("groups not alphabetically sorted: %q before %q", groups[i-1].Name, groups[i].Name)
		}
	}

	names := map[string]int{}
	for _, g := range groups {
		names[g.Name] = len(g.Contacts)
	}
	if names["Amministrazione politica"] != 1 {
		t.Errorf("Amministrazione politica count = %d, want 1", names["Amministrazione politica"])
	}
	if names["Senza reparto"] != 1 {
		t.Errorf("Senza reparto count = %d, want 1", names["Senza reparto"])
	}
	if names["Urbanistica"] != 1 {
		t.Errorf("Urbanistica count = %d, want 1", names["Urbanistica"])
	}
}

func TestGroupByDepartmentEmptyInput(t *testing.T) {
	groups := GroupByDepartment(nil)
	if len(groups) != 0 {
		t.Errorf("got %d groups for nil input, want 0", len(groups))
	}
}
