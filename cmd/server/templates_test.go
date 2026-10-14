package main

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/ldap"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
)

// Il form della rubrica telefoni ha un campo testo seguito da un campo
// password: il browser lo scambia per un login e può autocompilare la
// password dell'admin, che salvata diventerebbe la password di bind di tutti
// i telefoni. I campi devono dichiarare autocomplete esplicito.
func TestPhoneDirectoryFormDisablesAutofill(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_phone_directory.html"))
	var buf bytes.Buffer
	data := map[string]interface{}{
		"Messages": map[string]string{"save": "Salva"},
		"Enabled":  true,
		"BaseDN":   "dc=rubrica,dc=local",
		"BindDN":   "cn=telefoni,dc=rubrica,dc=local",
	}
	if err := tpl.ExecuteTemplate(&buf, "admin_phone_directory.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	checks := map[string]*regexp.Regexp{
		"bind_password": regexp.MustCompile(`<input[^>]*name="bind_password"[^>]*autocomplete="new-password"`),
		"bind_dn":       regexp.MustCompile(`<input[^>]*name="bind_dn"[^>]*autocomplete="off"`),
	}
	for field, re := range checks {
		if !re.MatchString(html) {
			t.Errorf("campo %s senza autocomplete esplicito", field)
		}
	}
}

func TestPhoneDirectoryShowsListenError(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_phone_directory.html"))
	var buf bytes.Buffer
	data := map[string]interface{}{
		"Messages":    map[string]string{"save": "Salva"},
		"Enabled":     true,
		"ListenError": "listen tcp 0.0.0.0:10389: bind: address already in use",
	}
	if err := tpl.ExecuteTemplate(&buf, "admin_phone_directory.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("address already in use")) {
		t.Error("errore del listener non mostrato nella pagina")
	}
}

func TestAdminAreasTreeRendersSubAreas(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_areas.html"))
	interni := &database.Area{ID: 1, Key: "interni", Name: "Personale comunale"}
	dir := &database.Area{ID: 2, Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}
	data := map[string]interface{}{
		"Messages":      map[string]string{"save": "Salva", "delete": "Elimina"},
		"AreaNodes":     []*database.AreaNode{{Area: interni, Children: []*database.Area{dir}}},
		"RuleCounts":    map[int64]int{1: 1, 2: 0},
		"AreaCounts":    map[string]int{"interni": 240},
		"SubAreaCounts": map[string]int{"dirigenti": 4},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_areas.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{`href="/admin/areas/2"`, "Dirigenti", `name="parent_key" value="interni"`, "nessuna regola", "/admin/areas/2/move?dir=up"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q nel frammento", want)
		}
	}
}

func TestAdminRulesBlockRenders(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_rules.html"))
	data := rulesBlock{
		Rules:           []ruleView{{ID: 7, Label: "Gruppo AD rubrica_dirigenti"}},
		PostURL:         "/admin/areas/2/rules",
		DeleteURLPrefix: "/admin/areas/2/rules/",
		Target:          "#area-detail-content",
		FormID:          "rule-2",
		AllowRange:      true,
		Error:           "nome di attributo LDAP non valido",
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_rules.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"Gruppo AD rubrica_dirigenti", `hx-post="/admin/areas/2/rules/7/delete"`, `value="range"`, "/admin/ad/suggest", "nome di attributo LDAP non valido"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q", want)
		}
	}
}

func TestAdminSuggestionsRender(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_ad_suggestions.html"))
	data := map[string]interface{}{
		"Target":      "rule-2-value",
		"Suggestions": []ldap.Suggestion{{Value: `CN=A"B,DC=x`, Label: "A\"B"}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_ad_suggestions.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `data-target="rule-2-value"`) || strings.Contains(buf.String(), `A"B,DC`) {
		t.Errorf("suggerimento non reso o non escapato: %s", buf.String())
	}
}

func TestAdminScopeRendersHeldWarning(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_scope.html", "../../web/templates/admin_rules.html"))
	data := map[string]interface{}{
		"ScopeRules": rulesBlock{PostURL: "/admin/scope/rules", DeleteURLPrefix: "/admin/scope/rules/", Target: "#scope-content", FormID: "scope"},
		"StaleHeld":  42,
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_scope.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"42 contatti", `hx-vals='{"force":"1"}'`, `hx-get="/admin/scope/preview"`, "Nessuna regola = tutti"} {
		if !strings.Contains(html, want) {
			t.Errorf("manca %q", want)
		}
	}
	if strings.Contains(html, `value="range"`) {
		t.Error("il perimetro non deve offrire regole range")
	}
}

func TestAdminScopePreviewRenders(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_scope_preview.html"))
	data := map[string]interface{}{"Preview": ldap.ScopePreview{In: 250, Current: 248, Entering: []string{"Nuovo"}, Leaving: []string{"Uscente"}, MoreLeaving: 3}}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_scope_preview.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"250", "248", "Nuovo", "Uscente", "altri 3"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("manca %q", want)
		}
	}
}

func TestSearchResultsShowsSubAreaTags(t *testing.T) {
	funcs := template.FuncMap{
		"initials": func(string) string { return "XX" }, "sentenceCase": func(s string) string { return s },
		"extList": func(s string) string { return s },
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFiles(
		"../../web/templates/search_results.html", "../../web/templates/group_category_node.html", "../../web/templates/group_row.html"))
	cw := &phonebook.ContactWithGroups{
		Contact:  &database.Contact{UID: "a", DisplayName: "Anna"},
		SubAreas: []phonebook.SubAreaTag{{Key: "dirigenti", Name: "Dirigente"}, {Key: "pc", Name: "Protezione civile"}},
	}
	data := map[string]interface{}{
		"Groups":   []*phonebook.DepartmentGroup{{Name: "Ragioneria", Contacts: []*phonebook.ContactWithGroups{cw}}},
		"Messages": map[string]string{"no_results": "Nessun risultato"},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "search_results.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Count(buf.String(), `class="sub-tag"`) != 2 || !strings.Contains(buf.String(), "Protezione civile") {
		t.Errorf("tag mancanti: %s", buf.String())
	}
}

func TestAdminContactsShowsSubAreaCheckboxes(t *testing.T) {
	tpl := template.Must(template.New("").ParseFiles("../../web/templates/admin_contacts.html"))
	c := &database.Contact{ID: 5, UID: "manual-x", DisplayName: "X", Area: "interni"}
	data := map[string]interface{}{
		"Messages":        map[string]string{"save": "Salva", "delete": "Elimina"},
		"Contacts":        []*database.Contact{c},
		"Areas":           []*database.Area{{Key: "interni", Name: "Personale comunale"}},
		"AreaNodes":       []*database.AreaNode{{Area: &database.Area{Key: "interni", Name: "Personale comunale"}, Children: []*database.Area{{Key: "dirigenti", Name: "Dirigenti", ParentKey: "interni"}}}},
		"ContactSubAreas": map[int64]map[string]bool{5: {"dirigenti": true}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin_contacts.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !regexp.MustCompile(`name="sub_areas" value="dirigenti"[^>]*checked`).MatchString(buf.String()) {
		t.Errorf("checkbox dirigenti non spuntata: %s", buf.String())
	}
}
