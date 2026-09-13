// Comando pbxpoc: strumento di debug manuale per il sync PBX. Fa login,
// scarica peers + call group con internal/pbx, e stampa a schermo cosa
// verrebbe applicato (peers esclusi quelli già in dominio LDAP) — utile
// per verificare credenziali/endpoint prima che il sync orario reale
// (cmd/server, vedi internal/pbx.SyncPBX) tocchi il database.
//
// Uso:
//
//	PBX_URL=https://192.0.2.10 PBX_USER=admin PBX_PASS=... go run ./cmd/pbxpoc
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/pbx"
)

func main() {
	pbxURL := mustEnv("PBX_URL")
	pbxUser := mustEnv("PBX_USER")
	pbxPass := mustEnv("PBX_PASS")

	cfg := config.Load()
	db, err := database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("apertura db: %v", err)
	}
	defer db.Close()

	domainExts, err := db.ListDomainExtensions()
	if err != nil {
		log.Fatalf("lettura interni dominio: %v", err)
	}
	inDomain := make(map[string]struct{}, len(domainExts))
	for _, e := range domainExts {
		inDomain[e] = struct{}{}
	}
	log.Printf("interni già in dominio (esclusi dai peers PBX): %d", len(inDomain))

	client := pbx.NewClient(pbxURL)
	if err := client.Login(pbxUser, pbxPass); err != nil {
		log.Fatalf("login PBX: %v", err)
	}

	peers, err := client.FetchPeers()
	if err != nil {
		log.Fatalf("fetch peers: %v", err)
	}

	fmt.Println("\n=== Peers SOLO centralino (esclusi quelli già sincronizzati da LDAP) ===")
	for _, p := range peers {
		if _, ok := inDomain[p.Extension]; ok {
			continue
		}
		fmt.Printf("%-6s %-40s status=%q\n", p.Extension, p.CallerID, p.Status)
	}

	groups, err := client.FetchCallGroups()
	if err != nil {
		log.Fatalf("fetch call groups: %v", err)
	}

	fmt.Println("\n=== Gruppi di chiamata ===")
	for _, g := range groups {
		fmt.Printf("%-35s interno=%-6s strategy=%-10s timeout=%-4s abilitato=%v membri=%v\n",
			g.Name, g.Extension, g.Strategy, g.Timeout, g.Enabled, g.Members)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("variabile env %s mancante", key)
	}
	return v
}
