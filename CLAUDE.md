# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**Rubrica** (module `github.com/Comune-di-Montesilvano/Rubrica` — historically "gorubrica" → "LdavSync" → "Rubrica", repo transferred from `mirkochipdotcom/LdavSync`): a corporate directory app, purpose-built for a specific municipality's AD/PBX (not designed for reuse elsewhere), that syncs contacts hourly from LDAP/AD, stores them in embedded SQLite, serves a search UI (HTMX), and exposes a CardDAV server for Thunderbird/iOS/Android address book clients.

Docker image: `ghcr.io/comune-di-montesilvano/rubrica` (lowercase — GHCR/OCI require lowercase image names, unlike the GitHub org/repo name itself). Binary/container/system-user name: `rubrica`. Database file path is unchanged (`/data/ldavsync.db`, see `DATABASE_PATH` in `compose.yml`) — deliberately not renamed, to avoid touching the volume path of any already-running deployment.

## Commands

```bash
# Local run (requires .env — copy from .env.example, set LDAP_* vars)
go run cmd/server/main.go

# Build (CGO required — go-sqlite3 is a cgo driver)
CGO_ENABLED=1 go build -o rubrica ./cmd/server

# Tests
go test ./...
go test ./internal/ldap/...          # single package
go test -run TestName ./internal/... # single test
go vet ./...

# Docker
docker build --build-arg VERSION=0.1.0 -t rubrica:0.1.0 .
docker compose up -d --build   # rebuild + redeploy against real LDAP for end-to-end checks
```

**On Windows dev machines without a native C compiler**, `go build`/`go test` fail (cgo needs gcc for `go-sqlite3`). Run them in a throwaway container instead:

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine \
  sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./... -v"
```
`MSYS_NO_PATHCONV=1` and the `//app` double-slash stop Git Bash from mangling the `-w` path into a Windows path.

CI (`.github/workflows/test.yml` + `release.yml`) on push/PR to `main`: `go build` → `go vet` → `go test -race`, plus a blocking Trivy filesystem scan. Tags build+push a multi-arch image to `ghcr.io` (report-only Trivy image scan) via `release.yml`.

## Architecture

Single Go binary, no framework beyond `gorilla/mux` for routing. `cmd/server/main.go` wires everything and holds all HTTP handlers as package-level functions (no per-domain handler files) — package-level vars (`db`, `cfg`, `pbService`, `templates`, `store`) are shared mutable state, not injected.

