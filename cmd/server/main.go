package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/backup"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/carddav"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/i18n"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldapserver"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/pbx"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

var (
	AppVersion  = "dev"
	templates   *template.Template
	store       *sessions.CookieStore
	db          *database.DB
	cfg         *config.Config
	pbService   *phonebook.Service
	lastSync    time.Time
	lastPBXSync time.Time
	// lastPBXSyncResult raccoglie le diagnostiche (mismatch nome, interni
	// riciclabili) calcolate dall'ultimo sync PBX riuscito — non esiste un
	// modo economico per ricalcolarle senza interrogare di nuovo il
	// centralino, quindi restano valide fino al prossimo sync.
	lastPBXSyncResult pbx.SyncResult
)

// syncStatus è lo stato di un sync manuale in corso, mostrato dalla UI
// admin che fa polling (hx-get ogni ~1.2s) finché Running non torna false.
// Un solo sync manuale per volta ha senso mostrarne (LDAP e PBX sono
// indipendenti, ognuno ha il proprio); i sync automatici (ticker/startup)
// non aggiornano questo stato, sono fire-and-forget in background come
// prima — qui serve solo il feedback per il click esplicito dell'admin.
type syncStatus struct {
	mu      sync.Mutex
	Running bool
	Phase   string // testo fase corrente, es. "Elaborazione contatti: 120/350"
	Message string // messaggio finale (successo o errore)
	IsError bool
}

func (s *syncStatus) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Running = true
	s.Phase = "Avvio..."
	s.Message = ""
	s.IsError = false
}

func (s *syncStatus) setPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Phase = phase
}

func (s *syncStatus) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked(err)
}

// finishLocked: come finish, con s.mu già acquisito dal chiamante.
func (s *syncStatus) finishLocked(err error) {
	s.Running = false
	if err != nil {
		s.IsError = true
		s.Message = "Sync fallito: " + err.Error()
	} else if held := getLastSyncReport().StaleHeld; held > 0 && s == ldapManualSync {
		s.IsError = true
		s.Message = fmt.Sprintf("Calo anomalo: %d contatti uscirebbero dalla rubrica. Cancellazione sospesa: verifica il perimetro in Aree e conferma.", held)
	} else {
		s.IsError = false
		s.Message = "Sync completato con successo."
	}
}

func (s *syncStatus) snapshot() syncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return syncStatus{Running: s.Running, Phase: s.Phase, Message: s.Message, IsError: s.IsError}
}

// ldapSrv è il server LDAP per i telefoni; nil se disattivato
// (LDAP_SERVER_ENABLED=false) o se l'inizializzazione è fallita.
var ldapSrv *ldapserver.Server

var (
	ldapManualSync   = &syncStatus{}
	pbxManualSync    = &syncStatus{}
	backupManualSync = &syncStatus{}
)

// backupDir è la directory dei backup, derivata da DatabasePath (stesso
// volume Docker della DB — vedi spec, nessuna env var dedicata in questa
// prima versione).
func backupDir() string {
	return filepath.Join(filepath.Dir(cfg.DatabasePath), "backups")
}

