package main

// Handler admin di aree, sotto-aree, regole e perimetro (vedi
// docs/superpowers/specs/2026-10-14-aree-regole-design.md). Unico file di
// handler separato da main.go: stesso pacchetto, stesse variabili globali.

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
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
	"github.com/gorilla/mux"
)

var (
	syncReportMu   sync.Mutex
	lastSyncReport ldap.SyncReport
	// syncRerun: una modifica alle regole arrivata durante un sync in corso
	// fa ripartire il sync al termine, invece di avviarne un secondo in
	// parallelo. Protetto da ldapManualSync.mu.
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
			} else {
				log.Printf("[SYNC] Manual sync completed")
			}
			ldapManualSync.mu.Lock()
			again := syncRerun
			syncRerun = false
			ldapManualSync.mu.Unlock()
			if !again {
				ldapManualSync.finish(err)
				return
			}
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

// handleAdminAreas serve la pagina "Aree" completa (perimetro + albero); le
// scritture ricevono solo il frammento della sezione toccata.
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

// manualContactsData: aree di primo livello, albero e sotto-aree attuali
// per la pagina "Contatti locali".
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
