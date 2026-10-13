package ldapserver

import (
	"testing"

	"github.com/go-ldap/ldap/v3"
	"github.com/jimlambrt/gldap"
)

func TestEscapeDNValue(t *testing.T) {
	cases := map[string]string{
		"mrossi":  "mrossi",
		"a,b":     `a\,b`,
		"a+b=c":   `a\+b\=c`,
		" lead":   `\ lead`,
		"trail ":  `trail\ `,
		"#hash":   `\#hash`,
		`q"uo\te`: `q\"uo\\te`,
	}
	for in, want := range cases {
		if got := escapeDNValue(in); got != want {
			t.Errorf("escapeDNValue(%q) = %q, atteso %q", in, got, want)
		}
		if _, err := ldap.ParseDN("uid=" + escapeDNValue(in) + ",dc=x"); err != nil {
			t.Errorf("DN con valore %q non parsabile: %v", in, err)
		}
	}
}

func TestDNEqual(t *testing.T) {
	if !dnEqual("cn=Telefoni, dc=Rubrica,dc=local", "cn=telefoni,dc=rubrica,dc=local") {
		t.Error("atteso uguale ignorando maiuscole e spazi")
	}
	if dnEqual("cn=altro,dc=rubrica,dc=local", "cn=telefoni,dc=rubrica,dc=local") {
		t.Error("atteso diverso")
	}
	if dnEqual("", "cn=telefoni,dc=rubrica,dc=local") {
		t.Error("DN vuoto non deve essere uguale")
	}
	if dnEqual("garbage", "garbage") {
		t.Error("DN non valido non deve essere uguale")
	}
}

func TestInScope(t *testing.T) {
	base, _ := ldap.ParseDN("dc=rubrica,dc=local")
	ou, _ := ldap.ParseDN("ou=contatti,dc=rubrica,dc=local")
	person, _ := ldap.ParseDN("uid=mrossi,ou=contatti,dc=rubrica,dc=local")

	cases := []struct {
		name  string
		dn    *ldap.DN
		scope gldap.Scope
		want  bool
	}{
		{"base su se stessa", base, gldap.BaseObject, true},
		{"base su figlio", ou, gldap.BaseObject, false},
		{"one su figlio", ou, gldap.SingleLevel, true},
		{"one su nipote", person, gldap.SingleLevel, false},
		{"one su se stessa", base, gldap.SingleLevel, false},
		{"sub su se stessa", base, gldap.WholeSubtree, true},
		{"sub su nipote", person, gldap.WholeSubtree, true},
	}
	for _, tc := range cases {
		if got := inScope(tc.dn, base, tc.scope); got != tc.want {
			t.Errorf("%s: inScope = %v, atteso %v", tc.name, got, tc.want)
		}
	}
}
