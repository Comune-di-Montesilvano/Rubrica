# Rubrica telefoni via LDAP — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rubrica espone un server LDAPv3 read-only che i telefoni VoIP interrogano per ricerca per nome e lookup del chiamante per interno.

**Architecture:** Nuovo package `internal/ldapserver` basato su `github.com/jimlambrt/gldap`. A ogni search le entry vengono costruite in memoria da contatti e gruppi (stesso perimetro della rubrica pubblica), filtrate con un valutatore di filtri LDAP proprio (`go-ldap` `CompileFilter` → albero BER) e restituite. Credenziali e base DN vivono in `app_config`, editabili da `/admin/phone-directory`; la porta è l'env `LDAP_SERVER_PORT`.

**Tech Stack:** Go 1.25, `github.com/jimlambrt/gldap` v0.1.14 (server), `github.com/go-ldap/ldap/v3` v3.4.14 (già presente: compilazione filtri, parsing DN, client nei test), `github.com/go-asn1-ber/asn1-ber` (già presente come indiretta), `github.com/hashicorp/go-hclog` (logger richiesto da gldap), SQLite via `internal/database`.

**Spec:** `docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md`

## Global Constraints

- Build/test richiedono CGO (go-sqlite3). Su Windows senza gcc, **ogni** comando `go` di questo piano va eseguito nel container:
  `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 <COMANDO GO>"`
  Nel piano i comandi sono scritti come `go ...`: sostituirli in `<COMANDO GO>`.
- Chiavi `app_config`: `ldapsrv_base_dn` (default `dc=rubrica,dc=local`), `ldapsrv_bind_dn` (default `cn=telefoni,dc=rubrica,dc=local`), `ldapsrv_bind_password` (nessun default).
- Env `LDAP_SERVER_PORT`, default `3389`; valore `0` = server LDAP non avviato.
- Porta host in compose: `${LDAP_SERVER_PORT_HOST:-389}`.
- Perimetro: contatti non cancellati e `disabled=0` con almeno un interno; gruppi con almeno un membro attivo (`ActiveMembers()`), come la rubrica pubblica.
- `telephoneNumber` = solo interni (`LDAPExt`, separatore `;`), mai `PrimaryNumber`.
- Tetto risultati per search: 1000.
- Bind: solo simple bind con bind DN + password configurati; password non configurata → ogni bind rifiutato (`invalidCredentials`); search senza bind riuscito → `insufficientAccessRights`.
- Nessun TLS. Commenti nel codice in italiano, come nel resto del repo.
- Log con prefisso `[LDAPSRV]`.

## Review Focus

1. Nomi con accenti/maiuscole (AD spesso li ha tutti maiuscoli, es. `NICOLÒ`): la ricerca `cn=*nicolò*` deve trovarli → test in Task 1.
2. Apostrofo nel filtro (`D'Addiego`, arriva escapato `\27`): deve matchare → test in Task 1.
3. `LDAPExt` sporco (`"700; 701 ;"`): ogni interno pulito, nessun valore vuoto → test in Task 2.
4. Due telefoni collegati insieme, uno solo autenticato: l'altro non deve poter cercare (stato auth per connessione, non globale) → test in Task 4.
5. Base DN cambiato da admin mentre i telefoni usano ancora il vecchio: vecchia base → `noSuchObject`, nuova base funziona subito senza riavvio → test in Task 4.

---

## File Structure

- Create `internal/ldapserver/entry.go` — tipo `Entry` e lookup attributi case-insensitive.
- Create `internal/ldapserver/filter.go` — `ParseFilter`: filtro LDAP → predicato su `Entry`.
- Create `internal/ldapserver/build.go` — `BuildEntries`: contatti/gruppi → entry LDAP ordinate.
- Create `internal/ldapserver/dn.go` — escaping valori DN, confronto DN, scope.
- Create `internal/ldapserver/settings.go` — chiavi `app_config`, `LoadSettings`/`SaveSettings`.
- Create `internal/ldapserver/server.go` — server gldap, handler bind/search.
- Tests: `filter_test.go`, `build_test.go`, `dn_test.go`, `settings_test.go`, `server_test.go` nello stesso package.
- Modify `internal/config/config.go` — campo `LDAPServerPort`.
- Modify `cmd/server/main.go` — avvio server, handler pagina admin.
- Create `web/templates/admin_page_phone_directory.html`, `web/templates/admin_phone_directory.html`.
- Modify `web/templates/rail.html` — voce "Rubrica telefoni".
- Modify `docker-compose.yml`, `Dockerfile`, `.env.example`, `CLAUDE.md`, spec.

---

### Task 1: Entry e valutatore di filtri LDAP

**Files:**
- Create: `internal/ldapserver/entry.go`
- Create: `internal/ldapserver/filter.go`
- Test: `internal/ldapserver/filter_test.go`

**Interfaces:**
- Consumes: niente.
- Produces:
  - `type Entry struct { DN string; Attrs map[string][]string }`
  - `func (e *Entry) Get(name string) []string` — lookup case-insensitive sul nome attributo, `nil` se assente.
  - `type Filter func(e *Entry) bool`
  - `func ParseFilter(s string) (Filter, error)`

- [ ] **Step 1: Scrivere il test (fallisce)**

`internal/ldapserver/filter_test.go`:

```go
package ldapserver

import "testing"

func filterTestEntry() *Entry {
	return &Entry{
		DN: "uid=ndaddiego,ou=contatti,dc=rubrica,dc=local",
		Attrs: map[string][]string{
			"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
			"cn":              {"NICOLÒ D'ADDIEGO"},
			"sn":              {"D'ADDIEGO"},
			"telephoneNumber": {"700", "759"},
		},
	}
}

func TestParseFilter(t *testing.T) {
	cases := []struct {
		filter string
		want   bool
	}{
		{"(telephoneNumber=759)", true},
		{"(telephoneNumber=700)", true},
		{"(telephoneNumber=75)", false},
		{"(TELEPHONENUMBER=759)", true},
		{"(cn=*addiego*)", true},
		{"(cn=nicol*)", true},
		{"(cn=*nicolò*)", true},
		{"(sn=*d\\27add*)", true},
		{"(cn=nicolò d'addiego)", true},
		{"(cn=rossi)", false},
		{"(cn=*o*d*o)", true},
		{"(cn=n*z*)", false},
		{"(cn=*iego)", true},
		{"(cn=*nicol)", false},
		{"(mobile=759)", false},
		{"(|(telephoneNumber=759)(mobile=759))", true},
		{"(|(cn=*zzz*)(sn=*zzz*))", false},
		{"(&(objectClass=person)(cn=*dad*))", true},
		{"(&(objectClass=person)(cn=*zzz*))", false},
		{"(!(cn=*zzz*))", true},
		{"(!(cn=*dad*))", false},
		{"(objectClass=*)", true},
		{"(mail=*)", false},
		{"(telephoneNumber>=700)", false},
		{"(cn~=nicolo)", false},
	}
	e := filterTestEntry()
	for _, tc := range cases {
		f, err := ParseFilter(tc.filter)
		if err != nil {
			t.Fatalf("ParseFilter(%q) error: %v", tc.filter, err)
		}
		if got := f(e); got != tc.want {
			t.Errorf("filtro %q = %v, atteso %v", tc.filter, got, tc.want)
		}
	}
}

func TestParseFilterInvalid(t *testing.T) {
	if _, err := ParseFilter("(cn="); err == nil {
		t.Fatal("atteso errore per filtro malformato")
	}
}

func TestEntryGetCaseInsensitive(t *testing.T) {
	e := filterTestEntry()
	if got := e.Get("TelephoneNumber"); len(got) != 2 {
		t.Fatalf("Get(TelephoneNumber) = %v, attesi 2 valori", got)
	}
	if got := e.Get("mobile"); got != nil {
		t.Fatalf("Get(mobile) = %v, atteso nil", got)
	}
}
```

