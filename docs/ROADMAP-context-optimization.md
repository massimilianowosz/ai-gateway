# Roadmap task: Context Optimization per Ubiquum Gateway

## Obiettivo

Aggiungere a Ubiquum un layer di ottimizzazione del contesto nativo del
gateway, complementare a HiveCache, HiveState e HiveRoute.

Il risultato desiderato combina quattro strategie indipendenti:

```text
Richiesta
  -> HiveCache: riuso di risposte semanticamente equivalenti
  -> Live Optimizer: compressione deterministica del nuovo tool output
  -> HiveState: sostituzione della history lunga con stato strutturato
  -> Cache Policy: scelta tra prompt-cache stability e riduzione token
  -> HiveRoute: modello ed effort adeguati alla difficoltà
  -> Provider
```

La roadmap non prevede il porting di Headroom. Le idee utili vengono
ridisegnate per il modello operativo di Ubiquum: singolo binario Go,
multi-tenancy, streaming, API OpenAI/Anthropic/Responses e fail-open.

## Invarianti

- [ ] Nessuna trasformazione modifica system prompt o prompt umano per default.
- [ ] Codice sorgente e contenuti brevi restano passthrough per default.
- [ ] Ogni trasformatore è deterministico e fail-open.
- [ ] Il percorso `audit` non modifica mai il body inoltrato al provider.
- [ ] Ogni stato o contenuto salvato è isolato almeno per tenant, chiave e
      sessione.
- [ ] Le API native mantengono passthrough byte-for-byte quando
      l'ottimizzazione non è esplicitamente abilitata.
- [ ] Streaming e tool call non vengono degradati silenziosamente.
- [ ] I risparmi dichiarati sono netti del costo di embedding, estrazione,
      retrieval e richieste interne.
- [ ] Nessuna percentuale di risparmio entra nella documentazione pubblica
      prima di un benchmark riproducibile.

## Non-obiettivi iniziali

- Runtime Python, ONNX o modelli ML nel percorso caldo.
- Compressione generica del codice sorgente.
- Persistent memory, failure learning o modifica automatica di `AGENTS.md`.
- Image compression.
- Tool Output Intelligence adattiva prima di avere CCR e telemetry affidabili.
- Loop di tool calling trasparenti per richieste streaming nella prima release.

---

## Milestone 0 — Correttezza e contratto

### [ ] OPT-001 — Definire il contratto di ottimizzazione

**Priorità:** P0  
**Dimensione:** S  
**Dipendenze:** nessuna

Definire i modi pubblici senza implementare ancora le trasformazioni:

- `off`: passthrough completo;
- `audit`: analisi e metriche, body immutato;
- `cache`: preservazione del prefisso, ottimizzazione del solo delta recente;
- `token`: massima riduzione, HiveState autorizzato a riscrivere la history;
- `auto`: scelta economica dinamica, introdotta in una milestone successiva.

**Criteri di accettazione**

- La precedenza tra configurazione globale, tenant, chiave e header è definita.
- Opt-out e opt-in sono coerenti su Chat Completions, Messages e Responses.
- Il comportamento di default rimane compatibile con la release precedente.
- Header e nomi di configurazione sono documentati prima dell'uso nel codice.

### [ ] OPT-002 — Aggiungere test end-to-end sul body realmente inoltrato

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OPT-001

I test di `HiveState.Process()` non sono sufficienti: devono verificare il body
che il middleware HTTP consegna al proxy/provider.

**Criteri di accettazione**

- Test per OpenAI Chat Completions.
- Test per Anthropic Messages con blocchi `tool_use` e `tool_result`.
- Test per Responses API con `function_call` e `function_call_output`.
- Verifica che recent window e sequenze tool restino atomiche.
- Verifica esplicita di ogni messaggio sintetico che deve raggiungere upstream.
- Verifica che un errore di rewrite ripristini il body originale.

### [ ] OPT-003 — Allineare CCR, Code Registry e CacheAligner al percorso HTTP

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OPT-002

