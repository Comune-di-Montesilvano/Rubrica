package ldap

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/mirkochipdotcom/ldavsync/internal/config"
	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

// SyncContacts reads contacts from LDAP and updates the database
func SyncContacts(db *database.DB, cfg *config.Config) error {
	log.Printf("[SYNC] Starting LDAP sync...")

	conn, err := BindForSync(cfg)
	if err != nil {
		return fmt.Errorf("failed to bind for sync: %w", err)
	}
	defer conn.Close()

	searchFilter := buildSyncSearchFilter(cfg)

	// Search for all users
	searchRequest := ldap.NewSearchRequest(
		cfg.LDAPBaseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0,
		0,
		false,
		searchFilter,
		[]string{"uid", "sAMAccountName", "userPrincipalName", "mail", "displayName", "cn", "telephoneNumber", "physicalDeliveryOfficeName", "title", "description", "ou", "memberOf", "userAccountControl"},
		nil,
	)

	sr, err := conn.Search(searchRequest)
	if err != nil {
		if ldapErr, ok := err.(*ldap.Error); ok && ldapErr.ResultCode == ldap.LDAPResultOperationsError {
			return fmt.Errorf("LDAP search requires authenticated bind (AD 000004DC). Set LDAP_BIND_DN and LDAP_BIND_PASSWORD in .env: %w", err)
		}
		return fmt.Errorf("failed to search LDAP: %w", err)
	}

	syncTime := time.Now()
	count := 0
	totalEntries := 0
	filteredByGroup := 0
	filteredByDisabled := 0
	missingGroupInfo := 0
	observedGroups := map[string]int{}

	for _, entry := range sr.Entries {
		totalEntries++
		for _, dn := range entry.GetAttributeValues("memberOf") {
			if cn := extractCNFromDN(dn); cn != "" {
				observedGroups[cn]++
			}
		}
		if len(cfg.LDAPAllowedGroups) > 0 && len(entry.GetAttributeValues("memberOf")) == 0 {
			missingGroupInfo++
		}

		// Verifica se l'account è attivo (solo per AD)
		if cfg.LDAPOnlyActive {
			uacStr := entry.GetAttributeValue("userAccountControl")
			if uacStr != "" {
				uac, err := strconv.ParseInt(uacStr, 10, 64)
				if err == nil {
					// Bit 2 (0x02) = ACCOUNTDISABLE
					// Se il bit è impostato, l'account è disabilitato
					if (uac & 0x02) != 0 {
						filteredByDisabled++
						continue
					}
				}
			}
		}

		if !isEntryAllowedByGroups(entry, cfg.LDAPAllowedGroups) {
			filteredByGroup++
			continue
		}

		uid := entry.GetAttributeValue("uid")
		if uid == "" {
			uid = entry.GetAttributeValue("sAMAccountName")
		}
		if uid == "" {
			uid = entry.GetAttributeValue("userPrincipalName")
		}
		if uid == "" {
			continue
		}

		displayName := entry.GetAttributeValue("displayName")
		if displayName == "" {
			displayName = entry.GetAttributeValue("cn")
			if displayName == "" {
				displayName = uid
			}
		}

		email := entry.GetAttributeValue("mail")
		telephoneNumber := entry.GetAttributeValue("telephoneNumber")

		department := entry.GetAttributeValue("physicalDeliveryOfficeName")
		if department == "" {
			department = entry.GetAttributeValue("ou")
		}

		title := entry.GetAttributeValue("title")
		description := normalizeDescription(entry.GetAttributeValue("description"))

		// Generate primary number from template
		primaryNumber := generatePrimaryNumber(telephoneNumber, cfg)

		// Extract LDAP groups (CN from memberOf)
		var ldapGroups []string
		for _, dn := range entry.GetAttributeValues("memberOf") {
			if cn := extractCNFromDN(dn); cn != "" {
				ldapGroups = append(ldapGroups, cn)
			}
		}
		ldapGroupsStr := strings.Join(ldapGroups, ",")

		contact := &database.Contact{
			UID:           uid,
			DisplayName:   displayName,
			Email:         email,
			LDAPExt:       telephoneNumber,
			PrimaryNumber: primaryNumber,
			Department:    department,
			Title:         title,
			Description:   description,
			LDAPGroups:    ldapGroupsStr,
			LDAPDN:        entry.DN,
			Area:          deriveArea(entry.DN),
			LastSync:      syncTime,
		}

		if err := db.UpsertContact(contact); err != nil {
			log.Printf("[SYNC] Failed to upsert contact %s: %v", uid, err)
			continue
		}

		count++
	}

	log.Printf("[SYNC] Successfully synced %d contacts from LDAP", count)
	if cfg.LDAPOnlyActive {
		log.Printf("[SYNC] Disabled accounts filtered: %d of %d total entries", filteredByDisabled, totalEntries)
	}
	if len(cfg.LDAPAllowedGroups) > 0 {
		log.Printf("[SYNC] Group filter stats: total=%d filtered=%d missingMemberOf=%d allowedGroups=%v", totalEntries, filteredByGroup, missingGroupInfo, cfg.LDAPAllowedGroups)
		log.Printf("[SYNC] Observed memberOf CNs (top up to 30): %v", topObservedGroups(observedGroups, 30))
	}
	return nil
}

