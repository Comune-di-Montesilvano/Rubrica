package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"
)

// apiSearchLimit: voci restituite da /api/cerca (la tendina del Cruscotto ne
// mostra poche).
const apiSearchLimit = 8

// apiContact: voce di /api/cerca, usata dalla ricerca a tendina di CruscottoPA.
type apiContact struct {
	UID     string `json:"uid"`
	Nome    string `json:"nome"`
	Reparto string `json:"reparto"`
	Interno string `json:"interno"`
	Mail    string `json:"mail"`
}

// formatExt: più interni in AD sono separati da ";" — mostrati "700, 701".
func formatExt(s string) string {
	return strings.ReplaceAll(s, ";", ", ")
}

// handleAPISearch: GET /api/cerca?q= — stessa ricerca della pagina pubblica
// (disattivati esclusi), al massimo apiSearchLimit contatti, in JSON.
func handleAPISearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []apiContact{}
	if utf8.RuneCountInString(q) >= 2 {
		results, err := pbService.SearchContactsWithGroups(q, apiSearchLimit)
		if err != nil {
			log.Printf("[API] ricerca non riuscita: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"errore":"ricerca non riuscita"}`))
			return
		}
		for _, res := range results {
			c := res.Contact
			interno := formatExt(c.LDAPExt)
			if interno == "" {
				interno = c.PrimaryNumber
			}
			out = append(out, apiContact{UID: c.UID, Nome: c.DisplayName, Reparto: c.Department, Interno: interno, Mail: c.Email})
		}
	}
	json.NewEncoder(w).Encode(out)
}