Oggi `HiveState.Process()` costruisce `Result.Messages` con CCR, Code Registry e
system prompt allineato, mentre i rewriter HTTP ricostruiscono la richiesta da
`StateJSON` e recent window. Decidere esplicitamente per ciascun elemento se:

1. deve essere inoltrato e quindi va rappresentato nel body raw; oppure
2. non fa parte della feature corrente e va rimosso dalle dichiarazioni
   pubbliche finché non viene implementato.

**Criteri di accettazione**

- Nessuna feature viene riportata nelle metriche o nella documentazione se non
  è presente nel body upstream.
- CacheAligner diventa detector-only: hash, drift e warning, senza mutazione del
  system prompt.
- Il CCR legacy non può reiniettare contenuti tra tenant o sessioni diverse.
- I nuovi test falliscono se un messaggio sintetico viene perso nel rewrite.

---

## Milestone 1 — Accounting e osservabilità

### [ ] OBS-001 — Normalizzare l'usage della provider prompt cache

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OPT-001

Estendere l'usage interno per distinguere:

- input token normali;
- input token letti dalla cache;
- input token scritti nella cache;
- output token;
- reasoning token;
- valore misurato o stimato.

**Criteri di accettazione**

- OpenAI/Azure mappano `cached_tokens` senza perderlo nello spend record.
- Anthropic/Vertex/Bedrock mappano cache read e cache creation quando presenti.
- Responses native e Responses tradotte producono lo stesso modello interno.
- Streaming e non-streaming convergono sullo stesso accounting.
- L'assenza del dettaglio provider degrada a `estimated`, non a zero.

### [ ] OBS-002 — Rendere il pricing cache-aware

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OBS-001

**Criteri di accettazione**

- Il catalogo prezzi supporta input, cached input e cache creation.
- `computeCost` usa le categorie corrette senza doppio conteggio.
- Le configurazioni modello possono sovrascrivere ogni tariffa.
- Billing mode `flat` continua a produrre costo zero.
- Budget enforcement usa il costo cache-aware.
- Test con usage misurato, parziale e completamente stimato.

### [ ] OBS-003 — Introdurre un Optimization Metric unificato

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OBS-001, OBS-002

Registrare per richiesta:

- token prima e dopo ogni stage;
- token inviati e cached token osservati;
- trasformazioni applicate o simulate;
- costo evitato lordo;
- costo interno di embedding, estrazione e retrieval;
- risparmio netto;
- latenza per stage;
- modalità e motivo della decisione;
- team, key prefix, modello e provider.

**Criteri di accettazione**

- Una richiesta può essere ricostruita senza salvare il contenuto dei messaggi.
- I costi interni non vengono attribuiti due volte.
- Metriche e spend record concordano sul costo finale.
- Retention e cardinalità sono limitate e configurabili.

### [ ] OBS-004 — Esporre metriche operative

**Priorità:** P1  
**Dimensione:** S  
**Dipendenze:** OBS-003

**Criteri di accettazione**

- Prometheus espone token saved, net USD saved, compression ratio, overhead,
  prefix drift e cache hit/miss.
- Le label non includono valori ad alta cardinalità come request ID o session ID.
- Gli header diagnostici sono presenti soltanto quando richiesti o configurati.
- Le API admin permettono aggregazione per giorno, modello, team e stage.

---

## Milestone 2 — Audit e simulazione

### [ ] SIM-001 — Costruire il block analyzer

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OBS-003

Normalizzare le tre API in blocchi logici senza perdere il raw payload:

- system/developer;
- user;
- assistant;
- tool call;
- tool result;
- RAG/document context;
- image o file reference.

**Criteri di accettazione**

- Tool call e relativo result hanno un legame stabile.
- Il parser riconosce stringhe e content part strutturate.
- Ogni blocco conserva path/indice per una riscrittura localizzata.
- Token count per blocco è disponibile senza serializzare nuovamente l'intera
  richiesta.

