# Wallet Service

Serviço em Go para movimentação de carteiras de jogadores, desenvolvido como resposta ao
desafio backend da Jungle Gaming (processamento distribuído de apostas).

> **Estado atual: implementação parcial.** Veja a seção "O que está pronto e o que falta"
> e o arquivo [ARCHITECTURE.md](ARCHITECTURE.md), que lista as decisões, as limitações e o
> trabalho não concluído.

## Pré-requisitos

- Go (versão declarada em `go.mod`)
- Docker e Docker Compose

## Como executar

```
docker compose up --build
```

Isso sobe o PostgreSQL, aplica as migrations (serviço `migrate`) e inicia a API em
`http://localhost:8080`.

Para rodar a API fora do Docker (com o Postgres do compose já no ar):

```
cp .env.example .env      # opcional; o padrão já aponta para localhost
go run .
```

## Variáveis de ambiente

| Variável       | Descrição                    | Padrão local                                               |
|----------------|------------------------------|------------------------------------------------------------|
| `DATABASE_URL` | String de conexão PostgreSQL | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` |

## Migrations

Ficam em `db/migrations`, no formato do [golang-migrate](https://github.com/golang-migrate/migrate)
(`NNNNNN_nome.up.sql` e `NNNNNN_nome.down.sql`).

Aplicar (feito automaticamente pelo `docker compose up`):

```
docker compose run --rm migrate up
```

Reverter a última migration:

```
docker compose run --rm migrate down 1
```

## Testes

```
go test ./...
go vet ./...
go test -race ./...
```

Hoje só o pacote `internal/money` tem testes que rodam de verdade. O teste de concorrência
de `internal/wallet` está marcado com `t.Skip` e **não** valida nada ainda.

## Endpoints atuais

- `GET /health/live` e `GET /health/ready` (readiness consulta o PostgreSQL)
- `POST /wallets` (hoje recebe `player_id` e `currency`; ainda **não** segue o contrato do desafio)
- `POST /wallets/{id}/deposit` e `POST /wallets/{id}/withdraw` (domínio provisório de
  depósito/saque, a ser substituído por `POST /wagering/transactions`)

## O que está pronto e o que falta

Pronto:

- `Money` (centavos em `int64`, escala fixa de 2 casas, moeda, overflow tratado) com testes.
- Schema com `CHECK (balance >= 0)` e `UNIQUE (player_id, currency)`.
- Atualização de saldo com `SELECT ... FOR UPDATE` e controle de versão.
- Health checks e logs JSON.

Não implementado (requisitos obrigatórios do desafio):

- Autenticação/autorização (OAuth2/OIDC com Keycloak).
- Domínio de apostas (`WagerTransaction`, `BET`/`WIN`/`LOSS`/`REFUND`/`ROLLBACK`) e ledger
  append-only com `balanceBefore`/`balanceAfter`.
- Idempotência completa (detecção de duplicata, hash do payload e `idempotentReplay`).
- SQS (LocalStack), inbox, outbox e worker publicador.
- Uber Fx na composição.
- Testes de integração contra banco real e testes de concorrência.
