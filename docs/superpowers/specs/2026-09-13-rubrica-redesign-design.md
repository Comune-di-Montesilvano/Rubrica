# Rubrica LdavSync — redesign funzioni e rappresentazione dati

Data: 2026-09-13
Stato: approvato in brainstorming, in attesa di piano di implementazione

## Contesto

L'app è ferma da mesi, appena rimessa in funzione (sync LDAP ripristinato, 231
contatti attivi, Comune di Montesilvano). L'interfaccia attuale ha tre problemi
concreti riscontrati durante l'analisi:

1. **Incoerenza visiva**: `phonebook.html`/`search_results.html` usano CSS
   custom, `contact_detail.html` e tutto l'admin usano Tailwind via CDN — due
   sistemi di stile scollegati, bottoni blu/verde/rosso generici, badge
   colorati senza significato.
2. **Contatori sidebar rotti**: solo il totale contatti si aggiorna via JS;
   Interni/Esterni/Area Politica restano bloccati su "...".
3. **Area (Interni/Esterni/Politica) non è un dato**: viene dedotta ad ogni
   ricerca facendo `strings.Contains` sul DN del contatto
   (`main.go:handleSearch`) — fragile, e per questo i contatori non si possono
   nemmeno calcolare lato server in modo semplice.

Analisi sui dati reali (231 contatti sincronizzati, 425 entry LDAP totali):

| Campo | Stato reale |
|---|---|
| `Title` (LDAP `title`) | 0/206 popolato — campo morto, mai scritto in AD |
| `Description` | popolato quasi sempre, ma mescola ruolo (Dirigente, Funzionario, Comandante), mansione (Imu, Tari, Anagrafe), carica politica (Assessore, Consigliere) — con typo/duplicati da inserimento libero (*"Agente di Polizi Locale"* / *"...Polizia Locale"* / *"...polizia Locale"*, *"Edilizia"/"Edliizia"*, *"Istruttore"/"Istuttore"*) |
| `Department` (da `physicalDeliveryOfficeName`) | 20 valori distinti, pulito, memorizzato in maiuscolo AD ("POLIZIA LOCALE") |
| `Email` | quasi completo (203/206) |
| Area (Interni/Esterni/Politica) | strutturale nell'OU LDAP (`OU=INTERNI`/`OU=ESTERNI`/`OU=AREA_POLITICA`), stabile — oggi non persistita |
| `LDAPGroups` | rumore ACL AD (`share_xxx_rw/ro`) — dato tecnico, non organizzativo, correttamente mai mostrato in UI |
| OU `DUMMY` | 172 entry (ex-dipendenti/pensionati/dimissioni), correttamente esclusa dal filtro disabled-bit già in `sync.go` — nessuna modifica necessaria lì |

## Obiettivi

- Un solo linguaggio visivo su tutte le pagine (pubbliche + admin), dark mode
  automatico via `prefers-color-scheme`.
- Area come campo persistito, non calcolato a runtime — sistema i contatori
  sidebar e rende il filtro robusto.
- `Description` normalizzata a sync-time (dedup typo/casing) invece di
  mostrata grezza.
- Il numero di telefono è l'elemento centrale della UI (il caso d'uso reale è
  "trovo il numero, chiamo"), con prefisso esterno mostrato una sola volta
  (helper dismissibile) invece che ripetuto su ogni riga.
- `Title` sparisce dall'UI (resta in DB per compatibilità, non si legge più).

## Non-obiettivi

- Non si tocca la logica di sync/filtro LDAP (`sync.go`, group filtering,
  disabled-bit) — già verificata corretta sui dati reali.
- Non si tocca CardDAV (`internal/carddav`) — fuori scope, nessun problema
  riscontrato.
- Non si introduce login/ruoli oltre l'esistente (admin via gruppo LDAP).
- Niente redesign della feature "etichette numero" (group_numbers) a livello
  di modello dati — solo restyling della UI che la gestisce.

## Modifiche al modello dati

### Nuova colonna `contacts.area`

