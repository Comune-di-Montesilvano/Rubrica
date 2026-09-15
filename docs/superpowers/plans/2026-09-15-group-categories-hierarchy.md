# Group Categories Hierarchy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Raggruppare gerarchicamente (2 livelli) la colonna "Chiamate di gruppo" in `search_results.html`, es. "Uffici" (400-499) contenente "Settore VI - Legale" (401-404) contenente le singole chiamate.

**Architecture:** Nuova tabella `group_categories` (entità isolata da `areas`, self-referenziata via `parent_id`), matching interno→categoria calcolato a display-time in `handleSearch` (nessuna colonna nuova su `group_numbers`, nessun tocco a `internal/pbx`). Nuova pagina admin CRUD `/admin/group-categories`, stesso pattern di `/admin/areas`.

**Tech Stack:** Go 1.25, `mattn/go-sqlite3` (raw SQL, no ORM), `html/template` (template Go nativo, nested `{{define}}` per la ricorsione dell'albero), `gorilla/mux`, HTMX (nessun JS nuovo).

**Spec:** `docs/superpowers/specs/2026-09-15-group-categories-hierarchy-design.md`

## Global Constraints

- Nessuna colonna nuova su `group_numbers`/`contacts`/`areas` — solo la nuova tabella `group_categories`.
- Il matching categoria è calcolato ad ogni richiesta di `/search`, mai persistito.
- Range più specifico (più stretto) vince quando un'estensione cade in più categorie annidate.
- Bucket "Altre chiamate" per i gruppi senza categoria, sempre in fondo alla colonna.
- Ordinamento per `range_start` crescente ad ogni livello, non alfabetico.
- **Prerequisito**: questo piano assume la PR #18 (colonna "Chiamate di gruppo" affiancata, `web/templates/search_results.html` con wrapper `.results-groups`/`.results-contacts`, `phonebook.GroupWithMembers.ActiveMembers()`) già mergiata in `main` — i riferimenti a righe/contenuto sotto assumono quello stato.

---

### Task 1: Schema `group_categories` + struct + letture

**Files:**
- Modify: `internal/database/sqlite.go` (schema in `migrate()`, nuovo struct, `ListGroupCategories`, `GetGroupCategory`)
- Test: `internal/database/sqlite_test.go`

**Interfaces:**
- Produces:
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
  func (db *DB) ListGroupCategories() ([]*GroupCategory, error) // ORDER BY range_start (NULL ultimo), poi name
  func (db *DB) GetGroupCategory(id int64) (*GroupCategory, error) // nil, nil se non trovato
  ```

- [ ] **Step 1: Aggiungi la tabella allo schema embedded**

In `internal/database/sqlite.go`, trova il blocco `schema := \`` che contiene `CREATE TABLE IF NOT EXISTS areas (...)` (vicino alla riga 162) e aggiungi subito dopo, prima del backtick di chiusura:

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

- [ ] **Step 2: Aggiungi il tipo `GroupCategory`**

Subito dopo la definizione di `type GroupNumber struct { ... }` in `internal/database/sqlite.go`, aggiungi:

```go
// GroupCategory raggruppa i gruppi di chiamata (group_numbers) in
// sezioni gerarchiche a display-time (nessuna colonna su group_numbers) —
// vedi docs/superpowers/specs/2026-09-15-group-categories-hierarchy-design.md.
// ParentID nil = categoria di primo livello (es. "Uffici"); non-nil =
// annidata sotto un'altra categoria (es. "Settore VI - Legale" dentro
// "Uffici"). RangeStart/RangeEnd nil = nessuna regola (categoria mai
// auto-assegnata, solo organizzativa se in futuro serve raggruppare a
// mano — oggi il matching è sempre su range).
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

- [ ] **Step 3: Implementa `ListGroupCategories` e `GetGroupCategory`**

Aggiungi in `internal/database/sqlite.go`, dopo `func MatchExtensionRange(...)`:

```go
// ListGroupCategories returns all group categories, ordinate per
// range_start crescente (NULL per ultimo) poi per nome — stesso criterio
// usato per l'ordinamento a display-time nell'albero pubblico.
func (db *DB) ListGroupCategories() ([]*GroupCategory, error) {
	rows, err := db.Query(`
	SELECT id, key, name, parent_id, range_start, range_end, created_at, updated_at
	FROM group_categories
	ORDER BY (range_start IS NULL), range_start, name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list group categories: %w", err)
	}
	defer rows.Close()

	var categories []*GroupCategory
	for rows.Next() {
		c := &GroupCategory{}
		if err := rows.Scan(&c.ID, &c.Key, &c.Name, &c.ParentID, &c.RangeStart, &c.RangeEnd, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan group category: %w", err)
		}
		categories = append(categories, c)
	}
	return categories, rows.Err()
}

// GetGroupCategory returns nil, nil se l'id non esiste.
func (db *DB) GetGroupCategory(id int64) (*GroupCategory, error) {
	c := &GroupCategory{}
	err := db.QueryRow(`
	SELECT id, key, name, parent_id, range_start, range_end, created_at, updated_at
	FROM group_categories WHERE id = ?
	`, id).Scan(&c.ID, &c.Key, &c.Name, &c.ParentID, &c.RangeStart, &c.RangeEnd, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group category: %w", err)
	}
	return c, nil
}
```

- [ ] **Step 4: Scrivi il test**

Aggiungi in `internal/database/sqlite_test.go`:

```go
func TestGroupCategoryListEmpty(t *testing.T) {
	db := newTestDB(t)
	categories, err := db.ListGroupCategories()
	if err != nil {
		t.Fatalf("ListGroupCategories failed: %v", err)
	}
	if len(categories) != 0 {
		t.Fatalf("got %d categories on fresh DB, want 0", len(categories))
	}
}

func TestGroupCategoryGetMissing(t *testing.T) {
	db := newTestDB(t)
	c, err := db.GetGroupCategory(999)
	if err != nil {
		t.Fatalf("GetGroupCategory failed: %v", err)
	}
	if c != nil {
		t.Fatal("GetGroupCategory should return nil for missing id")
	}
}
```

- [ ] **Step 5: Esegui i test (in container, niente compilatore C nativo su Windows)**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run TestGroupCategory -v"`
Expected: PASS su entrambi i test (compileranno solo se schema/struct/funzioni sono corretti — non c'è un "fail atteso" preliminare qui perché la tabella è nuova, non c'è comportamento da rompere prima).

- [ ] **Step 6: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): aggiungi tabella group_categories e letture base"
```

---

### Task 2: Create/Update/Delete `group_categories`

**Files:**
- Modify: `internal/database/sqlite.go`
- Test: `internal/database/sqlite_test.go`

**Interfaces:**
- Consumes: `GroupCategory` da Task 1
- Produces:
  ```go
  func (db *DB) CreateGroupCategory(c *GroupCategory) error
  func (db *DB) UpdateGroupCategory(c *GroupCategory) error // ritorna errore se il parent_id proposto crea un ciclo
  func (db *DB) DeleteGroupCategory(id int64) error // orfana i figli (parent_id -> NULL)
  ```

- [ ] **Step 1: Scrivi i test (falliscono: funzioni non esistono)**

Aggiungi in `internal/database/sqlite_test.go`:

```go
func TestGroupCategoryCRUD(t *testing.T) {
	db := newTestDB(t)

	parent := &GroupCategory{Key: "uffici", Name: "Uffici"}
	if err := db.CreateGroupCategory(parent); err != nil {
		t.Fatalf("CreateGroupCategory (parent) failed: %v", err)
	}
	if parent.ID == 0 {
		t.Fatal("CreateGroupCategory did not set ID")
	}

	start, end := 401, 404
	child := &GroupCategory{Key: "settore_vi", Name: "Settore VI - Legale", ParentID: &parent.ID, RangeStart: &start, RangeEnd: &end}
	if err := db.CreateGroupCategory(child); err != nil {
		t.Fatalf("CreateGroupCategory (child) failed: %v", err)
	}

	child.Name = "Settore VI - Legale (rinominato)"
	if err := db.UpdateGroupCategory(child); err != nil {
		t.Fatalf("UpdateGroupCategory failed: %v", err)
	}
	got, err := db.GetGroupCategory(child.ID)
	if err != nil {
		t.Fatalf("GetGroupCategory failed: %v", err)
	}
	if got.Name != "Settore VI - Legale (rinominato)" {
		t.Errorf("Name = %q, want rinominato", got.Name)
	}
	if got.ParentID == nil || *got.ParentID != parent.ID {
		t.Errorf("ParentID = %v, want %d", got.ParentID, parent.ID)
	}

	if err := db.DeleteGroupCategory(parent.ID); err != nil {
		t.Fatalf("DeleteGroupCategory (parent) failed: %v", err)
	}
	got, _ = db.GetGroupCategory(child.ID)
	if got == nil {
		t.Fatal("child should still exist after parent deletion")
	}
	if got.ParentID != nil {
		t.Errorf("child.ParentID = %v after parent deletion, want nil (orphaned)", got.ParentID)
	}
}

func TestGroupCategoryUpdateRejectsCycle(t *testing.T) {
	db := newTestDB(t)

	a := &GroupCategory{Key: "a", Name: "A"}
	if err := db.CreateGroupCategory(a); err != nil {
		t.Fatalf("CreateGroupCategory a failed: %v", err)
	}
	b := &GroupCategory{Key: "b", Name: "B", ParentID: &a.ID}
	if err := db.CreateGroupCategory(b); err != nil {
		t.Fatalf("CreateGroupCategory b failed: %v", err)
	}

	// a -> parent b creerebbe un ciclo (b è già figlio di a).
	a.ParentID = &b.ID
	if err := db.UpdateGroupCategory(a); err == nil {
		t.Fatal("UpdateGroupCategory should reject a cycle (a -> b -> a)")
	}

	// una categoria non può essere genitore di se stessa.
	selfRef := &GroupCategory{Key: "c", Name: "C"}
	if err := db.CreateGroupCategory(selfRef); err != nil {
		t.Fatalf("CreateGroupCategory c failed: %v", err)
	}
	selfRef.ParentID = &selfRef.ID
	if err := db.UpdateGroupCategory(selfRef); err == nil {
		t.Fatal("UpdateGroupCategory should reject self-parenting")
	}
}
```

- [ ] **Step 2: Esegui i test per vedere il fallimento atteso**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run TestGroupCategoryCRUD -v"`
Expected: FAIL con `undefined: (*DB).CreateGroupCategory` (o simile) — le funzioni non esistono ancora.

- [ ] **Step 3: Implementa Create/Update/Delete**

Aggiungi in `internal/database/sqlite.go`, dopo `GetGroupCategory`:

```go
// CreateGroupCategory inserts a new group category. Key must be unique.
func (db *DB) CreateGroupCategory(c *GroupCategory) error {
	now := time.Now()
	c.CreatedAt = now
	c.UpdatedAt = now

	result, err := db.Exec(`
	INSERT INTO group_categories (key, name, parent_id, range_start, range_end, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	`, c.Key, c.Name, c.ParentID, c.RangeStart, c.RangeEnd, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("failed to create group category: %w", err)
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

// groupCategoryCreatesCycle risale la catena parent_id partendo da
// candidateParent e ritorna true se incontra targetID — cioè se
// assegnare candidateParent come genitore di targetID creerebbe un
// ciclo (incluso il caso candidateParent == targetID, auto-genitore).
func (db *DB) groupCategoryCreatesCycle(targetID, candidateParent int64) (bool, error) {
	current := candidateParent
	for {
		if current == targetID {
			return true, nil
		}
		var parentID sql.NullInt64
		err := db.QueryRow(`SELECT parent_id FROM group_categories WHERE id = ?`, current).Scan(&parentID)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to walk group category parent chain: %w", err)
		}
		if !parentID.Valid {
			return false, nil
		}
		current = parentID.Int64
	}
}

// UpdateGroupCategory aggiorna nome, genitore e range insieme. Rifiuta un
// parent_id che creerebbe un ciclo (una categoria genitore di se stessa,
// direttamente o attraverso la catena).
func (db *DB) UpdateGroupCategory(c *GroupCategory) error {
	if c.ParentID != nil {
		cycle, err := db.groupCategoryCreatesCycle(c.ID, *c.ParentID)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("parent_id %d would create a cycle for group category %d", *c.ParentID, c.ID)
		}
	}
	_, err := db.Exec(`
	UPDATE group_categories SET name = ?, parent_id = ?, range_start = ?, range_end = ?, updated_at = ?
	WHERE id = ?
	`, c.Name, c.ParentID, c.RangeStart, c.RangeEnd, time.Now(), c.ID)
	if err != nil {
		return fmt.Errorf("failed to update group category: %w", err)
	}
	return nil
}

// DeleteGroupCategory removes a group category and orphans any child
// (parent_id -> NULL, i figli restano ma tornano di primo livello)
// invece di lasciare un riferimento pendente.
func (db *DB) DeleteGroupCategory(id int64) error {
	if _, err := db.Exec(`UPDATE group_categories SET parent_id = NULL WHERE parent_id = ?`, id); err != nil {
		return fmt.Errorf("failed to orphan child group categories: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM group_categories WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete group category: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run TestGroupCategory -v"`
Expected: PASS su tutti e 4 i test di Task 1+2.

- [ ] **Step 5: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): CRUD group_categories con validazione anti-ciclo"
```

---

### Task 3: `MatchGroupCategory` (range più specifico vince)

**Files:**
- Modify: `internal/database/sqlite.go`
- Test: `internal/database/sqlite_test.go`

**Interfaces:**
- Consumes: `GroupCategory` da Task 1
- Produces:
  ```go
  func MatchGroupCategory(ext string, categories []*GroupCategory) *GroupCategory
  ```

- [ ] **Step 1: Scrivi il test (fallisce: funzione non esiste)**

Aggiungi in `internal/database/sqlite_test.go`:

```go
func TestMatchGroupCategoryPrefersNarrowestRange(t *testing.T) {
	parentStart, parentEnd := 400, 499
	childStart, childEnd := 401, 404
	parent := &GroupCategory{ID: 1, Key: "uffici", Name: "Uffici", RangeStart: &parentStart, RangeEnd: &parentEnd}
	child := &GroupCategory{ID: 2, Key: "settore_vi", Name: "Settore VI - Legale", ParentID: &parent.ID, RangeStart: &childStart, RangeEnd: &childEnd}

	// Ordine deliberatamente "genitore prima" nello slice, per verificare
	// che vinca comunque il range più stretto e non il primo dell'elenco.
	categories := []*GroupCategory{parent, child}

	got := MatchGroupCategory("401", categories)
	if got == nil || got.ID != child.ID {
		t.Fatalf("MatchGroupCategory(401) = %v, want child (id=2, range più stretto)", got)
	}

	got = MatchGroupCategory("450", categories)
	if got == nil || got.ID != parent.ID {
		t.Fatalf("MatchGroupCategory(450) = %v, want parent (fuori dal range del figlio)", got)
	}

	got = MatchGroupCategory("999", categories)
	if got != nil {
		t.Fatalf("MatchGroupCategory(999) = %v, want nil (nessun range copre 999)", got)
	}

	got = MatchGroupCategory("not-a-number", categories)
	if got != nil {
		t.Fatalf("MatchGroupCategory(non numerico) = %v, want nil", got)
	}
}
```

- [ ] **Step 2: Esegui il test, verifica il fallimento**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run TestMatchGroupCategory -v"`
Expected: FAIL con `undefined: MatchGroupCategory`.

- [ ] **Step 3: Implementa `MatchGroupCategory`**

Aggiungi in `internal/database/sqlite.go`, dopo `MatchExtensionRange`:

```go
// MatchGroupCategory ritorna la group_category più specifica (range più
// stretto) il cui [RangeStart, RangeEnd] contiene ext — a differenza di
// MatchExtensionRange (che ritorna il primo match nell'ordine di lista e
// non gestisce gerarchie), qui più categorie annidate possono coprire lo
// stesso interno: vince quella col range più stretto, indipendentemente
// dall'ordine di iterazione. Nil se ext non è numerico o nessuna regola
// lo copre.
func MatchGroupCategory(ext string, categories []*GroupCategory) *GroupCategory {
	n, err := strconv.Atoi(ext)
	if err != nil {
		return nil
	}
	var best *GroupCategory
	bestWidth := -1
	for _, c := range categories {
		if c.RangeStart == nil || c.RangeEnd == nil {
			continue
		}
		if n < *c.RangeStart || n > *c.RangeEnd {
			continue
		}
		width := *c.RangeEnd - *c.RangeStart
		if best == nil || width < bestWidth {
			best = c
			bestWidth = width
		}
	}
	return best
}
```

- [ ] **Step 4: Esegui il test, verifica che passi**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/database/... -run TestMatchGroupCategory -v"`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/database/sqlite.go internal/database/sqlite_test.go
git commit -m "feat(db): MatchGroupCategory preferisce il range piu' specifico"
```

---

### Task 4: Costruzione dell'albero (`phonebook.BuildGroupCategoryTree`)

**Files:**
- Modify: `internal/phonebook/service.go`
- Test: `internal/phonebook/service_test.go`

**Interfaces:**
- Consumes: `GroupWithMembers` (esistente in `phonebook`), `database.GroupCategory`, `database.MatchGroupCategory` da Task 3
- Produces:
  ```go
  type GroupCategoryNode struct {
      Category *database.GroupCategory // mai nil per i nodi dell'albero
      Children []*GroupCategoryNode
      Groups   []*GroupWithMembers
  }
  // Ritorna l'albero (solo categorie che coprono almeno un gruppo, diretto
  // o tramite figli) e i gruppi senza categoria (bucket "Altre chiamate",
  // gestito a parte dal chiamante).
  func BuildGroupCategoryTree(groups []*GroupWithMembers, categories []*database.GroupCategory) (tree []*GroupCategoryNode, uncategorized []*GroupWithMembers)
  ```

- [ ] **Step 1: Scrivi il test (fallisce: funzione non esiste)**

Aggiungi in `internal/phonebook/service_test.go`:

```go
func TestBuildGroupCategoryTree(t *testing.T) {
	ufficiStart, ufficiEnd := 400, 499
	settoreStart, settoreEnd := 401, 404
	dirigentiStart, dirigentiEnd := 300, 350

	uffici := &database.GroupCategory{ID: 1, Key: "uffici", Name: "Uffici", RangeStart: &ufficiStart, RangeEnd: &ufficiEnd}
	settoreVI := &database.GroupCategory{ID: 2, Key: "settore_vi", Name: "Settore VI - Legale", ParentID: &uffici.ID, RangeStart: &settoreStart, RangeEnd: &settoreEnd}
	dirigenti := &database.GroupCategory{ID: 3, Key: "dirigenti", Name: "Dirigenti", RangeStart: &dirigentiStart, RangeEnd: &dirigentiEnd}
	// Categoria senza alcun gruppo assegnato: non deve comparire nell'albero.
	vuota := &database.GroupCategory{ID: 4, Key: "vuota", Name: "Vuota"}

	categories := []*database.GroupCategory{uffici, settoreVI, dirigenti, vuota}

	groups := []*GroupWithMembers{
		{Group: &database.GroupNumber{Number: "401", Name: "Servizio difesa legale"}},
		{Group: &database.GroupNumber{Number: "405", Name: "Segretario Generale"}}, // in "Uffici" ma non in "Settore VI"
		{Group: &database.GroupNumber{Number: "310", Name: "Dirigente Settore III"}},
		{Group: &database.GroupNumber{Number: "900", Name: "COC"}}, // nessuna categoria
	}

	tree, uncategorized := BuildGroupCategoryTree(groups, categories)

	if len(tree) != 2 {
		t.Fatalf("got %d top-level nodes, want 2 (Dirigenti, Uffici)", len(tree))
	}
	// range_start crescente: Dirigenti (300) prima di Uffici (400).
	if tree[0].Category.Key != "dirigenti" || tree[1].Category.Key != "uffici" {
		t.Fatalf("top-level order = [%s, %s], want [dirigenti, uffici]", tree[0].Category.Key, tree[1].Category.Key)
	}

	dirigentiNode := tree[0]
	if len(dirigentiNode.Children) != 0 {
		t.Errorf("dirigenti should have no children, got %d", len(dirigentiNode.Children))
	}
	if len(dirigentiNode.Groups) != 1 || dirigentiNode.Groups[0].Group.Number != "310" {
		t.Errorf("dirigenti.Groups = %v, want [310]", dirigentiNode.Groups)
	}

	ufficiNode := tree[1]
	if len(ufficiNode.Groups) != 1 || ufficiNode.Groups[0].Group.Number != "405" {
		t.Errorf("uffici.Groups = %v, want [405] (401 va nel figlio settore_vi)", ufficiNode.Groups)
	}
	if len(ufficiNode.Children) != 1 || ufficiNode.Children[0].Category.Key != "settore_vi" {
		t.Fatalf("uffici.Children = %v, want [settore_vi]", ufficiNode.Children)
	}
	if len(ufficiNode.Children[0].Groups) != 1 || ufficiNode.Children[0].Groups[0].Group.Number != "401" {
		t.Errorf("settore_vi.Groups = %v, want [401]", ufficiNode.Children[0].Groups)
	}

	if len(uncategorized) != 1 || uncategorized[0].Group.Number != "900" {
		t.Fatalf("uncategorized = %v, want [900]", uncategorized)
	}
}

func TestBuildGroupCategoryTreeEmptyInput(t *testing.T) {
	tree, uncategorized := BuildGroupCategoryTree(nil, nil)
	if len(tree) != 0 || len(uncategorized) != 0 {
		t.Errorf("got tree=%v uncategorized=%v for nil input, want both empty", tree, uncategorized)
	}
}
```

- [ ] **Step 2: Esegui il test, verifica il fallimento**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/phonebook/... -run TestBuildGroupCategoryTree -v"`
Expected: FAIL con `undefined: BuildGroupCategoryTree`.

- [ ] **Step 3: Implementa `BuildGroupCategoryTree`**

Aggiungi in `internal/phonebook/service.go`, dopo `GroupByDepartment`:

```go
// GroupCategoryNode è un nodo dell'albero di categorie per la colonna
// "Chiamate di gruppo" (vedi
// docs/superpowers/specs/2026-09-15-group-categories-hierarchy-design.md).
// Category non è mai nil per i nodi dell'albero — i gruppi senza
// categoria sono il secondo valore di ritorno di BuildGroupCategoryTree,
// non un nodo con Category nil.
type GroupCategoryNode struct {
	Category *database.GroupCategory
	Children []*GroupCategoryNode
	Groups   []*GroupWithMembers
}

// BuildGroupCategoryTree raggruppa groups sotto la categoria più
// specifica che copre il loro numero (database.MatchGroupCategory),
// costruendo l'albero a partire dalla catena ParentID di ogni categoria
// coinvolta. Una categoria senza alcun gruppo (diretto o nei discendenti)
// non compare nell'albero — niente sezioni vuote. Figli e gruppi sono
// ordinati per RangeStart/Number crescente ad ogni livello. I gruppi il
// cui numero non cade in nessun range sono ritornati separatamente
// (uncategorized) — il chiamante li mostra nel bucket "Altre chiamate".
func BuildGroupCategoryTree(groups []*GroupWithMembers, categories []*database.GroupCategory) (tree []*GroupCategoryNode, uncategorized []*GroupWithMembers) {
	nodes := make(map[int64]*GroupCategoryNode, len(categories))
	for _, c := range categories {
		nodes[c.ID] = &GroupCategoryNode{Category: c}
	}

	used := make(map[int64]bool, len(categories))
	for _, g := range groups {
		leaf := database.MatchGroupCategory(g.Group.Number, categories)
		if leaf == nil {
			uncategorized = append(uncategorized, g)
			continue
		}
		nodes[leaf.ID].Groups = append(nodes[leaf.ID].Groups, g)
		used[leaf.ID] = true
	}

	// Marca come "usata" ogni categoria che ha un discendente usato,
	// risalendo la catena — altrimenti un genitore con figli popolati ma
	// senza gruppi propri (es. "Uffici" con solo "Settore VI" popolato)
	// verrebbe scartato insieme al figlio.
	for id := range used {
		c := nodes[id].Category
		for c.ParentID != nil {
			used[*c.ParentID] = true
			c = nodes[*c.ParentID].Category
		}
	}

	var roots []*GroupCategoryNode
	for _, c := range categories {
		if !used[c.ID] {
			continue
		}
		node := nodes[c.ID]
		if c.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := nodes[*c.ParentID]
		if !ok {
			roots = append(roots, node) // genitore inesistente/orfano: tratta come radice
			continue
		}
		parent.Children = append(parent.Children, node)
	}

	sortNodes(roots)
	return roots, uncategorized
}

// sortNodes ordina un livello di nodi per RangeStart crescente (nil per
// ultimo) e ricorre sui figli — stesso criterio di ListGroupCategories.
func sortNodes(nodes []*GroupCategoryNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].Category.RangeStart, nodes[j].Category.RangeStart
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a < *b
	})
	for _, n := range nodes {
		sortNodes(n.Children)
		sort.SliceStable(n.Groups, func(i, j int) bool {
			return n.Groups[i].Group.Number < n.Groups[j].Group.Number
		})
	}
}
```

- [ ] **Step 4: Esegui i test, verifica che passino**

Run: `MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/app" -w //app golang:1.25-alpine sh -c "apk add --no-cache gcc musl-dev sqlite-dev >/dev/null 2>&1 && CGO_ENABLED=1 go test ./internal/phonebook/... -v"`
Expected: PASS su tutti i test del package, inclusi i due nuovi.

- [ ] **Step 5: Commit**

```bash
git add internal/phonebook/service.go internal/phonebook/service_test.go
git commit -m "feat(phonebook): costruzione albero categorie per le chiamate di gruppo"
```

---

### Task 5: Admin UI — `/admin/group-categories`

**Files:**
- Modify: `cmd/server/main.go` (routes + handlers + `railData`/nav non serve, pagina autonoma come `/admin/pbx`)
- Create: `web/templates/admin_group_categories.html` (frammento tabella+form, pattern di `admin_areas.html`)
- Create: `web/templates/admin_page_group_categories.html` (shell pagina, pattern di `admin_page_areas.html`)
- Modify: `web/templates/rail.html` (voce di navigazione)

**Interfaces:**
- Consumes: `database.GroupCategory`, `ListGroupCategories`/`CreateGroupCategory`/`UpdateGroupCategory`/`DeleteGroupCategory` da Task 1-2

- [ ] **Step 1: Aggiungi le route**

In `cmd/server/main.go`, trova il blocco (vicino alla riga 244-247):

```go
	admin.HandleFunc("/areas", handleAdminAreas).Methods("GET")
	admin.HandleFunc("/areas", handleAdminCreateArea).Methods("POST")
	admin.HandleFunc("/areas/{id}", handleAdminRenameArea).Methods("POST")
	admin.HandleFunc("/areas/{id}/delete", handleAdminDeleteArea).Methods("POST")
```

e aggiungi subito dopo:

```go
	admin.HandleFunc("/group-categories", handleAdminGroupCategories).Methods("GET")
	admin.HandleFunc("/group-categories", handleAdminCreateGroupCategory).Methods("POST")
	admin.HandleFunc("/group-categories/{id}", handleAdminUpdateGroupCategory).Methods("POST")
	admin.HandleFunc("/group-categories/{id}/delete", handleAdminDeleteGroupCategory).Methods("POST")
```

- [ ] **Step 2: Aggiungi gli handler**

In `cmd/server/main.go`, dopo `handleAdminDeleteArea` (vicino alla riga 1193), aggiungi:

```go
// renderAdminGroupCategories re-renders solo il frammento (usato dopo
// crea/modifica/elimina via HTMX) — stesso pattern di renderAdminAreas.
func renderAdminGroupCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := db.ListGroupCategories()
	if err != nil {
		http.Error(w, "Failed to list group categories", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := map[string]interface{}{
		"Categories": categories,
		"Messages":   i18n.GetMessages(locale),
	}
	templates.ExecuteTemplate(w, "admin_group_categories.html", data)
}

// handleAdminGroupCategories serve la pagina completa (navigazione
// diretta) — le scritture continuano a ricevere solo il frammento via
// renderAdminGroupCategories.
func handleAdminGroupCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := db.ListGroupCategories()
	if err != nil {
		http.Error(w, "Failed to list group categories", http.StatusInternalServerError)
		return
	}

	locale := i18n.ResolveLocale(r)
	data := railData()
	data["Categories"] = categories
	data["Username"] = sessionAdminUsername(r)
	data["Section"] = "admin-group-categories"
	data["Messages"] = i18n.GetMessages(locale)

	templates.ExecuteTemplate(w, "admin_page_group_categories.html", data)
}

// parseOptionalGroupCategoryInt legge un campo form numerico opzionale
// (range_start/range_end/parent_id) — stringa vuota o non numerica torna
// nil, stesso comportamento di SetAreaRange per i range delle Aree.
func parseOptionalGroupCategoryInt(r *http.Request, field string) *int {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func handleAdminCreateGroupCategory(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminGroupCategories(w, r)
		return
	}
	key := slugify(name)
	if key == "" {
		renderAdminGroupCategories(w, r)
		return
	}

	c := &database.GroupCategory{
		Key:        key,
		Name:       name,
		RangeStart: parseOptionalGroupCategoryInt(r, "range_start"),
		RangeEnd:   parseOptionalGroupCategoryInt(r, "range_end"),
	}
	if pid := parseOptionalGroupCategoryInt(r, "parent_id"); pid != nil {
		id64 := int64(*pid)
		c.ParentID = &id64
	}
	if err := db.CreateGroupCategory(c); err != nil {
		log.Printf("[ADMIN] Failed to create group category %q: %v", name, err)
	}
	renderAdminGroupCategories(w, r)
}

func handleAdminUpdateGroupCategory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderAdminGroupCategories(w, r)
		return
	}

	c := &database.GroupCategory{
		ID:         id,
		Name:       name,
		RangeStart: parseOptionalGroupCategoryInt(r, "range_start"),
		RangeEnd:   parseOptionalGroupCategoryInt(r, "range_end"),
	}
	if pid := parseOptionalGroupCategoryInt(r, "parent_id"); pid != nil {
		id64 := int64(*pid)
		c.ParentID = &id64
	}
	if err := db.UpdateGroupCategory(c); err != nil {
		log.Printf("[ADMIN] Failed to update group category %d: %v", id, err)
	}
	renderAdminGroupCategories(w, r)
}

func handleAdminDeleteGroupCategory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)

	if err := db.DeleteGroupCategory(id); err != nil {
		log.Printf("[ADMIN] Failed to delete group category %d: %v", id, err)
	}
	renderAdminGroupCategories(w, r)
}
```

- [ ] **Step 3: Crea il frammento `admin_group_categories.html`**

Crea `web/templates/admin_group_categories.html`:

```html
<div class="section-head">
    <h3>Categorie chiamate di gruppo</h3>
</div>
<p class="helptext" style="margin:8px 0 12px;">Raggruppano la colonna "Chiamate di gruppo" in rubrica per interno (es. "Uffici" 400&ndash;499 contiene "Settore VI - Legale" 401&ndash;404). Una categoria genitore mostra le proprie chiamate insieme a quelle delle categorie figlie. Eliminare una categoria con figli li riporta a primo livello, non li elimina.</p>

<table>
    <thead><tr><th>Nome</th><th>Chiave</th><th>Genitore</th><th>Range interni</th><th></th></tr></thead>
    <tbody>
        {{range .Categories}}
        <tr>
            <td>
                <form id="gc-form-{{.ID}}" hx-post="/admin/group-categories/{{.ID}}" hx-target="#group-categories-content" hx-swap="innerHTML" style="margin:0;">
                    <input type="text" name="name" value="{{.Name}}" class="input" style="width:100%;">
                </form>
            </td>
            <td class="num">{{.Key}}</td>
            <td>
                <select name="parent_id" form="gc-form-{{.ID}}" class="input">
                    <option value="">Nessuna</option>
                    {{$current := .ID}}
                    {{range $.Categories}}
                    {{if ne .ID $current}}
                    <option value="{{.ID}}" {{if and $current.ParentID (eq $current.ParentID .ID)}}selected{{end}}>{{.Name}}</option>
                    {{end}}
                    {{end}}
                </select>
            </td>
            <td>
                <div class="field-row" style="margin:0;">
                    <input type="text" name="range_start" form="gc-form-{{.ID}}" value="{{if .RangeStart}}{{.RangeStart}}{{end}}" placeholder="da" class="input" style="width:70px;">
                    <span class="helptext">&ndash;</span>
                    <input type="text" name="range_end" form="gc-form-{{.ID}}" value="{{if .RangeEnd}}{{.RangeEnd}}{{end}}" placeholder="a" class="input" style="width:70px;">
                </div>
            </td>
            <td>
                <div class="row-actions">
                    <button type="submit" form="gc-form-{{.ID}}" class="btn btn-ghost">{{index $.Messages "save"}}</button>
                    <button hx-post="/admin/group-categories/{{.ID}}/delete" hx-confirm="Eliminare la categoria {{.Name}}?" hx-target="#group-categories-content" hx-swap="innerHTML" class="btn-danger">
                        {{index $.Messages "delete"}}
                    </button>
                </div>
            </td>
        </tr>
        {{end}}
    </tbody>
</table>

<form hx-post="/admin/group-categories" hx-target="#group-categories-content" hx-swap="innerHTML" class="field-row" style="margin-top:14px;">
    <input type="text" name="name" placeholder="Nuova categoria (es. Settore VI - Legale)" class="input" required>
    <select name="parent_id" class="input">
        <option value="">Nessuna (primo livello)</option>
        {{range .Categories}}
        <option value="{{.ID}}">{{.Name}}</option>
        {{end}}
    </select>
    <input type="text" name="range_start" placeholder="da" class="input" style="width:70px;">
    <input type="text" name="range_end" placeholder="a" class="input" style="width:70px;">
    <button type="submit" class="btn btn-primary" style="flex:none;">Aggiungi categoria</button>
</form>
```

*Nota sul template*: il confronto `eq $current.ParentID .ID` in Go template funziona anche quando `$current.ParentID` è nil (`eq` con un puntatore nil e un `int64` non nil ritorna semplicemente false, senza panic — `html/template`/`text/template` gestiscono il confronto per tipo dinamico). Verificalo comunque al passo di test manuale (Step 6).

- [ ] **Step 4: Crea la shell `admin_page_group_categories.html`**

Crea `web/templates/admin_page_group_categories.html`:

```html
<!DOCTYPE html>
<html lang="{{.Locale}}">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Categorie chiamate - {{index .Messages "app_title"}}</title>
    <script src="https://unpkg.com/htmx.org@2.0.0"></script>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="shell">
        {{template "rail.html" .}}
        <main class="main" style="max-width:920px;">
            <h1 class="page-title">Categorie chiamate di gruppo</h1>
            <div id="group-categories-content">{{template "admin_group_categories.html" .}}</div>
        </main>
    </div>
</body>
</html>
```

- [ ] **Step 5: Aggiungi la voce di navigazione in `rail.html`**

In `web/templates/rail.html`, trova il link `/admin/pbx` (vicino alla riga 55-58):

```html
    <a href="/admin/pbx" class="rail-item{{if eq .Section "admin-pbx"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M22 16.92v3a2 2 0 01-2.18 2 19.79 19.79 0 01-8.63-3.07 19.5 19.5 0 01-6-6 19.79 19.79 0 01-3.07-8.67A2 2 0 014.11 2h3a2 2 0 012 1.72c.127.96.362 1.903.7 2.81a2 2 0 01-.45 2.11L8.09 9.91a16 16 0 006 6l1.27-1.27a2 2 0 012.11-.45c.907.338 1.85.573 2.81.7A2 2 0 0122 16.92z"/></svg>
        <span>Centralino</span>
    </a>
```

e aggiungi subito dopo:

```html
    <a href="/admin/group-categories" class="rail-item{{if eq .Section "admin-group-categories"}} active{{end}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 21V10l8-6 8 6v11"/><path d="M9 21v-6h6v6"/><path d="M4 10h16"/></svg>
        <span>Categorie chiamate</span>
    </a>
```

- [ ] **Step 6: Verifica manuale**

Run: `cd /c/Users/mirko.daddiego/Documents/rubrica && docker compose up -d --build && sleep 3 && docker logs rubrica --tail 15`
Expected: nessun panic `unexpected {{end}}`/template parse error nei log; server risponde su `/health`.

Poi, via browser (login admin richiesto): naviga `/admin/group-categories`, crea una categoria "Uffici" con range 400-499, crea una categoria "Settore VI - Legale" con genitore "Uffici" e range 401-404, salva, ricarica la pagina e verifica che il genitore selezionato resti "Uffici" (il `select` preselezionato). Elimina "Uffici" e verifica che "Settore VI - Legale" resti in lista con genitore "Nessuna".

- [ ] **Step 7: Commit**

```bash
git add cmd/server/main.go web/templates/admin_group_categories.html web/templates/admin_page_group_categories.html web/templates/rail.html
git commit -m "feat(admin): pagina CRUD /admin/group-categories"
```

---

### Task 6: Vista pubblica — colonna "Chiamate di gruppo" ad albero

**Files:**
- Modify: `cmd/server/main.go` (`handleSearch`)
- Modify: `web/templates/search_results.html`
- Create: `web/templates/group_category_node.html` (template ricorsivo)
- Modify: `web/static/css/style.css` (indentazione/chevron per i nodi categoria)

**Interfaces:**
- Consumes: `phonebook.BuildGroupCategoryTree`, `phonebook.GroupCategoryNode` da Task 4; `db.ListGroupCategories` da Task 1

- [ ] **Step 1: Estrai la riga-gruppo in un template nominato riusabile**

In `web/templates/search_results.html`, il blocco righe-gruppo (dentro `{{range .CallGroups}}...{{end}}`, dalla riga `<details class="group-row-details">` alla sua chiusura `</details>`) va estratto in un file a parte così sia il livello "categoria" sia il bucket "Altre chiamate" possono riusarlo. Crea `web/templates/group_row.html`:

```html
{{define "group_row.html"}}
<details class="group-row-details">
    <summary>
        <div class="row">
            <div class="badge group-badge">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="15" height="15"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 00-3-3.87M16 3.13a4 4 0 010 7.75"/></svg>
            </div>
            <div class="who">
                <div class="name">
                    <svg class="chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 18l6-6-6-6"/></svg>
                    {{.Group.Name}}
                </div>
                {{if .Group.Description}}<div class="role">{{.Group.Description}}</div>{{end}}
            </div>
            <div class="phone">
                <div class="phone-line">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 5c0-1 1-1.5 1.8-1.2l2.4 1c.6.2 1 .8 1 1.4v2c0 .5-.2 1-.6 1.3l-1 .9c1 2.3 2.8 4.1 5.1 5.1l.9-1c.3-.4.8-.6 1.3-.6h2c.6 0 1.2.4 1.4 1l1 2.4c.3.8-.2 1.8-1.2 1.8-8 0-14-6-14-14z"/></svg>
                    {{.Group.Number}}
                </div>
                <div class="phone-line muted">{{len .ActiveMembers}} membri</div>
            </div>
        </div>
    </summary>
    <div class="group-members">
        {{if .ActiveMembers}}
        {{range .ActiveMembers}}
        <div class="group-member-line">
            <span>{{.DisplayName}}</span>
            <span>{{if .LDAPExt}}int. {{extList .LDAPExt}}{{else}}{{.PrimaryNumber}}{{end}}</span>
        </div>
        {{end}}
        {{else}}
        <div class="group-member-line"><span class="muted">Nessun membro</span></div>
        {{end}}
    </div>
</details>
{{end}}
```

- [ ] **Step 2: Crea il template ricorsivo del nodo categoria**

Crea `web/templates/group_category_node.html`:

```html
{{define "group_category_node.html"}}
<details class="group-category-details" open>
    <summary>
        <svg class="chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 18l6-6-6-6"/></svg>
        <span class="group-category-name">{{.Category.Name}}</span>
    </summary>
    <div class="group-category-children">
        {{range .Children}}{{template "group_category_node.html" .}}{{end}}
        {{range .Groups}}{{template "group_row.html" .}}{{end}}
    </div>
</details>
{{end}}
```

- [ ] **Step 3: Sostituisci il rendering piatto in `search_results.html`**

In `web/templates/search_results.html`, sostituisci l'intero blocco da `{{if .CallGroups}}` fino al suo `{{end}}` di chiusura (il blocco che contiene `<div class="results-groups">...{{range .CallGroups}}...{{end}}</div>`) con:

```html
{{if .CallGroups}}
<div class="results-groups">
<details class="dept" open>
    <summary class="dept-head">
        <span class="dept-name">Chiamate di gruppo</span>
        <span class="dept-count">{{len .CallGroups}}</span>
    </summary>
    {{range .CallGroupTree}}{{template "group_category_node.html" .}}{{end}}
    {{if .UncategorizedGroups}}
    <details class="group-category-details" open>
        <summary>
            <svg class="chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 18l6-6-6-6"/></svg>
            <span class="group-category-name">Altre chiamate</span>
        </summary>
        <div class="group-category-children">
            {{range .UncategorizedGroups}}{{template "group_row.html" .}}{{end}}
        </div>
    </details>
    {{end}}
</details>
</div>
{{end}}
```

Il resto del file (blocco `.Groups`/contatti, `results-contacts`, empty-state) resta invariato.

- [ ] **Step 4: Wiring in `handleSearch`**

In `cmd/server/main.go`, trova il blocco che costruisce `callGroups` (quello con il commento "Le chiamate di gruppo (group_numbers) compaiono..."), e subito dopo la sua chiusura (dopo il blocco `if groupFilter == "" || groupFilter == "uffici" { ... }`), prima della costruzione di `data := map[string]interface{}{...}`, aggiungi:

```go
	var callGroupTree []*phonebook.GroupCategoryNode
	var uncategorizedGroups []*phonebook.GroupWithMembers
	if len(callGroups) > 0 {
		categories, err := db.ListGroupCategories()
		if err != nil {
			log.Printf("[SEARCH] Failed to list group categories: %v", err)
			uncategorizedGroups = callGroups
		} else {
			callGroupTree, uncategorizedGroups = phonebook.BuildGroupCategoryTree(callGroups, categories)
		}
	}
```

Poi, nel `data := map[string]interface{}{...}` esistente, aggiungi le due chiavi accanto a `"CallGroups": callGroups`:

```go
		"CallGroupTree":       callGroupTree,
		"UncategorizedGroups": uncategorizedGroups,
```

- [ ] **Step 5: CSS per i nodi categoria**

In `web/static/css/style.css`, dopo la regola `.group-row-details { ... }` e le regole correlate (`.group-members`, `.group-member-line`, `.group-badge`, `.chevron`, `.group-row-details[open] .chevron`), aggiungi:

```css
/* --- Nodo categoria nella colonna "Chiamate di gruppo" (albero a 2
   livelli, vedi group_category_node.html) --- */
.group-category-details { border-bottom: 1px solid var(--line); }
.group-category-details > summary {
    list-style: none; cursor: pointer; display: flex; align-items: center; gap: 8px;
    padding: 9px 4px; font-family: 'Archivo', sans-serif; font-weight: 600; font-size: 13.5px;
}
.group-category-details > summary::-webkit-details-marker { display: none; }
.group-category-details[open] > summary .chevron { transform: rotate(90deg); }
.group-category-children { padding-left: 18px; border-left: 1px solid var(--line); margin-left: 8px; }
.group-category-children .group-row-details { border-bottom: none; }
```

- [ ] **Step 6: Verifica manuale**

Run: `cd /c/Users/mirko.daddiego/Documents/rubrica && docker compose up -d --build && sleep 3 && docker logs rubrica --tail 15`
Expected: nessun panic di parsing template nei log.

Poi, dall'admin già configurato al Task 5 ("Uffici" 400-499, "Settore VI - Legale" 401-404 dentro "Uffici"): apri `/` (rubrica pubblica), verifica nella colonna "Chiamate di gruppo" che compaia la sezione "Uffici" contenente annidata "Settore VI - Legale" con dentro il gruppo 401 (se esiste tra i call group sincronizzati dal centralino), e che tutti gli altri gruppi non coperti da nessun range finiscano sotto "Altre chiamate" in fondo. Controlla anche con `mcp__plugin_chrome-devtools-mcp_chrome-devtools__take_screenshot` per un confronto visivo diretto.

- [ ] **Step 7: Commit**

```bash
git add cmd/server/main.go web/templates/search_results.html web/templates/group_category_node.html web/templates/group_row.html web/static/css/style.css
git commit -m "feat(ui): colonna chiamate di gruppo ad albero per categoria"
```

---

## Self-Review

- **Copertura spec**: modello dati (Task 1-2), matching range più specifico (Task 3), costruzione albero/ordinamento (Task 4), admin dedicato (Task 5), vista pubblica ad albero + bucket "Altre chiamate" (Task 6) — tutte le sezioni della spec hanno un task corrispondente. Il punto aperto della spec ("nodo speciale Category==nil per Altre chiamate") è stato risolto nel piano con un secondo valore di ritorno (`uncategorized`) invece di un nodo sentinella — più semplice da testare e da renderizzare, coerente con "nessuna implementazione ancora, solo design" della spec.
- **Placeholder**: nessun TBD/TODO; ogni step ha codice completo o comando eseguibile.
- **Coerenza tipi**: `GroupCategory`, `GroupCategoryNode`, `MatchGroupCategory`, `BuildGroupCategoryTree` usano nomi/firme identici in tutti i task che li consumano (verificato: Task 4 importa `database.GroupCategory`/`database.MatchGroupCategory` da Task 1/3; Task 6 importa `phonebook.GroupCategoryNode`/`BuildGroupCategoryTree` da Task 4).
