package pbx

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
)

// Chiavi app_config per la configurazione PBX, editabile da /admin/pbx.
// Nessuna env var: questa è l'unica fonte di verità.
const (
	PBXURLConfigKey  = "pbx_url"
	PBXUserConfigKey = "pbx_user"
	PBXPassConfigKey = "pbx_pass"

	filterExcludeUnnamedConfigKey        = "pbx_exclude_unnamed"
	filterExcludeInactiveGroupsConfigKey = "pbx_exclude_inactive_groups"
	filterExcludeEmptyGroupsConfigKey    = "pbx_exclude_empty_groups"
)

// LoadPBXConfig legge url/utente/password da app_config (stringa vuota se
// non ancora configurato). Usata sia dal sync automatico che dalla pagina
// admin (per precompilare url/utente, mai la password).
func LoadPBXConfig(db *database.DB) (url, user, pass string) {
	url, _ = db.GetConfig(PBXURLConfigKey)
	user, _ = db.GetConfig(PBXUserConfigKey)
	pass, _ = db.GetConfig(PBXPassConfigKey)
	return url, user, pass
}

// Filters sono i filtri di esclusione configurabili da /admin/pbx: dati
// "spazzatura" del centralino che, di default, non vogliamo veder comparire
// in rubrica. Tutti true di default (vedi LoadFilters) — un admin può
// disattivarli singolarmente se preferisce vedere anche quei dati.
type Filters struct {
	ExcludeUnnamed        bool // contatti con nome placeholder tipo "<534>"
	ExcludeInactiveGroups bool // call group disattivi sul centralino
	ExcludeEmptyGroups    bool // call group senza destinatari
}

func boolConfig(db *database.DB, key string, fallback bool) bool {
	v, _ := db.GetConfig(key)
	if v == "" {
		return fallback
	}
	return v == "true"
}

func setBoolConfig(db *database.DB, key string, value bool) error {
	v := "false"
	if value {
		v = "true"
	}
	return db.SetConfig(key, v)
}

// LoadFilters legge i filtri da app_config. Ogni filtro non ancora
// impostato torna true (default "prudente": nascondi la spazzatura finché
// l'admin non chiede esplicitamente di vederla).
func LoadFilters(db *database.DB) Filters {
	return Filters{
		ExcludeUnnamed:        boolConfig(db, filterExcludeUnnamedConfigKey, true),
		ExcludeInactiveGroups: boolConfig(db, filterExcludeInactiveGroupsConfigKey, true),
		ExcludeEmptyGroups:    boolConfig(db, filterExcludeEmptyGroupsConfigKey, true),
	}
}

// SaveFilters salva i tre filtri in app_config.
func SaveFilters(db *database.DB, f Filters) error {
	if err := setBoolConfig(db, filterExcludeUnnamedConfigKey, f.ExcludeUnnamed); err != nil {
		return err
	}
	if err := setBoolConfig(db, filterExcludeInactiveGroupsConfigKey, f.ExcludeInactiveGroups); err != nil {
		return err
	}
	return setBoolConfig(db, filterExcludeEmptyGroupsConfigKey, f.ExcludeEmptyGroups)
}

// placeholderNameRe riconosce i nomi "segnaposto" che il centralino usa per
// gli interni senza un nome configurato, es. " <521>" o "<534>".
var placeholderNameRe = regexp.MustCompile(`^<\d+>$`)

func isPlaceholderName(callerID string) bool {
	return placeholderNameRe.MatchString(strings.TrimSpace(callerID))
}

