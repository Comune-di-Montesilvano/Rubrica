# Aree e sotto-aree definite da regole — Design

## Contesto e obiettivo

L'area `interni` (da rinominare "Personale comunale", key invariata) contiene
tutto il personale dell'ente e va suddivisa in sotto-aree — Dipendenti,
Dirigenti, Collaboratori, … — navigabili dalla rubrica pubblica.

Oggi l'assegnazione dell'area è in parte fissa nel codice:
`DefaultOUAreaMapping`, `ClassificationOU` (salta a mano le OU `Users` e
`COMUNE-MS`), range numerici come colonne di `areas`, rubriche CardDAV
filtrate per sottostringa del DN da `LDAP_OU_FILTERS` in `.env`.

Verifica sul dominio (2026-10-14): nessun attributo AD distingue oggi i ruoli
in modo affidabile (`title`, `department`, `manager`, `extensionAttribute*`
vuoti; `employeeType` = `COMUNE-MS_STD` per tutti; `description` mescola
ruolo, ufficio e stato). Serve quindi la libertà di combinare più fonti.

**Obiettivo:** aree e sotto-aree definite **interamente da interfaccia, senza
nulla di fisso nel codice**, con regole su Active Directory e sugli interni,
sul modello dei "gruppi della plancia" di CruscottoPA
(`internal/audience`, spec `2026-10-06-filtri-contenuti-design.md`).

## Decisioni

- Un solo motore di regole per aree di primo livello e sotto-aree.
- Un livello di annidamento (area → sotto-area).
- Regole valutate **nel sync**, risultato salvato su `contacts`; salvare una
  regola avvia subito un sync.
- `/admin/ou-mapping` confluisce in `/admin/areas`.
- CardDAV: una rubrica per area/sotto-area, `LDAP_OU_FILTERS` eliminato.
- Fuori scope: `LDAP_ALLOWED_GROUPS` in UI (spec successiva), badge ruolo
  sulla riga contatto, LDAP telefoni, formato vCard.

## 1. Dati

```sql
-- areas: nuove colonne (ALTER TABLE ... ADD COLUMN in migrate())
parent_key TEXT NOT NULL DEFAULT ''   -- '' = area di primo livello
sort       INTEGER NOT NULL DEFAULT 0

CREATE TABLE IF NOT EXISTS area_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    area_id    INTEGER NOT NULL REFERENCES areas(id),
    kind       TEXT NOT NULL,               -- vedi tabella tipi
    attr       TEXT NOT NULL DEFAULT '',    -- nome attributo (attr/present/absent)
    value      TEXT NOT NULL DEFAULT '',    -- valore, DN, OU, uid, "700-799"
    created_at DATETIME NOT NULL
);

-- contacts: nuova colonna
sub_area TEXT NOT NULL DEFAULT ''
```

- `parent_key` di una sotto-area punta a un'area di primo livello; una
  sotto-area non può avere figlie (validato lato server).
- `areas.range_start`/`range_end` restano nello schema (SQLite, nessun DROP)
  ma non sono più letti dopo la migrazione.
- L'area riservata `uffici` (gruppi di chiamata) resta com'è.

### Tipi di regola

| kind | significato | valutata su |
|---|---|---|
| `ou` | il DN contiene `OU=<value>` in qualsiasi posizione (senza maiuscole) | contatti AD |
| `attr` | attributo `attr` ha valore `value` (uno qualsiasi dei valori) | contatti AD |
| `adgroup` | membro del gruppo AD `value` (DN), **annidati compresi** | contatti AD |
| `range` | un interno numerico del contatto/gruppo in `[da, a]` (`value` = `"700-799"`) | AD, PBX, gruppi di chiamata |
| `user` | utente `value` (uid / sAMAccountName) incluso | contatti AD |
| `exclude` | utente `value` escluso | contatti AD |
| `present` / `absent` | requisito: attributo `attr` compilato / vuoto | contatti AD |

Normalizzazione al salvataggio: uid minuscoli, DN in forma canonica minuscola
(`ldap.ParseDN`), OU maiuscola trimmata, range validato (`da <= a`, interi).
Nomi attributo: `^[A-Za-z][A-Za-z0-9-]{0,63}$`.

## 2. Appartenenza e precedenza (logica pura)