### [ ] SIM-002 — Rilevare waste signals deterministici

**Priorità:** P1  
**Dimensione:** M  
**Dipendenze:** SIM-001

Waste signals iniziali:

- array JSON grandi o ripetitivi;
- log ripetuti;
- ANSI/control sequence;
- HTML boilerplate;
- whitespace e righe duplicate;
- diff con contesto eccessivo;
- contenuto duplicato tra turni;
- prefisso volatile;
- tool schema ripetuti o molto grandi.

**Criteri di accettazione**

- Ogni segnale riporta token stimati recuperabili e confidence.
- Nessun segnale modifica il body.
- Il costo dell'analisi è misurato.
- Falsi positivi e casi limite hanno fixture dedicate.

### [ ] SIM-003 — Esporre simulate e audit mode

**Priorità:** P1  
**Dimensione:** M  
**Dipendenze:** SIM-002, OBS-004

**Criteri di accettazione**

- Endpoint autenticato che restituisce piano, blocchi e risparmio previsto senza
  chiamare il provider.
- Audit live inoltra la richiesta originale byte-for-byte.
- Il piano elenca trasformazioni, skip reason e stima di cache impact.
- Output privo del contenuto sensibile originale salvo richiesta admin esplicita.
- OpenAPI e documentazione aggiornate.

---

## Milestone 3 — Live-zone Content Optimizer

### [ ] CTX-001 — Definire l'interfaccia dei content transformer

**Priorità:** P0  
**Dimensione:** S  
**Dipendenze:** SIM-001

Ogni transformer deve implementare concettualmente:

```go
Detect(block) -> confidence
Plan(block, budget) -> candidate
Apply(block, candidate) -> transformedBlock
```

**Criteri di accettazione**

- Registry ordinato e configurabile.
- Un blocco viene trasformato da un solo handler.
- Limiti su input, output, CPU e tempo.
- Panic/error restituiscono il blocco originale.
- Ogni risultato include reason, token before/after e reversibility level.

### [ ] CTX-002 — Implementare il JSON transformer

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** CTX-001

Strategie iniziali:

- preservazione di chiavi e schema;
- conteggio totale;
- first/last item;
- errori, warning e valori anomali;
- preservazione di ID, hash, timestamp e numeri ad alta informazione;
- deduplicazione di oggetti equivalenti;
- statistiche per array numerici.

**Criteri di accettazione**

- JSON di output valido quando il formato richiede JSON.
- Nessun array piccolo viene trasformato.
- Chiavi discriminatorie configurabili.
- Fixture per array di oggetti, stringhe, numeri e mixed type.
- Golden test deterministici.

### [ ] CTX-003 — Implementare log e search transformer

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** CTX-001

**Criteri di accettazione**

- Preserva fatal, error, warning, stack trace e exit status.
- Raggruppa righe ripetute con contatore.
- Preserva head/tail entro budget.
- Search result raggruppati per file o sorgente.
- Non altera comandi, patch o codice presentati come output eseguibile.
- Fixture per test runner, compiler, Docker, Kubernetes e grep/rg.

### [ ] CTX-004 — Implementare transformer secondari

**Priorità:** P2  
**Dimensione:** L  
**Dipendenze:** CTX-001, CTX-002, CTX-003

- HTML extraction.
- CSV/TSV/Markdown table summary.
- Diff compaction.
- YAML/TOML/INI structural compaction.
- Plain-text deduplication conservativa.

**Criteri di accettazione**

- Ogni formato ha detector, soglia minima e golden test indipendenti.
- Plain text non usa riassunto LLM nella prima release.
- Il codice sorgente resta escluso per default.

### [ ] CTX-005 — Integrare il middleware nel request lifecycle

**Priorità:** P0  
**Dimensione:** L  
**Dipendenze:** CTX-002, CTX-003, OPT-003

Posizione iniziale:

```text
workflow -> HiveCache -> request log/audit -> Live Optimizer -> HiveState -> proxy
```

**Criteri di accettazione**

