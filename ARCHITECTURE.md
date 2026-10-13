# Arquitetura e decisões

Documento de decisões, interpretações e limitações. Tudo o que está marcado como implementado tem teste
que o exercita (veja README); o que não existe está na seção final.

## Visão geral

API HTTP + worker no mesmo binário, estado exclusivamente no PostgreSQL (nada em memória que afete
correção), então qualquer número de instâncias pode rodar ao mesmo tempo. Camadas: `money`/`wallet`/`wagering`
(domínio puro, sem HTTP/SQL/Fx) → `app` (casos de uso + SQL explícito) → `httpapi` → `bootstrap` (Fx).

## Dinheiro

- `int64` em centavos + moeda (3 letras maiúsculas). Nenhum `float` em parsing, cálculo, serialização ou persistência.
- Entrada: string decimal com **exatamente duas casas** (`"25.00"`); rejeita vazio, sinal, notação científica,
  `NaN`/`Infinity`, escala diferente de 2 e overflow. Não há normalização (logo, o hash de idempotência é estável).
- Overflow tratado em parse, soma, subtração e negação (limite: `MaxInt64` centavos). Moedas diferentes → `ErrCurrencyMismatch`.
- Persistência: `BIGINT` em centavos. Biblioteca: `database/sql` + `pgx/stdlib`, SQL escrito à mão; transações,
  locks e constraints explícitos em `internal/app/store.go` e na migration.
- A moeda é validada pelo formato, não contra a lista ISO 4217 completa.

## Transações SQL e concorrência

Cada operação é **uma** transação `READ COMMITTED`: `SELECT ... FOR UPDATE` na linha da carteira →
consulta de duplicata → decisão → linha de `wager_transactions` → lançamento no ledger → `UPDATE wallets ... WHERE version = n`
→ eventos no outbox → commit.

- Coordenação **por carteira** (lock de linha); carteiras diferentes não se bloqueiam (sem lock global).
- `WHERE version = n` é a segunda barreira contra lost updates; `CHECK (balance >= 0)` é a terceira.
- Trigger deferido: no commit, `wallets.balance` precisa ser igual ao `balance_after` do último lançamento.
  Alterar saldo sem lançamento falha no commit, mesmo com SQL manual.
- Ordem de locks fixa (carteira, depois a linha da transação) → sem deadlock entre API e worker. Deadlock/falha de
  serialização (`40P01`/`40001`) e violação de unicidade (corrida entre instâncias) repetem a operação até 3 vezes.
- `SERIALIZABLE` foi descartado: o lock por linha já elimina lost updates com menos retries.

## Idempotência

- Persistente: índices únicos parciais em `(provider_id, idempotency_key)` e `(provider_id, external_transaction_id)`.
  Sobrevive a reinícios e funciona entre instâncias.
- `Idempotency-Key` é obrigatório (HTTP); no SQS será `data.idempotencyKey`. O servidor nunca substitui a chave.
- **Hash do payload**: SHA-256 (hex) do JSON canônico (chaves ordenadas) de `providerId, externalTransactionId, playerId,
  walletId, roundId, gameId, kind, money{amount,currency}, referenceExternalTransactionId` (`""` se ausente). Fora do hash:
  `idempotencyKey`, `correlationId` e metadados de transporte. HTTP e SQS montam o mesmo `Command`, logo o mesmo hash.
- Mesma chave + mesmo hash → resultado persistido, `idempotentReplay: true`, com o **saldo observado no processamento original**.
  Mesma chave + hash diferente → `409 IDEMPOTENCY_KEY_CONFLICT`. Mesma operação `(provider, externalId)` com outra
  chave → `409 EXTERNAL_TRANSACTION_CONFLICT`. Replay de rejeição e de pendência devolve o mesmo estado.
- A chave é escopada por provedor (`provider_id` faz parte do índice). O provedor vem do token, não do corpo.

## WagerTransaction: estados e códigos

`PENDING` (somente em memória: operações sem dependência concluem na mesma transação, sem commit de aceite) →
`PROCESSED` | `REJECTED` | `PENDING_REFERENCE` → (`PROCESSED` | `REJECTED`). `FAILED` existe no domínio e no schema
(terminal, falha permanente de infraestrutura), mas **hoje nenhum caminho o grava**: falhas transitórias revertem a
transação e viram `503`/retry. Estados terminais são imutáveis no domínio **e** por trigger no banco; `DELETE` é bloqueado.

