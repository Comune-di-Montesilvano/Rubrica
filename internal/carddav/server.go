package carddav

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
)

// Server handles CardDAV protocol requests
type Server struct {
	db     *database.DB
	cfg    *config.Config
	router *mux.Router
}

// NewServer creates a new CardDAV server
func NewServer(db *database.DB, cfg *config.Config) *Server {
	s := &Server{
		db:     db,
		cfg:    cfg,
		router: mux.NewRouter(),
	}

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	s.router.HandleFunc("/carddav/", s.handleOptions).Methods("OPTIONS")
	s.router.HandleFunc("/carddav/", s.requireAuth(s.handlePropFind)).Methods("PROPFIND")
	s.router.HandleFunc("/carddav/", s.requireAuth(s.handleReport)).Methods("REPORT")
	s.router.HandleFunc("/carddav/{uid}.vcf", s.requireAuth(s.handleGetVCard)).Methods("GET")

	// OU-specific collections (e.g. /carddav/interni/)
	s.router.HandleFunc("/carddav/{book}/", s.handleOptions).Methods("OPTIONS")
	s.router.HandleFunc("/carddav/{book}/", s.requireAuth(s.handlePropFind)).Methods("PROPFIND")
	s.router.HandleFunc("/carddav/{book}/", s.requireAuth(s.handleReport)).Methods("REPORT")
	s.router.HandleFunc("/carddav/{book}/{uid}.vcf", s.requireAuth(s.handleGetVCard)).Methods("GET")
}

// GetRouter returns the router for mounting in main server
func (s *Server) GetRouter() *mux.Router {
	return s.router
}

