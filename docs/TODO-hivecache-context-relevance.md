# TODO: HiveCache context relevance selection

## Obiettivo

Rendere HiveCache capace di dividere la history conversazionale in segmenti,
valutare la rilevanza di ogni segmento rispetto all'ultima query utente e usare
soltanto il contesto pertinente per calcolare il context score della semantic
cache.

La funzionalità desiderata è:

```text
Ultima query utente
        │
        ▼
Divisione della history in chunk o turni
        │
        ▼
Relevance score tra query e ogni chunk
        │
        ▼
Selezione dei chunk pertinenti
        │
        ▼
Ripristino dell'ordine cronologico dei chunk selezionati
        │
        ▼
Context embedding del solo contesto rilevante
        │
        ▼
Dual score query + relevant context
```

## Comportamento attuale

Il codice corrente:

1. usa l'ultimo messaggio `user` come query;
2. concatena tutta la history precedente in un unico testo;
3. mantiene soltanto gli ultimi 2000 caratteri;
4. genera un solo embedding per l'intero contesto;
5. combina query similarity e whole-context similarity.

Il calcolo attuale è:

```text
score =
    alpha * similarity(currentQuery, cachedQuery)
  + beta  * similarity(currentWholeContext, cachedWholeContext)
```

Non vengono attualmente eseguiti:

- chunking della history;
- relevance scoring dei singoli segmenti;
- selezione top-k;
- soglia minima di pertinenza;
- riordino o promozione dei segmenti rilevanti.

File di riferimento:

- [`internal/cache/digest.go`](../internal/cache/digest.go);
- [`internal/cache/cache.go`](../internal/cache/cache.go);
- [`internal/cache/similarity.go`](../internal/cache/similarity.go);
- [`internal/cache/embed.go`](../internal/cache/embed.go).

## Prima proposta di implementazione

Mantenere invariato lo schema `cache.Entry`, continuando a salvare un singolo
`CtxEmbedding`.

Per ogni richiesta:

1. estrarre l'ultima query utente;
2. dividere la history in chunk atomici;
3. calcolare la rilevanza di ogni chunk rispetto alla query;
4. selezionare i migliori chunk sopra una soglia;
5. applicare un limite complessivo di token o caratteri;
6. rimettere i chunk selezionati nell'ordine cronologico originale;
7. unirli in `relevantContext`;
8. generare il `CtxEmbedding` da `relevantContext`;
9. usare il `DualScore` esistente.

Pseudocodice:

```text
queryEmbedding = embed(lastUserQuery)

for chunk in splitContext(history):
    chunkEmbedding = embed(chunk.text)
    chunk.relevance = cosine(queryEmbedding, chunkEmbedding)

selected = chunks
    .filter(relevance >= threshold)
    .topK(by relevance)
    .sort(by original position)

relevantContext = join(selected)
contextEmbedding = embed(relevantContext)

score =
    alpha * querySimilarity
  + beta  * relevantContextSimilarity
```

Questo approccio evita, in una prima versione, di modificare:

- `cache.Entry`;
- memory store;
- Redis store;
- Qdrant store;
- pgvector store.

## Decisioni ancora da prendere

### Unità di chunking

Valutare una delle seguenti opzioni:

- singolo messaggio;
- coppia user/assistant;
- turno completo con eventuali tool call e tool result;
- chunk semantici con dimensione massima.

Per le conversazioni agentiche, tool call e relativo tool result devono restare
atomici.

### Strategia di relevance scoring

Possibili alternative:

- embedding di ogni chunk;
- scoring lessicale economico seguito da embedding dei soli candidati;
- algoritmo ibrido;
- riuso di embedding già calcolati.

### Costo degli embedding

L'implementazione ingenua richiede un embedding per ogni chunk. Prima di
procedere bisogna valutare:

- supporto batch nell'`EmbedFunc`;
- cache degli embedding dei chunk;
- numero massimo di chunk analizzati;
- early filtering lessicale;
- latenza e costo massimo accettabili.

### Selezione

Definire configurazione e default per:

- `context_top_k`;
- `context_relevance_threshold`;
- `context_max_tokens`;
- eventuale peso della recency;
- fallback quando nessun chunk supera la soglia.

### Messaggi protetti

Chiarire quali elementi devono sempre entrare nel contesto, indipendentemente
dallo score:

- system e developer prompt;
- pinned memory;
- constraint espliciti;
- ultimi turni;
- tool result ancora attivi.

## File probabilmente coinvolti

- `internal/config/types.go`
  - nuovi parametri di configurazione e default;
- `internal/cache/digest.go`
  - chunking e costruzione del relevant context;
- `internal/cache/embed.go`
  - eventuale supporto agli embedding batch;
- `internal/cache/cache.go`
  - selezione del contesto in `Lookup()` e `Store()`;
- `internal/cache/middleware.go`
  - metriche e header diagnostici;
- `internal/cache/cache_test.go`
  - test del flusso principale;
- `internal/cache/core_behavior_test.go`
  - test di selezione, fallback e compatibilità.

## Criteri di completamento

- La history viene divisa in segmenti deterministici.
- Ogni segmento riceve una relevance rispetto all'ultima query.
- Solo i segmenti rilevanti contribuiscono al `CtxEmbedding`.
- I segmenti selezionati vengono ricomposti nell'ordine originale.
- Tool call e tool result non vengono separati.
- `Lookup()` e `Store()` usano la stessa identica strategia.
- Il comportamento rimane isolato per team e modello.
- Esiste un fallback sicuro quando nessun segmento è rilevante.
- Costo embedding e latenza sono misurati.
- I backend esistenti rimangono compatibili.
- I test coprono conversazioni normali, agentiche e multi-turn.

## Metriche utili

Valutare l'aggiunta di:

- numero di chunk analizzati;
- numero di chunk selezionati;
- relevance minima, massima e media;
- token del contesto originale;
- token del contesto selezionato;
- latenza della selezione;
- token e costo embedding aggiuntivi;
- variazione di hit rate e false-positive rate.

## Nota per documentazione e presentazioni

Finché questo TODO non viene implementato, la descrizione corretta del
comportamento corrente è:

> HiveCache separa l'ultima query dalla history, recupera i candidati sulla
> query e li rivaluta combinando similarità della query e similarità del
> contesto complessivo.

Dopo l'implementazione si potrà invece affermare:

> HiveCache segmenta la history, seleziona le parti pertinenti rispetto
> all'ultima query e usa soltanto il contesto rilevante nel semantic cache
> score.