- [ ] **Step 2: Eseguire il test e verificare che fallisca**

Run: `go test ./internal/ldapserver/ -run 'TestParseFilter|TestEntryGet' -v`
Expected: FAIL, errore di compilazione `undefined: Entry` / `undefined: ParseFilter`.

- [ ] **Step 3: Implementare**

`internal/ldapserver/entry.go`:

```go
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
```

`internal/ldapserver/filter.go`:

```go
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
```

- [ ] **Step 4: Eseguire i test e verificare che passino**

Run: `go mod tidy && go test ./internal/ldapserver/ -v`
Expected: PASS (`go mod tidy` sposta `go-asn1-ber/asn1-ber` tra le dipendenze dirette).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/ldapserver/entry.go internal/ldapserver/filter.go internal/ldapserver/filter_test.go
git commit -m "feat(ldapserver): valutatore filtri LDAP su entry in memoria"
```

---

### Task 2: Costruzione entry da contatti e gruppi + helper DN

**Files:**
- Create: `internal/ldapserver/dn.go`
- Create: `internal/ldapserver/build.go`
- Test: `internal/ldapserver/dn_test.go`, `internal/ldapserver/build_test.go`

**Interfaces:**
- Consumes: `Entry` (Task 1); `database.Contact`, `database.GroupNumber`, `phonebook.GroupWithMembers` con `ActiveMembers() []*database.Contact`.
- Produces:
  - `func BuildEntries(baseDN string, contacts []*database.Contact, groups []*phonebook.GroupWithMembers) []*Entry` — prima le entry di struttura (base, `ou=contatti`, `ou=gruppi`), poi contatti e gruppi ordinati per `cn` case-insensitive.
  - `func escapeDNValue(v string) string`
  - `func dnEqual(a, b string) bool` — confronto case-insensitive, `false` se uno dei due non è un DN valido.
  - `func inScope(dn, base *ldap.DN, scope gldap.Scope) bool`

- [ ] **Step 1: Aggiungere la dipendenza gldap** (serve a `dn.go` per `gldap.Scope`)

Run: `go get github.com/jimlambrt/gldap@v0.1.14`
Expected: `go.mod` contiene `github.com/jimlambrt/gldap v0.1.14`.

- [ ] **Step 2: Scrivere i test (falliscono)**

`internal/ldapserver/dn_test.go`:

```go
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
```

`internal/ldapserver/build_test.go`:

```go
package ldapserver