// requireAuth middleware checks Basic Auth against LDAP
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			log.Printf("[CARDDAV] No Basic Auth header provided")
			w.Header().Set("WWW-Authenticate", `Basic realm="Rubrica CardDAV"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		log.Printf("[CARDDAV] Auth attempt for user: %s", username)

		isAuth, _, err := ldap.Authenticate(username, password, s.cfg)
		if err != nil {
			log.Printf("[CARDDAV] Auth error for user %s: %v", username, err)
			w.Header().Set("WWW-Authenticate", `Basic realm="Rubrica CardDAV"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if !isAuth {
			log.Printf("[CARDDAV] Auth failed for user %s", username)
			w.Header().Set("WWW-Authenticate", `Basic realm="Rubrica CardDAV"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		log.Printf("[CARDDAV] Auth successful for user %s", username)
		next(w, r)
	}
}

func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/carddav/", http.StatusMovedPermanently)
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, PROPFIND, REPORT, GET, HEAD")
	w.Header().Set("DAV", "1, 3, extended-mkcol, addressbook")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePropFind(w http.ResponseWriter, r *http.Request) {
	book := s.getRequestedBook(r)
	collectionPath := s.collectionPath(book)
	log.Printf("[CARDDAV] PROPFIND request - Book: %s, Method: %s, Depth: %s, Content-Type: %s", book, r.Method, r.Header.Get("Depth"), r.Header.Get("Content-Type"))

	// Return address book collection
	contacts, err := s.db.ListAllContacts(1000, 0)
	if err != nil {
		log.Printf("[CARDDAV] Failed to list contacts: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	contacts = s.filterContactsByBook(contacts, book)

	log.Printf("[CARDDAV] PROPFIND returning %d contacts for book %s", len(contacts), book)

	// Build XML manually for RFC 6352 compliance
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\r\n")
	sb.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + "\r\n")

	// Add collection itself
	sb.WriteString(`  <D:response>` + "\r\n")
	sb.WriteString(fmt.Sprintf(`    <D:href>%s</D:href>`+"\r\n", collectionPath))
	sb.WriteString(`    <D:propstat>` + "\r\n")
	sb.WriteString(`      <D:prop>` + "\r\n")
	sb.WriteString(`        <D:resourcetype>` + "\r\n")
	sb.WriteString(`          <D:collection/>` + "\r\n")
	sb.WriteString(`          <C:addressbook/>` + "\r\n")
	sb.WriteString(`        </D:resourcetype>` + "\r\n")
	sb.WriteString(fmt.Sprintf(`        <D:displayname>%s</D:displayname>`+"\r\n", s.bookDisplayName(book)))
	sb.WriteString(`        <D:getcontenttype>text/vcard; charset=utf-8</D:getcontenttype>` + "\r\n")
	sb.WriteString(`      </D:prop>` + "\r\n")
	sb.WriteString(`      <D:status>HTTP/1.1 200 OK</D:status>` + "\r\n")
	sb.WriteString(`    </D:propstat>` + "\r\n")
	sb.WriteString(`  </D:response>` + "\r\n")

	// Add each contact as a resource
	for _, contact := range contacts {
		sb.WriteString(`  <D:response>` + "\r\n")
		sb.WriteString(fmt.Sprintf(`    <D:href>%s%s.vcf</D:href>`+"\r\n", collectionPath, contact.UID))
		sb.WriteString(`    <D:propstat>` + "\r\n")
		sb.WriteString(`      <D:prop>` + "\r\n")
		sb.WriteString(`        <D:resourcetype/>` + "\r\n")
		sb.WriteString(fmt.Sprintf(`        <D:getetag>"%d"</D:getetag>`+"\r\n", contact.UpdatedAt.Unix()))
		sb.WriteString(`        <D:getcontenttype>text/vcard; charset=utf-8</D:getcontenttype>` + "\r\n")
		sb.WriteString(`      </D:prop>` + "\r\n")
		sb.WriteString(`      <D:status>HTTP/1.1 200 OK</D:status>` + "\r\n")
		sb.WriteString(`    </D:propstat>` + "\r\n")
		sb.WriteString(`  </D:response>` + "\r\n")
	}

	sb.WriteString(`</D:multistatus>` + "\r\n")

	xmlData := sb.String()

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", "1, 3, extended-mkcol, addressbook")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(xmlData)))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(xmlData))
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	// REPORT is used for advanced queries
	book := s.getRequestedBook(r)
	log.Printf("[CARDDAV] REPORT request received for book: %s", book)

	// Read the request body to see what Thunderbird is asking for
	body, err := io.ReadAll(r.Body)
	if err == nil && len(body) > 0 {
		log.Printf("[CARDDAV] REPORT body: %s", string(body)[:min(len(body), 500)])
	}

	// For now, return all contacts with full vCard data inline
	s.handleReportWithVCards(w, r, book)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Server) handleReportWithVCards(w http.ResponseWriter, r *http.Request, book string) {
	collectionPath := s.collectionPath(book)
	contacts, err := s.db.ListAllContacts(1000, 0)
	if err != nil {
		log.Printf("[CARDDAV] Failed to list contacts for REPORT: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	contacts = s.filterContactsByBook(contacts, book)

	log.Printf("[CARDDAV] REPORT returning %d contacts with vCard data for book %s", len(contacts), book)

	// Build XML manually for RFC 6352 compliance with vCard data inline
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\r\n")
	sb.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + "\r\n")

	// Add each contact with full vCard data
	for _, contact := range contacts {
		groups, _ := s.db.GetContactGroups(contact.ID)
		if groups == nil {
			groups = []*database.GroupNumber{}
		}
		vcard := generateVCard(contact, groups)

		sb.WriteString(`  <D:response>` + "\r\n")
		sb.WriteString(fmt.Sprintf(`    <D:href>%s%s.vcf</D:href>`+"\r\n", collectionPath, contact.UID))
		sb.WriteString(`    <D:propstat>` + "\r\n")
		sb.WriteString(`      <D:prop>` + "\r\n")
		sb.WriteString(fmt.Sprintf(`        <D:getetag>"%d"</D:getetag>`+"\r\n", contact.UpdatedAt.Unix()))

		// Include the vCard data inline
		sb.WriteString(`        <C:address-data>` + "\r\n")
		// Escape the vCard for XML
		escapedVCard := strings.ReplaceAll(vcard, "&", "&amp;")
		escapedVCard = strings.ReplaceAll(escapedVCard, "<", "&lt;")
		escapedVCard = strings.ReplaceAll(escapedVCard, ">", "&gt;")
		sb.WriteString(escapedVCard)
		sb.WriteString(`        </C:address-data>` + "\r\n")

		sb.WriteString(`      </D:prop>` + "\r\n")
		sb.WriteString(`      <D:status>HTTP/1.1 200 OK</D:status>` + "\r\n")
		sb.WriteString(`    </D:propstat>` + "\r\n")
		sb.WriteString(`  </D:response>` + "\r\n")
	}

	sb.WriteString(`</D:multistatus>` + "\r\n")

	xmlData := sb.String()

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", "1, 3, extended-mkcol, addressbook")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(xmlData)))
	w.WriteHeader(http.StatusMultiStatus)
	w.Write([]byte(xmlData))
}

func (s *Server) handleGetVCard(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	uid := vars["uid"]
	book := s.getRequestedBook(r)

	log.Printf("[CARDDAV] GET request for contact: %s (book: %s)", uid, book)

	contact, err := s.db.GetContact(uid)
	if err != nil {
		log.Printf("[CARDDAV] Failed to get contact %s: %v", uid, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if contact == nil {
		log.Printf("[CARDDAV] Contact not found: %s", uid)
		http.NotFound(w, r)
		return
	}

	// Enforce OU filter also on direct vCard fetch for collection-specific URLs.
	if !s.contactMatchesBook(contact, book) {
		log.Printf("[CARDDAV] Contact %s does not match book %s", uid, book)
		http.NotFound(w, r)
		return
	}

	// Get groups for this contact
	groups, err := s.db.GetContactGroups(contact.ID)
	if err != nil {
		log.Printf("[CARDDAV] Failed to get groups for contact %s: %v", uid, err)
		groups = []*database.GroupNumber{} // Continue without groups
	}

	vcard := generateVCard(contact, groups)

	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, contact.UpdatedAt.Unix()))
	w.Write([]byte(vcard))
}

func (s *Server) getRequestedBook(r *http.Request) string {
	book := strings.ToLower(strings.TrimSpace(mux.Vars(r)["book"]))
	if book == "" {
		return "all"
	}
	return book
}

func (s *Server) collectionPath(book string) string {
	if book == "" || book == "all" {
		return "/carddav/"
	}
	return fmt.Sprintf("/carddav/%s/", book)
}

func (s *Server) bookDisplayName(book string) string {
	if book == "" || book == "all" {
		return "Rubrica Contatti"
	}
	return fmt.Sprintf("Rubrica %s", strings.Title(book))
}

func (s *Server) filterContactsByBook(contacts []*database.Contact, book string) []*database.Contact {
	if book == "" || book == "all" {
		return contacts
	}

	filtered := make([]*database.Contact, 0, len(contacts))
	for _, c := range contacts {
		if s.contactMatchesBook(c, book) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func (s *Server) contactMatchesBook(contact *database.Contact, book string) bool {
	if book == "" || book == "all" {
		return true
	}

	dn := strings.ToLower(contact.LDAPDN)
	patterns := s.cfg.LDAPOUFilters[book]
	if len(patterns) == 0 {
		patterns = []string{"ou=" + book, "/" + book}
	}

	for _, pattern := range patterns {
		if strings.Contains(dn, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func generateVCard(contact *database.Contact, groups []*database.GroupNumber) string {
	var sb strings.Builder

	sb.WriteString("BEGIN:VCARD\r\n")
	sb.WriteString("VERSION:3.0\r\n")
	sb.WriteString(fmt.Sprintf("UID:%s\r\n", contact.UID))

	// Parse DisplayName into parts (format: "Cognome Nome" or "Nome")
	parts := strings.Fields(contact.DisplayName)
	var lastName, firstName string
	if len(parts) >= 2 {
		lastName = parts[len(parts)-1]
		firstName = strings.Join(parts[:len(parts)-1], " ")
	} else if len(parts) == 1 {
		lastName = parts[0]
		firstName = ""
	}

	// N is required in vCard 3.0: N:LastName;FirstName;;;
	sb.WriteString(fmt.Sprintf("N:%s;%s;;;\r\n", lastName, firstName))

	// FN is the formatted name
	sb.WriteString(fmt.Sprintf("FN:%s\r\n", contact.DisplayName))

	if contact.Email != "" {
		sb.WriteString(fmt.Sprintf("EMAIL;TYPE=INTERNET:%s\r\n", contact.Email))
	}

	if contact.PrimaryNumber != "" {
		sb.WriteString(fmt.Sprintf("TEL;TYPE=WORK,VOICE:%s\r\n", contact.PrimaryNumber))
	}

	// Add group numbers as additional phone numbers
	for _, group := range groups {
		sb.WriteString(fmt.Sprintf("TEL;TYPE=WORK,X-GROUP:%s\r\n", group.Number))
		sb.WriteString(fmt.Sprintf("X-ABLABEL:Gruppo %s\r\n", group.Name))
	}

	if contact.Department != "" {
		sb.WriteString(fmt.Sprintf("ORG:%s\r\n", contact.Department))
	}

	sb.WriteString(fmt.Sprintf("REV:%s\r\n", contact.UpdatedAt.Format(time.RFC3339)))
	sb.WriteString("END:VCARD\r\n")

	return sb.String()
}
