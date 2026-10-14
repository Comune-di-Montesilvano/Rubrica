package ldap

import "testing"

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