import (
	"reflect"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

const testBase = "dc=rubrica,dc=local"

func findEntry(entries []*Entry, dn string) *Entry {
	for _, e := range entries {
		if e.DN == dn {
			return e
		}
	}
	return nil
}

func TestBuildEntriesContacts(t *testing.T) {
	now := time.Now()
	contacts := []*database.Contact{
		{UID: "mrossi", DisplayName: "Mario Rossi", LDAPExt: "700; 701 ;", Department: "Ragioneria", Title: "Istruttore", Email: "m.rossi@example.it"},
		{UID: "noext", DisplayName: "Senza Interno", LDAPExt: ""},
		{UID: "cher", DisplayName: "Cher", LDAPExt: "800"},
		{UID: "off", DisplayName: "Disattivo", LDAPExt: "900", Disabled: true},
		{UID: "del", DisplayName: "Cancellato", LDAPExt: "901", DeletedAt: &now},
		{UID: "a,b", DisplayName: "", LDAPExt: "902"},
	}
	entries := BuildEntries(testBase, contacts, nil)

	rossi := findEntry(entries, "uid=mrossi,ou=contatti,"+testBase)
	if rossi == nil {
		t.Fatal("entry mrossi mancante")
	}
	want := map[string][]string{
		"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
		"uid":             {"mrossi"},
		"cn":              {"Mario Rossi"},
		"displayName":     {"Mario Rossi"},
		"sn":              {"Rossi"},
		"givenName":       {"Mario"},
		"telephoneNumber": {"700", "701"},
		"ou":              {"Ragioneria"},
		"title":           {"Istruttore"},
		"mail":            {"m.rossi@example.it"},
	}
	if !reflect.DeepEqual(rossi.Attrs, want) {
		t.Errorf("attributi mrossi = %v\natteso %v", rossi.Attrs, want)
	}

	cher := findEntry(entries, "uid=cher,ou=contatti,"+testBase)
	if cher == nil {
		t.Fatal("entry cher mancante")
	}
	if got := cher.Get("sn"); !reflect.DeepEqual(got, []string{"Cher"}) {
		t.Errorf("sn di nome singolo = %v, atteso [Cher]", got)
	}
	if got := cher.Get("givenName"); got != nil {
		t.Errorf("givenName di nome singolo = %v, atteso assente", got)
	}
	if cher.Get("ou") != nil || cher.Get("mail") != nil || cher.Get("title") != nil {
		t.Error("attributi vuoti devono essere omessi")
	}

	for _, dn := range []string{
		"uid=noext,ou=contatti," + testBase,
		"uid=off,ou=contatti," + testBase,
		"uid=del,ou=contatti," + testBase,
	} {
		if findEntry(entries, dn) != nil {
			t.Errorf("entry %s non doveva esserci", dn)
		}
	}

	comma := findEntry(entries, `uid=a\,b,ou=contatti,`+testBase)
	if comma == nil {
		t.Fatal("entry con uid da escapare mancante")
	}
	if got := comma.Get("cn"); !reflect.DeepEqual(got, []string{"a,b"}) {
		t.Errorf("cn senza DisplayName = %v, atteso uid", got)
	}
}

func TestBuildEntriesGroups(t *testing.T) {
	active := &database.Contact{UID: "mrossi", DisplayName: "Mario Rossi"}
	disabled := &database.Contact{UID: "off", DisplayName: "Off", Disabled: true}
	groups := []*phonebook.GroupWithMembers{
		{Group: &database.GroupNumber{Number: "600", Name: "Reception"}, Members: []*database.Contact{active, disabled}},
		{Group: &database.GroupNumber{Number: "601", Name: "Tutti disattivi"}, Members: []*database.Contact{disabled}},
		{Group: &database.GroupNumber{Number: "602", Name: "Vuoto"}},
		{Group: &database.GroupNumber{Number: "603", Name: ""}, Members: []*database.Contact{active}},
	}
	entries := BuildEntries(testBase, nil, groups)

	rec := findEntry(entries, "uid=gruppo-600,ou=gruppi,"+testBase)
	if rec == nil {
		t.Fatal("entry gruppo 600 mancante")
	}
	want := map[string][]string{
		"objectClass":     {"top", "person", "organizationalPerson", "inetOrgPerson"},
		"uid":             {"gruppo-600"},
		"cn":              {"Reception"},
		"displayName":     {"Reception"},
		"sn":              {"Reception"},
		"telephoneNumber": {"600"},
	}
	if !reflect.DeepEqual(rec.Attrs, want) {
		t.Errorf("attributi gruppo = %v\natteso %v", rec.Attrs, want)
	}
	if findEntry(entries, "uid=gruppo-601,ou=gruppi,"+testBase) != nil {
		t.Error("gruppo con soli membri disattivi non doveva esserci")
	}
	if findEntry(entries, "uid=gruppo-602,ou=gruppi,"+testBase) != nil {
		t.Error("gruppo senza membri non doveva esserci")
	}
	noName := findEntry(entries, "uid=gruppo-603,ou=gruppi,"+testBase)
	if noName == nil || !reflect.DeepEqual(noName.Get("cn"), []string{"603"}) {
		t.Error("gruppo senza nome deve usare il numero come cn")
	}
}

func TestBuildEntriesStructureAndOrder(t *testing.T) {
	contacts := []*database.Contact{
		{UID: "z", DisplayName: "zeta Zeta", LDAPExt: "1"},
		{UID: "a", DisplayName: "Alfa Alfa", LDAPExt: "2"},
	}
	groups := []*phonebook.GroupWithMembers{
		{Group: &database.GroupNumber{Number: "600", Name: "Mensa"}, Members: []*database.Contact{{UID: "x"}}},
	}
	entries := BuildEntries(testBase, contacts, groups)

	var dns []string
	for _, e := range entries {
		dns = append(dns, e.DN)
	}
	want := []string{
		testBase,
		"ou=contatti," + testBase,
		"ou=gruppi," + testBase,
		"uid=a,ou=contatti," + testBase,
		"uid=gruppo-600,ou=gruppi," + testBase,
		"uid=z,ou=contatti," + testBase,
	}
	if !reflect.DeepEqual(dns, want) {
		t.Errorf("ordine DN = %v\natteso %v", dns, want)
	}
	if got := entries[0].Get("dc"); !reflect.DeepEqual(got, []string{"rubrica"}) {
		t.Errorf("attributo RDN della base = %v, atteso [rubrica]", got)
	}
	if got := entries[1].Get("objectClass"); !reflect.DeepEqual(got, []string{"top", "organizationalUnit"}) {
		t.Errorf("objectClass ou = %v", got)
	}
}
```

- [ ] **Step 3: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/ldapserver/ -run 'TestEscapeDNValue|TestDNEqual|TestInScope|TestBuildEntries' -v`
Expected: FAIL, `undefined: escapeDNValue` / `undefined: BuildEntries`.

- [ ] **Step 4: Implementare**

`internal/ldapserver/dn.go`:

```go
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
```

`internal/ldapserver/build.go`:

```go
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
```

- [ ] **Step 5: Eseguire i test e verificare che passino**

Run: `go mod tidy && go test ./internal/ldapserver/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/ldapserver/dn.go internal/ldapserver/dn_test.go internal/ldapserver/build.go internal/ldapserver/build_test.go
git commit -m "feat(ldapserver): entry LDAP da contatti e gruppi di chiamata"
```

---

### Task 3: Impostazioni in app_config

**Files:**
- Create: `internal/ldapserver/settings.go`
- Test: `internal/ldapserver/settings_test.go`

**Interfaces:**
- Consumes: `database.DB` con `GetConfig(key string) (string, error)` e `SetConfig(key, value string) error`.
- Produces:
  - costanti `BaseDNConfigKey`, `BindDNConfigKey`, `BindPasswordConfigKey`, `DefaultBaseDN`, `DefaultBindDN`
  - `type Settings struct { BaseDN, BindDN, BindPassword string }`
  - `func LoadSettings(db *database.DB) Settings`
  - `func SaveSettings(db *database.DB, baseDN, bindDN, newPassword string) error` — DN vuoti → default; password vuota → invariata; DN non valido → errore, niente salvato.

- [ ] **Step 1: Scrivere il test (fallisce)**

`internal/ldapserver/settings_test.go`:

```go
package ldapserver

import (
	"path/filepath"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

func newTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLoadSettingsDefaults(t *testing.T) {
	st := LoadSettings(newTestDB(t))
	if st.BaseDN != DefaultBaseDN || st.BindDN != DefaultBindDN || st.BindPassword != "" {
		t.Fatalf("default inattesi: %+v", st)
	}
}

func TestSaveSettings(t *testing.T) {
	db := newTestDB(t)
	if err := SaveSettings(db, " dc=comune,dc=local ", "cn=phone,dc=comune,dc=local", "segreta"); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	st := LoadSettings(db)
	if st.BaseDN != "dc=comune,dc=local" || st.BindDN != "cn=phone,dc=comune,dc=local" || st.BindPassword != "segreta" {
		t.Fatalf("valori salvati inattesi: %+v", st)
	}

	// password vuota = invariata, DN vuoti = default
	if err := SaveSettings(db, "", "", ""); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	st = LoadSettings(db)
	if st.BaseDN != DefaultBaseDN || st.BindDN != DefaultBindDN || st.BindPassword != "segreta" {
		t.Fatalf("atteso default DN e password invariata: %+v", st)
	}
}

func TestSaveSettingsInvalidDN(t *testing.T) {
	db := newTestDB(t)
	if err := SaveSettings(db, "non-un-dn", "", "x"); err == nil {
		t.Fatal("atteso errore per base DN non valido")
	}
	if err := SaveSettings(db, "", "non-un-dn", "x"); err == nil {
		t.Fatal("atteso errore per bind DN non valido")
	}
	if st := LoadSettings(db); st.BindPassword != "" {
		t.Fatalf("con DN non valido non deve salvare nulla: %+v", st)
	}
}
```

- [ ] **Step 2: Eseguire il test e verificare che fallisca**

Run: `go test ./internal/ldapserver/ -run 'Settings' -v`
Expected: FAIL, `undefined: LoadSettings`.

- [ ] **Step 3: Implementare**

`internal/ldapserver/settings.go`:

```go
package ldapserver

import (
	"fmt"
	"strings"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// Chiavi app_config del server LDAP: nessuna env var, si editano da
// /admin/phone-directory (come la config del centralino).
const (
	BaseDNConfigKey       = "ldapsrv_base_dn"
	BindDNConfigKey       = "ldapsrv_bind_dn"
	BindPasswordConfigKey = "ldapsrv_bind_password"

	DefaultBaseDN = "dc=rubrica,dc=local"
	DefaultBindDN = "cn=telefoni,dc=rubrica,dc=local"
)

// Settings è la configurazione corrente del server LDAP. Letta dal DB a
// ogni bind/search: una modifica da admin vale subito, senza riavvio.
type Settings struct {
	BaseDN       string
	BindDN       string
	BindPassword string
}

// LoadSettings legge la config da app_config, con i default per i DN.
// BindPassword vuota = nessuna password configurata (ogni bind rifiutato).
func LoadSettings(db *database.DB) Settings {
	st := Settings{BaseDN: DefaultBaseDN, BindDN: DefaultBindDN}
	if v, _ := db.GetConfig(BaseDNConfigKey); v != "" {
		st.BaseDN = v
	}
	if v, _ := db.GetConfig(BindDNConfigKey); v != "" {
		st.BindDN = v
	}
	st.BindPassword, _ = db.GetConfig(BindPasswordConfigKey)
	return st
}

// SaveSettings valida e salva la config. DN vuoti tornano al default; la
// password vuota lascia invariata quella salvata (mai ri-mostrata nel form).
// Se un DN non è valido non salva nulla.
func SaveSettings(db *database.DB, baseDN, bindDN, newPassword string) error {
	baseDN = strings.TrimSpace(baseDN)
	if baseDN == "" {
		baseDN = DefaultBaseDN
	}
	bindDN = strings.TrimSpace(bindDN)
	if bindDN == "" {
		bindDN = DefaultBindDN
	}
	if _, err := ldap.ParseDN(baseDN); err != nil {
		return fmt.Errorf("base DN non valido: %w", err)
	}
	if _, err := ldap.ParseDN(bindDN); err != nil {
		return fmt.Errorf("bind DN non valido: %w", err)
	}
	if err := db.SetConfig(BaseDNConfigKey, baseDN); err != nil {
		return err
	}
	if err := db.SetConfig(BindDNConfigKey, bindDN); err != nil {
		return err
	}
	if newPassword != "" {
		return db.SetConfig(BindPasswordConfigKey, newPassword)
	}
	return nil
}
```

- [ ] **Step 4: Eseguire i test e verificare che passino**

Run: `go test ./internal/ldapserver/ -v`
Expected: PASS. Se `TestSaveSettingsInvalidDN` fallisce perché `ldap.ParseDN("non-un-dn")` non restituisce errore, usare nel test un valore sicuramente invalido come `"cn"` (RDN senza `=`) e verificare di nuovo.

- [ ] **Step 5: Commit**

```bash
git add internal/ldapserver/settings.go internal/ldapserver/settings_test.go
git commit -m "feat(ldapserver): impostazioni bind/base DN in app_config"
```

---

### Task 4: Server LDAP (bind + search)

**Files:**
- Create: `internal/ldapserver/server.go`
- Test: `internal/ldapserver/server_test.go`

**Interfaces:**
- Consumes: `ParseFilter`, `Entry` (Task 1); `BuildEntries`, `dnEqual`, `inScope` (Task 2); `LoadSettings`, `SaveSettings` (Task 3); `db.ListAllContacts(limit, offset int)`, `(*phonebook.Service).ListGroupsWithMembers()`.
- Produces:
  - `func New(db *database.DB, pb *phonebook.Service) (*Server, error)`
  - `func (s *Server) Run(addr string) error` — bloccante.
  - `func (s *Server) Stop() error`
  - `func (s *Server) Ready() bool`

- [ ] **Step 1: Aggiungere la dipendenza del logger**

Run: `go get github.com/hashicorp/go-hclog@v1.6.3`

- [ ] **Step 2: Scrivere i test di integrazione (falliscono)**

`internal/ldapserver/server_test.go`:

```go
package ldapserver

import (
	"net"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
	"github.com/go-ldap/ldap/v3"
)

const testPassword = "segreta"

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func seedDirectory(t *testing.T, db *database.DB) {
	t.Helper()
	now := time.Now()
	contacts := []*database.Contact{
		{UID: "mrossi", DisplayName: "Mario Rossi", LDAPExt: "759", LastSync: now},
		{UID: "lbianchi", DisplayName: "Luca Bianchi", LDAPExt: "700;701", LastSync: now},
		{UID: "gverdi", DisplayName: "Giulia Verdi", LDAPExt: "800", Disabled: true, LastSync: now},
	}
	for _, c := range contacts {
		if err := db.UpsertContact(c); err != nil {
			t.Fatalf("UpsertContact: %v", err)
		}
	}
	g := &database.GroupNumber{Number: "600", Name: "Reception"}
	if err := db.CreateGroup(g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := db.AddGroupMember(g.ID, contacts[0].ID); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
}

// startServer avvia il server su una porta libera con una rubrica di prova;
// withPassword=false simula il server appena installato (password mai impostata).
func startServer(t *testing.T, withPassword bool) (string, *database.DB) {
	t.Helper()
	db := newTestDB(t)
	if withPassword {
		if err := SaveSettings(db, "", "", testPassword); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}
	}
	seedDirectory(t, db)

	srv, err := New(db, phonebook.NewService(db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := freeAddr(t)
	go func() { _ = srv.Run(addr) }()
	t.Cleanup(func() { _ = srv.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for !srv.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("server LDAP non pronto")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, db
}

func dial(t *testing.T, addr string) *ldap.Conn {
	t.Helper()
	l, err := ldap.DialURL("ldap://" + addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func bindOK(t *testing.T, l *ldap.Conn) {
	t.Helper()
	if err := l.Bind(DefaultBindDN, testPassword); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func search(l *ldap.Conn, base, filter string, sizeLimit int, attrs ...string) (*ldap.SearchResult, error) {
	return l.Search(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		sizeLimit, 0, false, filter, attrs, nil))
}

// phoneAttrs sono gli attributi che chiede il template del telefono.
var phoneAttrs = []string{"cn", "sn", "displayName", "telephoneNumber", "mobile"}

func TestBindRejected(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	if err := l.Bind(DefaultBindDN, "sbagliata"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("password errata: err = %v, atteso invalidCredentials", err)
	}
	if err := l.Bind("cn=altro,dc=rubrica,dc=local", testPassword); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("DN errato: err = %v, atteso invalidCredentials", err)
	}
	if err := l.UnauthenticatedBind(""); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("bind anonimo: err = %v, atteso invalidCredentials", err)
	}
	if err := l.Bind("CN=Telefoni, DC=Rubrica, DC=Local", testPassword); err != nil {
		t.Errorf("bind con DN in maiuscolo: %v", err)
	}
}

func TestBindWithoutConfiguredPassword(t *testing.T) {
	addr, _ := startServer(t, false)
	l := dial(t, addr)
	if err := l.Bind(DefaultBindDN, "qualsiasi"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("err = %v, atteso invalidCredentials senza password configurata", err)
	}
}

func TestSearchRequiresBind(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	_, err := search(l, DefaultBaseDN, "(objectClass=*)", 0)
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("err = %v, atteso insufficientAccessRights", err)
	}
}

func TestSearchByNumber(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	cases := map[string]string{"759": "Mario Rossi", "701": "Luca Bianchi", "600": "Reception"}
	for number, name := range cases {
		res, err := search(l, DefaultBaseDN, "(|(telephoneNumber="+number+")(mobile="+number+"))", 1000, phoneAttrs...)
		if err != nil {
			t.Fatalf("search %s: %v", number, err)
		}
		if len(res.Entries) != 1 || res.Entries[0].GetAttributeValue("displayName") != name {
			t.Errorf("numero %s: entries = %d, atteso 1 con displayName %q", number, len(res.Entries), name)
		}
	}

	res, err := search(l, DefaultBaseDN, "(|(telephoneNumber=800)(mobile=800))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search 800: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("contatto disabled restituito: %d entry", len(res.Entries))
	}
}

func TestSearchByName(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(|(cn=*ROSS*)(sn=*ROSS*))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].GetAttributeValue("telephoneNumber") != "759" {
		t.Fatalf("attesa 1 entry con interno 759, avute %d", len(res.Entries))
	}

	res, err = search(l, DefaultBaseDN, "(|(cn=*i*)(sn=*i*))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.GetAttributeValue("cn"))
	}
	want := []string{"Luca Bianchi", "Mario Rossi", "Reception"}
	if len(names) != len(want) {
		t.Fatalf("nomi = %v, attesi %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("ordine nomi = %v, atteso %v", names, want)
		}
	}
}

func TestSearchSizeLimit(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(telephoneNumber=*)", 2)
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		t.Fatalf("err = %v, atteso sizeLimitExceeded", err)
	}
	if res == nil || len(res.Entries) != 2 {
		t.Fatalf("attese 2 entry parziali, avuto %v", res)
	}
}

func TestSearchSelectedAttributes(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(telephoneNumber=759)", 0, "cn")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 1 || len(res.Entries[0].Attributes) != 1 || res.Entries[0].Attributes[0].Name != "cn" {
		t.Fatalf("atteso solo cn, avuto %+v", res.Entries)
	}
}

func TestSearchBaseDNChange(t *testing.T) {
	addr, db := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	if _, err := search(l, "dc=voip,dc=local", "(objectClass=*)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Fatalf("base sconosciuta: err = %v, atteso noSuchObject", err)
	}

	if err := SaveSettings(db, "dc=comune,dc=local", "", ""); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if _, err := search(l, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Fatalf("vecchia base dopo la modifica: err = %v, atteso noSuchObject", err)
	}
	res, err := search(l, "dc=comune,dc=local", "(telephoneNumber=759)", 0)
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("nuova base: err = %v, entries = %v", err, res)
	}
	if res.Entries[0].DN != "uid=mrossi,ou=contatti,dc=comune,dc=local" {
		t.Errorf("DN = %q", res.Entries[0].DN)
	}
}

func TestAuthIsPerConnection(t *testing.T) {
	addr, _ := startServer(t, true)
	authed := dial(t, addr)
	bindOK(t, authed)
	anon := dial(t, addr)

	if _, err := search(anon, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("connessione non autenticata: err = %v, atteso insufficientAccessRights", err)
	}
	if res, err := search(authed, DefaultBaseDN, "(telephoneNumber=759)", 0); err != nil || len(res.Entries) != 1 {
		t.Fatalf("connessione autenticata: err = %v", err)
	}

	// un bind fallito sulla connessione autenticata la riporta a non autenticata
	_ = authed.Bind(DefaultBindDN, "sbagliata")
	if _, err := search(authed, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("dopo bind fallito: err = %v, atteso insufficientAccessRights", err)
	}
}
```

- [ ] **Step 3: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/ldapserver/ -run 'TestBind|TestSearch|TestAuth' -v`
Expected: FAIL, `undefined: New`.

- [ ] **Step 4: Implementare**

`internal/ldapserver/server.go`:

```go
package ldapserver

import (
	"crypto/subtle"
	"log"
	"strings"
	"sync"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
	"github.com/go-ldap/ldap/v3"
	"github.com/hashicorp/go-hclog"
	"github.com/jimlambrt/gldap"
)

// maxResults è il tetto di entry per search, qualunque sizeLimit chieda il
// client (i telefoni chiedono fino a 1000).
const maxResults = 1000

// Server è il server LDAP read-only per i telefoni. Le operazioni di
// scrittura non hanno route: gldap risponde da sé unwillingToPerform.
type Server struct {
	db *database.DB
	pb *phonebook.Service
	gs *gldap.Server

	mu     sync.Mutex
	authed map[int]bool // connectionID -> bind riuscito
}

func New(db *database.DB, pb *phonebook.Service) (*Server, error) {
	s := &Server{db: db, pb: pb, authed: map[int]bool{}}
	gs, err := gldap.NewServer(
		gldap.WithLogger(hclog.New(&hclog.LoggerOptions{Name: "ldapsrv", Level: hclog.Warn})),
		gldap.WithOnClose(s.forget),
	)
	if err != nil {
		return nil, err
	}
	mux, err := gldap.NewMux()
	if err != nil {
		return nil, err
	}
	if err := mux.Bind(s.handleBind); err != nil {
		return nil, err
	}
	if err := mux.Search(s.handleSearch); err != nil {
		return nil, err
	}
	if err := gs.Router(mux); err != nil {
		return nil, err
	}
	s.gs = gs
	return s, nil
}

// Run avvia il listener su addr (host:porta) e blocca finché il server non
// viene fermato.
func (s *Server) Run(addr string) error { return s.gs.Run(addr) }

func (s *Server) Stop() error { return s.gs.Stop() }

// Ready dice se il listener è attivo.
func (s *Server) Ready() bool { return s.gs.Ready() }

func (s *Server) forget(connID int) {
	s.mu.Lock()
	delete(s.authed, connID)
	s.mu.Unlock()
}

func (s *Server) setAuthed(connID int, ok bool) {
	s.mu.Lock()
	s.authed[connID] = ok
	s.mu.Unlock()
}

func (s *Server) isAuthed(connID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authed[connID]
}

func (s *Server) handleBind(w *gldap.ResponseWriter, r *gldap.Request) {
	resp := r.NewBindResponse(gldap.WithResponseCode(gldap.ResultInvalidCredentials))
	defer func() { _ = w.Write(resp) }()

	m, err := r.GetSimpleBindMessage()
	if err != nil {
		s.setAuthed(r.ConnectionID(), false)
		return
	}
	ok := checkCredentials(LoadSettings(s.db), m.UserName, string(m.Password))
	s.setAuthed(r.ConnectionID(), ok)
	if !ok {
		log.Printf("[LDAPSRV] Bind rifiutato per %q (conn %d)", m.UserName, r.ConnectionID())
		return
	}
	resp.SetResultCode(gldap.ResultSuccess)
}

// checkCredentials accetta solo il bind DN configurato con la sua password;
// senza password configurata rifiuta sempre (server acceso ma chiuso).
func checkCredentials(st Settings, user, pass string) bool {
	if st.BindPassword == "" || pass == "" || !dnEqual(user, st.BindDN) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(st.BindPassword)) == 1
}

