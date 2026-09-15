package pbx

import (
	"fmt"
	"log"
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

// isPlaceholderName riconosce i nomi "segnaposto" che il centralino usa per
// gli interni senza un nome configurato. Sul dispositivo reale non è un
// bare "<534>" isolato come ipotizzato inizialmente, ma l'interno tra
// parentesi angolari ovunque nella stringa — es. "Interno <592>",
// "700 <700>", "prova<853>" — quindi il confronto è "contiene <extension>",
// non un match esatto sull'intera stringa.
// isNumericExtension è vero se ext è composto solo da cifre — gli interni
// veri sul centralino sono sempre numerici; trunk/gateway (es. "gw") non lo
// sono e non vanno mai trasformati in contatti.
func isNumericExtension(ext string) bool {
	if ext == "" {
		return false
	}
	for _, r := range ext {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isPlaceholderName(callerID, extension string) bool {
	if extension == "" {
		return false
	}
	return strings.Contains(callerID, "<"+extension+">")
}

// FilterPeers applica Filters.ExcludeUnnamed alla lista di peer.
func FilterPeers(peers []Peer, f Filters) []Peer {
	if !f.ExcludeUnnamed {
		return peers
	}
	var out []Peer
	for _, p := range peers {
		if isPlaceholderName(p.CallerID, p.Extension) {
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

// NameMismatch segnala un interno dove PBX e dominio non concordano sul
// nome della persona pur condividendo lo stesso interno — es. numero
// riassegnato senza aggiornare uno dei due sistemi, o refuso in uno dei
// due. Diverso dall'utility "numero mancante in AD": lì l'interno manca
// SOLO da un lato, qui è presente su entrambi ma con nomi incompatibili.
type NameMismatch struct {
	Extension  string
	DomainName string
	PBXName    string
}

// nameWords normalizza un nome per il confronto: maiuscolo, diviso in
// parole, scartando token troppo corti (iniziali, articoli) per non
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

// namesLookAlike è vero se i due nomi condividono almeno una parola
// significativa (ordine libero) — o se uno dei due non ha parole
// abbastanza lunghe da giudicare, nel qual caso non si segnala nulla
// piuttosto che generare un falso positivo.
func namesLookAlike(a, b string) bool {
	wa, wb := nameWords(a), nameWords(b)
	if len(wa) == 0 || len(wb) == 0 {
		return true
	}
	for w := range wa {
		if wb[w] {
			return true
		}
	}
	return false
}

// FindNameMismatches confronta ogni peer del centralino con l'omonimo
// interno di dominio (quando esiste) e segnala quelli il cui nome non ha
// nessuna parola in comune — va chiamato PRIMA di ApplyPeers, che scarta
// silenziosamente i peer già coperti da un interno di dominio: sono
// esattamente i candidati a questo controllo.
func FindNameMismatches(db *database.DB, peers []Peer) ([]NameMismatch, error) {
	domainNames, err := db.ListActiveLDAPExtensionNames()
	if err != nil {
		return nil, fmt.Errorf("failed to list domain extension names: %w", err)
	}
	var mismatches []NameMismatch
	for _, p := range peers {
		domainName, ok := domainNames[p.Extension]
		if !ok || p.CallerID == "" {
			continue
		}
		if !namesLookAlike(domainName, p.CallerID) {
			mismatches = append(mismatches, NameMismatch{
				Extension:  p.Extension,
				DomainName: domainName,
				PBXName:    p.CallerID,
			})
		}
	}
	return mismatches, nil
}

// ReclaimableExtension segnala un interno ancora presente e attivo sul
// centralino, con nome corrispondente (fuzzy) al titolare in AD/LDAP, ma
// quel titolare è disabled=true — la persona non c'è più/non usa più
// quell'interno: il numero è candidato per essere riassegnato.
type ReclaimableExtension struct {
	Extension  string
	DomainName string
	PBXName    string
}

// FindReclaimableExtensions confronta ogni peer del centralino con
// l'omonimo interno di un contatto di dominio DISABILITATO (se esiste) e
// segnala quelli il cui nome coincide (fuzzy) — a differenza di
// FindNameMismatches, qui il nome DEVE combaciare: è la conferma che si
// tratta della stessa persona, solo disattivata in AD ma il cui interno è
// ancora "occupato" sul centralino.
func FindReclaimableExtensions(db *database.DB, peers []Peer) ([]ReclaimableExtension, error) {
	disabledNames, err := db.ListDisabledLDAPExtensionNames()
	if err != nil {
		return nil, fmt.Errorf("failed to list disabled ldap extension names: %w", err)
	}
	var reclaimable []ReclaimableExtension
	for _, p := range peers {
		domainName, ok := disabledNames[p.Extension]
		if !ok || p.CallerID == "" {
			continue
		}
		if namesLookAlike(domainName, p.CallerID) {
			reclaimable = append(reclaimable, ReclaimableExtension{
				Extension:  p.Extension,
				DomainName: domainName,
				PBXName:    p.CallerID,
			})
		}
	}
	return reclaimable, nil
}

// DisabledGroupMember segnala un membro di un gruppo di chiamata del
// centralino il cui interno corrisponde a un contatto di dominio
// DISABILITATO — il gruppo continua a "chiamare" un interno il cui titolare
// non è più attivo in AD (account disabilitato, spesso anche fuori dai
// gruppi AD che pilotano LDAP_ALLOWED_GROUPS). Diverso da
// ReclaimableExtension: qui il confronto non è sul nome del peer SIP, ma
// sull'appartenenza effettiva al gruppo (membri già risolti da ApplyCallGroups
// via GetContactByExtension, che non filtra disabled).
type DisabledGroupMember struct {
	GroupNumber string
	GroupName   string
	Extension   string
	DomainName  string
}

// FindDisabledGroupMembers confronta i membri di ogni call group con
// l'anagrafica interni disabilitati in dominio e segnala quelli disattivi —
// va chiamato sui call group già filtrati (stesso punto di
// FindNameMismatches/FindReclaimableExtensions), prima di ApplyCallGroups.
// Un interno può comparire sia tra i disabilitati sia tra gli attivi
// (numero riassegnato senza ripulire il vecchio titolare in AD — vedi
// database.DuplicateExtension): in quel caso NON va segnalato, perché
// GetContactByExtension (usata da ApplyCallGroups per risolvere i membri)
// preferisce già il contatto attivo, quindi il gruppo squilla comunque sul
// titolare giusto.
func FindDisabledGroupMembers(db *database.DB, groups []CallGroup) ([]DisabledGroupMember, error) {
	disabledNames, err := db.ListDisabledLDAPExtensionNames()
	if err != nil {
		return nil, fmt.Errorf("failed to list disabled ldap extension names: %w", err)
	}
	activeNames, err := db.ListActiveLDAPExtensionNames()
	if err != nil {
		return nil, fmt.Errorf("failed to list active ldap extension names: %w", err)
	}
	var flagged []DisabledGroupMember
	for _, g := range groups {
		for _, ext := range g.Members {
			domainName, ok := disabledNames[ext]
			if !ok {
				continue
			}
			if _, alsoActive := activeNames[ext]; alsoActive {
				continue
			}
			flagged = append(flagged, DisabledGroupMember{
				GroupNumber: g.Extension,
				GroupName:   g.Name,
				Extension:   ext,
				DomainName:  domainName,
			})
		}
	}
	return flagged, nil
}

// EmptyActiveGroup segnala un gruppo di chiamata con membri lato centralino
// ma nessuno di essi attivo in dominio — il gruppo risulta "0 membri" nella
// rubrica pubblica (che nasconde i membri disabled, vedi ActiveMembers in
// handleSearch) pur non essendo un gruppo genuinamente vuoto: va sistemato
// (membri da riassegnare), non è un errore transitorio di sync.
type EmptyActiveGroup struct {
	GroupNumber string
	GroupName   string
}

// FindEmptyActiveGroups interroga il DB (dopo ApplyCallGroups, quindi sui
// membri già risolti e persistiti) per i gruppi con membri ma nessuno
// attivo — a differenza delle altre diagnostiche in questo file, non lavora
// sui dati grezzi del centralino: serve lo stato applicato in
// group_members per sapere quali contact_id sono davvero disabled.
func FindEmptyActiveGroups(db *database.DB) ([]EmptyActiveGroup, error) {
	groups, err := db.ListEmptyActiveGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to list empty active groups: %w", err)
	}
	empty := make([]EmptyActiveGroup, 0, len(groups))
	for _, g := range groups {
		empty = append(empty, EmptyActiveGroup{GroupNumber: g.Number, GroupName: g.Name})
	}
	return empty, nil
}

// SyncResult raccoglie le diagnostiche calcolate durante un giro di sync,
// oltre all'applicazione vera e propria dei dati — mostrate dalla pagina
// admin /admin/pbx per aiutare a tenere allineati centralino e dominio.
type SyncResult struct {
	Mismatches           []NameMismatch
	Reclaimable          []ReclaimableExtension
	DisabledGroupMembers []DisabledGroupMember
	EmptyActiveGroups    []EmptyActiveGroup
}

// SyncPBX esegue un giro completo di sync (login, fetch, filtra, applica).
// No-op silenzioso se l'URL non è ancora configurato da /admin/pbx —
// subsystem disattivo. Un errore di rete/login/parsing salta l'intero giro
// senza toccare i dati esistenti. onPhase (opzionale, nil se non serve) è
// chiamato ad ogni fase — non c'è una percentuale reale da riportare (lo
// screen-scraping è poche richieste HTTP, non un loop su tanti elementi),
// ma un giro può comunque richiedere secondi se il centralino è lento a
// rispondere: onPhase dà alla UI qualcosa da mostrare invece di un bottone
// "appeso" senza feedback.
func SyncPBX(db *database.DB, onPhase func(phase string)) (SyncResult, error) {
	if onPhase == nil {
		onPhase = func(phase string) {}
	}
	url, user, pass := LoadPBXConfig(db)
	if url == "" {
		return SyncResult{}, nil
	}

	log.Printf("[PBX] Starting PBX sync...")

	onPhase("Accesso al centralino...")
	client := NewClient(url)
	if err := client.Login(user, pass); err != nil {
		return SyncResult{}, fmt.Errorf("pbx login failed: %w", err)
	}

	onPhase("Recupero interni...")
	peers, err := client.FetchPeers()
	if err != nil {
		return SyncResult{}, fmt.Errorf("pbx fetch peers failed: %w", err)
	}

	onPhase("Recupero gruppi di chiamata...")
	groups, err := client.FetchCallGroups()
	if err != nil {
		return SyncResult{}, fmt.Errorf("pbx fetch call groups failed: %w", err)
	}

	filters := LoadFilters(db)
	peers = FilterPeers(peers, filters)
	groups = FilterCallGroups(groups, filters)

	// I confronti nome PBX/dominio vanno fatti sui peer ancora "grezzi"
	// (già filtrati dai placeholder, ma prima di ApplyPeers): ApplyPeers
	// scarta silenziosamente qualunque peer il cui interno è già coperto
	// da un contatto di dominio (attivo o disabled), che è esattamente
	// l'insieme su cui ha senso questi controlli.
	result := SyncResult{}
	if result.Mismatches, err = FindNameMismatches(db, peers); err != nil {
		log.Printf("[PBX] Failed to compute name mismatches: %v", err)
	}
	if result.Reclaimable, err = FindReclaimableExtensions(db, peers); err != nil {
		log.Printf("[PBX] Failed to compute reclaimable extensions: %v", err)
	}
	if result.DisabledGroupMembers, err = FindDisabledGroupMembers(db, groups); err != nil {
		log.Printf("[PBX] Failed to compute disabled group members: %v", err)
	}

	onPhase("Applicazione dati...")
	applied, err := ApplyPeers(db, peers, time.Now())
	if err != nil {
		return result, fmt.Errorf("pbx apply peers failed: %w", err)
	}

	if err := ApplyCallGroups(db, groups); err != nil {
		return result, fmt.Errorf("pbx apply call groups failed: %w", err)
	}

	if result.EmptyActiveGroups, err = FindEmptyActiveGroups(db); err != nil {
		log.Printf("[PBX] Failed to compute empty active groups: %v", err)
	}

	log.Printf("[PBX] Sync completed: %d peers applied, %d call groups, %d name mismatches, %d reclaimable extensions",
		applied, len(groups), len(result.Mismatches), len(result.Reclaimable))
	return result, nil
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

	// Regole area-per-range (vedi Area.RangeStart/RangeEnd): un peer non
	// altrimenti mappato il cui interno cade in un range assegnato prende
	// quella come Area — e la usa anche come Department, altrimenti
	// resterebbe con solo il generico "Centralino - non mappato".
	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[PBX] Failed to list areas for range matching: %v", err)
	}

	applied := 0
	for _, p := range peers {
		if _, ok := inDomain[p.Extension]; ok {
			continue
		}
		// I trunk/gateway del centralino (es. extension "gw") non sono
		// persone: compaiono tra i peer SIP ma non hanno un interno
		// numerico reale — mai contatti, altrimenti finiscono nei
		// risultati del filtro "solo numero" come voce senza nome.
		if !isNumericExtension(p.Extension) {
			continue
		}
		area, department := "", "Centralino - non mappato"
		if a := database.MatchExtensionRange(p.Extension, areas); a != nil {
			area = a.Key
			department = a.Name
		}
		c := &database.Contact{
			UID:           "pbx-" + p.Extension,
			DisplayName:   p.CallerID,
			LDAPExt:       p.Extension,
			PrimaryNumber: p.Extension,
			Department:    department,
			Area:          area,
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

	// Regole area-per-range (vedi Area.RangeStart/RangeEnd): un gruppo del
	// centralino il cui interno cade in un range assegnato prende quella
	// come Area — vedi anche ApplyPeers per lo stesso meccanismo sui
	// contatti.
	areas, err := db.ListAreas()
	if err != nil {
		log.Printf("[PBX] Failed to list areas for range matching: %v", err)
	}

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

		area := ""
		if a := database.MatchExtensionRange(g.Extension, areas); a != nil {
			area = a.Key
		}

		row, err := db.UpsertPBXGroup(g.Extension, name, "", nameOverride, area)
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
