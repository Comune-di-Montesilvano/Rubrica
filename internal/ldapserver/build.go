package ldapserver

import (
	"sort"
	"strings"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
	"github.com/go-ldap/ldap/v3"
)

const (
	contactsOU = "contatti"
	groupsOU   = "gruppi"
)

var personObjectClass = []string{"top", "person", "organizationalPerson", "inetOrgPerson"}

// BuildEntries costruisce l'albero LDAP servito ai telefoni: prima le entry
// di struttura (base, ou=contatti, ou=gruppi), poi contatti e gruppi di
// chiamata ordinati per cn (case-insensitive). Perimetro = rubrica pubblica:
// contatti non cancellati e non disabled con almeno un interno, gruppi con
// almeno un membro attivo. Pubblica solo gli interni (telephoneNumber), mai
// PrimaryNumber: il caller ID che arriva ai telefoni è l'interno.
func BuildEntries(baseDN string, contacts []*database.Contact, groups []*phonebook.GroupWithMembers) []*Entry {
	out := structuralEntries(baseDN)

	var items []*Entry
	for _, c := range contacts {
		if e := contactEntry(baseDN, c); e != nil {
			items = append(items, e)
		}
	}
	for _, g := range groups {
		if e := groupEntry(baseDN, g); e != nil {
			items = append(items, e)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return strings.ToLower(items[i].Attrs["cn"][0]) < strings.ToLower(items[j].Attrs["cn"][0])
	})
	return append(out, items...)
}

func structuralEntries(baseDN string) []*Entry {
	baseAttrs := map[string][]string{"objectClass": {"top"}}
	if dn, err := ldap.ParseDN(baseDN); err == nil && len(dn.RDNs) > 0 {
		for _, a := range dn.RDNs[0].Attributes {
			baseAttrs[a.Type] = []string{a.Value}
		}
	}
	ou := func(name string) *Entry {
		return &Entry{
			DN:    "ou=" + name + "," + baseDN,
			Attrs: map[string][]string{"objectClass": {"top", "organizationalUnit"}, "ou": {name}},
		}
	}
	return []*Entry{{DN: baseDN, Attrs: baseAttrs}, ou(contactsOU), ou(groupsOU)}
}

func contactEntry(baseDN string, c *database.Contact) *Entry {
	if c.Disabled || c.DeletedAt != nil {
		return nil
	}
	exts := splitExtensions(c.LDAPExt)
	if len(exts) == 0 {
		return nil
	}
	name := strings.TrimSpace(c.DisplayName)
	if name == "" {
		name = c.UID
	}
	given, sn := splitName(name)
	attrs := map[string][]string{
		"objectClass":     personObjectClass,
		"uid":             {c.UID},
		"cn":              {name},
		"displayName":     {name},
		"sn":              {sn},
		"telephoneNumber": exts,
	}
	setIfNotEmpty(attrs, "givenName", given)
	setIfNotEmpty(attrs, "ou", c.Department)
	setIfNotEmpty(attrs, "title", c.Title)
	setIfNotEmpty(attrs, "mail", c.Email)
	return &Entry{
		DN:    "uid=" + escapeDNValue(c.UID) + ",ou=" + contactsOU + "," + baseDN,
		Attrs: attrs,
	}
}

func groupEntry(baseDN string, g *phonebook.GroupWithMembers) *Entry {
	number := strings.TrimSpace(g.Group.Number)
	if number == "" || len(g.ActiveMembers()) == 0 {
		return nil
	}
	name := strings.TrimSpace(g.Group.Name)
	if name == "" {
		name = number
	}
	uid := "gruppo-" + number
	return &Entry{
		DN: "uid=" + escapeDNValue(uid) + ",ou=" + groupsOU + "," + baseDN,
		Attrs: map[string][]string{
			"objectClass":     personObjectClass,
			"uid":             {uid},
			"cn":              {name},
			"displayName":     {name},
			"sn":              {name},
			"telephoneNumber": {number},
		},
	}
}

// splitName divide un nome visualizzato in givenName/sn usando l'ultima
// parola come cognome. È un'euristica (AD può avere "COGNOME NOME"): i
// telefoni cercano comunque su cn con wildcard, sn serve solo come fallback.
func splitName(name string) (given, sn string) {
	fields := strings.Fields(name)
	if len(fields) <= 1 {
		return "", name
	}
	return strings.Join(fields[:len(fields)-1], " "), fields[len(fields)-1]
}

// splitExtensions divide ldap_ext sui ";" (un contatto può avere più
// interni, vedi database.splitExtensions), scartando token vuoti.
func splitExtensions(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ";") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func setIfNotEmpty(attrs map[string][]string, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		attrs[key] = []string{value}
	}
}