func (s *Server) handleSearch(w *gldap.ResponseWriter, r *gldap.Request) {
	resp := r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultSuccess))
	defer func() { _ = w.Write(resp) }()

	if !s.isAuthed(r.ConnectionID()) {
		resp.SetResultCode(gldap.ResultInsufficientAccessRights)
		return
	}
	m, err := r.GetSearchMessage()
	if err != nil {
		resp.SetResultCode(gldap.ResultProtocolError)
		return
	}
	filter, err := ParseFilter(m.Filter)
	if err != nil {
		resp.SetResultCode(gldap.ResultProtocolError)
		return
	}
	entries, err := s.loadEntries(LoadSettings(s.db).BaseDN)
	if err != nil {
		log.Printf("[LDAPSRV] Lettura rubrica fallita: %v", err)
		resp.SetResultCode(gldap.ResultOperationsError)
		return
	}
	matched, found := selectEntries(entries, m.BaseDN, m.Scope, filter)
	if !found {
		resp.SetResultCode(gldap.ResultNoSuchObject)
		return
	}

	limit := maxResults
	if m.SizeLimit > 0 && m.SizeLimit < int64(limit) {
		limit = int(m.SizeLimit)
	}
	if len(matched) > limit {
		matched = matched[:limit]
		resp.SetResultCode(gldap.ResultSizeLimitExceeded)
	}
	for _, e := range matched {
		_ = w.Write(r.NewSearchResponseEntry(e.DN, gldap.WithAttributes(selectAttributes(e, m.Attributes))))
	}
}

