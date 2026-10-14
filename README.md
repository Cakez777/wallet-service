# Wallet Service

Serviço financeiro desenvolvido em Go, com Uber Fx, PostgreSQL, Keycloak e Amazon SQS, para processar operações de provedores de jogos sobre carteiras de jogadores.

O serviço implementa operações `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK`, com idempotência persistente, ledger imutável, controle de concorrência por carteira e publicação de eventos por meio do padrão Transactional Outbox.

Projeto desenvolvido como resposta ao desafio backend da Jungle Gaming.

**Estado do projeto:** núcleo financeiro, API HTTP, autenticação, consumidor SQS com Inbox transacional, publicador da Outbox, métricas e testes automatizados.

Consulte [ARCHITECTURE.md](ARCHITECTURE.md) para conhecer as decisões de arquitetura, as regras de negócio e as limitações conhecidas.

## Pré-requisitos

- Go compatível com a versão declarada em `go.mod`.
- Docker.
- Docker Compose.
- Git, para clonar o repositório.

Para executar os testes de integração, também é necessário ter acesso a um PostgreSQL. Os testes que utilizam LocalStack ou Keycloak exigem os respectivos serviços disponíveis.

## Executar o projeto

Na raiz do repositório:

```bash
go mod tidy
docker compose up --build
```

O `go mod tidy` sincroniza as dependências e os arquivos de módulos. Se o repositório já estiver com as dependências sincronizadas, essa etapa pode ser dispensada.

O Docker Compose inicia os serviços definidos na configuração do projeto, incluindo PostgreSQL, migrations, Keycloak, API e LocalStack.

A API fica disponível em:

- API HTTP: `http://localhost:8080`
- Keycloak: `http://localhost:8081`

O Keycloak pode levar alguns segundos para iniciar. Enquanto o serviço não conseguir validar os tokens, os endpoints protegidos podem responder com `503 Service Unavailable`.

Para iniciar os serviços de infraestrutura separadamente e executar a aplicação fora do Docker:

```bash
docker compose up -d postgres migrate keycloak
go run .
```

Nesse caso, configure no ambiente as variáveis necessárias, conforme `.env.example`. A URL do banco, o emissor OIDC e a URL do JWKS precisam corresponder ao ambiente utilizado.

## Variáveis de ambiente

| Variável | Descrição | Padrão |
|---|---|---|
| `DATABASE_URL` | URL de conexão com o PostgreSQL | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` |
| `OIDC_ISSUER` | Valor esperado para o claim `iss` do token | Obrigatória |
| `OIDC_JWKS_URL` | URL das chaves públicas do provedor de identidade | Obrigatória |
| `HTTP_ADDR` | Endereço de escuta da API | `:8080` |
| `PENDING_MAX_ATTEMPTS` | Tentativas antes de rejeitar uma referência pendente | `8` |
| `PENDING_BASE_DELAY_MS` | Intervalo inicial do backoff das referências pendentes | `2000` |
| `PENDING_MAX_DELAY_MS` | Intervalo máximo do backoff das referências pendentes | `300000` |
| `WORKER_INTERVAL_MS` | Intervalo de execução do worker de pendências | `1000` |
| `SHUTDOWN_GRACE_SECONDS` | Prazo para encerramento gracioso do servidor HTTP | `20` |
| `MESSAGING_ENABLED` | Habilita ou desabilita consumidor, publicador e provisionamento de filas | `true` |
| `SQS_ENDPOINT` | Endpoint do SQS; no Docker Compose, utiliza o LocalStack | `http://localstack:4566` no ambiente local; vazio para AWS real |
| `AWS_REGION` | Região utilizada pelo SDK da AWS | `us-east-1` |
| `AWS_ACCESS_KEY_ID` | Credencial do SDK da AWS | Conforme o ambiente |
| `AWS_SECRET_ACCESS_KEY` | Credencial do SDK da AWS | Conforme o ambiente |
| `COMMANDS_QUEUE_NAME` | Fila de comandos financeiros | `wager-transactions.fifo` |
| `COMMANDS_DLQ_NAME` | Dead-letter queue dos comandos | `wager-transactions-dlq.fifo` |
| `EVENTS_QUEUE_NAME` | Fila de eventos financeiros | `wallet-events.fifo` |
| `EVENTS_DLQ_NAME` | Dead-letter queue dos eventos | `wallet-events-dlq.fifo` |
| `SQS_MAX_RECEIVE` | Número de entregas configurado antes do redrive para a DLQ | `5` |
| `SQS_VISIBILITY_SECONDS` | Período de invisibilidade da mensagem durante o processamento | `60` |
| `CONSUMER_WORKERS` | Número de goroutines de consumo por instância | `4` |
| `OUTBOX_INTERVAL_MS` | Intervalo entre ciclos do publicador da Outbox | `500` |
| `OUTBOX_BATCH` | Quantidade máxima de eventos processados por lote | `20` |

