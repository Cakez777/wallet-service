# Registro de validação — Wallet Service

Este arquivo registra as verificações executadas manualmente no ambiente de desenvolvimento em **14 de outubro de 2026**. Os resultados são evidências daquela execução; não garantem que os mesmos comandos passem automaticamente em qualquer máquina ou ambiente.

## Ambiente usado

- Sistema: Windows, PowerShell integrado ao VS Code.
- Go: `go1.27.2 windows/amd64`.
- Docker Engine: `29.8.2`.
- Docker Compose: `v5.6.0`.
- PostgreSQL do Compose: `postgres:17`.
- Keycloak do Compose: `quay.io/keycloak/keycloak:26.0`.
- LocalStack: `localstack/localstack:3.8.1`, SQS reportado como `running`.
- GCC: MSYS2 UCRT64, com `CGO_ENABLED=1` e `CC=gcc` para testes com `-race`.

## Comandos de testes aprovados

| Comando | Resultado observado |
|---|---|
| `go test -tags integration -count=1 -v ./...` | Todos os pacotes aplicáveis passaram; inclui cenários financeiros, idempotência, três processos, domínio, HTTP, Fx e testes de mensageria do grupo `integration`. |
| `go test -tags integration -race -count=1 ./...` | Todos os pacotes passaram; executado após as alterações de moedas/Docker e novamente após o novo teste LocalStack. |
| `go test -tags keycloak -count=1 -v ./internal/auth/` | Passou, incluindo obtenção de tokens Keycloak reais e rejeição de token expirado. |
| `go test -tags keycloak -race -count=1 -v ./internal/auth/` | Passou. |
| `go test -tags localstack -count=1 -v ./internal/inbound/` | Passou com SQS do LocalStack, incluindo duplicatas, ordem, retry, DLQ e publicação da Outbox. |
| `go test -tags localstack -run TestSQSDeleteFailureAfterCommitIsHarmless -count=1 -v ./internal/inbound/` | Passou. |
| `go test -tags localstack -race -run TestSQSDeleteFailureAfterCommitIsHarmless -count=1 -v ./internal/inbound/` | Passou. |
| `go test -tags localstack -race -count=1 ./internal/inbound/` | Suíte LocalStack completa passou depois de adicionar o teste de falha na exclusão após commit. |
| `go vet ./...` | Terminou sem saída de erro/avisos. Executado após as mudanças. |
| `go test ./...` | Todos os pacotes com testes passaram. |
| `go test ./internal/money` | Passou após adicionar validação ISO 4217. |
| `go test -tags integration -race -count=1 ./internal/money ./internal/wagering ./internal/wallet` | Passou após a alteração de moedas. |

## Cenários financeiros conferidos via testes

- A mesma aposta enviada 50 vezes em paralelo foi reconhecida como uma movimentação financeira única.
- Duas apostas de `80.00` competindo por saldo de `100.00` não produziram saldo negativo.
- Carteiras distintas foram processadas em paralelo.
- Concorrência entre três processos independentes foi exercitada.
- Reversões concorrentes, referências pendentes, expiração de referências, restrições de banco e reconciliação foram cobertas pela suíte de integração.
- Os testes de domínio cobriram operações financeiras, transições, abertura da carteira e validações monetárias.

## Cenários de autenticação e mensageria

- Testes de autenticação rejeitaram tokens malformados, vazios, expirados, sem `exp`, emissor errado, assinatura de outra chave, algoritmo `HS256`, `kid` desconhecido, audiência ausente/incorreta, ausência de `subject`/`azp` e divergência de claims de cliente.
- Testes Keycloak obtiveram tokens do realm local e verificaram token expirado e rotação/atualização de chaves.
- Testes LocalStack verificaram mensagens duplicadas, ordenação, saldo sem overdraft, mensagens inválidas na DLQ, retry após falha antes do commit, redrive para DLQ e publicação de eventos na fila real de eventos.
- O teste `TestSQSDeleteFailureAfterCommitIsHarmless` simulou falha na primeira exclusão depois do commit. A mensagem foi reentregue pelo SQS do LocalStack, a Inbox identificou o replay e o saldo não foi debitado novamente.

## Fluxos manuais contra a API e PostgreSQL

### Aposta, replay, conflito e reinicialização

1. Um token `client_credentials` foi obtido do Keycloak local para `wallet-internal-service` e uma carteira foi criada com saldo inicial `100.00 BRL`.
2. Com um token novo de `provider-a`, foi enviada uma aposta `BET` de `25.00 BRL`. A API respondeu `PROCESSED`, saldo `75.00` e `idempotentReplay: false`.
3. A mesma requisição, com a mesma chave, retornou o mesmo `transactionId`, saldo `75.00` e `idempotentReplay: true`.
4. O mesmo identificador/chave com payload alterado para `30.00` foi rejeitado com HTTP `409` e `IDEMPOTENCY_KEY_CONFLICT`.
5. Consultas SQL confirmaram saldo `7500` centavos, versão `2`, uma transação externa, um débito de `2500` centavos e um evento `WagerTransactionProcessed` mais um `WalletBalanceChanged` para a aposta.
6. Após `docker compose restart app`, a prontidão voltou a `ok`. Um token novo foi obtido e o replay retornou `idempotentReplay: true`; consultas confirmaram que transação, débito e eventos não foram duplicados.

