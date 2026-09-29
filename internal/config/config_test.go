package config

import "testing"

func TestLoadLDAPServerDefaults(t *testing.T) {
	t.Setenv("LDAP_SERVER_PORT", "")
	t.Setenv("LDAP_SERVER_ENABLED", "")
	cfg := Load()
	if cfg.LDAPServerPort != "10389" {
		t.Errorf("LDAPServerPort = %q, atteso 10389", cfg.LDAPServerPort)
	}
	if !cfg.LDAPServerEnabled {
		t.Error("LDAPServerEnabled = false, atteso true di default")
	}
}

func TestLoadLDAPServerFromEnv(t *testing.T) {
	t.Setenv("LDAP_SERVER_PORT", "20389")
	t.Setenv("LDAP_SERVER_ENABLED", "false")
	cfg := Load()
	if cfg.LDAPServerPort != "20389" {
		t.Errorf("LDAPServerPort = %q, atteso 20389", cfg.LDAPServerPort)
	}
	if cfg.LDAPServerEnabled {
		t.Error("LDAPServerEnabled = true, atteso false da env")
	}
}
