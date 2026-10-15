package ldap

import (
	"reflect"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/go-ldap/ldap/v3"
)

const testGroupDN = "cn=rubrica_dirigenti,ou=comune-ms,dc=x"

func entry(dn string, attrs map[string][]string) *ldap.Entry { return ldap.NewEntry(dn, attrs) }

func testTree() []arearules.Node {
	return []arearules.Node{
		{Key: "interni", Name: "Personale comunale", Rules: []arearules.Rule{{Kind: "ou", Value: "INTERNI"}}, Children: []arearules.Node{
			{Key: "dirigenti", Name: "Dirigenti", Rules: []arearules.Rule{{Kind: "adgroup", Value: testGroupDN}}},
			{Key: "pc", Name: "Protezione civile", Rules: []arearules.Rule{{Kind: "range", Value: "700-709"}}},
		}},
	}
}

func TestEvaluateEntriesAreasSubAreasDepartment(t *testing.T) {
	rossiDN := "CN=Mario Rossi,OU=Users,OU=INTERNI,DC=x"
	in := syncInput{
		Entries: []*ldap.Entry{
			entry(rossiDN, map[string][]string{"sAMAccountName": {"mario.rossi"}, "displayName": {"Mario Rossi"}, "telephoneNumber": {"700;701"}}),
			entry("CN=Esterno,OU=Users,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"est"}, "physicalDeliveryOfficeName": {"Ditta"}}),
		},
		GroupMembers: map[string]map[string]bool{testGroupDN: {arearules.CanonicalDNLoose(rossiDN): true}},
		Tree:         testTree(),
	}
	out, st := evaluateEntries(in, time.Now())
	if len(out) != 2 || st.Total != 2 {
		t.Fatalf("out=%d stats=%+v", len(out), st)
	}
	r := out[0]
	if r.Contact.Area != "interni" || r.Contact.Department != "Personale comunale" {
		t.Errorf("rossi area=%q dept=%q, want interni e nome area come reparto", r.Contact.Area, r.Contact.Department)
	}
	if !reflect.DeepEqual(r.SubAreas, []string{"dirigenti", "pc"}) {
		t.Errorf("rossi sub = %v", r.SubAreas)
	}
	if out[1].Contact.Area != "" || out[1].Contact.Department != "Ditta" || out[1].SubAreas != nil {
		t.Errorf("esterno = %+v %v, want senza area e reparto invariato", out[1].Contact, out[1].SubAreas)
	}
}

func TestEvaluateEntriesScope(t *testing.T) {
	in := syncInput{
		Entries: []*ldap.Entry{
			entry("CN=A,OU=INTERNI,DC=x", map[string][]string{"sAMAccountName": {"a"}}),
			entry("CN=B,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"b"}}),
			entry("CN=C,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"c"}, "userAccountControl": {"514"}}),
		},
		Tree:       testTree(),
		Scope:      []arearules.Rule{{Kind: "ou", Value: "INTERNI"}},
		OnlyActive: true,
	}
	out, st := evaluateEntries(in, time.Now())
	var uids []string
	for _, e := range out {
		uids = append(uids, e.Contact.UID)
	}
	if !reflect.DeepEqual(uids, []string{"a", "c"}) || st.OutOfScope != 1 || st.Disabled != 1 {
		t.Errorf("uids=%v stats=%+v, want a e c (disabled non filtrato dal perimetro), 1 fuori perimetro", uids, st)
	}
	if !out[1].Contact.Disabled {
		t.Error("c deve risultare disabled")
	}
}

func TestEvaluateEntriesNoScopeMeansEveryone(t *testing.T) {
	in := syncInput{Entries: []*ldap.Entry{entry("CN=B,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"b"}})}}
	if out, _ := evaluateEntries(in, time.Now()); len(out) != 1 {
		t.Errorf("senza regole di perimetro deve entrare: %d", len(out))
	}
}

func TestShouldHoldStale(t *testing.T) {
	cases := []struct {
		stale, active int
		want          bool
	}{
		{0, 100, false}, {4, 10, false}, {5, 100, false}, {21, 100, true}, {20, 100, false}, {5, 20, true},
	}
	for _, c := range cases {
		if got := shouldHoldStale(c.stale, c.active); got != c.want {
			t.Errorf("shouldHoldStale(%d,%d) = %v, want %v", c.stale, c.active, got, c.want)
		}
	}
}

func TestInChainFilterEscapes(t *testing.T) {
	got := inChainFilter("cn=a(b),dc=x")
	want := `(memberOf:1.2.840.113556.1.4.1941:=cn=a\28b\29,dc=x)`
	if got != want {
		t.Errorf("inChainFilter = %q, want %q", got, want)
	}
}

func TestSearchAttributesAddsRuleAttrs(t *testing.T) {
	got := searchAttributes([]string{"employeetype", "Mail"})
	seen := map[string]int{}
	for _, a := range got {
		seen[a]++
	}
	if seen["employeetype"] != 1 || seen["mail"] != 1 || seen["memberOf"] != 1 {
		t.Errorf("attributi = %v (mail già presente non va duplicato)", got)
	}
}

func TestEvaluateEntriesNotADGroup(t *testing.T) {
	rossiDN := "CN=Mario Rossi,OU=Users,OU=INTERNI,DC=x"
	tree := []arearules.Node{{Key: "interni", Rules: []arearules.Rule{{Kind: "ou", Value: "INTERNI"}}, Children: []arearules.Node{
		{Key: "dipendenti", Rules: []arearules.Rule{{Kind: "ou", Value: "INTERNI"}, {Kind: "notadgroup", Value: testGroupDN}}},
	}}}
	in := syncInput{
		Entries: []*ldap.Entry{
			entry(rossiDN, map[string][]string{"sAMAccountName": {"rossi"}}),
			entry("CN=Bianchi,OU=Users,OU=INTERNI,DC=x", map[string][]string{"sAMAccountName": {"bianchi"}}),
		},
		GroupMembers: map[string]map[string]bool{testGroupDN: {arearules.CanonicalDNLoose(rossiDN): true}},
		Tree:         tree,
	}
	if got := arearules.GroupDNs(tree, nil); len(got) != 1 {
		t.Fatalf("il sync deve risolvere i membri dei gruppi notadgroup: %v", got)
	}
	out, _ := evaluateEntries(in, time.Now())
	if out[0].SubAreas != nil || !reflect.DeepEqual(out[1].SubAreas, []string{"dipendenti"}) {
		t.Errorf("rossi=%v bianchi=%v, want rossi escluso (dirigente), bianchi dipendente", out[0].SubAreas, out[1].SubAreas)
	}
}