`ALTER TABLE contacts ADD COLUMN area TEXT` — valori: `interni` / `esterni` /
`politica` / `` (vuoto se non determinabile). Calcolata in `sync.go` durante
`SyncContacts`, deducendola dall'OU del DN LDAP (`OU=INTERNI` ecc.), con lo
stesso criterio già usato (ma runtime, in `main.go`) per il filtro sidebar.
Migrazione via `ALTER TABLE ... ADD COLUMN` idempotente, stesso pattern già
usato per `title`/`description` in `sqlite.go:migrate()`.

Query `ListContactsWithGroups`/`SearchContactsWithGroups` restano identiche
nella firma; si aggiunge un metodo `CountByArea() (map[string]int, error)`
per popolare i contatori sidebar lato server (niente più JS che legge
`len(.Results)` e basta).

### Normalizzazione `description`

In `sync.go`, prima dell'upsert, si passa `description` per una funzione di
normalizzazione (mappa statica alias → canonico, case-insensitive,
trim/collassa spazi). Esempio di mappa iniziale (da estendere quando si
trovano nuovi typo):

```go
// chiavi in minuscolo: il lookup normalizza il valore letto da LDAP con
// strings.ToLower prima del confronto, quindi varianti di sola maiuscola
// ("Pubblica istruzione" vs "Pubblica Istruzione") collassano già così.
var descriptionAliases = map[string]string{
    "agente di polizi locale":  "Agente di Polizia Locale",
    "agente di polizia locale": "Agente di Polizia Locale",
    "edliizia":                 "Edilizia",
    "istuttore":                "Istruttore",
    "usciere":                  "Usciere",
}
```

La normalizzazione **non modifica LDAP**, solo il valore scritto in
`contacts.description` — resta soggetta a `manual_override` come oggi. Non
si introduce un secondo campo "ruolo" separato da "ambito": sono troppo
intrecciati nei dati reali (per i profili politici `description` *è* la
carica) per giustificare uno split strutturale ora; si documenta come
possibile evoluzione futura se la mappa alias cresce troppo.

### `title` non più letto in UI

Nessuna modifica DB (resta la colonna, dato storicamente sempre vuoto);
si rimuove solo dai template.

## Sistema visivo

### Palette (token, light + dark via `prefers-color-scheme`)

| Token | Light | Dark | Uso |
|---|---|---|---|
| `--ink` | `#1B242E` | `#E7E4DC` | testo primario |
| `--ink-soft` | `#5B6672` | `#9BA3AB` | testo secondario (ruolo, label) |
| `--paper` | `#F3F1EC` | `#14181C` | sfondo pagina |
| `--paper-raised` | `#FFFFFF` | `#1B2025` | card, input, pannelli |
| `--line` | `#DEDAD1` | `#2A2F34` | separatori |
| `--accent` (verdigris) | `#3E6E63` | `#6FAE9E` | selezione, stato attivo, struttura |
| `--accent-tint` | `#E8EFED` | `#1D2723` | sfondo stato attivo/helper |
| `--muted` | `#9B958A` | `#6B7178` | testo terziario, placeholder |
| `--danger` (mattone tenue) | `#9B4A3F` | `#D48477` | azioni distruttive, mai colore acceso |

Un solo colore d'accento con significato strutturale (verdigris); nessun
codice-colore per Area — l'area politica si segnala con un'etichetta
testuale discreta, non un badge colorato aggiuntivo (evita l'effetto
"arcobaleno" scartato nella revisione).

### Tipografia

- **Archivo** (500/600/700) — intestazioni reparto, brand, titoli pagina.
  Registro da segnaletica istituzionale, non un default SaaS.
- **Inter** (400/500/600), cifre tabulari (`font-variant-numeric:
  tabular-nums`) — corpo testo, righe dati, numeri di telefono. Gli allinea
  in colonna come in un registro.
- Reparti normalizzati a sentence case in UI ("Polizia Locale") anche se il
  dato AD è tutto maiuscolo — nessun maiuscolo-tutto generato dalla pagina.
- Niente separatori "·" tra ruolo e reparto — due righe, peso tipografico
  diverso (evita il tic "A · B · C").

### Iconografia

