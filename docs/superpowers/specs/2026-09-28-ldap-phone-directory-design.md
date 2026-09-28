# Rubrica telefoni via LDAP — Design

## Contesto

I telefoni VoIP (modello TJE-282P, OEM con firmware stile Fanvil) oggi leggono
la rubrica dall'LDAP integrato nel centralino ViVo (`10.0.90.253:389`,
base `dc=voip,dc=local`), configurato via auto-provisioning del centralino.
Quella rubrica non contiene i dati di Rubrica (contatti AD, PBX, manuali,
gruppi di chiamata con nomi curati).

Obiettivo: Rubrica espone un **server LDAPv3 read-only** che i telefoni
interrogano al posto dell'LDAP del ViVo. Il passaggio avviene cambiando il
template di provisioning sul centralino (server/porta/base/credenziali/filtro
nomi) — nessuna configurazione telefono per telefono.

Requisiti chiave:
- ricerca per nome dal telefono (rubrica);
- **ricerca per numero** per il nome del chiamante in arrivo: il caller ID
  che arriva al telefono è l'interno (`700`, `759`, `800`...);
- stessi dati e stesso perimetro della rubrica pubblica.

## Scelta tecnica

Libreria `github.com/jimlambrt/gldap` (server LDAPv3 in Go, router
bind/search, parsing filtri) integrata nel binario. Scartati: implementazione
a mano su `go-asn1-ber` (troppo codice di protocollo da mantenere) e container
OpenLDAP separato alimentato da LDIF (servizio in più, dati in ritardo,
credenziali doppie).

## Architettura

Nuovo package `internal/ldapserver` (distinto da `internal/ldap`, che resta
il client verso AD):

- `server.go` — avvio/arresto listener, handler bind e search.
- `entries.go` — costruzione delle entry LDAP da contatti e gruppi.
- `filter.go` — valutazione di un filtro LDAP su una entry in memoria.

Avvio in `cmd/server/main.go` accanto al server HTTP, in una goroutine; errori
del listener loggati con prefisso `[LDAPSRV]`, non fatali per l'HTTP.

### Porta

- Env `LDAP_SERVER_PORT` (default `3389`). Il container gira come utente non
  root `rubrica` → non può aprire porte < 1024 all'interno.
- `LDAP_SERVER_PORT` vuota → server LDAP non avviato.
- `docker-compose.yml`: mapping `"${LDAP_SERVER_PORT_HOST:-389}:${LDAP_SERVER_PORT:-3389}"`
  e `LDAP_SERVER_PORT` nella lista `environment:` (niente `env_file`, vedi
  CLAUDE.md). `Dockerfile`: `EXPOSE 3389`.
- Nessun TLS: il traffico resta sulla LAN voce, come l'LDAP attuale del ViVo.

### Configurazione (admin)

Chiavi in `app_config`, editabili da una nuova pagina admin
`/admin/phone-directory` (voce propria nel rail, `Section`
`"admin-phone-directory"`, stesso schema delle altre pagine admin):

- `ldapsrv_base_dn` — default `dc=rubrica,dc=local`;
- `ldapsrv_bind_dn` — default `cn=telefoni,dc=rubrica,dc=local`;
- `ldapsrv_bind_password` — nessun default; mai ri-mostrata in pagina
  (campo vuoto = password invariata, come per il PBX).

La pagina mostra anche lo stato (server in ascolto sì/no, porta) e un riepilogo
dei valori da mettere nel template di provisioning del ViVo (vedi sotto).

Config letta dal DB a ogni richiesta (bind/search): una modifica da admin vale
subito, senza riavvio.

### Autenticazione

- Solo **simple bind** con `ldapsrv_bind_dn` + `ldapsrv_bind_password`
  (confronto DN case-insensitive, password con `subtle.ConstantTimeCompare`).
- Password non configurata → ogni bind rifiutato (`invalidCredentials`):
  server acceso ma chiuso finché l'admin non la imposta.
- Bind anonimo e search senza bind riuscito → rifiutati
  (`invalidCredentials` / `insufficientAccessRights`).

## Entry

Base DN = `ldapsrv_base_dn` (sotto: `$BASE`).

**Contatto** (perimetro = rubrica pubblica: non cancellato, `disabled=0`,
qualunque `source`), solo se ha almeno un interno in `LDAPExt`:

```
dn: uid=<uid>,ou=contatti,$BASE
objectClass: top, person, organizationalPerson, inetOrgPerson
uid: <uid>
cn: <DisplayName>
displayName: <DisplayName>
sn: <ultima parola di DisplayName>       (DisplayName intero se una sola parola)
givenName: <DisplayName senza l'ultima parola>   (omesso se vuoto)
telephoneNumber: <interno>               (un valore per ogni interno di LDAPExt)
ou: <Department>                         (omesso se vuoto)
title: <Title>                           (omesso se vuoto)
mail: <Email>                            (omesso se vuoto)
```

**Gruppo di chiamata** (`group_numbers`, perimetro = rubrica pubblica: esclusi
i gruppi con zero membri attivi, stesso criterio di `handleSearch`):

```
dn: cn=<Number>,ou=gruppi,$BASE
objectClass: top, person, inetOrgPerson
cn: <Name>
displayName: <Name>
sn: <Name>
telephoneNumber: <Number>
```

Più le entry di struttura `$BASE`, `ou=contatti,$BASE`, `ou=gruppi,$BASE`
(restituite solo se il filtro le seleziona, es. `(objectClass=*)` con scope
`base`).

Solo l'interno viene pubblicato (niente `PrimaryNumber`): chiamando dalla
rubrica il telefono fa una chiamata interna, e il caller ID in arrivo è
l'interno.

## Search

- Dati letti dal DB a ogni search (poche centinaia di righe); filtro e
  ordinamento (per `cn`, case-insensitive) in memoria.
- Scope: `base` → solo l'entry col DN esatto; `one` → figli diretti;
  `sub` → tutto il sottoalbero. DN base fuori da `$BASE` → `noSuchObject`.
- Attributi richiesti: se la lista è vuota o `*` → tutti; altrimenti solo
  quelli richiesti (nomi case-insensitive).
- `sizeLimit` della richiesta (0 = nessuno) con tetto interno di 1000;
  superato → risultati troncati + `sizeLimitExceeded`.

### Filtri supportati

`&`, `|`, `!`, uguaglianza, substring (`*` iniziale/intermedio/finale),
presenza (`attr=*`). Confronto case-insensitive su tutti gli attributi.
Qualsiasi altro tipo (`>=`, `<=`, `~=`, extensible) → nessun match per quel
sotto-filtro (non errore). Attributo inesistente su una entry (es. `mobile`)
→ nessun match.

`telephoneNumber=759` è un'uguaglianza esatta → lookup caller ID.

## Template di provisioning ViVo

Valori da impostare nel template del centralino (sezione LDAP del telefono):

| Campo | Valore |
|---|---|
| Indirizzo del server | IP host Docker di Rubrica |
| Porta | `389` (o `LDAP_SERVER_PORT_HOST`) |
| Base | `ldapsrv_base_dn` |
| Nome utente / Password | `ldapsrv_bind_dn` / `ldapsrv_bind_password` |
| Filtro nome | `(\|(cn=*%*)(sn=*%*))` — wildcard aggiunti rispetto a oggi |
| Filtro numerico | `(\|(telephoneNumber=%)(mobile=%))` — invariato |
| Attributi nome | `cn sn displayName` — invariato |
| Attributi numero | `telephoneNumber mobile` — invariato |
| Nome visualizzato | `%displayName` — invariato |
| Protocollo | Versione 3 |

Prerequisito di rete: il firewall deve permettere il traffico VLAN telefoni →
host Rubrica sulla porta LDAP.

## Test

- `filter_test.go`: tabella filtro → entry → match atteso (uguaglianza,
  substring, `|`/`&`/`!`, presenza, attributo mancante, case-insensitive,
  multi-valore `telephoneNumber`).
- `entries_test.go`: contatto con più interni, senza interno (escluso),
  nome di una parola (`sn`), disabled escluso, gruppo senza membri attivi
  escluso.
- `server_test.go` (integrazione): server su porta casuale con DB SQLite
  temporaneo, client `go-ldap` (già in `go.mod`): bind ok, bind con password
  errata, search senza bind, search per nome con wildcard, search per numero
  esatto, `sizeLimit`, attributi selezionati.

## Fuori scope

TLS/LDAPS, scrittura (add/modify/delete → `unwillingToPerform`), più utenti di
bind, numeri esterni/cellulari, rubrica XML remota.