func main() {
	log.Printf("[MAIN] Starting Rubrica %s", AppVersion)

	// Load configuration
	cfg = config.Load()
	log.Printf("[CONFIG] Loaded configuration")

	// Initialize database
	var err error
	db, err = database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("[DATABASE] Failed to initialize: %v", err)
	}
	defer db.Close()

	// Migrazione una tantum: se il prefisso non è mai stato salvato da
	// admin (DB vuoto), importa il valore da .env come seed iniziale. Da
	// qui in poi la fonte di verità è il pannello admin, non più .env.
	if stored, _ := db.GetConfig(ldap.PrimaryNumberPrefixConfigKey); stored == "" && cfg.PrimaryNumberPrefix != "" {
		if err := db.SetConfig(ldap.PrimaryNumberPrefixConfigKey, cfg.PrimaryNumberPrefix); err != nil {
			log.Printf("[CONFIG] Failed to import primary_number_prefix from .env: %v", err)
		} else {
			log.Printf("[CONFIG] Imported primary_number_prefix from .env into DB: %s", cfg.PrimaryNumberPrefix)
		}
	}

	// Initialize phonebook service
	pbService = phonebook.NewService(db)

	// Server LDAP read-only per i telefoni VoIP: errori non fatali, la
	// rubrica web resta su anche se la porta LDAP non si apre.
	if cfg.LDAPServerEnabled {
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

	// Initialize session store
	store = sessions.NewCookieStore([]byte(cfg.SessionSecret))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		Secure:   false, // Set to true in production with HTTPS
		SameSite: http.SameSiteLaxMode,
	}

	// Load templates with custom functions
	funcMap := template.FuncMap{
		// extList: un contatto può avere più interni in AD separati da
		// ";" (telephoneNumber multi-valore) — mostrati "700, 701" invece
		// del ";" grezzo di storage.
		"extList": func(s string) string {
			return strings.ReplaceAll(s, ";", ", ")
		},
		// derefInt64ID: confronto diretto {{eq $ptr .ID}} tra *int64 e
		// int64 fa fallire "eq" (tipi incompatibili) e tronca il resto
		// del template — usata per confrontare GroupCategory.ParentID
		// (nullable) con l'ID di un'altra categoria nel select "genitore".
		// nil -> 0, mai collidente con un ID reale (AUTOINCREMENT parte da 1).
		"derefInt64ID": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"substr": func(s string, start, length int) string {
			if start < 0 || start >= len(s) {
				return ""
			}
			end := start + length
			if end > len(s) {
				end = len(s)
			}
			return strings.ToUpper(s[start:end])
		},
		"initials": func(name string) string {
			parts := strings.Fields(name)
			if len(parts) == 0 {
				return ""
			}
			result := strings.ToUpper(string(parts[0][0]))
			if len(parts) > 1 {
				result += strings.ToUpper(string(parts[len(parts)-1][0]))
			}
			return result
		},
		// sentenceCase riscrive un'etichetta tutta maiuscola (come i reparti
		// letti da AD, es. "POLIZIA LOCALE") in sentence case per la UI,
		// senza toccare il dato in DB. Le etichette non interamente
		// maiuscole (es. "Amministrazione politica", generata dal codice,
		// non da AD) passano invariate: non sono il caso che deve normalizzare.
		"sentenceCase": func(s string) string {
			if s == "" || s != strings.ToUpper(s) {
				return s
			}
			words := strings.Fields(strings.ToLower(s))
			for i, w := range words {
				r := []rune(w)
				if len(r) > 0 {
					r[0] = unicode.ToUpper(r[0])
				}
				words[i] = string(r)
			}
			return strings.Join(words, " ")
		},
	}
	templates = template.Must(template.New("").Funcs(funcMap).ParseGlob("web/templates/*.html"))
	log.Printf("[TEMPLATES] Loaded templates")

	// Start LDAP sync goroutine
	go ldapSyncWorker()

	// Start scheduled backup goroutine
	go backupWorker()

	// Perform initial sync
	go func() {
		if err := runLDAPSync(ldap.SyncOptions{}, nil); err != nil {
			log.Printf("[SYNC] Initial sync failed: %v", err)
		}
		if result, err := pbx.SyncPBX(db, nil); err != nil {
			log.Printf("[PBX] Initial sync failed: %v", err)
		} else {
			lastPBXSync = time.Now()
			lastPBXSyncResult = result
		}
	}()

	// Setup router
	r := mux.NewRouter()

	// Static files
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Public routes
	r.HandleFunc("/", handleIndex).Methods("GET")
	r.HandleFunc("/search", handleSearch).Methods("GET")
	r.HandleFunc("/contacts", handleContacts).Methods("GET")
	r.HandleFunc("/contacts/{uid}", handleContactDetail).Methods("GET")
	r.HandleFunc("/contacts/{uid}/fragment", handleContactDetailFragment).Methods("GET")
	r.HandleFunc("/contacts/{uid}/export", handleExportVCard).Methods("GET")
	r.HandleFunc("/health", handleHealth).Methods("GET")
	r.HandleFunc("/version", handleVersion).Methods("GET")

	// Auth routes
	r.HandleFunc("/login", handleLogin).Methods("GET", "POST")
	r.HandleFunc("/logout", handleLogout).Methods("POST")

	// Admin routes (protected)
	admin := r.PathPrefix("/admin").Subrouter()
	admin.Use(requireAuth)
	admin.Use(requireAdmin)
	admin.HandleFunc("", handleAdminDashboard).Methods("GET")
	admin.HandleFunc("/sync", handleAdminSync).Methods("POST")
	admin.HandleFunc("/sync/status", handleAdminSyncStatus).Methods("GET")
	admin.HandleFunc("/config", handleAdminConfig).Methods("GET", "POST")
	admin.HandleFunc("/groups", handleAdminListGroups).Methods("GET")
	admin.HandleFunc("/groups", handleAdminCreateGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}", handleAdminUpdateGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}/delete", handleAdminDeleteGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}/members", handleAdminGroupMembers).Methods("GET")
	admin.HandleFunc("/groups/{id}/members", handleAdminAddMember).Methods("POST")
	admin.HandleFunc("/groups/{id}/members/{contact_id}/delete", handleAdminRemoveMember).Methods("POST")
	admin.HandleFunc("/groups/{id}/contacts/search", handleAdminContactSearch).Methods("GET")
	admin.HandleFunc("/areas", handleAdminAreas).Methods("GET")
	admin.HandleFunc("/areas", handleAdminCreateArea).Methods("POST")
	admin.HandleFunc("/areas/{id}", handleAdminRenameArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/move", handleAdminMoveArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/delete", handleAdminDeleteArea).Methods("POST")
	admin.HandleFunc("/areas/{id}", handleAdminAreaDetail).Methods("GET")
	admin.HandleFunc("/areas/{id}/rules", handleAdminAddAreaRule).Methods("POST")
	admin.HandleFunc("/areas/{id}/rules/{rid}/delete", handleAdminDeleteAreaRule).Methods("POST")
	admin.HandleFunc("/ad/suggest", handleAdminADSuggest).Methods("GET")
	admin.HandleFunc("/scope/rules", handleAdminAddScopeRule).Methods("POST")
	admin.HandleFunc("/scope/rules/{rid}/delete", handleAdminDeleteScopeRule).Methods("POST")
	admin.HandleFunc("/scope/preview", handleAdminScopePreview).Methods("GET")
	admin.HandleFunc("/scope/apply", handleAdminScopeApply).Methods("POST")
	admin.HandleFunc("/group-categories", handleAdminGroupCategories).Methods("GET")
	admin.HandleFunc("/group-categories", handleAdminCreateGroupCategory).Methods("POST")
	admin.HandleFunc("/group-categories/{id}", handleAdminUpdateGroupCategory).Methods("POST")
	admin.HandleFunc("/group-categories/{id}/delete", handleAdminDeleteGroupCategory).Methods("POST")
	admin.HandleFunc("/local-contacts", handleAdminContacts).Methods("GET")
	admin.HandleFunc("/local-contacts", handleAdminCreateContact).Methods("POST")
	admin.HandleFunc("/local-contacts/{uid}", handleAdminUpdateContact).Methods("POST")
	admin.HandleFunc("/local-contacts/{uid}/delete", handleAdminDeleteContact).Methods("POST")
	admin.HandleFunc("/contacts/{uid}/override", handleAdminContactOverride).Methods("POST")
	admin.HandleFunc("/pbx", handleAdminPBX).Methods("GET")
	admin.HandleFunc("/pbx", handleAdminSavePBXConfig).Methods("POST")
	admin.HandleFunc("/pbx/sync", handleAdminSyncPBX).Methods("POST")
	admin.HandleFunc("/pbx/sync/status", handleAdminSyncPBXStatus).Methods("GET")
	admin.HandleFunc("/phone-directory", handleAdminPhoneDirectory).Methods("GET")
	admin.HandleFunc("/phone-directory", handleAdminSavePhoneDirectory).Methods("POST")
	admin.HandleFunc("/backup", handleAdminBackup).Methods("GET")
	admin.HandleFunc("/backup/create", handleAdminCreateBackup).Methods("POST")
	admin.HandleFunc("/backup/status", handleAdminBackupStatus).Methods("GET")
	admin.HandleFunc("/backup/download/{name}", handleAdminDownloadBackup).Methods("GET")
	admin.HandleFunc("/backup/{name}/delete", handleAdminDeleteBackup).Methods("POST")
	admin.HandleFunc("/backup/restore/start", handleAdminRestoreStart).Methods("POST")
	admin.HandleFunc("/backup/restore/chunk/{id}", handleAdminRestoreChunk).Methods("POST")
	admin.HandleFunc("/backup/restore/complete/{id}", handleAdminRestoreComplete).Methods("POST")

	// CardDAV server
	carddavServer := carddav.NewServer(db, cfg)
	carddav := r.PathPrefix("/carddav").Subrouter()
	carddav.PathPrefix("/").Handler(carddavServer.GetRouter())
	r.HandleFunc("/.well-known/carddav", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/carddav/", http.StatusMovedPermanently)
	})

	// Start server
	addr := fmt.Sprintf("%s:%s", cfg.ServerHost, cfg.ServerPort)
	log.Printf("[HTTP] Starting server on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("[HTTP] Server failed: %v", err)
	}
}

func ldapSyncWorker() {
	ticker := time.NewTicker(time.Duration(cfg.SyncIntervalHours) * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("[SYNC] Starting scheduled sync...")
		if err := runLDAPSync(ldap.SyncOptions{}, nil); err != nil {
			log.Printf("[SYNC] Failed: %v", err)
		}
		if result, err := pbx.SyncPBX(db, nil); err != nil {
			log.Printf("[PBX] Failed: %v", err)
		} else {
			lastPBXSync = time.Now()
			lastPBXSyncResult = result
		}
	}
}