// loadEntries legge contatti e gruppi dal DB a ogni search: poche centinaia
// di righe, e così i dati sono sempre quelli dell'ultimo sync.
func (s *Server) loadEntries(baseDN string) ([]*Entry, error) {
	contacts, err := s.db.ListAllContacts(100000, 0)
	if err != nil {
		return nil, err
	}
	groups, err := s.pb.ListGroupsWithMembers()
	if err != nil {
		return nil, err
	}
	return BuildEntries(baseDN, contacts, groups), nil
}

// selectEntries applica base DN, scope e filtro. found=false se il base DN
// della richiesta non corrisponde a nessuna entry (→ noSuchObject).
func selectEntries(entries []*Entry, baseDN string, scope gldap.Scope, f Filter) ([]*Entry, bool) {
	base, err := ldap.ParseDN(baseDN)
	if err != nil {
		return nil, false
	}
	found := false
	var out []*Entry
	for _, e := range entries {
		dn, err := ldap.ParseDN(e.DN)
		if err != nil {
			continue
		}
		if dn.EqualFold(base) {
			found = true
		}
		if inScope(dn, base, scope) && f(e) {
			out = append(out, e)
		}
	}
	return out, found
}

// selectAttributes restituisce solo gli attributi richiesti (nomi
// case-insensitive); lista vuota o "*" = tutti.
func selectAttributes(e *Entry, requested []string) map[string][]string {
	if len(requested) == 0 {
		return e.Attrs
	}
	for _, a := range requested {
		if a == "*" {
			return e.Attrs
		}
	}
	out := map[string][]string{}
	for _, a := range requested {
		for k, v := range e.Attrs {
			if strings.EqualFold(k, a) {
				out[k] = v
			}
		}
	}
	return out
}
```

- [ ] **Step 5: Eseguire i test (con race detector, come in CI) e verificare che passino**

Run: `go mod tidy && go test -race ./internal/ldapserver/ -v`
Expected: PASS. Se `-race` fallisce per un problema di toolchain del container alpine (non per un data race), rieseguire senza `-race` e segnalarlo nel report del task: la CI (`.github/workflows/test.yml`) lo esegue comunque con `-race`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/ldapserver/server.go internal/ldapserver/server_test.go
git commit -m "feat(ldapserver): server LDAP read-only con bind e search"
```

