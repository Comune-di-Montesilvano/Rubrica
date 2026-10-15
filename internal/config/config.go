package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Server
	ServerHost string
	ServerPort string
	// Server LDAP per i telefoni (internal/ldapserver). La porta è la stessa
	// dentro e fuori dal container (come SERVER_PORT); per spegnerlo si usa
	// LDAPServerEnabled, non la porta (uno 0 nel mapping compose rompe lo stack).
	LDAPServerEnabled bool
	LDAPServerPort    string

	// LDAP
	LDAPHost           string
	LDAPBaseDN         string
	LDAPUserDNTemplate string
	LDAPBindDN         string
	LDAPBindPassword   string
	LDAPAdminGroup     string
	LDAPStartTLS       bool
	LDAPTLSSkipVerify  bool
	LDAPSearchFilter   string
	LDAPOnlyActive     bool
	// LDAPAllowedGroups è solo il seed una tantum delle regole di perimetro
	// (scope_rules, vedi internal/ldap/scope.go): letto al primo sync dopo
	// l'aggiornamento, poi ignorato. Il perimetro si gestisce da /admin/areas.
	LDAPAllowedGroups []string

	// Admin
	AdminUsers []string

	// Sync
	SyncIntervalHours int

	// Backup
	BackupIntervalHours int

	// Database
	DatabasePath string

	// Session
	SessionSecret string

	// Primary Number Template
	PrimaryNumberPrefix string
}

func Load() *Config {
	// Try to load .env file (optional)
	_ = godotenv.Load()

	return &Config{
		ServerHost:          getEnv("SERVER_HOST", "0.0.0.0"),
		ServerPort:          getEnv("SERVER_PORT", "8080"),
		LDAPServerEnabled:   getEnvBool("LDAP_SERVER_ENABLED", true),
		LDAPServerPort:      getEnv("LDAP_SERVER_PORT", "10389"),
		LDAPHost:            getEnv("LDAP_HOST", "ldap://localhost:389"),
		LDAPBaseDN:          getEnv("LDAP_BASE_DN", "dc=example,dc=com"),
		LDAPUserDNTemplate:  getEnv("LDAP_USER_DN_TEMPLATE", "uid={username},ou=users,dc=example,dc=com"),
		LDAPBindDN:          getEnv("LDAP_BIND_DN", ""),
		LDAPBindPassword:    getEnv("LDAP_BIND_PASSWORD", ""),
		LDAPAdminGroup:      getEnv("LDAP_ADMIN_GROUP", ""),
		LDAPStartTLS:        getEnvBool("LDAP_START_TLS", true),
		LDAPTLSSkipVerify:   getEnvBool("LDAP_TLS_SKIP_VERIFY", false),
		LDAPSearchFilter:    getEnv("LDAP_SEARCH_FILTER", "(|(objectClass=inetOrgPerson)(&(objectCategory=person)(objectClass=user)))"),
		LDAPOnlyActive:      getEnvBool("LDAP_ONLY_ACTIVE", true),
		LDAPAllowedGroups:   getEnvList("LDAP_ALLOWED_GROUPS", ";", []string{}),
		AdminUsers:          getEnvList("ADMIN_USERS", ";", []string{}),
		SyncIntervalHours:   getEnvInt("SYNC_INTERVAL_HOURS", 1),
		BackupIntervalHours: getEnvInt("BACKUP_INTERVAL_HOURS", 24),
		DatabasePath:        getEnv("DATABASE_PATH", "/data/ldavsync.db"),
		SessionSecret:       getEnv("SESSION_SECRET", "change-me-in-production"),
		PrimaryNumberPrefix: getEnv("PRIMARY_NUMBER_PREFIX_TEMPLATE", "0854321{ext}"),
	}
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
		log.Printf("[CONFIG] Invalid integer for %s: %s, using fallback %d", key, value, fallback)
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
		log.Printf("[CONFIG] Invalid boolean for %s: %s, using fallback %t", key, value, fallback)
	}
	return fallback
}

func getEnvList(key, separator string, fallback []string) []string {
	if value := os.Getenv(key); value != "" {
		parts := strings.Split(value, separator)
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return fallback
}
