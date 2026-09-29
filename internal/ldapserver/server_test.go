package ldapserver

import (
	"net"
	"testing"
	"time"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/database"
	"github.com/Comune-di-Montesilvano/Rubrica/internal/phonebook"
	"github.com/go-ldap/ldap/v3"
)

const testPassword = "segreta"

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func seedDirectory(t *testing.T, db *database.DB) {
	t.Helper()
	now := time.Now()
	contacts := []*database.Contact{
		{UID: "mrossi", DisplayName: "Mario Rossi", LDAPExt: "759", LastSync: now},
		{UID: "lbianchi", DisplayName: "Luca Bianchi", LDAPExt: "700;701", LastSync: now},
		{UID: "gverdi", DisplayName: "Giulia Verdi", LDAPExt: "800", Disabled: true, LastSync: now},
	}
	for _, c := range contacts {
		if err := db.UpsertContact(c); err != nil {
			t.Fatalf("UpsertContact: %v", err)
		}
	}
	g := &database.GroupNumber{Number: "600", Name: "Reception"}
	if err := db.CreateGroup(g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := db.AddGroupMember(g.ID, contacts[0].ID); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
}

// startServer avvia il server su una porta libera con una rubrica di prova;
// withPassword=false simula il server appena installato (password mai impostata).
func startServer(t *testing.T, withPassword bool) (string, *database.DB) {
	t.Helper()
	db := newTestDB(t)
	if withPassword {
		if err := SaveSettings(db, "", "", testPassword); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}
	}
	seedDirectory(t, db)

	srv, err := New(db, phonebook.NewService(db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := freeAddr(t)
	go func() { _ = srv.Run(addr) }()
	t.Cleanup(func() { _ = srv.Stop() })

	deadline := time.Now().Add(2 * time.Second)
	for !srv.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("server LDAP non pronto")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, db
}

func dial(t *testing.T, addr string) *ldap.Conn {
	t.Helper()
	l, err := ldap.DialURL("ldap://" + addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func bindOK(t *testing.T, l *ldap.Conn) {
	t.Helper()
	if err := l.Bind(DefaultBindDN, testPassword); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func search(l *ldap.Conn, base, filter string, sizeLimit int, attrs ...string) (*ldap.SearchResult, error) {
	return l.Search(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		sizeLimit, 0, false, filter, attrs, nil))
}

// phoneAttrs sono gli attributi che chiede il template del telefono.
var phoneAttrs = []string{"cn", "sn", "displayName", "telephoneNumber", "mobile"}

func TestBindRejected(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	if err := l.Bind(DefaultBindDN, "sbagliata"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("password errata: err = %v, atteso invalidCredentials", err)
	}
	if err := l.Bind("cn=altro,dc=rubrica,dc=local", testPassword); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("DN errato: err = %v, atteso invalidCredentials", err)
	}
	if err := l.UnauthenticatedBind(""); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("bind anonimo: err = %v, atteso invalidCredentials", err)
	}
	if err := l.Bind("CN=Telefoni, DC=Rubrica, DC=Local", testPassword); err != nil {
		t.Errorf("bind con DN in maiuscolo: %v", err)
	}
}

func TestBindWithoutConfiguredPassword(t *testing.T) {
	addr, _ := startServer(t, false)
	l := dial(t, addr)
	if err := l.Bind(DefaultBindDN, "qualsiasi"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		t.Errorf("err = %v, atteso invalidCredentials senza password configurata", err)
	}
}

func TestSearchRequiresBind(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	_, err := search(l, DefaultBaseDN, "(objectClass=*)", 0)
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("err = %v, atteso insufficientAccessRights", err)
	}
}

func TestSearchByNumber(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	cases := map[string]string{"759": "Mario Rossi", "701": "Luca Bianchi", "600": "Reception"}
	for number, name := range cases {
		res, err := search(l, DefaultBaseDN, "(|(telephoneNumber="+number+")(mobile="+number+"))", 1000, phoneAttrs...)
		if err != nil {
			t.Fatalf("search %s: %v", number, err)
		}
		if len(res.Entries) != 1 || res.Entries[0].GetAttributeValue("displayName") != name {
			t.Errorf("numero %s: entries = %d, atteso 1 con displayName %q", number, len(res.Entries), name)
		}
	}

	res, err := search(l, DefaultBaseDN, "(|(telephoneNumber=800)(mobile=800))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search 800: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("contatto disabled restituito: %d entry", len(res.Entries))
	}
}

func TestSearchByName(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(|(cn=*ROSS*)(sn=*ROSS*))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].GetAttributeValue("telephoneNumber") != "759" {
		t.Fatalf("attesa 1 entry con interno 759, avute %d", len(res.Entries))
	}

	res, err = search(l, DefaultBaseDN, "(|(cn=*i*)(sn=*i*))", 1000, phoneAttrs...)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.GetAttributeValue("cn"))
	}
	want := []string{"Luca Bianchi", "Mario Rossi", "Reception"}
	if len(names) != len(want) {
		t.Fatalf("nomi = %v, attesi %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("ordine nomi = %v, atteso %v", names, want)
		}
	}
}

