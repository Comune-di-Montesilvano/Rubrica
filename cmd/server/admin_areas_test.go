package main

import (
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/config"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
	"github.com/gorilla/mux"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

func TestParseRuleForm(t *testing.T) {
	form := url.Values{"kind": {"attr"}, "attr": {"Description"}, "value": {" Dirigente "}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got, err := parseRuleForm(r)
	if err != nil || got != (arearules.Rule{Kind: "attr", Attr: "description", Value: "Dirigente"}) {
		t.Errorf("parseRuleForm = %+v, %v", got, err)
	}
	bad := url.Values{"kind": {"attr"}, "attr": {"1x"}, "value": {"v"}}
	r = httptest.NewRequest("POST", "/", strings.NewReader(bad.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := parseRuleForm(r); !errors.Is(err, arearules.ErrInvalidRule) {
		t.Errorf("err = %v, want ErrInvalidRule", err)
	}
}

func TestFilterBySubArea(t *testing.T) {
	results := []*phonebook.ContactWithGroups{
		{Contact: &database.Contact{UID: "a"}, SubAreas: []phonebook.SubAreaTag{{Key: "dirigenti"}, {Key: "pc"}}},
		{Contact: &database.Contact{UID: "b"}},
	}
	if got := filterBySubArea(results, "pc"); len(got) != 1 || got[0].Contact.UID != "a" {
		t.Errorf("pc = %+v", got)
	}
	if got := filterBySubArea(results, "inesistente"); len(got) != 0 {
		t.Errorf("sotto-area inesistente = %d risultati, want 0", len(got))
	}
	if got := filterBySubArea(results, ""); len(got) != 2 {
		t.Errorf("senza filtro = %d", len(got))
	}
}

func TestManualSubAreasKeepsOnlyChosenArea(t *testing.T) {
	nodes := []*database.AreaNode{
		{Area: &database.Area{Key: "interni"}, Children: []*database.Area{{Key: "dirigenti", ParentKey: "interni"}}},
		{Area: &database.Area{Key: "esterni"}, Children: []*database.Area{{Key: "fornitori", ParentKey: "esterni"}}},
	}
	form := url.Values{"sub_areas": {"dirigenti", "fornitori", "fantasma"}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := manualSubAreas(r, "interni", nodes)
	if len(got) != 1 || got[0] != "dirigenti" {
		t.Errorf("manualSubAreas = %v, want [dirigenti]", got)
	}
}

func setupHandlerEnv(t *testing.T) {
	t.Helper()
	var err error
	db, err = database.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg = &config.Config{}
	funcs := template.FuncMap{
		"extList": func(s string) string { return s }, "derefInt64ID": func(p *int64) int64 { return 0 },
		"substr": func(s string, a, b int) string { return s }, "initials": func(string) string { return "XX" },
		"sentenceCase": func(s string) string { return s },
	}
	templates = template.Must(template.New("").Funcs(funcs).ParseGlob("../../web/templates/*.html"))
}

func postForm(h http.HandlerFunc, vars map[string]string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = mux.SetURLVars(r, vars)
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// Review: htmx 2 non fa lo swap delle risposte 4xx, quindi gli errori di
// validazione vanno restituiti con 200 e il frammento che li mostra.
func TestValidationErrorsAreSwappable(t *testing.T) {
	setupHandlerEnv(t)
	db.CreateArea(&database.Area{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"})
	interni, _ := db.GetAreaByKey("interni")

	rec := postForm(handleAdminAddAreaRule, map[string]string{"id": strconv.FormatInt(interni.ID, 10)},
		url.Values{"kind": {"range"}, "value": {"799-700"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "inizio maggiore della fine") {
		t.Errorf("regola non valida: code %d, body senza errore", rec.Code)
	}

	rec = postForm(handleAdminCreateArea, nil, url.Values{"name": {"Sotto sotto"}, "parent_key": {"dirigenti"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "non può avere sotto-aree") {
		t.Errorf("sotto-area annidata: code %d, body senza errore", rec.Code)
	}

	rec = postForm(handleAdminAddScopeRule, nil, url.Values{"kind": {"range"}, "value": {"1-2"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "perimetro vale solo") {
		t.Errorf("perimetro con range: code %d, body senza errore", rec.Code)
	}
}

// Review: niente due sync in parallelo, nessun rerun perso, force non perso.
func TestStartLDAPSyncSerializesAndKeepsForce(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	running, maxRunning := 0, 0
	var forces []bool
	orig := ldapSyncFn
	ldapSyncFn = func(o ldap.SyncOptions, _ func(int, int)) (ldap.SyncReport, error) {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		forces = append(forces, o.Force)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return ldap.SyncReport{}, nil
	}
	defer func() { ldapSyncFn = orig }()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); startLDAPSync(false) }()
	}
	wg.Wait()
	startLDAPSync(true)
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for ldapManualSync.snapshot().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxRunning != 1 {
		t.Errorf("sync concorrenti = %d, want 1", maxRunning)
	}
	if len(forces) != 2 || !forces[1] {
		t.Errorf("forces = %v, want [false true] (un solo rerun, con force)", forces)
	}
}

// Review: anche worker orario e sync iniziale passano dalla stessa guardia.
func TestRunLDAPSyncIsSerialized(t *testing.T) {
	var mu sync.Mutex
	running, maxRunning := 0, 0
	orig := ldapSyncFn
	ldapSyncFn = func(ldap.SyncOptions, func(int, int)) (ldap.SyncReport, error) {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return ldap.SyncReport{}, nil
	}
	defer func() { ldapSyncFn = orig }()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); runLDAPSync(ldap.SyncOptions{}, nil) }()
	}
	wg.Wait()
	if maxRunning != 1 {
		t.Errorf("runLDAPSync concorrenti = %d, want 1", maxRunning)
	}
}