// backupWorker esegue uno snapshot schedulato ogni BACKUP_INTERVAL_HOURS
// e applica subito dopo la retention GFS — stesso pattern di
// ldapSyncWorker, ma con un ticker indipendente (la cadenza di backup
// non ha motivo di essere legata a quella del sync LDAP/PBX).
func backupWorker() {
	ticker := time.NewTicker(time.Duration(cfg.BackupIntervalHours) * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("[BACKUP] Starting scheduled backup...")
		b, err := backup.CreateBackup(db.DB, backupDir(), false)
		if err != nil {
			log.Printf("[BACKUP] Scheduled backup failed: %v", err)
			continue
		}
		log.Printf("[BACKUP] Scheduled backup created: %s", b.Name)

		backups, err := backup.ListBackups(backupDir())
		if err != nil {
			log.Printf("[BACKUP] Failed to list backups for pruning: %v", err)
			continue
		}
		deleted, err := backup.PruneScheduled(backups, time.Now())
		if err != nil {
			log.Printf("[BACKUP] Pruning failed: %v", err)
			continue
		}
		if len(deleted) > 0 {
			log.Printf("[BACKUP] Pruned %d old scheduled backups", len(deleted))
		}
	}
}

// Middleware

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "rubrica-session")
		if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "rubrica-session")
		if admin, ok := session.Values["admin"].(bool); !ok || !admin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionAdminUsername returns the logged-in admin's username, or "" if
// the current request has no valid admin session. Used to render the
// shared rail (rail.html) the same way on public and admin pages: a
// logged-in admin sees the "Gestione" section and "Esci" everywhere, a
// visitor sees only "Pannello Admin".
func sessionAdminUsername(r *http.Request) string {
	session, _ := store.Get(r, "rubrica-session")
	auth, _ := session.Values["authenticated"].(bool)
	admin, _ := session.Values["admin"].(bool)
	if !auth || !admin {
		return ""
	}
	username, _ := session.Values["username"].(string)
	return username
}

// Public handlers

