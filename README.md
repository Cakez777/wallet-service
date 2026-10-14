# Wallet Service

Serviço backend em Go para processar operações financeiras de provedores de jogos sobre carteiras de jogadores. O projeto foi desenvolvido como resposta ao [Backend Challenge da Jungle Gaming](https://github.com/junglegaming/backend-challenge-go).

O objetivo principal é manter o saldo correto mesmo diante de requisições repetidas, concorrência entre instâncias, reentregas de mensagens e falhas de infraestrutura.

> **Estado do projeto:** o núcleo financeiro, a API HTTP, a autenticação com Keycloak, a integração SQS com inbox/outbox, métricas e diferentes camadas de testes estão implementados no código. Existem limitações conhecidas, listadas em [Limitações conhecidas](#limitações-conhecidas) e detalhadas em [`ARCHITECTURE.md`](ARCHITECTURE.md). A documentação não deve ser interpretada como prova de que todos os testes de integração ou o ambiente completo foram executados em todas as máquinas.

## Conteúdo

- [Principais recursos](#principais-recursos)
- [Tecnologias](#tecnologias)
- [Pré-requisitos](#pré-requisitos)
- [Executar localmente](#executar-localmente)
- [Configuração](#configuração)
- [Autenticação de desenvolvimento](#autenticação-de-desenvolvimento)
- [Exemplos de uso](#exemplos-de-uso)
- [Endpoints](#endpoints)
- [Mensageria e observabilidade](#mensageria-e-observabilidade)
- [Migrations](#migrations)
- [Testes](#testes)
- [Estrutura do projeto](#estrutura-do-projeto)
- [Limitações conhecidas](#limitações-conhecidas)
- [Arquitetura](#arquitetura)

## Principais recursos

- **Precisão monetária:** valores representados em centavos por `int64`; cálculos financeiros não dependem de `float`.
- **Concorrência por carteira:** transações PostgreSQL, `SELECT ... FOR UPDATE`, controle de versão e constraints no banco.
- **Idempotência persistente:** chaves e identificadores externos protegidos por índices únicos; o estado persiste após reinícios.
- **Ledger imutável:** lançamentos guardam saldo anterior e posterior, com restrições no domínio e no PostgreSQL.
- **Operações de apostas:** `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK`, incluindo validação de referências e reversões.
- **Autenticação e autorização:** Keycloak por OAuth 2.0/OIDC, JWT assinado com RS256 e controle de acesso por papel/provedor.
- **Mensageria resiliente:** consumidor SQS, inbox transacional, outbox persistente, filas FIFO e dead-letter queues (DLQs).
- **Operação e diagnóstico:** health checks, logs JSON, correlation ID e métricas Prometheus.
- **Composição e encerramento:** dependências e ciclo de vida gerenciados por Uber Fx.

## Tecnologias

- Go — versão declarada em `go.mod`.
- PostgreSQL e migrations versionadas com `golang-migrate`.
- `database/sql` com `pgx/stdlib`.
- Uber Fx para composição e ciclo de vida.
- Keycloak para identidade OAuth 2.0/OIDC.
- AWS SQS e LocalStack para desenvolvimento local.
- Docker Compose para subir os serviços de apoio.

## Pré-requisitos

Instale Go compatível com `go.mod`, Docker e Docker Compose. Para os testes com tags `integration`, `keycloak` ou `localstack`, consulte as dependências específicas na seção [Testes](#testes).

## Executar localmente

Na raiz do repositório:

```bash
# Opcional: crie um arquivo local de ambiente a partir do exemplo.
cp .env.example .env

# Sobe o PostgreSQL, aplica as migrations, inicia o Keycloak e o LocalStack,
# e inicia a API.
docker compose up --build
```

A API fica disponível em `http://localhost:8080`. O console do Keycloak fica em `http://localhost:8081`; o LocalStack usa `http://localhost:4566` dentro do ambiente Compose.

O Keycloak pode levar alguns segundos para ficar pronto. Durante a inicialização, endpoints protegidos podem responder `503` enquanto a aplicação não consegue consultar as chaves públicas do provedor de identidade.

Para interromper os serviços, use `Ctrl+C`. Para removê-los:

```bash
docker compose down
```

> **Atenção:** `docker compose down -v` remove também volumes persistentes, incluindo os dados locais do PostgreSQL. Use esse comando apenas quando realmente quiser apagar o banco de desenvolvimento.

### Executar a API fora do Docker

Suba primeiro os serviços de apoio:

```bash
docker compose up -d postgres migrate keycloak localstack
```

Depois, configure no terminal as variáveis equivalentes às de `.env.example` e inicie a aplicação:

```bash
go run .
```

A configuração precisa apontar para o PostgreSQL e usar o `OIDC_ISSUER` e `OIDC_JWKS_URL` correspondentes ao Keycloak acessível pelo processo da API. Os endereços usados de dentro do Docker podem ser diferentes dos endereços usados no host.

## Configuração

Os valores padrão do ambiente local estão documentados em `.env.example` e no `docker-compose.yml`. As principais variáveis são:

| Variável | Finalidade | Valor padrão/observação |
|---|---|---|
| `DATABASE_URL` | Conexão com PostgreSQL | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` no host |
| `OIDC_ISSUER` | Valor esperado do claim `iss` | Obrigatória |
| `OIDC_JWKS_URL` | URL das chaves públicas JWKS | Obrigatória |
| `HTTP_ADDR` | Endereço de escuta da API | `:8080` |
| `PENDING_MAX_ATTEMPTS` | Tentativas antes de encerrar uma referência pendente | `8` |
| `PENDING_BASE_DELAY_MS` / `PENDING_MAX_DELAY_MS` | Intervalo inicial e limite do backoff das pendências | `2000` / `300000` ms |
| `WORKER_INTERVAL_MS` | Intervalo do worker de referências pendentes | `1000` ms |
| `SHUTDOWN_GRACE_SECONDS` | Prazo para encerramento do HTTP | `20` s |
| `MESSAGING_ENABLED` | Desliga consumidor, publicador e criação de filas quando `false` | `true` |
| `SQS_ENDPOINT` | Endpoint SQS alternativo, como o LocalStack | vazio usa endpoint AWS configurado pelo SDK |
| `AWS_REGION` | Região usada pelo SDK AWS | `us-east-1` no exemplo local |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Credenciais AWS/LocalStack | Credenciais de desenvolvimento no Compose |
| `COMMANDS_QUEUE_NAME` / `COMMANDS_DLQ_NAME` | Fila de comandos e respectiva DLQ | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` |
| `EVENTS_QUEUE_NAME` / `EVENTS_DLQ_NAME` | Fila de eventos e respectiva DLQ | `wallet-events.fifo` / `wallet-events-dlq.fifo` |
| `SQS_MAX_RECEIVE` | Entregas antes do redrive para DLQ | `5` |
| `SQS_VISIBILITY_SECONDS` | Período de visibilidade das mensagens | `60` s; mantenha acima do limite de processamento configurado |
| `CONSUMER_WORKERS` | Consumidores por instância | `4` |
| `OUTBOX_INTERVAL_MS` / `OUTBOX_BATCH` | Intervalo e tamanho do lote da outbox | `500` ms / `20` |

Consulte `.env.example` para a lista completa e os valores exatos da configuração do repositório. Não publique segredos reais nem use as credenciais de exemplo fora de um ambiente local.

## Autenticação de desenvolvimento

O Compose importa o realm de desenvolvimento `wallet` no Keycloak (`http://localhost:8081`). Os clientes de teste usam o fluxo `client_credentials`:

| Cliente | Segredo de desenvolvimento | Papel | Uso |
|---|---|---|---|
| `provider-a` | `provider-a-secret` | `wallet-provider` | Simula o provedor A |
| `provider-b` | `provider-b-secret` | `wallet-provider` | Simula o provedor B |
| `wallet-internal-service` | `internal-secret` | `wallet-internal` | Operações administrativas/internas de carteira |

Essas credenciais são **somente para desenvolvimento local**. Não as reutilize em ambientes reais.

### Obter um token com PowerShell

```powershell
$token = (Invoke-RestMethod -Method Post `
  -Uri 'http://localhost:8081/realms/wallet/protocol/openid-connect/token' `
  -Body @{
    grant_type    = 'client_credentials'
    client_id     = 'wallet-internal-service'
    client_secret = 'internal-secret'
  }).access_token
```

Para o cliente `provider-a`, troque `client_id` por `provider-a` e `client_secret` por `provider-a-secret`. Envie o token nas chamadas protegidas com `Authorization: Bearer <token>`.

> **Limite de segurança conhecido:** a aplicação valida a assinatura RS256 via JWKS, o emissor (`iss`) e a expiração (`exp`), mas a validação de audiência (`aud`) ainda não está implementada. Veja [`ARCHITECTURE.md`](ARCHITECTURE.md#autenticação-e-autorização). Isso deve ser considerado antes de usar o serviço em produção.

## Exemplos de uso

Os exemplos abaixo mostram o formato das chamadas. Substitua os identificadores de exemplo pelos IDs retornados pela sua própria execução.

### 1. Abrir carteira

Esta operação exige token com o papel `wallet-internal`:

```bash
curl -X POST 'http://localhost:8080/wallets' \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "initialBalance": {
      "amount": "1000.00",
      "currency": "BRL"
    }
  }'
```

### 2. Registrar uma aposta (`BET`)

Esta operação exige token de provedor e uma chave `Idempotency-Key`:

```bash
curl -X POST 'http://localhost:8080/wagering/transactions' \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -H 'Content-Type: application/json' \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "<WALLET_ID_RETORNADO_PELA_API>",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": {
      "amount": "25.00",
      "currency": "BRL"
    }
  }'
```

O formato monetário é uma string decimal com duas casas. Para `BET`, o valor precisa ser positivo e o saldo precisa ser suficiente. As regras completas de `WIN`, `LOSS`, `REFUND` e `ROLLBACK` estão em [`ARCHITECTURE.md`](ARCHITECTURE.md#regras-de-domínio-e-estados).

## Endpoints

| Método | Rota | Acesso/resumo |
|---|---|---|
| `GET` | `/health/live` | Público; verifica se o processo está vivo |
| `GET` | `/health/ready` | Público; verifica dependências de prontidão |
| `GET` | `/metrics` | Métricas Prometheus; restrinja por rede em ambientes reais |
| `POST` | `/wallets` | Papel interno; cria carteira |
| `GET` | `/wallets/{id}` | Papel interno; consulta carteira |
| `GET` | `/wallets/{id}/ledger?cursor=&limit=` | Papel interno; consulta lançamentos paginados |
| `POST` | `/wallets/{id}/reconciliation` | Papel interno; reconcilia saldo e ledger |
| `POST` | `/wagering/transactions` | Provedor autenticado; processa operação idempotente |
| `GET` | `/wagering/transactions/{id}` | Consulta transação conforme autorização implementada |
| `GET` | `/providers/{providerId}/wagering/transactions/{externalId}` | Provedor consulta transação do próprio escopo |

Códigos HTTP, contratos de erro e regras de autorização estão descritos em [`ARCHITECTURE.md`](ARCHITECTURE.md#contratos-http-e-códigos-de-resposta).

## Mensageria e observabilidade

O Compose sobe o LocalStack; na inicialização, a aplicação provisiona as filas FIFO e as respectivas DLQs. A fila de entrada recebe comandos de operação financeira (`wager-transactions.fifo`); a fila de saída publica eventos (`wallet-events.fifo`).

O consumidor registra a mensagem na inbox e processa o comando na mesma transação SQL. O publicador envia eventos persistidos na outbox após o commit. Essa estratégia permite recuperar mensagens/eventos após falhas sem depender apenas da deduplicação temporária do SQS.

Acesse as métricas em:

```text
http://localhost:8080/metrics
```

Para detalhes sobre IDs de grupo, deduplicação, retentativas, DLQs e ordem dos eventos, consulte a seção de mensageria de [`ARCHITECTURE.md`](ARCHITECTURE.md#mensageria-sqs-inbox-e-outbox).

## Migrations

As migrations ficam em `db/migrations` no formato `golang-migrate`, com arquivos `NNNNNN_nome.up.sql` e `NNNNNN_nome.down.sql`. O Compose aplica as migrations durante a inicialização.

Também é possível executá-las manualmente:

```bash
# Aplicar migrations pendentes
docker compose run --rm migrate up

# Reverter a última migration
docker compose run --rm migrate down 1
```

A reversão altera o schema; avalie o impacto antes de usá-la em dados que deseja preservar.

## Testes

### Testes comuns

```bash
go test ./...
go vet ./...
go test -race ./...
```

### Integração com PostgreSQL

Inicie o PostgreSQL e configure a variável de teste:

```bash
docker compose up -d postgres
export TEST_DATABASE_URL='postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'
go test -tags integration -race -count=1 ./...
```

No PowerShell, use:

```powershell
docker compose up -d postgres
$env:TEST_DATABASE_URL = 'postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'
go test -tags integration -race -count=1 ./...
```

### Integrações opcionais

```bash
# Requer Keycloak ativo
go test -tags keycloak -count=1 ./internal/auth/

# Requer PostgreSQL e LocalStack ativos
go test -tags localstack -count=1 -v ./internal/inbound/
```

Os testes estão organizados em camadas: unitários, integração com PostgreSQL, fluxo contra Keycloak e fluxo de mensageria contra LocalStack. A existência de um teste ou de um comando neste documento não significa que ele tenha sido executado com sucesso no seu ambiente; registre separadamente quais comandos foram realmente executados e seus resultados.

## Estrutura do projeto

```text
.
├── main.go
├── internal/
│   ├── money/       # Valor monetário e operações seguras
│   ├── wallet/      # Domínio de carteira e ledger
│   ├── wagering/    # Comandos, transações e regras de apostas
│   ├── app/         # Casos de uso e persistência PostgreSQL
│   ├── auth/        # Validação JWT/JWKS
│   ├── httpapi/     # Handlers HTTP, contratos e autorização
│   ├── inbound/     # Consumidor SQS e inbox
│   ├── outbox/      # Publicação de eventos persistidos
│   ├── messaging/   # Abstração de mensageria e adaptadores
│   ├── metrics/     # Métricas Prometheus
│   └── bootstrap/   # Composição com Uber Fx e ciclo de vida
├── db/migrations/  # Schema versionado
├── docker-compose.yml
├── dockerfile
├── .env.example
├── README.md
└── ARCHITECTURE.md
```

## Limitações conhecidas

- A validação do claim `aud` (audiência), introspecção/revogação de tokens e rotação de segredos não estão implementadas.
- O estado `FAILED` existe no domínio e no schema, mas nenhum fluxo atual o persiste.
- A política IAM de acesso às filas está documentada, mas não é aplicada pelo LocalStack do Compose.
- A integração com AWS SQS em uma conta AWS real não foi validada como parte do ambiente local documentado.
- Tracing distribuído com OpenTelemetry, partidas dobradas e testes de carga não fazem parte do escopo implementado neste momento.

Essas limitações não anulam as decisões de concorrência, ledger, idempotência ou recuperação descritas no código, mas precisam ser levadas em conta ao avaliar o grau de prontidão para produção.

## Arquitetura

O documento [`ARCHITECTURE.md`](ARCHITECTURE.md) explica os limites de cada camada, invariantes financeiros, idempotência, estados das operações, autenticação, mensageria, observabilidade, códigos HTTP e limitações do projeto.
