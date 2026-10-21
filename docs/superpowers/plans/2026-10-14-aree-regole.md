# Aree e sotto-aree definite da regole — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** aree, sotto-aree (più d'una per contatto) e perimetro della rubrica definiti interamente da UI con regole su AD e interni, al posto di mapping OU, range per area, `LDAP_ALLOWED_GROUPS` e `LDAP_OU_FILTERS`.

**Architecture:** un package puro `internal/arearules` (regole, appartenenza, precedenza) usato da sync LDAP, sync PBX e admin. Le regole stanno in `area_rules`/`scope_rules`; il sync valuta e salva `contacts.area` + `contact_sub_areas`. L'admin `/admin/areas` gestisce albero, regole, perimetro con anteprima e suggerimenti AD dal vivo; rubrica pubblica e CardDAV leggono dal DB.

**Tech Stack:** Go 1.25, `mattn/go-sqlite3` (cgo), `go-ldap/ldap/v3` v3.4.14, `gorilla/mux`, `html/template` + HTMX 2.

**Spec:** `docs/superpowers/specs/2026-10-14-aree-regole-design.md`

## Global Constraints

- Lingua: UI, log, commenti e messaggi d'errore in italiano, come il codice esistente.
- Nuove colonne testo in SQLite: sempre `NOT NULL DEFAULT ''` (vedi CLAUDE.md).
- Schema: solo `CREATE TABLE IF NOT EXISTS` / `ALTER TABLE ... ADD COLUMN` in `migrate()`; nessun DROP.
- Nomi attributo LDAP validi: `^[A-Za-z][A-Za-z0-9-]{0,63}$`; valori nei filtri sempre via `ldap.EscapeFilter`.
- Key area `uffici` riservata ai gruppi di chiamata: resta, non va mai cancellata dal codice.
- Protezione svuotamento: soft-delete sospeso se contatti in uscita ≥ 5 **e** > 20% dei contatti LDAP non cancellati.
- Template parsati una volta all'avvio (`ParseGlob`): un template nuovo deve stare in `web/templates/*.html`.
- Test con cgo (tutto tranne `internal/arearules`) solo in container:
  `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test <PKG> -run '<RE>' -v"`
  — nel resto del piano abbreviato in **`DTEST <PKG> '<RE>'`**. `internal/arearules` è puro: `go test ./internal/arearules/ -v` va anche su Windows.
