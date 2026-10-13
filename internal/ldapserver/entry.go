// Package ldapserver espone la rubrica ai telefoni VoIP come server LDAPv3
// read-only (vedi docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md).
// Distinto da internal/ldap, che è il client verso AD.
package ldapserver

import "strings"

// Entry è una entry LDAP costruita in memoria. Le chiavi di Attrs usano la
// grafia canonica (es. "telephoneNumber"), restituita così ai client; il
// lookup via Get è case-insensitive come da RFC 4512.
type Entry struct {
	DN    string
	Attrs map[string][]string
}

// Get restituisce i valori dell'attributo name (confronto case-insensitive
// sul nome), nil se l'entry non ha quell'attributo.
func (e *Entry) Get(name string) []string {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}
