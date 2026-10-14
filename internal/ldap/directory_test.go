package ldap

import (
	"reflect"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

func TestSubstringFilter(t *testing.T) {
	if got := substringFilter("cn", ""); got != "(cn=*)" {
		t.Errorf("q vuota = %q", got)
	}
	if got := substringFilter("cn", "a*(b)"); got != `(cn=*a\2a\28b\29*)` {
		t.Errorf("escape = %q", got)
	}
}

func TestComputeAttrStats(t *testing.T) {
	es := []*ldap.Entry{
		ldap.NewEntry("cn=a", map[string][]string{"description": {"Dirigente"}, "objectGUID": {"x"}, "employeeType": {"STD"}}),
		ldap.NewEntry("cn=b", map[string][]string{"description": {"Dirigente"}}),
		ldap.NewEntry("cn=c", map[string][]string{"description": {"Istruttore"}}),
	}
	got := computeAttrStats(es)
	if len(got) != 2 || got[0].Name != "description" || got[0].Count != 3 {
		t.Fatalf("stats = %+v (objectGUID tecnico va escluso)", got)
	}
	if !reflect.DeepEqual(got[0].Examples, []string{"Dirigente", "Istruttore"}) {
		t.Errorf("esempi = %v", got[0].Examples)
	}
}

func TestDiffScope(t *testing.T) {
	evals := []evaluated{
		{Contact: &database.Contact{UID: "a", DisplayName: "Anna"}},
		{Contact: &database.Contact{UID: "n", DisplayName: "Nuovo"}},
		{Contact: &database.Contact{UID: "d", DisplayName: "Disab", Disabled: true}},
	}
	current := map[string]string{"a": "Anna", "x": "Uscente"}
	p := diffScope(evals, current)
	if p.In != 2 || p.Current != 2 {
		t.Errorf("In=%d Current=%d, want 2 e 2 (disabled non conta)", p.In, p.Current)
	}
	if !reflect.DeepEqual(p.Entering, []string{"Nuovo"}) || !reflect.DeepEqual(p.Leaving, []string{"Uscente"}) {
		t.Errorf("entering=%v leaving=%v", p.Entering, p.Leaving)
	}
}
