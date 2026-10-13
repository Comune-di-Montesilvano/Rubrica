package ldapserver

import (
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/jimlambrt/gldap"
)

// escapeDNValue fa l'escape di un valore da usare in un RDN (RFC 4514):
// i caratteri speciali ovunque, spazio/# in testa e spazio in coda.
func escapeDNValue(v string) string {
	var b strings.Builder
	last := len(v) - 1
	for i, r := range v {
		if strings.ContainsRune(`,+"\<>;=`, r) ||
			(i == 0 && (r == ' ' || r == '#')) ||
			(i == last && r == ' ') {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dnEqual confronta due DN ignorando maiuscole e spazi tra componenti.
// Un DN vuoto o non valido non è mai uguale a nulla.
func dnEqual(a, b string) bool {
	da, err := ldap.ParseDN(a)
	if err != nil || len(da.RDNs) == 0 {
		return false
	}
	db, err := ldap.ParseDN(b)
	if err != nil {
		return false
	}
	return da.EqualFold(db)
}

// inScope dice se dn rientra nello scope di una search con base base.
func inScope(dn, base *ldap.DN, scope gldap.Scope) bool {
	switch scope {
	case gldap.BaseObject:
		return dn.EqualFold(base)
	case gldap.SingleLevel:
		return len(dn.RDNs) == len(base.RDNs)+1 && base.AncestorOfFold(dn)
	default:
		return dn.EqualFold(base) || base.AncestorOfFold(dn)
	}
}
