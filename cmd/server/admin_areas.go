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

// scopeData: dati della sezione Perimetro (completata nel Task 9).
func scopeData() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
