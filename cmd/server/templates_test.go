package main

import (
	"bytes"
	"html/template"
	"regexp"
	"testing"
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
