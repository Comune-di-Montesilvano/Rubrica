# LdavSync - Corporate Directory with CardDAV

[![Go Version](https://img.shields.io/github/go-mod/go-version/mirkochipdotcom/ldavsync)](https://go.dev/)
[![License](https://img.shields.io/github/license/mirkochipdotcom/ldavsync)](LICENSE)
[![Docker](https://img.shields.io/badge/docker-ready-blue)](https://github.com/mirkochipdotcom/ldavsync/pkgs/container/ldavsync)

> **LdavSync** is a corporate directory application with automatic LDAP synchronization, centralized group phone numbers, and CardDAV protocol support for Thunderbird and external clients.

[🇮🇹 Versione Italiana](README.it.md)

## Features

- ✨ **Automatic LDAP Sync**: Hourly synchronization of contacts from LDAP directory
- 📞 **Group Numbers**: Centralized management of group phone numbers (e.g., Reception, Protocol Office)
- 🔍 **Live Search**: Google Contacts-style search with HTMX
- 📱 **CardDAV Server**: Native integration with Thunderbird, iOS, Android
- 🌍 **Multilingual**: Italian and English interface
- 🔐 **LDAP Authentication**: Secure admin access with LDAP credentials
- 🐳 **Single Container**: Easy deployment with Docker/Podman
- 💾 **Embedded SQLite**: Zero-configuration database

## Quick Start

### With Docker Compose

```bash
# Clone repository
git clone https://github.com/mirkochipdotcom/ldavsync.git
cd ldavsync

# Copy and configure environment
cp .env.example .env
nano .env  # Edit LDAP settings

# Start service
docker compose up -d

# Check logs
docker compose logs -f
```

Access the application at [http://localhost:8080](http://localhost:8080)

### With Podman

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

## Configuration

All configuration is via environment variables (`.env` file):

| Variable | Description | Example |
|----------|-------------|---------|
| `LDAP_HOST` | LDAP server URL | `ldap://ldap.example.com:389` |
| `LDAP_BASE_DN` | Base DN for searches | `dc=example,dc=com` |
| `LDAP_BIND_DN` | Service account DN | `cn=admin,dc=example,dc=com` |
| `LDAP_BIND_PASSWORD` | Service account password | `secret` |
| `ADMIN_USERS` | Admin usernames (`;` separated) | `admin;mario.rossi` |
| `SYNC_INTERVAL_HOURS` | Sync frequency (hours) | `1` |
| `PRIMARY_NUMBER_PREFIX_TEMPLATE` | Phone number template | `0854321{ext}` |
| `SESSION_SECRET` | Session encryption key | Random string |

See [`.env.example`](.env.example) for complete configuration.

## Architecture

```
ldavsync/
├── cmd/server/           # Main application
│   └── main.go
├── internal/             # Internal packages
│   ├── config/          # Configuration
│   ├── database/        # SQLite operations
│   ├── ldap/            # LDAP auth & sync
│   ├── phonebook/       # Business logic
│   ├── carddav/         # CardDAV protocol
│   └── i18n/            # Internationalization
├── web/                 # Frontend assets
│   ├── templates/       # HTML templates
│   └── static/          # CSS, JS
├── Dockerfile           # Multi-stage build
└── compose.yml          # Docker Compose
```

## Usage

### Public Access

- **Search**: Available at `/` - no authentication required
- **Contact Details**: Click on any contact to view full details
- **Export vCard**: Download individual contacts as `.vcf` files

### Admin Panel

1. Login at `/login` with LDAP credentials
2. Configure primary number prefix template
3. Manage group numbers (create, edit, delete)
4. Associate contacts with groups
5. Override contact information manually
6. Trigger manual LDAP sync

### CardDAV Integration

#### Thunderbird

1. Go to **Address Book**
2. **File → New → CardDAV Address Book**
3. Enter:
   - **URL**: `http://your-server:8080/carddav/`
   - **Username**: Your LDAP username
   - **Password**: Your LDAP password
4. Click **Continue**

#### iOS

1. **Settings → Contacts → Accounts → Add Account → Other**
2. Select **CardDAV Account**
3. Enter:
   - **Server**: `your-server:8080`
   - **Username**: Your LDAP username
   - **Password**: Your LDAP password
4. Save

#### Android

For Android, use a CardDAV-compatible app like **DAVx⁵**:

1. Install DAVx⁵ from Play Store
2. Add new account → CardDAV
3. Enter server URL and credentials

## Development

### Prerequisites

- Go 1.25+
- Docker/Podman (optional)
- LDAP server for testing

### Local Development

```
# Install dependencies
go mod download

# Run locally
cp .env.example .env
# Edit .env with your LDAP settings
go run cmd/server/main.go

# Build
go build -o ldavsync cmd/server/main.go

# Run tests
go test ./...
```

### Build Docker Image

```bash
# Build with version tag
docker build --build-arg VERSION=0.1.0 -t ldavsync:0.1.0 .

# Run
docker run -p 8080:8080 -v $(pwd)/data:/data --env-file .env ldavsync:0.1.0
```

## API Endpoints

### Public

- `GET /` - Main phonebook interface
- `GET /search?q={query}` - Search contacts (HTMX partial)
- `GET /contacts` - List contacts (JSON)
- `GET /contacts/{uid}` - Contact details
- `GET /contacts/{uid}/export` - Export vCard
- `GET /health` - Health check

### Admin (requires authentication)

- `POST /admin/sync` - Trigger manual sync
- `GET/POST /admin/config` - Manage configuration
- `GET/POST /admin/groups` - Manage groups
- `GET /admin/groups/{id}/members` - View group members
- `POST /admin/groups/{id}/members` - Add member
- `POST /admin/contacts/{uid}/override` - Override contact data

### CardDAV

- `PROPFIND /carddav/` - List address book
- `REPORT /carddav/` - Query contacts
- `GET /carddav/{uid}.vcf` - Get vCard
- `GET /.well-known/carddav` - Discovery

## Troubleshooting

### LDAP Sync Not Working

Check logs:
```bash
docker compose logs -f ldavsync
```

Look for `[SYNC]` entries. Common issues:
- Incorrect `LDAP_BASE_DN`
- Service account permissions
- Firewall blocking LDAP port

### CardDAV Authentication Failed

Ensure:
1. LDAP authentication is working (test with web login)
2. LDAP credentials are correct
3. Server is reachable from client

### Database Locked Errors

Check:
- Volume permissions: `chown -R 1001:1001 ./data`
- No multiple instances accessing same database

## Contributing

Contributions welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Commit your changes
4. Push to the branch
5. Open a Pull Request

## License

AGPL v3 - see [LICENSE](LICENSE) file for details

## Credits

Built with:
- [Go](https://go.dev/) programming language
- [gorilla/mux](https://github.com/gorilla/mux) HTTP router
- [HTMX](https://htmx.org/) dynamic HTML
- [Tailwind CSS](https://tailwindcss.com/) styling
- [go-ldap](https://github.com/go-ldap/ldap) LDAP client

## Support

For issues and questions:
- GitHub Issues: [github.com/mirkochipdotcom/ldavsync/issues](https://github.com/mirkochipdotcom/ldavsync/issues)
- Documentation: See [docs/](docs/) folder

---

Made with ❤️ by [mirkochipdotcom](https://github.com/mirkochipdotcom)
