# Ubiquum Gateway: guida al flusso di esecuzione

Questa guida spiega `ubiquum-ai-gateway` seguendo il codice sorgente: avvio,
pipeline HTTP, autenticazione, semantic cache, HiveState, HiveRoute, guardrail,
routing verso i provider, streaming e persistenza.

Il modello mentale da tenere presente è:

> Il gateway riceve un protocollo API pubblico, applica policy e
> ottimizzazioni, trasforma la richiesta in un formato interno comune, sceglie
> modello, deployment e provider, chiama l'upstream e registra utilizzo e costi.

## 1. Avvio del gateway

Il processo parte da [`cmd/gateway/main.go`](../cmd/gateway/main.go).

Con `ubiquum serve` viene eseguito `runServer()`:

1. Carica il file YAML tramite
   [`internal/config/config.go`](../internal/config/config.go).
2. Risolve provider, modelli, valori di default e variabili `${ENV}`.
3. Costruisce il provider registry.
4. Apre SQLite o PostgreSQL ed esegue le migration.
5. Inizializza pricing, router, autenticazione, webhook e spend writer.
6. Inizializza opzionalmente semantic cache, request log e HiveState.
7. Registra le rotte HTTP.
8. Avvia il server.

La parte principale di dependency wiring è in
[`cmd/gateway/main.go`](../cmd/gateway/main.go). I provider concreti vengono
creati dallo switch in
[`cmd/gateway/factory.go`](../cmd/gateway/factory.go): OpenAI, Azure,
Anthropic, Vertex, Bedrock, Google AI, Codex e gli altri provider supportati.

Il lifecycle del server HTTP è in
[`internal/server/server.go`](../internal/server/server.go). Qui vengono
configurati porta, timeout, `http.ServeMux` e graceful shutdown.

## 2. Flusso completo di una richiesta

Per una richiesta `POST /v1/chat/completions`, il percorso nominale è:

```text
Client
  │
  ▼
http.ServeMux
  │
  ▼
Recovery
Request ID
Logging e metriche
Limite dimensione body
Autenticazione e budget
Upstream-token forwarding
  │
  ▼
Semantic Cache, se abilitata
Request logging, se abilitato
HiveState e HiveRoute, se abilitati
Feedback
Rate limit per chiave
Limite richieste concorrenti
Guardrail
Pre-request hook
Post-request hook
  │
  ▼
CompletionHandler
  │
  ▼
Router dei deployment
  │
  ▼
Provider concreto
  │
  ▼
OpenAI / Anthropic / Azure / Vertex / ...
  │
  ▼
Risposta, spend, metriche e cache store
```

La pipeline reale viene costruita in
[`internal/server/routes.go`](../internal/server/routes.go).

`middleware.Chain()` applica il primo middleware come wrapper più esterno:
[`internal/middleware/chain.go`](../internal/middleware/chain.go). L'ordine
nell'array è quindi l'ordine di ingresso. Al ritorno della risposta i wrapper si
chiudono in ordine inverso.

Gli `extraMiddlewares` vengono aggiunti in questo ordine da `runServer()`:

1. cache, se abilitata;
2. request logging, se abilitato;
3. HiveState, se abilitato.

Successivamente vengono aggiunti feedback, rate limit, semaphore, guardrail e
hook.

## 3. Autenticazione e request context

L'autenticazione è implementata in
[`internal/auth/auth.go`](../internal/auth/auth.go).

Supporta:

- master key;
- virtual key salvata nel database;
- pass-through mode;
- header `Authorization: Bearer ...`;
- fallback `X-Api-Key` per client Anthropic.

Per una virtual key controlla:

- esistenza;
- flag `active`;
- scadenza;
- budget della chiave;
- esistenza del team;
- budget del team.

Dopo la validazione inserisce nel `context.Context`:

- chiave raw;
- record `APIKey`;
- record del team;
- eventuale token OAuth da inoltrare al provider.

I middleware successivi recuperano questi valori tramite
`KeyInfoFromContext()`, `TeamFromContext()` e le funzioni per i token upstream.

