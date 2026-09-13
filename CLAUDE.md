# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

LdavSync (module `github.com/mirkochipdotcom/ldavsync`, historically "gorubrica"): a corporate directory app that syncs contacts hourly from LDAP/AD, stores them in embedded SQLite, serves a search UI (HTMX), and exposes a CardDAV server for Thunderbird/iOS/Android address book clients.

## Commands

```bash
# Local run (requires .env — copy from .env.example, set LDAP_* vars)
go run cmd/server/main.go

# Build (CGO required — go-sqlite3 is a cgo driver)
CGO_ENABLED=1 go build -o ldavsync ./cmd/server

# Tests (none exist yet; this is how CI runs them)
go test ./...
go test ./internal/ldap/...          # single package
go test -run TestName ./internal/... # single test

# Vet (run in CI before build)
go vet ./...

# Docker
docker build --build-arg VERSION=0.1.0 -t ldavsync:0.1.0 .
docker compose up -d
```

CI (`.github/workflows/build.yml`) on push/PR to `main`: `go vet` → `go test -v ./...` → `CGO_ENABLED=1 go build`. Tags `v*` also build+push a multi-arch image to `ghcr.io` and cut a GitHub release.

## Architecture

Single Go binary, no framework beyond `gorilla/mux` for routing. `cmd/server/main.go` wires everything and holds all HTTP handlers as package-level functions (no per-domain handler files) — package-level vars (`db`, `cfg`, `pbService`, `templates`, `store`) are shared mutable state, not injected.

- **`internal/config`**: all runtime config comes from env vars (loaded via `godotenv`, see `.env.example`). `LDAP_OU_FILTERS` and `LDAP_ALLOWED_GROUPS` use custom multi-level delimiter parsing (`;`/`:`/`,`) — see `getEnvMapList` in `config.go` before changing that format.
- **`internal/ldap`**: `auth.go` binds against LDAP for login (checks membership in `LDAP_ADMIN_GROUP` for admin rights); `sync.go` runs the periodic full-tree search-and-upsert (`SyncContacts`, called from a ticker goroutine in `main.go` plus once at startup). Sync applies, in order: `userAccountControl` disabled-bit filter (AD only, gated by `LDAP_ONLY_ACTIVE`), then `LDAP_ALLOWED_GROUPS` membership filter (via `memberOf`, matched by DN/CN alias), then upserts by `uid` (falling back to `sAMAccountName`/`userPrincipalName`).
- **`internal/database`**: raw SQL over `mattn/go-sqlite3`, no ORM/migrations framework — schema changes are made directly in `sqlite.go`.
- **`internal/phonebook`**: read-side service joining contacts with their group-number associations (`ContactWithGroups`, `GroupWithMembers`) for the UI/API layer.
- **`internal/carddav`**: implements CardDAV (`PROPFIND`/`REPORT`) as its own sub-router mounted at `/carddav`, independent of the mux routes in `main.go`. vCard generation logic is duplicated between here and `generateVCard` in `main.go` — keep both in sync if the vCard format changes.
- **`internal/i18n`**: locale resolved per-request (`i18n.ResolveLocale`); templates receive a `Messages` map, not a Go i18n library.
- **`web/templates`**: parsed once at startup via `template.ParseGlob`, not re-loaded on change — restart the server to see template edits. Custom `substr` template func is uppercase-only (used for initials/avatars).
- Group **phone numbers** are a separate concept from LDAP contacts: `PRIMARY_NUMBER_PREFIX_TEMPLATE` (e.g. `0854321{ext}`) generates each contact's primary number from their LDAP extension; "group numbers" (e.g. Reception) are managed independently in the admin panel and linked to contacts via a many-to-many membership table.

## Gotchas

- `go-sqlite3` requires CGO; cross-compiling or using `CGO_ENABLED=0` will fail the build.
- Templates and static assets are loaded from relative paths (`web/templates`, `web/static`) — run binaries from the repo root, not from an arbitrary `cmd/server` working directory.
- `SESSION_SECRET` defaults to `change-me-in-production`; must be overridden for any real deployment.
