# Wallet Service

Serviço backend em Go para processar operações financeiras de provedores de jogos sobre carteiras de jogadores. O projeto foi desenvolvido como resposta ao [Backend Challenge da Jungle Gaming](https://github.com/junglegaming/backend-challenge-go).

O objetivo é manter a integridade do saldo diante de requisições repetidas, concorrência entre instâncias, reentregas de mensagens e falhas de infraestrutura.

> **Estado do projeto:** o núcleo financeiro, a API HTTP, a autenticação OAuth 2.0/OIDC com Keycloak, a integração SQS com inbox/outbox, métricas e testes em diferentes camadas estão implementados. Os comandos de validação e os resultados efetivamente executados estão em [`docs/VALIDATION.md`](docs/VALIDATION.md). As limitações conhecidas estão listadas abaixo e detalhadas em [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Conteúdo

- [Recursos](#recursos)
- [Tecnologias](#tecnologias)
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

## Recursos

- **Precisão monetária:** valores armazenados como centavos em `int64`; os cálculos não dependem de ponto flutuante.
- **Validação de moeda:** códigos reconhecidos pela lista ISO 4217, com rejeição de códigos inválidos e de `XXX`.
- **Concorrência por carteira:** transações PostgreSQL, `SELECT ... FOR UPDATE`, controle de versão e constraints no banco.
- **Idempotência persistente:** chaves e identificadores externos protegidos por índices únicos; os resultados permanecem após reinícios.
- **Ledger imutável:** lançamentos com saldos anterior e posterior, protegidos por constraints e triggers.
- **Operações financeiras:** `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK`, com validação de referências.
- **Autenticação e autorização:** Keycloak via OAuth 2.0/OIDC, JWT RS256 validado com JWKS, verificação de `iss`, `exp` e `aud`, e autorização por papel/provedor.
- **Mensageria resiliente:** consumidor SQS, inbox transacional, outbox persistente, filas FIFO, retry e dead-letter queues (DLQs).
- **Operação e diagnóstico:** health checks, logs estruturados, correlation ID e métricas Prometheus.
- **Composição e encerramento:** dependências e ciclo de vida gerenciados por Uber Fx.

## Tecnologias

| Responsabilidade | Tecnologia |
|---|---|
| Linguagem | Go `1.27.2`, declarado em `go.mod` |
| Dependências | Go Modules (`go.mod`, `go.sum`) |
| Composição | Uber Fx |
| HTTP | `net/http` |
| Autenticação | OAuth 2.0/OIDC com Keycloak |
| Persistência | PostgreSQL e `database/sql` com `pgx/stdlib` |
| Mensageria | AWS SQS; LocalStack para desenvolvimento e testes locais |
| Migrations | `golang-migrate`, com arquivos versionados `.up.sql` e `.down.sql` |
| Ambiente local | Docker e Docker Compose |
| Testes | `testing`, `go test`, tags de integração e detector `-race` |

## Pré-requisitos

- Go `1.27.2` ou versão compatível com `go.mod`.
- Docker e Docker Compose.
- Para testes com `-race`, CGO habilitado e um compilador C suportado. No Windows, o teste foi executado com GCC do MSYS2 UCRT64.

## Executar localmente

Na raiz do repositório:

```bash
# Opcional: crie configurações locais a partir do exemplo.
cp .env.example .env

# Inicia PostgreSQL, migrations, Keycloak, LocalStack e a API.
docker compose up --build
```

A API fica disponível em `http://localhost:8080`; o Keycloak em `http://localhost:8081`; o endpoint local do LocalStack em `http://localhost:4566`.

O Keycloak pode levar alguns segundos para ficar pronto. Durante a inicialização, endpoints protegidos podem responder `503` enquanto a aplicação não consegue validar tokens com as chaves públicas do provedor.

Para interromper os serviços, use `Ctrl+C`. Para remover os containers e a rede:

```bash
docker compose down
```

> **Atenção:** `docker compose down -v` remove volumes persistentes, incluindo os dados locais do PostgreSQL. Use-o somente se realmente quiser apagar o banco local.

### Executar a API fora do Docker

Suba os serviços de apoio:

```bash
docker compose up -d postgres migrate keycloak localstack
```

Configure as variáveis equivalentes às de `.env.example` no terminal e inicie a API:

```bash
go run .
```

Os endereços de PostgreSQL, OIDC e SQS devem ser acessíveis a partir do processo da API. Endereços válidos dentro da rede do Docker podem diferir dos endereços acessíveis no host.

## Configuração

Os valores padrão do ambiente local estão em `.env.example` e `docker-compose.yml`. Principais variáveis:

| Variável | Finalidade | Padrão/observação |
|---|---|---|
| `DATABASE_URL` | Conexão PostgreSQL | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` no host |
| `OIDC_ISSUER` | Emissor esperado no claim `iss` | Obrigatória |
| `OIDC_JWKS_URL` | URL das chaves públicas JWKS | Obrigatória |
| `HTTP_ADDR` | Endereço HTTP | `:8080` |
| `PENDING_MAX_ATTEMPTS` | Tentativas de resolver referência pendente | `8` no exemplo |
| `PENDING_BASE_DELAY_MS` / `PENDING_MAX_DELAY_MS` | Backoff das referências pendentes | `2000` / `300000` ms no exemplo |
| `WORKER_INTERVAL_MS` | Intervalo do worker de referências | `1000` ms no exemplo |
| `SHUTDOWN_GRACE_SECONDS` | Prazo para encerrar HTTP | `20` s no exemplo |
| `MESSAGING_ENABLED` | Liga/desliga consumidor, publicador e provisionamento de filas | `true` |
| `SQS_ENDPOINT` | Endpoint SQS alternativo, por exemplo LocalStack | Vazio usa endpoint configurado pelo SDK |
| `AWS_REGION` | Região AWS SDK | `us-east-1` no exemplo |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Credenciais AWS/LocalStack | Somente credenciais de desenvolvimento no Compose |
| `COMMANDS_QUEUE_NAME` / `COMMANDS_DLQ_NAME` | Fila de comandos e DLQ | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` |
| `EVENTS_QUEUE_NAME` / `EVENTS_DLQ_NAME` | Fila de eventos e DLQ | `wallet-events.fifo` / `wallet-events-dlq.fifo` |
| `SQS_MAX_RECEIVE` | Entregas antes do redrive | `5` no exemplo |
| `SQS_VISIBILITY_SECONDS` | Visibilidade da mensagem | `60` s no exemplo; deve exceder o tempo de processamento esperado |
| `CONSUMER_WORKERS` | Consumidores por instância | `4` no exemplo |
| `OUTBOX_INTERVAL_MS` / `OUTBOX_BATCH` | Intervalo/tamanho de lote da Outbox | `500` ms / `20` no exemplo |

Consulte `.env.example` para a lista exata e completa. As credenciais incluídas no realm e no Compose são **somente para desenvolvimento local**; não as reutilize em produção.

## Autenticação de desenvolvimento

O Compose importa o realm `wallet` no Keycloak (`http://localhost:8081`). Os clientes locais usam o fluxo `client_credentials`:

| Cliente | Segredo local | Papel | Uso |
|---|---|---|---|
| `provider-a` | `provider-a-secret` | `wallet-provider` | Simula o provedor A |
| `provider-b` | `provider-b-secret` | `wallet-provider` | Simula o provedor B |
| `wallet-internal-service` | `internal-secret` | `wallet-internal` | Operações internas de carteira |

| `provider-short-lived` | `short-lived-secret` | `wallet-provider` | Token de 3 s de validade, usado só no teste de token expirado |

**Provisionamento automático:** o realm, os clientes, os papéis e as contas de serviço estão em `deploy/keycloak/realm-wallet.json` e são importados pelo Keycloak na inicialização do Compose; não há passo manual. Se o realm mudar, recrie o volume (`docker compose down -v`, apaga os dados locais) para reimportá-lo.

Esses valores pertencem exclusivamente ao ambiente local. Não publique nem reutilize segredos reais.

### Obter token com PowerShell

```powershell
$token = (Invoke-RestMethod -Method Post `
  -Uri 'http://localhost:8081/realms/wallet/protocol/openid-connect/token' `
  -Body @{
    grant_type    = 'client_credentials'
    client_id     = 'wallet-internal-service'
    client_secret = 'internal-secret'
  }).access_token
```

Para um token de provedor, use `client_id = 'provider-a'` e `client_secret = 'provider-a-secret'`. Envie o token nas requisições protegidas com `Authorization: Bearer <token>`.

A validação implementada verifica assinatura RS256 por JWKS, emissor (`iss`), expiração (`exp`), audiência (`aud`) e coerência dos claims de cliente usados na autorização. Introspecção/revogação de tokens e rotação operacional de segredos continuam fora do escopo implementado.

## Exemplos de uso

Os exemplos apresentam os contratos básicos. Substitua os identificadores pelos valores retornados pela sua execução.

### 1. Abrir carteira

Exige token com papel `wallet-internal`:

```bash
curl -X POST 'http://localhost:8080/wallets' \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "initialBalance": {"amount": "1000.00", "currency": "BRL"}
  }'
```

### 2. Registrar uma aposta (`BET`)

Exige token de provedor e `Idempotency-Key`:

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
    "money": {"amount": "25.00", "currency": "BRL"}
  }'
```

O valor monetário é uma string decimal com exatamente duas casas. `BET`, `WIN`, `REFUND` e `ROLLBACK` exigem valor positivo; `LOSS` exige `0.00` e não movimenta saldo. As regras de referência e reversão estão em [`ARCHITECTURE.md`](ARCHITECTURE.md#regras-de-domínio-e-estados).

### Fluxo autenticado completo no PowerShell (Windows)

No PowerShell, `curl` é um apelido de outro comando; use os comandos abaixo (ou `curl.exe`). Com a pilha no ar (`docker compose up --build -d`):

```powershell
# Tokens: serviço interno (carteiras) e provedor A (operações)
$tokenUrl = 'http://localhost:8081/realms/wallet/protocol/openid-connect/token'
$int  = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{ grant_type='client_credentials'; client_id='wallet-internal-service'; client_secret='internal-secret' }).access_token
$prov = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{ grant_type='client_credentials'; client_id='provider-a'; client_secret='provider-a-secret' }).access_token

# Carteira com 100.00 BRL
$playerId = [guid]::NewGuid().ToString()
$wallet = Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wallets' `
  -Headers @{ Authorization = "Bearer $int" } -ContentType 'application/json' `
  -Body (@{ playerId=$playerId; initialBalance=@{ amount='100.00'; currency='BRL' } } | ConvertTo-Json)

# BET de 25.00 (repita o último comando para ver idempotentReplay = true)
$bet = @{ providerId='provider-a'; externalTransactionId='tx-1'; playerId=$playerId; walletId=$wallet.id
          roundId='round-1'; gameId='fortune-chimp'; kind='BET'; money=@{ amount='25.00'; currency='BRL' } } | ConvertTo-Json
$h = @{ Authorization = "Bearer $prov"; 'Idempotency-Key' = 'provider-a:tx-1' }
Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wagering/transactions' -Headers $h -ContentType 'application/json' -Body $bet

# Saldo, ledger e reconciliação (somente serviço interno)
$ih = @{ Authorization = "Bearer $int" }
(Invoke-RestMethod -Uri "http://localhost:8080/wallets/$($wallet.id)" -Headers $ih).balance
Invoke-RestMethod -Uri "http://localhost:8080/wallets/$($wallet.id)/ledger?limit=50" -Headers $ih
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets/$($wallet.id)/reconciliation" -Headers $ih
```

Resultados esperados: saldo `75.00` após a aposta, `idempotentReplay: true` na repetição, e reconciliação com `consistent: true`. Verificações de autorização:

```powershell
# Token de provedor não cria carteira: 403 Forbidden
try { Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wallets' -Headers @{ Authorization = "Bearer $prov" } -ContentType 'application/json' -Body (@{ playerId=[guid]::NewGuid().ToString(); initialBalance=@{ amount='1.00'; currency='BRL' } } | ConvertTo-Json) } catch { $_.Exception.Response.StatusCode }
# Sem token: 401 Unauthorized
try { Invoke-RestMethod -Uri "http://localhost:8080/wallets/$($wallet.id)" } catch { $_.Exception.Response.StatusCode }
```

Para uma reversão, envie `kind` `REFUND` ou `ROLLBACK` com `referenceExternalTransactionId` apontando para a transação original; se ela ainda não existir, a resposta é `202` com `PENDING_REFERENCE` e o serviço retoma a operação quando a referência chegar.

### Enviar uma operação pelo SQS (LocalStack)

A mesma operação pode entrar pela fila de comandos. O corpo segue o envelope oficial; `data.idempotencyKey` é a chave de idempotência. Continuando o fluxo acima:

```powershell
@"
{"messageId":"msg-1","type":"WagerTransactionRequested","occurredAt":"2026-10-14T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"tx-sqs-1","idempotencyKey":"provider-a:tx-sqs-1","playerId":"$playerId","walletId":"$($wallet.id)","roundId":"round-2","gameId":"fortune-chimp","kind":"BET","money":{"amount":"10.00","currency":"BRL"}}}
"@ | Set-Content -Encoding ascii msg.json
docker cp msg.json wallet_localstack:/tmp/msg.json
docker exec wallet_localstack awslocal sqs send-message --queue-url http://localhost:4566/000000000000/wager-transactions.fifo --message-group-id $wallet.id --message-deduplication-id sqs-key-1 --message-body file:///tmp/msg.json
Start-Sleep -Seconds 3
(Invoke-RestMethod -Uri "http://localhost:8080/wallets/$($wallet.id)" -Headers $ih).balance    # 65.00
Remove-Item msg.json
```

Reenviar a mesma mensagem (com outro `--message-deduplication-id`) não debita de novo: a inbox a reconhece e o contador `wallet_sqs_messages_total{result="duplicate"}` em `/metrics` aumenta. Os eventos publicados ficam em `wallet-events.fifo`:

```powershell
docker exec wallet_localstack awslocal sqs receive-message --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10
```

## Endpoints

| Método | Rota | Acesso/resumo |
|---|---|---|
| `GET` | `/health/live` | Público; verifica se o processo está vivo |
| `GET` | `/health/ready` | Público; verifica prontidão das dependências |
| `GET` | `/metrics` | Métricas Prometheus; restrinja por rede em ambiente real |
| `POST` | `/wallets` | Papel interno; cria carteira |
| `GET` | `/wallets/{id}` | Papel interno; consulta carteira |
| `GET` | `/wallets/{id}/ledger?cursor=&limit=` | Papel interno; consulta lançamentos paginados |
| `POST` | `/wallets/{id}/reconciliation` | Papel interno; reconcilia saldo e ledger |
| `POST` | `/wagering/transactions` | Provedor autenticado; processa operação idempotente |
| `GET` | `/wagering/transactions/{id}` | Consulta transação conforme autorização implementada |
| `GET` | `/providers/{providerId}/wagering/transactions/{externalId}` | Provedor consulta transação do próprio escopo |

Códigos HTTP, payloads de erro e regras de autorização são detalhados em [`ARCHITECTURE.md`](ARCHITECTURE.md#contratos-http-e-códigos-de-resposta).

## Mensageria e observabilidade

O Compose inicia o LocalStack. A aplicação provisiona duas filas FIFO e suas DLQs:

- `wager-transactions.fifo`: fila de comandos de operação financeira.
- `wallet-events.fifo`: fila de eventos de domínio publicados pela Outbox.

O consumidor registra a mensagem na Inbox e processa o comando na mesma transação SQL. O publicador envia os eventos persistidos na Outbox após o commit. A idempotência durável depende do PostgreSQL, e não somente da janela de deduplicação do SQS.

Métricas Prometheus ficam em `http://localhost:8080/metrics`. Para detalhes sobre ordenação, retry, DLQ, lease da Outbox e encerramento, consulte [`ARCHITECTURE.md`](ARCHITECTURE.md#mensageria-sqs-inbox-e-outbox).

## Migrations

As migrations ficam em `db/migrations`, usando `golang-migrate` e os arquivos `NNNNNN_nome.up.sql` e `NNNNNN_nome.down.sql`. O Compose aplica migrations durante a inicialização.

```bash
# Aplicar migrations pendentes
docker compose run --rm migrate up

# Reverter a última migration
docker compose run --rm migrate down 1
```

Reverter migrations altera o schema. Faça isso somente depois de avaliar o impacto e de confirmar que está usando um banco de desenvolvimento/teste.

## Testes

Os testes são organizados por nível; as tags de integração devem ser executadas separadamente para garantir que a dependência necessária esteja disponível.

### Testes rápidos e análise estática

```bash
go test ./...
go vet ./...
go test -race ./...
```

### PostgreSQL e integração de aplicação

Inicie o PostgreSQL e defina a URL do banco de teste:

```bash
docker compose up -d postgres
export TEST_DATABASE_URL='postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'
go test -tags integration -race -count=1 ./...
```

No PowerShell do Windows:

```powershell
docker compose up -d postgres
$env:TEST_DATABASE_URL = 'postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'
$env:CGO_ENABLED = '1'
$env:CC = 'gcc'
# Se o GCC do MSYS2 UCRT64 não estiver no PATH permanente:
$env:Path = "C:\msys64\ucrt64\bin;$env:Path"
go test -tags integration -race -count=1 ./...
```

### Keycloak real

Com o Keycloak de desenvolvimento iniciado:

```bash
docker compose up -d keycloak
go test -tags keycloak -race -count=1 -v ./internal/auth/
```

### SQS real com LocalStack

Com PostgreSQL e LocalStack disponíveis:

```bash
docker compose up -d postgres localstack
go test -tags localstack -race -count=1 -v ./internal/inbound/
```

O teste `TestSQSDeleteFailureAfterCommitIsHarmless` simula a falha inicial de exclusão **depois do commit**, usando o SQS do LocalStack para a entrega e reentrega. O teste valida que a Inbox reconhece a mensagem repetida e impede um segundo débito.

### Formatação e todos os testes de uma vez

```bash
gofmt -l .        # não deve listar nenhum arquivo
go vet ./...
```

Com PostgreSQL, Keycloak e LocalStack no ar (`docker compose up -d postgres keycloak localstack`) e `TEST_DATABASE_URL` definida, todas as camadas, com cobertura combinada (no PowerShell, mantenha as aspas em `"-coverprofile=cover.out"`):

```powershell
go test -tags "integration keycloak localstack" -race -count=1 -coverpkg=./... "-coverprofile=cover.out" ./...
go tool cover "-func=cover.out" | Select-Object -Last 1
```

Se o realm do Keycloak mudou desde a última execução, recrie o volume (`docker compose down -v`) antes de subir os serviços, para que o cliente `provider-short-lived` exista.

### Preparar as dependências dos testes

| Camada | Tag | Precisa de | Variável |
|---|---|---|---|
| Unitários | nenhuma | apenas Go | — |
| Integração (PostgreSQL real, fila em memória) | `integration` | `docker compose up -d postgres` | `TEST_DATABASE_URL` |
| IdP real | `keycloak` | `docker compose up -d keycloak` | `KEYCLOAK_URL` (padrão `http://localhost:8081`) |
| SQS real | `localstack` | `docker compose up -d postgres localstack` | `TEST_DATABASE_URL` |

Cada teste de integração cria um banco temporário no servidor apontado por `TEST_DATABASE_URL`, aplica as migrations e o remove ao final; portanto os testes não alteram o banco da aplicação.

### Múltiplas instâncias e simulações de falha

Cenários executáveis individualmente (com `-tags integration`, ou `-tags localstack` onde indicado):

| Cenário | Comando |
|---|---|
| 3 processos do sistema operacional disputam a mesma carteira (100.00 com duas apostas de 80.00; mesma aposta enviada pelos 3) | `go test -tags integration -race -count=1 -run "TestThreeProcesses" -v ./internal/app/` |
| Mesma aposta 50 vezes em paralelo; duas apostas de 80.00; carteiras independentes | `go test -tags integration -race -count=1 -run "TestSameBet50TimesInParallel\|TestTwoBetsOf80On100\|TestIndependentWalletsInParallel" -v ./internal/app/` |
| Dois consumidores na mesma carteira, sem saldo negativo | `go test -tags integration -race -count=1 -run "TestTwoInstancesConsume" -v ./internal/inbound/` |
| Falha antes do commit (retry) e depois do commit, antes de apagar a mensagem (reentrega) | `go test -tags integration -race -count=1 -run "TestFailureBeforeCommit\|TestDeleteFailureAfterCommit" -v ./internal/inbound/` |
| Mesma operação por HTTP e por SQS | `go test -tags integration -race -count=1 -run "TestSameOperationViaHTTPAndSQS" -v ./internal/inbound/` |
| Reinício: idempotência e referências pendentes preservadas | `go test -tags integration -race -count=1 -run "TestRestartKeeps" -v ./internal/inbound/` |
| Referência chega depois / expira | `go test -tags integration -race -count=1 -run "TestPendingReference" -v ./internal/app/` |
| Vários publishers, publisher que morre (lease), broker fora do ar | `go test -tags integration -race -count=1 -v ./internal/outbox/` |
| Encerramento gracioso (Fx e consumidor) | `go test -tags integration -race -count=1 -run "TestLifecycle\|TestGracefulStop" -v ./internal/bootstrap/ ./internal/inbound/` |
| DLQ, redrive e falha de exclusão no SQS real | `go test -tags localstack -race -count=1 -v ./internal/inbound/` |

Para exercitar várias instâncias da API manualmente, deixe o Compose rodando e inicie outra instância no host, apontando para os mesmos PostgreSQL, Keycloak e LocalStack (variáveis de `.env.example`), com outra porta:

```powershell
$env:HTTP_ADDR = ':8082'
go run .
```

As duas instâncias compartilham o banco e as filas: o lock da carteira, a idempotência, a inbox e a outbox garantem o resultado correto. Para simular indisponibilidade, use `docker compose stop postgres` (a API responde `503` com `Retry-After`, sem efeito financeiro) e `docker compose start postgres`; com `docker compose stop localstack`, a API continua operando e a outbox drena quando a fila volta.

Os resultados efetivamente executados no ambiente de desenvolvimento estão em [`docs/VALIDATION.md`](docs/VALIDATION.md). A presença de um comando documentado não significa que ele passou em toda máquina ou em toda configuração.

## Estrutura do projeto

```text
.
├── main.go
├── internal/
│   ├── money/       # Valor monetário, parsing e aritmética segura
│   ├── wallet/      # Domínio de carteira e ledger
│   ├── wagering/    # Tipos de operação, estados e validações
│   ├── app/         # Casos de uso e persistência PostgreSQL
│   ├── auth/        # Validação JWT/JWKS
│   ├── httpapi/     # Endpoints, contratos e autorização
│   ├── inbound/     # Consumidor SQS e Inbox
│   ├── outbox/      # Publicação de eventos persistidos
│   ├── messaging/   # Abstração e adaptadores de mensageria
│   ├── metrics/     # Métricas Prometheus
│   └── bootstrap/   # Uber Fx e ciclo de vida
├── db/migrations/  # Schema versionado
├── docker-compose.yml
├── Dockerfile
├── .env.example
├── README.md
├── ARCHITECTURE.md
└── docs/VALIDATION.md
```

## Limitações conhecidas

- O estado `FAILED` existe no domínio e no schema, mas nenhum caminho atual da aplicação o persiste. Falhas transitórias de infraestrutura são revertidas e retornadas para retry; referências pendentes terminam conforme a política documentada. A persistência de falhas terminais exige uma política explícita que diferencie falhas permanentes de erros transitórios e de resultado incerto de commit.
- Introspecção/revogação de tokens e rotação operacional de segredos não estão implementadas.
- A política IAM de acesso às filas não é aplicada pelo LocalStack no Compose.
- A integração com uma conta AWS real não foi validada nesta verificação.
- Tracing distribuído com OpenTelemetry, partidas dobradas e testes de carga não fazem parte do escopo validado.

Essas limitações não anulam as garantias de concorrência, ledger, idempotência e recuperação que foram exercitadas nos testes listados em [`docs/VALIDATION.md`](docs/VALIDATION.md), mas precisam ser consideradas antes de usar o serviço em produção.

## Arquitetura

[`ARCHITECTURE.md`](ARCHITECTURE.md) detalha camadas, invariantes financeiras, idempotência, estados, referências pendentes, ledger, Inbox/Outbox, autenticação, mensageria, observabilidade e limitações.