Os valores padrão devem ser conferidos na configuração da aplicação. Em produção, utilize credenciais e segredos próprios do ambiente, sem reutilizar os valores de desenvolvimento.

## Autenticação

O ambiente de desenvolvimento utiliza Keycloak, com o realm `wallet` importado pelo Docker Compose.

Os clientes de teste usam o fluxo OAuth 2.0 `client_credentials`.

| Cliente | Segredo de desenvolvimento | Papel | Finalidade |
|---|---|---|---|
| `provider-a` | `provider-a-secret` | `wallet-provider` | Simular o provedor A |
| `provider-b` | `provider-b-secret` | `wallet-provider` | Simular o provedor B |
| `wallet-internal-service` | `internal-secret` | `wallet-internal` | Operações internas de carteira |

Os segredos acima são destinados exclusivamente ao ambiente local.

### Obter um token no PowerShell

Para o serviço interno:

```powershell
$INTERNAL = (Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8081/realms/wallet/protocol/openid-connect/token" `
  -Body @{
    grant_type    = "client_credentials"
    client_id     = "wallet-internal-service"
    client_secret = "internal-secret"
  }).access_token
```

Para o provedor A:

```powershell
$PROVIDER_A = (Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8081/realms/wallet/protocol/openid-connect/token" `
  -Body @{
    grant_type    = "client_credentials"
    client_id     = "provider-a"
    client_secret = "provider-a-secret"
  }).access_token
```

Para o provedor B:

```powershell
$PROVIDER_B = (Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8081/realms/wallet/protocol/openid-connect/token" `
  -Body @{
    grant_type    = "client_credentials"
    client_id     = "provider-b"
    client_secret = "provider-b-secret"
  }).access_token
```

Os tokens são usados no cabeçalho `Authorization: Bearer <token>`.

## Endpoints HTTP

| Método | Endpoint | Finalidade |
|---|---|---|
| `POST` | `/wallets` | Criar uma carteira |
| `GET` | `/wallets/{id}` | Consultar uma carteira |
| `GET` | `/wallets/{id}/ledger?cursor=&limit=` | Consultar o ledger paginado |
| `POST` | `/wallets/{id}/reconciliation` | Reconciliar saldo e ledger |
| `POST` | `/wagering/transactions` | Processar uma operação financeira |
| `GET` | `/wagering/transactions/{id}` | Consultar uma transação |
| `GET` | `/providers/{providerId}/wagering/transactions/{externalId}` | Consultar uma transação por provedor e identificador externo |
| `GET` | `/health/live` | Verificar se a aplicação está ativa |
| `GET` | `/health/ready` | Verificar a disponibilidade das dependências |
| `GET` | `/metrics` | Consultar métricas no formato Prometheus |

Os endpoints de carteira são destinados ao serviço interno. Os provedores podem consultar apenas as próprias transações.

Consulte `ARCHITECTURE.md` para conhecer os códigos HTTP, as regras de autorização e os códigos de erro.

## Exemplos de chamadas HTTP

Os exemplos a seguir usam PowerShell.

### Criar uma carteira

Obtenha primeiro o token interno, conforme a seção de autenticação.

```powershell
$body = @{
  playerId = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
  initialBalance = @{
    amount = "1000.00"
    currency = "BRL"
  }
} | ConvertTo-Json -Depth 5

$wallet = Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8080/wallets" `
  -Headers @{ Authorization = "Bearer $INTERNAL" } `
  -ContentType "application/json" `
  -Body $body

$wallet
```

Guarde o identificador retornado pela API para utilizar nos próximos exemplos.

### Enviar uma aposta

Substitua `WALLET_ID` pelo identificador da carteira criada e utilize o token do provedor correspondente.

```powershell
$body = @{
  providerId = "provider-a"
  externalTransactionId = "transaction-123"
  playerId = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
  walletId = "WALLET_ID"
  roundId = "round-987"
  gameId = "fortune-chimp"
  kind = "BET"
  money = @{
    amount = "25.00"
    currency = "BRL"
  }
} | ConvertTo-Json -Depth 5

Invoke-RestMethod -Method Post `
  -Uri "http://localhost:8080/wagering/transactions" `
  -Headers @{
    Authorization = "Bearer $PROVIDER_A"
    "Idempotency-Key" = "provider-a:transaction-123"
  } `
  -ContentType "application/json" `
  -Body $body
```

O cabeçalho `Idempotency-Key` é obrigatório. Para uma nova operação financeira, utilize uma nova chave.

Repetir a mesma solicitação com a mesma chave e o mesmo conteúdo não deve realizar um segundo débito. Reutilizar a chave com conteúdo diferente resulta em conflito de idempotência.

### Consultar uma carteira

```powershell
Invoke-RestMethod -Method Get `
  -Uri "http://localhost:8080/wallets/WALLET_ID" `
  -Headers @{ Authorization = "Bearer $INTERNAL" }
