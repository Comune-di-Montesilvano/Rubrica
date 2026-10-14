package main

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