func TestSearchSizeLimit(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(telephoneNumber=*)", 2)
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		t.Fatalf("err = %v, atteso sizeLimitExceeded", err)
	}
	if res == nil || len(res.Entries) != 2 {
		t.Fatalf("attese 2 entry parziali, avuto %v", res)
	}
}

func TestSearchSelectedAttributes(t *testing.T) {
	addr, _ := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	res, err := search(l, DefaultBaseDN, "(telephoneNumber=759)", 0, "cn")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 1 || len(res.Entries[0].Attributes) != 1 || res.Entries[0].Attributes[0].Name != "cn" {
		t.Fatalf("atteso solo cn, avuto %+v", res.Entries)
	}
}

func TestSearchBaseDNChange(t *testing.T) {
	addr, db := startServer(t, true)
	l := dial(t, addr)
	bindOK(t, l)

	if _, err := search(l, "dc=voip,dc=local", "(objectClass=*)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Fatalf("base sconosciuta: err = %v, atteso noSuchObject", err)
	}

	if err := SaveSettings(db, "dc=comune,dc=local", "", ""); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if _, err := search(l, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Fatalf("vecchia base dopo la modifica: err = %v, atteso noSuchObject", err)
	}
	res, err := search(l, "dc=comune,dc=local", "(telephoneNumber=759)", 0)
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("nuova base: err = %v, entries = %v", err, res)
	}
	if res.Entries[0].DN != "uid=mrossi,ou=contatti,dc=comune,dc=local" {
		t.Errorf("DN = %q", res.Entries[0].DN)
	}
}

func TestReadyFalseWhenListenFails(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	db := newTestDB(t)
	srv, err := New(db, phonebook.NewService(db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Run(busy.Addr().String()); err == nil {
		t.Fatal("Run su porta occupata: atteso errore")
	}
	if srv.Ready() {
		t.Error("Ready() = true dopo listen fallito, atteso false")
	}
	if srv.Err() == nil {
		t.Error("Err() = nil dopo listen fallito")
	}
}

func TestAuthIsPerConnection(t *testing.T) {
	addr, _ := startServer(t, true)
	authed := dial(t, addr)
	bindOK(t, authed)
	anon := dial(t, addr)

	if _, err := search(anon, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("connessione non autenticata: err = %v, atteso insufficientAccessRights", err)
	}
	if res, err := search(authed, DefaultBaseDN, "(telephoneNumber=759)", 0); err != nil || len(res.Entries) != 1 {
		t.Fatalf("connessione autenticata: err = %v", err)
	}

	// un bind fallito sulla connessione autenticata la riporta a non autenticata
	_ = authed.Bind(DefaultBindDN, "sbagliata")
	if _, err := search(authed, DefaultBaseDN, "(telephoneNumber=759)", 0); !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Fatalf("dopo bind fallito: err = %v, atteso insufficientAccessRights", err)
	}
}