- **`internal/config`**: all runtime config comes from env vars (loaded via `godotenv`, see `.env.example`). `LDAP_OU_FILTERS`/`LDAP_ALLOWED_GROUPS` (custom `;`/`:`/`,` parsing, see `getEnvMapList`) are used by `internal/carddav` for per-book filtering — unrelated to the Area system below. `PRIMARY_NUMBER_PREFIX_TEMPLATE` is only the one-time seed imported into `app_config` on first boot; from then on the admin panel (Configurazione) is the source of truth, editing `.env` after that has no effect.
- **`internal/ldap`**: `auth.go` binds against LDAP for login (checks membership in `LDAP_ADMIN_GROUP` for admin rights); `sync.go` runs the periodic full-tree search-and-upsert (`SyncContacts`). A disabled AD account (`userAccountControl` bit 2) is no longer excluded from the search or from `LDAP_ALLOWED_GROUPS` filtering (disabling an account in AD often clears its group memberships too) — it's synced normally but stored with `contacts.disabled=1`: every public query (search, listing, CardDAV, `CountByArea`) filters `disabled=0`, but it stays visible to the `/admin/pbx` name-matching utilities. `deriveArea` (OU → Area via an admin-editable JSON mapping in `app_config`, key `ou_area_mapping`) runs first; if it yields no area, `database.MatchExtensionRange` is tried next (see Areas bullet below) before falling back to empty. `normalizeDescription` fixes typo/casing aliases for the AD `description` field. After the loop, `db.SoftDeleteStale` soft-deletes any **`source='ldap'`** contact whose `last_sync` wasn't refreshed this pass — **never touches `source='manual'` contacts**.
- **`internal/database`**: raw SQL over `mattn/go-sqlite3`, no ORM/migrations framework — schema changes are `ALTER TABLE ... ADD COLUMN` statements in `migrate()`. **Any new text column added this way needs `DEFAULT ''`** (or every existing row scans as `NULL` → "converting NULL to string is unsupported" the moment some other `INSERT` path omits it — hit twice this session, once for `contacts`, once for `group_numbers.area`); a `bool`/`INTEGER` column defaults to Go's zero value (`false`/`0`) if some code path never sets it, so prefer naming new flags so the zero value is the safe one (e.g. `disabled` not `active`) rather than auditing every construction site.
- **`internal/phonebook`**: read-side service joining contacts with their group-number associations (`ContactWithGroups`, `GroupWithMembers`), plus `GroupByDepartment` (always alphabetical) for the grouped index view.
- **`internal/pbx`**: screen-scraping (nessuna API ufficiale) della web console del centralino Invidea "ViVo" — sync orario opzionale, stesso giro di `ldap.SyncContacts`, attivo solo se configurato da `/admin/pbx` (nessuna env var: URL/utente/password vivono solo in `app_config`). **A logged-in session still can't hit a module URL (`vivo.index.php?module=...`) directly** — it bounces to a blank frame-buster page unless the request's `Referer` is the exact "_view" landing page that precedes it in the real frameset (`sip&mode=peers_view` → `monitor&mode=peers`, `extensions&mode=callgroups_view` → `extensions&mode=callgroups`); `Client.getWithLanding` replicates this two-hop navigation — don't call the module endpoints with a bare `Referer: vivo.php`. Peers SIP non coperti da un contatto `source='ldap'` (stesso `ldap_ext`, qualunque `disabled`) diventano `contacts` `source='pbx'`. Call group del centralino estendono `group_numbers` (colonne `source`/`name_override`/`area`): il nome è overridabile in locale, ma il mapping membri è sempre sovrascritto dal centralino. `/admin/pbx` mostra anche diagnostica calcolata dall'ultimo sync riuscito (`pbx.SyncResult`): interni con nome corrispondente ma mancante da un lato, nomi disallineati sullo stesso interno, ed "interni riciclabili" (stesso interno+nome, ma il titolare in AD è `disabled`). Vedi `docs/superpowers/specs/2026-09-13-pbx-scraping-design.md`.
- **Areas can carry a numeric range** (`areas.range_start`/`range_end`, editable in `/admin/areas`): a contact or PBX call group with no area otherwise assigned whose numeric extension falls in `[start, end]` is auto-assigned that area via `database.MatchExtensionRange`, and — if it had no department/name either — that area's name too, so nothing lands unlabeled. Applied in `ldap.SyncContacts`, `pbx.ApplyPeers` and `pbx.ApplyCallGroups`.
- **Call groups (`group_numbers`) appear in the public directory** alongside contacts (`search_results.html`, expandable via native `<details>`, no JS) under the reserved area key `"uffici"` (auto-seeded idempotently in `migrate()`) — a contact row also shows a popover of the call groups it belongs to. A PBX peer whose extension isn't purely numeric (a trunk/gateway line, e.g. `"gw"`) is never turned into a contact (`isNumericExtension` in `internal/pbx/sync.go`).
- **`internal/carddav`**: implements CardDAV (`PROPFIND`/`REPORT`) as its own sub-router mounted at `/carddav`, independent of the mux routes in `main.go`. vCard generation logic is duplicated between here and `generateVCard` in `main.go` — keep both in sync if the vCard format changes.
- **`internal/i18n`**: locale resolved per-request (`i18n.ResolveLocale`); templates receive a `Messages` map, not a Go i18n library.
- **Areas are a DB-editable entity, not a fixed enum**: the `areas` table (CRUD via `/admin/areas`) replaces the old hardcoded Interni/Esterni/Politica. `contacts.area` stores an area's `key`; deleting an area clears it (`""`) from any contact using it rather than leaving a dangling reference. OU → Area assignment is a separate admin screen (`/admin/ou-mapping`).
- **`contacts.source`** is `'ldap'` (default, from sync) or `'manual'` (created via `/admin/local-contacts`, UID prefixed `manual-` so it can never collide with a real LDAP `uid`). Manual contacts appear in the public directory through the same queries as LDAP ones — no separate code path for listing/search.
- **Admin is multiple pages, each with its own route and full HTML shell**, not one page with hx-get fragments: `/admin` (Panoramica), `/admin/groups`, `/admin/ou-mapping`, `/admin/areas`, `/admin/local-contacts`. Every page embeds `web/templates/rail.html` (shared sidebar, needs `AreaCounts`/`Total`/`Areas`/`Username`/`Section` in its template data — see `railData()` in `main.go`) and a content `<div id="...">` that also doubles as the HTMX swap target for that section's create/update/delete handlers (which re-render only the fragment template, e.g. `admin_groups.html`, not the full page template `admin_page_groups.html`).
- **`web/templates`**: parsed once via `template.ParseGlob("web/templates/*.html")` at startup (restart to see edits). Files are addressable by basename in `{{template "name.html" .}}`, which is how pages compose `rail.html` and fragments — but a nested `{{range}}` shadows the outer `.`, so capture what you need with `{{$x := .}}` *before* entering a nested range if you'll need the outer value inside it. Template funcs of note: `initials` (2-letter avatar), `sentenceCase` (only touches all-caps strings — never re-cases an already mixed-case label), `substr` (uppercase-only).
- Group **phone numbers** (`group_numbers` table) are a separate concept from contacts: "group numbers" (e.g. Reception) are managed in `/admin/groups` and linked to contacts via a many-to-many `group_members` table; the member picker there is search-to-add (click a result to add), not a raw contact-ID field.

## Gotchas

- `go-sqlite3` requires CGO; cross-compiling or using `CGO_ENABLED=0` will fail the build. On a Windows box with no C compiler, build/test inside a container (see Commands above).
- Templates and static assets are loaded from relative paths (`web/templates`, `web/static`) — run binaries from the repo root, not from an arbitrary `cmd/server` working directory.
- `SESSION_SECRET` defaults to `change-me-in-production`; must be overridden for any real deployment.