L'ACL sui modelli non viene verificata dal middleware di autenticazione. Viene
verificata successivamente negli handler con `auth.IsModelAllowed()`, per
esempio in
[`internal/proxy/handler.go`](../internal/proxy/handler.go).

## 4. Semantic Cache: HiveCache

L'inizializzazione avviene in
[`cmd/gateway/main.go`](../cmd/gateway/main.go).

I backend supportati sono:

- memory;
- Redis;
- Qdrant;
- pgvector.

L'interfaccia comune è
[`internal/cache/store.go`](../internal/cache/store.go). Le implementazioni
sono:

- [`store_memory.go`](../internal/cache/store_memory.go);
- [`store_redis.go`](../internal/cache/store_redis.go);
- [`store_qdrant.go`](../internal/cache/store_qdrant.go);
- [`store_pgvector.go`](../internal/cache/store_pgvector.go).

### 4.1 Cache lookup

Il middleware è
[`internal/cache/middleware.go`](../internal/cache/middleware.go).

Attualmente elabora soltanto:

- `/v1/chat/completions`;
- `/v1/messages`.

Il percorso è:

1. Legge e ripristina il body HTTP.
2. Estrae modello, messaggi e modalità streaming.
3. Esclude le richieste agentiche che contengono tool.
4. Determina `teamID` e modello usati come scope della cache.
5. Estrae l'ultima domanda user e il contesto precedente.
6. Calcola gli embedding.
7. Cerca i cinque candidati più vicini.
8. Calcola il punteggio combinato query + contesto.
9. Classifica il risultato.

La logica principale è in
[`internal/cache/cache.go`](../internal/cache/cache.go), mentre la cosine
similarity e il dual score sono in
[`internal/cache/similarity.go`](../internal/cache/similarity.go).

Le bande sono:

- `DIRECT`: risposta restituita direttamente;
- `REUSE`: risposta riutilizzata;
- `TWEAK`: risposta cached adattata da un modello economico;
- `MISS`: richiesta inoltrata al resto della pipeline.

Nell'implementazione attuale `DIRECT` e `REUSE` seguono lo stesso ramo e
restituiscono la risposta cached senza una validazione aggiuntiva.

### 4.2 Tweak

Se il risultato cade nella banda `TWEAK`, il middleware confronta il costo
stimato della chiamata originale con quello del tweak model.

Se il tweak è conveniente:

1. estrae la nuova query;
2. chiama il modello configurato;
3. sostituisce il contenuto della risposta cached;
4. restituisce la risposta adattata;
5. salva la nuova risposta per i lookup futuri.

La costruzione della richiesta al tweak model è in
[`internal/cache/tweak.go`](../internal/cache/tweak.go).

### 4.3 Cache miss e salvataggio

In caso di `MISS`, il cache middleware avvolge il `ResponseWriter`, lascia
proseguire la richiesta e cattura la risposta prodotta dai componenti
successivi.

Se la risposta ha status `2xx`, viene salvata asincronamente:

- le risposte non-streaming OpenAI vengono salvate;
- lo stream Anthropic viene ricostruito come JSON e può essere salvato;
- lo stream OpenAI non viene attualmente salvato.

L'header `x-ubiquum-cache: false` salta il lookup ma non disabilita il salvataggio
della nuova risposta.

## 5. HiveState

HiveState non mantiene la conversazione completa come una sessione server-side.
Riceve dal client l'intera conversazione, comprime la parte vecchia e riscrive
il body prima della chiamata principale.

Il middleware è
[`internal/hivestate/middleware.go`](../internal/hivestate/middleware.go).

Supporta:

- Chat Completions;
- Anthropic Messages;
- Responses API.

### 5.1 Divisione dei messaggi

I messaggi vengono divisi in quattro zone:

```text
System      istruzioni protette
History     conversazione vecchia comprimibile
Recent      ultimi step preservati
Last        ultima richiesta user preservata
```

La divisione è in
[`internal/hivestate/safety.go`](../internal/hivestate/safety.go).

### 5.2 Elaborazione

