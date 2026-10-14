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
- Un contatto ha **una sola area** (vince la prima in ordine) ma **zero o più
  sotto-aree** dell'area: tutte quelle le cui regole soddisfa. Le sotto-aree
  possono sovrapporsi (es. `Dirigente` + `Protezione civile`).
- Regole valutate **nel sync**, risultato salvato nel DB; salvare una
  regola avvia subito un sync.
- `/admin/ou-mapping` confluisce in `/admin/areas`.
- CardDAV: una rubrica per area/sotto-area, `LDAP_OU_FILTERS` eliminato.
- Anche il **perimetro della rubrica** (chi entra nel sync, oggi
  `LDAP_ALLOWED_GROUPS` in `.env`) diventa un insieme di regole da UI, con
  le stesse regole e protezioni contro lo svuotamento accidentale.
  `LDAP_ALLOWED_GROUPS` eliminato.
- **Tag della sotto-area** sulla riga contatto nella rubrica pubblica.
- Fuori scope (voluto): server LDAP dei telefoni — il telefono deve solo
  mostrare il nome di chi chiama, nessuna area/sotto-area esposta; formato
  vCard invariato.

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

-- sotto-aree di un contatto (molti-a-molti)
CREATE TABLE IF NOT EXISTS contact_sub_areas (
    contact_id   INTEGER NOT NULL REFERENCES contacts(id),
    sub_area_key TEXT NOT NULL,
    PRIMARY KEY (contact_id, sub_area_key)
);

-- perimetro della rubrica (chi entra nel sync LDAP), stesse colonne
CREATE TABLE IF NOT EXISTS scope_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    attr       TEXT NOT NULL DEFAULT '',
    value      TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);
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