- Un HiveCache hit non paga il costo dell'optimizer.
- HiveCache continua a indicizzare la richiesta semantica originale.
- HiveState riceve il live delta già ottimizzato su cache miss.
- Vengono trasformati soltanto i blocchi candidati nella live zone.
- I content part non testuali restano byte-identici.
- Chat, Messages e Responses hanno parità di comportamento.
- Test completi per streaming e non-streaming.

### [ ] CTX-006 — Configurazione e rollout sicuro

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** CTX-005, SIM-003

**Criteri di accettazione**

- Default globale `off` o `audit` nella prima release.
- Override per tenant e chiave.
- Soglia minima token, budget output e lista transformer configurabili.
- Canary percentage e kill switch runtime.
- Header con trasformazioni applicate senza esporre dati sensibili.
- Error rate dell'optimizer separato dall'error rate upstream.

### [ ] CTX-007 — Benchmark del Live Optimizer

**Priorità:** P0  
**Dimensione:** L  
**Dipendenze:** CTX-006

Dataset minimi:

- Tau-Bench tool output;
- SWE-smith e sessioni coding;
- log di build/test sintetici e reali anonimizzati;
- JSON API e query DB;
- short chat come controllo negativo.

**Criteri di accettazione**

- Token reduction per content type.
- Task success/continuation rispetto al baseline.
- p50/p95/p99 di overhead.
- Prompt-cache hit rate prima e dopo.
- Risparmio USD netto per modello.
- Regressione automatica in CI su un sottoinsieme stabile.

**Gate di rilascio**

- Nessuna regressione statisticamente significativa sul task success.
- Errori di trasformazione sempre fail-open.
- Overhead p99 entro il budget concordato.
- Risparmio netto positivo sul workload target.

---

## Milestone 4 — Provider prompt-cache policy

### [ ] CACHE-001 — Definire l'identità di sessione

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** OPT-001

**Criteri di accettazione**

- Responses usa la chain `previous_response_id` quando disponibile.
- Chat e Messages supportano un header sessione esplicito.
- In assenza di identità affidabile, `auto` degrada a una policy conservativa.
- Session ID non viene usato come label Prometheus.
- Stato isolato per tenant, chiave, modello e provider.

### [ ] CACHE-002 — Implementare prefix fingerprint e drift detection

**Priorità:** P0  
**Dimensione:** M  
**Dipendenze:** CACHE-001, SIM-001

**Criteri di accettazione**

- Hash byte-level del prefisso cacheabile.
- Rilevamento di date, UUID, session token e altre zone volatili.
- Nessuna riscrittura automatica del prompt.
- Metriche stable prefix bytes/tokens, changed, age e warm/cold confidence.
- Nessun contenuto del prefisso viene salvato nei log per default.

### [ ] CACHE-003 — Implementare modalità `cache` e `token`

**Priorità:** P0  
**Dimensione:** L  
**Dipendenze:** CACHE-002, CTX-005

**Criteri di accettazione**

- `cache` trasforma soltanto il delta recente e non riscrive il frozen prefix.
- `token` può attivare HiveState secondo threshold e policy.
- Il motivo della decisione è osservabile.
- Il cambio modalità non mescola state cache o CCR tra policy incompatibili.
- Benchmark confronta costo reale, non solo token rimossi.

### [ ] CACHE-004 — Implementare modalità `auto`

**Priorità:** P1  
**Dimensione:** L  
**Dipendenze:** CACHE-003, OBS-003, CTX-007

La decisione deve confrontare:

- valore previsto del prompt-cache hit;
- token eliminabili;
- costo di HiveState e transformer;
- prezzo del modello;
- età e TTL/confidence della cache;
- latenza aggiuntiva;
- rischio configurato dal tenant.

**Criteri di accettazione**

- Formula e input della decisione sono documentati.
- Decision reason e stime sono registrati.
- Nessuna ricompattazione completa con cache warm incerta.
- Fallback conservativo quando prezzi, TTL o sessione sono sconosciuti.
- A/B test contro policy fisse `cache` e `token`.