Il cuore del motore è
[`internal/hivestate/hivestate.go`](../internal/hivestate/hivestate.go).

HiveState:

1. rileva il profilo della conversazione;
2. calcola le zone System, History, Recent e Last;
3. conta i token;
4. se è sotto `threshold`, fa pass-through;
5. verifica che esista spazio sufficiente per ottenere un risparmio;
6. cerca uno state già estratto nella cache locale;
7. prova eventualmente un'estrazione incrementale;
8. altrimenti chiama il modello di estrazione sulla history preprocessata;
9. produce uno state JSON;
10. ricostruisce il body con state + messaggi recenti;
11. annulla la compressione se il risultato è più grande dell'originale.

Lo state contiene principalmente:

- `intent`;
- `difficulty`;
- `reasoning_effort`;
- `active_constraints`;
- stato di avanzamento;
- azioni effettuate;
- errori;
- `conversation_status`.

Il modello di estrazione viene chiamato da
[`internal/hivestate/state.go`](../internal/hivestate/state.go), usando il
prompt definito in
[`internal/hivestate/prompt.go`](../internal/hivestate/prompt.go).

Altri componenti importanti sono:

- [`preprocess.go`](../internal/hivestate/preprocess.go): comprime tool output,
  JSON e contenuti molto lunghi;
- [`identifiers.go`](../internal/hivestate/identifiers.go): costruisce il Code
  Registry di file, funzioni, classi e identificatori;
- [`ccr.go`](../internal/hivestate/ccr.go): conserva e recupera il working set;
- [`cache_align.go`](../internal/hivestate/cache_align.go): stabilizza il
  prefisso dei system prompt per migliorare il prefix cache dei provider.

### 5.3 Riscrittura del body

Il middleware mantiene raw i messaggi recenti per non perdere strutture come:

- `tool_calls`;
- `tool_result`;
- function call;
- input multimodale.

Esistono riscrittori distinti per:

- OpenAI Chat Completions;
- Anthropic Messages;
- Responses API.

Sono tutti in
[`internal/hivestate/middleware.go`](../internal/hivestate/middleware.go).

### 5.4 Dove vive lo state

La cache dello state è locale al processo:

- massimo 128 entry;
- TTL 10 minuti;
- chiave basata sull'hash della History.

L'implementazione è in
[`internal/hivestate/state_cache.go`](../internal/hivestate/state_cache.go).

Il database salva le metriche HiveState, non lo state conversazionale. State
cache e CCR si perdono al riavvio e non vengono condivisi automaticamente tra
pod.

## 6. HiveRoute

HiveRoute è integrato nel middleware HiveState:
[`internal/hivestate/middleware.go`](../internal/hivestate/middleware.go).

Il modello di estrazione può restituire, per esempio:

```json
{
  "difficulty": "trivial",
  "reasoning_effort": "low"
}
```

HiveRoute associa il livello `trivial` a un modello configurato e modifica il
campo `model` del body prima che questo arrivi al proxy handler.

La configurazione effettiva segue questa priorità:

```text
per-key > per-team > configurazione YAML globale
```

La fusione della configurazione è in
[`internal/hivestate/hiveroute.go`](../internal/hivestate/hiveroute.go).

HiveRoute può inoltre iniettare:

- `reasoning_effort` per OpenAI-compatible;
- `thinking.budget_tokens` per Anthropic.

Per `/v1/responses` il reasoning effort non viene ancora inoltrato.

Prima di instradare verso un modello `restricted`, HiveRoute controlla che sia
disponibile il token OAuth upstream per il provider target.

### 6.1 HiveRoute non è il Router

Sono due componenti differenti:

| Componente | Cosa sceglie |
| --- | --- |
| HiveRoute | Un modello diverso in base alla difficoltà |
| `router.Router` | Un deployment del modello già scelto |

Esempio:

```text
Modello richiesto: general-assistant
HiveRoute: task trivial -> fast-model
Router: fast-model ha tre deployment -> sceglie fast-model-2
```

