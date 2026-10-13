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
- `Idempotency-Key` é obrigatório (HTTP); no SQS é o campo `idempotencyKey` do corpo. O servidor nunca substitui a chave.
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
  `eventType`, `aggregateId`, `correlationId`, `causationId`, `occurredAt` UTC RFC 3339, `version`, `data`). A abertura de carteira
  com saldo > 0 cria `OPENING` + crédito + eventos no mesmo commit. O publicador está descrito em "Mensageria".
- `inbox_messages` (`PRIMARY KEY (consumer_name, message_id)`): linha gravada pelo consumidor SQS **na mesma transação** que altera o
  saldo (veja "Mensageria").

## Mensageria (SQS)

Duas filas FIFO (cada uma com sua DLQ, criadas pelo app na inicialização, com `RedrivePolicy` de `SQS_MAX_RECEIVE` entregas):

| Fila | Sentido | `MessageGroupId` | `MessageDeduplicationId` |
|---|---|---|---|
| `wager-transactions.fifo` (+ `-dlq`) | provedores/serviços → este serviço (comandos) | `walletId` | `idempotencyKey` (sugerido ao produtor) |
| `wallet-events.fifo` (+ `-dlq`) | este serviço → consumidores (eventos) | `aggregateId` | `eventId` |

**Por que FIFO e esses grupos.** O grupo `walletId` faz a fila entregar as mensagens de uma carteira em ordem e nunca em paralelo
(enquanto uma está em voo, a próxima do grupo espera), mesmo com várias instâncias consumindo; carteiras diferentes andam em paralelo.
A ordem não é a única proteção: o lock de linha da carteira e a idempotência continuam valendo, então a correção não depende da fila.
A deduplicação da fila só cobre 5 minutos; a deduplicação **definitiva** é a da inbox e das chaves de idempotência no PostgreSQL.