Nuovo package `internal/arearules`, senza dipendenze da DB/LDAP:

```go
type Profile struct {
    Username   string              // minuscolo; "" per PBX e gruppi di chiamata
    DN         string
    Attrs      map[string][]string // nome attributo minuscolo → valori
    Groups     []string            // DN gruppi AD citati nelle regole di cui è membro (annidati compresi)
    Extensions []string            // interni (già divisi da ';')
}

type Rule struct{ Kind, Attr, Value string }

// Member: almeno una regola d'ingresso (ou/attr/adgroup/range/user)
// soddisfatta, tutti i requisiti (present/absent) rispettati, nessuna exclude.
// Un'area senza regole non ha membri.
func Member(p Profile, rules []Rule) bool

type Node struct {
    Key      string
    Rules    []Rule
    Children []Node // ordinati per sort
}

// Resolve: prima area di primo livello (in ordine) di cui p è membro, poi
// prima sotto-area di quell'area di cui p è membro. "" se nessuna.
func Resolve(p Profile, tree []Node) (area, subArea string)
```

- Profili senza dati AD (PBX, gruppi di chiamata) soddisfano solo `range`;
  `present` su di loro fallisce, `absent` passa.
- I gruppi di chiamata ricevono solo l'area di primo livello
  (`group_numbers.area`), nessuna sotto-area.

## 3. Sync

`ldap.SyncContacts`:

1. carica albero aree + regole;
2. attributi richiesti ad AD = quelli già sincronizzati ∪ attributi citati
   da regole `attr`/`present`/`absent`;
3. per ogni DN citato da regole `adgroup`: una ricerca
   `(memberOf:1.2.840.113556.1.4.1941:=<DN escaped>)` → insieme dei DN
   utenti membri (annidati compresi). Gruppo inesistente o errore: log
   `[SYNC]`, insieme vuoto, sync prosegue;
4. per ogni utente costruisce `Profile` e chiama `Resolve` → `contacts.area`,
   `contacts.sub_area`. Nessuna area → `""` (comportamento attuale).
   Department vuoto → nome dell'area (oggi solo per i match da range; ora
   per qualsiasi regola — cambio voluto: nessun contatto senza etichetta);
5. sostituisce `deriveArea` e `MatchExtensionRange` (rimossi).

`pbx.ApplyPeers` / `pbx.ApplyCallGroups`: area dai soli `range` tramite
`Resolve` con profilo senza dati AD.

Nuova `ReapplyRangeAreas(db)`: riapplica `Resolve` a contatti `source='pbx'`
e `group_numbers` dal DB, senza scraping.

Contatti `source='manual'`: area/sotto-area scelte a mano in
`/admin/local-contacts` (aggiungere il selettore di sotto-area), mai toccate
dalle regole.

**Dopo ogni modifica a regole o albero** (create/update/delete/sposta):
avvio sync LDAP in background (stesso worker e stesso stato di
`handleAdminSync`, nessun sync concorrente) + `ReapplyRangeAreas`.

### Migrazione una tantum

In `migrate()`, se `app_config` non ha `area_rules_migrated=1`:

- ogni voce di `ou_area_mapping` (o `DefaultOUAreaMapping` se assente)
  `OU → key` diventa una regola `ou` sull'area `key`, se esiste;
- ogni area con `range_start`/`range_end` valorizzati diventa una regola
  `range`;
- imposta `sort` delle aree esistenti in ordine alfabetico;
- scrive `area_rules_migrated=1`.

Risultato atteso: stesse aree di prima su tutti i contatti. Unica differenza
nota: `ou` ora corrisponde a qualsiasi OU del DN, non solo alla prima non
"wrapper" — con l'albero attuale (`OU=Users,OU=<CLASSE>,OU=COMUNE-MS`) è
equivalente.

## 4. Admin

`/admin/areas` (pagina `admin_page_areas.html`, frammento `admin_areas.html`,
pattern HTMX esistente). Rimossi `/admin/ou-mapping`, i relativi template e
la voce nella rail.

**Elenco:** albero aree → sotto-aree; per ognuna nome, key, numero regole,
numero contatti; nuova (area o sotto-area di …), rinomina, su/giù, elimina.
Eliminare un'area elimina sotto-aree e regole e azzera `area`/`sub_area`
dei contatti che la usavano.

