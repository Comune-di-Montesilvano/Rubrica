# Integrazione dati centralino (PBX Invidea "ViVo") — Design

## Contesto

Il centralino aziendale (Invidea "ViVo", PHP + lighttpd, IP `10.0.90.253` in
questo ambiente) espone via web console una lista di peers SIP e call-group
("chiamate di gruppo") che non passano da LDAP/AD — sono interni non
sincronizzati nel dominio (es. numeri politici, gruppi comunali, uffici che
non hanno un account AD dedicato). L'obiettivo è arricchire la rubrica
esistente con questi dati, mantenendo LDAP come unica fonte per i contatti di
dominio e il PBX come fonte aggiuntiva per tutto il resto.

Non esiste un'API ufficiale documentata: l'integrazione fa screen-scraping
delle pagine interne autenticate della web console (vedi PoC in
`cmd/pbxpoc/main.go` per i dettagli di parsing, verificati contro dati reali).

## Endpoint PBX usati

- Login: `POST /verifica__login.php` (form: `username`, `password`,
  `redirect2=5`, `Submit3=Accedi`) → cookie di sessione `PHPSESSID`.
  Cert TLS self-signed, verifica disabilitata (dispositivo su IP privato).
- Peers: `GET /vivo.index.php?module=monitor&mode=peers` → HTML con una var
  JS `infopeers` = array JSON `{"defaultuser","callerid","status"}` per ogni
  interno SIP configurato.
- Call groups: `GET /vivo.index.php?module=extensions&mode=callgroups` →
  tabella HTML, una riga per gruppo: nome, interno (`number`), membri
  (`SIP/xxx`, uno o più), strategia di ring, timeout, stato attivo/disattivo.

Entrambi richiedono `Referer: <base>/vivo.php` e la sessione autenticata.

## Config

Nuove env var, tutte opzionali:

- `PBX_URL` — base URL del centralino (es. `https://10.0.90.253`). Se
  assente, l'intero subsystem PBX resta disattivo (nessun tentativo di
  connessione, nessun impatto sul resto dell'app).
- `PBX_USER`, `PBX_PASS` — credenziali login.

Il sync PBX gira nello stesso ticker orario già usato per `SyncContacts`
(LDAP) in `main.go` — non un ticker separato, un job aggiuntivo nello stesso
giro, eseguito anche lui una volta allo startup.

## Schema

### `contacts`

Nessuna nuova colonna. `source` acquisisce un terzo valore possibile:
`'pbx'` (oltre a `'ldap'` e `'manual'` già esistenti).

Per un peer PBX non in dominio:
- `uid = "pbx-" + extension` (prefisso dedicato, come già fa `manual-` per i
  contatti manuali, per garantire l'unicità e riconoscere la fonte da `uid`).
- `ldap_ext = extension`
- `primary_number = extension` (il numero PBX è già il numero dialabile
  reale — a differenza di LDAP non si applica `PRIMARY_NUMBER_PREFIX_TEMPLATE`).
- `display_name = callerid` (dal centralino)
- `department = "Centralino - non mappato"` (valore di default, riusa il
  raggruppamento per reparto già esistente in `phonebook.GroupByDepartment`
  — nessuna nuova UI necessaria per farli comparire in rubrica).
- `area` — vuoto di default, assegnabile a mano dall'admin come per i
  contatti manuali (stesso meccanismo, nessun codice nuovo).
- `manual_override` — stesso significato di oggi: se `1`, il sync PBX non
  tocca più `display_name`/`department`/`area`/`email`/altri campi testuali
  per quella riga (blocco totale via override, non solo sul nome).

### `group_numbers`

- **+ colonna `source TEXT NOT NULL DEFAULT 'manual'`** (stesso pattern di
  `contacts.source`, valori `'manual' | 'pbx'`).
- **+ colonna `name_override INTEGER NOT NULL DEFAULT 0`** — a differenza di
  `contacts.manual_override` (che blocca tutto), qui blocca *solo* il nome:
  i membri di una riga `source='pbx'` sono **sempre** sovrascritti dal
  centralino ad ogni sync, indipendentemente da `name_override` (il mapping
  peers→gruppo è dichiarato fonte di verità assoluta lato PBX).

## Sync flow (nuovo package `internal/pbx`)

Mirror strutturale di `internal/ldap/sync.go`: una funzione `SyncPBX(db, cfg)`
chiamata dal ticker in `main.go`, no-op silenzioso se `cfg.PBXURL == ""`.

1. **Login** verso `PBX_URL`. Se fallisce (rete giù, credenziali cambiate):
   log dell'errore, skip dell'intero giro — **non tocca dati esistenti**.
   Stesso principio di resilienza già in uso per il sync LDAP: un errore
   transitorio non deve mai cancellare dati validi dal giro precedente.

2. **Fetch peers** (`monitor&mode=peers`), parse `infopeers`.

3. **Fetch call groups** (`extensions&mode=callgroups`), parse tabella.

4. **Applica peers**:
   - Costruisci l'insieme degli interni già in dominio:
     `SELECT ldap_ext FROM contacts WHERE source='ldap' AND ldap_ext IS NOT NULL AND ldap_ext != ''`
     (esplicitamente filtrato per `source='ldap'`, non su tutti i contatti —
     altrimenti un peer PBX già sincronizzato escluderebbe se stesso al giro
     successivo).
   - Per ogni peer con `defaultuser` **non** in quell'insieme: upsert su
     `contacts` con `uid="pbx-"+defaultuser`, tramite un nuovo metodo
     `UpsertPBXContact` che replica il pattern `CASE WHEN manual_override=0
     THEN excluded.X ELSE X END` già usato da `UpsertContact`, ma
     impostando `source='pbx'` esplicitamente nell'INSERT (che oggi
     `UpsertContact` non fa — vedi nota sotto).
   - Soft-delete (`deleted_at`, stesso meccanismo di `SoftDeleteStale`) dei
     contatti `source='pbx'` il cui `ldap_ext` non compare più tra i peers
     di questo giro.

   > Nota implementativa: `UpsertContact` esistente non scrive mai la
   > colonna `source` (si affida al `DEFAULT 'ldap'` dello schema e non la
   > tocca in `ON CONFLICT`) — va bene così per LDAP, ma il nuovo metodo per
   > i peers PBX deve impostarla esplicitamente all'`INSERT` e non
   > sovrascriverla mai in `ON CONFLICT` (comportamento invariato dopo la
   > creazione).