**Consumidor** (`internal/inbound`). Corpo JSON = o mesmo contrato do HTTP, com a chave no corpo
(`providerId`, `idempotencyKey`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money`,
`referenceExternalTransactionId`, `correlationId`). Usa o **mesmo** `app.Service.Process` do HTTP (via `ProcessWith`).
Em uma única transação SQL: `INSERT` na inbox (`consumer_name`, `providerId:idempotencyKey`, hash) → trava da carteira → decisão →
transação, ledger, saldo e outbox → `completed_at` da inbox → commit. Só depois do commit a mensagem é apagada.
- Entrega repetida: a inbox acusa a duplicata, nada é reprocessado, a mensagem é apagada (`duplicate`).
- Queda entre o commit e o delete: a mensagem volta e vira duplicata. Queda antes do commit: tudo é desfeito e a mensagem volta (testado).
- Falha transitória (banco fora, deadlock): `ChangeMessageVisibility` com backoff exponencial (1s, 2s, 4s… teto 60s). Após
  `SQS_MAX_RECEIVE` entregas, a **própria fila** move a mensagem para a DLQ.
- Mensagem inválida (JSON, campos, tipo desconhecido), conflito de idempotência (mesma chave, conteúdo diferente) ou carteira inexistente:
  vai direto para a DLQ, sem gastar retentativas. Rejeições de negócio (saldo insuficiente etc.) **não** são erro: viram transação `REJECTED` e a mensagem é confirmada.
- `correlationId`: vem do corpo; se faltar, usa-se o id da mensagem SQS (como o HTTP, que gera um quando o cabeçalho não vem).
- Se uma mensagem de um grupo falha, as seguintes do mesmo grupo no mesmo lote são devolvidas sem processar (preserva a ordem).
- Encerramento: para de buscar mensagens, conclui a que está em processamento (limite de 30s) e retorna.
- `SQS_VISIBILITY_SECONDS` (padrão 60) precisa ser maior que esse limite, para a mensagem não reaparecer enquanto é processada.

**Publicador da outbox** (`internal/outbox`). Publica depois do commit, **pelo menos uma vez**.
- Reivindica lotes com `FOR UPDATE SKIP LOCKED` e uma concessão (`locked_by`, `locked_until`, 30s). Vários publicadores/instâncias
  nunca pegam o mesmo evento (testado: 4 publicadores, 200 eventos, exatamente 200 envios).
- Só o evento **mais antigo não publicado de cada agregado** é elegível: a ordem por agregado vale mesmo com vários publicadores
  (um evento em backoff segura os seguintes do mesmo agregado).
- Falha ao publicar: `attempts+1`, `last_error`, `next_attempt_at` com backoff exponencial (1s → teto 60s). Fila fora do ar não derruba o serviço;
  ao voltar, a outbox drena sozinha (testado).
- Queda do publicador depois de reivindicar: a concessão expira e outro publica. Queda **depois** de enviar e **antes** de marcar:
  o evento é republicado com o **mesmo `eventId`**, que também é o `MessageDeduplicationId` (a fila descarta repetições nos 5 minutos seguintes;
  consumidores devem deduplicar por `eventId`).
- Eventos: `WagerTransactionProcessed`, `WagerTransactionRejected`, `WagerTransactionPendingReference`, `WalletBalanceChanged`.

**Controle de acesso à fila.** A fila de comandos é um canal **confiável**: o serviço aceita o `providerId` que vem na mensagem, sem validar JWT por
mensagem. Quem pode escrever nela é decidido pelo broker (IAM). Política pretendida na AWS real:
produtores → `sqs:SendMessage` só em `wager-transactions.fifo`; este serviço → `sqs:ReceiveMessage`, `sqs:DeleteMessage`,
`sqs:ChangeMessageVisibility`, `sqs:GetQueueAttributes` nela, e `sqs:SendMessage` em `wallet-events.fifo` e nas DLQs; consumidores de eventos → só leitura.
**No LocalStack do compose essa política não é aplicada** (qualquer credencial funciona): é uma limitação do ambiente local, não uma garantia.

## Autenticação e autorização

- IdP externo: **Keycloak** (compose, realm `wallet` importado), OAuth 2.0 `client_credentials` entre serviços. Não há cadastro
  de senhas nem emissão própria de tokens.
- O serviço valida o JWT **localmente**: assinatura RS256 contra o JWKS do IdP (cache + refresh em `kid` novo), `iss`, `exp`
  obrigatório (tolerância de 30s); outros algoritmos (inclusive `HS256`/`none`) são recusados. IdP inalcançável → `503`, não `401`.
- Identidade: o claim `azp` (client id) é o **`providerId` autorizado**. Papéis (realm roles): `wallet-provider` e `wallet-internal`.
- Matriz: provedor envia e consulta só as próprias transações (o `providerId` do corpo/URL deve ser o do token → senão `403`;
  transação de outro provedor responde `404`, sem revelar existência; vale também para replays). Operações de carteira
  (abrir, consultar, ledger, reconciliar) são só do serviço interno. `/health/*` é público. Acessos negados não têm efeito financeiro (testado).
- Segredos do realm são de **desenvolvimento**. O acesso à fila SQS é controlado pelo broker (veja "Mensageria"); no LocalStack não é aplicado.

## Composição com Uber Fx e shutdown

`bootstrap.Module()`: `LoadConfig` (valida; sem `OIDC_*` a app não sobe) → `newDB` (ping na partida; `Close` na parada) → `Service` (com observador de métricas) →
verificador de JWT → API → `Invoke(startWorker, startMessaging, startServer)`. Início: falha se o PostgreSQL estiver inalcançável, a porta ocupada
ou o SQS não responder dentro do prazo de partida (30s; o app tenta provisionar as filas de novo a cada 2s). Parada (ordem inversa): o servidor deixa de
aceitar conexões e conclui as requisições em andamento; o consumidor SQS e o publicador da outbox concluem o que estão fazendo; o worker de pendências é
cancelado e **aguardado**; só então o pool do banco fecha. `MESSAGING_ENABLED=false` desliga a mensageria (usado nos testes de ciclo de vida do Fx).

## Observabilidade

Logs JSON (`slog`) com `correlationId` (cabeçalho `X-Correlation-Id` ou gerado, ecoado e persistido; nas mensagens SQS vem do corpo),
`transactionId`, `walletId`, `providerId`; sem credenciais nem payload completo. Health: `live` e `ready` (PostgreSQL + as duas filas).

`GET /metrics` (formato Prometheus, sem autenticação, como `/health/*`; em produção restrinja por rede):

| Métrica | Significado |
|---|---|
| `wallet_wager_results_total{source,kind,status}` | resultados por origem (`http`/`sqs`), tipo e status (`PROCESSED`, `REJECTED`, `PENDING_REFERENCE`, `ERROR`) |
| `wallet_wager_duplicates_total{source}` | repetições respondidas sem reprocessar |
| `wallet_wager_duration_seconds{source}` | histograma de latência |
| `wallet_concurrency_conflicts_total` | tentativas repetidas por deadlock/serialização/unicidade concorrente |
| `wallet_pending_retries_total` | retentativas de referências pendentes |
| `wallet_sqs_messages_total{result}` | `acked`, `duplicate`, `retried`, `dead_lettered` |
| `wallet_sqs_dlq_total` | mensagens que o consumidor enviou à DLQ (veneno) |
| `wallet_outbox_published_total` / `wallet_outbox_publish_failures_total` | publicação da outbox |
| `wallet_outbox_lag_seconds`, `wallet_outbox_unpublished` | idade do evento mais antigo e quantidade não publicada (−1 se o banco não respondeu) |
| `wallet_reconciliation_divergences_total` | divergências entre saldo e ledger |

Limite conhecido: mensagens movidas para a DLQ pelo *redrive* da própria fila (após `SQS_MAX_RECEIVE` falhas) acontecem dentro do SQS e **não**
são contadas por este serviço; o que o serviço vê é cada `retried`. Divergência de reconciliação também vai para o log.

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
- O `docker compose` completo (Keycloak com hostname fixo `localhost:8081`, LocalStack) não foi executado no ambiente em que o código foi escrito; lá só rodaram os testes (PostgreSQL real, filas em memória). A execução do compose é verificada na máquina do autor.
- Os testes de mensageria usam uma fila em memória com a mesma semântica relevante do SQS FIFO (grupo, visibilidade, contagem de entregas, deduplicação, DLQ). O adaptador do SDK da AWS compila, mas só é exercitado contra o LocalStack.
- Validação de `aud`, introspecção/revogação de token e rotação de segredos não foram implementadas.

## Não implementado

| Requisito | Situação |
|---|---|
| `FAILED` gravado por algum caminho | Estado existe; sem uso (falhas técnicas voltam como erro e são retentadas) |
| Validação de `aud`, introspecção/revogação de token | Não implementado |
| Política IAM do SQS **aplicada** | Documentada; o LocalStack do compose não a aplica |
| Execução contra a AWS real | Não testada; só LocalStack (e filas em memória nos testes) |
| Três processos reais contra o mesmo banco/fila | Não automatizado (os testes usam várias instâncias no mesmo processo) |
| Tracing OpenTelemetry, partidas dobradas, testes de carga | Opcionais; não feitos |
