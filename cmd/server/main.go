package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/carddav"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/i18n"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
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
	s.Running = false
	if err != nil {
		s.IsError = true
		s.Message = "Sync fallito: " + err.Error()
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

var (
	ldapManualSync = &syncStatus{}
	pbxManualSync  = &syncStatus{}
)

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

	// Perform initial sync
	go func() {
		if err := ldap.SyncContacts(db, cfg, nil); err != nil {
			log.Printf("[SYNC] Initial sync failed: %v", err)
		} else {
			lastSync = time.Now()
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
	admin.HandleFunc("/ou-mapping", handleAdminOUMapping).Methods("GET")
	admin.HandleFunc("/ou-mapping", handleAdminSaveOUMapping).Methods("POST")
	admin.HandleFunc("/ou-mapping/add", handleAdminAddOU).Methods("POST")
	admin.HandleFunc("/ou-mapping/{ou}/delete", handleAdminDeleteOU).Methods("POST")
	admin.HandleFunc("/areas", handleAdminAreas).Methods("GET")
	admin.HandleFunc("/areas", handleAdminCreateArea).Methods("POST")
	admin.HandleFunc("/areas/{id}", handleAdminRenameArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/delete", handleAdminDeleteArea).Methods("POST")
	admin.HandleFunc("/local-contacts", handleAdminContacts).Methods("GET")
	admin.HandleFunc("/local-contacts", handleAdminCreateContact).Methods("POST")
	admin.HandleFunc("/local-contacts/{uid}", handleAdminUpdateContact).Methods("POST")
	admin.HandleFunc("/local-contacts/{uid}/delete", handleAdminDeleteContact).Methods("POST")
	admin.HandleFunc("/contacts/{uid}/override", handleAdminContactOverride).Methods("POST")
	admin.HandleFunc("/pbx", handleAdminPBX).Methods("GET")
	admin.HandleFunc("/pbx", handleAdminSavePBXConfig).Methods("POST")
	admin.HandleFunc("/pbx/sync", handleAdminSyncPBX).Methods("POST")
	admin.HandleFunc("/pbx/sync/status", handleAdminSyncPBXStatus).Methods("GET")

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
		if err := ldap.SyncContacts(db, cfg, nil); err != nil {
			log.Printf("[SYNC] Failed: %v", err)
		} else {
			lastSync = time.Now()
		}
		if result, err := pbx.SyncPBX(db, nil); err != nil {
			log.Printf("[PBX] Failed: %v", err)
		} else {
			lastPBXSync = time.Now()
			lastPBXSyncResult = result
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

	return map[string]interface{}{
		"AreaCounts": counts,
		"Total":      total,
		"Areas":      areas,
		"AppVersion": AppVersion,
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)
	activeArea := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))

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
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "contacts"
	data["PrefixHelperText"] = i18n.T(locale, "prefix_helper", prefixDigits)
	templates.ExecuteTemplate(w, "phonebook.html", data)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	groupFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))

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
	if groupFilter == "" || groupFilter == "uffici" {
		allGroups, err := pbService.ListGroupsWithMembers()
		if err != nil {
			log.Printf("[SEARCH] Failed to list call groups: %v", err)
		} else if query == "" {
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

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Groups":     phonebook.GroupByDepartment(results),
		"CallGroups": callGroups,
		"Messages":   i18n.GetMessages(locale),
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
	ldapManualSync.start()
	go func() {
		err := ldap.SyncContacts(db, cfg, func(done, total int) {
			if total > 0 {
				ldapManualSync.setPhase(fmt.Sprintf("Elaborazione contatti: %d/%d", done, total))
			}
		})
		if err != nil {
			log.Printf("[SYNC] Manual sync failed: %v", err)
		} else {
			lastSync = time.Now()
			log.Printf("[SYNC] Manual sync completed")
		}
		ldapManualSync.finish(err)
	}()

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

// loadOUMapping legge il mapping salvato, o il default se non ancora
// configurato / JSON non valido.
func loadOUMapping() map[string]string {
	raw, _ := db.GetConfig(ldap.OUAreaMappingConfigKey)
	if raw == "" {
		return ldap.DefaultOUAreaMapping
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || len(stored) == 0 {
		return ldap.DefaultOUAreaMapping
	}
	return stored
}

// buildOUMappingData raccoglie i dati per la sezione mapping OU->Area:
// l'unione delle OU viste nei dati LDAP e di quelle già mappate a mano
// (una OU aggiunta manualmente per un contatto non ancora sincronizzato
// non deve sparire dalla lista solo perché nessun contatto la usa ancora).
func buildOUMappingData(r *http.Request) (map[string]interface{}, error) {
	dns, err := db.ListDistinctLDAPDNs()
	if err != nil {
		return nil, fmt.Errorf("failed to list OUs: %w", err)
	}

	mapping := loadOUMapping()

	ouSet := make(map[string]bool)
	for _, dn := range dns {
		if ou := ldap.ClassificationOU(dn); ou != "" {
			ouSet[ou] = true
		}
	}
	for ou := range mapping {
		ouSet[ou] = true
	}
	ous := make([]string, 0, len(ouSet))
	for ou := range ouSet {
		ous = append(ous, ou)
	}
	sort.Strings(ous)

	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[ADMIN] Failed to list areas: %v", err)
	}

	locale := i18n.ResolveLocale(r)
	return map[string]interface{}{
		"OUs":      ous,
		"Mapping":  mapping,
		"Areas":    areas,
		"Messages": i18n.GetMessages(locale),
	}, nil
}

// renderOUMapping re-renders solo il frammento (usato dopo save/add via
// HTMX, che sostituisce #ou-mapping-content in place).
func renderOUMapping(w http.ResponseWriter, r *http.Request) {
	data, err := buildOUMappingData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	templates.ExecuteTemplate(w, "admin_ou_mapping.html", data)
}

// handleAdminAddOU aggiunge una nuova OU (digitata a mano) al mapping,
// senza richiedere che sia già stata vista in un sync — serve per
// preparare in anticipo l'area di contatti extra-dominio non ancora
// presenti.
func handleAdminAddOU(w http.ResponseWriter, r *http.Request) {
	ou := strings.ToUpper(strings.TrimSpace(r.FormValue("new_ou")))
	area := strings.TrimSpace(r.FormValue("new_area"))
	if ou == "" {
		renderOUMapping(w, r)
		return
	}

	mapping := loadOUMapping()
	// copia: loadOUMapping può ritornare la mappa di default condivisa,
	// non va mutata in place.
	updated := make(map[string]string, len(mapping)+1)
	for k, v := range mapping {
		updated[k] = v
	}
	if area != "" {
		updated[ou] = area
	} else if _, exists := updated[ou]; !exists {
		updated[ou] = ""
	}

	raw, err := json.Marshal(updated)
	if err != nil {
		http.Error(w, "Failed to encode mapping", http.StatusInternalServerError)
		return
	}
	if err := db.SetConfig(ldap.OUAreaMappingConfigKey, string(raw)); err != nil {
		http.Error(w, "Failed to save mapping", http.StatusInternalServerError)
		return
	}

	renderOUMapping(w, r)
}

// handleAdminDeleteOU rimuove una singola associazione OU->Area,
// indipendentemente dalle altre righe della tabella — azione esplicita,
// non affidata al side-effect implicito di "seleziona Nessuna e salva
// tutto" che risultava poco affidabile/scopribile.
func handleAdminDeleteOU(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ou := vars["ou"]

	mapping := loadOUMapping()
	updated := make(map[string]string, len(mapping))
	for k, v := range mapping {
		if k == ou {
			continue
		}
		updated[k] = v
	}

	raw, err := json.Marshal(updated)
	if err != nil {
		http.Error(w, "Failed to encode mapping", http.StatusInternalServerError)
		return
	}
	if err := db.SetConfig(ldap.OUAreaMappingConfigKey, string(raw)); err != nil {
		http.Error(w, "Failed to save mapping", http.StatusInternalServerError)
		return
	}

	renderOUMapping(w, r)
}

// handleAdminOUMapping serve la pagina "Mapping OU" completa (navigazione
// diretta) — le scritture (save/add) continuano a ricevere solo il
// frammento via renderOUMapping.
func handleAdminOUMapping(w http.ResponseWriter, r *http.Request) {
	content, err := buildOUMappingData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := railData()
	for k, v := range content {
		data[k] = v
	}
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-ou-mapping"
	templates.ExecuteTemplate(w, "admin_page_ou_mapping.html", data)
}

// handleAdminSaveOUMapping salva il mapping OU->Area scelto dall'admin e
// avvia subito un resync in background, così l'effetto si vede senza
// dover aspettare il prossimo ciclo orario.
func handleAdminSaveOUMapping(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	mapping := make(map[string]string)
	for key, vals := range r.Form {
		if !strings.HasPrefix(key, "area_") || len(vals) == 0 {
			continue
		}
		ou := strings.TrimPrefix(key, "area_")
		if area := strings.TrimSpace(vals[0]); area != "" {
			mapping[ou] = area
		}
	}

	raw, err := json.Marshal(mapping)
	if err != nil {
		http.Error(w, "Failed to encode mapping", http.StatusInternalServerError)
		return
	}
	if err := db.SetConfig(ldap.OUAreaMappingConfigKey, string(raw)); err != nil {
		http.Error(w, "Failed to save mapping", http.StatusInternalServerError)
		return
	}

	go func() {
		if err := ldap.SyncContacts(db, cfg, nil); err != nil {
			log.Printf("[SYNC] Resync after OU mapping change failed: %v", err)
		} else {
			lastSync = time.Now()
		}
	}()

	renderOUMapping(w, r)
}

// renderAdminAreas re-renders the CRUD aree section (list + form nuova
// area + rinomina/elimina).
func renderAdminAreas(w http.ResponseWriter, r *http.Request) {
	areas, err := db.ListAreas()
	if err != nil {
		http.Error(w, "Failed to list areas", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Areas":    areas,
		"Messages": i18n.GetMessages(locale),
	}
	templates.ExecuteTemplate(w, "admin_areas.html", data)
}

// handleAdminAreas serve la pagina "Aree" completa (navigazione diretta)
// — le scritture (crea/rinomina/elimina) continuano a ricevere solo il
// frammento via renderAdminAreas.
func handleAdminAreas(w http.ResponseWriter, r *http.Request) {
	areas, err := db.ListAreas()
	if err != nil {
		http.Error(w, "Failed to list areas", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	data["Areas"] = areas
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-areas"
	data["Messages"] = i18n.GetMessages(locale)

	templates.ExecuteTemplate(w, "admin_page_areas.html", data)
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

func handleAdminCreateArea(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminAreas(w, r)
		return
	}
	key := slugify(name)
	if key == "" {
		renderAdminAreas(w, r)
		return
	}

	if err := db.CreateArea(&database.Area{Key: key, Name: name}); err != nil {
		log.Printf("[ADMIN] Failed to create area %q: %v", name, err)
	}
	renderAdminAreas(w, r)
}

func handleAdminRenameArea(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminAreas(w, r)
		return
	}

	if err := db.RenameArea(id, name); err != nil {
		log.Printf("[ADMIN] Failed to rename area %d: %v", id, err)
	}

	// Regola per range interno (opzionale): entrambi i campi vuoti =
	// nessuna regola (SetAreaRange con nil, nil la rimuove).
	var rangeStart, rangeEnd *int
	if v := strings.TrimSpace(r.FormValue("range_start")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rangeStart = &n
		}
	}
	if v := strings.TrimSpace(r.FormValue("range_end")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rangeEnd = &n
		}
	}
	if err := db.SetAreaRange(id, rangeStart, rangeEnd); err != nil {
		log.Printf("[ADMIN] Failed to set area range %d: %v", id, err)
	}

	renderAdminAreas(w, r)
}

func handleAdminDeleteArea(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	if err := db.DeleteArea(id); err != nil {
		log.Printf("[ADMIN] Failed to delete area %d: %v", id, err)
	}
	renderAdminAreas(w, r)
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
	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[ADMIN] Failed to list areas: %v", err)
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Contacts": contacts,
		"Areas":    areas,
		"Messages": i18n.GetMessages(locale),
	}
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
	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[ADMIN] Failed to list areas: %v", err)
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	data["Contacts"] = contacts
	data["Areas"] = areas
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