**Dettaglio area** (`/admin/areas/{id}`):

- regole con etichetta leggibile ("OU INTERNI", "Gruppo AD
  RUBRICA_DIRIGENTI", "description = Dirigente", "Interni 700–799",
  "Utente mario.rossi", "Escludi stagista1", "Richiede mail compilato") e
  pulsante "Togli";
- aggiunta regola: tipo → campi; suggerimenti HTMX dal vivo su AD:
  `/admin/ad/ou?q=`, `/admin/ad/gruppi?q=`, `/admin/ad/attributi?q=`
  (statistiche alla `attrstats` di CruscottoPA: nome, quanti utenti, 3
  valori più frequenti), `/admin/ad/valori?attr=&q=`, `/admin/ad/utenti?q=`;
  risposta = lista di opzioni cliccabili;
- membri attuali (max 30 nomi + totale) da `contacts`;
- se l'area ha sotto-aree: contatti dell'area "senza sottogruppo".

## 5. Rubrica pubblica

- `railData()` fornisce l'albero e i conteggi per area e sotto-area.
- `rail.html`: sotto-aree come voci indentate sotto la madre, in entrambe
  le varianti (bottoni HTMX e link).
- `/search?group=<area>&sub=<sub>`: filtro su `contacts.sub_area`; con `sub`
  i gruppi di chiamata non sono mostrati. L'area madre mostra tutti i suoi
  contatti.
- `CountByArea` affiancato da conteggio per sotto-area.

## 6. CardDAV

- `/carddav/{book}/`: `book` = key di area di primo livello (tutti i contatti
  con quella `area`) o di sotto-area (contatti con quella `sub_area`); key
  sconosciuta → 404. `""`/`all` invariati.
- Rimossi `LDAPOUFilters`, `defaultOUFilters`, `LDAP_OU_FILTERS`
  (`.env.example`, `docker-compose.yml`, `config.go`) e il match per
  sottostringa in `contactMatchesBook`.
- `bookDisplayName`: nome dell'area dal DB.
- Le key `interni`/`esterni`/`politica` non cambiano: i client già
  configurati continuano a funzionare.

## 7. Errori

| Caso | Comportamento |
|---|---|
| AD giù durante i suggerimenti | messaggio "AD non disponibile"; salvataggio regole funziona |
| Gruppo AD di una regola sparito | regola resta, nessun membro; log `[SYNC]` |
| Area senza regole | nessun membro (salvo manuali) |
| Nome attributo non valido / range malformato | 422 con messaggio |
| Sotto-area di una sotto-area | 422 |
| Sync già in corso al salvataggio | nessun secondo sync; la modifica è presa dal sync in corso o dal prossimo (flag "risincronizza al termine") |

## 8. Rollout

1. Deploy: migrazione → stesse aree di prima, nessun effetto visibile.
2. Admin rinomina `interni` in "Personale comunale".
3. Crea le sotto-aree e le regole (es. Dirigenti = gruppo AD
   `RUBRICA_DIRIGENTI` oppure `description = Dirigente`; Collaboratori = …).
4. Controlla "senza sottogruppo" e completa le regole o i gruppi in AD.
5. Client CardDAV: nessuna azione; nuove rubriche per sotto-area disponibili
   su `/carddav/<key>/`.

## 9. Test

- `arearules`: `Member` per ogni tipo, exclude vince, requisiti AND,
  maiuscole/spazi, DN equivalenti, `range` con interni multipli e non
  numerici, area senza regole; `Resolve` precedenza area e sotto-area,
  profilo PBX (solo range).
- `database`: migrazione da `ou_area_mapping` + range (una sola volta,
  idempotente), CRUD regole, cascade eliminazione area, `sort`, rifiuto
  annidamento a due livelli, `sub_area` con default `''`.
- `ldap`: costruzione filtro in-chain con escape; elenco attributi richiesti.
- `carddav`: rubrica per area, per sotto-area, key sconosciuta 404.
- `main`: `/search` con `group`+`sub`; avvio sync dopo modifica regole.

## 10. Documentazione

Aggiornare `CLAUDE.md` (bullet Areas, `internal/ldap`, `internal/carddav`,
rimozione `LDAP_OU_FILTERS`), `.env.example`, `docker-compose.yml`.
