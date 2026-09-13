package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/mirkochipdotcom/ldavsync/internal/carddav"
	"github.com/mirkochipdotcom/ldavsync/internal/config"
	"github.com/mirkochipdotcom/ldavsync/internal/database"
	"github.com/mirkochipdotcom/ldavsync/internal/i18n"
	"github.com/mirkochipdotcom/ldavsync/internal/ldap"
	"github.com/mirkochipdotcom/ldavsync/internal/phonebook"
)

var (
	AppVersion = "dev"
	templates  *template.Template
	store      *sessions.CookieStore
	db         *database.DB
	cfg        *config.Config
	pbService  *phonebook.Service
	lastSync   time.Time
)

func main() {
	log.Printf("[MAIN] Starting LdavSync %s", AppVersion)

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
		if err := ldap.SyncContacts(db, cfg); err != nil {
			log.Printf("[SYNC] Initial sync failed: %v", err)
		} else {
			lastSync = time.Now()
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

	// Auth routes
	r.HandleFunc("/login", handleLogin).Methods("GET", "POST")
	r.HandleFunc("/logout", handleLogout).Methods("POST")

	// Admin routes (protected)
	admin := r.PathPrefix("/admin").Subrouter()
	admin.Use(requireAuth)
	admin.Use(requireAdmin)
	admin.HandleFunc("", handleAdminDashboard).Methods("GET")
	admin.HandleFunc("/sync", handleAdminSync).Methods("POST")
	admin.HandleFunc("/config", handleAdminConfig).Methods("GET", "POST")
	admin.HandleFunc("/groups", handleAdminListGroups).Methods("GET")
	admin.HandleFunc("/groups", handleAdminCreateGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}", handleAdminUpdateGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}/delete", handleAdminDeleteGroup).Methods("POST")
	admin.HandleFunc("/groups/{id}/members", handleAdminGroupMembers).Methods("GET")
	admin.HandleFunc("/groups/{id}/members", handleAdminAddMember).Methods("POST")
	admin.HandleFunc("/groups/{id}/members/{contact_id}/delete", handleAdminRemoveMember).Methods("POST")
	admin.HandleFunc("/contacts/{uid}/override", handleAdminContactOverride).Methods("POST")

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
		if err := ldap.SyncContacts(db, cfg); err != nil {
			log.Printf("[SYNC] Failed: %v", err)
		} else {
			lastSync = time.Now()
		}
	}
}

// Middleware

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "ldavsync-session")
		if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "ldavsync-session")
		if admin, ok := session.Values["admin"].(bool); !ok || !admin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Public handlers

func handleIndex(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)

	counts, err := db.CountByArea()
	if err != nil {
		log.Printf("[INDEX] Failed to count by area: %v", err)
		counts = map[string]int{}
	}
	total := 0
	for _, n := range counts {
		total += n
	}

	data := map[string]interface{}{
		"Messages":   i18n.GetMessages(locale),
		"Locale":     locale,
		"AreaCounts": counts,
		"Total":      total,
	}
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

	if groupFilter == "interni" || groupFilter == "esterni" || groupFilter == "politica" {
		filtered := make([]*phonebook.ContactWithGroups, 0, len(results))
		for _, result := range results {
			if result.Contact.Area == groupFilter {
				filtered = append(filtered, result)
			}
		}
		results = filtered
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Groups":   phonebook.GroupByDepartment(results),
		"Messages": i18n.GetMessages(locale),
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

// Auth handlers

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
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

	session, _ := store.Get(r, "ldavsync-session")
	session.Values["authenticated"] = true
	session.Values["admin"] = isAdmin
	session.Values["username"] = username
	session.Save(r, w)

	http.Redirect(w, r, "/admin", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "ldavsync-session")
	session.Values["authenticated"] = false
	session.Values["admin"] = false
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Admin handlers

func handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	locale := i18n.ResolveLocale(r)
	session, _ := store.Get(r, "ldavsync-session")

	data := map[string]interface{}{
		"Messages": i18n.GetMessages(locale),
		"Username": session.Values["username"],
		"LastSync": lastSync.Format("2006-01-02 15:04:05"),
	}

	templates.ExecuteTemplate(w, "admin.html", data)
}

func handleAdminSync(w http.ResponseWriter, r *http.Request) {
	go func() {
		if err := ldap.SyncContacts(db, cfg); err != nil {
			log.Printf("[SYNC] Manual sync failed: %v", err)
		} else {
			lastSync = time.Now()
			log.Printf("[SYNC] Manual sync completed")
		}
	}()

	w.Write([]byte("Sync started"))
}

func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		prefix, _ := db.GetConfig("primary_number_prefix")
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
	if err := db.SetConfig("primary_number_prefix", prefix); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Config saved"))
}

func handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
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

	w.Write([]byte("Group created"))
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

	w.Write([]byte("Group deleted"))
}

func handleAdminGroupMembers(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	groupWithMembers, err := pbService.GetGroupWithMembers(id)
	if err != nil {
		http.Error(w, "Failed to get group members", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Group":    groupWithMembers.Group,
		"Members":  groupWithMembers.Members,
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

	w.Write([]byte("Member added"))
}

func handleAdminRemoveMember(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	groupID, _ := strconv.ParseInt(vars["id"], 10, 64)
	contactID, _ := strconv.ParseInt(vars["contact_id"], 10, 64)

	if err := db.RemoveGroupMember(groupID, contactID); err != nil {
		http.Error(w, "Failed to remove member", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Member removed"))
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
