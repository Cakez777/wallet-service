# Arquitetura e decisões

Serviço Go (Uber Fx) que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre
carteiras de jogadores, por **HTTP** e por **SQS**, com as mesmas garantias. O PostgreSQL é a única fonte de estado: qualquer número de
instâncias pode rodar ao mesmo tempo, e nada que afete o resultado financeiro vive em memória.

Este documento registra as decisões, as interpretações adotadas e o que não foi feito (seção 14).

## 1. Visão geral e pacotes

| Pacote | Responsabilidade |
|---|---|
| `internal/money` | Value object `Money` (domínio puro) |
| `internal/wallet` | Agregado `Wallet` e `LedgerEntry` (domínio puro) |
| `internal/wagering` | Comando, `WagerTransaction`, máquina de estados, regras (`Plan`), eventos (domínio puro) |
| `internal/app` | Casos de uso (`Service`) e acesso ao PostgreSQL |
| `internal/httpapi` | Handlers, autenticação/autorização, contratos HTTP |
| `internal/inbound` | Consumidor SQS com inbox transacional |
| `internal/outbox` | Publicador da outbox |
| `internal/messaging` | Abstração de fila, adaptador SQS (AWS SDK v2), fila FIFO em memória (testes) |
| `internal/auth` | Validação de JWT (JWKS) |
| `internal/metrics` | Métricas no formato Prometheus |
| `internal/bootstrap` | Composição com Uber Fx e ciclo de vida |

Os pacotes `money`, `wallet` e `wagering` não importam Fx, HTTP, SQS nem `database/sql`. Entidades têm estado encapsulado, construtores que
validam, métodos explícitos de transição e funções de **reidratação** separadas da criação (reidratar não reaplica movimentações, transições
nem eventos; há testes para isso). Erros de domínio são tipados (`*ValidationError`, sentinelas comparáveis com `errors.Is`); não há `panic`
para regras de negócio. Toda operação de I/O recebe `context.Context`.

## 2. Dinheiro

- **Representação:** `int64` em centavos mais o código da moeda. Faixa: ±9.223.372.036.854.775.807 centavos (≈ 92 quatrilhões de reais).
  Overflow é verificado no parsing, na soma, na subtração e na negação (`ErrOverflow`).
- **Entrada e saída:** `{"amount":"25.00","currency":"BRL"}`. O formato aceito é exatamente `^[0-9]+\.[0-9]{2}$`: dois decimais, sem sinal,
  sem notação científica, sem `NaN`/`Infinity`, sem valor vazio. Nada é arredondado: `"25"`, `"25.5"` e `"25.001"` são **rejeitados**.
  Como só existe uma forma textual válida por valor, não há normalização anterior ao hash de idempotência.
- **Moeda:** três letras maiúsculas (`^[A-Z]{3}$`). Valida o formato, **não** consulta a lista completa da ISO 4217.
- **Aritmética:** soma, subtração, negação e comparação exigem moedas iguais (`ErrCurrencyMismatch`). Valores negativos existem em cálculos
  internos, mas o saldo nunca fica negativo (domínio e `CHECK` no banco).
- **Float:** nenhum `float32`/`float64` toca em dinheiro (parsing, cálculo, JSON ou SQL). O único `float64` do código está na métrica de atraso da outbox (segundos).
- **Persistência:** `BIGINT` (centavos) e `CHAR(3)` (moeda), preservando valor e moeda exatamente.
- **Política de zero:** zero é aceito no saldo inicial e em `LOSS` (que **exige** `"0.00"`); `BET`, `WIN`, `REFUND` e `ROLLBACK` exigem valor maior que zero.

## 3. Acesso ao banco e transações

- **Biblioteca:** `database/sql` com o driver `pgx` (`pgx/v5/stdlib`) e **SQL explícito** (sem ORM). Transações, locks e constraints são todos
  visíveis no código e nas migrations.