| Operação | Movimento | Condição |
|---|---|---|
| `BET` | débito | valor > 0, saldo suficiente |
| `WIN` | crédito | valor > 0; referência opcional (deve ser BET da mesma rodada) |
| `LOSS` | nenhum | valor exatamente `0.00`; sem ledger nem mudança de versão; emite só `WagerTransactionProcessed` |
| `REFUND` | crédito | devolve integralmente uma BET processada |
| `ROLLBACK` | contrário ao original | desfaz integralmente BET (crédito), WIN ou REFUND (débito) |

Referência: resolvida por `(providerId, referenceExternalTransactionId)`; precisa concordar em jogador, carteira, moeda e
rodada; valor da reversão = valor referenciado (sem reversão parcial).

**Códigos de rejeição (`failureCode`)** — definitivos, persistidos e com evento: `INSUFFICIENT_FUNDS` (BET sem saldo),
`REVERSAL_INSUFFICIENT_FUNDS` (reversão que debitaria além do saldo), `WALLET_PLAYER_MISMATCH`, `CURRENCY_MISMATCH`,
`REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED`, `REFERENCE_MISMATCH`, `REFERENCE_KIND_INVALID`,
`REVERSAL_AMOUNT_MISMATCH`, `ALREADY_REVERSED`. Entradas corrigíveis (JSON, valor, tipo, campos) **não** são persistidas:
respondem `400`.

**REFUND × ROLLBACK:** uma transação recebe no máximo **uma** reversão bem-sucedida (REFUND *ou* ROLLBACK), garantido
por índice único parcial em `reference_transaction_id`. A segunda é `ALREADY_REVERSED`, o que impede devolver o mesmo débito
duas vezes. Um ROLLBACK pode ter como alvo o próprio REFUND (desfaz o crédito), o que não reabre a BET original.

## Referências pendentes

Se a referência ainda não existe, a operação é persistida como `PENDING_REFERENCE` (`202`, evento
`WagerTransactionPendingReference`). O worker (qualquer instância) reprocessa com **backoff exponencial**
(2s, 4s, … teto 5 min), travando a carteira e rechecando o estado antes de agir. Após `PENDING_MAX_ATTEMPTS` (8) tentativas
sem sucesso → `REJECTED` com `REFERENCE_NOT_FOUND`. Se a referência existe mas ainda está pendente, continua aguardando e, ao
esgotar, termina `REFERENCE_NOT_PROCESSED`; se a referência terminou `REJECTED`/`FAILED`, rejeita na hora com `REFERENCE_NOT_PROCESSED`.
Pendências sobrevivem a reinícios (estado no banco).

## Ledger, inbox e outbox

- `wallet_ledger_entries`: direção, valor, saldo antes/depois, `CHECK` da aritmética, `UNIQUE (wallet_id, transaction_id)`,
  triggers que bloqueiam `UPDATE`/`DELETE`/`TRUNCATE`. `LOSS` e rejeições não geram lançamento. Paginação por cursor opaco (`seq`).
- `outbox_events`: o evento é gravado **na mesma transação** do estado, do saldo e do ledger (envelope com `eventId`,
  `eventType`, `aggregateId`, `correlationId`, `causationId`, `occurredAt` UTC RFC 3339, `version`, `data`). Colunas de retry,
  backoff e lock de publicador já existem. A abertura de carteira com saldo > 0 cria `OPENING` + crédito + eventos no mesmo commit.
- `inbox_messages` (`PRIMARY KEY (consumer_name, message_id)`) existe no schema.
- **Falta** (veja "Não implementado"): o publicador da outbox e o consumidor SQS/inbox.

## Autenticação e autorização

- IdP externo: **Keycloak** (compose, realm `wallet` importado), OAuth 2.0 `client_credentials` entre serviços. Não há cadastro
  de senhas nem emissão própria de tokens.
- O serviço valida o JWT **localmente**: assinatura RS256 contra o JWKS do IdP (cache + refresh em `kid` novo), `iss`, `exp`
  obrigatório (tolerância de 30s); outros algoritmos (inclusive `HS256`/`none`) são recusados. IdP inalcançável → `503`, não `401`.
