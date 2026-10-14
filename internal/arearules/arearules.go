// Package arearules decide l'area, le sotto-aree e il perimetro di un
// contatto a partire da regole configurate da admin (OU, attributi AD,
// gruppi AD, range di interni, utenti). Logica pura: nessun accesso a DB o
// LDAP, il chiamante costruisce il Profile. Modello ripreso dai "gruppi della
// plancia" di CruscottoPA (internal/audience).
package arearules

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

const (
	KindOU       = "ou"       // il DN contiene OU=<Value>
	KindAttr     = "attr"     // attributo Attr ha valore Value
	KindADGroup  = "adgroup"  // membro del gruppo AD Value (DN), annidati compresi
	KindRange    = "range"    // un interno in "da-a"
	KindUser     = "user"     // utente Value incluso
	KindExclude  = "exclude"  // utente Value escluso
	KindPresent  = "present"  // requisito: Attr compilato
	KindAbsent   = "absent"   // requisito: Attr vuoto
	KindAttrName = "attrname" // solo suggerimenti admin (nomi di attributo), mai salvato
)

// ErrInvalidRule: regola non salvabile (tipo sconosciuto, campo vuoto,
// attributo/DN/range malformato).
var ErrInvalidRule = errors.New("regola non valida")

type Rule struct{ Kind, Attr, Value string }

// Profile è ciò che serve sapere di un contatto per valutare le regole.
// Attrs ha chiavi minuscole; Groups contiene DN canonici (CanonicalDNLoose)
// dei soli gruppi citati nelle regole di cui il contatto è membro, annidati
// compresi. Contatti PBX e gruppi di chiamata hanno solo Extensions.
type Profile struct {
	Username   string
	DN         string
	Attrs      map[string][]string
	Groups     []string
	Extensions []string
}

// Node è un'area di primo livello (Children = sotto-aree) o una sotto-area.
// L'ordine della slice è la precedenza.
type Node struct {
	Key      string
	Name     string
	Rules    []Rule
	Children []Node
}

var attrNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

func ValidAttrName(s string) bool { return attrNameRe.MatchString(s) }

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Member: almeno una regola d'ingresso (ou/attr/adgroup/range/user)
// soddisfatta, tutti i requisiti (present/absent) rispettati e nessuna
// exclude. Nessuna regola = nessun membro.
func Member(p Profile, rules []Rule) bool {
	user := norm(p.Username)
	in := false
	for _, r := range rules {
		switch r.Kind {
		case KindExclude:
			if user != "" && norm(r.Value) == user {
				return false
			}
		case KindPresent, KindAbsent:
			if hasValue(p.Attrs[norm(r.Attr)]) != (r.Kind == KindPresent) {
				return false
			}
		case KindUser:
			if user != "" && norm(r.Value) == user {
				in = true
			}
		case KindAttr:
			for _, v := range p.Attrs[norm(r.Attr)] {
				if norm(v) == norm(r.Value) {
					in = true
				}
			}
		case KindADGroup:
			for _, g := range p.Groups {
				if SameDN(g, r.Value) {
					in = true
				}
			}
		case KindOU:
			if dnHasOU(p.DN, r.Value) {
				in = true
			}
		case KindRange:
			if extInRange(p.Extensions, r.Value) {
				in = true
			}
		}
	}
	return in
}

// Resolve: prima area di primo livello (in ordine) di cui p è membro, poi
// tutte le sue sotto-aree di cui p è membro, in ordine. ("", nil) se
// nessuna area corrisponde.
func Resolve(p Profile, tree []Node) (string, []string) {
	for _, n := range tree {
		if Member(p, n.Rules) {
			return n.Key, SubAreasFor(p, tree, n.Key)
		}
	}
	return "", nil
}

// SubAreasFor: sotto-aree dell'area areaKey di cui p è membro. Serve anche
// quando l'area salvata differisce da quella risolta (manual_override).
func SubAreasFor(p Profile, tree []Node, areaKey string) []string {
	if areaKey == "" {
		return nil
	}
	var out []string
	for _, n := range tree {
		if n.Key != areaKey {
			continue
		}
		for _, c := range n.Children {
			if Member(p, c.Rules) {
				out = append(out, c.Key)
			}
		}
	}
	return out
}

// FindNode cerca un'area o sotto-area per key (nil se assente).
func FindNode(tree []Node, key string) *Node {
	for i := range tree {
		if tree[i].Key == key {
			return &tree[i]
		}
		for j := range tree[i].Children {
			if tree[i].Children[j].Key == key {
				return &tree[i].Children[j]
			}
		}
	}
	return nil
}

func allRules(tree []Node, extra []Rule) []Rule {
	out := append([]Rule{}, extra...)
	for _, n := range tree {
		out = append(out, n.Rules...)
		for _, c := range n.Children {
			out = append(out, c.Rules...)
		}
	}
	return out
}

// GroupDNs: DN canonici distinti citati da regole adgroup (albero + extra),
// ordinati. Il sync risolve i membri annidati solo di questi.
func GroupDNs(tree []Node, extra []Rule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range allRules(tree, extra) {
		if r.Kind != KindADGroup {
			continue
		}
		dn := CanonicalDNLoose(r.Value)
		if dn != "" && !seen[dn] {
			seen[dn] = true
			out = append(out, dn)
		}
	}
	sort.Strings(out)
	return out
}

