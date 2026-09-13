// Comando pbxpoc: PoC di lettura dati dal centralino Invidea "ViVo" via la sua
// web console (non un'API ufficiale — screen-scraping delle pagine interne
// autenticate). Estrae:
//   - lo stato SIP dei peers (interno + nome + stato registrazione), filtrando
//     quelli già presenti come contatti sincronizzati da LDAP (per numero
//     interno, campo contacts.ldap_ext);
//   - i gruppi di chiamata ("call groups": nome, interno del gruppo, interni
//     membri/destinatari, strategia di ring).
//
// Uso:
//
//	PBX_URL=https://10.0.90.253 PBX_USER=admin PBX_PASS=... go run ./cmd/pbxpoc
//
// Credenziali NON hardcodate: passare via env. Il certificato TLS del
// centralino è self-signed → verifica disabilitata volutamente (dispositivo
// interno, IP privato).
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/mirkochipdotcom/ldavsync/internal/config"
	"github.com/mirkochipdotcom/ldavsync/internal/database"
)

// Peer rappresenta un interno SIP come esposto da
// vivo.index.php?module=monitor&mode=peers (var JS "infopeers").
type Peer struct {
	Extension string `json:"defaultuser"`
	CallerID  string `json:"callerid"`
	Status    string `json:"status"`
}

// CallGroup rappresenta una riga della tabella
// vivo.index.php?module=extensions&mode=callgroups.
type CallGroup struct {
	ID        string
	Name      string
	Extension string
	Members   []string // numeri interni destinatari (da "SIP/xxx")
	Strategy  string
	Timeout   string
	Enabled   bool
}

func main() {
	pbxURL := strings.TrimRight(mustEnv("PBX_URL"), "/")
	pbxUser := mustEnv("PBX_USER")
	pbxPass := mustEnv("PBX_PASS")

	cfg := config.Load()
	db, err := database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("apertura db: %v", err)
	}
	defer db.Close()

	domainExts, err := loadDomainExtensions(db)
	if err != nil {
		log.Fatalf("lettura interni dominio: %v", err)
	}
	log.Printf("interni già in dominio (esclusi dai peers PBX): %d", len(domainExts))

	client := newPBXClient()
	if err := login(client, pbxURL, pbxUser, pbxPass); err != nil {
		log.Fatalf("login PBX: %v", err)
	}

	peers, err := fetchPeers(client, pbxURL)
	if err != nil {
		log.Fatalf("fetch peers: %v", err)
	}

	fmt.Println("\n=== Peers SOLO centralino (esclusi quelli già sincronizzati da LDAP) ===")
	for _, p := range peers {
		if _, inDomain := domainExts[p.Extension]; inDomain {
			continue
		}
		fmt.Printf("%-6s %-40s status=%q\n", p.Extension, p.CallerID, p.Status)
	}

	groups, err := fetchCallGroups(client, pbxURL)
	if err != nil {
		log.Fatalf("fetch call groups: %v", err)
	}

	fmt.Println("\n=== Gruppi di chiamata ===")
	for _, g := range groups {
		fmt.Printf("[%s] %-35s interno=%-6s strategy=%-10s timeout=%-4s abilitato=%v membri=%v\n",
			g.ID, g.Name, g.Extension, g.Strategy, g.Timeout, g.Enabled, g.Members)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("variabile env %s mancante", key)
	}
	return v
}

func loadDomainExtensions(db *database.DB) (map[string]struct{}, error) {
	rows, err := db.Query(`
		SELECT ldap_ext FROM contacts
		WHERE deleted_at IS NULL AND ldap_ext IS NOT NULL AND ldap_ext != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	set := make(map[string]struct{})
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			return nil, err
		}
		set[strings.TrimSpace(ext)] = struct{}{}
	}
	return set, rows.Err()
}

func newPBXClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // cert self-signed del centralino
		},
	}
}

func login(client *http.Client, base, user, pass string) error {
	form := url.Values{
		"redirect2": {"5"},
		"username":  {user},
		"password":  {pass},
		"Submit3":   {"Accedi"},
	}
	req, err := http.NewRequest(http.MethodPost, base+"/verifica__login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `window.open("header.php"`) {
		return fmt.Errorf("login fallito (status %d) — credenziali errate?", resp.StatusCode)
	}
	return nil
}

func pbxGet(client *http.Client, base, path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", base+"/vivo.php")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s -> HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

var infopeersRe = regexp.MustCompile(`var infopeers\s*=\s*(\[.*?\]);`)

func fetchPeers(client *http.Client, base string) ([]Peer, error) {
	html, err := pbxGet(client, base, "vivo.index.php?module=monitor&mode=peers")
	if err != nil {
		return nil, err
	}
	m := infopeersRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("var infopeers non trovata nella risposta (sessione scaduta o pagina cambiata?)")
	}
	var peers []Peer
	if err := json.Unmarshal([]byte(m[1]), &peers); err != nil {
		return nil, fmt.Errorf("parse infopeers: %w", err)
	}
	return peers, nil
}

// Regex sulla struttura HTML della tabella callgroups (vedi
// vivo.index.php?module=extensions&mode=callgroups). Fragile per natura
// (screen-scraping): se il centralino cambia versione/markup va rivista.
var (
	rowRe     = regexp.MustCompile(`(?s)<tr class="vivo_rowColor\d".*?</tr>`)
	cgidRe    = regexp.MustCompile(`name="cgid\[\]"\s*\n?\s*value="(\d+)"`)
	nameRe    = regexp.MustCompile(`editCallGroup\('\d+'\)">([^<]+)</td>`)
	cellsRe   = regexp.MustCompile(`editCallGroup\('\d+'\)">([^<]*)</td>`)
	sipRe     = regexp.MustCompile(`SIP/(\d+)`)
	enabledRe = regexp.MustCompile(`/(on|off)\.gif`)
)

func fetchCallGroups(client *http.Client, base string) ([]CallGroup, error) {
	html, err := pbxGet(client, base, "vivo.index.php?module=extensions&mode=callgroups")
	if err != nil {
		return nil, err
	}

	var groups []CallGroup
	for _, row := range rowRe.FindAllString(html, -1) {
		idm := cgidRe.FindStringSubmatch(row)
		if idm == nil {
			continue
		}
		cells := cellsRe.FindAllStringSubmatch(row, -1)
		// colonne attese (dopo la checkbox): nome, interno, peers, strategy, timeout
		if len(cells) < 5 {
			continue
		}
		members := []string{}
		for _, sm := range sipRe.FindAllStringSubmatch(cells[2][1], -1) {
			members = append(members, sm[1])
		}
		enabled := false
		if em := enabledRe.FindStringSubmatch(row); em != nil {
			enabled = em[1] == "on"
		}
		groups = append(groups, CallGroup{
			ID:        idm[1],
			Name:      strings.TrimSpace(cells[0][1]),
			Extension: strings.TrimSpace(cells[1][1]),
			Members:   members,
			Strategy:  strings.TrimSpace(cells[3][1]),
			Timeout:   strings.TrimSpace(cells[4][1]),
			Enabled:   enabled,
		})
	}
	_ = nameRe // tenuta per chiarezza/futuro debug, non usata direttamente
	return groups, nil
}