// FilterPeers applica Filters.ExcludeUnnamed alla lista di peer.
func FilterPeers(peers []Peer, f Filters) []Peer {
	if !f.ExcludeUnnamed {
		return peers
	}
	var out []Peer
	for _, p := range peers {
		if isPlaceholderName(p.CallerID) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FilterCallGroups applica Filters.ExcludeInactiveGroups/ExcludeEmptyGroups
// alla lista di call group.
func FilterCallGroups(groups []CallGroup, f Filters) []CallGroup {
	var out []CallGroup
	for _, g := range groups {
		if f.ExcludeInactiveGroups && !g.Enabled {
			continue
		}
		if f.ExcludeEmptyGroups && len(g.Members) == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}

// SyncPBX esegue un giro completo di sync (login, fetch, filtra, applica).
// No-op silenzioso se l'URL non è ancora configurato da /admin/pbx —
// subsystem disattivo. Un errore di rete/login/parsing salta l'intero giro
// senza toccare i dati esistenti.
func SyncPBX(db *database.DB) error {
	url, user, pass := LoadPBXConfig(db)
	if url == "" {
		return nil
	}

	log.Printf("[PBX] Starting PBX sync...")

	client := NewClient(url)
	if err := client.Login(user, pass); err != nil {
		return fmt.Errorf("pbx login failed: %w", err)
	}

	peers, err := client.FetchPeers()
	if err != nil {
		return fmt.Errorf("pbx fetch peers failed: %w", err)
	}

	groups, err := client.FetchCallGroups()
	if err != nil {
		return fmt.Errorf("pbx fetch call groups failed: %w", err)
	}

	filters := LoadFilters(db)
	peers = FilterPeers(peers, filters)
	groups = FilterCallGroups(groups, filters)

	applied, err := ApplyPeers(db, peers, time.Now())
	if err != nil {
		return fmt.Errorf("pbx apply peers failed: %w", err)
	}

	if err := ApplyCallGroups(db, groups); err != nil {
		return fmt.Errorf("pbx apply call groups failed: %w", err)
	}

	log.Printf("[PBX] Sync completed: %d peers applied, %d call groups", applied, len(groups))
	return nil
}

// ApplyPeers upserta come contacts source='pbx' i peer non già coperti da
// un contatto source='ldap' (stesso ldap_ext), e soft-delete quelli
// scomparsi dal centralino in questo giro. Esportata per i test.
func ApplyPeers(db *database.DB, peers []Peer, syncTime time.Time) (int, error) {
	domainExts, err := db.ListDomainExtensions()
	if err != nil {
		return 0, err
	}
	inDomain := make(map[string]struct{}, len(domainExts))
	for _, e := range domainExts {
		inDomain[e] = struct{}{}
	}

	applied := 0
	for _, p := range peers {
		if _, ok := inDomain[p.Extension]; ok {
			continue
		}
		c := &database.Contact{
			UID:           "pbx-" + p.Extension,
			DisplayName:   p.CallerID,
			LDAPExt:       p.Extension,
			PrimaryNumber: p.Extension,
			Department:    "Centralino - non mappato",
			LastSync:      syncTime,
		}
		if err := db.UpsertPBXContact(c); err != nil {
			log.Printf("[PBX] skip peer %s: %v", p.Extension, err)
			continue
		}
		applied++
	}

	if _, err := db.SoftDeleteStalePBXContacts(syncTime); err != nil {
		return applied, err
	}
	return applied, nil
}

// ApplyCallGroups upserta i call group come group_numbers source='pbx' e
// ne sovrascrive sempre i membri (fonte di verità assoluta lato PBX). Un
// gruppo manuale preesistente con lo stesso interno viene sostituito,
// migrandone il nome come override se diverso da quello del centralino.
// Esportata per i test.
func ApplyCallGroups(db *database.DB, groups []CallGroup) error {
	seen := make(map[string]struct{}, len(groups))

	for _, g := range groups {
		seen[g.Extension] = struct{}{}

		name := g.Name
		nameOverride := false

		existing, err := db.GetGroupByNumber(g.Extension)
		if err != nil {
			return err
		}
		if existing != nil && existing.Source == "manual" {
			if existing.Name != g.Name {
				name = existing.Name
				nameOverride = true
			}
			if err := db.DeleteGroup(existing.ID); err != nil {
				return err
			}
		}

		row, err := db.UpsertPBXGroup(g.Extension, name, "", nameOverride)
		if err != nil {
			log.Printf("[PBX] skip call group %s: %v", g.Extension, err)
			continue
		}

		var memberIDs []int64
		for _, ext := range g.Members {
			contact, err := db.GetContactByExtension(ext)
			if err != nil {
				return err
			}
			if contact == nil {
				log.Printf("[PBX] call group %s: interno membro %s non trovato tra i contatti, ignorato", g.Extension, ext)
				continue
			}
			memberIDs = append(memberIDs, contact.ID)
		}
		if err := db.ReplaceGroupMembers(row.ID, memberIDs); err != nil {
			return err
		}
	}

	stale, err := db.ListGroupsBySource("pbx")
	if err != nil {
		return err
	}
	for _, g := range stale {
		if _, ok := seen[g.Number]; ok {
			continue
		}
		if err := db.DeleteGroup(g.ID); err != nil {
			return err
		}
	}
	return nil
}
