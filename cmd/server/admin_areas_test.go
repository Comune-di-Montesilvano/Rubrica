package main

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
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
