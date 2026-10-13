# Wallet Service

Serviço em Go (Uber Fx, PostgreSQL, Keycloak) que processa operações financeiras de provedores de
jogos — `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK` — sobre carteiras de jogadores, com idempotência
persistente, ledger imutável e coordenação por carteira. Resposta ao desafio backend da Jungle Gaming.

> **Estado:** núcleo financeiro, API, autenticação, SQS (consumidor + inbox), publicador da outbox, métricas e testes.
> Veja [ARCHITECTURE.md](ARCHITECTURE.md) para as decisões, interpretações e o que **não** foi feito.

## Pré-requisitos

Go (versão do `go.mod`), Docker e Docker Compose.

## Executar

```
cp .env.example .env      # opcional; o compose já define as variáveis do app
go mod tidy               # gera/atualiza go.sum (necessário uma vez após clonar)
docker compose up --build
```

Sobe PostgreSQL, aplica as migrations (`migrate`), sobe o Keycloak (realm `wallet` importado) e a API
em `http://localhost:8080`. O Keycloak leva alguns segundos; até lá a API responde `503` aos endpoints
autenticados (não consegue validar tokens).

Fora do Docker: `docker compose up -d postgres migrate keycloak`, depois `go run .` com as variáveis de
`.env.example` exportadas.

## Variáveis de ambiente

| Variável | Descrição | Padrão |
|---|---|---|
| `DATABASE_URL` | PostgreSQL | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` |
| `OIDC_ISSUER` | valor exato do claim `iss` | **obrigatória** |
| `OIDC_JWKS_URL` | chaves públicas do IdP | **obrigatória** |
| `HTTP_ADDR` | endereço do servidor | `:8080` |
| `PENDING_MAX_ATTEMPTS` | tentativas antes de rejeitar uma referência ausente | `8` |
| `PENDING_BASE_DELAY_MS` / `PENDING_MAX_DELAY_MS` | backoff exponencial | `2000` / `300000` |
| `WORKER_INTERVAL_MS` | intervalo do worker de pendências | `1000` |
| `SHUTDOWN_GRACE_SECONDS` | prazo do shutdown do HTTP | `20` |
| `MESSAGING_ENABLED` | `false` desliga consumidor, publicador e criação de filas | `true` |
| `SQS_ENDPOINT` | LocalStack (`http://localstack:4566`); vazio = AWS real | vazio |
| `AWS_REGION` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | região e credenciais do SDK | `us-east-1` / — / — |
| `COMMANDS_QUEUE_NAME` / `COMMANDS_DLQ_NAME` | fila de comandos e sua DLQ | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` |
| `EVENTS_QUEUE_NAME` / `EVENTS_DLQ_NAME` | fila de eventos e sua DLQ | `wallet-events.fifo` / `wallet-events-dlq.fifo` |
| `SQS_MAX_RECEIVE` | entregas antes de a fila mover a mensagem para a DLQ | `5` |
| `SQS_VISIBILITY_SECONDS` | visibilidade (deve ser > 30) | `60` |
| `CONSUMER_WORKERS` | goroutines de consumo por instância | `4` |
| `OUTBOX_INTERVAL_MS` / `OUTBOX_BATCH` | cadência e tamanho de lote do publicador | `500` / `20` |

## Autenticação (fluxos de teste)

O Keycloak do compose (`http://localhost:8081`, admin `admin/admin`) tem clientes `client_credentials`:

| Cliente | Segredo | Papel | Uso |
|---|---|---|---|
| `provider-a` | `provider-a-secret` | `wallet-provider` | provedor A (o `clientId` é o `providerId`) |
| `provider-b` | `provider-b-secret` | `wallet-provider` | provedor B |
| `wallet-internal-service` | `internal-secret` | `wallet-internal` | operações de carteira |

Obter um token (PowerShell):

```
$t = (Invoke-RestMethod -Method Post -Uri http://localhost:8081/realms/wallet/protocol/openid-connect/token `
  -Body @{grant_type='client_credentials'; client_id='wallet-internal-service'; client_secret='internal-secret'}).access_token
```

Com `curl`: `curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret http://localhost:8081/realms/wallet/protocol/openid-connect/token`.

## Exemplos de chamadas