// railData raccoglie i dati richiesti da rail.html su ogni pagina che la
// include (pubblica o admin): conteggi/elenco Aree e stato di login.
func railData() map[string]interface{} {
	counts, err := db.CountByArea()
	if err != nil {
		log.Printf("[RAIL] Failed to count by area: %v", err)
		counts = map[string]int{}
	}
	total := 0
	for _, n := range counts {
		total += n
	}

	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[RAIL] Failed to list areas: %v", err)
	}
	subCounts, err := db.CountBySubArea()
	if err != nil {
		log.Printf("[RAIL] Failed to count by sub-area: %v", err)
		subCounts = map[string]int{}
	}

	return map[string]interface{}{
		"AreaCounts":    counts,
		"SubAreaCounts": subCounts,
		"AreaNodes":     database.BuildAreaNodes(areas),
		"Total":         total,
		"Areas":         areas,
		"AppVersion": AppVersion,
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)
	activeArea := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))
	activeSub := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sub")))

	prefix, _ := db.GetConfig(ldap.PrimaryNumberPrefixConfigKey)
	if prefix == "" {
		prefix = cfg.PrimaryNumberPrefix
	}
	prefixDigits := strings.ReplaceAll(prefix, "{ext}", "")

	data := railData()
	data["Messages"] = i18n.GetMessages(locale)
	data["Locale"] = locale
	data["ActiveArea"] = activeArea
	data["InitialGroup"] = activeArea
	data["ActiveSub"] = activeSub
	data["InitialSub"] = activeSub
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "contacts"
	data["PrefixHelperText"] = i18n.T(locale, "prefix_helper", prefixDigits)
	templates.ExecuteTemplate(w, "phonebook.html", data)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	groupFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))
	subFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sub")))

	var (
		results []*phonebook.ContactWithGroups
		err     error
	)

	if query == "" {
		results, err = pbService.ListContactsWithGroups(500, 0)
	} else {
		results, err = pbService.SearchContactsWithGroups(query, 50)
	}

	if err != nil {
		log.Printf("[SEARCH] Failed: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

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

	if groupFilter != "" {
		filtered := make([]*phonebook.ContactWithGroups, 0, len(results))
		for _, result := range results {
			if result.Contact.Area == groupFilter {
				filtered = append(filtered, result)
			}
		}
		results = filtered
	}

	if r.URL.Query().Get("only_number") == "on" {
		filtered := make([]*phonebook.ContactWithGroups, 0, len(results))
		for _, result := range results {
			if result.Contact.PrimaryNumber != "" || result.Contact.LDAPExt != "" {
				filtered = append(filtered, result)
			}
		}
		results = filtered
	}

	// Le chiamate di gruppo (group_numbers) compaiono in rubrica come i
	// contatti, sotto l'area "Uffici" (key riservata "uffici", vedi
	// migrate() in internal/database) oltre che nell'elenco non filtrato.
	// Un filtro testuale le riguarda comunque: cerca anche per
	// numero/nome del gruppo.
	var callGroups []*phonebook.GroupWithMembers
	if subFilter == "" && (groupFilter == "" || groupFilter == "uffici") {
		allGroups, err := pbService.ListGroupsWithMembers()
		if err != nil {
			log.Printf("[SEARCH] Failed to list call groups: %v", err)
		} else {
			// Un gruppo con soli membri disabled=true ha ActiveMembers()
			// vuoto (vedi phonebook.ActiveMembers) anche se il centralino
			// lo considera pieno — pbx.Filters.ExcludeEmptyGroups non lo
			// intercetta perché valuta i membri grezzi PBX, non lo stato
			// dominio, quindi va filtrato qui in rubrica pubblica.
			nonEmpty := allGroups[:0]
			for _, g := range allGroups {
				if len(g.ActiveMembers()) > 0 {
					nonEmpty = append(nonEmpty, g)
				}
			}
			allGroups = nonEmpty

			if query == "" {
				callGroups = allGroups
			} else {
				q := strings.ToLower(query)
				for _, g := range allGroups {
					if strings.Contains(strings.ToLower(g.Group.Name), q) || strings.Contains(g.Group.Number, q) {
						callGroups = append(callGroups, g)
					}
				}
			}
		}
	}

	var callGroupTree []*phonebook.GroupCategoryNode
	var uncategorizedGroups []*phonebook.GroupWithMembers
	if len(callGroups) > 0 {
		categories, err := db.ListGroupCategories()
		if err != nil {
			log.Printf("[SEARCH] Failed to list group categories: %v", err)
			uncategorizedGroups = callGroups
		} else {
			callGroupTree, uncategorizedGroups = phonebook.BuildGroupCategoryTree(callGroups, categories)
		}
	}

	// GroupMatches alimenta il dropdown "hover" sopra l'header della
	// colonna Chiamate di gruppo: solo con una query attiva (altrimenti
	// mostrerebbe i primi 10 gruppi sempre, anche a ricerca vuota), primi
	// 10 risultati già filtrati sopra su nome/numero.
	var groupMatches []*phonebook.GroupWithMembers
	if query != "" {
		groupMatches = callGroups
		if len(groupMatches) > 10 {
			groupMatches = groupMatches[:10]
		}
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Groups":              phonebook.GroupByDepartment(results),
		"CallGroups":          callGroups,
		"CallGroupTree":       callGroupTree,
		"UncategorizedGroups": uncategorizedGroups,
		"GroupMatches":        groupMatches,
		"Messages":            i18n.GetMessages(locale),
	}

	templates.ExecuteTemplate(w, "search_results.html", data)
}

func handleContacts(w http.ResponseWriter, r *http.Request) {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			page = parsed
		}
	}

	limit := 50
	offset := (page - 1) * limit

	contacts, err := pbService.ListContactsWithGroups(limit, offset)
	if err != nil {
		log.Printf("[CONTACTS] Failed to list: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(contacts)
}

func handleContactDetail(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]

	contactWithGroups, err := pbService.GetContactWithGroups(uid)
	if err != nil {
		log.Printf("[CONTACT] Failed to get %s: %v", uid, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if contactWithGroups == nil {
		http.NotFound(w, r)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Contact":  contactWithGroups.Contact,
		"Groups":   contactWithGroups.Groups,
		"Messages": i18n.GetMessages(locale),
	}

	templates.ExecuteTemplate(w, "contact_detail.html", data)
}

// handleContactDetailFragment serve solo il frammento (senza shell di
// pagina) per il popup modale in rubrica pubblica — stessa logica di
// handleContactDetail, la pagina dedicata resta per link diretti/condivisi.
func handleContactDetailFragment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]

	contactWithGroups, err := pbService.GetContactWithGroups(uid)
	if err != nil {
		log.Printf("[CONTACT] Failed to get %s: %v", uid, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if contactWithGroups == nil {
		http.NotFound(w, r)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Contact":  contactWithGroups.Contact,
		"Groups":   contactWithGroups.Groups,
		"Messages": i18n.GetMessages(locale),
	}
	templates.ExecuteTemplate(w, "contact_detail_fragment.html", data)
}

func handleExportVCard(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]

	contact, err := db.GetContact(uid)
	if err != nil {
		log.Printf("[EXPORT] Failed to get contact %s: %v", uid, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if contact == nil {
		http.NotFound(w, r)
		return
	}

	groups, _ := db.GetContactGroups(contact.ID)

	vcard := generateVCard(contact, groups)

	w.Header().Set("Content-Type", "text/vcard")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.vcf"`, uid))
	w.Write([]byte(vcard))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"status":    "ok",
		"version":   AppVersion,
		"last_sync": lastSync.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleVersion serve la versione corrente in esecuzione — usato dal poll
// lato client (rail.html) per accorgersi che il container è stato
// aggiornato e proporre un reload, senza dover controllare manualmente.
func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": AppVersion})
}

// Auth handlers

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if sessionAdminUsername(r) != "" {
			// Già loggato: mostrare di nuovo il form di login sembra un
			// logout inaspettato. Manda direttamente in admin.
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		locale := i18n.ResolveLocale(r)
		data := map[string]interface{}{
			"Messages": i18n.GetMessages(locale),
		}
		templates.ExecuteTemplate(w, "login.html", data)
		return
	}

	// POST
	username := r.FormValue("username")
	password := r.FormValue("password")

	isAuth, isAdmin, err := ldap.Authenticate(username, password, cfg)
	if err != nil {
		log.Printf("[AUTH] Error for user %s: %v", username, err)
		http.Redirect(w, r, "/login?error=1", http.StatusFound)
		return
	}

	if !isAuth {
		http.Redirect(w, r, "/login?error=1", http.StatusFound)
		return
	}

	if !isAdmin {
		http.Redirect(w, r, "/login?error=2", http.StatusFound)
		return
	}

	session, _ := store.Get(r, "rubrica-session")
	session.Values["authenticated"] = true
	session.Values["admin"] = isAdmin
	session.Values["username"] = username
	session.Save(r, w)

	http.Redirect(w, r, "/admin", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "rubrica-session")
	session.Values["authenticated"] = false
	session.Values["admin"] = false
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Admin handlers

func handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)

	prefix, _ := db.GetConfig(ldap.PrimaryNumberPrefixConfigKey)
	if prefix == "" {
		prefix = cfg.PrimaryNumberPrefix
	}

	data := railData()
	data["Messages"] = i18n.GetMessages(locale)
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin"
	data["LastSync"] = lastSync.Format("2006-01-02 15:04:05")
	data["PrimaryNumberPrefix"] = prefix

	templates.ExecuteTemplate(w, "admin.html", data)
}

// handleAdminSync avvia il sync LDAP manuale in background e ritorna subito
// il frammento di stato "in corso" — la UI fa polling su /admin/sync/status
// finché non risulta completato (successo o errore), invece di restare con
// un bottone senza alcun feedback per tutta la durata del sync.
func handleAdminSync(w http.ResponseWriter, r *http.Request) {
	startLDAPSync(r.FormValue("force") == "1")
	renderSyncStatus(w, r)
}

// handleAdminSyncStatus serve lo stato corrente per il polling htmx.
func handleAdminSyncStatus(w http.ResponseWriter, r *http.Request) {
	renderSyncStatus(w, r)
}

func renderSyncStatus(w http.ResponseWriter, r *http.Request) {
	st := ldapManualSync.snapshot()
	data := map[string]interface{}{
		"Running": st.Running,
		"Phase":   st.Phase,
		"Message": st.Message,
		"IsError": st.IsError,
		"LastSync": func() string {
			if lastSync.IsZero() {
				return "mai"
			}
			return lastSync.Format("2006-01-02 15:04:05")
		}(),
		"StatusURL": "/admin/sync/status",
		"OOBTarget": "last-sync-time",
		"OOBLabel":  "",
	}
	templates.ExecuteTemplate(w, "sync_status.html", data)
}

func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		prefix, _ := db.GetConfig(ldap.PrimaryNumberPrefixConfigKey)
		if prefix == "" {
			prefix = cfg.PrimaryNumberPrefix
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"primary_number_prefix": prefix,
		})
		return
	}

	// POST
	prefix := r.FormValue("primary_number_prefix")
	if err := db.SetConfig(ldap.PrimaryNumberPrefixConfigKey, prefix); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Salvato — verrà applicato al prossimo sync"))
}