---

## Milestone 5 — CCR v2

### [ ] CCR-001 — Definire Context Object e storage contract

**Priorità:** P1  
**Dimensione:** L  
**Dipendenze:** OPT-003, CACHE-001

Campi minimi:

- ID opaco;
- tenant, key e session scope;
- content type e tool name;
- hash dell'originale;
- contenuto originale o riferimento cifrato;
- summary/marker;
- token count;
- TTL e timestamps;
- retrieval count;
- source request/turn senza contenuto sensibile nei log.

**Criteri di accettazione**

- Backend SQLite/Postgres iniziali.
- Quota per tenant/sessione e eviction deterministica.
- Nessun accesso cross-tenant o cross-session.
- Retention opt-in e cancellazione esplicita.
- Nessun contenuto originale nelle metriche.

### [ ] CCR-002 — Salvare gli originali dei blocchi compressi

**Priorità:** P1  
**Dimensione:** M  
**Dipendenze:** CCR-001, CTX-005

**Criteri di accettazione**

- Ogni trasformazione lossy salva l'originale prima del rewrite.
- Fallimento dello store impedisce la trasformazione lossy.
- Marker include solo ID opaco, conteggio e istruzione breve.
- Trasformazioni realmente lossless non richiedono Context Object.
- Deduplicazione per hash entro lo stesso scope.

### [ ] CCR-003 — Retrieval proattivo budget-gated

**Priorità:** P1  
**Dimensione:** L  
**Dipendenze:** CCR-002, TODO HiveCache context relevance

**Criteri di accettazione**

- Retrieval basato su query, tool name, path/ID e relevance.
- Budget massimo assoluto e percentuale sul prompt originale.
- I blocchi recuperati mantengono ordine e provenance.
- Nessun retrieval se annulla il risparmio previsto.
- Metriche hit, miss, token reiniettati e valore netto.

### [ ] CCR-004 — Endpoint e tool di retrieval esplicito

**Priorità:** P2  
**Dimensione:** L  
**Dipendenze:** CCR-002

**Criteri di accettazione**

- Endpoint tenant-scoped per recuperare un Context Object.
- Tool schema OpenAI/Anthropic/Responses equivalente.
- Il client può gestire il tool call senza logica proprietaria non documentata.
- Tool name collision rilevata prima dell'iniezione.
- ACL, rate limit e audit log dedicati.

### [ ] CCR-005 — Retrieval trasparente non-streaming

**Priorità:** P3  
**Dimensione:** XL  
**Dipendenze:** CCR-004

Feature separata e opt-in: il gateway intercetta il proprio tool call, recupera
l'originale e continua la richiesta fino alla risposta finale.

**Criteri di accettazione**

- Solo richieste buffered/non-streaming nella prima versione.
- Limite rigido al numero di round trip interni.
- Costi e token di ogni round trip contabilizzati.
- Tool call applicativi misti non vengono intercettati o persi.
- Timeout/cancel del client cancella l'intera catena.
- Native passthrough disabilitato soltanto con consenso esplicito.

---

## Milestone 6 — Output effort shaping

### [ ] EFF-001 — Classificare le continuazioni di routine senza LLM

**Priorità:** P2  
**Dimensione:** M  
**Dipendenze:** SIM-001, OBS-003

Segnali iniziali:

- tool result riuscito;
- test/build riuscito;
- semplice file read;
- nessuna nuova domanda umana;
- assenza di errori, warning o richiesta distruttiva.

**Criteri di accettazione**

- Il classificatore è deterministico e spiegabile.
- Errori, domande utente e ambiguità forzano passthrough.
- Il classificatore non aumenta mai l'effort richiesto dal client.
- Audit mode misura quante richieste sarebbero state modificate.

### [ ] EFF-002 — Applicare clamp provider-specific

**Priorità:** P2  
**Dimensione:** M  
**Dipendenze:** EFF-001