- Identidade: o claim `azp` (client id) é o **`providerId` autorizado**. Papéis (realm roles): `wallet-provider` e `wallet-internal`.
- Matriz: provedor envia e consulta só as próprias transações (o `providerId` do corpo/URL deve ser o do token → senão `403`;
  transação de outro provedor responde `404`, sem revelar existência; vale também para replays). Operações de carteira
  (abrir, consultar, ledger, reconciliar) são só do serviço interno. `/health/*` é público. Acessos negados não têm efeito financeiro (testado).
- Segredos do realm são de **desenvolvimento**. O controle de acesso à fila SQS (credenciais/políticas do broker) não existe ainda.

## Composição com Uber Fx e shutdown

`bootstrap.Module()`: `LoadConfig` (valida; sem `OIDC_*` a app não sobe) → `newDB` (ping na partida; `Close` na parada) → `Service` →
verificador de JWT → API → `Invoke(startWorker, startServer)`. Início: falha se o PostgreSQL estiver inalcançável ou a porta ocupada.
Parada (ordem inversa): o servidor deixa de aceitar conexões e conclui as requisições em andamento (`Shutdown` com prazo); o worker é
cancelado e **aguardado**; só então o pool do banco fecha. Testado (`bootstrap_integration_test.go`).

## Observabilidade

Logs JSON (`slog`) com `correlationId` (cabeçalho `X-Correlation-Id` ou gerado, ecoado e persistido), `transactionId`, `walletId`,
`providerId`; sem credenciais nem payload completo. Health: `live` e `ready` (PostgreSQL). Divergência de reconciliação vai para o log.

## Códigos HTTP

| Situação | HTTP | `error.code` / corpo |
|---|---|---|
| Processada / replay | 200 | `status: PROCESSED`, `idempotentReplay` |
| Aguardando referência | 202 | `status: PENDING_REFERENCE`, `nextAttemptAt` |
| Carteira criada | 201 | `{id, playerId, balance, version}` |
| Entrada inválida | 400 | `INVALID_REQUEST`, `MISSING_IDEMPOTENCY_KEY` |
| Sem/Token inválido | 401 | `UNAUTHORIZED` (+ `WWW-Authenticate`) |
| Sem permissão / provedor diferente | 403 | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Não encontrado | 404 | `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND` |
| Conflito | 409 | `IDEMPOTENCY_KEY_CONFLICT`, `EXTERNAL_TRANSACTION_CONFLICT`, `WALLET_ALREADY_EXISTS` |
| Rejeição de negócio | 422 | `status: REJECTED`, `failureCode` |
| PostgreSQL/IdP indisponível | 503 | `SERVICE_UNAVAILABLE`, `IDENTITY_PROVIDER_UNAVAILABLE` (+ `Retry-After`) |

## Interpretações e limitações

- `go.mod` declara `go 1.27.2` (a versão usada no desenvolvimento); o Dockerfile usa `golang:1.27-alpine`.
- "Três instâncias" nos testes = três `Service` com pools próprios no mesmo processo; três processos não estão automatizados.
- Contra o Keycloak real há um teste (`-tags keycloak`) e o fluxo do README, mas ambos dependem de subir o compose.
- O `docker compose` do Keycloak (hostname fixo `localhost:8081`, import do realm) não foi executado no ambiente em que o código foi escrito.
- Validação de `aud`, introspecção/revogação de token e rotação de segredos não foram implementadas.

## Não implementado

| Requisito | Situação |
|---|---|
| SQS (LocalStack), filas FIFO + DLQ, consumidor, uso da inbox, `MessageGroupId`/`MessageDeduplicationId`, retry/DLQ | **Não implementado** (só a tabela `inbox_messages`) |
| Publicador da outbox (múltiplos publishers, backoff, recuperação, destino de eventos) | **Não implementado** (eventos são gravados, não publicados) |
| Métricas (resultados, duplicatas, retries, DLQ, conflitos, atraso da outbox, latência) | **Não implementado** |
| `FAILED` gravado por algum caminho | Estado existe; sem uso |
| Controle de acesso à mensageria | Não implementado |
| Tracing OpenTelemetry, partidas dobradas, testes de carga | Opcionais; não feitos |
