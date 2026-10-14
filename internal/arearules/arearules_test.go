package arearules

import (
	"errors"
	"reflect"
	"testing"
)

const dnRossi = "CN=Mario Rossi,OU=Users,OU=INTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it"
const dnDirigenti = "CN=RUBRICA_DIRIGENTI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it"

func rossi() Profile {
	return Profile{
		Username:   "mario.rossi",
		DN:         dnRossi,
		Attrs:      map[string][]string{"description": {"Dirigente"}, "mail": {"m@x.it"}},
		Groups:     []string{CanonicalDNLoose(dnDirigenti)},
		Extensions: []string{"700", " 701 ", ""},
	}
}

func TestMemberKinds(t *testing.T) {
	p := rossi()
	cases := []struct {
		name  string
		rules []Rule
		want  bool
	}{
		{"nessuna regola", nil, false},
		{"ou in qualsiasi posizione", []Rule{{Kind: KindOU, Value: "INTERNI"}}, true},
		{"ou wrapper", []Rule{{Kind: KindOU, Value: "comune-ms"}}, true},
		{"ou assente", []Rule{{Kind: KindOU, Value: "ESTERNI"}}, false},
		{"attr maiuscole/spazi", []Rule{{Kind: KindAttr, Attr: "description", Value: " dirigente "}}, true},
		{"attr valore diverso", []Rule{{Kind: KindAttr, Attr: "description", Value: "Istruttore"}}, false},
		{"adgroup DN diverso nel case", []Rule{{Kind: KindADGroup, Value: "cn=rubrica_dirigenti,ou=comune-ms,dc=intranet,dc=comune,dc=montesilvano,dc=pe,dc=it"}}, true},
		{"range secondo interno con spazi", []Rule{{Kind: KindRange, Value: "701-701"}}, true},
		{"range fuori", []Rule{{Kind: KindRange, Value: "800-899"}}, false},
		{"user", []Rule{{Kind: KindUser, Value: "MARIO.ROSSI"}}, true},
		{"exclude vince", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindExclude, Value: "mario.rossi"}}, false},
		{"present ok", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindPresent, Attr: "mail"}}, true},
		{"present ko", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindPresent, Attr: "title"}}, false},
		{"absent ok", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindAbsent, Attr: "title"}}, true},
		{"solo requisiti non bastano", []Rule{{Kind: KindPresent, Attr: "mail"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Member(p, c.rules); got != c.want {
				t.Errorf("Member = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMemberRangeNonNumeric(t *testing.T) {
	p := Profile{Extensions: []string{"gw", "abc"}}
	if Member(p, []Rule{{Kind: KindRange, Value: "0-99999"}}) {
		t.Error("interni non numerici non devono corrispondere a un range")
	}
	if Member(p, []Rule{{Kind: KindRange, Value: "rotto"}}) {
		t.Error("range malformato non deve corrispondere")
	}
}

func TestMemberPBXProfile(t *testing.T) {
	p := Profile{Extensions: []string{"750"}}
	if !Member(p, []Rule{{Kind: KindRange, Value: "700-799"}}) {
		t.Error("profilo PBX deve corrispondere al range")
	}
	if Member(p, []Rule{{Kind: KindRange, Value: "700-799"}, {Kind: KindPresent, Attr: "mail"}}) {
		t.Error("present su profilo senza attributi deve fallire")
	}
	if !Member(p, []Rule{{Kind: KindRange, Value: "700-799"}, {Kind: KindAbsent, Attr: "mail"}}) {
		t.Error("absent su profilo senza attributi deve passare")
	}
	if Member(p, []Rule{{Kind: KindUser, Value: ""}}) {
		t.Error("utente vuoto non deve corrispondere a profilo senza username")
	}
}

func tree() []Node {
	return []Node{
		{Key: "esterni", Name: "Esterni", Rules: []Rule{{Kind: KindOU, Value: "ESTERNI"}}},
		{Key: "interni", Name: "Personale comunale", Rules: []Rule{{Kind: KindOU, Value: "INTERNI"}}, Children: []Node{
			{Key: "dirigenti", Name: "Dirigenti", Rules: []Rule{{Kind: KindADGroup, Value: dnDirigenti}}},
			{Key: "protciv", Name: "Protezione civile", Rules: []Rule{{Kind: KindRange, Value: "700-709"}}},
			{Key: "collab", Name: "Collaboratori", Rules: []Rule{{Kind: KindAttr, Attr: "description", Value: "Collaboratore"}}},
		}},
		{Key: "tutti", Name: "Tutti", Rules: []Rule{{Kind: KindOU, Value: "COMUNE-MS"}}, Children: []Node{
			{Key: "dirigenti2", Name: "Dirigenti bis", Rules: []Rule{{Kind: KindADGroup, Value: dnDirigenti}}},
		}},
	}
}

func TestResolvePrecedenceAndMultipleSubAreas(t *testing.T) {
	area, subs := Resolve(rossi(), tree())
	if area != "interni" {
		t.Fatalf("area = %q, want interni (prima area in ordine che corrisponde)", area)
	}
	if want := []string{"dirigenti", "protciv"}; !reflect.DeepEqual(subs, want) {
		t.Errorf("subAreas = %v, want %v (tutte quelle dell'area, in ordine; mai quelle di un'altra area)", subs, want)
	}
}

func TestResolveNoMatch(t *testing.T) {
	area, subs := Resolve(Profile{DN: "CN=x,CN=Users,DC=a"}, tree())
	if area != "" || subs != nil {
		t.Errorf("Resolve = (%q, %v), want (\"\", nil)", area, subs)
	}
}

func TestSubAreasForStoredArea(t *testing.T) {
	got := SubAreasFor(rossi(), tree(), "tutti")
	if want := []string{"dirigenti2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SubAreasFor(tutti) = %v, want %v", got, want)
	}
	if got := SubAreasFor(rossi(), tree(), ""); got != nil {
		t.Errorf("SubAreasFor(\"\") = %v, want nil", got)
	}
}

func TestGroupDNsAndAttributes(t *testing.T) {
	extra := []Rule{{Kind: KindPresent, Attr: "mail"}, {Kind: KindADGroup, Value: dnDirigenti}}
	if got := GroupDNs(tree(), extra); len(got) != 1 || got[0] != CanonicalDNLoose(dnDirigenti) {
		t.Errorf("GroupDNs = %v, want un solo DN canonico", got)
	}
	if got := Attributes(tree(), extra); !reflect.DeepEqual(got, []string{"description", "mail"}) {
		t.Errorf("Attributes = %v", got)
	}
}

func TestNormalize(t *testing.T) {
	ok := []struct {
		kind, attr, value string
		want              Rule
	}{
		{KindOU, "", " interni ", Rule{Kind: KindOU, Value: "INTERNI"}},
		{KindAttr, "Description", " Dirigente ", Rule{Kind: KindAttr, Attr: "description", Value: "Dirigente"}},
		{KindPresent, "mail", "ignorato", Rule{Kind: KindPresent, Attr: "mail"}},
		{KindADGroup, "", dnDirigenti, Rule{Kind: KindADGroup, Value: CanonicalDNLoose(dnDirigenti)}},
		{KindRange, "", " 700 - 799 ", Rule{Kind: KindRange, Value: "700-799"}},
		{KindUser, "", " Mario.Rossi ", Rule{Kind: KindUser, Value: "mario.rossi"}},
		{" EXCLUDE ", "", "x", Rule{Kind: KindExclude, Value: "x"}},
	}
	for _, c := range ok {
		got, err := Normalize(c.kind, c.attr, c.value)
		if err != nil || got != c.want {
			t.Errorf("Normalize(%q,%q,%q) = %+v, %v; want %+v", c.kind, c.attr, c.value, got, err, c.want)
		}
	}
	bad := [][3]string{
		{KindOU, "", ""},
		{KindAttr, "1bad", "x"},
		{KindAttr, "description", ""},
		{KindPresent, "a b", ""},
		{KindADGroup, "", "non un dn"},
		{KindRange, "", "799-700"},
		{KindRange, "", "abc"},
		{KindUser, "", " "},
		{"boh", "", "x"},
		{KindAttrName, "", "x"},
	}
	for _, b := range bad {
		if _, err := Normalize(b[0], b[1], b[2]); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("Normalize(%q,%q,%q) err = %v, want ErrInvalidRule", b[0], b[1], b[2], err)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := map[Rule]string{
		{Kind: KindOU, Value: "INTERNI"}:                          "OU INTERNI",
		{Kind: KindAttr, Attr: "description", Value: "Dirigente"}: "description = Dirigente",
		{Kind: KindADGroup, Value: CanonicalDNLoose(dnDirigenti)}: "Gruppo AD rubrica_dirigenti",
		{Kind: KindRange, Value: "700-799"}:                       "Interni 700–799",
		{Kind: KindUser, Value: "mario.rossi"}:                    "Utente mario.rossi",
		{Kind: KindExclude, Value: "stagista1"}:                   "Escludi stagista1",
		{Kind: KindPresent, Attr: "mail"}:                         "Richiede mail compilato",
		{Kind: KindAbsent, Attr: "title"}:                         "Richiede title vuoto",
	}
	for r, want := range cases {
		if got := Label(r); got != want {
			t.Errorf("Label(%+v) = %q, want %q", r, got, want)
		}
	}
}
