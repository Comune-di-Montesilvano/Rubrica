package ldapserver

import (
	"fmt"
	"strings"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/go-ldap/ldap/v3"
)

// Chiavi app_config del server LDAP: nessuna env var, si editano da
// /admin/phone-directory (come la config del centralino).
const (
	BaseDNConfigKey       = "ldapsrv_base_dn"
	BindDNConfigKey       = "ldapsrv_bind_dn"
	BindPasswordConfigKey = "ldapsrv_bind_password"

	DefaultBaseDN = "dc=rubrica,dc=local"
	DefaultBindDN = "cn=telefoni,dc=rubrica,dc=local"
)

// Settings è la configurazione corrente del server LDAP. Letta dal DB a
// ogni bind/search: una modifica da admin vale subito, senza riavvio.
type Settings struct {
	BaseDN       string
	BindDN       string
	BindPassword string
}

// LoadSettings legge la config da app_config, con i default per i DN.
// BindPassword vuota = nessuna password configurata (ogni bind rifiutato).
func LoadSettings(db *database.DB) Settings {
	st := Settings{BaseDN: DefaultBaseDN, BindDN: DefaultBindDN}
	if v, _ := db.GetConfig(BaseDNConfigKey); v != "" {
		st.BaseDN = v
	}
	if v, _ := db.GetConfig(BindDNConfigKey); v != "" {
		st.BindDN = v
	}
	st.BindPassword, _ = db.GetConfig(BindPasswordConfigKey)
	return st
}

// SaveSettings valida e salva la config. DN vuoti tornano al default; la
// password vuota lascia invariata quella salvata (mai ri-mostrata nel form).
// Se un DN non è valido non salva nulla.
func SaveSettings(db *database.DB, baseDN, bindDN, newPassword string) error {
	baseDN = strings.TrimSpace(baseDN)
	if baseDN == "" {
		baseDN = DefaultBaseDN
	}
	bindDN = strings.TrimSpace(bindDN)
	if bindDN == "" {
		bindDN = DefaultBindDN
	}
	if _, err := ldap.ParseDN(baseDN); err != nil {
		return fmt.Errorf("base DN non valido: %w", err)
	}
	if _, err := ldap.ParseDN(bindDN); err != nil {
		return fmt.Errorf("bind DN non valido: %w", err)
	}
	if err := db.SetConfig(BaseDNConfigKey, baseDN); err != nil {
		return err
	}
	if err := db.SetConfig(BindDNConfigKey, bindDN); err != nil {
		return err
	}
	if newPassword != "" {
		return db.SetConfig(BindPasswordConfigKey, newPassword)
	}
	return nil
}