func buildSyncSearchFilter(cfg *config.Config) string {
	base := strings.TrimSpace(cfg.LDAPSearchFilter)
	if base == "" {
		base = "(objectClass=*)"
	}

	if cfg.LDAPOnlyActive {
		// AD active users: disabled bit (2) must be unset in userAccountControl
		base = "(&" + base + "(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"
	}

	return base
}

func isEntryAllowedByGroups(entry *ldap.Entry, allowedGroups []string) bool {
	if len(allowedGroups) == 0 {
		return true
	}

	allowed := make(map[string]struct{}, len(allowedGroups)*2)
	for _, g := range allowedGroups {
		for _, alias := range groupAliases(g) {
			allowed[alias] = struct{}{}
		}
	}

	for _, member := range entry.GetAttributeValues("memberOf") {
		for _, alias := range groupAliases(member) {
			if _, ok := allowed[alias]; ok {
				return true
			}
		}
	}

	return false
}

func groupAliases(group string) []string {
	g := strings.TrimSpace(strings.ToLower(group))
	if g == "" {
		return nil
	}

	aliases := map[string]struct{}{g: {}}

	if strings.Contains(g, "\\") {
		parts := strings.Split(g, "\\")
		aliases[parts[len(parts)-1]] = struct{}{}
	}

	if strings.Contains(g, "/") {
		parts := strings.Split(g, "/")
		aliases[parts[len(parts)-1]] = struct{}{}
	}

	if strings.HasPrefix(g, "cn=") {
		rest := g[3:]
		if idx := strings.Index(rest, ","); idx > 0 {
			aliases[rest[:idx]] = struct{}{}
		} else {
			aliases[rest] = struct{}{}
		}
	}

	out := make([]string, 0, len(aliases))
	for k := range aliases {
		out = append(out, k)
	}
	return out
}

// deriveArea maps an LDAP DN to the app's Area classification, based on
// the OU segment used by this AD structure (OU=INTERNI / OU=ESTERNI /
// OU=AREA_POLITICA sotto OU=COMUNE-MS). Ritorna "" se nessuna OU nota è
// trovata (account builtin/servizio come Guest, Administrator, krbtgt).
func deriveArea(dn string) string {
	d := strings.ToLower(dn)
	switch {
	case strings.Contains(d, "ou=interni"):
		return "interni"
	case strings.Contains(d, "ou=esterni"):
		return "esterni"
	case strings.Contains(d, "ou=area_politica"):
		return "politica"
	default:
		return ""
	}
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

func topObservedGroups(stats map[string]int, limit int) []string {
	if len(stats) == 0 {
		return []string{}
	}

	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(stats))
	for k, v := range stats {
		items = append(items, kv{k: k, v: v})
	}

	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].v > items[i].v {
				items[i], items[j] = items[j], items[i]
			}
		}
	}

	if limit > len(items) {
		limit = len(items)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, fmt.Sprintf("%s(%d)", items[i].k, items[i].v))
	}
	return out
}

// generatePrimaryNumber creates a full phone number from extension using the configured template
func generatePrimaryNumber(ext string, cfg *config.Config) string {
	if ext == "" {
		return ""
	}

	// Replace {ext} placeholder in template
	template := cfg.PrimaryNumberPrefix
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