**Criteri di accettazione**

- OpenAI/Responses usa `reasoning_effort` supportato dal modello.
- Anthropic usa effort o thinking budget appropriato alla versione.
- Provider non supportati restano passthrough.
- HiveRoute e client override hanno precedenza definita.
- Nessun parametro sconosciuto viene inviato upstream.

### [ ] EFF-003 — Introdurre holdout e misurazione qualità

**Priorità:** P2  
**Dimensione:** M  
**Dipendenze:** EFF-002

**Criteri di accettazione**

- Holdout stabile per sessione e configurabile per tenant.
- Confronto output token, costo, latenza, errori e task continuation.
- Risparmio marcato `measured` solo con gruppo di controllo valido.
- Kill switch automatico/manuale in caso di regressione.

---

## Milestone 7 — Learning adattivo, differito

### [ ] LEARN-001 — Raccogliere segnali di utilità dei campi

**Priorità:** P3  
**Dimensione:** L  
**Dipendenze:** CCR-003, CCR-004

Misurare per tenant e tool type quali campi o elementi vengono successivamente
recuperati, citati o richiesti.

**Criteri di accettazione**

- Nessuna condivisione dei pattern tra tenant.
- Cold start usa soltanto euristiche statiche.
- Confidence minima prima di influenzare un transformer.
- Possibilità di azzerare i dati appresi.
- A/B test dimostra miglioramento rispetto alle euristiche statiche.

### [ ] LEARN-002 — Usare i pattern nel JSON transformer

**Priorità:** P3  
**Dimensione:** M  
**Dipendenze:** LEARN-001, CTX-002

**Criteri di accettazione**

- I pattern appresi modificano solo ranking e keep decision.
- Chiavi protette statiche hanno sempre precedenza.
- Bassa confidence produce comportamento identico al baseline.
- Decisione e confidence sono osservabili.

---

## Ordine di consegna consigliato

### Release A — Misurare correttamente

- [ ] OPT-001
- [ ] OPT-002
- [ ] OPT-003
- [ ] OBS-001
- [ ] OBS-002
- [ ] OBS-003
- [ ] OBS-004
- [ ] SIM-001
- [ ] SIM-002
- [ ] SIM-003

**Uscita:** audit/simulate e accounting cache-aware, nessuna trasformazione
abilitata per default.

### Release B — Ridurre il live delta

- [ ] CTX-001
- [ ] CTX-002
- [ ] CTX-003
- [ ] CTX-005
- [ ] CTX-006
- [ ] CTX-007

**Uscita:** JSON/log optimization opt-in, canary e fail-open.

### Release C — Ottimizzare cache contro token

- [ ] CACHE-001
- [ ] CACHE-002
- [ ] CACHE-003
- [ ] CACHE-004

**Uscita:** modalità `cache`, `token` e successivamente `auto`.

### Release D — Rendere la compressione reversibile

- [ ] CCR-001
- [ ] CCR-002
- [ ] CCR-003
- [ ] CCR-004

**Uscita:** Context Object e retrieval esplicito/proattivo. `CCR-005` resta una
feature successiva e separata.

### Release E — Ridurre gli output token

- [ ] EFF-001
- [ ] EFF-002
- [ ] EFF-003

**Uscita:** effort shaping opt-in con holdout misurabile.

## Definition of Done globale

- [ ] Configurazione documentata e inclusa nell'esempio YAML.
- [ ] OpenAPI aggiornata per endpoint e header pubblici.
- [ ] Test unitari, middleware, provider e integrazione verdi.
- [ ] Race detector verde sui nuovi store/cache in memoria.
- [ ] Fixture multi-tenant dimostra assenza di leakage.
- [ ] Streaming e cancellation verificati.
- [ ] Benchmark riproducibile incluso nel repository.
- [ ] Metriche distinguono saving lordo, costo interno e saving netto.
- [ ] Rollback/kill switch documentato.
- [ ] README descrive soltanto feature effettivamente attive nel percorso HTTP.