---

### Task 5: Integrazione — config, avvio, pagina admin, deploy, documentazione

**Files:**
- Modify: `internal/config/config.go` (struct `Config` righe ~12-50, `Load()` righe ~53-78)
- Modify: `cmd/server/main.go` (var package-level ~righe 90-101, `main()` dopo `pbService = phonebook.NewService(db)` ~riga 138, route admin ~riga 298, handler in fondo accanto a `handleAdminPBX` ~riga 1708)
- Create: `web/templates/admin_page_phone_directory.html`, `web/templates/admin_phone_directory.html`
- Modify: `web/templates/rail.html` (dopo la voce `/admin/pbx`, ~riga 58)
- Modify: `docker-compose.yml`, `Dockerfile`, `.env.example`, `CLAUDE.md`
- Modify: `docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md`

**Interfaces:**
- Consumes: `ldapserver.New`, `(*Server).Run/Ready`, `ldapserver.LoadSettings`, `ldapserver.SaveSettings`, costanti `DefaultBaseDN`/`DefaultBindDN` (Task 3-4); helper esistenti `railData()`, `i18n.GetMessages`, `i18n.ResolveLocale`, `sessionAdminUsername(r)`, `templates`.
- Produces: `cfg.LDAPServerPort string`; route `GET/POST /admin/phone-directory`.