// Resolve: prima area di primo livello (in ordine) di cui p è membro ("" se
// nessuna), poi TUTTE le sotto-aree di quell'area di cui p è membro, in
// ordine di sort (nil se nessuna).
func Resolve(p Profile, tree []Node) (area string, subAreas []string)
```

- Profili senza dati AD (PBX, gruppi di chiamata) soddisfano solo `range`;
  `present` su di loro fallisce, `absent` passa.
- I gruppi di chiamata ricevono solo l'area di primo livello
  (`group_numbers.area`), nessuna sotto-area.

## 3. Sync

`ldap.SyncContacts`:

1. carica albero aree + regole e regole di perimetro;
2. attributi richiesti ad AD = quelli già sincronizzati ∪ attributi citati
   da regole `attr`/`present`/`absent`;
3. per ogni DN citato da regole `adgroup`: una ricerca
   `(memberOf:1.2.840.113556.1.4.1941:=<DN escaped>)` → insieme dei DN
   utenti membri (annidati compresi). Gruppo inesistente o errore: log
   `[SYNC]`, insieme vuoto, sync prosegue;
4. per ogni utente costruisce `Profile`; se ci sono regole di perimetro e
   l'utente non è `Member` delle regole di perimetro, viene scartato (come
   oggi con `isEntryAllowedByGroups`, che viene rimossa). Nessuna regola di
   perimetro = nessun filtro (come oggi con `LDAP_ALLOWED_GROUPS` vuoto).
   Gli account `disabled` non passano dal filtro di perimetro (comportamento
   attuale: AD svuota spesso il `memberOf` di chi viene disabilitato);
5. per ogni utente ammesso chiama `Resolve` → `contacts.area` e righe di
   `contact_sub_areas` (sostituite in blocco per quel contatto: delete +
   insert nella stessa transazione dell'upsert). Nessuna area → `""`
   (comportamento attuale), nessuna sotto-area → nessuna riga.
   Department vuoto → nome dell'area (oggi solo per i match da range; ora
   per qualsiasi regola — cambio voluto: nessun contatto senza etichetta);
6. sostituisce `deriveArea` e `MatchExtensionRange` (rimossi);
7. **protezione svuotamento**: prima di `SoftDeleteStale`, se i contatti LDAP
   attivi che verrebbero soft-deleted sono più del 20% di quelli attivi
   (e almeno 5), il soft-delete **non** viene eseguito: lo stato del sync
   riporta "Calo anomalo: N contatti uscirebbero dalla rubrica" e in
   `/admin/areas` → Perimetro compare il pulsante "Conferma e applica", che
   rilancia il sync con il soft-delete forzato. Upsert e aree vengono
   comunque aggiornati.

`pbx.ApplyPeers` / `pbx.ApplyCallGroups`: area dai soli `range` tramite
`Resolve` con profilo senza dati AD.

Nuova `ReapplyRangeAreas(db)`: riapplica `Resolve` a contatti `source='pbx'`
e `group_numbers` dal DB, senza scraping.

Contatti `source='manual'`: area/sotto-area scelte a mano in
`/admin/local-contacts` (aggiungere checkbox multiple delle sotto-aree
dell'area scelta), mai toccate
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

Perimetro: la conversione richiede AD (i CN di `LDAP_ALLOWED_GROUPS` vanno
risolti in DN), quindi avviene **al primo sync** dopo il deploy, se
`app_config` non ha `scope_rules_migrated=1`: ogni CN di
`LDAP_ALLOWED_GROUPS` viene cercato in AD (`(&(objectClass=group)(cn=<CN>))`)
e il DN trovato diventa una regola `adgroup`; un CN inesistente viene
scartato con log (es. `all_users-servizi_esterni`, che non esiste). Poi
`scope_rules_migrated=1`. Da quel momento la variabile è ignorata e va tolta
da `.env.example`, `docker-compose.yml` e `config.go`.

Differenza: oggi conta solo la membership diretta, `adgroup` include gli
annidati. Verificato il 2026-10-14 su `ALL_USERS-SERVIZI_INTERNI`: 248
diretti, 248 con annidati — nessun cambio.

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
Eliminare un'area elimina sotto-aree e regole, azzera `area` dei contatti
che la usavano ed elimina le righe `contact_sub_areas` delle sue
sotto-aree. Eliminare una sotto-area elimina le sue righe
`contact_sub_areas`. Il `sort` delle sotto-aree decide solo l'ordine di
visualizzazione (rail e tag), non la precedenza.

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

**Perimetro della rubrica** (sezione in cima a `/admin/areas`): stesse
regole e stessi suggerimenti delle aree, tipi `ou`, `attr`, `adgroup`,
`user`, `exclude`, `present`, `absent` (niente `range`: vale solo per AD).
Testo esplicativo: "Solo gli utenti AD che soddisfano queste regole entrano
in rubrica. Nessuna regola = tutti gli utenti AD".

- **Anteprima** prima di applicare: esegue ricerca AD + valutazione senza
  scrivere nulla e mostra "Con queste regole entrano N utenti attivi (oggi
  M in rubrica)", più fino a 30 nomi di chi entrerebbe e di chi uscirebbe.
- Le modifiche al perimetro avviano il sync come le altre regole, soggetto
  alla protezione svuotamento (sezione 3).

## 5. Rubrica pubblica

- `railData()` fornisce l'albero e i conteggi per area e sotto-area.
- `rail.html`: sotto-aree come voci indentate sotto la madre, in entrambe
  le varianti (bottoni HTMX e link).
- `/search?group=<area>&sub=<sub>`: filtro sui contatti con una riga
  `contact_sub_areas` per `<sub>`; con `sub`
  i gruppi di chiamata non sono mostrati. L'area madre mostra tutti i suoi
  contatti.
- `CountByArea` affiancato da conteggio per sotto-area (contatti attivi
  distinti). Poiché le sotto-aree si sovrappongono, la somma dei conteggi
  delle sotto-aree può superare quello dell'area: è atteso.
- **Tag sotto-area**: nella riga contatto di `search_results.html`, accanto
  al nome, un'etichetta per ogni sotto-area del contatto (es. `Dirigente`
  `Protezione civile`), in ordine di `sort`; nessun tag se non ne ha.
  `ContactWithGroups` riceve `SubAreas []SubAreaLabel{Key, Name}`, caricate
  in un'unica query per tutti i risultati (niente N+1). Stile: pillola
  piccola, coerente con le altre etichette della riga.

## 6. CardDAV

- `/carddav/{book}/`: `book` = key di area di primo livello (tutti i contatti
  con quella `area`) o di sotto-area (contatti con una riga
  `contact_sub_areas` per quella key: un contatto può stare in più
  rubriche); key
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
| Perimetro: troppi contatti in uscita | soft-delete sospeso, avviso + "Conferma e applica" |
| AD giù durante l'anteprima del perimetro | messaggio "AD non disponibile", nessuna modifica |
| Sync già in corso al salvataggio | nessun secondo sync; la modifica è presa dal sync in corso o dal prossimo (flag "risincronizza al termine") |

## 8. Rollout

1. Deploy: migrazione → stesse aree di prima, nessun effetto visibile. Al
   primo sync `LDAP_ALLOWED_GROUPS` diventa regole di perimetro; verificare
   in `/admin/areas` → Perimetro, poi togliere la variabile dallo stack
   Portainer.
2. Admin rinomina `interni` in "Personale comunale".
3. Crea le sotto-aree e le regole (es. Dirigenti = gruppo AD
   `RUBRICA_DIRIGENTI` oppure `description = Dirigente`; Collaboratori = …).
4. Controlla "senza sottogruppo" e completa le regole o i gruppi in AD.
5. Client CardDAV: nessuna azione; nuove rubriche per sotto-area disponibili
   su `/carddav/<key>/`.

## 9. Test

- `arearules`: `Member` per ogni tipo, exclude vince, requisiti AND,
  maiuscole/spazi, DN equivalenti, `range` con interni multipli e non
  numerici, area senza regole; `Resolve`: precedenza tra aree, più
  sotto-aree restituite tutte in ordine, sotto-aree di un'altra area
  ignorate,
  profilo PBX (solo range).
- `database`: migrazione da `ou_area_mapping` + range (una sola volta,
  idempotente), CRUD regole, cascade eliminazione area, `sort`, rifiuto
  annidamento a due livelli, `contact_sub_areas` sostituite in blocco al
  sync e ripulite all'eliminazione di area/sotto-area.
- `ldap`: costruzione filtro in-chain con escape; elenco attributi richiesti.
- `carddav`: rubrica per area, per sotto-area, key sconosciuta 404.
- `ldap`: perimetro (nessuna regola = tutti, utente fuori scartato,
  disabled non filtrato), protezione svuotamento (sotto/sopra soglia,
  forzatura), migrazione `LDAP_ALLOWED_GROUPS` (CN trovato/inesistente,
  una sola volta).
- `main`: `/search` con `group`+`sub`; tag sotto-area nel risultato; avvio
  sync dopo modifica regole; anteprima perimetro con directory finta.

## 10. Documentazione

Aggiornare `CLAUDE.md` (bullet Areas, `internal/ldap`, `internal/carddav`,
rimozione `LDAP_OU_FILTERS` e `LDAP_ALLOWED_GROUPS`, perimetro e protezione
svuotamento), `.env.example`, `docker-compose.yml`, memoria "Gruppi AD vs
filtro sync" (diventa obsoleta).