// handleAdminListGroups serve la pagina "Etichette numero" completa
// (navigazione diretta) — le scritture (crea/elimina) continuano a
// ricevere solo il frammento via renderAdminGroups.
func handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := pbService.ListGroupsWithMembers()
	if err != nil {
		http.Error(w, "Failed to list groups", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	data["Groups"] = groups
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-groups"
	data["Messages"] = i18n.GetMessages(locale)

	templates.ExecuteTemplate(w, "admin_page_groups.html", data)
}

// renderAdminGroups re-renders the etichette numero table (admin_groups.html).
// Used both for the initial hx-get "load" and after create/delete, so the
// UI reflects the change instead of showing the handler's plain-text result.
func renderAdminGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := pbService.ListGroupsWithMembers()
	if err != nil {
		http.Error(w, "Failed to list groups", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Groups":   groups,
		"Messages": i18n.GetMessages(locale),
	}

	templates.ExecuteTemplate(w, "admin_groups.html", data)
}

func handleAdminCreateGroup(w http.ResponseWriter, r *http.Request) {
	group := &database.GroupNumber{
		Number:      r.FormValue("number"),
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
	}

	if err := db.CreateGroup(group); err != nil {
		http.Error(w, "Failed to create group", http.StatusInternalServerError)
		return
	}

	renderAdminGroups(w, r)
}

func handleAdminUpdateGroup(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	group := &database.GroupNumber{
		ID:          id,
		Number:      r.FormValue("number"),
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
	}

	if err := db.UpdateGroup(group); err != nil {
		http.Error(w, "Failed to update group", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Group updated"))
}

func handleAdminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	if err := db.DeleteGroup(id); err != nil {
		http.Error(w, "Failed to delete group", http.StatusInternalServerError)
		return
	}

	renderAdminGroups(w, r)
}

func handleAdminGroupMembers(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	groupWithMembers, err := pbService.GetGroupWithMembers(id)
	if err != nil {
		http.Error(w, "Failed to get group members", http.StatusInternalServerError)
		return
	}

	// Candidati di default: contatti con un numero già mostrati subito nel
	// picker, prima ancora di digitare una ricerca (click = aggiungi).
	candidates, err := db.ListContactsWithNumber(20)
	if err != nil {
		log.Printf("[ADMIN] Failed to list candidate contacts: %v", err)
		candidates = nil
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Group":    groupWithMembers.Group,
		"GroupID":  groupWithMembers.Group.ID,
		"Members":  groupWithMembers.Members,
		"Contacts": candidates,
		"Messages": i18n.GetMessages(locale),
	}

	templates.ExecuteTemplate(w, "admin_group_members.html", data)
}

func handleAdminAddMember(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	groupID, _ := strconv.ParseInt(vars["id"], 10, 64)
	contactID, _ := strconv.ParseInt(r.FormValue("contact_id"), 10, 64)

	if err := db.AddGroupMember(groupID, contactID); err != nil {
		http.Error(w, "Failed to add member", http.StatusInternalServerError)
		return
	}

	renderMembersList(w, r, groupID)
}

func handleAdminRemoveMember(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	groupID, _ := strconv.ParseInt(vars["id"], 10, 64)
	contactID, _ := strconv.ParseInt(vars["contact_id"], 10, 64)

	if err := db.RemoveGroupMember(groupID, contactID); err != nil {
		http.Error(w, "Failed to remove member", http.StatusInternalServerError)
		return
	}

	renderMembersList(w, r, groupID)
}

// renderMembersList re-renders just the #members-list fragment for a
// group, used after add/remove so the modal reflects the change instead
// of showing the handler's plain-text result.
func renderMembersList(w http.ResponseWriter, r *http.Request, groupID int64) {
	members, err := db.GetGroupMembers(groupID)
	if err != nil {
		http.Error(w, "Failed to list members", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"GroupID":  groupID,
		"Members":  members,
		"Messages": i18n.GetMessages(locale),
	}
	templates.ExecuteTemplate(w, "admin_members_list.html", data)
}

// handleAdminContactSearch returns a clickable dropdown of contacts
// matching the query, for the "cerca e aggiungi" member picker. Each
// result posts itself as a new group member via HTMX — no separate
// submit step, no raw contact ID typed by hand.
func handleAdminContactSearch(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	groupID := vars["id"]
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	var contacts []*database.Contact
	var err error
	if query == "" {
		// Campo svuotato (es. cancellato col backspace dopo una ricerca):
		// tornare ai candidati di default invece di lasciare il div
		// vuoto — altrimenti htmx ci scrive dentro una risposta vuota e
		// la lista sparisce fino al reload della pagina.
		contacts, err = db.ListContactsWithNumber(20)
	} else {
		contacts, err = db.SearchContacts(query, 8)
	}
	if err != nil {
		http.Error(w, "Search failed", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Contacts": contacts,
		"GroupID":  groupID,
	}
	templates.ExecuteTemplate(w, "admin_contact_search.html", data)
}

// slugify converte un nome area in una chiave stabile (minuscolo,
// solo lettere/numeri/underscore) usata come valore di contacts.area.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('_')
		}
	}
	return b.String()
}

// renderAdminGroupCategories re-renders solo il frammento (usato dopo
// crea/modifica/elimina via HTMX) — stesso pattern di renderAdminAreas.
func renderAdminGroupCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := db.ListGroupCategories()
	if err != nil {
		http.Error(w, "Failed to list group categories", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Categories": categories,
		"Messages":   i18n.GetMessages(locale),
	}
	templates.ExecuteTemplate(w, "admin_group_categories.html", data)
}

// handleAdminGroupCategories serve la pagina completa (navigazione
// diretta) — le scritture continuano a ricevere solo il frammento via
// renderAdminGroupCategories.
func handleAdminGroupCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := db.ListGroupCategories()
	if err != nil {
		http.Error(w, "Failed to list group categories", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	data["Categories"] = categories
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-group-categories"
	data["Messages"] = i18n.GetMessages(locale)

	templates.ExecuteTemplate(w, "admin_page_group_categories.html", data)
}

// parseOptionalGroupCategoryInt legge un campo form numerico opzionale
// (range_start/range_end/parent_id) — stringa vuota o non numerica torna
// nil, stesso comportamento di SetAreaRange per i range delle Aree.
func parseOptionalGroupCategoryInt(r *http.Request, field string) *int {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func handleAdminCreateGroupCategory(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminGroupCategories(w, r)
		return
	}
	key := slugify(name)
	if key == "" {
		renderAdminGroupCategories(w, r)
		return
	}

	c := &database.GroupCategory{
		Key:        key,
		Name:       name,
		RangeStart: parseOptionalGroupCategoryInt(r, "range_start"),
		RangeEnd:   parseOptionalGroupCategoryInt(r, "range_end"),
	}
	if pid := parseOptionalGroupCategoryInt(r, "parent_id"); pid != nil {
		id64 := int64(*pid)
		c.ParentID = &id64
	}
	if err := db.CreateGroupCategory(c); err != nil {
		log.Printf("[ADMIN] Failed to create group category %q: %v", name, err)
	}
	renderAdminGroupCategories(w, r)
}

func handleAdminUpdateGroupCategory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminGroupCategories(w, r)
		return
	}

	c := &database.GroupCategory{
		ID:         id,
		Name:       name,
		RangeStart: parseOptionalGroupCategoryInt(r, "range_start"),
		RangeEnd:   parseOptionalGroupCategoryInt(r, "range_end"),
	}
	if pid := parseOptionalGroupCategoryInt(r, "parent_id"); pid != nil {
		id64 := int64(*pid)
		c.ParentID = &id64
	}
	if err := db.UpdateGroupCategory(c); err != nil {
		log.Printf("[ADMIN] Failed to update group category %d: %v", id, err)
	}
	renderAdminGroupCategories(w, r)
}