5. **Applica call groups**: per ogni gruppo PBX (chiave: `number` = interno
   del gruppo):
   - Se esiste già una riga `group_numbers` con quel `number` e
     `source='manual'`: il suo `name` (se diverso da quello riportato dal
     PBX) viene copiato come override sulla nuova riga PBX, poi la riga
     manuale viene eliminata — **nessuna perdita del lavoro fatto
     dall'admin**.
   - Upsert riga `group_numbers` `source='pbx'` per quel `number`: `name`
     gated da `name_override` (stesso pattern CASE), `description` libera.
   - Membri: `DELETE FROM group_members WHERE group_id=?` seguito da
     reinsert basato sugli `SIP/xxx` del gruppo, risolvendo ogni `xxx` a un
     `contact_id` tramite `ldap_ext=xxx` (funziona sia per contatti LDAP che
     per peers PBX appena creati/aggiornati al punto 4 — l'ordine di
     applicazione peers-poi-callgroups nello stesso giro è quindi
     importante). Un `SIP/xxx` che non risolve a nessun contatto viene
     loggato e ignorato (non blocca il resto del gruppo).
   - Gruppi `source='pbx'` scomparsi dal centralino in questo giro:
     **cancellazione diretta** (riga `group_numbers` + `group_members`
     associati) — nessun soft-delete/audit trail per questi dati esterni,
     a differenza dei contatti.

## Error handling

- Errori di rete/parsing durante login o fetch → log, skip dell'intero giro,
  nessuna modifica al DB.
- Un singolo peer/gruppo con dati inattesi (regex non matcha, JSON malformato
  per una riga) → skip di quella riga singola con log, il resto del giro
  prosegue (stesso principio di tolleranza già usato nel sync LDAP per righe
  LDAP malformate).

## Testing

- Unit test di parsing (`infopeers` JSON + tabella call groups) su fixture
  HTML statiche (le pagine reali catturate durante l'esplorazione di questa
  sessione, salvate come testdata).
- Unit test della logica di merge/override (peers esclusi per dominio,
  migrazione nome manuale→override, sovrascrittura membri) con sqlite
  in-memory, stesso pattern di `internal/database/sqlite_test.go`.
- Nessun test contro il centralino reale in CI (richiede rete interna e
  credenziali — resta manuale/PoC come `cmd/pbxpoc`).

## Fuori scope (YAGNI)

- Nessuna UI admin dedicata per il PBX: gli override si fanno con la UI
  esistente (edit contatto per i peers, edit gruppo per i call group).
- Nessuna gestione di più centralini contemporanei.
- Nessuna cache/retry sofisticata sul login — un fallimento salta il giro,
  il prossimo tick (un'ora dopo) riprova.
