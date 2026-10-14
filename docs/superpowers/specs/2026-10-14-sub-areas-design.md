# Sotto-aree da gruppi AD — Design

## Contesto

L'area `interni` (da rinominare "Personale comunale" da `/admin/areas`, key
invariata) contiene tutto il personale dell'ente. Serve suddividerla in
sottogruppi — Dipendenti, Dirigenti, Collaboratori, … — navigabili dalla
rubrica pubblica.

Verifica sul dominio (2026-10-14): in AD non esiste oggi alcun attributo
affidabile per distinguere i ruoli. `title`, `department`, `manager`,
`extensionAttribute*` sono vuoti; `employeeType` vale `COMUNE-MS_STD` per
tutti; `description` mescola ruolo, ufficio e stato ("Dirigente",
"Anagrafe", "PENSIONATO"). Nessun gruppo esistente corrisponde ai ruoli.

Decisione: **ogni sotto-area è mappata su un gruppo AD dedicato**, creato e
mantenuto dal sistemista (es. `RUBRICA_DIPENDENTI`, `RUBRICA_DIRIGENTI`,
`RUBRICA_COLLABORATORI`). AD resta fonte di verità; nessuna sotto-area
"residua"/predefinita.

## Requisiti

- Un'area può avere zero o più sotto-aree, configurabili da admin senza
  codice (meccanismo generico, non specifico di `interni`).
- Ogni sotto-area è associata a esattamente un gruppo AD (CN).
- Un contatto appartiene alla sotto-area il cui gruppo compare nel suo
  `memberOf` diretto, solo se il contatto è nell'area madre.
- Un contatto può essere in una sola sotto-area: in caso di più gruppi vince
  quella con `sort` più basso.
- Un contatto dell'area madre in nessun gruppo mappato resta visibile
  nell'area madre e viene segnalato in admin come "senza sottogruppo".
- La rubrica pubblica mostra le sotto-aree come voci annidate sotto l'area
  madre nella rail, con conteggio, filtrabili.

## Architettura

### Dati

Nuova tabella, creata in `migrate()` con `CREATE TABLE IF NOT EXISTS`,
nessun seed:

```sql
CREATE TABLE IF NOT EXISTS sub_areas (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    area_key TEXT NOT NULL,
    ldap_group TEXT NOT NULL DEFAULT '',
    sort INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
```

- `area_key` → `areas.key`. `DeleteArea` elimina anche le sotto-aree con
  quell'`area_key`.
- `ldap_group` salvato minuscolo e trimmato (stesso formato dei CN in
  `contacts.ldap_groups`, vedi `extractCNFromDN`).
- La tabella `areas` e la colonna `contacts.area` non cambiano.

### Risoluzione (calcolata in lettura, mai salvata)

La sotto-area di un contatto **non è persistita**: si calcola da
`contacts.ldap_groups` (già popolato dal sync con i CN di `memberOf`) e
dalla configurazione corrente. Un cambio di configurazione ha effetto
immediato, senza attendere il sync e senza colonne nuove su `contacts`.

Funzione pura in `internal/database`:

```go
// ResolveSubArea ritorna la key della sotto-area del contatto, "" se nessuna.
// subAreas deve essere ordinato per sort, poi name.
func ResolveSubArea(c *Contact, subAreas []*SubArea) string
```

1. considera solo le sotto-aree con `AreaKey == c.Area`;
2. divide `c.LDAPGroups` su `,`, trim + lowercase;
3. ritorna la key della prima sotto-area (in ordine) il cui `LDAPGroup` è
   tra i gruppi del contatto;
4. altrimenti `""`.

Contatti `manual`/`pbx` non hanno `ldap_groups` → sempre `""`.

### Database API

- `type SubArea struct { ID; Key; Name; AreaKey; LDAPGroup; Sort; CreatedAt; UpdatedAt }`
- `ListSubAreas() ([]*SubArea, error)` — ordinato `sort, name`
- `CreateSubArea`, `UpdateSubArea` (nome, gruppo, sort), `DeleteSubArea`
- `CountBySubArea(contacts, subAreas) map[string]int` — calcolato in Go su
  `ListAllContacts` (contatti attivi, già filtrati `disabled=0`).

### Admin (`/admin/areas`)

Sotto ogni area, elenco delle sue sotto-aree: nome, gruppo AD, ordine,
numero contatti, azioni modifica/elimina; form "aggiungi sotto-area".
Stesso pattern esistente: pagina `admin_page_areas.html`, frammento
`admin_areas.html` come target HTMX dei handler create/update/delete.

Key generata dal nome come per le aree. Validazione server: nome e gruppo
obbligatori, `area_key` esistente.

Diagnostica, per ogni area con almeno una sotto-area: elenco dei contatti
attivi dell'area con `ResolveSubArea == ""` ("senza sottogruppo") — indica
chi va aggiunto a un gruppo in AD.

### Rubrica pubblica

- `railData()` aggiunge `SubAreas` (raggruppate per `area_key`) e
  `SubAreaCounts`.
- `rail.html`: sotto ogni area, le sue sotto-aree come voci indentate
  (`rail-item` con classe di indentazione), entrambe le varianti (bottoni
  HTMX in `contacts`, link nelle pagine admin).
- Link: `/search?group=<area>&sub=<subkey>` (e `/?group=<area>&sub=<subkey>`).
- `handleSearch`: dopo il filtro per area, se `sub` è presente filtra i
  risultati con `ResolveSubArea(contact, subAreas) == sub`. Con `sub`
  valorizzato i gruppi di chiamata (area `uffici`) non vengono mostrati.
- L'area madre continua a mostrare tutti i suoi contatti.
- Stato attivo nella rail: voce sotto-area attiva se `ActiveSubArea`
  coincide; `setActiveArea` lato JS gestisce anche `data-sub`.

## Fuori scope

- CardDAV, server LDAP telefoni, vCard: invariati.
- Badge del ruolo sulla riga contatto.
- Unificazione rubriche CardDAV (`LDAP_OU_FILTERS`) con le Aree:
  intervento separato.
- Gruppi AD annidati: il sync legge solo `memberOf` diretto, i gruppi
  `RUBRICA_*` devono contenere direttamente gli utenti.

## Rollout

1. Deploy (tabella creata vuota: nessun effetto visibile).
2. Sistemista crea i gruppi `RUBRICA_*` in AD e li popola (membership
   diretta). `LDAP_ALLOWED_GROUPS` non va toccato.
3. Admin rinomina l'area `interni` in "Personale comunale" e crea le
   sotto-aree in `/admin/areas`.
4. Dopo il sync successivo, verificare la diagnostica "senza sottogruppo".

## Test

- `ResolveSubArea`: match singolo; più gruppi → vince `sort` più basso;
  gruppo presente ma area diversa → `""`; maiuscole/spazi nei CN;
  `ldap_groups` vuoto; nessuna sotto-area per l'area.
- DB: CRUD `sub_areas`; `DeleteArea` elimina le sotto-aree figlie;
  `ldap_group` normalizzato minuscolo.
- `handleSearch` con `group` + `sub`: filtra correttamente, esclude i
  gruppi di chiamata.
