# Rubrica

[![Go Version](https://img.shields.io/github/go-mod/go-version/mirkochipdotcom/ldavsync)](https://go.dev/)
[![License](https://img.shields.io/github/license/mirkochipdotcom/ldavsync)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ready-blue)](https://github.com/mirkochipdotcom/ldavsync/pkgs/container/ldavsync)

> Rubrica aziendale con sincronizzazione automatica da LDAP/Active Directory, numeri di gruppo centralizzati (centralino) e supporto CardDAV per Thunderbird, iOS e Android.

Applicazione costruita su misura per l'anagrafica e l'infrastruttura AD di un ente specifico — non pensata per essere riusata così com'è altrove.

## Funzioni

- **Sincronizzazione LDAP automatica**: ogni ora, con riconciliazione (chi non è più presente/attivo in AD viene rimosso dalla rubrica)
- **Vista raggruppata per reparto**: elenco a gruppi collassabili, ricerca full-text (nome, email, reparto, ruolo)
- **Aree editabili da admin**: Interni/Esterni/Politica sono dati, non valori fissi — crea/rinomina/elimina aree, assegna le OU LDAP a ciascuna
- **Contatti locali**: aggiungi a mano contatti extra-dominio (fornitori, enti esterni), mai toccati dal sync automatico
- **Numeri di gruppo**: numeri centralizzati (es. Centralino, Ufficio Protocollo) assegnati a più contatti, gestiti da admin con ricerca-e-aggiungi
- **CardDAV**: integrazione nativa con Thunderbird, iOS, Android
- **Autenticazione LDAP**: accesso admin con le stesse credenziali di dominio
- **Dark mode automatica**, container singolo, SQLite embedded (zero configurazione DB)

## Avvio rapido

```bash
git clone https://github.com/mirkochipdotcom/ldavsync.git
cd ldavsync

cp .env.example .env
nano .env  # imposta i parametri LDAP

docker compose up -d --build
docker compose logs -f
```

Applicazione raggiungibile su `http://localhost:<SERVER_PORT>` (porta da `.env`, default 8080).

## Configurazione

Parametri iniziali via variabili d'ambiente (`.env`, vedi [`.env.example`](.env.example)):

| Variabile | Descrizione |
|---|---|
| `LDAP_HOST` | URL server LDAP |
| `LDAP_BASE_DN` | Base DN per le ricerche |
| `LDAP_BIND_DN` / `LDAP_BIND_PASSWORD` | Credenziali service account per il sync |
| `ADMIN_USERS` | Username abilitati come admin (separati da `;`) |
| `SYNC_INTERVAL_HOURS` | Frequenza sync, in ore |
| `SESSION_SECRET` | Chiave di cifratura sessione — da cambiare in produzione |

**Prefisso numero primario** e **mapping OU→Area**: impostati inizialmente da `.env`/default interno, ma editabili solo dal pannello admin da lì in avanti — non da variabili d'ambiente.

## Pannello admin

Login su `/login` con le credenziali LDAP (utente incluso in `ADMIN_USERS`). Sezioni, ognuna con URL propria:

- **Panoramica** — stato ultimo sync, sync manuale, prefisso numero primario
- **Etichette numero** — numeri di gruppo, membri (ricerca per nome, click per aggiungere)
- **Mapping OU** — assegna ogni Organizational Unit vista in LDAP a un'area
- **Aree** — crea/rinomina/elimina le aree
- **Contatti locali** — CRUD contatti extra-dominio

## Integrazione CardDAV

**Thunderbird**: Rubrica indirizzi → File → Nuovo → Rubrica CardDAV → URL `http://<server>:<porta>/carddav/`, credenziali LDAP.

**iOS**: Impostazioni → Contatti → Account → Aggiungi Account → Altro → Account CardDAV → server/credenziali LDAP.

**Android**: app compatibile CardDAV (es. DAVx⁵) → nuovo account CardDAV → URL/credenziali.

## Sviluppo

Prerequisiti: Go 1.25+, Docker (build CGO — `go-sqlite3` richiede un compilatore C; su Windows senza gcc nativo, build/test in un container `golang:1.25-alpine`, vedi `CLAUDE.md`).

```bash
cp .env.example .env
go run cmd/server/main.go

CGO_ENABLED=1 go build -o ldavsync ./cmd/server
go test ./...
```

## Endpoint principali

**Pubblici**: `GET /` (rubrica), `GET /search`, `GET /contacts/{uid}`, `GET /contacts/{uid}/export` (vCard), `GET /health`, `GET /version`.

**CardDAV**: `PROPFIND`/`REPORT /carddav/`, `GET /carddav/{uid}.vcf`, `GET /.well-known/carddav`.

**Admin** (autenticazione richiesta): `/admin`, `/admin/groups`, `/admin/ou-mapping`, `/admin/areas`, `/admin/local-contacts` — CRUD via form HTMX su ciascuna pagina.

## Risoluzione problemi

**Sync LDAP non funziona**: `docker compose logs -f` e cerca le righe `[SYNC]` — cause comuni: `LDAP_BASE_DN` errato, permessi service account, firewall sulla porta LDAP.

**Errori "database locked"**: verifica i permessi sul volume dati (`chown -R 1001:1001 ./data`) e che non ci siano più istanze in esecuzione sullo stesso file.

## Licenza

AGPL v3 — vedi [LICENSE](LICENSE).