- **Delimitação da transação:** quem abre e confirma a transação SQL é o **caso de uso** (`app.Service`). As funções de acesso a dados
  (`store.go`: `lockWallet`, `insertTx`, `insertLedger`, `updateWallet`, `insertEvent`...) recebem um `*sql.Tx` e **nunca** abrem transação
  própria; assim, estado, saldo, ledger, inbox e outbox são confirmados atomicamente. O consumidor SQS participa da mesma transação por meio
  de ganchos (`app.Hooks.Before/After`), sem que o serviço conheça mensageria.
- **Migrations:** `golang-migrate` (`db/migrations`, pares `up`/`down`), aplicadas pelo serviço `migrate` do Compose.

### Concorrência (estratégia)

Locking **pessimista por carteira**, com proteção adicional otimista e constraints:

1. A transação usa `READ COMMITTED` e trava a linha da carteira com `SELECT ... FOR UPDATE`. Operações da mesma carteira são serializadas
   pelo banco, entre qualquer número de processos; carteiras diferentes não se bloqueiam (**não há lock global**).
2. O saldo é gravado com `UPDATE wallets ... WHERE id = $1 AND version = $n` (versão esperada): se alguém alterou fora do lock, a operação falha
   em vez de sobrescrever (sem *lost update*).
3. `CHECK (balance >= 0)` e `UNIQUE (player_id, currency)` no schema.
4. Um trigger de constraint **deferido** exige que `wallets.balance` seja igual ao `balance_after` do último lançamento do ledger ao confirmar.
5. Ordem de aquisição de locks fixa (inbox → carteira → transação), evitando deadlocks entre instâncias.
6. Erros repetíveis (violação de unicidade por corrida de chaves de idempotência, deadlock `40P01`, falha de serialização `40001`) refazem a
   operação inteira, até 3 tentativas; o conflito é contado em `wallet_concurrency_conflicts_total`.

A deduplicação do SQS FIFO e a ordem por grupo **não** são necessárias à correção: a corretude vale mesmo se a fila entregar fora de ordem ou duplicado.

## 4. Idempotência financeira

Persistente, no PostgreSQL, por índices únicos **parciais**: `(provider_id, idempotency_key)` e `(provider_id, external_transaction_id)`
(apenas operações de origem externa). Sobrevive ao reinício de todos os processos.

- **Chave:** HTTP: cabeçalho `Idempotency-Key` (obrigatório; o servidor nunca a substitui). SQS: `data.idempotencyKey`.
- **Hash do payload:** SHA-256 do JSON canônico (objeto com chaves em ordem alfabética, valores monetários como strings) dos campos de negócio:
  `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency`,
  `referenceExternalTransactionId` (vazio quando ausente). **Excluídos:** `Idempotency-Key`/`idempotencyKey`, `correlationId`, `messageId` e
  demais metadados de transporte. HTTP e SQS montam o mesmo `Command`, logo o mesmo hash.
- **Comportamento:**
  - mesma chave e mesmo hash → devolve o resultado **persistido** com `idempotentReplay: true`, inclusive o **saldo observado no processamento
    original** (não o saldo atual);
  - mesma chave com hash diferente → `409 IDEMPOTENCY_KEY_CONFLICT`;
  - mesmo `(providerId, externalTransactionId)` com outra chave → `409 EXTERNAL_TRANSACTION_CONFLICT`;
  - replay de operação rejeitada ou pendente devolve o mesmo estado (`422` / `202`).
- **Duas camadas distintas:** a idempotência financeira (acima) impede reaplicar a *operação*; a inbox (seção 8) impede reprocessar a *mensagem*.

## 5. Estados, falhas e operações

### Máquina de estados

```
            ┌──────────────► PROCESSED   (terminal)
 PENDING ───┼──────────────► REJECTED    (terminal)
 (memória)  ├──────────────► FAILED      (terminal)
            └─► PENDING_REFERENCE ──┬──► PROCESSED / REJECTED / FAILED
                     ▲              │
                     └── reagenda ──┘ (tentativa seguinte, com backoff)
```