func handleAdminDeleteGroupCategory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	if err := db.DeleteGroupCategory(id); err != nil {
		log.Printf("[ADMIN] Failed to delete group category %d: %v", id, err)
	}
	renderAdminGroupCategories(w, r)
}

// Contatti locali (manuali, extra-dominio) — CRUD separato dai contatti
// LDAP: sono l'unico posto dove si può creare/modificare/eliminare un
// contatto direttamente, invece di override su un dato sincronizzato.

// generateManualUID produce uno UID univoco per un contatto manuale, col
// prefisso "manual-" per non poter mai collidere con uno UID reale
// proveniente da LDAP (che non usa mai questo prefisso).
func generateManualUID(name string) string {
	base := "manual-" + slugify(name)
	if base == "manual-" {
		base = "manual-contatto"
	}
	uid := base
	for i := 2; ; i++ {
		existing, err := db.GetContact(uid)
		if err != nil || existing == nil {
			return uid
		}
		uid = fmt.Sprintf("%s-%d", base, i)
	}
}

// renderAdminContacts re-renders solo il frammento (usato dopo
// crea/modifica/elimina via HTMX).
func renderAdminContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := db.ListManualContacts()
	if err != nil {
		http.Error(w, "Failed to list contacts", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := manualContactsData(contacts)
	data["Contacts"] = contacts
	data["Messages"] = i18n.GetMessages(locale)
	templates.ExecuteTemplate(w, "admin_contacts.html", data)
}

// handleAdminContacts serve la pagina "Contatti locali" completa
// (navigazione diretta) — le scritture continuano a ricevere solo il
// frammento via renderAdminContacts.
func handleAdminContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := db.ListManualContacts()
	if err != nil {
		http.Error(w, "Failed to list contacts", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	for k, v := range manualContactsData(contacts) {
		data[k] = v
	}
	data["Contacts"] = contacts
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-contacts"
	data["Messages"] = i18n.GetMessages(locale)

	templates.ExecuteTemplate(w, "admin_page_contacts.html", data)
}

func handleAdminCreateContact(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("display_name"))
	if name == "" {
		renderAdminContacts(w, r)
		return
	}

	c := &database.Contact{
		UID:           generateManualUID(name),
		DisplayName:   name,
		Email:         strings.TrimSpace(r.FormValue("email")),
		PrimaryNumber: strings.TrimSpace(r.FormValue("primary_number")),
		Department:    strings.TrimSpace(r.FormValue("department")),
		Description:   strings.TrimSpace(r.FormValue("description")),
		Area:          strings.TrimSpace(r.FormValue("area")),
	}
	if err := db.CreateManualContact(c); err != nil {
		log.Printf("[ADMIN] Failed to create manual contact %q: %v", name, err)
	} else {
		saveManualSubAreas(r, c.UID, c.Area)
	}
	renderAdminContacts(w, r)
}

func handleAdminUpdateContact(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]
	name := strings.TrimSpace(r.FormValue("display_name"))
	if name == "" {
		renderAdminContacts(w, r)
		return
	}

	c := &database.Contact{
		UID:           uid,
		DisplayName:   name,
		Email:         strings.TrimSpace(r.FormValue("email")),
		PrimaryNumber: strings.TrimSpace(r.FormValue("primary_number")),
		Department:    strings.TrimSpace(r.FormValue("department")),
		Description:   strings.TrimSpace(r.FormValue("description")),
		Area:          strings.TrimSpace(r.FormValue("area")),
	}
	if err := db.UpdateManualContact(c); err != nil {
		log.Printf("[ADMIN] Failed to update manual contact %q: %v", uid, err)
	} else {
		saveManualSubAreas(r, uid, c.Area)
	}
	renderAdminContacts(w, r)
}

func handleAdminDeleteContact(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]

	if err := db.DeleteManualContact(uid); err != nil {
		log.Printf("[ADMIN] Failed to delete manual contact %q: %v", uid, err)
	}
	renderAdminContacts(w, r)
}

func handleAdminContactOverride(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]

	email := r.FormValue("email")
	primaryNumber := r.FormValue("primary_number")

	if err := db.UpdateContactOverride(uid, email, primaryNumber); err != nil {
		http.Error(w, "Failed to update contact", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Contact updated"))
}

// PBX admin handlers (config centralino, filtri, sync manuale)

func pbxData(r *http.Request) map[string]interface{} {
	url, user, pass := pbx.LoadPBXConfig(db)
	filters := pbx.LoadFilters(db)

	data := railData()
	data["Messages"] = i18n.GetMessages(i18n.ResolveLocale(r))
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-pbx"
	data["PBXURL"] = url
	data["PBXUser"] = user
	data["PBXHasPassword"] = pass != ""
	data["PBXExcludeUnnamed"] = filters.ExcludeUnnamed
	data["PBXExcludeInactiveGroups"] = filters.ExcludeInactiveGroups
	data["PBXExcludeEmptyGroups"] = filters.ExcludeEmptyGroups
	if lastPBXSync.IsZero() {
		data["LastPBXSync"] = "mai"
	} else {
		data["LastPBXSync"] = lastPBXSync.Format("2006-01-02 15:04:05")
	}
	data["UnmappedContacts"] = pbxUnmappedContacts()
	data["NameMismatches"] = lastPBXSyncResult.Mismatches
	data["ReclaimableExtensions"] = lastPBXSyncResult.Reclaimable
	data["DisabledGroupMembers"] = lastPBXSyncResult.DisabledGroupMembers
	data["EmptyActiveGroups"] = lastPBXSyncResult.EmptyActiveGroups
	if dups, err := db.ListDuplicateExtensions(); err != nil {
		log.Printf("[ADMIN] Failed to list duplicate extensions: %v", err)
	} else {
		data["DuplicateExtensions"] = dups
	}
	return data
}

// pbxUnmappedRow è un interno del centralino senza corrispondenza per
// numero in dominio, con un'eventuale corrispondenza per nome (nome
// centralino "LIKE" nome dominio, ordine parole e spazi non contano) — un
// dipendente può avere l'account AD/LDAP ma senza il telefono compilato:
// in quel caso non manca dal dominio, manca solo il numero nella sua
// scheda AD, e PossibleMatch lo segnala.
type pbxUnmappedRow struct {
	*database.Contact
	PossibleMatch string
}

// nameWords normalizza un nome per il confronto fuzzy: maiuscolo, diviso
// in parole, scartando token troppo corti (iniziali, articoli) per non
// generare falsi positivi.
func nameWords(name string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.Fields(strings.ToUpper(name)) {
		if len(w) >= 3 {
			words[w] = true
		}
	}
	return words
}

// pbxUnmappedContacts elenca i contatti source='pbx' e prova ad
// abbinarli per nome (non per interno, già escluso a monte dal sync) a un
// contatto source='ldap' esistente — "nome centralino LIKE nome dominio",
// indipendente dall'ordine delle parole (es. "Cognome Nome" vs
// "Nome Cognome").
func pbxUnmappedContacts() []pbxUnmappedRow {
	pbxContacts, err := db.ListPBXContacts()
	if err != nil {
		log.Printf("[ADMIN] Failed to list PBX contacts: %v", err)
		return nil
	}
	ldapContacts, err := db.ListContactsBySource("ldap")
	if err != nil {
		log.Printf("[ADMIN] Failed to list LDAP contacts for name matching: %v", err)
		ldapContacts = nil
	}

	rows := make([]pbxUnmappedRow, 0, len(pbxContacts))
	for _, c := range pbxContacts {
		row := pbxUnmappedRow{Contact: c}
		pbxWords := nameWords(c.DisplayName)
		if len(pbxWords) > 0 {
			for _, l := range ldapContacts {
				ldapWords := nameWords(l.DisplayName)
				shorter := len(pbxWords)
				if len(ldapWords) < shorter {
					shorter = len(ldapWords)
				}
				if shorter == 0 {
					continue
				}
				matched := 0
				for w := range pbxWords {
					if ldapWords[w] {
						matched++
					}
				}
				if matched >= shorter {
					row.PossibleMatch = l.DisplayName
					break
				}
			}
		}
		if row.PossibleMatch != "" {
			rows = append(rows, row)
		}
	}
	return rows
}

// renderPBX re-renders solo il frammento (usato dopo save/sync via HTMX).
func renderPBX(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "admin_pbx.html", pbxData(r))
}