- [ ] **Step 1: Config**

In `internal/config/config.go`, nella struct `Config` dopo `ServerPort string`:

```go
	// LDAPServerPort è la porta del server LDAP per i telefoni
	// (internal/ldapserver); "0" = server non avviato.
	LDAPServerPort string
```

In `Load()`, dopo `ServerPort: getEnv("SERVER_PORT", "8080"),`:

```go
		LDAPServerPort:      getEnv("LDAP_SERVER_PORT", "3389"),
```

(allineare gli spazi con `gofmt`).

- [ ] **Step 2: Avvio del server in `main.go`**

Import: aggiungere `"github.com/Comune-di-Montesilvano/Rubrica/internal/ldapserver"`.

Tra le var package-level (blocco con `pbxManualSync`, `backupManualSync`), aggiungere in un blocco `var` accanto:

```go
// ldapSrv è il server LDAP per i telefoni; nil se disattivato
// (LDAP_SERVER_PORT=0) o se l'inizializzazione è fallita.
var ldapSrv *ldapserver.Server
```

In `main()`, subito dopo `pbService = phonebook.NewService(db)`:

```go
	// Server LDAP read-only per i telefoni VoIP: errori non fatali, la
	// rubrica web resta su anche se la porta LDAP non si apre.
	if cfg.LDAPServerPort != "0" {
		srv, err := ldapserver.New(db, pbService)
		if err != nil {
			log.Printf("[LDAPSRV] Init failed: %v", err)
		} else {
			ldapSrv = srv
			ldapAddr := fmt.Sprintf("%s:%s", cfg.ServerHost, cfg.LDAPServerPort)
			go func() {
				log.Printf("[LDAPSRV] Starting LDAP server on %s", ldapAddr)
				if err := srv.Run(ldapAddr); err != nil {
					log.Printf("[LDAPSRV] Server stopped: %v", err)
				}
			}()
		}
	}
```

- [ ] **Step 3: Handler admin in `main.go`**

Route, subito dopo `admin.HandleFunc("/pbx/sync/status", ...)`:

```go
	admin.HandleFunc("/phone-directory", handleAdminPhoneDirectory).Methods("GET")
	admin.HandleFunc("/phone-directory", handleAdminSavePhoneDirectory).Methods("POST")
```

Handler, subito dopo `handleAdminPBX`:

```go
// phoneDirectoryData prepara i dati della pagina "Rubrica telefoni"
// (server LDAP per i telefoni, vedi internal/ldapserver).
func phoneDirectoryData(r *http.Request) map[string]interface{} {
	st := ldapserver.LoadSettings(db)
	data := railData()
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-phone-directory"
	data["BaseDN"] = st.BaseDN
	data["BindDN"] = st.BindDN
	data["HasPassword"] = st.BindPassword != ""
	data["Enabled"] = cfg.LDAPServerPort != "0"
	data["Listening"] = ldapSrv != nil && ldapSrv.Ready()
	data["Port"] = cfg.LDAPServerPort
	return data
}

// handleAdminPhoneDirectory serve la pagina "Rubrica telefoni" completa.
func handleAdminPhoneDirectory(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "admin_page_phone_directory.html", phoneDirectoryData(r))
}

// handleAdminSavePhoneDirectory salva base DN, bind DN e password del server
// LDAP. Password vuota = invariata (mai ri-mostrata nel form); DN non valido
// = niente salvato, errore mostrato nel frammento.
func handleAdminSavePhoneDirectory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	err := ldapserver.SaveSettings(db, r.FormValue("base_dn"), r.FormValue("bind_dn"), r.FormValue("bind_password"))
	data := phoneDirectoryData(r)
	if err != nil {
		data["Error"] = err.Error()
	} else {
		data["Saved"] = true
	}
	templates.ExecuteTemplate(w, "admin_phone_directory.html", data)
}
```

- [ ] **Step 4: Template**

`web/templates/admin_page_phone_directory.html`:

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Rubrica telefoni - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        {{template "rail.html" .}}
        <main class="main" style="max-width:920px;">
            <h1 class="page-title">Rubrica telefoni</h1>
            <div id="phone-directory-content">{{template "admin_phone_directory.html" .}}</div>
        </main>
    </div>