- Transições são métodos do domínio (`MarkProcessed`, `Reject`, `Fail`, `AwaitReference`, `Reschedule`); partir de um estado terminal retorna erro.
  No banco, o trigger `wager_transactions_guard` bloqueia `DELETE`, `TRUNCATE`, alteração de linhas terminais e das colunas de identidade.
- **`PENDING` existe apenas em memória.** Operações sem dependência são decididas de forma síncrona dentro de uma única transação, sem commit
  intermediário de aceite (permitido pelo enunciado); logo não há `PENDING` confirmado a retomar. Quando há dependência, o estado confirmado já
  é `PENDING_REFERENCE`, e o worker de pendências (qualquer instância) o retoma.
- **`FAILED` existe no domínio e no schema, mas nenhum caminho de execução o grava hoje.** Falhas de infraestrutura são sempre repetíveis (abaixo).

### Falha transitória × permanente

| Classe | Exemplos | Efeito |
|---|---|---|
| **Transitória** | PostgreSQL ou rede fora, timeout, conexão caída, classes `08`/`53`/`57`, deadlock, falha de serialização (`app.IsTransient`) | Nada é persistido. HTTP: `503` com `Retry-After`. SQS: a mensagem volta com backoff |
| **Permanente — entrada inválida** | JSON malformado, campo ausente ou inválido, tipo desconhecido, `OPENING` enviado de fora | Nada é persistido. HTTP: `400`. SQS: DLQ direto |
| **Permanente — conflito** | Chave de idempotência reutilizada com outro conteúdo, mesma transação externa com outra chave | HTTP: `409`. SQS: DLQ direto |
| **Permanente — regra de negócio** | Saldo insuficiente, referência divergente etc. | Transação `REJECTED` **persistida e auditável** com `failureCode`. HTTP: `422`. SQS: mensagem confirmada (terminal) |

### Operações

| Tipo | Movimento | Condição |
|---|---|---|
| `BET` | Débito | Valor > 0 e saldo suficiente (`INSUFFICIENT_FUNDS`) |
| `WIN` | Crédito | Valor > 0; referência opcional (uma `BET` da mesma rodada) |
| `LOSS` | Nenhum | `amount = "0.00"`; sem lançamento, sem mudar a versão; gera `WagerTransactionProcessed`, sem `WalletBalanceChanged` |
| `REFUND` | Crédito | Devolve integralmente uma `BET` processada |
| `ROLLBACK` | Contrário ao original | Desfaz integralmente uma `BET`, `WIN` ou `REFUND` processada |

`REFUND` e `ROLLBACK` exigem `referenceExternalTransactionId`, resolvido por `(providerId, referenceExternalTransactionId)`. A referência deve
concordar em jogador, carteira, moeda e rodada, e o valor deve ser **igual** (reversão parcial não existe). `OPENING` é reservado à abertura interna.

### Códigos de falha (`failureCode`, estáveis)

| Código | Significado |
|---|---|
| `INSUFFICIENT_FUNDS` | `BET` sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | reversão que debitaria mais que o saldo (**distinto** do anterior) |
| `WALLET_PLAYER_MISMATCH` | a carteira não pertence ao jogador |
| `CURRENCY_MISMATCH` | moeda diferente da carteira |
| `REFERENCE_NOT_FOUND` | referência não chegou dentro do limite de tentativas |
| `REFERENCE_NOT_PROCESSED` | referência rejeitada/falhou, ou ainda pendente ao esgotar as tentativas |
| `REFERENCE_MISMATCH` | provedor, jogador, carteira, moeda ou rodada divergem |
| `REFERENCE_KIND_INVALID` | tipo da referência não aceita a operação |
| `REVERSAL_AMOUNT_MISMATCH` | valor diferente do referenciado |
| `ALREADY_REVERSED` | a referência já recebeu uma reversão |
| `INTERNAL_ERROR` | reservado a `FAILED` (não usado) |

Entradas corrigíveis (400) nunca viram transação; os códigos acima são resultados **definitivos** de negócio.