// handleAdminPBX serve la pagina "Centralino" completa (navigazione diretta).
func handleAdminPBX(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "admin_page_pbx.html", pbxData(r))
}

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
	data["Enabled"] = cfg.LDAPServerEnabled
	data["Listening"] = ldapSrv != nil && ldapSrv.Ready()
	if ldapSrv != nil && ldapSrv.Err() != nil {
		data["ListenError"] = ldapSrv.Err().Error()
	}
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

// handleAdminSavePBXConfig salva url/utente/password/filtri del centralino.
// La password inviata vuota lascia invariata quella già salvata (non viene
// mai ri-mostrata in chiaro nel form). Le checkbox dei filtri non compaiono
// nel form POST quando deselezionate (comportamento standard HTML) — la
// loro assenza va quindi letta come "false", non come "campo mancante".
func handleAdminSavePBXConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	if err := db.SetConfig(pbx.PBXURLConfigKey, strings.TrimSpace(r.FormValue("pbx_url"))); err != nil {
		http.Error(w, "Failed to save PBX URL", http.StatusInternalServerError)
		return
	}
	if err := db.SetConfig(pbx.PBXUserConfigKey, strings.TrimSpace(r.FormValue("pbx_user"))); err != nil {
		http.Error(w, "Failed to save PBX user", http.StatusInternalServerError)
		return
	}
	if newPass := r.FormValue("pbx_pass"); newPass != "" {
		if err := db.SetConfig(pbx.PBXPassConfigKey, newPass); err != nil {
			http.Error(w, "Failed to save PBX password", http.StatusInternalServerError)
			return
		}
	}

	filters := pbx.Filters{
		ExcludeUnnamed:        r.FormValue("exclude_unnamed") != "",
		ExcludeInactiveGroups: r.FormValue("exclude_inactive_groups") != "",
		ExcludeEmptyGroups:    r.FormValue("exclude_empty_groups") != "",
	}
	if err := pbx.SaveFilters(db, filters); err != nil {
		http.Error(w, "Failed to save PBX filters", http.StatusInternalServerError)
		return
	}

	renderPBX(w, r)
}

// handleAdminSyncPBX avvia il sync PBX manuale in background e ritorna
// subito il frammento di stato "in corso" nel div dedicato #pbx-sync-status
// — senza toccare config/filtri già mostrati sulla pagina — la UI fa
// polling su /admin/pbx/sync/status finché non risulta completato. Prima
// era sincrono (bloccava la richiesta HTTP per l'intera durata dello
// screen-scraping) e in caso di errore non mostrava nulla, solo un log.
func handleAdminSyncPBX(w http.ResponseWriter, r *http.Request) {
	pbxManualSync.start()
	go func() {
		result, err := pbx.SyncPBX(db, pbxManualSync.setPhase)
		if err != nil {
			log.Printf("[PBX] Manual sync failed: %v", err)
		} else {
			lastPBXSync = time.Now()
			lastPBXSyncResult = result
			log.Printf("[PBX] Manual sync completed")
		}
		pbxManualSync.finish(err)
	}()

	renderSyncStatusPBX(w, r)
}

// handleAdminSyncPBXStatus serve lo stato corrente per il polling htmx.
func handleAdminSyncPBXStatus(w http.ResponseWriter, r *http.Request) {
	renderSyncStatusPBX(w, r)
}

func renderSyncStatusPBX(w http.ResponseWriter, r *http.Request) {
	st := pbxManualSync.snapshot()
	data := map[string]interface{}{
		"Running": st.Running,
		"Phase":   st.Phase,
		"Message": st.Message,
		"IsError": st.IsError,
		"LastSync": func() string {
			if lastPBXSync.IsZero() {
				return "mai"
			}
			return lastPBXSync.Format("2006-01-02 15:04:05")
		}(),
		"StatusURL": "/admin/pbx/sync/status",
		"OOBTarget": "pbx-last-sync-time",
		"OOBLabel":  "Ultimo sync riuscito: ",
	}
	templates.ExecuteTemplate(w, "sync_status.html", data)
}

// backupPageData raccoglie i dati comuni alla pagina /admin/backup e al
// suo frammento — la lista backup va ricaricata ad ogni render perché
// riflette lo stato del filesystem, non della DB.
func backupPageData(r *http.Request) map[string]interface{} {
	backups, err := backup.ListBackups(backupDir())
	if err != nil {
		log.Printf("[ADMIN] Failed to list backups: %v", err)
	}
	return map[string]interface{}{
		"Backups":  backups,
		"Messages": i18n.GetMessages(i18n.ResolveLocale(r)),
	}
}

func handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	data := railData()
	for k, v := range backupPageData(r) {
		data[k] = v
	}
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-backup"
	templates.ExecuteTemplate(w, "admin_page_backup.html", data)
}

func renderAdminBackup(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "admin_backup.html", backupPageData(r))
}

