package phonebook

import (
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
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

func TestGroupByDepartmentMergesCaseInsensitive(t *testing.T) {
	contacts := []*ContactWithGroups{
		{Contact: &database.Contact{UID: "ldap1", Department: "PALACONGRESSI", Source: "ldap"}},
		{Contact: &database.Contact{UID: "pbx1", Department: "Palacongressi", Source: "pbx"}},
		{Contact: &database.Contact{UID: "pbx2", Department: "palacongressi", Source: "pbx"}},
	}

	groups := GroupByDepartment(contacts)

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1 (PALACONGRESSI/Palacongressi/palacongressi devono fondersi)", len(groups))
	}
	if len(groups[0].Contacts) != 3 {
		t.Errorf("got %d contacts nel gruppo fuso, want 3", len(groups[0].Contacts))
	}
}

func TestGroupByDepartmentEmptyInput(t *testing.T) {
	groups := GroupByDepartment(nil)
	if len(groups) != 0 {
		t.Errorf("got %d groups for nil input, want 0", len(groups))
	}
}

func TestBuildGroupCategoryTree(t *testing.T) {
	ufficiStart, ufficiEnd := 400, 499
	settoreStart, settoreEnd := 401, 404
	dirigentiStart, dirigentiEnd := 300, 350

	uffici := &database.GroupCategory{ID: 1, Key: "uffici", Name: "Uffici", RangeStart: &ufficiStart, RangeEnd: &ufficiEnd}
	settoreVI := &database.GroupCategory{ID: 2, Key: "settore_vi", Name: "Settore VI - Legale", ParentID: &uffici.ID, RangeStart: &settoreStart, RangeEnd: &settoreEnd}
	dirigenti := &database.GroupCategory{ID: 3, Key: "dirigenti", Name: "Dirigenti", RangeStart: &dirigentiStart, RangeEnd: &dirigentiEnd}
	// Categoria senza alcun gruppo assegnato: non deve comparire nell'albero.
	vuota := &database.GroupCategory{ID: 4, Key: "vuota", Name: "Vuota"}

	categories := []*database.GroupCategory{uffici, settoreVI, dirigenti, vuota}

	groups := []*GroupWithMembers{
		{Group: &database.GroupNumber{Number: "401", Name: "Servizio difesa legale"}},
		{Group: &database.GroupNumber{Number: "405", Name: "Segretario Generale"}}, // in "Uffici" ma non in "Settore VI"
		{Group: &database.GroupNumber{Number: "310", Name: "Dirigente Settore III"}},
		{Group: &database.GroupNumber{Number: "900", Name: "COC"}}, // nessuna categoria
	}

	tree, uncategorized := BuildGroupCategoryTree(groups, categories)

	if len(tree) != 2 {
		t.Fatalf("got %d top-level nodes, want 2 (Dirigenti, Uffici)", len(tree))
	}
	// range_start crescente: Dirigenti (300) prima di Uffici (400).
	if tree[0].Category.Key != "dirigenti" || tree[1].Category.Key != "uffici" {
		t.Fatalf("top-level order = [%s, %s], want [dirigenti, uffici]", tree[0].Category.Key, tree[1].Category.Key)
	}

	dirigentiNode := tree[0]
	if len(dirigentiNode.Children) != 0 {
		t.Errorf("dirigenti should have no children, got %d", len(dirigentiNode.Children))
	}
	if len(dirigentiNode.Groups) != 1 || dirigentiNode.Groups[0].Group.Number != "310" {
		t.Errorf("dirigenti.Groups = %v, want [310]", dirigentiNode.Groups)
	}

	ufficiNode := tree[1]
	if len(ufficiNode.Groups) != 1 || ufficiNode.Groups[0].Group.Number != "405" {
		t.Errorf("uffici.Groups = %v, want [405] (401 va nel figlio settore_vi)", ufficiNode.Groups)
	}
	if len(ufficiNode.Children) != 1 || ufficiNode.Children[0].Category.Key != "settore_vi" {
		t.Fatalf("uffici.Children = %v, want [settore_vi]", ufficiNode.Children)
	}
	if len(ufficiNode.Children[0].Groups) != 1 || ufficiNode.Children[0].Groups[0].Group.Number != "401" {
		t.Errorf("settore_vi.Groups = %v, want [401]", ufficiNode.Children[0].Groups)
	}

	if len(uncategorized) != 1 || uncategorized[0].Group.Number != "900" {
		t.Fatalf("uncategorized = %v, want [900]", uncategorized)
	}
}

func TestBuildGroupCategoryTreeEmptyInput(t *testing.T) {
	tree, uncategorized := BuildGroupCategoryTree(nil, nil)
	if len(tree) != 0 || len(uncategorized) != 0 {
		t.Errorf("got tree=%v uncategorized=%v for nil input, want both empty", tree, uncategorized)
	}
}