### `REFUND` e `ROLLBACK` sobre a mesma aposta

Um índice único parcial, `ux_wager_one_reversal_per_reference`, permite **no máximo uma** reversão `PROCESSED` (`REFUND` **ou** `ROLLBACK`) por
transação referenciada. Consequências:

- uma `BET` pode ser devolvida (`REFUND`) **ou** desfeita (`ROLLBACK`), nunca as duas coisas: o débito original não é devolvido duas vezes;
- uma `WIN` só pode ser desfeita (`ROLLBACK`), uma vez; um `REFUND` pode ser desfeito por `ROLLBACK` (que debita o jogador, com
  `REVERSAL_INSUFFICIENT_FUNDS` se faltar saldo), e a aposta original **não** volta a ser reembolsável (o `REFUND` continua ocupando o lugar);
- reverter uma reversão que já é um `ROLLBACK`, ou reverter `LOSS`, é `REFERENCE_KIND_INVALID`;
- reversões concorrentes da mesma aposta são decididas pelo lock da carteira mais o índice (testado: exatamente uma vence, as demais `ALREADY_REVERSED`).

## 6. Referências ainda indisponíveis

| Situação da referência | Comportamento |
|---|---|
| Não existe ainda | A operação é persistida como `PENDING_REFERENCE` (HTTP `202`) e o evento `WagerTransactionPendingReference` é gravado. Ao esgotar: `REJECTED` com `REFERENCE_NOT_FOUND` |
| Existe, mas ainda está `PENDING_REFERENCE` | Aguarda também. Ao esgotar: `REJECTED` com `REFERENCE_NOT_PROCESSED` |
| Existe e terminou `REJECTED` (ou `FAILED`) | Rejeição **imediata** com `REFERENCE_NOT_PROCESSED`, sem esperar |
| Existe e está `PROCESSED` | Segue as regras da seção 5 |

O worker (`RunPendingWorker`, uma goroutine por instância, qualquer número de instâncias) seleciona pendências vencidas e, para cada uma,
trava a carteira, **reavalia** a referência e decide; reagenda com **backoff exponencial** (2 s, 4 s, 8 s... teto 5 min). Padrão: **8 tentativas**
(≈ 4 min até a rejeição), configurável (`PENDING_MAX_ATTEMPTS`, `PENDING_BASE_DELAY_MS`, `PENDING_MAX_DELAY_MS`). O estado e o próximo horário ficam no
banco, então pendências sobrevivem a reinícios e podem ser assumidas por outra instância. A rejeição por expiração grava `WagerTransactionRejected`.

## 7. Ledger

`wallet_ledger_entries`: `id`, `wallet_id`, `transaction_id`, direção (`DEBIT`/`CREDIT`), valor, saldo anterior/posterior, instante. O banco impõe a
aritmética (`CHECK balance_after = balance_before ± amount`), `UNIQUE (wallet_id, transaction_id)` e **imutabilidade** (triggers bloqueiam
`UPDATE`, `DELETE` e `TRUNCATE`); correções exigem novos lançamentos. `LOSS` e rejeições não geram lançamento. A construção em Go
(`NewLedgerEntry`) também valida a aritmética.

**Abertura de carteira** (`POST /wallets`): com saldo > 0, uma só transação cria a carteira (versão 1), a transação `OPENING` em `PROCESSED`, o
lançamento de crédito e os eventos `WagerTransactionProcessed` e `WalletBalanceChanged`. Saldo zero cria só a carteira. Um índice impede crédito inicial
duplicado; outra abertura para o mesmo jogador e moeda dá `409`.

**Reconciliação** (`POST /wallets/{id}/reconciliation`): lê saldo e ledger no **mesmo snapshot** (`REPEATABLE READ`, somente leitura), compara e
responde `walletId`, `storedBalance`, `calculatedBalance`, `difference` (armazenado − calculado), `consistent`, `checkedEntries`. Divergências vão para a
resposta, para o log e para `wallet_reconciliation_divergences_total`. Nunca altera o saldo.

