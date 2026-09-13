package ldap

import "testing"

func TestDeriveArea(t *testing.T) {
	cases := []struct {
		name string
		dn   string
		want string
	}{
		{"interni", "CN=Mario Rossi,OU=Users,OU=INTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "interni"},
		{"esterni", "CN=Anna Bianchi,OU=Users,OU=ESTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "esterni"},
		{"politica", "CN=Corinna Sandias,OU=Users,OU=AREA_POLITICA,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", "politica"},
		{"ou sconosciuta", "CN=Guest,CN=Users,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it", ""},
		{"case insensitive", "cn=Test,ou=Users,ou=interni,ou=comune-ms,dc=intranet,dc=comune,dc=montesilvano,dc=pe,dc=it", "interni"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveArea(c.dn)
			if got != c.want {
				t.Errorf("deriveArea(%q) = %q, want %q", c.dn, got, c.want)
			}
		})
	}
}

func TestNormalizeDescription(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Agente di Polizi Locale", "Agente di Polizia Locale"},
		{"agente di polizia locale", "Agente di Polizia Locale"},
		{"Edliizia", "Edilizia"},
		{"Istuttore", "Istruttore"},
		{"  Dirigente  ", "Dirigente"},
		{"", ""},
		{"Assessore", "Assessore"},
		{"Multi   spazi   interni", "Multi spazi interni"},
	}
	for _, c := range cases {
		got := normalizeDescription(c.in)
		if got != c.want {
			t.Errorf("normalizeDescription(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