- Commit: messaggi Conventional Commits, chiusi da `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Branch: `feat/sub-areas` (già creato).

## Review Focus

1. Contatto con `manual_override` che conserva un'area diversa da quella risolta: le sotto-aree salvate devono appartenere all'area salvata, non a quella risolta (test in Task 4).
2. Interni multipli (`"700;701"`, spazi, token vuoti) e non numerici (`"gw"`) nelle regole `range`: match se **uno** qualsiasi cade nel range, mai panic (test in Task 1).
3. `LDAP_ALLOWED_GROUPS` valorizzata ma nessun gruppo trovato in AD al primo sync: il sync deve fallire senza scrivere il flag, non far entrare tutti (test in Task 4).
4. `?sub=` di una sotto-area di un'altra area o inesistente: risultato vuoto, nessun errore (test in Task 8).
5. Eliminazione di un contatto manuale con righe `contact_sub_areas`: nessun errore di FK, righe rimosse (test in Task 3).

---

## File Structure

| File | Responsabilità |
|---|---|
| `internal/arearules/arearules.go` (nuovo) | Tipi regola, `Member`, `Resolve`, `SubAreasFor`, `Normalize`, `Label`, utilità DN/range |
| `internal/arearules/arearules_test.go` (nuovo) | Test puri |
| `internal/database/areas.go` (nuovo) | `Area` CRUD ad albero, `area_rules`/`scope_rules`, `AreaTree`, migrazione regole |
| `internal/database/subareas.go` (nuovo) | `contact_sub_areas`, conteggi, elenchi membri, helper sync |
| `internal/database/sqlite.go` | Schema (`ALTER`/`CREATE`), rimozione `Area`/range/`MatchExtensionRange`/`SetAreaRange`/`ListAreas`/`CreateArea`/`RenameArea`/`DeleteArea` (spostati in `areas.go`) |
| `internal/ldap/sync.go` | Sync riscritto: fetch, valutazione regole, perimetro, protezione |
| `internal/ldap/scope.go` (nuovo) | Migrazione `LDAP_ALLOWED_GROUPS` → `scope_rules` |
| `internal/ldap/directory.go` (nuovo) | Suggerimenti AD, anteprima perimetro |
| `internal/ldap/attrstats.go` (nuovo) | Statistiche attributi (porting CruscottoPA) |
| `internal/pbx/sync.go` | Aree via `arearules`, `ReapplyAreas` |
| `cmd/server/admin_areas.go` (nuovo) | Handler admin aree/regole/perimetro/suggerimenti, trigger sync |
| `cmd/server/main.go` | Rotte, rail, search, contatti manuali, rimozione OU mapping |
| `internal/phonebook/service.go` | `SubAreaTag`, `AttachSubAreas` |
| `internal/carddav/server.go` | Rubriche per area/sotto-area |
| `internal/config/config.go` | Rimozione `LDAPOUFilters`; `LDAPAllowedGroups` solo seed |
| `web/templates/*` | Albero aree, dettaglio area, regole, perimetro, suggerimenti, rail, tag, contatti manuali |
| `web/static/css/style.css` | `.rail-sub`, `.sub-tag`, `.suggest-list` |

---

### Task 1: Package `arearules` (logica pura)

**Files:**
- Create: `internal/arearules/arearules.go`
- Test: `internal/arearules/arearules_test.go`

**Interfaces:**
- Consumes: niente.
- Produces:
  - costanti `KindOU="ou"`, `KindAttr="attr"`, `KindADGroup="adgroup"`, `KindRange="range"`, `KindUser="user"`, `KindExclude="exclude"`, `KindPresent="present"`, `KindAbsent="absent"`, `KindAttrName="attrname"` (solo per i suggerimenti, mai salvato)
  - `type Rule struct{ Kind, Attr, Value string }`
  - `type Profile struct{ Username, DN string; Attrs map[string][]string; Groups []string; Extensions []string }`
  - `type Node struct{ Key, Name string; Rules []Rule; Children []Node }`
  - `func Member(p Profile, rules []Rule) bool`
  - `func Resolve(p Profile, tree []Node) (area string, subAreas []string)`
  - `func SubAreasFor(p Profile, tree []Node, areaKey string) []string`
  - `func FindNode(tree []Node, key string) *Node`
  - `func GroupDNs(tree []Node, extra []Rule) []string`
  - `func Attributes(tree []Node, extra []Rule) []string`
  - `func Normalize(kind, attr, value string) (Rule, error)` — errori wrappano `ErrInvalidRule`
  - `func Label(r Rule) string`
  - `func ValidAttrName(s string) bool`, `func ParseRange(s string) (lo, hi int, err error)`
  - `func CanonicalDN(s string) (string, error)`, `func CanonicalDNLoose(s string) string`, `func SameDN(a, b string) bool`

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/arearules/arearules_test.go`:

```go
package arearules

import (
	"errors"
	"reflect"
	"testing"
)

const dnRossi = "CN=Mario Rossi,OU=Users,OU=INTERNI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it"
const dnDirigenti = "CN=RUBRICA_DIRIGENTI,OU=COMUNE-MS,DC=intranet,DC=comune,DC=montesilvano,DC=pe,DC=it"

func rossi() Profile {
	return Profile{
		Username:   "mario.rossi",
		DN:         dnRossi,
		Attrs:      map[string][]string{"description": {"Dirigente"}, "mail": {"m@x.it"}},
		Groups:     []string{CanonicalDNLoose(dnDirigenti)},
		Extensions: []string{"700", " 701 ", ""},
	}
}

func TestMemberKinds(t *testing.T) {
	p := rossi()
	cases := []struct {
		name  string
		rules []Rule
		want  bool
	}{
		{"nessuna regola", nil, false},
		{"ou in qualsiasi posizione", []Rule{{Kind: KindOU, Value: "INTERNI"}}, true},
		{"ou wrapper", []Rule{{Kind: KindOU, Value: "comune-ms"}}, true},
		{"ou assente", []Rule{{Kind: KindOU, Value: "ESTERNI"}}, false},
		{"attr maiuscole/spazi", []Rule{{Kind: KindAttr, Attr: "description", Value: " dirigente "}}, true},
		{"attr valore diverso", []Rule{{Kind: KindAttr, Attr: "description", Value: "Istruttore"}}, false},
		{"adgroup DN diverso nel case", []Rule{{Kind: KindADGroup, Value: "cn=rubrica_dirigenti,ou=comune-ms,dc=intranet,dc=comune,dc=montesilvano,dc=pe,dc=it"}}, true},
		{"range secondo interno con spazi", []Rule{{Kind: KindRange, Value: "701-701"}}, true},
		{"range fuori", []Rule{{Kind: KindRange, Value: "800-899"}}, false},
		{"user", []Rule{{Kind: KindUser, Value: "MARIO.ROSSI"}}, true},
		{"exclude vince", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindExclude, Value: "mario.rossi"}}, false},
		{"present ok", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindPresent, Attr: "mail"}}, true},
		{"present ko", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindPresent, Attr: "title"}}, false},
		{"absent ok", []Rule{{Kind: KindOU, Value: "INTERNI"}, {Kind: KindAbsent, Attr: "title"}}, true},
		{"solo requisiti non bastano", []Rule{{Kind: KindPresent, Attr: "mail"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Member(p, c.rules); got != c.want {
				t.Errorf("Member = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMemberRangeNonNumeric(t *testing.T) {
	p := Profile{Extensions: []string{"gw", "abc"}}
	if Member(p, []Rule{{Kind: KindRange, Value: "0-99999"}}) {
		t.Error("interni non numerici non devono corrispondere a un range")
	}
	if Member(p, []Rule{{Kind: KindRange, Value: "rotto"}}) {
		t.Error("range malformato non deve corrispondere")
	}
}

func TestMemberPBXProfile(t *testing.T) {
	p := Profile{Extensions: []string{"750"}}
	if !Member(p, []Rule{{Kind: KindRange, Value: "700-799"}}) {
		t.Error("profilo PBX deve corrispondere al range")
	}
	if Member(p, []Rule{{Kind: KindRange, Value: "700-799"}, {Kind: KindPresent, Attr: "mail"}}) {
		t.Error("present su profilo senza attributi deve fallire")
	}
	if !Member(p, []Rule{{Kind: KindRange, Value: "700-799"}, {Kind: KindAbsent, Attr: "mail"}}) {
		t.Error("absent su profilo senza attributi deve passare")
	}
	if Member(p, []Rule{{Kind: KindUser, Value: ""}}) {
		t.Error("utente vuoto non deve corrispondere a profilo senza username")
	}
}

func tree() []Node {
	return []Node{
		{Key: "esterni", Name: "Esterni", Rules: []Rule{{Kind: KindOU, Value: "ESTERNI"}}},
		{Key: "interni", Name: "Personale comunale", Rules: []Rule{{Kind: KindOU, Value: "INTERNI"}}, Children: []Node{
			{Key: "dirigenti", Name: "Dirigenti", Rules: []Rule{{Kind: KindADGroup, Value: dnDirigenti}}},
			{Key: "protciv", Name: "Protezione civile", Rules: []Rule{{Kind: KindRange, Value: "700-709"}}},
			{Key: "collab", Name: "Collaboratori", Rules: []Rule{{Kind: KindAttr, Attr: "description", Value: "Collaboratore"}}},
		}},
		{Key: "tutti", Name: "Tutti", Rules: []Rule{{Kind: KindOU, Value: "COMUNE-MS"}}, Children: []Node{
			{Key: "dirigenti2", Name: "Dirigenti bis", Rules: []Rule{{Kind: KindADGroup, Value: dnDirigenti}}},
		}},
	}
}

func TestResolvePrecedenceAndMultipleSubAreas(t *testing.T) {
	area, subs := Resolve(rossi(), tree())
	if area != "interni" {
		t.Fatalf("area = %q, want interni (prima area in ordine che corrisponde)", area)
	}
	if want := []string{"dirigenti", "protciv"}; !reflect.DeepEqual(subs, want) {
		t.Errorf("subAreas = %v, want %v (tutte quelle dell'area, in ordine; mai quelle di un'altra area)", subs, want)
	}
}

func TestResolveNoMatch(t *testing.T) {
	area, subs := Resolve(Profile{DN: "CN=x,CN=Users,DC=a"}, tree())
	if area != "" || subs != nil {
		t.Errorf("Resolve = (%q, %v), want (\"\", nil)", area, subs)
	}
}

func TestSubAreasForStoredArea(t *testing.T) {
	got := SubAreasFor(rossi(), tree(), "tutti")
	if want := []string{"dirigenti2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SubAreasFor(tutti) = %v, want %v", got, want)
	}
	if got := SubAreasFor(rossi(), tree(), ""); got != nil {
		t.Errorf("SubAreasFor(\"\") = %v, want nil", got)
	}
}

func TestGroupDNsAndAttributes(t *testing.T) {
	extra := []Rule{{Kind: KindPresent, Attr: "mail"}, {Kind: KindADGroup, Value: dnDirigenti}}
	if got := GroupDNs(tree(), extra); len(got) != 1 || got[0] != CanonicalDNLoose(dnDirigenti) {
		t.Errorf("GroupDNs = %v, want un solo DN canonico", got)
	}
	if got := Attributes(tree(), extra); !reflect.DeepEqual(got, []string{"description", "mail"}) {
		t.Errorf("Attributes = %v", got)
	}
}

func TestNormalize(t *testing.T) {
	ok := []struct {
		kind, attr, value string
		want              Rule
	}{
		{KindOU, "", " interni ", Rule{Kind: KindOU, Value: "INTERNI"}},
		{KindAttr, "Description", " Dirigente ", Rule{Kind: KindAttr, Attr: "description", Value: "Dirigente"}},
		{KindPresent, "mail", "ignorato", Rule{Kind: KindPresent, Attr: "mail"}},
		{KindADGroup, "", dnDirigenti, Rule{Kind: KindADGroup, Value: CanonicalDNLoose(dnDirigenti)}},
		{KindRange, "", " 700 - 799 ", Rule{Kind: KindRange, Value: "700-799"}},
		{KindUser, "", " Mario.Rossi ", Rule{Kind: KindUser, Value: "mario.rossi"}},
		{" EXCLUDE ", "", "x", Rule{Kind: KindExclude, Value: "x"}},
	}
	for _, c := range ok {
		got, err := Normalize(c.kind, c.attr, c.value)
		if err != nil || got != c.want {
			t.Errorf("Normalize(%q,%q,%q) = %+v, %v; want %+v", c.kind, c.attr, c.value, got, err, c.want)
		}
	}
	bad := [][3]string{
		{KindOU, "", ""},
		{KindAttr, "1bad", "x"},
		{KindAttr, "description", ""},
		{KindPresent, "a b", ""},
		{KindADGroup, "", "non un dn"},
		{KindRange, "", "799-700"},
		{KindRange, "", "abc"},
		{KindUser, "", " "},
		{"boh", "", "x"},
		{KindAttrName, "", "x"},
	}
	for _, b := range bad {
		if _, err := Normalize(b[0], b[1], b[2]); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("Normalize(%q,%q,%q) err = %v, want ErrInvalidRule", b[0], b[1], b[2], err)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := map[Rule]string{
		{Kind: KindOU, Value: "INTERNI"}:                         "OU INTERNI",
		{Kind: KindAttr, Attr: "description", Value: "Dirigente"}: "description = Dirigente",
		{Kind: KindADGroup, Value: CanonicalDNLoose(dnDirigenti)}: "Gruppo AD rubrica_dirigenti",
		{Kind: KindRange, Value: "700-799"}:                       "Interni 700–799",
		{Kind: KindUser, Value: "mario.rossi"}:                    "Utente mario.rossi",
		{Kind: KindExclude, Value: "stagista1"}:                   "Escludi stagista1",
		{Kind: KindPresent, Attr: "mail"}:                         "Richiede mail compilato",
		{Kind: KindAbsent, Attr: "title"}:                         "Richiede title vuoto",
	}
	for r, want := range cases {
		if got := Label(r); got != want {
			t.Errorf("Label(%+v) = %q, want %q", r, got, want)
		}
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `go test ./internal/arearules/ -v`
Expected: FAIL di compilazione (`undefined: Profile`, ...).

- [ ] **Step 3: Implementa**

`internal/arearules/arearules.go`:

```go
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
```

- [ ] **Step 4: Verifica che passino**

Run: `go test ./internal/arearules/ -v`
Expected: PASS su tutti i test. Se `TestLabel` per `adgroup` fallisce perché `d.String()` non abbassa il valore, è corretto che `CanonicalDN` usi `strings.ToLower` (lo fa): verificare l'atteso `rubrica_dirigenti`.

- [ ] **Step 5: Commit**

```bash
git add internal/arearules
git commit -m "feat(arearules): motore di regole per aree, sotto-aree e perimetro"
```

---

### Task 2: DB — albero aree, regole, migrazione

**Files:**
- Create: `internal/database/areas.go`
- Modify: `internal/database/sqlite.go` (struct `Area` righe 41-56; schema/alter in `migrate()` righe 181-260; rimuovere `ListAreas`, `SetAreaRange`, `MatchExtensionRange` righe 908-958 e `CreateArea`/`RenameArea`/`DeleteArea` righe 1110-1157)
- Test: `internal/database/areas_test.go`

**Interfaces:**
- Consumes: `arearules.Rule`, `arearules.Node`.
- Produces:
  - `type Area struct{ ID int64; Key, Name, ParentKey string; Sort int; CreatedAt, UpdatedAt time.Time }`
  - `type AreaNode struct{ *Area; Children []*Area }`; `func BuildAreaNodes(areas []*Area) []*AreaNode`
  - `type AreaRule struct{ ID, AreaID int64; arearules.Rule; CreatedAt time.Time }`; `func RulesOf(rs []*AreaRule) []arearules.Rule`
  - `var ErrNestedSubArea, ErrParentNotFound error`
  - `(db) ListAreas() ([]*Area, error)` (ordine `sort, name`), `GetArea(id) (*Area, error)`, `GetAreaByKey(key) (*Area, error)`, `CreateArea(a *Area) error`, `RenameArea(id, name) error`, `MoveArea(id int64, up bool) error`, `DeleteArea(id) error`
  - `(db) ListAreaRules(areaID) ([]*AreaRule, error)`, `AddAreaRule(areaID int64, r arearules.Rule) error`, `DeleteAreaRule(areaID, ruleID int64) error`
  - `(db) ListScopeRules() ([]*AreaRule, error)`, `AddScopeRule(r arearules.Rule) error`, `DeleteScopeRule(ruleID int64) error`
  - `(db) AreaTree() ([]arearules.Node, error)`
  - tabelle `area_rules`, `scope_rules`, `contact_sub_areas` (creata qui, usata in Task 3), colonne `areas.parent_key`, `areas.sort`.

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/database/areas_test.go`:

```go
package database

import (
	"errors"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
)

func areaByKey(t *testing.T, db *DB, key string) *Area {
	t.Helper()
	a, err := db.GetAreaByKey(key)
	if err != nil || a == nil {
		t.Fatalf("GetAreaByKey(%q) = %v, %v", key, a, err)
	}
	return a
}

func TestMigrationCreatesDefaultOURules(t *testing.T) {
	db := newTestDB(t)
	tree, err := db.AreaTree()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"interni": "INTERNI", "esterni": "ESTERNI", "politica": "AREA_POLITICA"}
	for key, ou := range want {
		n := arearules.FindNode(tree, key)
		if n == nil || len(n.Rules) != 1 || n.Rules[0] != (arearules.Rule{Kind: "ou", Value: ou}) {
			t.Errorf("area %s: regole = %+v, want una regola ou %s", key, n, ou)
		}
	}
}

func TestMigrationFromSavedMappingAndRanges(t *testing.T) {
	db := newTestDB(t)
	// Simula un DB precedente: flag assente, mapping personalizzato, range.
	if _, err := db.Exec(`DELETE FROM area_rules`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConfig("area_rules_migrated", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConfig("ou_area_mapping", `{"UNDEF":"esterni","INTERNI":"interni","FANTASMA":"inesistente"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE areas SET range_start = 700, range_end = 799 WHERE key = 'uffici'`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateAreaRules(); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateAreaRules(); err != nil { // idempotente
		t.Fatal(err)
	}
	rules, _ := db.ListAreaRules(areaByKey(t, db, "esterni").ID)
	if len(rules) != 1 || rules[0].Value != "UNDEF" {
		t.Errorf("esterni = %+v, want solo ou UNDEF", rules)
	}
	rules, _ = db.ListAreaRules(areaByKey(t, db, "uffici").ID)
	if len(rules) != 1 || rules[0].Kind != "range" || rules[0].Value != "700-799" {
		t.Errorf("uffici = %+v, want range 700-799", rules)
	}
	rules, _ = db.ListAreaRules(areaByKey(t, db, "politica").ID)
	if len(rules) != 0 {
		t.Errorf("politica = %+v, want nessuna regola (non nel mapping salvato)", rules)
	}
}

func TestCreateSubAreaAndNesting(t *testing.T) {
	db := newTestDB(t)
	sub := &Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}
	if err := db.CreateArea(sub); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateArea(&Area{Key: "x", Name: "X", ParentKey: "dirigenti"}); !errors.Is(err, ErrNestedSubArea) {
		t.Errorf("err = %v, want ErrNestedSubArea", err)
	}
	if err := db.CreateArea(&Area{Key: "y", Name: "Y", ParentKey: "nessuna"}); !errors.Is(err, ErrParentNotFound) {
		t.Errorf("err = %v, want ErrParentNotFound", err)
	}
	if err := db.CreateArea(&Area{Key: "collab", Name: "Collaboratori", ParentKey: "interni"}); err != nil {
		t.Fatal(err)
	}
	tree, _ := db.AreaTree()
	n := arearules.FindNode(tree, "interni")
	if n == nil || len(n.Children) != 2 || n.Children[0].Key != "dirigenti" || n.Children[1].Key != "collab" {
		t.Fatalf("figli di interni = %+v", n)
	}
	if err := db.MoveArea(areaByKey(t, db, "collab").ID, true); err != nil {
		t.Fatal(err)
	}
	tree, _ = db.AreaTree()
	if n := arearules.FindNode(tree, "interni"); n.Children[0].Key != "collab" {
		t.Errorf("dopo MoveArea su: primo figlio = %q, want collab", n.Children[0].Key)
	}
}

func TestDeleteAreaCascades(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateArea(&Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}); err != nil {
		t.Fatal(err)
	}
	sub := areaByKey(t, db, "dirigenti")
	if err := db.AddAreaRule(sub.ID, arearules.Rule{Kind: "user", Value: "u1"}); err != nil {
		t.Fatal(err)
	}
	c := &Contact{UID: "u1", DisplayName: "U1", Area: "interni", LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO contact_sub_areas (contact_id, sub_area_key) VALUES (?, 'dirigenti')`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteArea(areaByKey(t, db, "interni").ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAreaByKey("dirigenti"); a != nil {
		t.Error("sotto-area non eliminata con la madre")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM area_rules WHERE area_id = ?`, sub.ID).Scan(&n)
	if n != 0 {
		t.Errorf("regole residue = %d", n)
	}
	db.QueryRow(`SELECT COUNT(*) FROM contact_sub_areas`).Scan(&n)
	if n != 0 {
		t.Errorf("contact_sub_areas residue = %d", n)
	}
	got, _ := db.GetContact("u1")
	if got.Area != "" {
		t.Errorf("area contatto = %q, want vuota", got.Area)
	}
}

func TestScopeRulesCRUD(t *testing.T) {
	db := newTestDB(t)
	if err := db.AddScopeRule(arearules.Rule{Kind: "ou", Value: "INTERNI"}); err != nil {
		t.Fatal(err)
	}
	rs, _ := db.ListScopeRules()
	if len(rs) != 1 || rs[0].Value != "INTERNI" || rs[0].AreaID != 0 {
		t.Fatalf("scope = %+v", rs)
	}
	if err := db.DeleteScopeRule(rs[0].ID); err != nil {
		t.Fatal(err)
	}
	if rs, _ := db.ListScopeRules(); len(rs) != 0 {
		t.Errorf("scope dopo delete = %+v", rs)
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/database/ 'Migration|SubArea|DeleteAreaCascades|ScopeRules'`
Expected: FAIL di compilazione (`undefined: GetAreaByKey`, ...).

- [ ] **Step 3: Schema in `migrate()`**

In `internal/database/sqlite.go`, dentro la stringa `schema` dopo la tabella `group_categories`, aggiungere:

```sql
	CREATE TABLE IF NOT EXISTS area_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		area_id INTEGER NOT NULL REFERENCES areas(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		attr TEXT NOT NULL DEFAULT '',
		value TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS scope_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		attr TEXT NOT NULL DEFAULT '',
		value TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS contact_sub_areas (
		contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
		sub_area_key TEXT NOT NULL,
		PRIMARY KEY (contact_id, sub_area_key)
	);
	CREATE INDEX IF NOT EXISTS idx_contact_sub_areas_key ON contact_sub_areas(sub_area_key);
```

Nella slice `alterStatements` aggiungere in fondo:

```go
		"ALTER TABLE areas ADD COLUMN parent_key TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE areas ADD COLUMN sort INTEGER NOT NULL DEFAULT 0",
```

In fondo a `migrate()`, prima di `return nil`:

```go
	if err := db.migrateAreaRules(); err != nil {
		return fmt.Errorf("failed to migrate area rules: %w", err)
	}
```

Sostituire la struct `Area` (righe 41-56) con un commento che rimanda ad `areas.go` (la struct si sposta lì). Rimuovere da `sqlite.go` le funzioni `ListAreas`, `SetAreaRange`, `MatchExtensionRange`, `CreateArea`, `RenameArea`, `DeleteArea` (ricreate in `areas.go`). Aggiornare il commento di `GroupNumber.Area` in "area assegnata dalle regole range (vedi internal/arearules), \"\" se nessuna".

- [ ] **Step 4: Implementa `areas.go`**

`internal/database/areas.go`:

```go
package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
)

// Area è un'area di primo livello (ParentKey "") o una sotto-area
// (ParentKey = key della madre). Un solo livello di annidamento. Key è
// stabile: usata in contacts.area, contact_sub_areas e negli URL
// (?group=, ?sub=, /carddav/<key>/). Sort = precedenza fra fratelli.
type Area struct {
	ID        int64
	Key       string
	Name      string
	ParentKey string
	Sort      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AreaNode: area di primo livello con le sue sotto-aree, per i template.
type AreaNode struct {
	*Area
	Children []*Area
}

// AreaRule è una regola salvata; AreaID 0 per le regole di perimetro.
type AreaRule struct {
	ID     int64
	AreaID int64
	arearules.Rule
	CreatedAt time.Time
}

var (
	ErrNestedSubArea  = errors.New("una sotto-area non può avere sotto-aree")
	ErrParentNotFound = errors.New("area madre inesistente")
)

const areaRulesMigratedKey = "area_rules_migrated"

// RulesOf estrae le regole pure da quelle salvate.
func RulesOf(rs []*AreaRule) []arearules.Rule {
	out := make([]arearules.Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Rule)
	}
	return out
}

// BuildAreaNodes raggruppa le aree (già ordinate) in primo livello + figli.
func BuildAreaNodes(areas []*Area) []*AreaNode {
	var nodes []*AreaNode
	idx := map[string]*AreaNode{}
	for _, a := range areas {
		if a.ParentKey == "" {
			n := &AreaNode{Area: a}
			nodes = append(nodes, n)
			idx[a.Key] = n
		}
	}
	for _, a := range areas {
		if n, ok := idx[a.ParentKey]; ok && a.ParentKey != "" {
			n.Children = append(n.Children, a)
		}
	}
	return nodes
}

const areaColumns = `id, key, name, parent_key, sort, created_at, updated_at`

func scanArea(row interface{ Scan(...any) error }) (*Area, error) {
	a := &Area{}
	if err := row.Scan(&a.ID, &a.Key, &a.Name, &a.ParentKey, &a.Sort, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return a, nil
}

// ListAreas: tutte le aree, in ordine di precedenza (sort, poi nome).
func (db *DB) ListAreas() ([]*Area, error) {
	rows, err := db.Query(`SELECT ` + areaColumns + ` FROM areas ORDER BY sort, name`)
	if err != nil {
		return nil, fmt.Errorf("failed to list areas: %w", err)
	}
	defer rows.Close()
	var out []*Area
	for rows.Next() {
		a, err := scanArea(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan area: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) getAreaWhere(where string, arg any) (*Area, error) {
	a, err := scanArea(db.QueryRow(`SELECT `+areaColumns+` FROM areas WHERE `+where, arg))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get area: %w", err)
	}
	return a, nil
}

func (db *DB) GetArea(id int64) (*Area, error)        { return db.getAreaWhere("id = ?", id) }
func (db *DB) GetAreaByKey(key string) (*Area, error) { return db.getAreaWhere("key = ?", key) }

// CreateArea inserisce un'area in coda ai fratelli. Con ParentKey la madre
// deve esistere ed essere di primo livello.
func (db *DB) CreateArea(a *Area) error {
	if a.ParentKey != "" {
		parent, err := db.GetAreaByKey(a.ParentKey)
		if err != nil {
			return err
		}
		if parent == nil {
			return ErrParentNotFound
		}
		if parent.ParentKey != "" {
			return ErrNestedSubArea
		}
	}
	var maxSort int
	if err := db.QueryRow(`SELECT COALESCE(MAX(sort), -1) FROM areas WHERE parent_key = ?`, a.ParentKey).Scan(&maxSort); err != nil {
		return fmt.Errorf("failed to compute sort: %w", err)
	}
	now := time.Now()
	a.Sort, a.CreatedAt, a.UpdatedAt = maxSort+1, now, now
	res, err := db.Exec(`INSERT INTO areas (key, name, parent_key, sort, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.Key, a.Name, a.ParentKey, a.Sort, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create area: %w", err)
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// RenameArea cambia solo il nome: la key resta stabile.
func (db *DB) RenameArea(id int64, name string) error {
	if _, err := db.Exec(`UPDATE areas SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now(), id); err != nil {
		return fmt.Errorf("failed to rename area: %w", err)
	}
	return nil
}

// MoveArea sposta l'area di una posizione fra i fratelli e rinumera sort
// 0..n-1 (normalizza eventuali sort duplicati).
func (db *DB) MoveArea(id int64, up bool) error {
	a, err := db.GetArea(id)
	if err != nil || a == nil {
		return err
	}
	rows, err := db.Query(`SELECT id FROM areas WHERE parent_key = ? ORDER BY sort, name`, a.ParentKey)
	if err != nil {
		return fmt.Errorf("failed to list siblings: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, sid)
	}
	rows.Close()
	pos := -1
	for i, sid := range ids {
		if sid == id {
			pos = i
		}
	}
	other := pos + 1
	if up {
		other = pos - 1
	}
	if pos < 0 || other < 0 || other >= len(ids) {
		return nil
	}
	ids[pos], ids[other] = ids[other], ids[pos]
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i, sid := range ids {
		if _, err := tx.Exec(`UPDATE areas SET sort = ?, updated_at = ? WHERE id = ?`, i, time.Now(), sid); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to reorder areas: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteArea elimina l'area, le sue sotto-aree e le loro regole; azzera
// contacts.area / group_numbers.area che le usavano e rimuove le righe
// contact_sub_areas corrispondenti.
func (db *DB) DeleteArea(id int64) error {
	a, err := db.GetArea(id)
	if err != nil || a == nil {
		return err
	}
	ids, keys := []int64{a.ID}, []string{a.Key}
	if a.ParentKey == "" {
		rows, err := db.Query(`SELECT id, key FROM areas WHERE parent_key = ?`, a.Key)
		if err != nil {
			return fmt.Errorf("failed to list sub-areas: %w", err)
		}
		for rows.Next() {
			var cid int64
			var ckey string
			if err := rows.Scan(&cid, &ckey); err != nil {
				rows.Close()
				return err
			}
			ids, keys = append(ids, cid), append(keys, ckey)
		}
		rows.Close()
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmts := []struct {
		q   string
		arg func(i int) any
		n   int
	}{
		{`DELETE FROM area_rules WHERE area_id = ?`, func(i int) any { return ids[i] }, len(ids)},
		{`DELETE FROM areas WHERE id = ?`, func(i int) any { return ids[i] }, len(ids)},
		{`DELETE FROM contact_sub_areas WHERE sub_area_key = ?`, func(i int) any { return keys[i] }, len(keys)},
		{`UPDATE contacts SET area = '' WHERE area = ?`, func(i int) any { return keys[i] }, len(keys)},
		{`UPDATE group_numbers SET area = '' WHERE area = ?`, func(i int) any { return keys[i] }, len(keys)},
	}
	for _, s := range stmts {
		for i := 0; i < s.n; i++ {
			if _, err := tx.Exec(s.q, s.arg(i)); err != nil {
				tx.Rollback()
				return fmt.Errorf("failed to delete area: %w", err)
			}
		}
	}
	return tx.Commit()
}

func (db *DB) listRules(table, where string, args ...any) ([]*AreaRule, error) {
	cols := `id, 0, kind, attr, value, created_at`
	if table == "area_rules" {
		cols = `id, area_id, kind, attr, value, created_at`
	}
	rows, err := db.Query(`SELECT `+cols+` FROM `+table+` `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list rules: %w", err)
	}
	defer rows.Close()
	var out []*AreaRule
	for rows.Next() {
		r := &AreaRule{}
		if err := rows.Scan(&r.ID, &r.AreaID, &r.Kind, &r.Attr, &r.Value, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) ListAreaRules(areaID int64) ([]*AreaRule, error) {
	return db.listRules("area_rules", "WHERE area_id = ?", areaID)
}

func (db *DB) AddAreaRule(areaID int64, r arearules.Rule) error {
	_, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, ?, ?, ?, ?)`,
		areaID, r.Kind, r.Attr, r.Value, time.Now())
	if err != nil {
		return fmt.Errorf("failed to add area rule: %w", err)
	}
	return nil
}

func (db *DB) DeleteAreaRule(areaID, ruleID int64) error {
	if _, err := db.Exec(`DELETE FROM area_rules WHERE id = ? AND area_id = ?`, ruleID, areaID); err != nil {
		return fmt.Errorf("failed to delete area rule: %w", err)
	}
	return nil
}

func (db *DB) ListScopeRules() ([]*AreaRule, error) { return db.listRules("scope_rules", "") }

func (db *DB) AddScopeRule(r arearules.Rule) error {
	_, err := db.Exec(`INSERT INTO scope_rules (kind, attr, value, created_at) VALUES (?, ?, ?, ?)`, r.Kind, r.Attr, r.Value, time.Now())
	if err != nil {
		return fmt.Errorf("failed to add scope rule: %w", err)
	}
	return nil
}

func (db *DB) DeleteScopeRule(ruleID int64) error {
	if _, err := db.Exec(`DELETE FROM scope_rules WHERE id = ?`, ruleID); err != nil {
		return fmt.Errorf("failed to delete scope rule: %w", err)
	}
	return nil
}

// AreaTree: albero di valutazione (precedenza = ordine) con le regole.
func (db *DB) AreaTree() ([]arearules.Node, error) {
	areas, err := db.ListAreas()
	if err != nil {
		return nil, err
	}
	all, err := db.listRules("area_rules", "")
	if err != nil {
		return nil, err
	}
	byArea := map[int64][]arearules.Rule{}
	for _, r := range all {
		byArea[r.AreaID] = append(byArea[r.AreaID], r.Rule)
	}
	var tree []arearules.Node
	for _, n := range BuildAreaNodes(areas) {
		node := arearules.Node{Key: n.Key, Name: n.Name, Rules: byArea[n.ID]}
		for _, c := range n.Children {
			node.Children = append(node.Children, arearules.Node{Key: c.Key, Name: c.Name, Rules: byArea[c.ID]})
		}
		tree = append(tree, node)
	}
	return tree, nil
}

// migrateAreaRules converte, una sola volta, il vecchio mapping OU->Area
// (app_config "ou_area_mapping", o il default storico) e i range delle aree
// (areas.range_start/range_end) in regole, e assegna sort alfabetico.
func (db *DB) migrateAreaRules() error {
	if v, _ := db.GetConfig(areaRulesMigratedKey); v == "1" {
		return nil
	}
	mapping := map[string]string{"INTERNI": "interni", "ESTERNI": "esterni", "AREA_POLITICA": "politica"}
	if raw, _ := db.GetConfig("ou_area_mapping"); raw != "" {
		var stored map[string]string
		if err := json.Unmarshal([]byte(raw), &stored); err == nil && len(stored) > 0 {
			mapping = stored
		}
	}
	now := time.Now()
	for ou, key := range mapping {
		if key == "" {
			continue
		}
		a, err := db.GetAreaByKey(key)
		if err != nil {
			return err
		}
		if a == nil {
			continue
		}
		if _, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, 'ou', '', ?, ?)`,
			a.ID, strings.ToUpper(strings.TrimSpace(ou)), now); err != nil {
			return err
		}
	}

	type rng struct {
		id     int64
		lo, hi int
	}
	var ranges []rng
	rows, err := db.Query(`SELECT id, range_start, range_end FROM areas WHERE range_start IS NOT NULL AND range_end IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r rng
		if err := rows.Scan(&r.id, &r.lo, &r.hi); err != nil {
			rows.Close()
			return err
		}
		ranges = append(ranges, r)
	}
	rows.Close()
	for _, r := range ranges {
		if _, err := db.Exec(`INSERT INTO area_rules (area_id, kind, attr, value, created_at) VALUES (?, 'range', '', ?, ?)`,
			r.id, fmt.Sprintf("%d-%d", r.lo, r.hi), now); err != nil {
			return err
		}
	}

	var ids []int64
	rows, err = db.Query(`SELECT id FROM areas ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if _, err := db.Exec(`UPDATE areas SET sort = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return db.SetConfig(areaRulesMigratedKey, "1")
}
```

Nota: il test `TestMigrationFromSavedMappingAndRanges` cancella `area_rules` e azzera il flag a `""` — `GetConfig` restituisce `""` ≠ `"1"`, quindi la migrazione riparte. La seconda chiamata vede `"1"` e non duplica.

- [ ] **Step 5: Correggi gli altri riferimenti nel package**

Run: `grep -rn "RangeStart\|RangeEnd" internal/database/*.go | grep -v GroupCategory`
Expected: nessun riferimento a `Area.RangeStart`/`RangeEnd` rimasto (quelli di `GroupCategory` restano). Se `sqlite_test.go` usa `SetAreaRange` o `MatchExtensionRange`, rimuovere quei test (la funzionalità è coperta da `arearules` e da `TestMigrationFromSavedMappingAndRanges`).

- [ ] **Step 6: Verifica che passino**

Run: `DTEST ./internal/database/ ''`
Expected: PASS (tutto il package). Il resto del repo non compila ancora (ldap, pbx, main usano simboli rimossi): è atteso fino ai Task 4, 6, 7.

- [ ] **Step 7: Commit**

```bash
git add internal/database
git commit -m "feat(database): aree ad albero con regole e migrazione dal mapping OU"
```

---

### Task 3: DB — sotto-aree dei contatti, conteggi, elenchi

**Files:**
- Create: `internal/database/subareas.go`
- Test: `internal/database/subareas_test.go`

**Interfaces:**
- Consumes: tabella `contact_sub_areas` (Task 2).
- Produces:
  - `(db) SetContactSubAreas(contactID int64, keys []string) error`
  - `(db) ListSubAreasForContacts(ids []int64) (map[int64][]string, error)` — chiavi in ordine `areas.sort, areas.name`
  - `(db) CountBySubArea() (map[string]int, error)`
  - `(db) ContactIDsBySubArea(key string) (map[int64]bool, error)`
  - `(db) ListAreaMembers(areaKey string, limit int) ([]*Contact, int, error)`
  - `(db) ListSubAreaMembers(subKey string, limit int) ([]*Contact, int, error)`
  - `(db) ListAreaMembersWithoutSubArea(areaKey string, limit int) ([]*Contact, int, error)`
  - `(db) ContactIDAndArea(uid string) (int64, string, error)` — qualsiasi stato, `0, "", nil` se assente
  - `(db) SetContactArea(id int64, area string) error` — rispetta `manual_override`
  - `(db) SetGroupArea(id int64, area string) error`
  - `(db) CountStaleLDAP(syncTime time.Time) (int, error)`, `(db) CountActiveLDAP() (int, error)`
  - `(db) ListActiveLDAPNames() (map[string]string, error)` — uid → display_name, non cancellati e non disabled

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/database/subareas_test.go`:

```go
package database

import (
	"reflect"
	"testing"
	"time"
)

func seedSubAreas(t *testing.T, db *DB) {
	t.Helper()
	for _, a := range []*Area{
		{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"},
		{Key: "protciv", Name: "Protezione civile", ParentKey: "interni"},
	} {
		if err := db.CreateArea(a); err != nil {
			t.Fatal(err)
		}
	}
}

func addContact(t *testing.T, db *DB, uid, area string) *Contact {
	t.Helper()
	c := &Contact{UID: uid, DisplayName: uid, Email: uid + "@x.it", Area: area, LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatal(err)
	}
	id, _, err := db.ContactIDAndArea(uid)
	if err != nil || id == 0 {
		t.Fatalf("ContactIDAndArea(%s) = %d, %v", uid, id, err)
	}
	c.ID = id
	return c
}

func TestSetAndListContactSubAreas(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	a := addContact(t, db, "a", "interni")
	b := addContact(t, db, "b", "interni")
	if err := db.SetContactSubAreas(a.ID, []string{"protciv", "dirigenti", "dirigenti"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(b.ID, []string{"protciv"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(b.ID, nil); err != nil { // sostituzione in blocco
		t.Fatal(err)
	}
	got, err := db.ListSubAreasForContacts([]int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dirigenti", "protciv"}; !reflect.DeepEqual(got[a.ID], want) {
		t.Errorf("sub di a = %v, want %v (ordine di sort, senza duplicati)", got[a.ID], want)
	}
	if len(got[b.ID]) != 0 {
		t.Errorf("sub di b = %v, want vuoto", got[b.ID])
	}
	if m, _ := db.ListSubAreasForContacts(nil); len(m) != 0 {
		t.Errorf("ids vuoti = %v", m)
	}
}

func TestCountsAndMembers(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	a := addContact(t, db, "a", "interni")
	addContact(t, db, "b", "interni")
	db.SetContactSubAreas(a.ID, []string{"dirigenti", "protciv"})

	counts, err := db.CountBySubArea()
	if err != nil {
		t.Fatal(err)
	}
	if counts["dirigenti"] != 1 || counts["protciv"] != 1 {
		t.Errorf("counts = %v", counts)
	}
	members, total, _ := db.ListSubAreaMembers("dirigenti", 30)
	if total != 1 || len(members) != 1 || members[0].UID != "a" {
		t.Errorf("membri dirigenti = %d %+v", total, members)
	}
	without, total, _ := db.ListAreaMembersWithoutSubArea("interni", 30)
	if total != 1 || without[0].UID != "b" {
		t.Errorf("senza sottogruppo = %d %+v", total, without)
	}
	all, total, _ := db.ListAreaMembers("interni", 1)
	if total != 2 || len(all) != 1 {
		t.Errorf("membri interni = total %d len %d, want 2 e 1 (limit)", total, len(all))
	}
	ids, _ := db.ContactIDsBySubArea("protciv")
	if !ids[a.ID] || len(ids) != 1 {
		t.Errorf("ContactIDsBySubArea = %v", ids)
	}
}

func TestManualContactDeleteCascadesSubAreas(t *testing.T) {
	db := newTestDB(t)
	seedSubAreas(t, db)
	c := &Contact{UID: "manual-x", DisplayName: "X", Area: "interni"}
	if err := db.CreateManualContact(c); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactSubAreas(c.ID, []string{"dirigenti"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteManualContact("manual-x"); err != nil {
		t.Fatalf("DeleteManualContact con sotto-aree: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM contact_sub_areas`).Scan(&n)
	if n != 0 {
		t.Errorf("righe residue = %d", n)
	}
}

func TestStaleCounts(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().Add(-time.Hour)
	for _, uid := range []string{"a", "b", "c"} {
		db.UpsertContact(&Contact{UID: uid, DisplayName: uid, LastSync: old})
	}
	now := time.Now()
	db.UpsertContact(&Contact{UID: "a", DisplayName: "a", LastSync: now})
	stale, _ := db.CountStaleLDAP(now)
	active, _ := db.CountActiveLDAP()
	if stale != 2 || active != 3 {
		t.Errorf("stale=%d active=%d, want 2 e 3", stale, active)
	}
	names, _ := db.ListActiveLDAPNames()
	if len(names) != 3 || names["a"] != "a" {
		t.Errorf("names = %v", names)
	}
}

func TestSetContactAreaRespectsOverride(t *testing.T) {
	db := newTestDB(t)
	c := addContact(t, db, "u", "esterni")
	db.UpdateContactOverride("u", "u@x.it", "1")
	if err := db.SetContactArea(c.ID, "interni"); err != nil {
		t.Fatal(err)
	}
	if _, area, _ := db.ContactIDAndArea("u"); area != "esterni" {
		t.Errorf("area = %q, want esterni (manual_override)", area)
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/database/ 'SubAreas|CountsAndMembers|ManualContactDelete|StaleCounts|SetContactArea'`
Expected: FAIL di compilazione (`undefined: SetContactSubAreas`, ...).

- [ ] **Step 3: Implementa**

`internal/database/subareas.go`:

```go
package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// contactCols: colonne di contacts con alias c, nello stesso ordine dello
// Scan usato da GetContact.
const contactCols = `c.id, c.uid, c.display_name, c.email, c.ldap_ext, c.primary_number, c.department, c.title, c.description, c.ldap_groups, c.ldap_dn, c.area, c.source, c.disabled, c.manual_override, c.deleted_at, c.last_sync, c.created_at, c.updated_at`

// hasContactInfoSQL: stessa condizione "ha almeno un recapito" di CountByArea.
const hasContactInfoSQL = `(c.email IS NOT NULL AND c.email != '' OR c.ldap_ext IS NOT NULL AND c.ldap_ext != '' OR c.primary_number IS NOT NULL AND c.primary_number != '')`

func scanContactRows(rows *sql.Rows) ([]*Contact, error) {
	defer rows.Close()
	var out []*Contact
	for rows.Next() {
		c := &Contact{}
		if err := rows.Scan(&c.ID, &c.UID, &c.DisplayName, &c.Email, &c.LDAPExt, &c.PrimaryNumber, &c.Department, &c.Title,
			&c.Description, &c.LDAPGroups, &c.LDAPDN, &c.Area, &c.Source, &c.Disabled, &c.ManualOverride,
			&c.DeletedAt, &c.LastSync, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan contact: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetContactSubAreas sostituisce in blocco le sotto-aree del contatto.
func (db *DB) SetContactSubAreas(contactID int64, keys []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM contact_sub_areas WHERE contact_id = ?`, contactID); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to clear sub-areas: %w", err)
	}
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO contact_sub_areas (contact_id, sub_area_key) VALUES (?, ?)`, contactID, k); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to set sub-area: %w", err)
		}
	}
	return tx.Commit()
}

// ListSubAreasForContacts: id contatto → key sotto-aree in ordine di
// visualizzazione. Una sola query per tutti i risultati di una ricerca.
func (db *DB) ListSubAreasForContacts(ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(`
	SELECT csa.contact_id, csa.sub_area_key
	FROM contact_sub_areas csa LEFT JOIN areas a ON a.key = csa.sub_area_key
	WHERE csa.contact_id IN (`+ph+`)
	ORDER BY csa.contact_id, COALESCE(a.sort, 0), COALESCE(a.name, csa.sub_area_key)`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list contact sub-areas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = append(out[id], key)
	}
	return out, rows.Err()
}

// CountBySubArea: contatti attivi con almeno un recapito per sotto-area.
func (db *DB) CountBySubArea() (map[string]int, error) {
	rows, err := db.Query(`
	SELECT csa.sub_area_key, COUNT(*)
	FROM contact_sub_areas csa JOIN contacts c ON c.id = csa.contact_id
	WHERE c.deleted_at IS NULL AND c.disabled = 0 AND ` + hasContactInfoSQL + `
	GROUP BY csa.sub_area_key`)
	if err != nil {
		return nil, fmt.Errorf("failed to count by sub-area: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ContactIDsBySubArea: id dei contatti con quella sotto-area (CardDAV).
func (db *DB) ContactIDsBySubArea(key string) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT contact_id FROM contact_sub_areas WHERE sub_area_key = ?`, key)
	if err != nil {
		return nil, fmt.Errorf("failed to list sub-area contacts: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (db *DB) listMembers(where string, limit int, args ...any) ([]*Contact, int, error) {
	base := ` FROM contacts c WHERE c.deleted_at IS NULL AND c.disabled = 0 AND ` + where
	var total int
	if err := db.QueryRow(`SELECT COUNT(*)`+base, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count members: %w", err)
	}
	rows, err := db.Query(`SELECT `+contactCols+base+` ORDER BY c.display_name LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list members: %w", err)
	}
	list, err := scanContactRows(rows)
	return list, total, err
}

func (db *DB) ListAreaMembers(areaKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.area = ?`, limit, areaKey)
}

func (db *DB) ListSubAreaMembers(subKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.id IN (SELECT contact_id FROM contact_sub_areas WHERE sub_area_key = ?)`, limit, subKey)
}

func (db *DB) ListAreaMembersWithoutSubArea(areaKey string, limit int) ([]*Contact, int, error) {
	return db.listMembers(`c.area = ? AND NOT EXISTS (SELECT 1 FROM contact_sub_areas WHERE contact_id = c.id)`, limit, areaKey)
}

// ContactIDAndArea: id e area salvata di un contatto in qualsiasi stato
// (anche disabled/cancellato). 0, "", nil se non esiste. Dopo un upsert
// LastInsertId non è affidabile sul ramo UPDATE: usare questa.
func (db *DB) ContactIDAndArea(uid string) (int64, string, error) {
	var id int64
	var area sql.NullString
	err := db.QueryRow(`SELECT id, area FROM contacts WHERE uid = ?`, uid).Scan(&id, &area)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("failed to get contact id: %w", err)
	}
	return id, area.String, nil
}

// SetContactArea aggiorna l'area senza toccare chi ha manual_override.
func (db *DB) SetContactArea(id int64, area string) error {
	if _, err := db.Exec(`UPDATE contacts SET area = ?, updated_at = ? WHERE id = ? AND manual_override = 0`, area, time.Now(), id); err != nil {
		return fmt.Errorf("failed to set contact area: %w", err)
	}
	return nil
}

func (db *DB) SetGroupArea(id int64, area string) error {
	if _, err := db.Exec(`UPDATE group_numbers SET area = ?, updated_at = ? WHERE id = ?`, area, time.Now(), id); err != nil {
		return fmt.Errorf("failed to set group area: %w", err)
	}
	return nil
}

// CountStaleLDAP: contatti LDAP non cancellati che SoftDeleteStale
// cancellerebbe con questo syncTime.
func (db *DB) CountStaleLDAP(syncTime time.Time) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE deleted_at IS NULL AND source = 'ldap' AND last_sync < ?`, syncTime).Scan(&n)
	return n, err
}

// CountActiveLDAP: contatti LDAP non cancellati (disabled compresi).
func (db *DB) CountActiveLDAP() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE deleted_at IS NULL AND source = 'ldap'`).Scan(&n)
	return n, err
}

// ListActiveLDAPNames: uid → nome dei contatti LDAP oggi in rubrica, per
// l'anteprima del perimetro.
func (db *DB) ListActiveLDAPNames() (map[string]string, error) {
	rows, err := db.Query(`SELECT uid, display_name FROM contacts WHERE deleted_at IS NULL AND disabled = 0 AND source = 'ldap'`)
	if err != nil {
		return nil, fmt.Errorf("failed to list ldap contacts: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, name string
		if err := rows.Scan(&uid, &name); err != nil {
			return nil, err
		}
		out[uid] = name
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Verifica che passino**

Run: `DTEST ./internal/database/ ''`
Expected: PASS. Se `TestManualContactDeleteCascadesSubAreas` fallisce con "FOREIGN KEY constraint failed", controllare che lo schema in Task 2 abbia `ON DELETE CASCADE` e che `PRAGMA foreign_keys=ON` sia attivo (lo è in `InitDB`).

- [ ] **Step 5: Commit**

```bash
git add internal/database
git commit -m "feat(database): sotto-aree multiple per contatto, conteggi ed elenchi membri"
```

---

### Task 4: Sync LDAP con regole, perimetro e protezione svuotamento

**Files:**
- Modify: `internal/ldap/sync.go` (riscrivere righe 16-75 e 86-296; rimuovere `isEntryAllowedByGroups`, `groupAliases`, `topObservedGroups` se non più usati)
- Create: `internal/ldap/scope.go`
- Modify: `internal/ldap/sync_test.go` (rimuovere `TestDeriveArea`)
- Test: `internal/ldap/evaluate_test.go`, `internal/ldap/scope_test.go`

**Interfaces:**
- Consumes: `arearules.*` (Task 1); `db.AreaTree`, `db.ListScopeRules`, `database.RulesOf`, `db.AddScopeRule` (Task 2); `db.SetContactSubAreas`, `db.ContactIDAndArea`, `db.CountStaleLDAP`, `db.CountActiveLDAP` (Task 3).
- Produces:
  - `type SyncOptions struct{ Force bool }`
  - `type SyncReport struct{ Synced, OutOfScope, StaleHeld, StaleDeleted int }`
  - `func Sync(db *database.DB, cfg *config.Config, opts SyncOptions, onProgress func(done, total int)) (SyncReport, error)`
  - `func SyncContacts(db, cfg, onProgress) error` resta (wrapper con `SyncOptions{}`)
  - interni: `type syncInput`, `type evaluated{Contact *database.Contact; Profile arearules.Profile; SubAreas []string}`, `func evaluateEntries(in syncInput, syncTime time.Time) ([]evaluated, evalStats)`, `func shouldHoldStale(stale, active int) bool`, `func inChainFilter(dn string) string`, `func searchAttributes(extra []string) []string`, `func fetchEntries(conn *ldap.Conn, cfg *config.Config, attrs []string) ([]*ldap.Entry, error)`, `func fetchGroupMembers(conn *ldap.Conn, cfg *config.Config, dns []string) map[string]map[string]bool`, `func loadRules(db *database.DB) ([]arearules.Node, []arearules.Rule, error)`
  - `const ScopeMigratedConfigKey = "scope_rules_migrated"`; `func migrateScopeRules(db *database.DB, groups []string, find func(cnOrDN string) (string, error)) error`

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/ldap/evaluate_test.go`:

```go
package ldap

import (
	"reflect"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/go-ldap/ldap/v3"
)

const testGroupDN = "cn=rubrica_dirigenti,ou=comune-ms,dc=x"

func entry(dn string, attrs map[string][]string) *ldap.Entry { return ldap.NewEntry(dn, attrs) }

func testTree() []arearules.Node {
	return []arearules.Node{
		{Key: "interni", Name: "Personale comunale", Rules: []arearules.Rule{{Kind: "ou", Value: "INTERNI"}}, Children: []arearules.Node{
			{Key: "dirigenti", Name: "Dirigenti", Rules: []arearules.Rule{{Kind: "adgroup", Value: testGroupDN}}},
			{Key: "pc", Name: "Protezione civile", Rules: []arearules.Rule{{Kind: "range", Value: "700-709"}}},
		}},
	}
}

func TestEvaluateEntriesAreasSubAreasDepartment(t *testing.T) {
	rossiDN := "CN=Mario Rossi,OU=Users,OU=INTERNI,DC=x"
	in := syncInput{
		Entries: []*ldap.Entry{
			entry(rossiDN, map[string][]string{"sAMAccountName": {"mario.rossi"}, "displayName": {"Mario Rossi"}, "telephoneNumber": {"700;701"}}),
			entry("CN=Esterno,OU=Users,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"est"}, "physicalDeliveryOfficeName": {"Ditta"}}),
		},
		GroupMembers: map[string]map[string]bool{testGroupDN: {arearules.CanonicalDNLoose(rossiDN): true}},
		Tree:         testTree(),
	}
	out, st := evaluateEntries(in, time.Now())
	if len(out) != 2 || st.Total != 2 {
		t.Fatalf("out=%d stats=%+v", len(out), st)
	}
	r := out[0]
	if r.Contact.Area != "interni" || r.Contact.Department != "Personale comunale" {
		t.Errorf("rossi area=%q dept=%q, want interni e nome area come reparto", r.Contact.Area, r.Contact.Department)
	}
	if !reflect.DeepEqual(r.SubAreas, []string{"dirigenti", "pc"}) {
		t.Errorf("rossi sub = %v", r.SubAreas)
	}
	if out[1].Contact.Area != "" || out[1].Contact.Department != "Ditta" || out[1].SubAreas != nil {
		t.Errorf("esterno = %+v %v, want senza area e reparto invariato", out[1].Contact, out[1].SubAreas)
	}
}

func TestEvaluateEntriesScope(t *testing.T) {
	in := syncInput{
		Entries: []*ldap.Entry{
			entry("CN=A,OU=INTERNI,DC=x", map[string][]string{"sAMAccountName": {"a"}}),
			entry("CN=B,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"b"}}),
			entry("CN=C,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"c"}, "userAccountControl": {"514"}}),
		},
		Tree:       testTree(),
		Scope:      []arearules.Rule{{Kind: "ou", Value: "INTERNI"}},
		OnlyActive: true,
	}
	out, st := evaluateEntries(in, time.Now())
	var uids []string
	for _, e := range out {
		uids = append(uids, e.Contact.UID)
	}
	if !reflect.DeepEqual(uids, []string{"a", "c"}) || st.OutOfScope != 1 || st.Disabled != 1 {
		t.Errorf("uids=%v stats=%+v, want a e c (disabled non filtrato dal perimetro), 1 fuori perimetro", uids, st)
	}
	if !out[1].Contact.Disabled {
		t.Error("c deve risultare disabled")
	}
}

func TestEvaluateEntriesNoScopeMeansEveryone(t *testing.T) {
	in := syncInput{Entries: []*ldap.Entry{entry("CN=B,OU=ESTERNI,DC=x", map[string][]string{"sAMAccountName": {"b"}})}}
	if out, _ := evaluateEntries(in, time.Now()); len(out) != 1 {
		t.Errorf("senza regole di perimetro deve entrare: %d", len(out))
	}
}

func TestShouldHoldStale(t *testing.T) {
	cases := []struct {
		stale, active int
		want          bool
	}{
		{0, 100, false}, {4, 10, false}, {5, 100, false}, {21, 100, true}, {20, 100, false}, {5, 20, true},
	}
	for _, c := range cases {
		if got := shouldHoldStale(c.stale, c.active); got != c.want {
			t.Errorf("shouldHoldStale(%d,%d) = %v, want %v", c.stale, c.active, got, c.want)
		}
	}
}

func TestInChainFilterEscapes(t *testing.T) {
	got := inChainFilter("cn=a(b),dc=x")
	want := `(memberOf:1.2.840.113556.1.4.1941:=cn=a\28b\29,dc=x)`
	if got != want {
		t.Errorf("inChainFilter = %q, want %q", got, want)
	}
}

func TestSearchAttributesAddsRuleAttrs(t *testing.T) {
	got := searchAttributes([]string{"employeetype", "Mail"})
	seen := map[string]int{}
	for _, a := range got {
		seen[a]++
	}
	if seen["employeetype"] != 1 || seen["mail"] != 1 || seen["memberOf"] != 1 {
		t.Errorf("attributi = %v (mail già presente non va duplicato)", got)
	}
}
```

`internal/ldap/scope_test.go`:

```go
package ldap

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateScopeRules(t *testing.T) {
	db := newDB(t)
	find := func(cn string) (string, error) {
		if cn == "all_users-servizi_interni" {
			return "CN=ALL_USERS-SERVIZI_INTERNI,OU=COMUNE-MS,DC=x", nil
		}
		return "", nil
	}
	if err := migrateScopeRules(db, []string{"all_users-servizi_interni", "all_users-servizi_esterni"}, find); err != nil {
		t.Fatal(err)
	}
	if err := migrateScopeRules(db, []string{"all_users-servizi_interni"}, find); err != nil {
		t.Fatal(err)
	}
	rs, _ := db.ListScopeRules()
	if len(rs) != 1 || rs[0].Kind != "adgroup" || rs[0].Value != "cn=all_users-servizi_interni,ou=comune-ms,dc=x" {
		t.Errorf("scope = %+v, want una sola regola (CN inesistente scartato, seconda chiamata no-op)", rs)
	}
}

func TestMigrateScopeRulesNothingFound(t *testing.T) {
	db := newDB(t)
	find := func(string) (string, error) { return "", errors.New("non trovato") }
	if err := migrateScopeRules(db, []string{"fantasma"}, find); err == nil {
		t.Fatal("atteso errore: nessun gruppo trovato")
	}
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v == "1" {
		t.Error("il flag non deve essere scritto se la migrazione fallisce")
	}
}

// Review Focus 1: con manual_override l'upsert conserva l'area vecchia; le
// sotto-aree salvate devono essere quelle dell'area salvata.
func TestApplySubAreasFollowsStoredArea(t *testing.T) {
	db := newDB(t)
	for _, a := range []*database.Area{
		{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"},
		{Key: "fornitori", Name: "Fornitori", ParentKey: "esterni"},
	} {
		if err := db.CreateArea(a); err != nil {
			t.Fatal(err)
		}
	}
	db.UpsertContact(&database.Contact{UID: "u", DisplayName: "U", Area: "esterni", LastSync: time.Now()})
	db.UpdateContactOverride("u", "u@x.it", "1")

	tree := []arearules.Node{
		{Key: "interni", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}, Children: []arearules.Node{
			{Key: "dirigenti", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}},
		}},
		{Key: "esterni", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}, Children: []arearules.Node{
			{Key: "fornitori", Rules: []arearules.Rule{{Kind: "user", Value: "u"}}},
		}},
	}
	ev := evaluated{
		Contact:  &database.Contact{UID: "u", DisplayName: "U", Area: "interni", LastSync: time.Now()},
		Profile:  arearules.Profile{Username: "u"},
		SubAreas: []string{"dirigenti"},
	}
	if err := db.UpsertContact(ev.Contact); err != nil {
		t.Fatal(err)
	}
	applySubAreas(db, ev, tree)
	id, area, _ := db.ContactIDAndArea("u")
	got, _ := db.ListSubAreasForContacts([]int64{id})
	if area != "esterni" || len(got[id]) != 1 || got[id][0] != "fornitori" {
		t.Errorf("area=%q sub=%v, want esterni e [fornitori]", area, got[id])
	}
}

func TestMigrateScopeRulesEmptyEnv(t *testing.T) {
	db := newDB(t)
	if err := migrateScopeRules(db, nil, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v != "1" {
		t.Error("env vuota: flag atteso")
	}
}
```

In `internal/ldap/sync_test.go` rimuovere `TestDeriveArea` (la funzione sparisce).

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/ldap/ ''`
Expected: FAIL di compilazione (`undefined: syncInput`, `migrateScopeRules`, ...).

- [ ] **Step 3: Implementa `scope.go`**

`internal/ldap/scope.go`:

```go
package ldap

import (
	"fmt"
	"log"
	"strings"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// ScopeMigratedConfigKey: flag app_config della conversione una tantum di
// LDAP_ALLOWED_GROUPS in regole di perimetro (scope_rules).
const ScopeMigratedConfigKey = "scope_rules_migrated"

// migrateScopeRules converte i CN (o DN) di LDAP_ALLOWED_GROUPS in regole
// adgroup, una sola volta. find risolve un CN nel DN del gruppo ("" se non
// esiste). Se la variabile è valorizzata ma nessun gruppo viene trovato
// ritorna errore senza scrivere il flag: senza regole entrerebbero tutti
// gli utenti AD.
func migrateScopeRules(db *database.DB, groups []string, find func(string) (string, error)) error {
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v == "1" {
		return nil
	}
	if len(groups) == 0 {
		return db.SetConfig(ScopeMigratedConfigKey, "1")
	}
	var rules []arearules.Rule
	for _, g := range groups {
		dn, err := find(g)
		if err != nil || dn == "" {
			log.Printf("[SYNC] Perimetro: gruppo %q di LDAP_ALLOWED_GROUPS non trovato in AD, scartato (%v)", g, err)
			continue
		}
		r, err := arearules.Normalize(arearules.KindADGroup, "", dn)
		if err != nil {
			log.Printf("[SYNC] Perimetro: DN %q non valido, scartato", dn)
			continue
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return fmt.Errorf("migrazione perimetro: nessuno dei gruppi di LDAP_ALLOWED_GROUPS %v trovato in AD — sync interrotto, rubrica invariata", groups)
	}
	for _, r := range rules {
		if err := db.AddScopeRule(r); err != nil {
			return err
		}
		log.Printf("[SYNC] Perimetro: creata regola %s", arearules.Label(r))
	}
	return db.SetConfig(ScopeMigratedConfigKey, "1")
}

// groupFinder cerca il DN di un gruppo per CN; un valore che è già un DN
// viene restituito così com'è.
func groupFinder(conn *ldap.Conn, baseDN string) func(string) (string, error) {
	return func(cnOrDN string) (string, error) {
		if strings.Contains(cnOrDN, "=") {
			return cnOrDN, nil
		}
		req := ldap.NewSearchRequest(baseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
			"(&(objectClass=group)(cn="+ldap.EscapeFilter(cnOrDN)+"))", []string{"cn"}, nil)
		sr, err := conn.Search(req)
		if err != nil {
			return "", err
		}
		if len(sr.Entries) == 0 {
			return "", nil
		}
		return sr.Entries[0].DN, nil
	}
}
```

- [ ] **Step 4: Riscrivi `sync.go`**

In `internal/ldap/sync.go`:

1. Rimuovere `OUAreaMappingConfigKey`, `DefaultOUAreaMapping`, `ClassificationOU`, `deriveArea`, `loadOUAreaMapping`, `isEntryAllowedByGroups`, `groupAliases`, `topObservedGroups` (verificare con `grep -n` che non siano usati altrove nel package). Tenere `PrimaryNumberPrefixConfigKey`, `loadPrimaryNumberPrefix`, `buildSyncSearchFilter`, `normalizeDescription`, `extractCNFromDN`, `SplitExtensions`, `firstExtension`, `generatePrimaryNumber`.
2. Aggiungere l'import `"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"`.
3. Sostituire `SyncContacts` (righe 86-296) con:

```go
// SyncOptions: Force applica il soft-delete anche oltre la soglia della
// protezione svuotamento (pulsante "Conferma e applica" in admin).
type SyncOptions struct{ Force bool }

// SyncReport riassume un sync. StaleHeld > 0 = soft-delete sospeso dalla
// protezione svuotamento (da mostrare in admin).
type SyncReport struct {
	Synced       int
	OutOfScope   int
	StaleHeld    int
	StaleDeleted int
}

// baseSyncAttributes: attributi sempre richiesti ad AD; quelli citati dalle
// regole si aggiungono (searchAttributes).
var baseSyncAttributes = []string{"uid", "sAMAccountName", "userPrincipalName", "mail", "displayName", "cn",
	"telephoneNumber", "physicalDeliveryOfficeName", "title", "description", "ou", "memberOf", "userAccountControl"}

func searchAttributes(extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(baseSyncAttributes)+len(extra))
	for _, a := range append(append([]string{}, baseSyncAttributes...), extra...) {
		k := strings.ToLower(a)
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// SyncContacts: sync con opzioni di default (sync orario e all'avvio).
func SyncContacts(db *database.DB, cfg *config.Config, onProgress func(done, total int)) error {
	_, err := Sync(db, cfg, SyncOptions{}, onProgress)
	return err
}

// loadRules legge albero aree e perimetro dal DB.
func loadRules(db *database.DB) ([]arearules.Node, []arearules.Rule, error) {
	tree, err := db.AreaTree()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load area rules: %w", err)
	}
	scope, err := db.ListScopeRules()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load scope rules: %w", err)
	}
	return tree, database.RulesOf(scope), nil
}

// Sync legge AD, applica perimetro e regole di area e aggiorna il DB.
// onProgress come prima: (0, total) dopo la ricerca, poi a ogni contatto.
func Sync(db *database.DB, cfg *config.Config, opts SyncOptions, onProgress func(done, total int)) (SyncReport, error) {
	var report SyncReport
	if onProgress == nil {
		onProgress = func(done, total int) {}
	}
	log.Printf("[SYNC] Starting LDAP sync...")

	conn, err := BindForSync(cfg)
	if err != nil {
		return report, fmt.Errorf("failed to bind for sync: %w", err)
	}
	defer conn.Close()

	if err := migrateScopeRules(db, cfg.LDAPAllowedGroups, groupFinder(conn, cfg.LDAPBaseDN)); err != nil {
		return report, err
	}
	tree, scope, err := loadRules(db)
	if err != nil {
		return report, err
	}
	entries, err := fetchEntries(conn, cfg, searchAttributes(arearules.Attributes(tree, scope)))
	if err != nil {
		return report, err
	}
	groupMembers := fetchGroupMembers(conn, cfg, arearules.GroupDNs(tree, scope))
	onProgress(0, len(entries))

	syncTime := time.Now()
	evals, st := evaluateEntries(syncInput{
		Entries:        entries,
		GroupMembers:   groupMembers,
		Tree:           tree,
		Scope:          scope,
		OnlyActive:     cfg.LDAPOnlyActive,
		PrefixTemplate: loadPrimaryNumberPrefix(db, cfg),
	}, syncTime)
	report.OutOfScope = st.OutOfScope

	for i, ev := range evals {
		if err := db.UpsertContact(ev.Contact); err != nil {
			log.Printf("[SYNC] Failed to upsert contact %s: %v", ev.Contact.UID, err)
			continue
		}
		applySubAreas(db, ev, tree)
		report.Synced++
		onProgress(i+1, len(evals))
	}
	log.Printf("[SYNC] Successfully synced %d contacts from LDAP", report.Synced)

	// Reconciliation con protezione svuotamento: chi non è stato toccato in
	// questo giro esce dalla rubrica, ma non se sono troppi (regola di
	// perimetro sbagliata, AD parziale): in quel caso serve Force.
	stale, err := db.CountStaleLDAP(syncTime)
	if err != nil {
		return report, err
	}
	active, err := db.CountActiveLDAP()
	if err != nil {
		return report, err
	}
	if !opts.Force && shouldHoldStale(stale, active) {
		report.StaleHeld = stale
		log.Printf("[SYNC] Calo anomalo: %d contatti su %d uscirebbero dalla rubrica — soft-delete sospeso, serve conferma da admin", stale, active)
	} else if n, err := db.SoftDeleteStale(syncTime); err != nil {
		log.Printf("[SYNC] Failed to soft-delete stale contacts: %v", err)
	} else {
		report.StaleDeleted = int(n)
		if n > 0 {
			log.Printf("[SYNC] Soft-deleted %d stale contacts (no longer present in LDAP result)", n)
		}
	}
	log.Printf("[SYNC] Stats: total=%d disabled=%d outOfScope=%d scopeRules=%d", st.Total, st.Disabled, st.OutOfScope, len(scope))
	return report, nil
}

// shouldHoldStale: sospende il soft-delete se escono almeno 5 contatti e
// più del 20% di quelli non cancellati.
func shouldHoldStale(stale, active int) bool {
	return stale >= 5 && stale*5 > active
}

// applySubAreas salva le sotto-aree calcolate per l'area effettivamente
// salvata: con manual_override l'upsert conserva l'area precedente.
func applySubAreas(db *database.DB, ev evaluated, tree []arearules.Node) {
	id, storedArea, err := db.ContactIDAndArea(ev.Contact.UID)
	if err != nil || id == 0 {
		log.Printf("[SYNC] Sotto-aree di %s non salvate: %v", ev.Contact.UID, err)
		return
	}
	subs := ev.SubAreas
	if storedArea != ev.Contact.Area {
		subs = arearules.SubAreasFor(ev.Profile, tree, storedArea)
	}
	if err := db.SetContactSubAreas(id, subs); err != nil {
		log.Printf("[SYNC] Sotto-aree di %s non salvate: %v", ev.Contact.UID, err)
	}
}

func fetchEntries(conn *ldap.Conn, cfg *config.Config, attrs []string) ([]*ldap.Entry, error) {
	req := ldap.NewSearchRequest(cfg.LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		buildSyncSearchFilter(cfg), attrs, nil)
	sr, err := conn.Search(req)
	if err != nil {
		if ldapErr, ok := err.(*ldap.Error); ok && ldapErr.ResultCode == ldap.LDAPResultOperationsError {
			return nil, fmt.Errorf("LDAP search requires authenticated bind (AD 000004DC). Set LDAP_BIND_DN and LDAP_BIND_PASSWORD in .env: %w", err)
		}
		return nil, fmt.Errorf("failed to search LDAP: %w", err)
	}
	return sr.Entries, nil
}

// inChainFilter: utenti membri del gruppo, annidati compresi
// (LDAP_MATCHING_RULE_IN_CHAIN di AD).
func inChainFilter(groupDN string) string {
	return "(memberOf:1.2.840.113556.1.4.1941:=" + ldap.EscapeFilter(groupDN) + ")"
}

// fetchGroupMembers: DN gruppo → insieme dei DN (canonici) dei membri. Un
// gruppo inesistente o in errore produce un insieme vuoto, non blocca il sync.
func fetchGroupMembers(conn *ldap.Conn, cfg *config.Config, groupDNs []string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(groupDNs))
	for _, g := range groupDNs {
		members := map[string]bool{}
		req := ldap.NewSearchRequest(cfg.LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
			inChainFilter(g), []string{"dn"}, nil)
		sr, err := conn.Search(req)
		switch {
		case err != nil:
			log.Printf("[SYNC] Gruppo %s: ricerca membri fallita: %v", g, err)
		case len(sr.Entries) == 0:
			log.Printf("[SYNC] Gruppo %s: nessun membro (gruppo inesistente o vuoto)", g)
		default:
			for _, e := range sr.Entries {
				members[arearules.CanonicalDNLoose(e.DN)] = true
			}
		}
		out[g] = members
	}
	return out
}

type syncInput struct {
	Entries        []*ldap.Entry
	GroupMembers   map[string]map[string]bool
	Tree           []arearules.Node
	Scope          []arearules.Rule
	OnlyActive     bool
	PrefixTemplate string
}

type evaluated struct {
	Contact  *database.Contact
	Profile  arearules.Profile
	SubAreas []string
}

type evalStats struct{ Total, Disabled, OutOfScope int }

func entryUID(e *ldap.Entry) string {
	for _, a := range []string{"uid", "sAMAccountName", "userPrincipalName"} {
		if v := e.GetAttributeValue(a); v != "" {
			return v
		}
	}
	return ""
}

func isDisabled(e *ldap.Entry) bool {
	uac, err := strconv.ParseInt(e.GetAttributeValue("userAccountControl"), 10, 64)
	return err == nil && uac&0x02 != 0 // bit ACCOUNTDISABLE
}

func buildProfile(e *ldap.Entry, uid string, groupMembers map[string]map[string]bool) arearules.Profile {
	attrs := make(map[string][]string, len(e.Attributes))
	for _, a := range e.Attributes {
		attrs[strings.ToLower(a.Name)] = a.Values
	}
	dn := arearules.CanonicalDNLoose(e.DN)
	var groups []string
	for g, members := range groupMembers {
		if members[dn] {
			groups = append(groups, g)
		}
	}
	return arearules.Profile{
		Username:   strings.ToLower(uid),
		DN:         e.DN,
		Attrs:      attrs,
		Groups:     groups,
		Extensions: SplitExtensions(e.GetAttributeValue("telephoneNumber")),
	}
}

// evaluateEntries trasforma le voci AD in contatti: perimetro (gli account
// disabilitati non passano dal filtro, AD svuota spesso il loro memberOf),
// area e sotto-aree dalle regole, reparto di ripiego = nome dell'area.
func evaluateEntries(in syncInput, syncTime time.Time) ([]evaluated, evalStats) {
	var out []evaluated
	var st evalStats
	for _, e := range in.Entries {
		st.Total++
		disabled := in.OnlyActive && isDisabled(e)
		if disabled {
			st.Disabled++
		}
		uid := entryUID(e)
		if uid == "" {
			continue
		}
		p := buildProfile(e, uid, in.GroupMembers)
		if !disabled && len(in.Scope) > 0 && !arearules.Member(p, in.Scope) {
			st.OutOfScope++
			continue
		}
		area, subs := arearules.Resolve(p, in.Tree)

		displayName := e.GetAttributeValue("displayName")
		if displayName == "" {
			displayName = e.GetAttributeValue("cn")
		}
		if displayName == "" {
			displayName = uid
		}
		telephone := e.GetAttributeValue("telephoneNumber")
		department := e.GetAttributeValue("physicalDeliveryOfficeName")
		if department == "" {
			department = e.GetAttributeValue("ou")
		}
		if department == "" && area != "" {
			if n := arearules.FindNode(in.Tree, area); n != nil {
				department = n.Name
			}
		}
		var groupCNs []string
		for _, dn := range e.GetAttributeValues("memberOf") {
			if cn := extractCNFromDN(dn); cn != "" {
				groupCNs = append(groupCNs, cn)
			}
		}

		out = append(out, evaluated{
			Profile:  p,
			SubAreas: subs,
			Contact: &database.Contact{
				UID:           uid,
				DisplayName:   displayName,
				Email:         e.GetAttributeValue("mail"),
				LDAPExt:       telephone,
				PrimaryNumber: generatePrimaryNumber(firstExtension(telephone), in.PrefixTemplate),
				Department:    department,
				Title:         e.GetAttributeValue("title"),
				Description:   normalizeDescription(e.GetAttributeValue("description")),
				LDAPGroups:    strings.Join(groupCNs, ","),
				LDAPDN:        e.DN,
				Area:          area,
				Disabled:      disabled,
				LastSync:      syncTime,
			},
		})
	}
	return out, st
}
```

Note di implementazione:
- `generatePrimaryNumber` con template vuoto: verificare il comportamento esistente (`sed -n 464,486p internal/ldap/sync.go`); nei test `PrefixTemplate` è vuoto e il valore non viene controllato.
- Controllare che gli import (`strconv`, `encoding/json`) inutilizzati vengano rimossi: `go vet` li segnala.

- [ ] **Step 5: Verifica che passino**

Run: `DTEST ./internal/ldap/ ''`
Expected: PASS. Il pacchetto `internal/ldap` compila; `cmd/server` e `internal/pbx` non ancora (Task 6-7).

- [ ] **Step 6: Commit**

```bash
git add internal/ldap
git commit -m "feat(ldap): sync con regole di area, perimetro da UI e protezione svuotamento"
```

---

### Task 5: Suggerimenti AD e anteprima perimetro

**Files:**
- Create: `internal/ldap/attrstats.go`, `internal/ldap/directory.go`
- Test: `internal/ldap/directory_test.go`

**Interfaces:**
- Consumes: `evaluateEntries`, `fetchEntries`, `fetchGroupMembers`, `searchAttributes`, `loadRules` (Task 4); `db.ListActiveLDAPNames` (Task 3).
- Produces:
  - `type Suggestion struct{ Value, Label string }`
  - `func Suggest(cfg *config.Config, kind, attr, q string) ([]Suggestion, error)` — `ErrInvalidAttr` per nome attributo non valido
  - `type ScopePreview struct{ In, Current int; Entering, Leaving []string; MoreEntering, MoreLeaving int }`
  - `func PreviewScope(db *database.DB, cfg *config.Config) (ScopePreview, error)`
  - interni: `func substringFilter(attr, q string) string`, `func computeAttrStats(entries []*ldap.Entry) []AttrStat`, `func diffScope(evals []evaluated, current map[string]string) ScopePreview`

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/ldap/directory_test.go`:

```go
package ldap

import (
	"reflect"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

func TestSubstringFilter(t *testing.T) {
	if got := substringFilter("cn", ""); got != "(cn=*)" {
		t.Errorf("q vuota = %q", got)
	}
	if got := substringFilter("cn", "a*(b)"); got != `(cn=*a\2a\28b\29*)` {
		t.Errorf("escape = %q", got)
	}
}

func TestComputeAttrStats(t *testing.T) {
	es := []*ldap.Entry{
		ldap.NewEntry("cn=a", map[string][]string{"description": {"Dirigente"}, "objectGUID": {"x"}, "employeeType": {"STD"}}),
		ldap.NewEntry("cn=b", map[string][]string{"description": {"Dirigente"}}),
		ldap.NewEntry("cn=c", map[string][]string{"description": {"Istruttore"}}),
	}
	got := computeAttrStats(es)
	if len(got) != 2 || got[0].Name != "description" || got[0].Count != 3 {
		t.Fatalf("stats = %+v (objectGUID tecnico va escluso)", got)
	}
	if !reflect.DeepEqual(got[0].Examples, []string{"Dirigente", "Istruttore"}) {
		t.Errorf("esempi = %v", got[0].Examples)
	}
}

func TestDiffScope(t *testing.T) {
	evals := []evaluated{
		{Contact: &database.Contact{UID: "a", DisplayName: "Anna"}},
		{Contact: &database.Contact{UID: "n", DisplayName: "Nuovo"}},
		{Contact: &database.Contact{UID: "d", DisplayName: "Disab", Disabled: true}},
	}
	current := map[string]string{"a": "Anna", "x": "Uscente"}
	p := diffScope(evals, current)
	if p.In != 2 || p.Current != 2 {
		t.Errorf("In=%d Current=%d, want 2 e 2 (disabled non conta)", p.In, p.Current)
	}
	if !reflect.DeepEqual(p.Entering, []string{"Nuovo"}) || !reflect.DeepEqual(p.Leaving, []string{"Uscente"}) {
		t.Errorf("entering=%v leaving=%v", p.Entering, p.Leaving)
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/ldap/ 'Substring|AttrStats|DiffScope'`
Expected: FAIL (`undefined: substringFilter`, ...).

- [ ] **Step 3: Implementa `attrstats.go`**

`internal/ldap/attrstats.go` (porting da CruscottoPA `internal/identity/attrstats.go`):

```go
package ldap

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/go-ldap/ldap/v3"
)

// AttrStat descrive un attributo AD compilato sugli utenti, per scegliere
// l'attributo di una regola in admin.
type AttrStat struct {
	Name     string
	Count    int
	Examples []string // fino a 3 valori, i più frequenti
}

// Attributi tecnici o identificativi, inutili per raggruppare le persone.
var technicalAttrs = map[string]bool{
	"objectclass": true, "objectguid": true, "objectsid": true, "objectcategory": true,
	"distinguishedname": true, "cn": true, "name": true, "samaccountname": true,
	"userprincipalname": true, "samaccounttype": true, "useraccountcontrol": true,
	"primarygroupid": true, "memberof": true, "instancetype": true, "codepage": true,
	"countrycode": true, "logoncount": true, "badpwdcount": true, "badpasswordtime": true,
	"lastlogon": true, "lastlogontimestamp": true, "lastlogoff": true, "pwdlastset": true,
	"accountexpires": true, "whencreated": true, "whenchanged": true, "usncreated": true,
	"usnchanged": true, "dscorepropagationdata": true, "logonhours": true,
	"usercertificate": true, "thumbnailphoto": true, "jpegphoto": true, "mail": true,
	"proxyaddresses": true, "displayname": true, "givenname": true, "sn": true,
	"msds-supportedencryptiontypes": true, "lockouttime": true, "admincount": true,
}

// computeAttrStats conta gli attributi testuali brevi compilati, ordinati
// per numero di utenti (poi nome).
func computeAttrStats(entries []*ldap.Entry) []AttrStat {
	counts := map[string]int{}
	values := map[string]map[string]int{}
	names := map[string]string{}
	for _, e := range entries {
		for _, a := range e.Attributes {
			key := strings.ToLower(a.Name)
			if technicalAttrs[key] || !arearules.ValidAttrName(a.Name) || len(a.Values) == 0 {
				continue
			}
			v := strings.TrimSpace(a.Values[0])
			if v == "" || len(v) > 80 || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\x01\x02\x03") {
				continue
			}
			names[key] = a.Name
			counts[key]++
			if values[key] == nil {
				values[key] = map[string]int{}
			}
			values[key][v]++
		}
	}
	out := make([]AttrStat, 0, len(counts))
	for key, n := range counts {
		vs := make([]string, 0, len(values[key]))
		for v := range values[key] {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool {
			if values[key][vs[i]] != values[key][vs[j]] {
				return values[key][vs[i]] > values[key][vs[j]]
			}
			return vs[i] < vs[j]
		})
		if len(vs) > 3 {
			vs = vs[:3]
		}
		out = append(out, AttrStat{Name: strings.ToLower(names[key]), Count: n, Examples: vs})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}
```

Nota: `Name` è minuscolo (`employeetype`), coerente con le regole salvate; il test si aspetta `description`.

- [ ] **Step 4: Implementa `directory.go`**

`internal/ldap/directory.go`:

```go
package ldap

import (
	"errors"
	"fmt"
	"sort"
	"strings"

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
```

Aggiungere `"time"` agli import di `directory.go`.

- [ ] **Step 5: Verifica che passino**

Run: `DTEST ./internal/ldap/ ''`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ldap
git commit -m "feat(ldap): suggerimenti AD per le regole e anteprima del perimetro"
```

---

### Task 6: Sync PBX con regole e riapplicazione da DB

**Files:**
- Modify: `internal/pbx/sync.go:419-569` (`ApplyPeers`, `ApplyCallGroups`) + nuova `ReapplyAreas`
- Test: `internal/pbx/sync_test.go` (aggiungere test; adeguare quelli che usano `SetAreaRange`/`MatchExtensionRange`)

**Interfaces:**
- Consumes: `db.AreaTree` (Task 2), `arearules.Resolve`/`FindNode` (Task 1), `db.ContactIDAndArea`, `db.SetContactSubAreas`, `db.SetContactArea`, `db.SetGroupArea` (Task 3).
- Produces: `func ReapplyAreas(db *database.DB) error` — riapplica le regole a contatti `source='pbx'` e gruppi `source='pbx'` dal DB, senza scraping.

- [ ] **Step 1: Scrivi i test che falliscono**

Aggiungere a `internal/pbx/sync_test.go` (usa l'helper di DB già presente nel file; se non esiste, crearlo come `newTestDB` del package database con `database.InitDB(filepath.Join(t.TempDir(), "t.db"))`):

```go
func TestApplyPeersUsesRangeRules(t *testing.T) {
	db := newTestDB(t)
	a, _ := db.GetAreaByKey("uffici")
	if err := db.AddAreaRule(a.ID, arearules.Rule{Kind: "range", Value: "750-759"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateArea(&database.Area{Key: "centralino", Name: "Centralino", ParentKey: "uffici"}); err != nil {
		t.Fatal(err)
	}
	sub, _ := db.GetAreaByKey("centralino")
	db.AddAreaRule(sub.ID, arearules.Rule{Kind: "range", Value: "750-750"})

	if _, err := ApplyPeers(db, []Peer{{Extension: "750", CallerID: "Portineria"}, {Extension: "900", CallerID: "Altro"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, _ := db.GetContact("pbx-750")
	if c.Area != "uffici" || c.Department != "Uffici" {
		t.Errorf("pbx-750 area=%q dept=%q", c.Area, c.Department)
	}
	subs, _ := db.ListSubAreasForContacts([]int64{c.ID})
	if len(subs[c.ID]) != 1 || subs[c.ID][0] != "centralino" {
		t.Errorf("sotto-aree pbx-750 = %v", subs[c.ID])
	}
	if c, _ := db.GetContact("pbx-900"); c.Area != "" {
		t.Errorf("pbx-900 area = %q, want vuota", c.Area)
	}
}

func TestReapplyAreas(t *testing.T) {
	db := newTestDB(t)
	if _, err := ApplyPeers(db, []Peer{{Extension: "760", CallerID: "X"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCallGroups(db, []CallGroup{{Extension: "761", Name: "G"}}); err != nil {
		t.Fatal(err)
	}
	a, _ := db.GetAreaByKey("esterni")
	db.AddAreaRule(a.ID, arearules.Rule{Kind: "range", Value: "760-769"})
	if err := ReapplyAreas(db); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.GetContact("pbx-760"); c.Area != "esterni" {
		t.Errorf("contatto area = %q", c.Area)
	}
	if g, _ := db.GetGroupByNumber("761"); g.Area != "esterni" {
		t.Errorf("gruppo area = %q", g.Area)
	}
}
```

Import da aggiungere al file di test: `"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"`, `"github.com/Comune-di-Montesilvano/Rubrica/internal/database"`, `"time"` (se mancanti). Verificare con `grep -n "func newTestDB\|Peer{\|CallGroup{" internal/pbx/sync_test.go` i nomi esatti dei campi di `Peer`/`CallGroup` e adeguare. Se esistono test che chiamano `db.SetAreaRange`, sostituirli con `db.AddAreaRule(id, arearules.Rule{Kind: "range", Value: "<da>-<a>"})`.

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/pbx/ 'RangeRules|ReapplyAreas'`
Expected: FAIL (`undefined: ReapplyAreas`, `MatchExtensionRange` inesistente).

- [ ] **Step 3: Implementa**

In `internal/pbx/sync.go` aggiungere l'import `arearules` e:

1. In `ApplyPeers` sostituire il blocco `areas, err := db.ListAreas()` (con il commento) con:

```go
	// Regole di area (vedi internal/arearules): per un peer valgono solo le
	// regole range, il profilo non ha dati AD. L'area trovata fa anche da
	// reparto, altrimenti resterebbe "Centralino - non mappato".
	tree, err := db.AreaTree()
	if err != nil {
		log.Printf("[PBX] Failed to load area rules: %v", err)
	}
```

e nel loop sostituire il blocco `area, department := ...` fino a `UpsertPBXContact` compreso con:

```go
		area, department := "", "Centralino - non mappato"
		prof := arearules.Profile{Extensions: []string{p.Extension}}
		resolved, subs := arearules.Resolve(prof, tree)
		if resolved != "" {
			area = resolved
			if n := arearules.FindNode(tree, resolved); n != nil {
				department = n.Name
			}
		} else if c := database.MatchGroupCategory(p.Extension, categories); c != nil {
			department = c.Name
		}
		c := &database.Contact{
			UID:           "pbx-" + p.Extension,
			DisplayName:   p.CallerID,
			LDAPExt:       p.Extension,
			PrimaryNumber: p.Extension,
			Department:    department,
			Area:          area,
			LastSync:      syncTime,
		}
		if err := db.UpsertPBXContact(c); err != nil {
			log.Printf("[PBX] skip peer %s: %v", p.Extension, err)
			continue
		}
		if id, stored, err := db.ContactIDAndArea(c.UID); err == nil && id != 0 {
			if stored != area {
				subs = arearules.SubAreasFor(prof, tree, stored)
			}
			if err := db.SetContactSubAreas(id, subs); err != nil {
				log.Printf("[PBX] sotto-aree di %s non salvate: %v", c.UID, err)
			}
		}
```

2. In `ApplyCallGroups` sostituire `areas, err := db.ListAreas()` (e commento) con `tree, err := db.AreaTree()` + log analogo, e il blocco `area := ""; if a := database.MatchExtensionRange(...)` con:

```go
		area, _ := arearules.Resolve(arearules.Profile{Extensions: []string{g.Extension}}, tree)
```

3. Aggiungere in fondo al file:

```go
// ReapplyAreas riapplica le regole di area ai contatti e ai gruppi del
// centralino già nel DB, senza interrogare il ViVo: serve subito dopo una
// modifica alle regole in admin. Il reparto si riallinea al sync PBX
// successivo.
func ReapplyAreas(db *database.DB) error {
	tree, err := db.AreaTree()
	if err != nil {
		return err
	}
	contacts, err := db.ListPBXContacts()
	if err != nil {
		return err
	}
	for _, c := range contacts {
		prof := arearules.Profile{Extensions: []string{c.LDAPExt}}
		area, subs := arearules.Resolve(prof, tree)
		if err := db.SetContactArea(c.ID, area); err != nil {
			return err
		}
		_, stored, err := db.ContactIDAndArea(c.UID)
		if err != nil {
			return err
		}
		if stored != area {
			subs = arearules.SubAreasFor(prof, tree, stored)
		}
		if err := db.SetContactSubAreas(c.ID, subs); err != nil {
			return err
		}
	}
	groups, err := db.ListGroupsBySource("pbx")
	if err != nil {
		return err
	}
	for _, g := range groups {
		area, _ := arearules.Resolve(arearules.Profile{Extensions: []string{g.Number}}, tree)
		if err := db.SetGroupArea(g.ID, area); err != nil {
			return err
		}
	}
	return nil
}
```

Verificare con `sed -n 570,600p internal/database/sqlite.go` che `ListContactsBySource` includa anche i disabled/cancellati o meno: va bene in entrambi i casi (riapplicare a un cancellato è innocuo).

- [ ] **Step 4: Verifica che passino**

Run: `DTEST ./internal/pbx/ ''`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pbx
git commit -m "feat(pbx): aree e sotto-aree dei peer da regole, riapplicazione da DB"
```

---

### Task 7: Admin — trigger sync, albero aree, rimozione mapping OU

**Files:**
- Create: `cmd/server/admin_areas.go`
- Modify: `cmd/server/main.go` (rotte righe 293-300; rimuovere `loadOUMapping`, `buildOUMappingData`, `renderOUMapping`, `handleAdminOUMapping`, `handleAdminSaveOUMapping`, `handleAdminAddOU`, `handleAdminDeleteOU` righe ~1058-1240; rimuovere `handleAdminAreas`, `handleAdminCreateArea`, `handleAdminRenameArea`, `handleAdminDeleteArea`, `renderAdminAreas` righe ~1270-1360 che si spostano in `admin_areas.go`; `handleAdminSync` riga 797; `ldapSyncWorker` riga 341; sync iniziale riga 241)
- Delete: `web/templates/admin_ou_mapping.html`, `web/templates/admin_page_ou_mapping.html`
- Modify: `web/templates/rail.html:43` (voce "Mapping OU"), `web/templates/admin_areas.html`, `web/templates/admin_page_areas.html`
- Modify: `web/static/css/style.css`
- Test: `cmd/server/templates_test.go`

**Interfaces:**
- Consumes: `db.ListAreas`, `database.BuildAreaNodes`, `db.CreateArea`, `db.RenameArea`, `db.MoveArea`, `db.DeleteArea`, `db.ListAreaRules`, `db.CountByArea`, `db.CountBySubArea` (Task 2-3); `ldap.Sync`, `ldap.SyncOptions`, `ldap.SyncReport` (Task 4); `pbx.ReapplyAreas` (Task 6).
- Produces:
  - `func startLDAPSync(force bool)` — avvia il sync manuale; se già in corso imposta "risincronizza al termine"
  - `func rulesChanged()` — `pbx.ReapplyAreas` + `startLDAPSync(false)`
  - `var lastSyncReport ldap.SyncReport` (protetto da `syncReportMu sync.Mutex`), `func getLastSyncReport() ldap.SyncReport`
  - `func areasPageData() (map[string]interface{}, error)` — chiavi `AreaNodes []*database.AreaNode`, `RuleCounts map[int64]int`, `AreaCounts map[string]int`, `SubAreaCounts map[string]int`
  - template `admin_areas.html` (frammento `#areas-content`), rotte `POST /admin/areas/{id}/move`

- [ ] **Step 1: Scrivi il test del template che fallisce**

Aggiungere a `cmd/server/templates_test.go`:

```go
func TestAdminAreasTreeRendersSubAreas(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_areas.html"))
	interni := &database.Area{ID: 1, Key: "interni", Name: "Personale comunale"}
	dir := &database.Area{ID: 2, Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}
	data := map[string]interface{}{
		"Messages":      map[string]string{"save": "Salva", "delete": "Elimina"},
		"AreaNodes":     []*database.AreaNode{{Area: interni, Children: []*database.Area{dir}}},
		"RuleCounts":    map[int64]int{1: 1, 2: 0},
		"AreaCounts":    map[string]int{"interni": 240},
		"SubAreaCounts": map[string]int{"dirigenti": 4},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_areas.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{`href="/admin/areas/2"`, "Dirigenti", `name="parent_key" value="interni"`, "nessuna regola", "/admin/areas/2/move?dir=up"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q nel frammento", want)
		}
	}
}
```

Import aggiuntivi nel file di test: `"strings"`, `"github.com/Comune-di-Montesilvano/Rubrica/internal/database"`.

- [ ] **Step 2: Verifica che fallisca**

Run: `DTEST ./cmd/server/ 'AdminAreasTree'`
Expected: FAIL (il pacchetto non compila per i simboli rimossi nei task precedenti, poi per il template).

- [ ] **Step 3: Trigger del sync e worker**

Creare `cmd/server/admin_areas.go` con:

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/i18n"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/pbx"
	"github.com/gorilla/mux"
)

var (
	syncReportMu   sync.Mutex
	lastSyncReport ldap.SyncReport
	// syncRerun: una modifica alle regole arrivata durante un sync in corso
	// fa ripartire il sync al termine, invece di avviarne un secondo in
	// parallelo.
	syncRerun bool
)

func setLastSyncReport(r ldap.SyncReport) {
	syncReportMu.Lock()
	defer syncReportMu.Unlock()
	lastSyncReport = r
}

func getLastSyncReport() ldap.SyncReport {
	syncReportMu.Lock()
	defer syncReportMu.Unlock()
	return lastSyncReport
}

// runLDAPSync esegue un sync e ne registra esito e report (usato da worker
// orario, sync iniziale e sync manuali).
func runLDAPSync(opts ldap.SyncOptions, onProgress func(done, total int)) error {
	report, err := ldap.Sync(db, cfg, opts, onProgress)
	if err != nil {
		return err
	}
	lastSync = time.Now()
	setLastSyncReport(report)
	return nil
}

// startLDAPSync avvia il sync manuale in background con stato visibile in
// admin. Se ne è già in corso uno, chiede di ripeterlo al termine.
func startLDAPSync(force bool) {
	ldapManualSync.mu.Lock()
	if ldapManualSync.Running {
		syncRerun = true
		ldapManualSync.mu.Unlock()
		return
	}
	ldapManualSync.mu.Unlock()
	ldapManualSync.start()
	go func() {
		for {
			err := runLDAPSync(ldap.SyncOptions{Force: force}, func(done, total int) {
				if total > 0 {
					ldapManualSync.setPhase(fmt.Sprintf("Elaborazione contatti: %d/%d", done, total))
				}
			})
			if err != nil {
				log.Printf("[SYNC] Manual sync failed: %v", err)
			}
			ldapManualSync.mu.Lock()
			again := syncRerun
			syncRerun = false
			if !again {
				ldapManualSync.mu.Unlock()
				ldapManualSync.finish(err)
				return
			}
			ldapManualSync.mu.Unlock()
			force = false
		}
	}()
}

// rulesChanged: dopo una modifica a regole o albero delle aree, riapplica
// subito le regole ai dati del centralino e risincronizza AD.
func rulesChanged() {
	if err := pbx.ReapplyAreas(db); err != nil {
		log.Printf("[ADMIN] Riapplicazione regole al centralino fallita: %v", err)
	}
	startLDAPSync(false)
}
```

In `main.go`:
- sync iniziale (riga ~241): sostituire `ldap.SyncContacts(db, cfg, nil)` + `lastSync = time.Now()` con `if err := runLDAPSync(ldap.SyncOptions{}, nil); err != nil { log.Printf("[SYNC] Initial sync failed: %v", err) }`.
- `ldapSyncWorker`: stessa sostituzione.
- `handleAdminSync`: corpo diventa `startLDAPSync(r.FormValue("force") == "1"); renderSyncStatus(w, r)`.
- `syncStatus.finish(err)`: quando `err == nil` e `getLastSyncReport().StaleHeld > 0`, impostare `IsError = true` e `Message = fmt.Sprintf("Calo anomalo: %d contatti uscirebbero dalla rubrica. Cancellazione sospesa: verifica il perimetro in Aree e conferma.", report.StaleHeld)`. Implementarlo in `finish` leggendo `getLastSyncReport()` dopo il controllo di `err` (attenzione al lock: `getLastSyncReport` usa `syncReportMu`, non `s.mu`, quindi nessun deadlock).

- [ ] **Step 4: Handler albero aree**

Aggiungere a `cmd/server/admin_areas.go`:

```go
// areasPageData: dati per la pagina /admin/areas (albero, conteggi, regole).
func areasPageData() (map[string]interface{}, error) {
	areas, err := db.ListAreas()
	if err != nil {
		return nil, err
	}
	ruleCounts := map[int64]int{}
	for _, a := range areas {
		rs, err := db.ListAreaRules(a.ID)
		if err != nil {
			return nil, err
		}
		ruleCounts[a.ID] = len(rs)
	}
	counts, err := db.CountByArea()
	if err != nil {
		return nil, err
	}
	subCounts, err := db.CountBySubArea()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"AreaNodes":     database.BuildAreaNodes(areas),
		"RuleCounts":    ruleCounts,
		"AreaCounts":    counts,
		"SubAreaCounts": subCounts,
	}, nil
}

func renderAdminAreas(w http.ResponseWriter, r *http.Request) {
	data, err := areasPageData()
	if err != nil {
		http.Error(w, "Failed to load areas", http.StatusInternalServerError)
		return
	}
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	templates.ExecuteTemplate(w, "admin_areas.html", data)
}

func handleAdminAreas(w http.ResponseWriter, r *http.Request) {
	data := railData()
	page, err := areasPageData()
	if err != nil {
		http.Error(w, "Failed to load areas", http.StatusInternalServerError)
		return
	}
	for k, v := range page {
		data[k] = v
	}
	scope, err := scopeData()
	if err != nil {
		log.Printf("[ADMIN] Perimetro non caricato: %v", err)
	}
	for k, v := range scope {
		data[k] = v
	}
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-areas"
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	templates.ExecuteTemplate(w, "admin_page_areas.html", data)
}

func handleAdminCreateArea(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	parent := strings.TrimSpace(r.FormValue("parent_key"))
	key := slugify(name)
	if name == "" || key == "" {
		renderAdminAreas(w, r)
		return
	}
	if err := db.CreateArea(&database.Area{Key: key, Name: name, ParentKey: parent}); err != nil {
		if errors.Is(err, database.ErrNestedSubArea) || errors.Is(err, database.ErrParentNotFound) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		log.Printf("[ADMIN] Failed to create area %q: %v", name, err)
	}
	renderAdminAreas(w, r)
}

func areaIDFrom(r *http.Request) int64 {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	return id
}

func handleAdminRenameArea(w http.ResponseWriter, r *http.Request) {
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if err := db.RenameArea(areaIDFrom(r), name); err != nil {
			log.Printf("[ADMIN] Failed to rename area: %v", err)
		}
	}
	renderAdminAreas(w, r)
}

func handleAdminMoveArea(w http.ResponseWriter, r *http.Request) {
	if err := db.MoveArea(areaIDFrom(r), r.URL.Query().Get("dir") == "up"); err != nil {
		log.Printf("[ADMIN] Failed to move area: %v", err)
	}
	rulesChanged() // la precedenza cambia l'assegnazione
	renderAdminAreas(w, r)
}

func handleAdminDeleteArea(w http.ResponseWriter, r *http.Request) {
	if a, _ := db.GetArea(areaIDFrom(r)); a != nil && a.Key == "uffici" {
		http.Error(w, "L'area Uffici è riservata ai gruppi di chiamata", http.StatusUnprocessableEntity)
		return
	}
	if err := db.DeleteArea(areaIDFrom(r)); err != nil {
		log.Printf("[ADMIN] Failed to delete area: %v", err)
	}
	rulesChanged()
	renderAdminAreas(w, r)
}
```

`scopeData()` arriva in Task 9: per far compilare questo task aggiungere ora in `admin_areas.go` uno stub completo che il Task 9 sostituisce:

```go
// scopeData: dati della sezione Perimetro (completata nel Task 9).
func scopeData() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
```

Rimuovere da `main.go` le vecchie versioni di questi handler e `renderAdminAreas`, e tutto il codice OU mapping elencato in **Files**. Rotte in `main.go` (sostituire le righe 293-300):

```go
	admin.HandleFunc("/areas", handleAdminAreas).Methods("GET")
	admin.HandleFunc("/areas", handleAdminCreateArea).Methods("POST")
	admin.HandleFunc("/areas/{id}", handleAdminRenameArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/move", handleAdminMoveArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/delete", handleAdminDeleteArea).Methods("POST")
```

- [ ] **Step 5: Template albero**

Sostituire `web/templates/admin_areas.html` con:

```html
<div class="section-head">
    <h3>Aree e sotto-aree</h3>
</div>
<p class="helptext" style="margin:8px 0 12px;">Ogni contatto finisce nella <strong>prima</strong> area, in quest'ordine, di cui soddisfa le regole; poi in <strong>tutte</strong> le sotto-aree di quell'area di cui soddisfa le regole. Le regole si modificano aprendo l'area. Ogni modifica avvia subito un sync.</p>

{{$m := .Messages}}{{$rc := .RuleCounts}}{{$ac := .AreaCounts}}{{$sc := .SubAreaCounts}}
<table class="area-tree">
    <thead><tr><th>Nome</th><th>Chiave</th><th>Regole</th><th>Contatti</th><th></th></tr></thead>
    <tbody>
    {{range .AreaNodes}}
        {{$parent := .}}
        <tr>
            <td>
                <form id="area-form-{{.ID}}" hx-post="/admin/areas/{{.ID}}" hx-target="#areas-content" hx-swap="innerHTML" style="margin:0;">
                    <input type="text" name="name" value="{{.Name}}" class="input" style="width:100%;">
                </form>
            </td>
            <td class="num">{{.Key}}</td>
            <td><a href="/admin/areas/{{.ID}}">{{with index $rc .ID}}{{.}} regole{{else}}nessuna regola{{end}}</a></td>
            <td class="num">{{index $ac .Key}}</td>
            <td>
                <div class="row-actions">
                    <button type="submit" form="area-form-{{.ID}}" class="btn btn-ghost">{{index $m "save"}}</button>
                    <button hx-post="/admin/areas/{{.ID}}/move?dir=up" hx-target="#areas-content" hx-swap="innerHTML" class="btn btn-ghost" title="Sposta su">&uarr;</button>
                    <button hx-post="/admin/areas/{{.ID}}/move?dir=down" hx-target="#areas-content" hx-swap="innerHTML" class="btn btn-ghost" title="Sposta giù">&darr;</button>
                    {{if ne .Key "uffici"}}
                    <button hx-post="/admin/areas/{{.ID}}/delete" hx-confirm="Eliminare l'area {{.Name}} con le sue sotto-aree e regole?" hx-target="#areas-content" hx-swap="innerHTML" class="btn-danger">{{index $m "delete"}}</button>
                    {{end}}
                </div>
            </td>
        </tr>
        {{range .Children}}
        <tr class="area-sub-row">
            <td>
                <form id="area-form-{{.ID}}" hx-post="/admin/areas/{{.ID}}" hx-target="#areas-content" hx-swap="innerHTML" style="margin:0;padding-left:22px;">
                    <input type="text" name="name" value="{{.Name}}" class="input" style="width:100%;">
                </form>
            </td>
            <td class="num">{{.Key}}</td>
            <td><a href="/admin/areas/{{.ID}}">{{with index $rc .ID}}{{.}} regole{{else}}nessuna regola{{end}}</a></td>
            <td class="num">{{index $sc .Key}}</td>
            <td>
                <div class="row-actions">
                    <button type="submit" form="area-form-{{.ID}}" class="btn btn-ghost">{{index $m "save"}}</button>
                    <button hx-post="/admin/areas/{{.ID}}/move?dir=up" hx-target="#areas-content" hx-swap="innerHTML" class="btn btn-ghost" title="Sposta su">&uarr;</button>
                    <button hx-post="/admin/areas/{{.ID}}/move?dir=down" hx-target="#areas-content" hx-swap="innerHTML" class="btn btn-ghost" title="Sposta giù">&darr;</button>
                    <button hx-post="/admin/areas/{{.ID}}/delete" hx-confirm="Eliminare la sotto-area {{.Name}}?" hx-target="#areas-content" hx-swap="innerHTML" class="btn-danger">{{index $m "delete"}}</button>
                </div>
            </td>
        </tr>
        {{end}}
        {{if ne .Key "uffici"}}
        <tr class="area-sub-row">
            <td colspan="5">
                <form hx-post="/admin/areas" hx-target="#areas-content" hx-swap="innerHTML" class="field-row" style="margin:0;padding-left:22px;">
                    <input type="hidden" name="parent_key" value="{{$parent.Key}}">
                    <input type="text" name="name" placeholder="Nuova sotto-area di {{$parent.Name}}" class="input" required>
                    <button type="submit" class="btn btn-ghost" style="flex:none;">Aggiungi sotto-area</button>
                </form>
            </td>
        </tr>
        {{end}}
    {{end}}
    </tbody>
</table>

<form hx-post="/admin/areas" hx-target="#areas-content" hx-swap="innerHTML" class="field-row" style="margin-top:14px;">
    <input type="text" name="name" placeholder="Nuova area (es. Fornitori esterni)" class="input" required>
    <button type="submit" class="btn btn-primary" style="flex:none;">Aggiungi area</button>
</form>
```

In `web/templates/admin_page_areas.html` sostituire il blocco `<main>` con:

```html
        <main class="main" style="max-width:980px;">
            <h1 class="page-title">Aree</h1>
            <div id="scope-content">{{template "admin_scope.html" .}}</div>
            <div id="areas-content">{{template "admin_areas.html" .}}</div>
        </main>
```

Creare subito `web/templates/admin_scope.html` (sostituito nel Task 9) con il contenuto provvisorio:

```html
<div class="section-head"><h3>Perimetro della rubrica</h3></div>
```

In `rail.html` rimuovere il link `<a href="/admin/ou-mapping" ...>...</a>` (riga 43 e il suo contenuto fino a `</a>`). Eliminare `web/templates/admin_ou_mapping.html` e `web/templates/admin_page_ou_mapping.html` con `git rm`.

In `web/static/css/style.css` aggiungere in fondo:

```css
.area-sub-row td:first-child { padding-left: 18px; }
.area-sub-row { background: var(--accent-tint); }
```

- [ ] **Step 6: Verifica**

Run: `DTEST ./cmd/server/ ''` poi `DTEST ./... ''`
Expected: PASS su `cmd/server` (template test incluso). `go vet` è eseguito implicitamente da `go test`; se segnala import inutilizzati in `main.go` (`encoding/json` ecc.) rimuoverli. I riferimenti di `internal/carddav` a `LDAPOUFilters` restano finché il Task 12 non li rimuove: compilano perché il campo esiste ancora.

- [ ] **Step 7: Commit**

```bash
git add -A cmd/server web
git commit -m "feat(admin): albero aree e sotto-aree, sync dopo modifica, via il mapping OU"
```

---

### Task 8: Admin — dettaglio area, regole e suggerimenti AD

**Files:**
- Modify: `cmd/server/admin_areas.go`, `cmd/server/main.go` (rotte)
- Create: `web/templates/admin_page_area_detail.html`, `web/templates/admin_area_detail.html`, `web/templates/admin_rules.html`, `web/templates/admin_ad_suggestions.html`
- Modify: `web/static/css/style.css`
- Test: `cmd/server/templates_test.go`, `cmd/server/admin_areas_test.go`

**Interfaces:**
- Consumes: `arearules.Normalize`, `arearules.Label`, `arearules.ErrInvalidRule`; `db.GetArea`, `db.ListAreaRules`, `db.AddAreaRule`, `db.DeleteAreaRule`, `db.ListAreaMembers`, `db.ListSubAreaMembers`, `db.ListAreaMembersWithoutSubArea`, `db.ListAreas`; `ldap.Suggest`, `ldap.ErrInvalidAttr`; `rulesChanged()`.
- Produces:
  - `type ruleView struct{ ID int64; Label string }`, `func ruleViews(rs []*database.AreaRule) []ruleView`
  - `type rulesBlock struct{ Rules []ruleView; PostURL, DeleteURLPrefix, Target, FormID string; AllowRange bool; Error string }` — dati di `admin_rules.html`
  - `func parseRuleForm(r *http.Request) (arearules.Rule, error)`
  - rotte `GET /admin/areas/{id}`, `POST /admin/areas/{id}/rules`, `POST /admin/areas/{id}/rules/{rid}/delete`, `GET /admin/ad/suggest`

- [ ] **Step 1: Scrivi i test che falliscono**

`cmd/server/admin_areas_test.go`:

```go
package main

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
)

func TestParseRuleForm(t *testing.T) {
	form := url.Values{"kind": {"attr"}, "attr": {"Description"}, "value": {" Dirigente "}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got, err := parseRuleForm(r)
	if err != nil || got != (arearules.Rule{Kind: "attr", Attr: "description", Value: "Dirigente"}) {
		t.Errorf("parseRuleForm = %+v, %v", got, err)
	}
	bad := url.Values{"kind": {"attr"}, "attr": {"1x"}, "value": {"v"}}
	r = httptest.NewRequest("POST", "/", strings.NewReader(bad.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := parseRuleForm(r); !errors.Is(err, arearules.ErrInvalidRule) {
		t.Errorf("err = %v, want ErrInvalidRule", err)
	}
}
```

Aggiungere a `cmd/server/templates_test.go`:

```go
func TestAdminRulesBlockRenders(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_rules.html"))
	data := rulesBlock{
		Rules:           []ruleView{{ID: 7, Label: "Gruppo AD rubrica_dirigenti"}},
		PostURL:         "/admin/areas/2/rules",
		DeleteURLPrefix: "/admin/areas/2/rules/",
		Target:          "#area-detail-content",
		FormID:          "rule-2",
		AllowRange:      true,
		Error:           "nome di attributo LDAP non valido",
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_rules.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"Gruppo AD rubrica_dirigenti", `hx-post="/admin/areas/2/rules/7/delete"`, `value="range"`, "/admin/ad/suggest", "nome di attributo LDAP non valido"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q", want)
		}
	}
}

func TestAdminSuggestionsRender(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_ad_suggestions.html"))
	data := map[string]interface{}{
		"Target":      "rule-2-value",
		"Suggestions": []ldap.Suggestion{{Value: `CN=A"B,DC=x`, Label: "A\"B"}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_ad_suggestions.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `data-target="rule-2-value"`) || strings.Contains(buf.String(), `A"B,DC`) {
		t.Errorf("suggerimento non reso o non escapato: %s", buf.String())
	}
}
```

Import aggiuntivo: `"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"`.

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./cmd/server/ 'ParseRuleForm|RulesBlock|Suggestions'`
Expected: FAIL (`undefined: parseRuleForm`, template mancanti).

- [ ] **Step 3: Handler**

Aggiungere a `cmd/server/admin_areas.go`:

```go
type ruleView struct {
	ID    int64
	Label string
}

func ruleViews(rs []*database.AreaRule) []ruleView {
	out := make([]ruleView, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleView{ID: r.ID, Label: arearules.Label(r.Rule)})
	}
	return out
}

// rulesBlock alimenta admin_rules.html, condiviso da aree e perimetro.
type rulesBlock struct {
	Rules           []ruleView
	PostURL         string
	DeleteURLPrefix string // + "<id>/delete"
	Target          string
	FormID          string
	AllowRange      bool // false nel perimetro (vale solo per AD)
	Error           string
}

func parseRuleForm(r *http.Request) (arearules.Rule, error) {
	return arearules.Normalize(r.FormValue("kind"), r.FormValue("attr"), r.FormValue("value"))
}

// areaDetailData: regole, membri e "senza sottogruppo" di un'area.
func areaDetailData(a *database.Area, ruleErr string) (map[string]interface{}, error) {
	rules, err := db.ListAreaRules(a.ID)
	if err != nil {
		return nil, err
	}
	var members []*database.Contact
	var total int
	if a.ParentKey == "" {
		members, total, err = db.ListAreaMembers(a.Key, 30)
	} else {
		members, total, err = db.ListSubAreaMembers(a.Key, 30)
	}
	if err != nil {
		return nil, err
	}
	data := map[string]interface{}{
		"Area":         a,
		"Members":      members,
		"MembersTotal": total,
		"RulesBlock": rulesBlock{
			Rules:           ruleViews(rules),
			PostURL:         fmt.Sprintf("/admin/areas/%d/rules", a.ID),
			DeleteURLPrefix: fmt.Sprintf("/admin/areas/%d/rules/", a.ID),
			Target:          "#area-detail-content",
			FormID:          fmt.Sprintf("rule-%d", a.ID),
			AllowRange:      true,
			Error:           ruleErr,
		},
	}
	if a.ParentKey == "" {
		areas, err := db.ListAreas()
		if err != nil {
			return nil, err
		}
		hasSub := false
		for _, x := range areas {
			hasSub = hasSub || x.ParentKey == a.Key
		}
		if hasSub {
			without, n, err := db.ListAreaMembersWithoutSubArea(a.Key, 30)
			if err != nil {
				return nil, err
			}
			data["WithoutSub"], data["WithoutSubTotal"], data["HasSubAreas"] = without, n, true
		}
	} else if parent, err := db.GetAreaByKey(a.ParentKey); err == nil && parent != nil {
		data["Parent"] = parent
	}
	return data, nil
}

func renderAreaDetail(w http.ResponseWriter, r *http.Request, a *database.Area, ruleErr string) {
	data, err := areaDetailData(a, ruleErr)
	if err != nil {
		http.Error(w, "Failed to load area", http.StatusInternalServerError)
		return
	}
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	templates.ExecuteTemplate(w, "admin_area_detail.html", data)
}

func handleAdminAreaDetail(w http.ResponseWriter, r *http.Request) {
	a, err := db.GetArea(areaIDFrom(r))
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	detail, err := areaDetailData(a, "")
	if err != nil {
		http.Error(w, "Failed to load area", http.StatusInternalServerError)
		return
	}
	data := railData()
	for k, v := range detail {
		data[k] = v
	}
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-areas"
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	templates.ExecuteTemplate(w, "admin_page_area_detail.html", data)
}

func handleAdminAddAreaRule(w http.ResponseWriter, r *http.Request) {
	a, err := db.GetArea(areaIDFrom(r))
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	rule, err := parseRuleForm(r)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		renderAreaDetail(w, r, a, err.Error())
		return
	}
	if err := db.AddAreaRule(a.ID, rule); err != nil {
		log.Printf("[ADMIN] Failed to add rule: %v", err)
	}
	rulesChanged()
	renderAreaDetail(w, r, a, "")
}

func handleAdminDeleteAreaRule(w http.ResponseWriter, r *http.Request) {
	a, err := db.GetArea(areaIDFrom(r))
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	rid, _ := strconv.ParseInt(mux.Vars(r)["rid"], 10, 64)
	if err := db.DeleteAreaRule(a.ID, rid); err != nil {
		log.Printf("[ADMIN] Failed to delete rule: %v", err)
	}
	rulesChanged()
	renderAreaDetail(w, r, a, "")
}

// handleAdminADSuggest: suggerimenti dal vivo per il campo target.
func handleAdminADSuggest(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{"Target": r.URL.Query().Get("target")}
	sugg, err := ldap.Suggest(cfg, r.URL.Query().Get("kind"), r.URL.Query().Get("attr"), r.URL.Query().Get("q"))
	switch {
	case errors.Is(err, ldap.ErrInvalidAttr):
		w.WriteHeader(http.StatusUnprocessableEntity)
		data["Error"] = "Nome di attributo LDAP non valido"
	case err != nil:
		log.Printf("[ADMIN] Suggerimenti AD: %v", err)
		data["Error"] = "AD non disponibile"
	default:
		data["Suggestions"] = sugg
	}
	templates.ExecuteTemplate(w, "admin_ad_suggestions.html", data)
}
```

Rotte in `main.go`, dopo quelle del Task 7:

```go
	admin.HandleFunc("/areas/{id}", handleAdminAreaDetail).Methods("GET")
	admin.HandleFunc("/areas/{id}/rules", handleAdminAddAreaRule).Methods("POST")
	admin.HandleFunc("/areas/{id}/rules/{rid}/delete", handleAdminDeleteAreaRule).Methods("POST")
	admin.HandleFunc("/ad/suggest", handleAdminADSuggest).Methods("GET")
```

- [ ] **Step 4: Template**

`web/templates/admin_rules.html`:

```html
{{if .Error}}<p class="form-error" role="alert">{{.Error}}</p>{{end}}
{{if .Rules}}
<ul class="rule-list">
    {{$b := .}}
    {{range .Rules}}
    <li>
        <span>{{.Label}}</span>
        <button hx-post="{{$b.DeleteURLPrefix}}{{.ID}}/delete" hx-target="{{$b.Target}}" hx-swap="innerHTML" class="btn btn-ghost">Togli</button>
    </li>
    {{end}}
</ul>
{{else}}
<p class="helptext">Nessuna regola: nessun contatto ne fa parte.</p>
{{end}}

<form hx-post="{{.PostURL}}" hx-target="{{.Target}}" hx-swap="innerHTML" class="rule-form field-row" style="flex-wrap:wrap;">
    <select name="kind" class="input" id="{{.FormID}}-kind" style="flex:none;">
        <option value="ou">OU</option>
        <option value="adgroup">Gruppo AD</option>
        <option value="attr">Attributo = valore</option>
        {{if .AllowRange}}<option value="range">Interni (da-a)</option>{{end}}
        <option value="user">Utente</option>
        <option value="exclude">Escludi utente</option>
        <option value="present">Richiede attributo compilato</option>
        <option value="absent">Richiede attributo vuoto</option>
    </select>
    <input type="text" name="attr" id="{{.FormID}}-attr" class="input" placeholder="attributo (es. description)" autocomplete="off"
           hx-get="/admin/ad/suggest" hx-trigger="keyup changed delay:400ms, focus"
           hx-vals='js:{kind:"attrname", q:document.getElementById("{{.FormID}}-attr").value, target:"{{.FormID}}-attr"}'
           hx-target="#{{.FormID}}-attr-sugg" hx-swap="innerHTML">
    <input type="text" name="value" id="{{.FormID}}-value" class="input" placeholder="valore, OU, gruppo, utente o 700-799" autocomplete="off"
           hx-get="/admin/ad/suggest" hx-trigger="keyup changed delay:400ms"
           hx-vals='js:{kind:document.getElementById("{{.FormID}}-kind").value, attr:document.getElementById("{{.FormID}}-attr").value, q:document.getElementById("{{.FormID}}-value").value, target:"{{.FormID}}-value"}'
           hx-target="#{{.FormID}}-value-sugg" hx-swap="innerHTML">
    <button type="submit" class="btn btn-primary" style="flex:none;">Aggiungi regola</button>
    <div id="{{.FormID}}-attr-sugg" class="suggest-list"></div>
    <div id="{{.FormID}}-value-sugg" class="suggest-list"></div>
</form>
```

`web/templates/admin_ad_suggestions.html`:

```html
{{if .Error}}<div class="helptext">{{.Error}}</div>{{end}}
{{$t := .Target}}
{{range .Suggestions}}
<button type="button" class="suggest-item" data-target="{{$t}}" data-v="{{.Value}}"
        onclick="var i=document.getElementById(this.dataset.target); i.value=this.dataset.v; this.parentElement.innerHTML='';">{{.Label}}</button>
{{end}}
```

`web/templates/admin_area_detail.html`:

```html
<div class="section-head"><h3>Regole</h3></div>
{{if .Parent}}<p class="helptext">Sotto-area di <a href="/admin/areas/{{.Parent.ID}}">{{.Parent.Name}}</a>: le regole valgono solo per i contatti di quell'area.</p>{{end}}
{{template "admin_rules.html" .RulesBlock}}

<div class="section-head" style="margin-top:20px;"><h3>Membri attuali ({{.MembersTotal}})</h3></div>
<p class="helptext">Aggiornati all'ultimo sync: dopo una modifica alle regole ricaricare la pagina a sync concluso.</p>
<ul class="member-list">
    {{range .Members}}<li>{{.DisplayName}}</li>{{else}}<li class="helptext">Nessun membro.</li>{{end}}
    {{if gt .MembersTotal (len .Members)}}<li class="helptext">… e altri {{.MembersTotal}} in totale</li>{{end}}
</ul>

{{if .HasSubAreas}}
<div class="section-head" style="margin-top:20px;"><h3>Senza sottogruppo ({{.WithoutSubTotal}})</h3></div>
<p class="helptext">Contatti di quest'area che non rientrano in nessuna sotto-area: aggiungili a un gruppo AD o completa le regole.</p>
<ul class="member-list">
    {{range .WithoutSub}}<li>{{.DisplayName}}</li>{{else}}<li class="helptext">Nessuno.</li>{{end}}
</ul>
{{end}}
```

Nota: `gt .MembersTotal (len .Members)` confronta due `int`, nessun problema di tipo (vedi gotcha `eq` con puntatori in CLAUDE.md).

`web/templates/admin_page_area_detail.html` (stessa struttura di `admin_page_areas.html`):

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Area.Name}} - Aree - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        {{template "rail.html" .}}
        <main class="main" style="max-width:920px;">
            <p class="helptext"><a href="/admin/areas">&larr; Aree</a></p>
            <h1 class="page-title">{{.Area.Name}} <span class="helptext">({{.Area.Key}})</span></h1>
            <div id="area-detail-content">{{template "admin_area_detail.html" .}}</div>
        </main>
    </div>
</body>
</html>
```

CSS in fondo a `style.css`:

```css
.rule-list { list-style: none; padding: 0; margin: 8px 0 12px; }
.rule-list li { display: flex; align-items: center; justify-content: space-between; padding: 6px 0; border-bottom: 1px solid var(--line); }
.rule-form { position: relative; }
.suggest-list { flex-basis: 100%; display: flex; flex-direction: column; }
.suggest-item { text-align: left; background: none; border: none; border-bottom: 1px solid var(--line); padding: 5px 8px; cursor: pointer; font: inherit; color: var(--ink); }
.suggest-item:hover { background: var(--accent-tint); }
.member-list { columns: 2; padding-left: 18px; margin: 6px 0; }
.form-error { color: var(--danger, #b42318); margin: 6px 0; }
```

- [ ] **Step 5: Verifica**

Run: `DTEST ./cmd/server/ ''`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/server web
git commit -m "feat(admin): dettaglio area con regole, membri e suggerimenti da AD"
```

---

### Task 9: Admin — perimetro della rubrica con anteprima e conferma

**Files:**
- Modify: `cmd/server/admin_areas.go` (sostituire lo stub `scopeData`), `cmd/server/main.go` (rotte)
- Modify: `web/templates/admin_scope.html`
- Create: `web/templates/admin_scope_preview.html`
- Test: `cmd/server/templates_test.go`

**Interfaces:**
- Consumes: `db.ListScopeRules`, `db.AddScopeRule`, `db.DeleteScopeRule`; `ldap.PreviewScope`, `ldap.ScopePreview`; `getLastSyncReport()`, `startLDAPSync(force)`; `rulesBlock`, `ruleViews`, `parseRuleForm`.
- Produces: rotte `POST /admin/scope/rules`, `POST /admin/scope/rules/{rid}/delete`, `GET /admin/scope/preview`, `POST /admin/scope/apply` (`force=1` = conferma oltre soglia).

- [ ] **Step 1: Scrivi i test che falliscono**

Aggiungere a `cmd/server/templates_test.go`:

```go
func TestAdminScopeRendersHeldWarning(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_scope.html", "../../web/templates/admin_rules.html"))
	data := map[string]interface{}{
		"ScopeRules": rulesBlock{PostURL: "/admin/scope/rules", DeleteURLPrefix: "/admin/scope/rules/", Target: "#scope-content", FormID: "scope"},
		"StaleHeld":  42,
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_scope.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"42 contatti", `hx-vals='{"force":"1"}'`, `hx-get="/admin/scope/preview"`, "Nessuna regola = tutti"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q", want)
		}
	}
	if strings.Contains(html, `value="range"`) {
		t.Error("il perimetro non deve offrire regole range")
	}
}

func TestAdminScopePreviewRenders(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_scope_preview.html"))
	data := map[string]interface{}{"Preview": ldap.ScopePreview{In: 250, Current: 248, Entering: []string{"Nuovo"}, Leaving: []string{"Uscente"}, MoreLeaving: 3}}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_scope_preview.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"250", "248", "Nuovo", "Uscente", "altri 3"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("manca %q", want)
		}
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./cmd/server/ 'Scope'`
Expected: FAIL (template incompleti).

- [ ] **Step 3: Handler**

In `cmd/server/admin_areas.go` sostituire lo stub `scopeData` con:

```go
// scopeData: dati della sezione Perimetro.
func scopeData() (map[string]interface{}, error) {
	rules, err := db.ListScopeRules()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"ScopeRules": rulesBlock{
			Rules:           ruleViews(rules),
			PostURL:         "/admin/scope/rules",
			DeleteURLPrefix: "/admin/scope/rules/",
			Target:          "#scope-content",
			FormID:          "scope",
		},
		"StaleHeld": getLastSyncReport().StaleHeld,
	}, nil
}

func renderScope(w http.ResponseWriter, r *http.Request, ruleErr string) {
	data, err := scopeData()
	if err != nil {
		http.Error(w, "Failed to load scope", http.StatusInternalServerError)
		return
	}
	if ruleErr != "" {
		b := data["ScopeRules"].(rulesBlock)
		b.Error = ruleErr
		data["ScopeRules"] = b
	}
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	templates.ExecuteTemplate(w, "admin_scope.html", data)
}

// Le modifiche al perimetro NON avviano il sync: prima si guarda
// l'anteprima, poi "Applica ora" (o il sync orario).
func handleAdminAddScopeRule(w http.ResponseWriter, r *http.Request) {
	rule, err := parseRuleForm(r)
	if err == nil && rule.Kind == arearules.KindRange {
		err = fmt.Errorf("%w: il perimetro vale solo per gli utenti AD, i range di interni non si applicano", arearules.ErrInvalidRule)
	}
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		renderScope(w, r, err.Error())
		return
	}
	if err := db.AddScopeRule(rule); err != nil {
		log.Printf("[ADMIN] Failed to add scope rule: %v", err)
	}
	renderScope(w, r, "")
}

func handleAdminDeleteScopeRule(w http.ResponseWriter, r *http.Request) {
	rid, _ := strconv.ParseInt(mux.Vars(r)["rid"], 10, 64)
	if err := db.DeleteScopeRule(rid); err != nil {
		log.Printf("[ADMIN] Failed to delete scope rule: %v", err)
	}
	renderScope(w, r, "")
}

func handleAdminScopePreview(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{}
	p, err := ldap.PreviewScope(db, cfg)
	if err != nil {
		log.Printf("[ADMIN] Anteprima perimetro: %v", err)
		data["Error"] = "AD non disponibile: anteprima non calcolabile."
	} else {
		data["Preview"] = p
	}
	templates.ExecuteTemplate(w, "admin_scope_preview.html", data)
}

func handleAdminScopeApply(w http.ResponseWriter, r *http.Request) {
	startLDAPSync(r.FormValue("force") == "1")
	renderSyncStatus(w, r)
}
```

Rotte in `main.go`:

```go
	admin.HandleFunc("/scope/rules", handleAdminAddScopeRule).Methods("POST")
	admin.HandleFunc("/scope/rules/{rid}/delete", handleAdminDeleteScopeRule).Methods("POST")
	admin.HandleFunc("/scope/preview", handleAdminScopePreview).Methods("GET")
	admin.HandleFunc("/scope/apply", handleAdminScopeApply).Methods("POST")
```

Verificare con `grep -n "sync_status.html\|hx-get=\"/admin/sync/status\"" web/templates/*.html` come il frammento di stato fa polling: `renderSyncStatus` restituisce `sync_status.html`, che continua a interrogare `/admin/sync/status` finché il sync è in corso — riutilizzato tale e quale.

- [ ] **Step 4: Template**

`web/templates/admin_scope.html`:

```html
<div class="section-head"><h3>Perimetro della rubrica</h3></div>
<p class="helptext" style="margin:8px 0 12px;">Solo gli utenti AD che soddisfano queste regole entrano in rubrica. Nessuna regola = tutti gli utenti AD. Gli account disattivati entrano comunque (servono alla diagnostica del centralino) ma non sono mai pubblici. Le modifiche si applicano al prossimo sync orario, oppure subito con «Applica ora».</p>

{{if .StaleHeld}}
<div class="alert-warning" role="alert">
    <strong>Calo anomalo:</strong> all'ultimo sync {{.StaleHeld}} contatti sarebbero usciti dalla rubrica. La cancellazione è sospesa.
    Controlla le regole e l'anteprima, poi
    <button hx-post="/admin/scope/apply" hx-vals='{"force":"1"}' hx-target="#scope-sync-status" hx-swap="innerHTML"
            hx-confirm="Confermi? I contatti fuori perimetro verranno tolti dalla rubrica." class="btn-danger">Conferma e applica</button>
</div>
{{end}}

{{template "admin_rules.html" .ScopeRules}}

<div class="field-row" style="margin-top:10px;">
    <button hx-get="/admin/scope/preview" hx-target="#scope-preview" hx-swap="innerHTML" class="btn btn-ghost">Anteprima</button>
    <button hx-post="/admin/scope/apply" hx-target="#scope-sync-status" hx-swap="innerHTML" class="btn btn-primary">Applica ora</button>
</div>
<div id="scope-preview" hx-get="/admin/scope/preview" hx-trigger="load" hx-swap="innerHTML"></div>
<div id="scope-sync-status"></div>
```

`admin_rules.html` viene reso con `AllowRange` falso (zero value di `rulesBlock` in `scopeData`), quindi l'opzione `range` non compare: è ciò che verifica il test.

`web/templates/admin_scope_preview.html`:

```html
{{if .Error}}<p class="helptext">{{.Error}}</p>{{end}}
{{with .Preview}}
<div class="scope-preview">
    <p>Con queste regole entrano <strong>{{.In}}</strong> utenti attivi (oggi in rubrica: <strong>{{.Current}}</strong>).</p>
    {{if .Entering}}
    <p class="helptext">Entrerebbero:</p>
    <ul class="member-list">{{range .Entering}}<li>{{.}}</li>{{end}}{{if .MoreEntering}}<li class="helptext">… e altri {{.MoreEntering}}</li>{{end}}</ul>
    {{end}}
    {{if .Leaving}}
    <p class="helptext">Uscirebbero:</p>
    <ul class="member-list">{{range .Leaving}}<li>{{.}}</li>{{end}}{{if .MoreLeaving}}<li class="helptext">… e altri {{.MoreLeaving}}</li>{{end}}</ul>
    {{end}}
</div>
{{end}}
```

CSS in fondo a `style.css`:

```css
.alert-warning { border: 1px solid #f0b429; background: #fff8e6; border-radius: 6px; padding: 10px 12px; margin: 8px 0 12px; }
.scope-preview { margin-top: 10px; }
```

- [ ] **Step 5: Verifica**

Run: `DTEST ./cmd/server/ ''`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/server web
git commit -m "feat(admin): perimetro della rubrica con anteprima e conferma del calo"
```

---

### Task 10: Rubrica pubblica — rail annidata, filtro sotto-area, tag

**Files:**
- Modify: `internal/phonebook/service.go` (`ContactWithGroups`, nuovo `AttachSubAreas`)
- Modify: `cmd/server/main.go` (`railData` riga 432, `handleIndex` riga 457, `handleSearch` riga 471)
- Modify: `web/templates/rail.html`, `web/templates/phonebook.html:41`, `web/templates/search_results.html`
- Modify: `web/static/css/style.css`
- Test: `internal/phonebook/service_test.go`, `cmd/server/templates_test.go`, `cmd/server/admin_areas_test.go`

**Interfaces:**
- Consumes: `db.ListSubAreasForContacts`, `db.CountBySubArea`, `db.ListAreas`, `database.BuildAreaNodes`.
- Produces:
  - `type SubAreaTag struct{ Key, Name string }`; campo `ContactWithGroups.SubAreas []SubAreaTag`
  - `func (s *Service) AttachSubAreas(results []*ContactWithGroups, names map[string]string) error`
  - `func filterBySubArea(results []*phonebook.ContactWithGroups, sub string) []*phonebook.ContactWithGroups` (in `main`)
  - dati rail: `AreaNodes`, `SubAreaCounts`, `ActiveSub`

- [ ] **Step 1: Scrivi i test che falliscono**

In `internal/phonebook/service_test.go` (usa l'helper DB già presente; verificare il nome con `grep -n "func new" internal/phonebook/service_test.go`):

```go
func TestAttachSubAreas(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateArea(&database.Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}); err != nil {
		t.Fatal(err)
	}
	c := &database.Contact{UID: "u", DisplayName: "U", Email: "u@x.it", Area: "interni", LastSync: time.Now()}
	db.UpsertContact(c)
	id, _, _ := db.ContactIDAndArea("u")
	db.SetContactSubAreas(id, []string{"dirigenti"})
	got, _ := db.GetContact("u")

	s := NewService(db)
	results := []*ContactWithGroups{{Contact: got}}
	if err := s.AttachSubAreas(results, map[string]string{"dirigenti": "Dirigenti"}); err != nil {
		t.Fatal(err)
	}
	if len(results[0].SubAreas) != 1 || results[0].SubAreas[0] != (SubAreaTag{Key: "dirigenti", Name: "Dirigenti"}) {
		t.Errorf("SubAreas = %+v", results[0].SubAreas)
	}
}
```

In `cmd/server/admin_areas_test.go`:

```go
func TestFilterBySubArea(t *testing.T) {
	results := []*phonebook.ContactWithGroups{
		{Contact: &database.Contact{UID: "a"}, SubAreas: []phonebook.SubAreaTag{{Key: "dirigenti"}, {Key: "pc"}}},
		{Contact: &database.Contact{UID: "b"}},
	}
	if got := filterBySubArea(results, "pc"); len(got) != 1 || got[0].Contact.UID != "a" {
		t.Errorf("pc = %+v", got)
	}
	if got := filterBySubArea(results, "inesistente"); len(got) != 0 {
		t.Errorf("sotto-area inesistente = %d risultati, want 0", len(got))
	}
	if got := filterBySubArea(results, ""); len(got) != 2 {
		t.Errorf("senza filtro = %d", len(got))
	}
}
```

(import `phonebook` e `database` nel file di test.)

In `cmd/server/templates_test.go`:

```go
func TestSearchResultsShowsSubAreaTags(t *testing.T) {
	funcs := template.FuncMap{
		"initials": func(string) string { return "XX" }, "sentenceCase": func(s string) string { return s },
		"extList": func(s string) string { return s },
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFiles(
		"../../web/templates/search_results.html", "../../web/templates/group_category_node.html", "../../web/templates/group_row.html"))
	cw := &phonebook.ContactWithGroups{
		Contact:  &database.Contact{UID: "a", DisplayName: "Anna"},
		SubAreas: []phonebook.SubAreaTag{{Key: "dirigenti", Name: "Dirigente"}, {Key: "pc", Name: "Protezione civile"}},
	}
	data := map[string]interface{}{
		"Groups":   []*phonebook.DepartmentGroup{{Name: "Ragioneria", Contacts: []*phonebook.ContactWithGroups{cw}}},
		"Messages": map[string]string{"no_results": "Nessun risultato"},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "search_results.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Count(buf.String(), `class="sub-tag"`) != 2 || !strings.Contains(buf.String(), "Protezione civile") {
		t.Errorf("tag mancanti: %s", buf.String())
	}
}
```

Verificare con `grep -n "type .* struct" internal/phonebook/service.go` il nome reale del tipo restituito da `GroupByDepartment` (qui ipotizzato `DepartmentGroup` con campi `Name`, `Contacts`) e i template func realmente usati da `search_results.html` e dai template inclusi (`grep -o '{{[a-zA-Z]* ' web/templates/search_results.html web/templates/group_row.html web/templates/group_category_node.html | sort -u`); adeguare `funcs` e i nomi nel test.

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./internal/phonebook/ 'AttachSubAreas'` e `DTEST ./cmd/server/ 'FilterBySubArea|SubAreaTags'`
Expected: FAIL (`undefined: SubAreaTag`, `filterBySubArea`).

- [ ] **Step 3: Implementa phonebook**

In `internal/phonebook/service.go`:

```go
// SubAreaTag: sotto-area mostrata come etichetta sulla riga contatto.
type SubAreaTag struct {
	Key  string
	Name string
}

// ContactWithGroups represents a contact with its associated groups
type ContactWithGroups struct {
	Contact  *database.Contact
	Groups   []*database.GroupNumber
	SubAreas []SubAreaTag
}

// AttachSubAreas carica con una sola query le sotto-aree di tutti i
// risultati; names: key → nome visualizzato.
func (s *Service) AttachSubAreas(results []*ContactWithGroups, names map[string]string) error {
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.Contact.ID)
	}
	byID, err := s.db.ListSubAreasForContacts(ids)
	if err != nil {
		return err
	}
	for _, r := range results {
		for _, k := range byID[r.Contact.ID] {
			name := names[k]
			if name == "" {
				name = k
			}
			r.SubAreas = append(r.SubAreas, SubAreaTag{Key: k, Name: name})
		}
	}
	return nil
}
```

- [ ] **Step 4: Implementa main**

In `cmd/server/admin_areas.go` aggiungere:

```go
// filterBySubArea tiene i risultati che hanno la sotto-area sub ("" = tutti).
func filterBySubArea(results []*phonebook.ContactWithGroups, sub string) []*phonebook.ContactWithGroups {
	if sub == "" {
		return results
	}
	out := make([]*phonebook.ContactWithGroups, 0, len(results))
	for _, r := range results {
		for _, t := range r.SubAreas {
			if t.Key == sub {
				out = append(out, r)
				break
			}
		}
	}
	return out
}
```

(import `phonebook`.)

In `main.go`:
- `railData()`: dopo `areas, err := db.ListAreas()` aggiungere:

```go
	subCounts, err := db.CountBySubArea()
	if err != nil {
		log.Printf("[RAIL] Failed to count by sub-area: %v", err)
		subCounts = map[string]int{}
	}
```

e nella mappa restituita aggiungere `"AreaNodes": database.BuildAreaNodes(areas)` e `"SubAreaCounts": subCounts` (lasciare `"Areas": areas` per gli altri usi).

  Attenzione: `CountByArea` conta per area **salvata**, quindi `Total` non cambia.
- `handleIndex`: aggiungere `activeSub := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sub")))`, `data["ActiveSub"] = activeSub`, `data["InitialSub"] = activeSub`.
- `handleSearch`: dopo `groupFilter` leggere `subFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sub")))`; subito dopo il blocco `if err != nil {...}` della ricerca:

```go
	names := map[string]string{}
	if areas, err := db.ListAreas(); err == nil {
		for _, a := range areas {
			names[a.Key] = a.Name
		}
	}
	if err := pbService.AttachSubAreas(results, names); err != nil {
		log.Printf("[SEARCH] Failed to load sub-areas: %v", err)
	}
	results = filterBySubArea(results, subFilter)
```

  e cambiare la condizione dei gruppi di chiamata in `if subFilter == "" && (groupFilter == "" || groupFilter == "uffici") {`.

- [ ] **Step 5: Template**

`web/templates/phonebook.html:41`: sostituire l'attributo `hx-get` con
`hx-get="/search{{if .InitialGroup}}?group={{.InitialGroup}}{{if .InitialSub}}&sub={{.InitialSub}}{{end}}{{end}}"`.

`web/templates/rail.html`: nel ramo `{{if eq .Section "contacts"}}` sostituire il `{{range .Areas}}...{{end}}` dei bottoni con:

```html
    {{$active := .ActiveArea}}{{$activeSub := .ActiveSub}}{{$sc := .SubAreaCounts}}
    {{range .AreaNodes}}
    {{$area := .}}
    <button class="rail-item{{if and (eq $active .Key) (not $activeSub)}} active{{end}}" data-area="{{.Key}}"
            hx-get="/search?group={{.Key}}" hx-target="#search-results"
            onclick="if(window.setActiveArea)setActiveArea(this)">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M20.6 9.6a2 2 0 010 2.8l-8.2 8.2a2 2 0 01-2.8 0L3 14V3h11l6.6 6.6z"/><circle cx="7.5" cy="7.5" r="1.2" fill="currentColor" stroke="none"/></svg>
        <span>{{.Name}}</span><span class="n">{{index $.AreaCounts .Key}}</span>
    </button>
    {{range .Children}}
    <button class="rail-item rail-sub{{if eq $activeSub .Key}} active{{end}}" data-area="{{$area.Key}}" data-sub="{{.Key}}"
            hx-get="/search?group={{$area.Key}}&sub={{.Key}}" hx-target="#search-results"
            onclick="if(window.setActiveArea)setActiveArea(this)">
        <span>{{.Name}}</span><span class="n">{{index $sc .Key}}</span>
    </button>
    {{end}}
    {{end}}
```

e nel ramo `{{else}}` sostituire il `{{range .Areas}}...{{end}}` dei link con:

```html
    {{$sc := .SubAreaCounts}}
    {{range .AreaNodes}}
    {{$area := .}}
    <a href="/?group={{.Key}}" class="rail-item">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M20.6 9.6a2 2 0 010 2.8l-8.2 8.2a2 2 0 01-2.8 0L3 14V3h11l6.6 6.6z"/><circle cx="7.5" cy="7.5" r="1.2" fill="currentColor" stroke="none"/></svg>
        <span>{{.Name}}</span><span class="n">{{index $.AreaCounts .Key}}</span>
    </a>
    {{range .Children}}
    <a href="/?group={{$area.Key}}&sub={{.Key}}" class="rail-item rail-sub"><span>{{.Name}}</span><span class="n">{{index $sc .Key}}</span></a>
    {{end}}
    {{end}}
```

Nota: dentro `{{range .AreaNodes}}` il `.` è un `*AreaNode`; `.Key`/`.Name` sono promossi dall'`*Area` embedded. `$area` è catturato **prima** del range annidato (gotcha in CLAUDE.md). Verificare con `grep -rn "\.Areas" web/templates/rail.html` che non restino riferimenti a `.Areas`; le pagine che sovrascrivono `data["Areas"]` non toccano più la rail.

`web/templates/search_results.html`: dentro `<div class="name">`, dopo `{{.Contact.DisplayName}}`, aggiungere:

```html
                {{range .SubAreas}}<span class="sub-tag">{{.Name}}</span>{{end}}
```

CSS in fondo a `style.css`:

```css
.rail-item.rail-sub { padding-left: 38px; font-size: 13.5px; }
.sub-tag { display: inline-block; margin-left: 6px; font-size: 11.5px; font-weight: 500; color: var(--accent); background: var(--accent-tint); border-radius: 4px; padding: 1px 6px; vertical-align: 1px; }
@media (max-width: 760px) { .rail-item.rail-sub { padding-left: 12px; } }
```

(Il valore 760px va allineato al breakpoint già usato alla riga ~430 di `style.css`: verificare con `grep -n "@media" web/static/css/style.css`.)

- [ ] **Step 6: Verifica**

Run: `DTEST ./internal/phonebook/ ''` e `DTEST ./cmd/server/ ''`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/phonebook cmd/server web
git commit -m "feat(rubrica): sotto-aree nella barra laterale, filtro e tag sulla riga contatto"
```

---

### Task 11: Contatti manuali — scelta delle sotto-aree

**Files:**
- Modify: `cmd/server/main.go` (`renderAdminContacts`, `handleAdminContacts`, `handleAdminCreateContact`, `handleAdminUpdateContact`, righe ~1505-1590)
- Modify: `web/templates/admin_contacts.html`
- Test: `cmd/server/templates_test.go`, `cmd/server/admin_areas_test.go`

**Interfaces:**
- Consumes: `database.BuildAreaNodes`, `db.ListSubAreasForContacts`, `db.SetContactSubAreas`, `db.ContactIDAndArea`.
- Produces: `func manualSubAreas(r *http.Request, area string, nodes []*database.AreaNode) []string` (tiene solo le sotto-aree dell'area scelta); dati template `AreaNodes`, `ContactSubAreas map[int64]map[string]bool`.

- [ ] **Step 1: Scrivi i test che falliscono**

In `cmd/server/admin_areas_test.go`:

```go
func TestManualSubAreasKeepsOnlyChosenArea(t *testing.T) {
	nodes := []*database.AreaNode{
		{Area: &database.Area{Key: "interni"}, Children: []*database.Area{{Key: "dirigenti", ParentKey: "interni"}}},
		{Area: &database.Area{Key: "esterni"}, Children: []*database.Area{{Key: "fornitori", ParentKey: "esterni"}}},
	}
	form := url.Values{"sub_areas": {"dirigenti", "fornitori", "fantasma"}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := manualSubAreas(r, "interni", nodes)
	if len(got) != 1 || got[0] != "dirigenti" {
		t.Errorf("manualSubAreas = %v, want [dirigenti]", got)
	}
}
```

In `cmd/server/templates_test.go`:

```go
func TestAdminContactsShowsSubAreaCheckboxes(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_contacts.html"))
	c := &database.Contact{ID: 5, UID: "manual-x", DisplayName: "X", Area: "interni"}
	data := map[string]interface{}{
		"Messages":        map[string]string{"save": "Salva", "delete": "Elimina"},
		"Contacts":        []*database.Contact{c},
		"Areas":           []*database.Area{{Key: "interni", Name: "Personale comunale"}},
		"AreaNodes":       []*database.AreaNode{{Area: &database.Area{Key: "interni", Name: "Personale comunale"}, Children: []*database.Area{{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}}}},
		"ContactSubAreas": map[int64]map[string]bool{5: {"dirigenti": true}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_contacts.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !regexp.MustCompile(`name="sub_areas" value="dirigenti"[^>]*checked`).MatchString(buf.String()) {
		t.Errorf("checkbox dirigenti non spuntata: %s", buf.String())
	}
}
```

- [ ] **Step 2: Verifica che falliscano**

Run: `DTEST ./cmd/server/ 'ManualSubAreas|SubAreaCheckboxes'`
Expected: FAIL.

- [ ] **Step 3: Implementa**

In `cmd/server/admin_areas.go`:

```go
// manualSubAreas: sotto-aree spuntate nel form di un contatto manuale,
// ristrette a quelle dell'area scelta.
func manualSubAreas(r *http.Request, area string, nodes []*database.AreaNode) []string {
	valid := map[string]bool{}
	for _, n := range nodes {
		if n.Key != area {
			continue
		}
		for _, c := range n.Children {
			valid[c.Key] = true
		}
	}
	r.ParseForm()
	var out []string
	for _, k := range r.Form["sub_areas"] {
		if valid[k] {
			out = append(out, k)
		}
	}
	return out
}

// manualContactsData: aree, albero e sotto-aree attuali per la pagina
// "Contatti locali".
func manualContactsData(contacts []*database.Contact) map[string]interface{} {
	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[ADMIN] Failed to list areas: %v", err)
	}
	var top []*database.Area
	for _, a := range areas {
		if a.ParentKey == "" {
			top = append(top, a)
		}
	}
	ids := make([]int64, 0, len(contacts))
	for _, c := range contacts {
		ids = append(ids, c.ID)
	}
	selected := map[int64]map[string]bool{}
	if byID, err := db.ListSubAreasForContacts(ids); err == nil {
		for id, keys := range byID {
			selected[id] = map[string]bool{}
			for _, k := range keys {
				selected[id][k] = true
			}
		}
	}
	return map[string]interface{}{
		"Areas":           top,
		"AreaNodes":       database.BuildAreaNodes(areas),
		"ContactSubAreas": selected,
	}
}

// saveManualSubAreas salva le sotto-aree di un contatto manuale appena
// creato o modificato.
func saveManualSubAreas(r *http.Request, uid, area string) {
	areas, err := db.ListAreas()
	if err != nil {
		return
	}
	id, _, err := db.ContactIDAndArea(uid)
	if err != nil || id == 0 {
		return
	}
	if err := db.SetContactSubAreas(id, manualSubAreas(r, area, database.BuildAreaNodes(areas))); err != nil {
		log.Printf("[ADMIN] Sotto-aree di %s non salvate: %v", uid, err)
	}
}
```

In `main.go`:
- `renderAdminContacts` e `handleAdminContacts`: sostituire la lettura di `areas` e la chiave `"Areas"` con il merge di `manualContactsData(contacts)` nella mappa dati.
- `handleAdminCreateContact`: dopo `db.CreateManualContact(c)` riuscita, `saveManualSubAreas(r, c.UID, c.Area)`.
- `handleAdminUpdateContact`: dopo `db.UpdateManualContact(c)` riuscita, `saveManualSubAreas(r, uid, c.Area)`.

`web/templates/admin_contacts.html`: dentro il form di ogni contatto, dopo il `</select>` dell'area, aggiungere:

```html
    {{$sel := index $.ContactSubAreas $c.ID}}
    {{range $.AreaNodes}}{{if eq .Key $c.Area}}{{range .Children}}
    <label class="toolbar-check" style="flex:none;"><input type="checkbox" name="sub_areas" value="{{.Key}}"{{if index $sel .Key}} checked{{end}}> {{.Name}}</label>
    {{end}}{{end}}{{end}}
```

e nel form "Nuovo contatto locale", dopo il `</select>`:

```html
        {{range .AreaNodes}}{{range .Children}}
        <label class="toolbar-check" style="flex:none;" title="Sotto-area: vale solo se il contatto è nell'area madre"><input type="checkbox" name="sub_areas" value="{{.Key}}"> {{.Name}}</label>
        {{end}}{{end}}
```

Le checkbox di sotto-aree non dell'area scelta vengono scartate da `manualSubAreas`; per un contatto esistente compaiono solo quelle della sua area (cambiando area, salvare due volte: prima l'area, poi le sotto-aree — accettabile per pochi contatti manuali, annotarlo nell'helptext della pagina: "Le sotto-aree disponibili dipendono dall'area salvata").

`index $sel .Key` su `$sel` nil (contatto senza sotto-aree) restituisce il valore zero `false`: nessun errore in `html/template`.

- [ ] **Step 4: Verifica**

Run: `DTEST ./cmd/server/ ''`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/server web
git commit -m "feat(admin): sotto-aree selezionabili sui contatti locali"
```

---

### Task 12: CardDAV per area e sotto-area, pulizia configurazione

**Files:**
- Modify: `internal/carddav/server.go` (righe 98-120, 184-195, 241-266, 282-345)
- Modify: `internal/config/config.go` (rimuovere `LDAPOUFilters`, `defaultOUFilters`, `getEnvMapList`; commento su `LDAPAllowedGroups`)
- Modify: `.env.example`, `docker-compose.yml`
- Test: `internal/carddav/server_test.go` (nuovo)

**Interfaces:**
- Consumes: `db.ListAllContacts`, `db.GetArea*`/`db.ListAreas`, `db.ContactIDsBySubArea`.
- Produces:
  - `func (s *Server) contactsForBook(book string, contacts []*database.Contact) ([]*database.Contact, bool, error)` — `ok=false` = rubrica inesistente (404)
  - `func (s *Server) contactInBook(c *database.Contact, book string) (bool, bool, error)` — (appartiene, rubrica esiste, err)
  - `func (s *Server) bookDisplayName(book string) string` — nome area dal DB

- [ ] **Step 1: Scrivi i test che falliscono**

`internal/carddav/server_test.go`:

```go
package carddav

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func setup(t *testing.T) (*Server, []*database.Contact) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.CreateArea(&database.Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"})
	for _, c := range []*database.Contact{
		{UID: "a", DisplayName: "A", Email: "a@x.it", Area: "interni", LastSync: time.Now()},
		{UID: "b", DisplayName: "B", Email: "b@x.it", Area: "esterni", LastSync: time.Now()},
	} {
		db.UpsertContact(c)
	}
	id, _, _ := db.ContactIDAndArea("a")
	db.SetContactSubAreas(id, []string{"dirigenti"})
	all, _ := db.ListAllContacts(1000, 0)
	return NewServer(db, &config.Config{}), all
}

func uids(cs []*database.Contact) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.UID] = true
	}
	return m
}

func TestContactsForBook(t *testing.T) {
	s, all := setup(t)
	cases := []struct {
		book string
		ok   bool
		want []string
	}{
		{"all", true, []string{"a", "b"}},
		{"interni", true, []string{"a"}},
		{"dirigenti", true, []string{"a"}},
		{"esterni", true, []string{"b"}},
		{"fantasma", false, nil},
	}
	for _, c := range cases {
		got, ok, err := s.contactsForBook(c.book, all)
		if err != nil || ok != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.book, ok, err)
			continue
		}
		m := uids(got)
		if len(m) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.book, m, c.want)
		}
		for _, u := range c.want {
			if !m[u] {
				t.Errorf("%s: manca %s", c.book, u)
			}
		}
	}
	if name := s.bookDisplayName("dirigenti"); name != "Rubrica Dirigenti" {
		t.Errorf("bookDisplayName = %q", name)
	}
}
```

- [ ] **Step 2: Verifica che fallisca**

Run: `DTEST ./internal/carddav/ ''`
Expected: FAIL (`undefined: contactsForBook`).

- [ ] **Step 3: Implementa**

In `internal/carddav/server.go` sostituire `filterContactsByBook`, `contactMatchesBook` e `bookDisplayName` con:

```go
// resolveBook: "all" = tutta la rubrica; altrimenti key di un'area di primo
// livello o di una sotto-area. ok=false se la key non esiste.
func (s *Server) resolveBook(book string) (area *database.Area, ok bool, err error) {
	if book == "" || book == "all" {
		return nil, true, nil
	}
	a, err := s.db.GetAreaByKey(book)
	if err != nil || a == nil {
		return nil, false, err
	}
	return a, true, nil
}

func (s *Server) contactsForBook(book string, contacts []*database.Contact) ([]*database.Contact, bool, error) {
	a, ok, err := s.resolveBook(book)
	if err != nil || !ok || a == nil {
		return contacts, ok, err
	}
	var inSub map[int64]bool
	if a.ParentKey != "" {
		if inSub, err = s.db.ContactIDsBySubArea(a.Key); err != nil {
			return nil, true, err
		}
	}
	out := make([]*database.Contact, 0, len(contacts))
	for _, c := range contacts {
		if (a.ParentKey == "" && c.Area == a.Key) || (a.ParentKey != "" && inSub[c.ID]) {
			out = append(out, c)
		}
	}
	return out, true, nil
}

func (s *Server) contactInBook(c *database.Contact, book string) (bool, bool, error) {
	got, ok, err := s.contactsForBook(book, []*database.Contact{c})
	return len(got) == 1, ok, err
}

func (s *Server) bookDisplayName(book string) string {
	if a, ok, err := s.resolveBook(book); err == nil && ok && a != nil {
		return "Rubrica " + a.Name
	}
	return "Rubrica Contatti"
}
```

Nei tre handler:
- `handlePropFind` (riga ~110) e `handleReportWithVCards` (riga ~192): sostituire `contacts = s.filterContactsByBook(contacts, book)` con

```go
	contacts, ok, err := s.contactsForBook(book, contacts)
	if err != nil {
		log.Printf("[CARDDAV] Failed to filter book %s: %v", book, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
```

  (adeguare `:=`/`=` alle variabili già dichiarate nella funzione.)
- `handleGetVCard` (riga ~262): sostituire il blocco `if !s.contactMatchesBook(contact, book)` con

```go
	in, ok, err := s.contactInBook(contact, book)
	if err != nil || !ok || !in {
		log.Printf("[CARDDAV] Contact %s not in book %s", uid, book)
		http.NotFound(w, r)
		return
	}
```

In `internal/config/config.go`: rimuovere il campo `LDAPOUFilters`, la riga in `Load()`, `defaultOUFilters()` e `getEnvMapList` (verificare con `grep -rn getEnvMapList` che non abbiano altri usi). Sopra `LDAPAllowedGroups` aggiungere il commento:

```go
	// LDAPAllowedGroups è solo il seed una tantum delle regole di perimetro
	// (scope_rules, vedi internal/ldap/scope.go): letto al primo sync dopo
	// l'aggiornamento, poi ignorato. Il perimetro si gestisce da /admin/areas.
```

In `.env.example` (riga 79) sostituire il commento/variabile con:

```bash
# Seed una tantum: al primo sync i gruppi elencati diventano regole di
# perimetro in /admin/areas, poi la variabile è ignorata e si può togliere.
LDAP_ALLOWED_GROUPS=
```

e rimuovere ogni riga/commento su `LDAP_OU_FILTERS` (`grep -n OU_FILTERS .env.example docker-compose.yml`). In `docker-compose.yml` lasciare `LDAP_ALLOWED_GROUPS=${LDAP_ALLOWED_GROUPS}` con il commento `# seed una tantum del perimetro (vedi .env.example)` e rimuovere `LDAP_OU_FILTERS` se presente.

- [ ] **Step 4: Verifica**

Run: `DTEST ./... ''` poi `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go vet ./... && CGO_ENABLED=1 go build ./..."`
Expected: tutti i test PASS, `go vet` e build senza errori.

- [ ] **Step 5: Commit**

```bash
git add internal/carddav internal/config .env.example docker-compose.yml
git commit -m "feat(carddav): una rubrica per area e sotto-area, via LDAP_OU_FILTERS"
```

---

### Task 13: Verifica end-to-end e documentazione

**Files:**
- Modify: `CLAUDE.md`
- Modify: memoria `C:\Users\mirko.daddiego\.claude\projects\C--Users-mirko-daddiego-Documents-rubrica\memory\ldap-allowed-groups-ad.md` e `MEMORY.md`

**Interfaces:**
- Consumes: tutto quanto sopra.
- Produces: documentazione aggiornata; nessun codice.

- [ ] **Step 1: Prova contro AD reale**

Run: `docker compose up -d --build`, poi aprire `http://localhost:8080/admin/areas` (login admin).
Expected:
- Perimetro: una regola `Gruppo AD all_users-servizi_interni` (+ `all_users-area_politica` se trovato); log `[SYNC] Perimetro: gruppo "all_users-servizi_esterni" ... scartato`.
- Anteprima: "entrano N (oggi M)" con N ≈ M.
- Aree: Esterni, Interni, Politica, Uffici con regole `OU …`; conteggi uguali a prima del deploy (confrontare con la rail di produzione o con `SELECT area, COUNT(*) FROM contacts WHERE deleted_at IS NULL AND disabled=0 GROUP BY area` prima/dopo).
- Creare sotto-area "Dirigenti" sotto Interni con regola `description = Dirigente`: dopo il sync il dettaglio mostra 4 membri attivi; la rail pubblica mostra "Dirigenti 4"; i tag compaiono sulle righe; `/carddav/dirigenti/` restituisce 4 vCard (verificare con `curl -u <utente>:<pwd> -X PROPFIND -H "Depth: 1" http://localhost:8080/carddav/dirigenti/ | grep -c "<D:response>"` → 5 = collezione + 4).
- Aggiungere una regola di perimetro che escluda quasi tutti (es. `user = nessuno`), "Applica ora": avviso "Calo anomalo", nessun contatto cancellato; togliere la regola, "Applica ora": avviso sparisce.

- [ ] **Step 2: Aggiorna CLAUDE.md**

Sostituire i passaggi superati:
- bullet `internal/ldap`: `deriveArea`/OU mapping/`MatchExtensionRange` → "il sync valuta le regole di `internal/arearules` (area = prima area in ordine, sotto-aree = tutte quelle della sua area, salvate in `contact_sub_areas`); perimetro = `scope_rules` (seed una tantum da `LDAP_ALLOWED_GROUPS`); protezione svuotamento: soft-delete sospeso se escono ≥5 contatti e >20%, si conferma da `/admin/areas`".
- bullet "Areas can carry a numeric range" → regole `range` in `area_rules`.
- bullet "Areas are a DB-editable entity": aggiungere `parent_key`/`sort`, un livello di sotto-aree, `/admin/ou-mapping` rimosso, dettaglio `/admin/areas/{id}`, suggerimenti `/admin/ad/suggest`.
- bullet `internal/config`: `LDAP_OU_FILTERS` rimossa; `LDAP_ALLOWED_GROUPS` solo seed.
- bullet `internal/carddav`: `/carddav/<key>/` per area o sotto-area, key sconosciuta 404.
- bullet "Admin is multiple pages": togliere `/admin/ou-mapping`; citare `cmd/server/admin_areas.go` come eccezione alla regola "handler tutti in main.go".
- Gotcha nuovo: "dopo `UpsertContact` sul ramo UPDATE `LastInsertId` non è l'id del contatto: usare `db.ContactIDAndArea(uid)`".

- [ ] **Step 3: Aggiorna la memoria**

Riscrivere `ldap-allowed-groups-ad.md` (stesso `name`) indicando che dal 2026-10-14 il filtro d'ingresso è in `scope_rules` gestite da `/admin/areas` e `LDAP_ALLOWED_GROUPS` è solo seed; aggiornare la riga in `MEMORY.md`.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: aree a regole, perimetro da UI e CardDAV per area"
```
