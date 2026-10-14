package ldap

import (
	"fmt"
	"log"
	"strings"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// ScopeMigratedConfigKey: flag app_config della conversione una tantum di
// LDAP_ALLOWED_GROUPS in regole di perimetro (scope_rules).
const ScopeMigratedConfigKey = "scope_rules_migrated"

// migrateScopeRules converte i CN (o DN) di LDAP_ALLOWED_GROUPS in regole
// adgroup, una sola volta. find risolve un CN nel DN del gruppo ("" se non
// esiste). Se la variabile è valorizzata ma nessun gruppo viene trovato
// ritorna errore senza scrivere il flag: senza regole entrerebbero tutti
// gli utenti AD.
func migrateScopeRules(db *database.DB, groups []string, find func(string) (string, error)) error {
	if v, _ := db.GetConfig(ScopeMigratedConfigKey); v == "1" {
		return nil
	}
	if len(groups) == 0 {
		return db.SetConfig(ScopeMigratedConfigKey, "1")
	}
	var rules []arearules.Rule
	for _, g := range groups {
		dn, err := find(g)
		if err != nil {
			// Errore di ricerca (AD irraggiungibile, timeout) ≠ gruppo
			// inesistente: riprova al prossimo sync invece di scartarlo
			// per sempre.
			return fmt.Errorf("migrazione perimetro: ricerca del gruppo %q fallita, riprovo al prossimo sync: %w", g, err)
		}
		if dn == "" {
			log.Printf("[SYNC] Perimetro: gruppo %q di LDAP_ALLOWED_GROUPS non trovato in AD, scartato", g)
			continue
		}
		r, err := arearules.Normalize(arearules.KindADGroup, "", dn)
		if err != nil {
			log.Printf("[SYNC] Perimetro: DN %q non valido, scartato", dn)
			continue
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return fmt.Errorf("migrazione perimetro: nessuno dei gruppi di LDAP_ALLOWED_GROUPS %v trovato in AD, sync interrotto e rubrica invariata", groups)
	}
	for _, r := range rules {
		if err := db.AddScopeRule(r); err != nil {
			return err
		}
		log.Printf("[SYNC] Perimetro: creata regola %s", arearules.Label(r))
	}
	return db.SetConfig(ScopeMigratedConfigKey, "1")
}

// groupFinder cerca il DN di un gruppo per CN; un valore che è già un DN
// viene restituito così com'è.
func groupFinder(conn *ldap.Conn, baseDN string) func(string) (string, error) {
	return func(cnOrDN string) (string, error) {
		if strings.Contains(cnOrDN, "=") {
			return cnOrDN, nil
		}
		req := ldap.NewSearchRequest(baseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
			"(&(objectClass=group)(cn="+ldap.EscapeFilter(cnOrDN)+"))", []string{"cn"}, nil)
		sr, err := conn.Search(req)
		if err != nil {
			return "", err
		}
		if len(sr.Entries) == 0 {
			return "", nil
		}
		return sr.Entries[0].DN, nil
	}
}