```

## Mensageria SQS

O projeto utiliza filas FIFO para receber comandos financeiros e publicar eventos.

No ambiente de desenvolvimento, o Docker Compose inicia o LocalStack. O aplicativo provisiona as filas e as respectivas DLQs durante a inicialização.

A fila de comandos é:

`wager-transactions.fifo`

A fila de eventos é:

`wallet-events.fifo`

O endpoint `/health/ready` verifica a disponibilidade do PostgreSQL e das filas utilizadas pela aplicação.

As métricas ficam disponíveis em:

`http://localhost:8080/metrics`

### Enviar um comando para a fila

O consumidor espera uma mensagem no formato oficial de envelope, com os campos `messageId`, `type`, `occurredAt` e `data`.

No PowerShell, crie o arquivo `msg.json`. Substitua `PLAYER_ID` e `WALLET_ID` por identificadores reais e válidos.

```powershell
@'
{
  "messageId": "msg-sqs-001",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-10-13T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "idempotencyKey": "sqs-key-1",
    "externalTransactionId": "sqs-ext-1",
    "playerId": "PLAYER_ID",
    "walletId": "WALLET_ID",
    "roundId": "r1",
    "gameId": "g1",
    "kind": "BET",
    "money": {
      "amount": "10.00",
      "currency": "BRL"
    }
  }
}
'@ | Set-Content -Encoding ascii msg.json
```

Copie o arquivo para o container do LocalStack:

```powershell
docker cp msg.json wallet_localstack:/tmp/msg.json
```

Envie a mensagem:

```powershell
docker exec wallet_localstack awslocal sqs send-message `
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo `
  --message-group-id WALLET_ID `
  --message-deduplication-id sqs-key-1 `
  --message-body file:///tmp/msg.json
```

O exemplo pressupõe que o container do LocalStack se chama `wallet_localstack` e que a fila possui o nome padrão. Se o seu Docker Compose usar nomes diferentes, ajuste os comandos conforme os containers e as filas configurados no projeto.

Após o processamento, consulte o saldo pela API.

### Consultar eventos e mensagens na DLQ

Para consultar os eventos publicados:

```powershell
docker exec wallet_localstack awslocal sqs receive-message `
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo `
  --max-number-of-messages 5
```

Para consultar mensagens na DLQ de comandos:

```powershell
docker exec wallet_localstack awslocal sqs receive-message `
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo `
  --max-number-of-messages 5
```

A fila FIFO tem deduplicação de transporte por um período limitado. A Inbox persistida e a idempotência financeira no PostgreSQL têm responsabilidades diferentes:

- **Mesmo `messageId` no envelope:** a Inbox reconhece a entrega repetida da mesma mensagem e evita reprocessá-la.
- **`messageId` diferente, mas mesma operação financeira e mesma `idempotencyKey`:** a Inbox considera a mensagem distinta, mas a idempotência financeira impede que a operação seja aplicada novamente.
- **`MessageDeduplicationId` do SQS:** controla a deduplicação da fila FIFO; não substitui a Inbox nem a idempotência financeira.

Para testar especificamente a Inbox, a mesma mensagem pode ser reenviada com o mesmo `messageId` no envelope e outro `MessageDeduplicationId` no SQS. Isso permite distinguir a deduplicação do broker da deduplicação persistente da aplicação.

## Migrations

As migrations ficam em `db/migrations` e seguem o formato do `golang-migrate`:

- `NNNNNN_nome.up.sql`
- `NNNNNN_nome.down.sql`

O Docker Compose aplica as migrations durante a inicialização.

Para aplicar as migrations manualmente:

```bash
docker compose run --rm migrate up
```

Para reverter a última migration:

```bash
docker compose run --rm migrate down 1
```

**Atenção:** o comando `docker compose down -v` remove os volumes do Compose e pode apagar os dados persistidos do banco de desenvolvimento.

## Testes

### Testes sem dependências externas de integração

Execute na raiz do projeto:

```bash
go test ./...
```

Para análise estática:

```bash
go vet ./...
```

Para executar os testes com o detector de condições de corrida:

```bash
go test -race ./...
```

A execução de `-race` depende de um ambiente compatível com CGO e com o compilador necessário para a plataforma. Se falhar por ausência de GCC ou de outro compilador, isso não significa, por si só, que foi detectada uma condição de corrida.

### Testes de integração com PostgreSQL

Os testes de integração utilizam um PostgreSQL real e criam/removem um banco próprio para a execução.

Inicie o PostgreSQL:

```bash
docker compose up -d postgres
```

No PowerShell, configure a URL do banco:

```powershell
$env:TEST_DATABASE_URL = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"
```

Execute os testes:

```bash
go test -tags integration -count=1 ./...
```

Em um ambiente compatível com CGO e o detector de corrida, também é possível executar:

```bash
go test -tags integration -race -count=1 ./...
```

Os testes de integração cobrem, conforme os testes implementados no projeto:

- Solicitações idênticas concorrentes e idempotência.
- Disputa por saldo entre apostas.
- Processamento concorrente de carteiras diferentes.
- Reversões simultâneas.
- Referências pendentes, resolução e expiração.
- Consumidor SQS, entregas repetidas e falhas de processamento.
- Publicação da Outbox e recuperação após falhas.
- Imutabilidade do ledger e invariantes do banco.
- Isolamento entre provedores.
- Códigos HTTP e ciclo de vida da aplicação.

Consulte os testes em `internal/` para verificar os cenários e as condições exatas de cada caso.

### Testes com Keycloak

Os testes que utilizam o Keycloak real precisam que o serviço esteja iniciado:

```bash
docker compose up -d keycloak
```

Execute:

```bash
go test -tags keycloak -count=1 ./internal/auth/
```

### Testes com LocalStack

Os testes que exercitam o adaptador SQS real do LocalStack exigem o PostgreSQL e o LocalStack disponíveis:

```bash
docker compose up -d postgres localstack
```

Execute:

```bash
go test -tags localstack -count=1 -v ./internal/inbound/
```

Esses testes utilizam as filas do LocalStack, em vez de depender exclusivamente da fila em memória utilizada em outros testes.

**Importante:** os comandos acima documentam como executar os testes. A documentação não substitui a execução efetiva no ambiente em que o projeto será avaliado.

## Estrutura do projeto

```text
internal/
├── money/       Value object Money, com valores em centavos
├── wallet/      Agregado Wallet e LedgerEntry
├── wagering/    Comandos, transações, estados e regras de negócio
├── app/         Casos de uso e acesso ao PostgreSQL
├── inbound/     Consumidor SQS e Inbox transacional
├── outbox/      Publicador da Outbox
├── messaging/   Abstração de filas e adaptadores de mensageria
├── metrics/     Métricas no formato Prometheus
├── auth/        Validação de JWT e JWKS
├── httpapi/     Handlers HTTP, autenticação e autorização
└── bootstrap/   Composição de dependências e ciclo de vida com Uber Fx

db/
└── migrations/  Schema e migrations do PostgreSQL

deploy/
└── keycloak/    Configuração do realm utilizado no desenvolvimento
```

## Decisões importantes

- **Dinheiro:** `int64` em centavos + moeda; entrada só no formato `0.00` (sem arredondar), `BIGINT` no banco, nenhum `float` envolvido.
- **Concorrência:** `SELECT ... FOR UPDATE` na carteira (READ COMMITTED) + versão esperada no `UPDATE` + `CHECK balance >= 0` + trigger que amarra o saldo ao ledger. Sem lock global; funciona com várias instâncias (testado com 3 processos reais).
- **Idempotência:** índices únicos parciais no PostgreSQL (`providerId + idempotencyKey` e `providerId + externalTransactionId`) e hash SHA-256 do payload canônico; o replay devolve o resultado original e o conflito dá `409`.
- **Ledger:** imutável (triggers impedem `UPDATE`/`DELETE`/`TRUNCATE`); a reconciliação compara saldo e ledger no mesmo snapshot.
- **Inbox:** `(consumer, messageId do envelope)` gravada na mesma transação do efeito financeiro; a mensagem só é apagada do SQS depois do commit.
- **Outbox:** eventos gravados na mesma transação e publicados por worker separado (lease + `SKIP LOCKED`, ordem por agregado, backoff, mesmo `eventId` ao republicar).
- **Referências pendentes:** `REFUND`/`ROLLBACK` sem referência ficam `PENDING_REFERENCE` (HTTP `202`) e são retomados com backoff por qualquer instância; ao esgotar, viram `REJECTED`.
- **Autenticação:** tokens do Keycloak (`client_credentials`) validados localmente (JWKS, RS256, `iss`, `aud`, `exp`); o `azp` do token é o `providerId` autorizado.
- **Mensageria:** filas FIFO (grupo = carteira) com DLQ; mensagens inválidas vão direto à DLQ, falhas transitórias voltam com backoff.
- **Limitações conhecidas:** `FAILED` existe mas não é gravado, IAM do SQS não é aplicado no LocalStack, AWS real e testes de carga não foram feitos (detalhes na seção 14 do ARCHITECTURE.md).

Para detalhes sobre o fluxo transacional, os estados das operações, os códigos HTTP e as limitações conhecidas, consulte [ARCHITECTURE.md](ARCHITECTURE.md).