Le impostazioni per team e chiave possono essere lette e modificate tramite le
rotte admin implementate in
[`internal/admin/route_settings.go`](../internal/admin/route_settings.go).

## 7. Router dei deployment

Il registry raggruppa le configurazioni che espongono lo stesso nome modello:
[`internal/provider/registry.go`](../internal/provider/registry.go).

Il router è in [`internal/router/router.go`](../internal/router/router.go).

Le strategie supportate sono:

- `shuffle`;
- `round-robin`;
- `latency`, basata su una media mobile esponenziale della latenza.

Per ogni richiesta:

1. recupera tutti i deployment del modello;
2. li ordina secondo la strategia;
3. controlla il circuit breaker del deployment;
4. invoca la callback fornita dall'handler;
5. registra successo o fallimento;
6. se l'errore è retryable applica exponential backoff;
7. prova un altro deployment.

`retries: 3` significa al massimo quattro tentativi totali: il primo tentativo
più tre retry.

Il circuit breaker è per deployment, non per modello.

## 8. Proxy handler e provider

L'handler OpenAI Chat è
[`internal/proxy/handler.go`](../internal/proxy/handler.go).

Il percorso è:

1. decode del body;
2. validazione di `model` e `messages`;
3. controllo ACL del modello;
4. invocazione del router;
5. sostituzione del nome pubblico con `ProviderModel`;
6. rimozione dei parametri incompatibili indicati da `drop_params`;
7. chiamata a `Provider.Complete()` o `Provider.Stream()`.

L'interfaccia comune dei provider è
[`internal/provider/provider.go`](../internal/provider/provider.go).

Gli altri protocolli vengono tradotti nel formato interno OpenAI:

- Anthropic Messages:
  [`internal/proxy/anthropic.go`](../internal/proxy/anthropic.go);
- Responses API:
  [`internal/proxy/responses.go`](../internal/proxy/responses.go);
- legacy completions:
  [`internal/proxy/completions.go`](../internal/proxy/completions.go);
- embeddings:
  [`internal/proxy/embeddings.go`](../internal/proxy/embeddings.go).

Il provider concreto costruisce infine la richiesta HTTP upstream. Per esempio,
l'adapter OpenAI-compatible è in
[`internal/provider/openai/openai.go`](../internal/provider/openai/openai.go).

### 8.1 Streaming

Per lo streaming:

1. l'handler apre lo stream sul provider scelto;
2. riceve chunk dal `provider.StreamReader`;
3. traduce eventualmente il protocollo;
4. scrive l'evento SSE al client;
5. esegue `Flush()` dopo ogni chunk;
6. raccoglie usage e lunghezza della completion;
7. al termine registra token e costo.

Anthropic e Responses API hanno relay dedicati che traducono i chunk OpenAI nel
rispettivo protocollo pubblico.

## 9. Guardrail

L'engine viene costruito durante la registrazione delle rotte in
[`internal/server/routes.go`](../internal/server/routes.go).

Gli scanner disponibili sono:

- PII: [`internal/guardrail/pii.go`](../internal/guardrail/pii.go);
- secret e API key:
  [`internal/guardrail/secrets.go`](../internal/guardrail/secrets.go);
- prompt injection:
  [`internal/guardrail/injection.go`](../internal/guardrail/injection.go);
- classificazione tramite LLM:
  [`internal/guardrail/moderation.go`](../internal/guardrail/moderation.go).

Il middleware HTTP è
[`internal/middleware/guardrail.go`](../internal/middleware/guardrail.go).

Il flusso è:

1. legge il body che ha eventualmente già riscritto HiveState;
2. estrae il testo dei messaggi;
3. applica la configurazione globale;
4. applica l'override dell'header `x-ubiquum-guard` o `x-guardrails`;
5. applica i filtri configurati per il team;
6. usa l'engine inline oppure il servizio HTTP legacy;
7. restituisce `403` se il contenuto viene bloccato;
8. applica `fail_open` o fail-closed in caso di errore.

L'engine inline esegue gli scanner in parallelo:
[`internal/guardrail/guardrail.go`](../internal/guardrail/guardrail.go).