### REFUND antes da referência, reinício e resolução

1. Uma segunda carteira foi criada com saldo inicial `100.00 BRL`.
2. Foi enviado um `REFUND` de `25.00` para uma aposta que ainda não existia. A API retornou `PENDING_REFERENCE`; o banco registrou a pendência com saldo ainda `10000` centavos e nenhum lançamento do `REFUND`.
3. A API foi reiniciada. A pendência permaneceu no banco com estado `PENDING_REFERENCE`, saldo intacto e nenhum lançamento para o reembolso.
4. A aposta referenciada (`BET` de `25.00`) foi criada e processada. Após o worker resolver a referência, o `REFUND` passou para `PROCESSED`, apontando para o ID da aposta; o saldo voltou a `10000` centavos, versão `3`.
5. O ledger da carteira apresentou `OPENING CREDIT 10000` (`0 -> 10000`), `BET DEBIT 2500` (`10000 -> 7500`) e `REFUND CREDIT 2500` (`7500 -> 10000`).
6. O reenvio do mesmo `REFUND`, com a mesma chave e corpo, retornou o mesmo ID, `PROCESSED`, saldo `100.00` e `idempotentReplay: true`.
7. Consultas SQL confirmaram um único lançamento de crédito de `2500` centavos para o `REFUND` e os eventos esperados: um `WagerTransactionPendingReference`, um `WagerTransactionProcessed` e um `WalletBalanceChanged`, todos marcados como publicados na Outbox.

### Reconciliação SQL

- O saldo da carteira testada de `99000` centavos foi comparado com o saldo calculado pelo ledger; ambos eram `99000` (`reconciles = true`).
- Uma consulta de continuidade entre `balance_before` e o `balance_after` anterior não encontrou linhas inconsistentes.
- Uma consulta cruzando transações e ledger não encontrou transações processadas sem o lançamento esperado, movimentos associados a transações não processadas ou divergência entre os saldos finais registrados.
- A Outbox tinha quatro eventos publicados em uma consulta inicial; após criar uma nova carteira e uma aposta de teste, a fila `wallet-events.fifo` passou de quatro para oito mensagens visíveis e quatro eventos novos estavam marcados como publicados no banco. A correspondência individual de cada mensagem da fila não foi inspecionada nesse passo específico.

## Cobertura de testes

Medida em 14 de outubro de 2026 em um ambiente Linux de desenvolvimento (PostgreSQL 16 local, sem Docker/LocalStack/Keycloak), com o perfil combinado de todos os pacotes:

```powershell
go test -tags integration -race -count=1 -coverpkg=./... -coverprofile=cover.out ./...
go tool cover -func=cover.out
go tool cover -html=cover.out -o cover.html
```

- Cobertura total combinada: **72,5 %** das instruções, medida com as tags integration, keycloak e localstack e -race (Windows, PostgreSQL/Keycloak/LocalStack do Compose).
- Os `0 %` restantes são, em geral, código exercitado apenas por outras tags: adaptador SQS (`messaging/sqs.go`, `bootstrap/messaging.go`, `outbox.Run`) é coberto pelos testes `-tags localstack`; `WithLeeway` e a verificação do Keycloak, por `-tags keycloak`; `main()` não tem teste.
- Para o número completo na sua máquina, inclua as tags: `go test -tags "integration keycloak localstack" -race -coverpkg=./... -coverprofile=cover.out ./...` (com os serviços do Compose no ar).
- Itens relevantes que estavam em 0 % e agora têm teste: `app.IsTransient` (classificação de falhas transitórias) e os contadores do observador de métricas.

## Política do estado `FAILED`

Decidida e documentada em `ARCHITECTURE.md` (seção 7.2), **sem implementação**: nenhum caminho atual o persiste, de propósito, para não marcar como terminal uma falha transitória ou de resultado incerto.

## Limitações que não foram validadas aqui

- Não foi feito teste contra uma conta AWS real.
- A política IAM não foi comprovada como aplicada pelo LocalStack do Compose.
- Não foi realizada uma campanha de teste de carga.
- O caminho persistente de estado `FAILED` não está implementado; o método/transição de domínio é testado, mas não há fluxo atual de aplicação que persista esse estado.
- Introspecção/revogação de tokens, rotação operacional de segredos, tracing OpenTelemetry e partidas dobradas não foram validados como funcionalidades implementadas.

## Reproduzir no Windows

```powershell
docker compose up -d postgres keycloak localstack
$env:TEST_DATABASE_URL = 'postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable'
$env:CGO_ENABLED = '1'
$env:CC = 'gcc'
$env:Path = "C:\msys64\ucrt64\bin;$env:Path"

go test -tags integration -race -count=1 ./...
go test -tags keycloak -race -count=1 -v ./internal/auth/
go test -tags localstack -race -count=1 -v ./internal/inbound/
go vet ./...
go test ./...
```

Os testes reais devem ser executados com uma base local de desenvolvimento/teste. Não use `docker compose down -v` se precisar preservar os dados do volume PostgreSQL.
