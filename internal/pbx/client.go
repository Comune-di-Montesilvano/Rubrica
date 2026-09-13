// Package pbx implementa lo screen-scraping della web console del
// centralino Invidea "ViVo" (nessuna API ufficiale) per estrarre peers SIP
// e call group non presenti nel dominio LDAP. Vedi
// docs/superpowers/specs/2026-09-13-pbx-scraping-design.md per il design
// completo, gli endpoint usati e le regole di merge/override.
package pbx

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// Peer è un interno SIP come esposto da
// vivo.index.php?module=monitor&mode=peers (var JS "infopeers").
type Peer struct {
	Extension string `json:"defaultuser"`
	CallerID  string `json:"callerid"`
	Status    string `json:"status"`
}

// CallGroup è una riga della tabella
// vivo.index.php?module=extensions&mode=callgroups.
type CallGroup struct {
	Name      string
	Extension string
	Members   []string // interni destinatari, estratti da "SIP/xxx"
	Strategy  string
	Timeout   string
	Enabled   bool
}

// Client parla con la web console del centralino su una sessione autenticata.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient crea un client per il centralino a baseURL (es.
// "https://10.0.90.253"). Il certificato TLS è tipicamente self-signed
// (dispositivo su IP privato) — la verifica è disabilitata di proposito.
func NewClient(baseURL string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Jar: jar,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// Login autentica la sessione. Va chiamato prima di FetchPeers/FetchCallGroups.
func (c *Client) Login(user, pass string) error {
	form := url.Values{
		"redirect2": {"5"},
		"username":  {user},
		"password":  {pass},
		"Submit3":   {"Accedi"},
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/verifica__login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := c.http.Do(req)
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

func (c *Client) get(path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", c.baseURL+"/vivo.php")

	resp, err := c.http.Do(req)
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

// FetchPeers scarica e parsa lo stato dei peers SIP.
func (c *Client) FetchPeers() ([]Peer, error) {
	html, err := c.get("vivo.index.php?module=monitor&mode=peers")
	if err != nil {
		return nil, err
	}
	return ParsePeers(html)
}

// ParsePeers estrae l'array JSON dalla var JS "infopeers" incorporata
// nella pagina. Esportata per i test (fixture HTML statiche).
func ParsePeers(html string) ([]Peer, error) {
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

// FetchCallGroups scarica e parsa la tabella dei call group.
func (c *Client) FetchCallGroups() ([]CallGroup, error) {
	html, err := c.get("vivo.index.php?module=extensions&mode=callgroups")
	if err != nil {
		return nil, err
	}
	return ParseCallGroups(html), nil
}

// Regex sulla struttura HTML della tabella callgroups. Fragile per natura
// (screen-scraping): se il centralino cambia versione/markup va rivista.
var (
	rowRe     = regexp.MustCompile(`(?s)<tr class="vivo_rowColor\d".*?</tr>`)
	cgidRe    = regexp.MustCompile(`name="cgid\[\]"\s*\n?\s*value="(\d+)"`)
	// (?s)+non-greedy: il contenuto cella può includere tag innestati (es.
	// "SIP/740<br />SIP/741" per più membri) — sipRe estrae comunque solo
	// gli interni dal testo catturato, ignorando i tag di mezzo.
	cellsRe = regexp.MustCompile(`(?s)editCallGroup\('\d+'\)">(.*?)</td>`)
	sipRe     = regexp.MustCompile(`SIP/(\d+)`)
	enabledRe = regexp.MustCompile(`/(on|off)\.gif`)
)

// ParseCallGroups estrae la tabella dei call group dall'HTML della pagina.
// Righe non riconosciute (niente checkbox cgid, meno di 5 colonne testuali)
// vengono ignorate silenziosamente. Esportata per i test.
func ParseCallGroups(html string) []CallGroup {
	var groups []CallGroup
	for _, row := range rowRe.FindAllString(html, -1) {
		if cgidRe.FindStringSubmatch(row) == nil {
			continue
		}
		cells := cellsRe.FindAllStringSubmatch(row, -1)
		// colonne attese dopo la checkbox: nome, interno, peers, strategy, timeout
		if len(cells) < 5 {
			continue
		}
		var members []string
		for _, sm := range sipRe.FindAllStringSubmatch(cells[2][1], -1) {
			members = append(members, sm[1])
		}
		enabled := false
		if em := enabledRe.FindStringSubmatch(row); em != nil {
			enabled = em[1] == "on"
		}
		groups = append(groups, CallGroup{
			Name:      strings.TrimSpace(cells[0][1]),
			Extension: strings.TrimSpace(cells[1][1]),
			Members:   members,
			Strategy:  strings.TrimSpace(cells[3][1]),
			Timeout:   strings.TrimSpace(cells[4][1]),
			Enabled:   enabled,
		})
	}
	return groups
}