// Attributes: nomi attributo (minuscoli) distinti citati da regole
// attr/present/absent, ordinati: il sync li aggiunge alla ricerca AD.
func Attributes(tree []Node, extra []Rule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range allRules(tree, extra) {
		switch r.Kind {
		case KindAttr, KindPresent, KindAbsent:
			a := norm(r.Attr)
			if a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Normalize valida e porta in forma canonica una regola prima di salvarla.
func Normalize(kind, attr, value string) (Rule, error) {
	kind, attr, value = norm(kind), strings.TrimSpace(attr), strings.TrimSpace(value)
	switch kind {
	case KindOU:
		if value == "" {
			return Rule{}, fmt.Errorf("%w: OU vuota", ErrInvalidRule)
		}
		return Rule{Kind: kind, Value: strings.ToUpper(value)}, nil
	case KindAttr:
		if !ValidAttrName(attr) {
			return Rule{}, fmt.Errorf("%w: nome di attributo LDAP non valido", ErrInvalidRule)
		}
		if value == "" {
			return Rule{}, fmt.Errorf("%w: valore vuoto", ErrInvalidRule)
		}
		return Rule{Kind: kind, Attr: strings.ToLower(attr), Value: value}, nil
	case KindPresent, KindAbsent:
		if !ValidAttrName(attr) {
			return Rule{}, fmt.Errorf("%w: nome di attributo LDAP non valido", ErrInvalidRule)
		}
		return Rule{Kind: kind, Attr: strings.ToLower(attr)}, nil
	case KindADGroup:
		dn, err := CanonicalDN(value)
		if err != nil {
			return Rule{}, fmt.Errorf("%w: DN del gruppo non valido", ErrInvalidRule)
		}
		return Rule{Kind: kind, Value: dn}, nil
	case KindRange:
		lo, hi, err := ParseRange(value)
		if err != nil {
			return Rule{}, fmt.Errorf("%w: %v", ErrInvalidRule, err)
		}
		return Rule{Kind: kind, Value: fmt.Sprintf("%d-%d", lo, hi)}, nil
	case KindUser, KindExclude:
		if value == "" {
			return Rule{}, fmt.Errorf("%w: utente vuoto", ErrInvalidRule)
		}
		return Rule{Kind: kind, Value: strings.ToLower(value)}, nil
	}
	return Rule{}, fmt.Errorf("%w: tipo %q sconosciuto", ErrInvalidRule, kind)
}

// Label: descrizione leggibile per l'admin.
func Label(r Rule) string {
	switch r.Kind {
	case KindOU:
		return "OU " + r.Value
	case KindAttr:
		return r.Attr + " = " + r.Value
	case KindADGroup:
		return "Gruppo AD " + firstRDNValue(r.Value)
	case KindRange:
		if lo, hi, err := ParseRange(r.Value); err == nil {
			return fmt.Sprintf("Interni %d–%d", lo, hi)
		}
		return "Interni " + r.Value
	case KindUser:
		return "Utente " + r.Value
	case KindExclude:
		return "Escludi " + r.Value
	case KindPresent:
		return "Richiede " + r.Attr + " compilato"
	case KindAbsent:
		return "Richiede " + r.Attr + " vuoto"
	}
	return r.Kind + " " + r.Value
}

// ParseRange legge "da-a" (spazi ammessi), con da <= a.
func ParseRange(s string) (int, int, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("range %q: formato atteso da-a", s)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("range %q: estremi non numerici", s)
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("range %q: inizio maggiore della fine", s)
	}
	return lo, hi, nil
}

func extInRange(exts []string, rng string) bool {
	lo, hi, err := ParseRange(rng)
	if err != nil {
		return false
	}
	for _, e := range exts {
		n, err := strconv.Atoi(strings.TrimSpace(e))
		if err == nil && n >= lo && n <= hi {
			return true
		}
	}
	return false
}

func hasValue(vals []string) bool {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// CanonicalDN: forma canonica minuscola di un DN, per salvarlo e confrontarlo.
func CanonicalDN(s string) (string, error) {
	d, err := ldap.ParseDN(strings.TrimSpace(s))
	if err != nil || len(d.RDNs) == 0 {
		return "", ErrInvalidRule
	}
	return strings.ToLower(d.String()), nil
}

// CanonicalDNLoose: come CanonicalDN, ma su testo non-DN ripiega su
// minuscolo+trim invece di fallire.
func CanonicalDNLoose(s string) string {
	if c, err := CanonicalDN(s); err == nil {
		return c
	}
	return norm(s)
}

// SameDN confronta due DN come fa AD (maiuscole e spazi non contano).
func SameDN(a, b string) bool { return CanonicalDNLoose(a) == CanonicalDNLoose(b) }

func dnHasOU(dn, ou string) bool {
	want := norm(ou)
	if dn == "" || want == "" {
		return false
	}
	d, err := ldap.ParseDN(dn)
	if err != nil {
		return false
	}
	for _, rdn := range d.RDNs {
		for _, a := range rdn.Attributes {
			if strings.EqualFold(a.Type, "ou") && norm(a.Value) == want {
				return true
			}
		}
	}
	return false
}

func firstRDNValue(dn string) string {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d.RDNs) == 0 || len(d.RDNs[0].Attributes) == 0 {
		return dn
	}
	return d.RDNs[0].Attributes[0].Value
}
