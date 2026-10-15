package ldap

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// PrimaryNumberPrefixConfigKey è la chiave app_config per il prefisso
// numero primario, editabile da admin — sostituisce PRIMARY_NUMBER_PREFIX_TEMPLATE
// come fonte di verità una volta salvato (l'env resta solo il default
// iniziale per un deploy mai configurato da UI).
const PrimaryNumberPrefixConfigKey = "primary_number_prefix"

// loadPrimaryNumberPrefix legge il template prefisso salvato da admin, o
// quello di config.Config (env, default iniziale) se non ancora configurato.
func loadPrimaryNumberPrefix(db *database.DB, cfg *config.Config) string {
	if stored, err := db.GetConfig(PrimaryNumberPrefixConfigKey); err == nil && stored != "" {
		return stored
	}
	return cfg.PrimaryNumberPrefix
}

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

// SyncContacts: sync con opzioni di default. onProgress (opzionale) riceve
// (0, total) dopo la ricerca e poi l'avanzamento contatto per contatto.
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
		log.Printf("[SYNC] Calo anomalo: %d contatti su %d uscirebbero dalla rubrica, soft-delete sospeso: serve conferma da admin", stale, active)
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
// disabilitati non passano dal filtro, AD svuota spesso il loro memberOf e
// devono restare disponibili alla diagnostica del centralino), area e
// sotto-aree dalle regole, reparto di ripiego = nome dell'area.
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

// buildSyncSearchFilter costruisce il filtro LDAP di ricerca. Non esclude
// più gli account disabilitati lato server (LDAPOnlyActive li filtra a
// valle, nel loop di SyncContacts, salvandoli con active=false invece di
// scartarli): servono comunque per il matching nome centralino/dominio.
func buildSyncSearchFilter(cfg *config.Config) string {
	base := strings.TrimSpace(cfg.LDAPSearchFilter)
	if base == "" {
		base = "(objectClass=*)"
	}
	return base
}

// descriptionAliases raccoglie varianti note (typo, maiuscole incoerenti)
// del campo description di AD, osservate sui dati reali del Comune di
// Montesilvano. Le chiavi sono minuscole: normalizeDescription confronta
// dopo strings.ToLower, quindi qualsiasi variante di maiuscola collassa
// già a questo punto senza bisogno di duplicare le voci.
var descriptionAliases = map[string]string{
	"agente di polizi locale":  "Agente di Polizia Locale",
	"agente di polizia locale": "Agente di Polizia Locale",
	"edliizia":                 "Edilizia",
	"istuttore":                "Istruttore",
	"usciere":                  "Usciere",
}

// normalizeDescription collassa spazi multipli e riscrive typo/varianti
// note al valore canonico. Valori non mappati passano inalterati (solo
// trim). Non modifica LDAP, solo il valore scritto in contacts.description.
func normalizeDescription(raw string) string {
	trimmed := strings.Join(strings.Fields(raw), " ")
	if trimmed == "" {
		return ""
	}
	if canonical, ok := descriptionAliases[strings.ToLower(trimmed)]; ok {
		return canonical
	}
	return trimmed
}

func extractCNFromDN(dn string) string {
	dn = strings.TrimSpace(dn)
	if dn == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(dn), "cn=") {
		rest := dn[3:]
		if idx := strings.Index(rest, ","); idx > 0 {
			return strings.ToLower(rest[:idx])
		}
		return strings.ToLower(rest)
	}
	return ""
}

// SplitExtensions divide un valore telephoneNumber AD su più interni
// separati da ";" (es. "700;701"), scartando token vuoti/spazi — un
// contatto con un solo interno torna comunque uno slice di un elemento.
// Esportata: usata anche da internal/database per il matching PBX.
func SplitExtensions(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// firstExtension torna il primo interno di un valore telephoneNumber
// multi-valore (o l'unico, o "" se vuoto) — usato per il numero primario
// cliccabile, che non può contenere più di un numero.
func firstExtension(raw string) string {
	exts := SplitExtensions(raw)
	if len(exts) == 0 {
		return ""
	}
	return exts[0]
}

// generatePrimaryNumber creates a full phone number from extension using the configured template
func generatePrimaryNumber(ext string, template string) string {
	if ext == "" {
		return ""
	}

	// Replace {ext} placeholder in template
	if template == "" {
		return ext
	}

	// Simple replacement
	result := ""
	for i := 0; i < len(template); i++ {
		if i+4 < len(template) && template[i:i+5] == "{ext}" {
			result += ext
			i += 4 // Skip the rest of {ext}
		} else {
			result += string(template[i])
		}
	}

	return result
}
