package i18n

import (
	"fmt"
	"net/http"
	"strings"
)

var messages = map[string]map[string]string{
	"en": {
		"app_title":             "LdavSync - Corporate Directory",
		"search_placeholder":    "Search contacts...",
		"login":                 "Login",
		"logout":                "Logout",
		"admin_panel":           "Admin Panel",
		"username":              "Username",
		"password":              "Password",
		"contacts":              "Contacts",
		"group_numbers":         "Group Numbers",
		"name":                  "Name",
		"email":                 "Email",
		"phone":                 "Phone",
		"department":            "Department",
		"groups":                "Groups",
		"number":                "Number",
		"description":           "Description",
		"actions":               "Actions",
		"edit":                  "Edit",
		"delete":                "Delete",
		"save":                  "Save",
		"cancel":                "Cancel",
		"add_group":             "Add Group",
		"add_member":            "Add Member",
		"members":               "Members",
		"sync_now":              "Sync Now",
		"last_sync":             "Last Sync",
		"config":                "Configuration",
		"primary_number_prefix": "Primary Number Prefix",
		"export_vcard":          "Export vCard",
		"no_results":            "No results found",
		"area_all":              "All",
		"area_interni":          "Internal",
		"area_esterni":          "External",
		"area_politica":         "Political",
		"prefix_helper":         "From outside the office, dial %s before the extension shown below",
		"extension":             "ext.",
		"call_extension":        "Call extension",
	},
	"it": {
		"app_title":             "LdavSync - Rubrica Aziendale",
		"search_placeholder":    "Cerca contatti...",
		"login":                 "Accedi",
		"logout":                "Esci",
		"admin_panel":           "Pannello Admin",
		"username":              "Nome utente",
		"password":              "Password",
		"contacts":              "Contatti",
		"group_numbers":         "Numeri di Gruppo",
		"name":                  "Nome",
		"email":                 "Email",
		"phone":                 "Telefono",
		"department":            "Ufficio",
		"groups":                "Gruppi",
		"number":                "Numero",
		"description":           "Descrizione",
		"actions":               "Azioni",
		"edit":                  "Modifica",
		"delete":                "Elimina",
		"save":                  "Salva",
		"cancel":                "Annulla",
		"add_group":             "Aggiungi Gruppo",
		"add_member":            "Aggiungi Membro",
		"members":               "Membri",
		"sync_now":              "Sincronizza",
		"last_sync":             "Ultima Sincronizzazione",
		"config":                "Configurazione",
		"primary_number_prefix": "Prefisso Numero Primario",
		"export_vcard":          "Esporta vCard",
		"no_results":            "Nessun risultato",
		"area_all":              "Tutti",
		"area_interni":          "Interni",
		"area_esterni":          "Esterni",
		"area_politica":         "Politica",
		"prefix_helper":         "Da fuori l'ente, componi %s prima dell'interno mostrato sotto",
		"extension":             "interno",
		"call_extension":        "Chiama interno",
	},
}

// ResolveLocale parses Accept-Language header and returns best matching locale
func ResolveLocale(r *http.Request) string {
	acceptLang := r.Header.Get("Accept-Language")
	if acceptLang == "" {
		return "en"
	}

	// Parse quality values
	type langQuality struct {
		lang    string
		quality float64
	}

	var langs []langQuality
	for _, part := range strings.Split(acceptLang, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Check for quality value
		var lang string
		quality := 1.0
		if idx := strings.Index(part, ";q="); idx != -1 {
			lang = strings.TrimSpace(part[:idx])
			if qStr := strings.TrimSpace(part[idx+3:]); qStr != "" {
				fmt.Sscanf(qStr, "%f", &quality)
			}
		} else {
			lang = part
		}

		// Normalize language code (take first 2 chars)
		if len(lang) >= 2 {
			lang = strings.ToLower(lang[:2])
		}

		langs = append(langs, langQuality{lang, quality})
	}

	// Sort by quality (simple bubble sort for small lists)
	for i := 0; i < len(langs); i++ {
		for j := i + 1; j < len(langs); j++ {
			if langs[j].quality > langs[i].quality {
				langs[i], langs[j] = langs[j], langs[i]
			}
		}
	}

	// Find first supported locale
	for _, lq := range langs {
		if _, ok := messages[lq.lang]; ok {
			return lq.lang
		}
	}

	return "en"
}

// T translates a key for the given locale with optional arguments
func T(locale, key string, args ...interface{}) string {
	if msgs, ok := messages[locale]; ok {
		if msg, ok := msgs[key]; ok {
			if len(args) > 0 {
				return fmt.Sprintf(msg, args...)
			}
			return msg
		}
	}

	// Fallback to English
	if locale != "en" {
		if msgs, ok := messages["en"]; ok {
			if msg, ok := msgs[key]; ok {
				if len(args) > 0 {
					return fmt.Sprintf(msg, args...)
				}
				return msg
			}
		}
	}

	return key
}

// GetMessages returns all messages for a locale
func GetMessages(locale string) map[string]string {
	if msgs, ok := messages[locale]; ok {
		return msgs
	}
	return messages["en"]
}
