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