</body>
</html>
```

`web/templates/admin_phone_directory.html`:

```html
<div class="card">
    <h3>Server LDAP</h3>
    <div class="helptext" style="margin-bottom:10px;">
        {{if not .Enabled}}Disattivato (LDAP_SERVER_PORT=0).
        {{else if .Listening}}In ascolto sulla porta {{.Port}} del container (sull'host: LDAP_SERVER_PORT_HOST, default 389).
        {{else}}Non in ascolto: controllare i log del container ([LDAPSRV]).{{end}}
        {{if not .HasPassword}}<br><strong>Nessuna password impostata: ogni accesso dei telefoni viene rifiutato.</strong>{{end}}
    </div>
    {{if .Error}}<div class="helptext" style="color:var(--danger, #b42318);margin-bottom:10px;">{{.Error}}</div>{{end}}
    {{if .Saved}}<div class="helptext" style="margin-bottom:10px;">Configurazione salvata.</div>{{end}}
    <form hx-post="/admin/phone-directory" hx-target="#phone-directory-content" hx-swap="innerHTML" style="display:flex;flex-direction:column;gap:10px;">
        <div>
            <div class="helptext" style="margin-bottom:4px;">Base DN</div>
            <input type="text" name="base_dn" value="{{.BaseDN}}" class="input" style="width:100%;">
        </div>
        <div>
            <div class="helptext" style="margin-bottom:4px;">Utente (bind DN)</div>
            <input type="text" name="bind_dn" value="{{.BindDN}}" class="input" style="width:100%;">
        </div>
        <div>
            <div class="helptext" style="margin-bottom:4px;">Password</div>
            <input type="password" name="bind_password" value="" placeholder="{{if .HasPassword}}(lasciare vuoto per non modificare){{else}}nessuna password salvata{{end}}" class="input" style="width:100%;">
        </div>
        <button type="submit" class="btn btn-primary" style="flex:none;align-self:flex-start;">{{index .Messages "save"}}</button>
    </form>
</div>

<div class="card" style="margin-top:16px;">
    <h3>Template di provisioning del centralino</h3>
    <div class="helptext" style="margin-bottom:10px;">Valori da impostare nella sezione LDAP del template telefoni sul ViVo.</div>
    <div class="modal-scroll-x">
    <table>
        <tbody>
            <tr><td>Indirizzo del server</td><td>IP dell'host Docker di Rubrica</td></tr>
            <tr><td>Porta</td><td>389 (o LDAP_SERVER_PORT_HOST)</td></tr>
            <tr><td>Base</td><td><code>{{.BaseDN}}</code></td></tr>
            <tr><td>Nome utente</td><td><code>{{.BindDN}}</code></td></tr>
            <tr><td>Filtro nome LDAP</td><td><code>(|(cn=*%*)(sn=*%*))</code></td></tr>
            <tr><td>Filtro numerico LDAP</td><td><code>(|(telephoneNumber=%)(mobile=%))</code></td></tr>
            <tr><td>Attributi del nome</td><td><code>cn sn displayName</code></td></tr>
            <tr><td>Attributi numero</td><td><code>telephoneNumber mobile</code></td></tr>
            <tr><td>Nome visualizzato</td><td><code>%displayName</code></td></tr>
            <tr><td>Protocollo</td><td>Versione 3, senza TLS</td></tr>
        </tbody>
    </table>
    </div>
</div>
```

In `web/templates/rail.html`, subito dopo il blocco `<a href="/admin/pbx" ...>...</a>`:

```html
    <a href="/admin/phone-directory" class="rail-item{{if eq .Section "admin-phone-directory"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="5" y="2" width="14" height="20" rx="2"/><path d="M9 6h6M9 10h6M9 14h3"/></svg>
        <span>Rubrica telefoni</span>
    </a>
```

- [ ] **Step 5: Deploy**

`docker-compose.yml`, sotto `ports:` del servizio `rubrica`, aggiungere:

```yaml
      - "${LDAP_SERVER_PORT_HOST:-389}:${LDAP_SERVER_PORT:-3389}"
```

e sotto `environment:`, dopo `SERVER_PORT`:

```yaml
      - LDAP_SERVER_PORT=${LDAP_SERVER_PORT:-3389}
```

`Dockerfile`: sostituire `EXPOSE 8080` con `EXPOSE 8080 3389`.

`.env.example`: aggiungere dopo `SERVER_PORT`:

```
# Server LDAP read-only per i telefoni VoIP (porta nel container, 0 = disattivato).
# Credenziali e base DN si configurano da /admin/phone-directory.
LDAP_SERVER_PORT=3389
# Porta pubblicata sull'host (i telefoni usano questa).
LDAP_SERVER_PORT_HOST=389
```

- [ ] **Step 6: Documentazione**

`CLAUDE.md`, sezione Architecture, aggiungere dopo il bullet `internal/carddav`:

```markdown
- **`internal/ldapserver`**: server LDAPv3 read-only (`github.com/jimlambrt/gldap`) per i telefoni VoIP — distinto da `internal/ldap` (client verso AD). Porta `LDAP_SERVER_PORT` (default `3389` nel container, non root; `0` = spento; compose la pubblica su `LDAP_SERVER_PORT_HOST`, default `389`); base DN / bind DN / password solo in `app_config` (`ldapsrv_*`, pagina `/admin/phone-directory`), letti a ogni richiesta. Senza password configurata ogni bind è rifiutato. A ogni search ricostruisce le entry da `ListAllContacts` + `ListGroupsWithMembers` (stesso perimetro della rubrica pubblica: niente `disabled`, gruppi solo con `ActiveMembers()`), `telephoneNumber` = solo interni (è il caller ID che arriva ai telefoni). Filtri valutati in memoria (`filter.go`, via `ldap.CompileFilter`). Il cambio lato telefoni si fa sul template di provisioning del ViVo. Vedi `docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md`.
```

Spec `docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md`, allineare alle scelte del piano:
- sezione Porta: sostituire `` `LDAP_SERVER_PORT` vuota → server LDAP non avviato. `` con `` `LDAP_SERVER_PORT=0` → server LDAP non avviato. ``
- sezione Entry, gruppo di chiamata: DN `uid=gruppo-<Number>,ou=gruppi,$BASE` con attributo `uid: gruppo-<Number>` (al posto di `cn=<Number>`, così l'attributo dell'RDN è presente nell'entry) e `objectClass` uguale a quella dei contatti;
- sezione Entry, contatto: annotare che `sn`/`givenName` sono un'euristica (ultima parola) e che la ricerca dei telefoni si basa su `cn` con wildcard.

- [ ] **Step 7: Build, vet e test completi**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: tutto PASS, nessun warning di vet.

- [ ] **Step 8: Verifica end-to-end in Docker**

1. `docker compose up -d --build` (richiede `.env` locale; se la porta host 389 è occupata, impostare `LDAP_SERVER_PORT_HOST=10389` in `.env`).
2. `docker logs rubrica 2>&1 | grep LDAPSRV` → atteso `[LDAPSRV] Starting LDAP server on 0.0.0.0:3389`.
3. Aprire `http://localhost:8080/admin/phone-directory` da admin, impostare una password, salvare → messaggio "Configurazione salvata", stato "In ascolto".
4. Da un container con client LDAP (sostituire `<PORTA>` e `<PASSWORD>`):
   `MSYS_NO_PATHCONV=1 docker run --rm alpine sh -c "apk add --no-cache openldap-clients >/dev/null && ldapsearch -x -H ldap://host.docker.internal:<PORTA> -D cn=telefoni,dc=rubrica,dc=local -w <PASSWORD> -b dc=rubrica,dc=local '(|(telephoneNumber=759)(mobile=759))' cn displayName telephoneNumber"`
   Expected: una entry con l'intestatario reale dell'interno 759, `result: 0 Success`.
5. Stesso comando con filtro `'(|(cn=*ross*)(sn=*ross*))'` → contatti che contengono "ross".
6. Stesso comando con password errata → `ldap_bind: Invalid credentials (49)`.

- [ ] **Step 9: Commit**

```bash
git add internal/config/config.go cmd/server/main.go web/templates/admin_page_phone_directory.html web/templates/admin_phone_directory.html web/templates/rail.html docker-compose.yml Dockerfile .env.example CLAUDE.md docs/superpowers/specs/2026-09-28-ldap-phone-directory-design.md
git commit -m "feat: server LDAP per i telefoni, pagina admin e deploy"
```
