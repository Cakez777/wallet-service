Aqui está a sua lista de tarefas devidamente atualizada com base em tudo o que já construímos, testamos e validamos com sucesso no nosso ambiente containerizado:

# Próximas tarefas

## Etapa 1 — Validar Money [CONCLUÍDO]

* [x] Executar `go test ./...`.
* [x] Corrigir eventuais falhas.
* [x] Executar `go vet ./...`.
* [x] Revisar os testes de overflow.

## Etapa 2 — Implementar Wallet [CONCLUÍDO]

* [x] Definir a entidade Wallet.
* [x] Estabelecer as regras de saldo.
* [x] Definir as regras de moeda.
* [x] Definir a persistência no PostgreSQL.

## Etapa 3 — Criar a API de carteiras [CONCLUÍDO]

* [x] Implementar `POST /wallets`.
* [x] Validar a entrada.
* [x] Persistir a carteira.
* [x] Definir os códigos HTTP e o formato dos erros.
* [ ] Criar testes unitários e de integração para o pacote `wallet`.

## Etapa 4 — Garantir a integridade financeira [CONCLUÍDO / EM VALIDAÇÃO]

* [x] Definir o modelo de transações e ledger (`transactions`).
* [x] Garantir atomicidade das movimentações (`sql.Tx`).
* [x] Projetar o controle de concorrência (`FOR UPDATE` / bloqueio pessimista e controle de versão).
* [x] Definir as regras de idempotência (`X-Idempotency-Key`).
* [x] Implementar e testar endpoints de Depósito e Saque (`POST /wallets/{id}/deposit` e `POST /wallets/{id}/withdraw`).
* [ ] Criar testes automatizados para operações concorrentes.

## Etapa 5 — Validar o desafio completo [EM ANDAMENTO]

* [x] Validar o ambiente com Docker Compose (`wallet_postgres` e `wallet_service`).
* [ ] Conferir os requisitos do README original.
* [ ] Executar os testes automatizados com o detector de condições de corrida (`go test -race`).