## 8. Mensageria (SQS)

Duas filas FIFO, cada uma com sua DLQ e `RedrivePolicy` (provisionadas pelo próprio app na inicialização, com nova tentativa a cada 2 s enquanto o SQS sobe):

| Fila | Sentido | `MessageGroupId` | `MessageDeduplicationId` |
|---|---|---|---|
| `wager-transactions.fifo` (+ `-dlq`) | provedores/serviços → este serviço (comandos) | `walletId` | `idempotencyKey` (sugerido ao produtor) |
| `wallet-events.fifo` (+ `-dlq`) | este serviço → consumidores (eventos) | `aggregateId` | `eventId` |

O grupo `walletId` faz a fila entregar as mensagens de uma carteira em ordem e nunca em paralelo, mesmo com várias instâncias; carteiras diferentes
andam em paralelo. A deduplicação nativa dura só 5 minutos e **não** é a proteção financeira: esta é a inbox mais a idempotência do banco.

### 8.1 Formato do comando

```json
{
  "messageId": "msg-456",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-18T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-456",
    "idempotencyKey": "provider-a:transaction-456",
    "playerId": "PLAYER_ID",
    "walletId": "WALLET_ID",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

`messageId`, `type` (apenas `WagerTransactionRequested`) e `occurredAt` (RFC 3339) são obrigatórios; campos desconhecidos e JSON extra após o objeto
são recusados. `data.referenceExternalTransactionId` e `data.correlationId` são opcionais (sem `correlationId`, usa-se o `messageId`).

### 8.2 Inbox e fluxo de processamento

A identidade durável é `(consumer_name, message_id)` com o **`messageId` do envelope**, e o `payload_hash` é conferido em reentregas (mesmo `messageId` com
conteúdo diferente → `ErrInboxConflict` → DLQ). Em **uma única transação SQL**:

1. `INSERT` na inbox (`ON CONFLICT DO NOTHING`); se já existe com o mesmo hash, é duplicata: nada é reprocessado;
2. trava da carteira, consulta de idempotência, decisão;
3. transação, ledger, saldo e eventos da outbox (ou a pendência persistida);
4. `completed_at` da inbox; **commit**; só então `DeleteMessage`.

Uma referência pendente conclui a mensagem de entrada depois de a pendência estar persistida; o worker assume a continuidade.

### 8.3 Cenários

| Cenário | Resultado |
|---|---|
| Mesmo `messageId` reentregue | A inbox reconhece; sem novo efeito; a mensagem é apagada (`duplicate`) |
| `messageId` diferente, mesma operação (inclusive vinda antes por HTTP) | A idempotência financeira devolve o resultado original: sem segundo débito |
| Queda antes do commit | Tudo é desfeito; a mensagem volta e é processada |
| Queda depois do commit, antes do delete | A mensagem volta; a inbox a reconhece como duplicata |
| Mesma operação por HTTP e por SQS ao mesmo tempo | Os dois caminhos usam o mesmo caso de uso e o mesmo lock; um aplica, o outro devolve replay |

### 8.4 Tentativas, visibilidade e DLQ

| Parâmetro | Valor (configurável) |
|---|---|
| Entregas antes do redrive para a DLQ | 5 (`SQS_MAX_RECEIVE`) |
| Visibility timeout da fila | 60 s (`SQS_VISIBILITY_SECONDS`, precisa ser > 30 s) |
| Limite de processamento por mensagem | 30 s |
| Backoff de retentativa (`ChangeMessageVisibility`) | 1 s, 2 s, 4 s... teto 60 s |
| Workers por instância | 4 (`CONSUMER_WORKERS`) |

- **Falha transitória:** a mensagem volta à fila com backoff; esgotadas as entregas, a própria fila a move para a DLQ.
- **Mensagem inválida, conflito de idempotência, conflito de inbox, carteira inexistente:** vai **direto** para a DLQ (cópia + delete), sem gastar tentativas.
- **Rejeição de negócio persistida:** terminal; a mensagem é confirmada.
- Se uma mensagem de um grupo falha, as seguintes do mesmo grupo no mesmo lote são devolvidas sem processar (preserva a ordem).
- **`SIGTERM`:** o consumidor para de buscar mensagens, conclui a que está em processamento (até 30 s) e retorna; o publicador faz o mesmo e devolve ao banco
  os eventos reivindicados e ainda não tentados.

## 9. Outbox e publicação

Os eventos são gravados em `outbox_events` **na mesma transação** do estado, saldo e ledger; nada é publicado antes do commit. Um worker **separado**
(`internal/outbox`) os publica na fila de eventos, pelo menos uma vez.

- **Vários publicadores:** cada lote é reivindicado com `FOR UPDATE SKIP LOCKED` e uma concessão (`locked_by`, `locked_until`, 30 s); dois publicadores nunca pegam
  o mesmo evento (testado com 4 publicadores e 200 eventos: exatamente 200 envios).
- **Ordem por agregado:** só o evento mais antigo não publicado de cada agregado é elegível (um evento em backoff segura os seguintes do mesmo agregado).
- **Falha de publicação:** `attempts + 1`, `last_error`, `next_attempt_at` com backoff exponencial (1 s → 60 s). Com a fila fora do ar, o serviço continua operando e a outbox
  drena sozinha quando ela volta.
- **Recuperação:** se o publicador morrer depois de reivindicar, a concessão expira e outra instância assume. Se morrer **depois** de enviar e **antes** de marcar `published_at`, o evento é
  republicado com o **mesmo `eventId`** (que é o `MessageDeduplicationId`; a fila descarta repetições por 5 minutos e os consumidores devem deduplicar por `eventId`).
  Ambos os cenários são testados.

### Contrato dos eventos (fila `wallet-events.fifo`)

Envelope: `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` (opcional), `occurredAt` (UTC, RFC 3339), `version` (1), `data`. Valores monetários são strings decimais.

| `eventType` | Gatilho | `aggregateId` | `data` |
|---|---|---|---|
| `WagerTransactionProcessed` | operação concluída (inclui `LOSS` e `OPENING`) | id da transação | `transactionId`, `walletId`, `playerId`, `kind`, `money`, `balance` e, para origem externa, `providerId`, `externalTransactionId`, `roundId`, `gameId`, `referenceExternalTransactionId` |
| `WagerTransactionRejected` | rejeição definitiva de negócio | id da transação | os campos acima (sem `balance`) mais `failureCode` |
| `WagerTransactionPendingReference` | registro de espera pela referência | id da transação | os campos acima mais `nextAttemptAt` |
| `WalletBalanceChanged` | mudança efetiva de saldo | id da carteira | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |

Roteamento e consumo: `MessageGroupId = aggregateId` (ordem por transação e por carteira), `MessageDeduplicationId = eventId`. Consumidores devem ser idempotentes por `eventId`.
`LOSS` produz `WagerTransactionProcessed` e **não** produz `WalletBalanceChanged`. O payload é um snapshot imutável gravado em `JSONB`.

## 10. Autenticação e autorização

- **IdP:** **Keycloak** (Compose, realm `wallet` importado automaticamente). Escolhido por ser OIDC padrão, de código aberto, com JWKS público e importação de realm,
  o que torna os clientes de teste reproduzíveis. A aplicação **não** cadastra senhas nem emite tokens; serviço a serviço usa `client_credentials`.
- **Validação de credenciais:** local, sem chamar o IdP a cada requisição: assinatura **RS256** contra o JWKS (cache com atualização quando aparece um `kid` novo),
  `iss` exato, `aud` contendo `wallet-service` (mapper de audiência nos clientes do realm) e `exp` obrigatório (tolerância de relógio de 30 s, ajustável). Outros algoritmos (`HS256`, `none`) são recusados. IdP inalcançável → `503 IDENTITY_PROVIDER_UNAVAILABLE`, não `401`.
- **Modelo de permissões:** o claim `azp` (client id) **é** o `providerId` autorizado; papéis de realm `wallet-provider` e `wallet-internal`.

| Operação | Quem |
|---|---|
| `POST /wagering/transactions` | provedor (o `providerId` do corpo deve ser o do token, senão `403 PROVIDER_MISMATCH`) |
| `GET /wagering/transactions/{id}` e `GET /providers/{providerId}/...` | provedor só vê as próprias (as de outro dão `404`, inclusive em replays); serviço interno vê todas |
| `POST /wallets`, `GET /wallets/{id}`, ledger, reconciliação | somente serviço interno |
| `/health/*`, `/metrics` | públicos (em produção, restrinja `/metrics` por rede) |

Acessos negados não têm efeito financeiro nem expõem dados (testado). Não implementados: introspecção e revogação de tokens (a validade é limitada pelo `exp`, 5 min no realm).

- **Acesso à mensageria:** a fila de comandos é um canal **confiável**: o serviço aceita o `providerId` da mensagem, sem JWT por mensagem, e mantém **todas** as
  validações de domínio. Quem pode escrever nela é decidido pelo broker (IAM): produtores com `sqs:SendMessage` só em `wager-transactions.fifo`; este serviço com
  `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:ChangeMessageVisibility`, `sqs:GetQueueAttributes` nela e `sqs:SendMessage` na fila de eventos e nas DLQs; consumidores de eventos só leitura.
  **O LocalStack do Compose não aplica essa política** (qualquer credencial funciona): é limitação do ambiente local, não garantia.

## 11. Composição com Uber Fx e encerramento

`bootstrap.Module()` (`fx.Module`, `fx.Provide`, `fx.Invoke`): `LoadConfig` (valida; sem `OIDC_*` a aplicação não sobe) → `newDB` → `Service` (com observador de métricas) →
verificador de JWT → API → `Invoke(startWorker, startMessaging, startServer)`.

- **Início:** falha se o PostgreSQL estiver inalcançável, a porta ocupada ou o SQS não responder dentro do prazo (30 s).
- **Parada (ordem inversa):** o servidor HTTP deixa de aceitar conexões e conclui as requisições em andamento (`Shutdown` com prazo); consumidor SQS e publicador concluem o
  que estão fazendo; o worker de pendências é cancelado e **aguardado**; só então o pool do banco é fechado. Todo worker tem término observável (`WaitGroup`/canal) e prazo.
- `MESSAGING_ENABLED=false` desliga a mensageria (usado nos testes do ciclo de vida).

## 12. Observabilidade

Logs **JSON** (`slog`) com `correlationId` (cabeçalho `X-Correlation-Id` ou gerado, ecoado e persistido), `messageId`, `transactionId`, `walletId` e `providerId`; sem credenciais nem payloads
financeiros completos. Health: `/health/live` (processo) e `/health/ready` (PostgreSQL **e** as duas filas).

`GET /metrics` (formato Prometheus):

| Métrica | Significado |
|---|---|
| `wallet_wager_results_total{source,kind,status}` | resultados por origem (`http`/`sqs`), tipo e status |
| `wallet_wager_duplicates_total{source}` | repetições respondidas sem reprocessar |
| `wallet_wager_duration_seconds{source}` | histograma de latência |
| `wallet_concurrency_conflicts_total` | tentativas repetidas por deadlock/serialização/unicidade concorrente |
| `wallet_pending_retries_total` | retentativas de referências pendentes |
| `wallet_sqs_messages_total{result}` | `acked`, `duplicate`, `retried`, `dead_lettered` |
| `wallet_sqs_dlq_total` | mensagens que o consumidor enviou à DLQ |
| `wallet_outbox_published_total`, `wallet_outbox_publish_failures_total` | publicação da outbox |
| `wallet_outbox_lag_seconds`, `wallet_outbox_unpublished` | idade do evento mais antigo e quantidade pendente |
| `wallet_reconciliation_divergences_total` | divergências entre saldo e ledger |

Limite conhecido: mensagens movidas para a DLQ pelo *redrive* do próprio SQS acontecem dentro do broker e não são contadas por este serviço (o que se vê é cada `retried`).

## 13. Contrato HTTP: códigos e corpos

Erros: `{"error":{"code":"...","message":"..."}}`. Toda resposta carrega `X-Correlation-Id`.

| Situação | HTTP | Código / corpo |
|---|---:|---|
| Operação processada, ou replay de uma processada | `200` | `status: PROCESSED`, `balance`, `idempotentReplay` |
| Aguardando referência | `202` | `status: PENDING_REFERENCE`, `nextAttemptAt` |
| Carteira criada | `201` | dados da carteira |
| Entrada inválida / `Idempotency-Key` ausente | `400` | `INVALID_REQUEST`, `MISSING_IDEMPOTENCY_KEY` |
| Token ausente, inválido ou expirado | `401` | `UNAUTHORIZED` |
| Sem permissão / provedor diferente do token | `403` | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Carteira ou transação inexistente (ou de outro provedor) | `404` | `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND` |
| Conflito | `409` | `IDEMPOTENCY_KEY_CONFLICT`, `EXTERNAL_TRANSACTION_CONFLICT`, `WALLET_ALREADY_EXISTS` |
| Rejeição de negócio | `422` | `status: REJECTED`, `failureCode` |
| Erro interno | `500` | `INTERNAL_ERROR` |
| Indisponibilidade transitória (banco) ou do IdP | `503` | `SERVICE_UNAVAILABLE`, `IDENTITY_PROVIDER_UNAVAILABLE` (com `Retry-After`) |

Entrada inválida, conflito, rejeição de negócio, pendência e indisponibilidade são distinguíveis pelo código HTTP e pelo corpo.

## 14. Interpretações, limitações e trabalho não concluído

| Item | Situação |
|---|---|
| `FAILED` | Existe no domínio e no schema; **nenhum caminho o grava** (falhas de infraestrutura são sempre repetíveis) |
| Introspecção e revogação de token | Não implementadas (valem `exp` e assinatura) |
| Política IAM do SQS | Documentada; **não aplicada** no LocalStack. Não validada em AWS real |
| Execução contra a AWS real | Não testada; só LocalStack (e filas em memória nos testes rápidos) |
| Moeda | Valida o formato `[A-Z]{3}`, não a lista ISO 4217 |
| `PENDING` confirmado | Não existe: o aceite é síncrono (permitido pelo enunciado) |
| Ledger de partidas dobradas, OpenTelemetry, testes de carga | Opcionais; não feitos |
| `/metrics` | Sem autenticação (como `/health`); restringir por rede em produção |

Interpretações: `ROLLBACK` de `REFUND` é permitido e debita o jogador; uma aposta já reembolsada não aceita nova reversão (seção 5); referência existente porém pendente aguarda em vez de rejeitar (seção 6).

## 15. Como foi verificado

- **Unitários** (`go test ./...`): `Money`, carteira, máquina de estados, regras dos cinco tipos (inclusive zero), eventos, validação de JWT com IdP simulado (RSA/JWKS).
- **Integração** (`-tags integration`, PostgreSQL real): constraints, imutabilidade do ledger, atomicidade, idempotência, 50 envios idênticos em paralelo, 2 × 80,00 sobre 100,00, carteiras independentes,
  reversões concorrentes, pendências (resolução e expiração), inbox, reentrega, outbox com vários publicadores, ciclo de vida do Fx, isolamento entre provedores, códigos HTTP e
  **três processos do sistema operacional** disputando a mesma carteira, a mesma operação por **HTTP e SQS** (aplicada uma só vez, nas duas ordens) e **reinício** (novo pool e novo `Service`: idempotência e pendências preservadas).
- **Keycloak real** (`-tags keycloak`, inclusive token expirado: cliente `provider-short-lived` com validade de 3 s) e **SQS real** (`-tags localstack`: LocalStack com filas FIFO, DLQ e redrive).
- Rodam com `-race`. Os comandos estão no `README.md`.
