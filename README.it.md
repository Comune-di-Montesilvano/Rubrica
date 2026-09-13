# LdavSync - Rubrica Aziendale con CardDAV

[![Go Version](https://img.shields.io/github/go-mod/go-version/mirkochipdotcom/ldavsync)](https://go.dev/)
[![Licenza](https://img.shields.io/github/license/mirkochipdotcom/ldavsync)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ready-blue)](https://github.com/mirkochipdotcom/ldavsync/pkgs/container/ldavsync)

> **LdavSync** è un'applicazione per la rubrica aziendale con sincronizzazione automatica LDAP, gestione centralizzata dei numeri di gruppo del centralino e supporto del protocollo CardDAV per Thunderbird e client esterni.

[🇬🇧 English Version](README.md)

## Caratteristiche

- ✨ **Sincronizzazione LDAP Automatica**: Sincronizzazione oraria dei contatti dalla directory LDAP
- 📞 **Numeri di Gruppo**: Gestione centralizzata dei numeri di gruppo (es. Reception, Ufficio Protocollo)
- 🔍 **Ricerca Live**: Ricerca stile Google Contacts con HTMX
- 📱 **Server CardDAV**: Integrazione nativa con Thunderbird, iOS, Android
- 🌍 **Multilingua**: Interfaccia in italiano e inglese
- 🔐 **Autenticazione LDAP**: Accesso admin sicuro con credenziali LDAP
- 🐳 **Singolo Container**: Deployment semplice con Docker/Podman
- 💾 **SQLite Embedded**: Database a configurazione zero

## Avvio Rapido

### Con Docker Compose

```bash
# Clona il repository
git clone https://github.com/mirkochipdotcom/ldavsync.git
cd ldavsync

# Copia e configura l'ambiente
cp .env.example .env
nano .env  # Modifica le impostazioni LDAP

# Avvia il servizio
docker compose up -d

# Controlla i log
docker compose logs -f ldavsync
```

### Con Podman

```bash
# Build image
podman build -t ldavsync:latest .

# Run container
podman run -d \
   --name ldavsync \
   -p 8080:8080 \
   -v ./data:/data:Z \
   --env-file .env \
   ldavsync:latest
```

## Configurazione

Tutta la configurazione avviene tramite variabili d'ambiente (file `.env`):

| Variabile | Descrizione | Esempio |
|-----------|-------------|---------|
| `LDAP_HOST` | URL del server LDAP | `ldap://ldap.example.com:389` |
| `LDAP_BASE_DN` | Base DN per le ricerche | `dc=example,dc=com` |
| `LDAP_BIND_DN` | DN dell'account di servizio | `cn=admin,dc=example,dc=com` |
| `LDAP_BIND_PASSWORD` | Password dell'account di servizio | `secret` |
| `ADMIN_USERS` | Username admin (separati da `;`) | `admin;mario.rossi` |
| `SYNC_INTERVAL_HOURS` | Frequenza sincronizzazione (ore) | `1` |
| `PRIMARY_NUMBER_PREFIX_TEMPLATE` | Template numero telefonico | `0854321{ext}` |
| `SESSION_SECRET` | Chiave crittografia sessioni | Stringa casuale |

Vedi [`.env.example`](.env.example) per la configurazione completa.

## Architettura

```
ldavsync/
├── cmd/server/           # Applicazione principale
│   └── main.go
├── internal/             # Package interni
│   ├── config/          # Configurazione
│   ├── database/        # Operazioni SQLite
│   ├── ldap/            # Auth e sync LDAP
│   ├── phonebook/       # Logica di business
│   ├── carddav/         # Protocollo CardDAV
│   └── i18n/            # Internazionalizzazione
├── web/                 # Asset frontend
│   ├── templates/       # Template HTML
│   └── static/          # CSS, JS
├── Dockerfile           # Build multi-stage
└── compose.yml          # Docker Compose
```GoAddress

## Utilizzo

### Accesso Pubblico

- **Ricerca**: Disponibile su `/` - nessuna autenticazione richiesta
- **Dettagli Contatto**: Clicca su qualsiasi contatto per vedere i dettagli completi
- **Esporta vCard**: Scarica singoli contatti come file `.vcf`

### Pannello Admin

1. Login su `/login` con credenziali LDAP
2. Configura il template del prefisso numero primario
3. Gestisci numeri di gruppo (crea, modifica, elimina)
4. Associa contatti ai gruppi
5. Sovrascrivi manualmente informazioni contatti
6. Attiva sincronizzazione LDAP manuale

### Integrazione CardDAV

#### Thunderbird

1. Vai su **Rubrica**
2. **File → Nuovo → Rubrica CardDAV**
3. Inserisci:
   - **URL**: `http://tuo-server:8080/carddav/`
   - **Nome utente**: Il tuo username LDAP
   - **Password**: La tua password LDAP
4. Clicca su **Continua**

#### iOS

1. **Impostazioni → Contatti → Account → Aggiungi account → Altro**
2. Seleziona **Account CardDAV**
3. Inserisci:
   - **Server**: `tuo-server:8080`
   - **Nome utente**: Il tuo username LDAP
   - **Password**: La tua password LDAP
4. Salva

#### Android

Per Android, usa un'app compatibile con CardDAV come **DAVx⁵**:

1. Installa DAVx⁵ dal Play Store
2. Aggiungi nuovo account → CardDAV
3. Inserisci URL server e credenziali

## Sviluppo

### Prerequisiti

- Go 1.25+
- Docker/Podman (opzionale)
- Server LDAP per test

### Sviluppo Locale

```bash
# Installa dipendenze
go mod download

# Esegui localmente
cp .env.example .env
# Modifica .env con le tue impostazioni LDAP
go run cmd/server/main.go

# Build
go build -o ldavsync cmd/server/main.go

# Esegui test
go test ./...
```

### Costruisci Immagine Docker

```bash
# Build con tag versione
docker build --build-arg VERSION=0.1.0 -t ldavsync:0.1.0 .

# Esegui
docker run -p 8080:8080 -v $(pwd)/data:/data --env-file .env ldavsync:0.1.0
```

## Endpoint API

### Pubblici

- `GET /` - Interfaccia rubrica principale
- `GET /search?q={query}` - Cerca contatti (partial HTMX)
- `GET /contacts` - Lista contatti (JSON)
- `GET /contacts/{uid}` - Dettagli contatto
- `GET /contacts/{uid}/export` - Esporta vCard
- `GET /health` - Health check

### Admin (richiede autenticazione)

- `POST /admin/sync` - Attiva sincronizzazione manuale
- `GET/POST /admin/config` - Gestisci configurazione
- `GET/POST /admin/groups` - Gestisci gruppi
- `GET /admin/groups/{id}/members` - Visualizza membri gruppo
- `POST /admin/groups/{id}/members` - Aggiungi membro
- `POST /admin/contacts/{uid}/override` - Sovrascrivi dati contatto

### CardDAV

- `PROPFIND /carddav/` - Lista rubrica
- `REPORT /carddav/` - Query contatti
- `GET /carddav/{uid}.vcf` - Ottieni vCard
- `GET /.well-known/carddav` - Discovery

## Risoluzione Problemi

### Sincronizzazione LDAP Non Funziona

Controlla i log:
```bash
docker compose logs -f ldavsync
```

Cerca voci `[SYNC]`. Problemi comuni:
- `LDAP_BASE_DN` errato
- Permessi account di servizio
- Firewall che blocca porta LDAP

### Autenticazione CardDAV Fallita

Assicurati che:
1. L'autenticazione LDAP funzioni (testa con login web)
2. Le credenziali LDAP siano corrette
3. Il server sia raggiungibile dal client

### Errori Database Bloccato

Controlla:
- Permessi volume: `chown -R 1001:1001 ./data`
- Nessuna istanza multipla che accede allo stesso database

## Contribuire

I contributi sono benvenuti! Per favore:

1. Fai il fork del repository
2. Crea un branch feature
3. Committa le tue modifiche
4. Pusha sul branch
5. Apri una Pull Request

## Licenza

AGPL v3 - vedi il file [LICENSE](LICENSE) per i dettagli

## Crediti

Costruito con:
- [Go](https://go.dev/) linguaggio di programmazione
- [gorilla/mux](https://github.com/gorilla/mux) router HTTP
- [HTMX](https://htmx.org/) HTML dinamico
- [Tailwind CSS](https://tailwindcss.com/) styling
- [go-ldap](https://github.com/go-ldap/ldap) client LDAP

## Supporto

Per problemi e domande:
- GitHub Issues: [github.com/mirkochipdotcom/ldavsync/issues](https://github.com/mirkochipdotcom/ldavsync/issues)
- Documentazione: Vedi cartella [docs/](docs/)

---

Fatto con ❤️ da [mirkochipdotcom](https://github.com/mirkochipdotcom)
