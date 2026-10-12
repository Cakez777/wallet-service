# Arquitetura e decisões

Este documento descreve o que foi decidido, o que foi interpretado e o que **não** foi
concluído. Itens ausentes estão marcados como tal; nada aqui é afirmado sem existir no código.

## Dinheiro

- Representação: `int64` em unidades mínimas (centavos) + código de moeda de 3 letras
  maiúsculas (`internal/money`). Nenhum `float` é usado em parsing, cálculo ou persistência.
- Entrada externa: string decimal com exatamente duas casas (`"25.00"`). Rejeita vazio,
  sinal, notação científica, `NaN`/`Infinity` e escala diferente de 2. Não há normalização.
- Limite: `MaxInt64` centavos. Overflow é detectado em parse, soma, subtração e negação.
- A moeda é validada só pelo formato (`[A-Z]{3}`), não contra a lista ISO 4217 completa.
- Operações entre moedas diferentes retornam `ErrCurrencyMismatch`.
- Persistência: `BIGINT` em centavos. Biblioteca de acesso: `database/sql` com o driver
  `pgx/stdlib`, com SQL explícito.

## Concorrência

- Cada movimentação abre uma transação, trava a linha da carteira com `SELECT ... FOR UPDATE`
  e atualiza o saldo com `WHERE version = $n`, incrementando `version`.
- O banco impõe `CHECK (balance >= 0)`, independente do código da aplicação.
- Locks são por carteira (por linha); não há lock global.
- Ponto a revisar: a transação usa `LevelSerializable`. `READ COMMITTED` + `FOR UPDATE` já
  evita lost updates com menos custo e menos retries.
- **Não validado**: o teste de concorrência existente está com `t.Skip` e nunca foi
  executado contra um banco real.

## Idempotência (incompleta)

- Existe a coluna `transactions.idempotency_key` com `UNIQUE`, e o header lido é
  `X-Idempotency-Key`.
- Faltam: tratar a violação de unicidade, devolver o resultado original com
  `idempotentReplay: true`, hash do payload, conflito (409) para mesma chave com conteúdo
  diferente, e unicidade por `(providerId, externalTransactionId)`.

## Ledger

- A tabela `transactions` registra tipo, valor e moeda, mas **não** é um ledger adequado:
  é mutável e não guarda direção, `balanceBefore` nem `balanceAfter`.
- Trabalho pendente: `wallet_ledger_entries` imutável (triggers que bloqueiam
  `UPDATE`/`DELETE`), `UNIQUE (wallet_id, transaction_id)` e verificação
  `balance_after = balance_before ± amount`.

## Não implementado

| Requisito do desafio | Situação |
|---|---|
| Autenticação/autorização (OIDC, Keycloak) | Não implementado |
| `WagerTransaction` e tipos BET/WIN/LOSS/REFUND/ROLLBACK, máquina de estados | Não implementado (há depósito/saque provisório) |
| Referências pendentes (`PENDING_REFERENCE`) e worker de retry | Não implementado |
| SQS (LocalStack), inbox | Não implementado |
| Transactional outbox e publicador | Não implementado |
| Uber Fx e shutdown controlado | Não implementado (composição manual em `main.go`) |
| Reconciliação (`POST /wallets/:id/reconciliation`) | Não implementado |
| Métricas | Não implementado |
| Testes de integração e de concorrência | Não implementado |

## Interpretações e observações

- `go.mod` declara `go 1.27.2`, a versão instalada no ambiente de desenvolvimento; o
  Dockerfile usa `golang:1.27-alpine` para ficar alinhado.
- As migrations seguem o formato do golang-migrate; o `docker compose` aplica as `up`
  automaticamente por um serviço dedicado.