La moderazione LLM viene contabilizzata come provider virtuale `@hiveguard`.

Se l'engine inline è attivo, viene esposto anche l'endpoint compatibile con il
vecchio servizio:

```text
POST /analyze/batch/beta/litellm_basic_guardrail_api
```

Il relativo handler è
[`internal/guardrail/handler.go`](../internal/guardrail/handler.go).

## 10. Ritorno della risposta e spend

Dopo la chiamata al provider:

1. l'handler raccoglie token e usage;
2. calcola il costo;
3. inserisce i dati nel `UsageCapture` usato da HiveState;
4. registra lo spend;
5. converte la risposta nel protocollo richiesto;
6. HiveState registra ratio, route e risparmio;
7. la cache può salvare la risposta;
8. metriche e logging esterni chiudono la richiesta.

`UsageCapture` è definito in
[`internal/store/usage_capture.go`](../internal/store/usage_capture.go).

Lo spend writer è in
[`internal/spend/batch.go`](../internal/spend/batch.go). Per chiavi con budget o
appartenenti a un team la scrittura viene effettuata subito, per mantenere i
controlli di ammissione vicini allo spend reale. Negli altri casi i record
vengono bufferizzati.

La persistenza GORM è in
[`internal/store/open.go`](../internal/store/open.go). `LogSpend()` inserisce il
record e incrementa atomicamente lo spend della chiave e del team.

## 11. Matrice degli endpoint principali

| Endpoint | Cache | HiveState/HiveRoute | Guardrail effettivo | Handler |
| --- | --- | --- | --- | --- |
| `/v1/chat/completions` | Sì | Sì | Sì, su cache miss | `CompletionHandler` |
| `/v1/messages` | Sì | Sì | Sì, su cache miss | `AnthropicHandler` |
| `/v1/responses` | No | Sì | No, con il parser attuale | `ResponsesHandler` |
| `/v1/completions` | No | No | No, con il parser attuale | `CompletionsHandler` |
| `/v1/embeddings` | No | No | No | `EmbeddingsHandler` |
| `/v1/images/generations` | No | No | No | media handler |
| `/v1/audio/*` | No | No | No | media handler |

## 12. Persistenze da non confondere

| Stato | Dove vive |
| --- | --- |
| Chiavi, team, budget, spend e settings | SQLite/PostgreSQL |
| Semantic cache | Memory/Redis/Qdrant/pgvector |
| Metriche cache e HiveState | SQLite/PostgreSQL |
| State extraction cache | RAM del processo |
| CCR | RAM del processo |
| Circuit breaker e latenze router | RAM del processo |
| Rate limiter e metriche Prometheus | RAM del processo |

I modelli GORM per metriche e impostazioni sono in
[`internal/store/models.go`](../internal/store/models.go), mentre l'interfaccia
di persistenza è in
[`internal/store/store.go`](../internal/store/store.go).

## 13. Esempio end-to-end

Immaginiamo una richiesta verso il modello pubblico `general-assistant`:

1. Auth valida la virtual key e carica il team.
2. La cache cerca una risposta usando team e `general-assistant`.
3. La cache non trova nulla.
4. HiveState comprime la history e classifica il task come `trivial`.
5. HiveRoute sostituisce il modello con `fast-model`.
6. Rate limit e semaphore ammettono la richiesta.
7. Il guardrail analizza il body riscritto.
8. `CompletionHandler` decodifica il body con `model: fast-model`.
9. L'ACL verifica che la chiave possa usare `fast-model`.
10. Il router sceglie uno dei deployment di `fast-model`.
11. Il provider sostituisce il nome con il `ProviderModel` reale.
12. La richiesta viene inviata all'upstream.
13. L'handler registra token e costo.
14. HiveState registra compressione e scelta HiveRoute.
15. La semantic cache salva la risposta sotto lo scope originale
    `general-assistant`.

## 14. Attenzioni sul comportamento attuale

Questi punti derivano dal codice attuale e sono importanti prima di fare
modifiche.