```
# abrir carteira (token interno)
curl -X POST localhost:8080/wallets -H "Authorization: Bearer $INTERNAL" -H "Content-Type: application/json" \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'

# apostar (token do provedor A)
curl -X POST localhost:8080/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" \
  -H "Idempotency-Key: provider-a:transaction-123" -H "Content-Type: application/json" \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<id>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Endpoints: `POST /wallets`, `GET /wallets/{id}`, `GET /wallets/{id}/ledger?cursor=&limit=`,
`POST /wallets/{id}/reconciliation` (papel interno); `POST /wagering/transactions`,
`GET /wagering/transactions/{id}`, `GET /providers/{providerId}/wagering/transactions/{externalId}`
(provedor: só as suas); `GET /health/live`, `GET /health/ready` (públicos). Códigos HTTP e corpos de erro
estão no ARCHITECTURE.md.

## Mensageria (SQS) e métricas

`docker compose up --build` sobe também o LocalStack; o app cria as filas FIFO e as DLQs na inicialização.
`/health/ready` só fica `ok` com PostgreSQL e as duas filas respondendo. Métricas: `GET http://localhost:8080/metrics`.

Enviar um comando pela fila (PowerShell; troque `WALLET_ID`/`PLAYER_ID` pelos valores da carteira criada):

```powershell
@'
{"providerId":"provider-a","idempotencyKey":"sqs-key-1","externalTransactionId":"sqs-ext-1","playerId":"PLAYER_ID","walletId":"WALLET_ID","roundId":"r1","gameId":"g1","kind":"BET","money":{"amount":"10.00","currency":"BRL"}}
'@ | Set-Content -Encoding ascii msg.json
docker cp msg.json wallet_localstack:/tmp/msg.json
docker exec wallet_localstack awslocal sqs send-message --queue-url http://localhost:4566/000000000000/wager-transactions.fifo --message-group-id WALLET_ID --message-deduplication-id sqs-key-1 --message-body file:///tmp/msg.json
```

Depois consulte o saldo pela API e leia os eventos publicados:

```powershell
docker exec wallet_localstack awslocal sqs receive-message --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 5
docker exec wallet_localstack awslocal sqs receive-message --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo
```

Reenviar a mesma mensagem (outro `--message-deduplication-id`, mesma `idempotencyKey`) não debita de novo: a inbox a reconhece.

## Migrations

`db/migrations`, formato golang-migrate (`NNNNNN_nome.up.sql` / `.down.sql`). Aplicadas pelo `docker compose up`.

```
docker compose run --rm migrate up          # aplicar
docker compose run --rm migrate down 1      # reverter a última
```

Mudou o schema? Recrie o volume: `docker compose down -v`.

## Testes

```
go test ./...             # unitários: money, wallet, wagering, auth (IdP simulado com RSA/JWKS)
go vet ./...
go test -race ./...
```

Integração (PostgreSQL **real**; cada execução cria e remove um banco próprio):

```
docker compose up -d postgres
export TEST_DATABASE_URL='postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'   # PowerShell: $env:TEST_DATABASE_URL=...
go test -tags integration -race -count=1 ./...
```

Cobrem: 50 envios idênticos em paralelo (um único débito), duas apostas de 80.00 sobre 100.00,
carteiras independentes em paralelo, reversões simultâneas, referências pendentes (resolução e expiração,
com vários workers), consumidor SQS (entregas duplicadas, duas instâncias na mesma carteira sem saldo negativo e em ordem, mensagens veneno → DLQ,
falha antes do commit, falha no delete após o commit, encerramento gracioso), publicador da outbox (vários publicadores sem publicação dupla,
ordem por agregado, fila fora do ar e recuperação, concessão expirada), imutabilidade e invariantes impostas pelo banco, isolamento entre provedores,
códigos HTTP, e o ciclo de vida do Fx (início, falha de config/banco/porta, encerramento e liberação de recursos).

Autenticação contra o **Keycloak real** (exige `docker compose up -d keycloak`):

```
go test -tags keycloak -count=1 ./internal/auth/
```

> As três "instâncias independentes" dos testes são três `Service` com pools de conexão próprios no mesmo
> processo. Rodar três processos do binário contra o mesmo banco funciona pelo mesmo mecanismo (estado só no
> PostgreSQL), mas esse cenário não está automatizado.

## Estrutura

```
internal/money      value object Money (centavos int64 + moeda)
internal/wallet     agregado Wallet e LedgerEntry (domínio puro)
internal/wagering   comando, WagerTransaction, estados, regras (Plan) e eventos (domínio puro)
internal/app        casos de uso e acesso ao PostgreSQL
internal/inbound    consumidor SQS + inbox transacional
internal/outbox     publicador da outbox (lease, SKIP LOCKED, backoff)
internal/messaging  abstração de fila, adaptador SQS (AWS SDK v2) e fila FIFO em memória (testes)
internal/metrics    métricas em formato Prometheus
internal/auth       validação de JWT (JWKS)
internal/httpapi    handlers, autenticação/autorização, contratos
internal/bootstrap  composição com Uber Fx e ciclo de vida
db/migrations       schema; deploy/keycloak  realm importado
```
