package ldap

import (
	"crypto/tls"
	"fmt"
	"log"
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
)

// Authenticate checks LDAP credentials and returns authentication status and admin status
func Authenticate(username, password string, cfg *config.Config) (isAuthenticated bool, isAdmin bool, err error) {
	if username == "" || password == "" {
		return false, false, fmt.Errorf("username and password are required")
	}

	// Connect to LDAP
	conn, err := ldap.DialURL(cfg.LDAPHost)
	if err != nil {
		return false, false, fmt.Errorf("failed to connect to LDAP: %w", err)
	}
	defer conn.Close()

	// Start TLS if configured and using ldap:// (not ldaps://)
	if cfg.LDAPStartTLS && strings.HasPrefix(cfg.LDAPHost, "ldap://") {
		err = conn.StartTLS(&tls.Config{InsecureSkipVerify: cfg.LDAPTLSSkipVerify})
		if err != nil {
			return false, false, fmt.Errorf("failed to start TLS: %w", err)
		}
	}

	// Build user DN from template
	userDN := buildUserBindDN(cfg.LDAPUserDNTemplate, username)

	// Attempt to bind with user credentials
	err = conn.Bind(userDN, password)
	if err != nil {
		// Authentication failed
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to bind: %w", err)
	}

	// Authentication successful
	isAuthenticated = true

	// Check admin status
	isAdmin = isUserAdmin(username, cfg)

	return isAuthenticated, isAdmin, nil
}

// isUserAdmin checks if user is in admin list
func isUserAdmin(username string, cfg *config.Config) bool {
	for _, admin := range cfg.AdminUsers {
		if strings.EqualFold(admin, username) {
			return true
		}
	}
	return false
}

// BindForSync creates an LDAP connection for synchronization operations
func BindForSync(cfg *config.Config) (*ldap.Conn, error) {
	// Connect to LDAP
	conn, err := ldap.DialURL(cfg.LDAPHost)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to LDAP: %w", err)
	}

	// Start TLS if configured and using ldap:// (not ldaps://)
	if cfg.LDAPStartTLS && strings.HasPrefix(cfg.LDAPHost, "ldap://") {
		err = conn.StartTLS(&tls.Config{InsecureSkipVerify: cfg.LDAPTLSSkipVerify})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to start TLS: %w", err)
		}
	}

	// Bind with service account if configured
	if cfg.LDAPBindDN != "" {
		if cfg.LDAPBindPassword == "" {
			conn.Close()
			return nil, fmt.Errorf("LDAP_BIND_DN is set but LDAP_BIND_PASSWORD is empty")
		}
		err = conn.Bind(cfg.LDAPBindDN, cfg.LDAPBindPassword)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to bind with service account: %w", err)
		}
	} else {
		log.Printf("[LDAP] LDAP_BIND_DN not set: sync search will only work if server allows anonymous search")
	}

	return conn, nil
}

func buildUserBindDN(template, username string) string {
	if strings.Contains(template, "{username}") {
		return strings.ReplaceAll(template, "{username}", username)
	}
	if strings.Contains(template, "%s") {
		return strings.ReplaceAll(template, "%s", username)
	}
	return template
}
