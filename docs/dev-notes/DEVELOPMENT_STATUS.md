# Estado do desenvolvimento

## Projeto

Wallet Service — backend em Go para processamento de operações financeiras e apostas.

## Objetivo atual

Desenvolver uma implementação alinhada aos requisitos do desafio Backend Challenge da Jungle Gaming.

Repositório de referência:
https://github.com/junglegaming/backend-challenge-go

## Tecnologias e componentes identificados

- Go.
- PostgreSQL.
- Docker Compose.
- AWS SQS e LocalStack.
- Keycloak.
- Uber Fx.

A integração e o funcionamento de cada componente deverão ser confirmados durante o desenvolvimento.

## Implementações Realizadas

- Estrutura inicial da aplicação em Go.
- Componente `internal/money/money.go` e testes unitários aprovados (`money_test.go`).
- Migration inicial para a tabela `wallets`.
- Endpoints de health check (`GET /health/live`, `GET /health/ready`).
- Entidade `Wallet` integrada com o tipo `money.Money`.
- Camada de repositório e serviço para gerenciamento de carteiras.
- Endpoint HTTP `POST /wallets` implementado e integrado no `main.go`.

## Estado dos testes

- [x] Executar testes unitários do pacote `money` (Aprovados).
- [x] Compilação do projeto com sucesso (`go build`).

## Próximo objetivo

1. Criar testes unitários e de integração para a entidade e endpoint da Wallet.
2. Definir o modelo de transações (Ledger) e movimentações financeiras.
3. Garantir atomicidade, idempotência e controle de concorrência.