package ldapserver

import (
	"strings"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

// Filter valuta un filtro LDAP già compilato su una entry.
type Filter func(e *Entry) bool

// ParseFilter compila un filtro LDAP testuale (RFC 4515) in un predicato.
// Supportati: &, |, !, uguaglianza, substring, presenza — confronto
// case-insensitive (Unicode) sui valori. Gli altri tipi (>=, <=, ~=,
// extensible) non matchano mai: un telefono non li usa, e rispondere "nessun
// risultato" è più sicuro che un errore di protocollo.
func ParseFilter(s string) (Filter, error) {
	p, err := ldap.CompileFilter(s)
	if err != nil {
		return nil, err
	}
	return func(e *Entry) bool { return matchPacket(p, e) }, nil
}

func matchPacket(p *ber.Packet, e *Entry) bool {
	switch p.Tag {
	case ldap.FilterAnd:
		for _, c := range p.Children {
			if !matchPacket(c, e) {
				return false
			}
		}
		return true
	case ldap.FilterOr:
		for _, c := range p.Children {
			if matchPacket(c, e) {
				return true
			}
		}
		return false
	case ldap.FilterNot:
		if len(p.Children) != 1 {
			return false
		}
		return !matchPacket(p.Children[0], e)
	case ldap.FilterEqualityMatch:
		if len(p.Children) != 2 {
			return false
		}
		want := packetString(p.Children[1])
		for _, v := range e.Get(packetString(p.Children[0])) {
			if strings.EqualFold(v, want) {
				return true
			}
		}
		return false
	case ldap.FilterSubstrings:
		if len(p.Children) != 2 {
			return false
		}
		for _, v := range e.Get(packetString(p.Children[0])) {
			if matchSubstrings(strings.ToLower(v), p.Children[1].Children) {
				return true
			}
		}
		return false
	case ldap.FilterPresent:
		return len(e.Get(packetString(p))) > 0
	default:
		return false
	}
}

// matchSubstrings verifica v (già minuscolo) contro le parti initial/any/final
// di un filtro substring, nell'ordine in cui compaiono.
func matchSubstrings(v string, parts []*ber.Packet) bool {
	pos := 0
	for i, part := range parts {
		s := strings.ToLower(packetString(part))
		switch part.Tag {
		case ldap.FilterSubstringsInitial:
			if i != 0 || !strings.HasPrefix(v, s) {
				return false
			}
			pos = len(s)
		case ldap.FilterSubstringsAny:
			idx := strings.Index(v[pos:], s)
			if idx < 0 {
				return false
			}
			pos += idx + len(s)
		case ldap.FilterSubstringsFinal:
			if i != len(parts)-1 || !strings.HasSuffix(v[pos:], s) {
				return false
			}
			pos = len(v)
		}
	}
	return true
}

func packetString(p *ber.Packet) string {
	return ber.DecodeString(p.Data.Bytes())
}
