package ldap

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Comune-di-Montesilvano/Rubrica/internal/arearules"
	"github.com/go-ldap/ldap/v3"
)

// AttrStat descrive un attributo AD compilato sugli utenti, per scegliere
// l'attributo di una regola in admin (porting da CruscottoPA).
type AttrStat struct {
	Name     string // minuscolo, come salvato nelle regole
	Count    int
	Examples []string // fino a 3 valori, i più frequenti
}

// Attributi tecnici o identificativi, inutili per raggruppare le persone.
var technicalAttrs = map[string]bool{
	"objectclass": true, "objectguid": true, "objectsid": true, "objectcategory": true,
	"distinguishedname": true, "cn": true, "name": true, "samaccountname": true,
	"userprincipalname": true, "samaccounttype": true, "useraccountcontrol": true,
	"primarygroupid": true, "memberof": true, "instancetype": true, "codepage": true,
	"countrycode": true, "logoncount": true, "badpwdcount": true, "badpasswordtime": true,
	"lastlogon": true, "lastlogontimestamp": true, "lastlogoff": true, "pwdlastset": true,
	"accountexpires": true, "whencreated": true, "whenchanged": true, "usncreated": true,
	"usnchanged": true, "dscorepropagationdata": true, "logonhours": true,
	"usercertificate": true, "thumbnailphoto": true, "jpegphoto": true, "mail": true,
	"proxyaddresses": true, "displayname": true, "givenname": true, "sn": true,
	"msds-supportedencryptiontypes": true, "lockouttime": true, "admincount": true,
}

// computeAttrStats conta gli attributi testuali brevi compilati, ordinati
// per numero di utenti (poi nome).
func computeAttrStats(entries []*ldap.Entry) []AttrStat {
	counts := map[string]int{}
	values := map[string]map[string]int{}
	for _, e := range entries {
		for _, a := range e.Attributes {
			key := strings.ToLower(a.Name)
			if technicalAttrs[key] || !arearules.ValidAttrName(a.Name) || len(a.Values) == 0 {
				continue
			}
			v := strings.TrimSpace(a.Values[0])
			if v == "" || len(v) > 80 || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\x01\x02\x03") {
				continue
			}
			counts[key]++
			if values[key] == nil {
				values[key] = map[string]int{}
			}
			values[key][v]++
		}
	}
	out := make([]AttrStat, 0, len(counts))
	for key, n := range counts {
		vs := make([]string, 0, len(values[key]))
		for v := range values[key] {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool {
			if values[key][vs[i]] != values[key][vs[j]] {
				return values[key][vs[i]] > values[key][vs[j]]
			}
			return vs[i] < vs[j]
		})
		if len(vs) > 3 {
			vs = vs[:3]
		}
		out = append(out, AttrStat{Name: key, Count: n, Examples: vs})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}
