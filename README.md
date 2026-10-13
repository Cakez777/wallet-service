# Wallet Service

Serviço em Go (Uber Fx, PostgreSQL, Keycloak) que processa operações financeiras de provedores de
jogos — `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK` — sobre carteiras de jogadores, com idempotência
persistente, ledger imutável e coordenação por carteira. Resposta ao desafio backend da Jungle Gaming.

> **Estado:** núcleo financeiro, API, autenticação e testes concluídos. **Ainda não implementado:**
> SQS (consumidor e inbox em uso), publicador da outbox, métricas. Veja [ARCHITECTURE.md](ARCHITECTURE.md),
> que lista decisões, interpretações e todo o trabalho não concluído.

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
com vários workers), imutabilidade e invariantes impostas pelo banco, isolamento entre provedores,
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
internal/auth       validação de JWT (JWKS)
internal/httpapi    handlers, autenticação/autorização, contratos
internal/bootstrap  composição com Uber Fx e ciclo de vida
db/migrations       schema; deploy/keycloak  realm importado
```