SVG line-icon (stroke, no fill) per ricerca, telefono, aree, reparto — non
emoji, non icone a blocco pieno. Badge iniziali: rettangolo outline (non
bolla colorata piena) — registro "tesserino", non avatar SaaS generico.

## Struttura pagine

### Vista principale (`/`, `search_results.html`)

Sostituisce tabella + card grid con **vista unica a gruppi collassabili**:
intestazione reparto stile divisore da schedario (piatta, sticky, badge
conteggio) contenente righe dense (una persona per riga: badge tesserino,
nome+ruolo su due righe, numero interno grande a destra). Sidebar Area
(Tutti/Interni/Esterni/Politica) con conteggi reali da `CountByArea()`.

Ricerca: filtra sia intestazioni reparto (nascoste se nessun match dentro)
che righe persona; reparti con match restano espansi, gli altri collassati.

**Helper prefisso**: banner dismissibile sotto la ricerca ("Da fuori l'ente,
componi 085 448 prima dell'interno mostrato sotto") — unico posto dove il
prefisso compare. Ogni riga persona mostra solo l'interno (es. `1700`),
grande, `tel:` dietro compone comunque il numero completo. Stato
dismiss in `localStorage` (non serve persistere server-side).

### Dettaglio contatto (`contact_detail.html`)

Riscritta nello stesso linguaggio (via CSS del progetto, non più Tailwind
CDN): badge tesserino, nome, ruolo normalizzato, reparto come etichetta
discreta, campi (interno, email) con icona line, due azioni con gerarchia
chiara — primaria piena scura (chiama interno), secondaria solo contorno
(esporta vCard) — non due bottoni identici come oggi.

### Mobile (tutte le viste pubbliche)

Sidebar Area diventa tab orizzontali sotto la ricerca (scroll orizzontale).
Righe più compatte (badge 26px). Nessuna vista "mobile separata": stesso
markup, breakpoint CSS.

### Admin (`admin.html`, `admin_groups.html`, `admin_group_members.html`,
`login.html`)

Sidebar admin dedicata (Panoramica / Etichette numero / Configurazione)
invece di navbar Tailwind generica. Card "ultima sync" con orario grande
tabulare + singolo bottone azione primaria (scuro pieno) — non più
blu/verde/rosso senza gerarchia. Tabella etichette numero: azione
"Elimina" in `--danger` tenue, non `text-red-600` acceso. Form
etichetta inline (nessun modal fullscreen a scomparsa nascosto via CSS).

`login.html` non ancora rivisto in dettaglio nel brainstorming — da
allineare allo stesso design system nel piano di implementazione (form
semplice, stesso token set).

## Mockup di riferimento

Prodotti nella sessione di brainstorming via companion visivo, conservati in
`.superpowers/brainstorm/` (non versionati, `.gitignore` aggiornato):
`revolution-v3.html` (vista principale + helper prefisso, approvata),
`revolution-v4-detail-mobile.html` (dettaglio + mobile, approvata),
`revolution-v5-admin.html` (admin, approvata). Da usare come riferimento
diretto per markup/CSS nell'implementazione — non ripartire da zero.

## Rischi e note di migrazione

- `ALTER TABLE contacts ADD COLUMN area` su DB di produzione già popolato:
  serve un backfill una tantum (o semplicemente aspettare il prossimo sync
  orario, che already ri-scrive tutti i contatti attivi) — nessuna riga
  resta con `area` vuoto per più di un ciclo di sync.
- La mappa `descriptionAliases` è manuale e andrà estesa quando emergono
  nuovi typo in AD — non è validazione automatica, è una lista curata.
- Nessuna test suite esiste oggi (`go test ./...` non trova test) — il piano
  di implementazione dovrebbe includere almeno un test per la nuova logica
  di derivazione `area` e per la normalizzazione `description`, essendo
  logica pura facilmente testabile senza LDAP/DB reali.
- Rimuovere Tailwind CDN da `contact_detail.html`/admin richiede portare le
  utility usate oggi (grid, spacing) nel CSS custom del progetto
  (`web/static/css/style.css`) — non è solo un restyling di colore.
