package ldap

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// ErrInvalidAttr: nome di attributo non valido nella richiesta di suggerimenti.
var ErrInvalidAttr = errors.New("nome di attributo LDAP non valido")

// Suggestion: opzione cliccabile in admin; Value finisce nel campo.
type Suggestion struct{ Value, Label string }

const maxSuggestions = 20

func substringFilter(attr, q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "(" + attr + "=*)"
	}
	return "(" + attr + "=*" + ldap.EscapeFilter(q) + "*)"
}

const personFilter = "(objectCategory=person)(objectClass=user)"

func search(conn *ldap.Conn, cfg *config.Config, filter string, attrs []string, sizeLimit int) ([]*ldap.Entry, error) {
	req := ldap.NewSearchRequest(cfg.LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, sizeLimit, 10, false, filter, attrs, nil)
	sr, err := conn.Search(req)
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return nil, err
	}
	if sr == nil {
		return nil, nil
	}
	return sr.Entries, nil
}

// Suggest interroga AD dal vivo per i campi delle regole in admin.
func Suggest(cfg *config.Config, kind, attr, q string) ([]Suggestion, error) {
	switch kind {
	case arearules.KindNotAttr:
		kind = arearules.KindAttr
	case arearules.KindNotADGroup:
		kind = arearules.KindADGroup
	case arearules.KindRange:
		return nil, nil // nessun suggerimento: si scrive "da-a"
	}
	if kind == arearules.KindAttr && !arearules.ValidAttrName(attr) {
		return nil, ErrInvalidAttr
	}
	conn, err := BindForSync(cfg)
	if err != nil {
		return nil, fmt.Errorf("AD non disponibile: %w", err)
	}
	defer conn.Close()

	var out []Suggestion
	seen := map[string]bool{}
	add := func(v, label string) {
		if v != "" && !seen[strings.ToLower(v)] && len(out) < maxSuggestions {
			seen[strings.ToLower(v)] = true
			out = append(out, Suggestion{Value: v, Label: label})
		}
	}

	switch kind {
	case arearules.KindOU:
		es, err := search(conn, cfg, "(&(objectClass=organizationalUnit)"+substringFilter("ou", q)+")", []string{"ou"}, 200)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			ou := strings.ToUpper(e.GetAttributeValue("ou"))
			add(ou, ou)
		}
	case arearules.KindADGroup:
		es, err := search(conn, cfg, "(&(objectClass=group)"+substringFilter("cn", q)+")", []string{"cn"}, 200)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			add(e.DN, e.GetAttributeValue("cn"))
		}
	case arearules.KindUser, arearules.KindExclude:
		f := "(&" + personFilter + "(|" + substringFilter("sAMAccountName", q) + substringFilter("displayName", q) + "))"
		es, err := search(conn, cfg, f, []string{"sAMAccountName", "displayName"}, 200)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			sam := strings.ToLower(e.GetAttributeValue("sAMAccountName"))
			add(sam, e.GetAttributeValue("displayName")+" ("+sam+")")
		}
	case arearules.KindAttr:
		es, err := search(conn, cfg, "(&"+personFilter+substringFilter(attr, q)+")", []string{attr}, 1000)
		if err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, e := range es {
			for _, v := range e.GetAttributeValues(attr) {
				if v = strings.TrimSpace(v); v != "" {
					counts[v]++
				}
			}
		}
		vals := make([]string, 0, len(counts))
		for v := range counts {
			vals = append(vals, v)
		}
		sort.Slice(vals, func(i, j int) bool {
			if counts[vals[i]] != counts[vals[j]] {
				return counts[vals[i]] > counts[vals[j]]
			}
			return vals[i] < vals[j]
		})
		for _, v := range vals {
			add(v, fmt.Sprintf("%s (%d)", v, counts[v]))
		}
	case arearules.KindAttrName, arearules.KindPresent, arearules.KindAbsent:
		es, err := search(conn, cfg, "(&"+personFilter+")", []string{"*"}, 300)
		if err != nil {
			return nil, err
		}
		ql := strings.ToLower(strings.TrimSpace(q))
		for _, st := range computeAttrStats(es) {
			if ql == "" || strings.Contains(st.Name, ql) {
				add(st.Name, fmt.Sprintf("%s · %d utenti · es. %s", st.Name, st.Count, strings.Join(st.Examples, ", ")))
			}
		}
	default:
		return nil, fmt.Errorf("tipo di suggerimento %q sconosciuto", kind)
	}
	return out, nil
}

// ScopePreview: effetto delle regole di perimetro salvate rispetto a oggi.
type ScopePreview struct {
	In, Current               int
	Entering, Leaving         []string // max 30 nomi ciascuno, ordinati
	MoreEntering, MoreLeaving int      // nomi oltre i 30
}

// PreviewScope valuta il perimetro su AD senza scrivere nulla.
func PreviewScope(db *database.DB, cfg *config.Config) (ScopePreview, error) {
	tree, scope, err := loadRules(db)
	if err != nil {
		return ScopePreview{}, err
	}
	conn, err := BindForSync(cfg)
	if err != nil {
		return ScopePreview{}, fmt.Errorf("AD non disponibile: %w", err)
	}
	defer conn.Close()
	entries, err := fetchEntries(conn, cfg, searchAttributes(arearules.Attributes(tree, scope)))
	if err != nil {
		return ScopePreview{}, err
	}
	evals, _ := evaluateEntries(syncInput{
		Entries:      entries,
		GroupMembers: fetchGroupMembers(conn, cfg, arearules.GroupDNs(tree, scope)),
		Tree:         tree,
		Scope:        scope,
		OnlyActive:   cfg.LDAPOnlyActive,
	}, time.Now())
	current, err := db.ListActiveLDAPNames()
	if err != nil {
		return ScopePreview{}, err
	}
	return diffScope(evals, current), nil
}

func diffScope(evals []evaluated, current map[string]string) ScopePreview {
	var p ScopePreview
	in := map[string]bool{}
	for _, ev := range evals {
		if ev.Contact.Disabled {
			continue
		}
		in[ev.Contact.UID] = true
		p.In++
		if _, ok := current[ev.Contact.UID]; !ok {
			p.Entering = append(p.Entering, ev.Contact.DisplayName)
		}
	}
	p.Current = len(current)
	for uid, name := range current {
		if !in[uid] {
			p.Leaving = append(p.Leaving, name)
		}
	}
	sort.Strings(p.Entering)
	sort.Strings(p.Leaving)
	if len(p.Entering) > 30 {
		p.MoreEntering, p.Entering = len(p.Entering)-30, p.Entering[:30]
	}
	if len(p.Leaving) > 30 {
		p.MoreLeaving, p.Leaving = len(p.Leaving)-30, p.Leaving[:30]
	}
	return p
}
