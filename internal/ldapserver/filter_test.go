package ldapserver

import "testing"

func filterTestEntry() *Entry {
	return &Entry{
		DN: "uid=ndaddiego,ou=contatti,dc=rubrica,dc=local",
		Attrs: map[string][]string{
			"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
			"cn":              {"NICOLÒ D'ADDIEGO"},
			"sn":              {"D'ADDIEGO"},
			"telephoneNumber": {"700", "759"},
		},
	}
}

func TestParseFilter(t *testing.T) {
	cases := []struct {
		filter string
		want   bool
	}{
		{"(telephoneNumber=759)", true},
		{"(telephoneNumber=700)", true},
		{"(telephoneNumber=75)", false},
		{"(TELEPHONENUMBER=759)", true},
		{"(cn=*addiego*)", true},
		{"(cn=nicol*)", true},
		{"(cn=*nicolò*)", true},
		{"(sn=*d\\27add*)", true},
		{"(cn=nicolò d'addiego)", true},
		{"(cn=rossi)", false},
		{"(cn=*o*d*o)", true},
		{"(cn=n*z*)", false},
		{"(cn=*iego)", true},
		{"(cn=*nicol)", false},
		{"(mobile=759)", false},
		{"(|(telephoneNumber=759)(mobile=759))", true},
		{"(|(cn=*zzz*)(sn=*zzz*))", false},
		{"(&(objectClass=person)(cn=*addi*))", true},
		{"(&(objectClass=person)(cn=*zzz*))", false},
		{"(!(cn=*zzz*))", true},
		{"(!(cn=*addi*))", false},
		{"(objectClass=*)", true},
		{"(mail=*)", false},
		{"(telephoneNumber>=700)", false},
		{"(cn~=nicolo)", false},
		// accenti e apostrofi ignorati: dalla tastiera del telefono non si digitano
		{"(cn=*nicolo*)", true},
		{"(cn=*daddiego*)", true},
		{"(sn=daddiego)", true},
		{"(cn=nicolo daddiego)", true},
		{"(sn=dadd*)", true},
		{"(cn=*nicolà*)", false},
	}
	e := filterTestEntry()
	for _, tc := range cases {
		f, err := ParseFilter(tc.filter)
		if err != nil {
			t.Fatalf("ParseFilter(%q) error: %v", tc.filter, err)
		}
		if got := f(e); got != tc.want {
			t.Errorf("filtro %q = %v, atteso %v", tc.filter, got, tc.want)
		}
	}
}

func TestParseFilterInvalid(t *testing.T) {
	if _, err := ParseFilter("(cn="); err == nil {
		t.Fatal("atteso errore per filtro malformato")
	}
}

func TestEntryGetCaseInsensitive(t *testing.T) {
	e := filterTestEntry()
	if got := e.Get("TelephoneNumber"); len(got) != 2 {
		t.Fatalf("Get(TelephoneNumber) = %v, attesi 2 valori", got)
	}
	if got := e.Get("mobile"); got != nil {
		t.Fatalf("Get(mobile) = %v, atteso nil", got)
	}
}