### 14.1 Cache hit e livelli saltati

La semantic cache è prima di feedback, rate limit, semaphore, guardrail, hook e
proxy handler. Un cache hit termina la richiesta e non esegue questi livelli.

Poiché anche l'ACL del modello viene verificata nel proxy handler, un cache hit
avviene prima del controllo ACL normalmente effettuato dall'handler.

### 14.2 Guardrail dopo cache e HiveState

Cache ed embedding vengono eseguiti prima dei guardrail. HiveState può inoltre
inviare la history al modello di estrazione prima che il guardrail controlli il
contenuto.

Quando il guardrail viene infine eseguito, legge il body già compresso o
riscritto da HiveState.

### 14.3 Cache e HiveRoute

La cache esegue il lookup prima di HiveRoute e usa il modello originale come
scope. Su `MISS`, però, la risposta catturata può essere stata generata da un
modello scelto da HiveRoute.

Al successivo cache hit la risposta viene restituita senza rivalutare
difficulty, route e guardrail.

### 14.4 DIRECT e REUSE

`DIRECT` e `REUSE` vengono gestiti dallo stesso ramo. La light validation
descritta nei commenti per `REUSE` non è implementata.

### 14.5 Responses API e guardrail

Il cache middleware ignora `/v1/responses`.

Il guardrail viene inserito nella pipeline, ma il suo parser cerca il campo
`messages`, mentre la Responses API usa `input`. Con il codice attuale non
estrae quindi messaggi da `/v1/responses`.

Lo stesso problema rende di fatto inefficace il guardrail applicato alla legacy
`/v1/completions`, che usa `prompt`.

### 14.6 Solo input guardrail

Il guardrail controlla il body della richiesta. Non esiste in questa pipeline
uno scanner della risposta generata dal provider.

### 14.7 Eviction della semantic cache

`Cache.Evict()` e le implementazioni TTL/max entries esistono, ma non risultano
invocate o schedulate nel normale runtime del gateway. Di conseguenza bisogna
verificare il comportamento atteso di `ttl` e `max_entries` prima di farvi
affidamento.

### 14.8 Identificatore degli override per-key

Il middleware HiveState cerca `KeyRouteSettings` usando `ki.KeyHash`.
L'admin API, il modello GORM e altri componenti usano invece il concetto di
`KeyID`.

Questa differenza va verificata prima di modificare o usare gli override
HiveRoute per singola chiave.

### 14.9 Documentazione non completamente aggiornata

[`docs/architecture.md`](architecture.md) rimane una buona introduzione, ma non
descrive interamente cache, HiveState, HiveRoute, feedback e guardrail
nell'ordine attuale.

Per il flusso runtime, la fonte di verità deve rimanere:

1. `cmd/gateway/main.go`;
2. `internal/server/routes.go`;
3. il middleware interessato;
4. il relativo proxy handler;
5. router e provider.

## 15. Ordine consigliato per iniziare a lavorare sul progetto

Per uno junior, l'ordine di lettura consigliato è:

1. [`internal/config/types.go`](../internal/config/types.go);
2. [`cmd/gateway/main.go`](../cmd/gateway/main.go);
3. [`internal/server/routes.go`](../internal/server/routes.go);
4. [`internal/middleware/chain.go`](../internal/middleware/chain.go);
5. [`internal/auth/auth.go`](../internal/auth/auth.go);
6. [`internal/proxy/handler.go`](../internal/proxy/handler.go);
7. [`internal/provider/provider.go`](../internal/provider/provider.go);
8. [`internal/provider/registry.go`](../internal/provider/registry.go);
9. [`internal/router/router.go`](../internal/router/router.go);
10. [`internal/cache/middleware.go`](../internal/cache/middleware.go);
11. [`internal/hivestate/middleware.go`](../internal/hivestate/middleware.go);
12. [`internal/guardrail/guardrail.go`](../internal/guardrail/guardrail.go).

Con questo percorso si vede prima lo scheletro del gateway e solo dopo si entra
nei componenti che intercettano e modificano il flusso.
