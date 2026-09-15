# Raggruppamento gerarchico delle chiamate di gruppo

Data: 2026-09-15

## Problema

La colonna "Chiamate di gruppo" (introdotta in `search_results.html`, affiancata ai contatti) mostra oggi tutti i gruppi di chiamata del centralino come lista piatta, ordinata per numero interno. Con 100+ gruppi la lista è difficile da scorrere. Il comune vuole un raggruppamento gerarchico simile a un organigramma — es. "Uffici" (400-499) contiene "Settore VI - Legale" (401-404), che contiene le singole chiamate 401/402/403 — mentre categorie come "Dirigenti" (300-350) restano di primo livello, senza figli.

## Non-obiettivi

- Non tocca il raggruppamento dei **contatti** (`phonebook.GroupByDepartment`, sempre per Department/Area) — solo la colonna "Chiamate di gruppo".
- Non tocca `contacts.area` né la tabella `areas` esistente (usata per contatti e OU mapping) — entità separata, per decisione esplicita (vedi Decisioni).
- Non tocca `internal/pbx` (sync, `ApplyCallGroups`, screen-scraping) — il matching categoria/range è calcolato solo a display-time.

## Decisioni

- **Entità separata da `areas`**: nuova tabella `group_categories`, dedicata solo a raggruppare `group_numbers`. Motivo: isolamento — `areas` serve contatti/OU-mapping, riusarla per una gerarchia a 2 livelli avrebbe accoppiato due concetti (contatti vs. chiamate di gruppo) che oggi non si toccano.
- **Matching a display-time, non a sync-time**: nessuna colonna nuova su `group_numbers`. Ogni richiesta a `handleSearch` calcola la categoria (più specifica) di ogni gruppo al volo, dalla lista di `group_categories` già in cache/DB. Motivo: la gerarchia è pura presentazione, non deve toccare il flusso di sync PBX né richiedere un re-sync quando un admin modifica le categorie.
- **Range più specifico vince**: se un'estensione cade sia nel range di un'area genitore sia in quello di un figlio, vince il figlio (range più stretto). `MatchExtensionRange` (usata anche da `ldap`/`pbx` per `areas`) ha oggi un bug latente — ritorna il primo match nell'ordine di iterazione, non il più specifico — che va corretto **solo per il nuovo matching di `group_categories`**; non tocchiamo il comportamento esistente su `areas` per non introdurre regressioni nel sync (fuori scope, va gestito come fix separato se necessario).
- **Admin dedicato**: nuova pagina `/admin/group-categories`, stesso pattern CRUD di `/admin/areas`.
- **Non categorizzati**: gruppi il cui numero non cade in nessun range → bucket "Altre chiamate", sempre in fondo alla colonna.
- **Ordinamento**: per `range_start` crescente a ogni livello (non alfabetico).

## Modello dati

Nuova tabella (in `migrate()`, `internal/database`):

```sql
CREATE TABLE IF NOT EXISTS group_categories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    parent_id INTEGER REFERENCES group_categories(id),
    range_start INTEGER,
    range_end INTEGER,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
```

`parent_id` NULL = categoria di primo livello. `range_start`/`range_end` NULL = nessuna regola (categoria puramente organizzativa, mai auto-assegnata — coerente con `areas.RangeStart`/`RangeEnd` esistenti).

Go struct in `internal/database`:

```go
type GroupCategory struct {
    ID         int64
    Key        string
    Name       string
    ParentID   *int64
    RangeStart *int
    RangeEnd   *int
    CreatedAt  time.Time
    UpdatedAt  time.Time
}
```

CRUD: `CreateGroupCategory`, `UpdateGroupCategory`, `DeleteGroupCategory` (elimina, i figli restano orfani → `parent_id` va pulito a NULL, stesso pattern di `DeleteArea` che pulisce `contacts.area`), `ListGroupCategories`, `GetGroupCategory`.

Validazione anti-ciclo in `UpdateGroupCategory`: prima di salvare, risali la catena `parent_id` proposta e rifiuta se incontri l'id della categoria stessa.

## Matching

Nuova funzione, analoga a `MatchExtensionRange` ma che sceglie il range più specifico invece del primo match:

```go
// MatchGroupCategory ritorna la group_category più specifica (range più
// stretto) il cui [RangeStart, RangeEnd] contiene ext, o nil se nessuna
// corrisponde. A differenza di MatchExtensionRange (che ritorna il primo
// match e non gestisce gerarchie), qui più categorie annidate possono
// coprire lo stesso interno — vince quella con range più stretto.
func MatchGroupCategory(ext string, categories []*GroupCategory) *GroupCategory
```

Usata in `cmd/server/main.go`, `handleSearch`, dopo aver caricato `allGroups` (già filtrati a `ActiveMembers()>0`, vedi fix precedente): per ognuno, `MatchGroupCategory(g.Group.Number, categories)` determina la categoria leaf.

## Costruzione albero (display)

Nuovo helper (in `phonebook` o direttamente in `main.go`, valutare in fase di piano):

```go
type GroupCategoryNode struct {
    Category *database.GroupCategory // nil per il nodo radice "Altre chiamate"
    Children []*GroupCategoryNode
    Groups   []*phonebook.GroupWithMembers
}

func BuildGroupCategoryTree(groups []*phonebook.GroupWithMembers, categories []*database.GroupCategory) []*GroupCategoryNode
```

Algoritmo: per ogni gruppo calcola la leaf category (`MatchGroupCategory`); i gruppi senza leaf vanno nel bucket finale "Altre chiamate" (nodo speciale, sempre ultimo, `Category == nil`). Costruisce l'albero delle categorie effettivamente usate (categorie senza alcun gruppo, diretto o discendente, non compaiono — niente sezioni vuote). Ordina figli e gruppi per `RangeStart`/numero crescente ad ogni livello.

## UI

`search_results.html`, sezione "Chiamate di gruppo": sostituire il `{{range .CallGroups}}` piatto con un render ricorsivo dell'albero — `<details>` annidati (stesso stile visivo di oggi, un livello di indentazione in più per figli), leaf category senza figli mostra le righe gruppo direttamente. Nessun JS aggiuntivo (stesso pattern `<details>` nativo già in uso).

`/admin/group-categories`: nuova pagina admin, stesso schema di `/admin/areas` (CRUD form + tabella), con select "Categoria genitore" (opzionale, popolata dalle categorie esistenti escludendo se stessa e i propri discendenti in edit).

## Rischi / punti aperti per il piano di implementazione

- Conteggio "membri" per categoria (se serve un badge aggregato) non è nello scope iniziale — la colonna mostra solo i gruppi foglia con il loro conteggio esistente.
- Nessuna migrazione dati richiesta (tabella nuova, vuota finché l'admin non crea categorie) — comportamento identico a oggi finché `/admin/group-categories` è vuoto (bucket "Altre chiamate" contiene tutto).