// handleAdminCreateBackup avvia un backup manuale in background e
// ritorna subito il frammento di stato "in corso" — stesso pattern
// async di handleAdminSyncPBX, riusa lo stesso sync_status.html.
func handleAdminCreateBackup(w http.ResponseWriter, r *http.Request) {
	backupManualSync.start()
	go func() {
		_, err := backup.CreateBackup(db.DB, backupDir(), true)
		if err != nil {
			log.Printf("[BACKUP] Manual backup failed: %v", err)
		} else {
			log.Printf("[BACKUP] Manual backup completed")
		}
		backupManualSync.finish(err)
	}()
	renderBackupSyncStatus(w, r)
}

func handleAdminBackupStatus(w http.ResponseWriter, r *http.Request) {
	renderBackupSyncStatus(w, r)
}

func renderBackupSyncStatus(w http.ResponseWriter, r *http.Request) {
	st := backupManualSync.snapshot()
	data := map[string]interface{}{
		"Running":   st.Running,
		"Phase":     st.Phase,
		"Message":   st.Message,
		"IsError":   st.IsError,
		"StatusURL": "/admin/backup/status",
	}
	if !st.Running {
		// Il backup manuale appena creato deve comparire nella lista
		// senza bisogno di un reload — swap-out-of-band sullo stesso
		// contenitore usato dal caricamento pagina.
		data["OOBBackupList"] = true
	}
	templates.ExecuteTemplate(w, "backup_sync_status.html", data)
}

// backupNamePattern valida {name} dagli URL di download/elimina — deve
// combaciare esattamente con lo schema di naming di backup.CreateBackup,
// altrimenti rifiuta (previene path traversal tipo "../../etc/passwd").
var backupNamePattern = regexp.MustCompile(`^(scheduled|manual)-\d{8}T\d{6}\.db$`)

func handleAdminDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !backupNamePattern.MatchString(name) {
		http.Error(w, "Nome file non valido", http.StatusBadRequest)
		return
	}
	path := filepath.Join(backupDir(), name)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeFile(w, r, path)
}

func handleAdminDeleteBackup(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !backupNamePattern.MatchString(name) {
		http.Error(w, "Nome file non valido", http.StatusBadRequest)
		return
	}
	if err := os.Remove(filepath.Join(backupDir(), name)); err != nil && !os.IsNotExist(err) {
		log.Printf("[ADMIN] Failed to delete backup %s: %v", name, err)
	}
	renderAdminBackup(w, r)
}

// restoreUploadIDPattern valida l'id di un upload di ripristino in corso
// (32 caratteri esadecimali, generato server-side in
// handleAdminRestoreStart) — usato per costruire il path del file
// temporaneo, va validato prima di ogni uso per non finire con un path
// traversal se qualcuno chiama gli endpoint chunk/complete a mano.
var restoreUploadIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func restoreUploadPath(id string) string {
	return cfg.DatabasePath + ".restore-upload-" + id
}

// handleAdminRestoreStart genera un id di upload e crea il file
// temporaneo vuoto che raccoglierà i chunk — il reverse proxy davanti a
// Rubrica ha un limite di 1MB per richiesta, quindi il ripristino non può
// essere un singolo upload multipart (vedi handleAdminRestoreChunk).
func handleAdminRestoreStart(w http.ResponseWriter, r *http.Request) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "Impossibile avviare il ripristino", http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(idBytes)

	f, err := os.Create(restoreUploadPath(id))
	if err != nil {
		http.Error(w, "Impossibile creare il file temporaneo", http.StatusInternalServerError)
		return
	}
	f.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"uploadId": id})
}

// handleAdminRestoreChunk accoda il corpo grezzo della richiesta (un
// pezzo di massimo 900KB, tagliato lato client — vedi admin_backup.html)
// al file temporaneo dell'upload. I chunk arrivano in ordine perché il
// client attende la risposta di un chunk prima di inviare il successivo,
// quindi un semplice append basta, nessun indice/riordino lato server.
func handleAdminRestoreChunk(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !restoreUploadIDPattern.MatchString(id) {
		http.Error(w, "Upload non valido", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 chunk, mai oltre il limite del proxy
	f, err := os.OpenFile(restoreUploadPath(id), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, "Upload non trovato o scaduto", http.StatusNotFound)
		return
	}
	defer f.Close()

	if _, err := io.Copy(f, r.Body); err != nil {
		http.Error(w, "Errore durante la scrittura del chunk", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminRestoreComplete valida il file riassemblato e, se è un DB
// SQLite valido, sostituisce il DB corrente e riavvia il processo —
// restart: unless-stopped in docker-compose.yml lo rialza da solo (vedi
// spec). La validazione avviene PRIMA di chiudere la connessione DB
// corrente: se fallisce, l'app continua a girare normalmente invece di
// essere già a metà di uno swap.
func handleAdminRestoreComplete(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !restoreUploadIDPattern.MatchString(id) {
		http.Error(w, "Upload non valido", http.StatusBadRequest)
		return
	}
	tmpPath := restoreUploadPath(id)

	if err := backup.ValidateSQLiteHeader(tmpPath); err != nil {
		os.Remove(tmpPath)
		http.Error(w, "Il file caricato non e' un database SQLite valido", http.StatusBadRequest)
		return
	}

	log.Printf("[BACKUP] Ripristino richiesto da %s, riavvio in corso...", sessionAdminUsername(r))
	db.Close()

	if err := os.Rename(tmpPath, cfg.DatabasePath); err != nil {
		log.Fatalf("[BACKUP] Failed to replace database file during restore: %v", err)
	}
	// File "gemelli" del vecchio DB (WAL/SHM) non corrispondono più al
	// file appena scritto — lasciarli porta a "attempt to write a
	// readonly database" al riavvio (vedi CLAUDE.md, gotcha incontrato
	// manualmente in questa stessa sessione con un docker cp).
	os.Remove(cfg.DatabasePath + "-wal")
	os.Remove(cfg.DatabasePath + "-shm")

	w.Write([]byte("Ripristino completato, il servizio si sta riavviando..."))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	os.Exit(0)
}

// Helper function for vCard generation (reused from carddav package logic)
func generateVCard(contact *database.Contact, groups []*database.GroupNumber) string {
	vcard := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:%s\r\n",
		contact.UID, contact.DisplayName)

	if contact.Email != "" {
		vcard += fmt.Sprintf("EMAIL;TYPE=INTERNET:%s\r\n", contact.Email)
	}

	if contact.PrimaryNumber != "" {
		vcard += fmt.Sprintf("TEL;TYPE=WORK,VOICE:%s\r\n", contact.PrimaryNumber)
	}

	for _, group := range groups {
		vcard += fmt.Sprintf("TEL;TYPE=WORK,X-GROUP:%s\r\n", group.Number)
		vcard += fmt.Sprintf("X-ABLABEL:Gruppo %s\r\n", group.Name)
	}

	if contact.Department != "" {
		vcard += fmt.Sprintf("ORG:%s\r\n", contact.Department)
	}

	vcard += fmt.Sprintf("REV:%s\r\n", contact.UpdatedAt.Format(time.RFC3339))
	vcard += "END:VCARD\r\n"

	return vcard
}
