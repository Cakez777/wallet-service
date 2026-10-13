# Arquitetura e decisões

Este documento descreve a arquitetura do Wallet Service, suas decisões de implementação, as regras de negócio e as limitações conhecidas.

Os comportamentos descritos como implementados devem ser conferidos no código e nos testes correspondentes. A existência de um teste não substitui sua execução no ambiente de avaliação.

## 1. Visão geral

A aplicação reúne uma API HTTP e workers no mesmo binário. O PostgreSQL é a fonte persistente de estado financeiro, permitindo que várias instâncias da aplicação operem sobre os mesmos dados.

A arquitetura é organizada em camadas:

- `internal/money`: representação e operações monetárias.
- `internal/wallet`: agregado de carteira e lançamentos.
- `internal/wagering`: comandos, transações e regras financeiras.
- `internal/app`: casos de uso e persistência.
- `internal/httpapi`: API HTTP.
- `internal/inbound`: consumo de comandos SQS.
- `internal/outbox`: publicação de eventos.
- `internal/messaging`: abstração e adaptadores de mensageria.
- `internal/auth`: validação de tokens.
- `internal/bootstrap`: composição de dependências e ciclo de vida com Uber Fx.

O domínio procura manter as regras financeiras separadas de HTTP, SQL e infraestrutura.

## 2. Dinheiro

- Valores monetários são representados por `int64` em centavos, acompanhados da moeda.
- A entrada monetária utiliza strings decimais, como `"25.00"`.
- Não são utilizados números de ponto flutuante para representar valores financeiros.
- Valores inválidos, escala incorreta e overflow são rejeitados.
- As operações entre moedas diferentes retornam `ErrCurrencyMismatch`.
- A persistência utiliza valores inteiros em centavos no PostgreSQL.
- A moeda é validada pelo formato esperado, não necessariamente contra a lista completa da ISO 4217.

A representação inteira evita erros de arredondamento comuns em cálculos financeiros baseados em ponto flutuante.

## 3. Transações SQL e concorrência

O processamento financeiro utiliza transações SQL para manter consistentes o saldo, a transação, o ledger e os eventos da Outbox.

O fluxo geral de uma operação é:

1. Iniciar a transação SQL.
2. Bloquear a linha da carteira com `SELECT ... FOR UPDATE`.
3. Verificar operações anteriores e conflitos de idempotência.
4. Aplicar as regras de negócio.
5. Persistir a transação financeira.
6. Registrar o lançamento no ledger quando aplicável.
7. Atualizar o saldo e a versão da carteira.
8. Registrar os eventos na Outbox.
9. Confirmar a transação.

Os detalhes variam conforme o tipo de operação e o estado da transação.

### Proteções de concorrência

- O lock é aplicado por carteira, evitando um lock global que impediria o paralelismo entre carteiras diferentes.
- A atualização de saldo utiliza a versão da carteira como proteção adicional contra atualizações perdidas.
- Uma constraint impede que o saldo fique negativo.
- Constraints e triggers no banco ajudam a preservar a consistência entre saldo e ledger.
- Falhas de concorrência que podem ser repetidas são tratadas por mecanismos de retry definidos no código.

O PostgreSQL é a fonte de verdade para o estado financeiro. A fila FIFO ajuda a preservar a ordem de processamento, mas não substitui os locks, as constraints ou a idempotência do banco.

## 4. Idempotência financeira

A idempotência financeira impede que a mesma operação seja aplicada duas vezes, mesmo após reentregas, reinícios ou processamento por instâncias diferentes.

A aplicação mantém identificadores persistentes, incluindo:

- `provider_id` e `idempotency_key`;
- `provider_id` e `external_transaction_id`.

Essas combinações são protegidas por índices únicos no PostgreSQL.

### Chaves e escopo

No HTTP, a chave é recebida pelo cabeçalho `Idempotency-Key`.

No SQS, a chave financeira é informada pelo campo `idempotencyKey` dentro de `data`.

A chave é escopada por provedor. A identidade autorizada do provedor HTTP é obtida do token, e não deve ser confiada ao valor arbitrário enviado no corpo.

### Hash do payload

O hash do payload representa os campos de negócio da operação financeira, incluindo:

- `providerId`;
- `externalTransactionId`;
- `playerId`;
- `walletId`;
- `roundId`;
- `gameId`;
- `kind`;
- `money.amount`;
- `money.currency`;
- `referenceExternalTransactionId`, quando aplicável.

A chave de idempotência, o `correlationId` e os metadados de transporte não fazem parte do hash de negócio.

O HTTP e o SQS devem construir o mesmo comando financeiro para que a mesma operação produza o mesmo hash.

### Comportamento esperado

- Mesma chave e mesmo conteúdo: devolve o resultado persistido, sem repetir o efeito financeiro.
- Mesma chave e conteúdo diferente: retorna conflito de idempotência.
- Mesmo identificador externo com outra chave: retorna conflito de transação externa.
- Repetições de operações rejeitadas ou pendentes devem respeitar o estado persistido da operação original.

A idempotência financeira não é a mesma coisa que a deduplicação de mensagens SQS. Essa distinção é detalhada na seção de mensageria.

## 5. Estados e operações financeiras

Os estados utilizados incluem:

- `PROCESSED`: operação processada.
- `REJECTED`: operação rejeitada por uma regra de negócio.
- `PENDING_REFERENCE`: operação aguardando uma transação de referência.
- `FAILED`: estado existente para falha permanente, mas que não deve ser descrito como utilizado por um caminho de execução se nenhum caminho efetivamente o grava.

Os estados terminais são protegidos pelas regras do domínio e pelas restrições implementadas no banco.

### Operações

| Operação | Movimento financeiro | Regra geral |
|---|---|---|
| `BET` | Débito | Valor positivo e saldo suficiente |
| `WIN` | Crédito | Valor positivo; referência opcional conforme as regras do jogo |
| `LOSS` | Sem movimento | Valor exatamente `0.00`; não gera lançamento financeiro |
| `REFUND` | Crédito | Devolução integral de uma aposta processada |
| `ROLLBACK` | Movimento inverso | Desfaz integralmente uma operação elegível |

Quando uma operação exige referência, ela é resolvida dentro do escopo do provedor. As regras verificam a compatibilidade de jogador, carteira, moeda, rodada e tipo de operação.

As reversões são integrais, não parciais.

### Rejeições de negócio

As rejeições definitivas podem incluir códigos como:

- `INSUFFICIENT_FUNDS`;
- `REVERSAL_INSUFFICIENT_FUNDS`;
- `WALLET_PLAYER_MISMATCH`;
- `CURRENCY_MISMATCH`;
- `REFERENCE_NOT_FOUND`;
- `REFERENCE_NOT_PROCESSED`;
- `REFERENCE_MISMATCH`;
- `REFERENCE_KIND_INVALID`;
- `REVERSAL_AMOUNT_MISMATCH`;
- `ALREADY_REVERSED`.

A lista efetiva deve permanecer sincronizada com os códigos definidos no domínio e na API.

Erros de entrada, como JSON inválido ou campos obrigatórios ausentes, são diferentes de rejeições financeiras de negócio.

### REFUND e ROLLBACK

Uma transação não pode receber duas reversões bem-sucedidas quando as regras do domínio e a constraint correspondente impedem isso.

Um `ROLLBACK` pode desfazer uma operação de crédito elegível, inclusive um `REFUND`, sem reabrir automaticamente a operação financeira original que motivou o reembolso.

## 6. Referências pendentes

Quando uma operação depende de uma transação que ainda não existe, ela pode ser persistida como `PENDING_REFERENCE`.

A API pode responder `202 Accepted`, indicando que o processamento ainda não terminou.

O worker de pendências tenta resolver a referência com backoff exponencial, respeitando o número máximo de tentativas configurado.

O processamento reavalia o estado da referência antes de aplicar qualquer movimento financeiro. Isso permite que várias instâncias do serviço compartilhem o trabalho sem depender de estado em memória.

Quando o limite de tentativas é atingido, a operação pode ser rejeitada com um código de falha de referência apropriado.

Como o estado fica persistido no PostgreSQL, a pendência pode sobreviver a reinícios da aplicação.

## 7. Ledger, Inbox e Outbox

### Ledger

A tabela `wallet_ledger_entries` registra os movimentos financeiros, incluindo direção, valor e saldos antes e depois do lançamento.

As restrições do banco ajudam a garantir a consistência aritmética e a unicidade dos lançamentos associados a uma transação.

Os lançamentos são tratados como imutáveis. Operações sem movimento financeiro, como `LOSS`, e rejeições não devem gerar lançamentos de débito ou crédito.

### Outbox

Os eventos são registrados na tabela `outbox_events` dentro da mesma transação SQL que altera o estado financeiro.

O envelope de evento inclui campos como:

- `eventId`;
- `eventType`;
- `aggregateId`;
- `correlationId`;
- `causationId`;
- `occurredAt`;
- `version`;
- `data`.

O publicador da Outbox envia os eventos depois do commit. Dessa forma, uma falha de publicação não exige reverter uma operação financeira já confirmada.

A publicação é pelo menos uma vez. Por isso, os consumidores de eventos devem deduplicar usando o identificador estável do evento, como `eventId`.

### Inbox

A tabela `inbox_messages` mantém a identidade das mensagens recebidas pelo consumidor SQS.

A chave primária é composta por:

`(consumer_name, message_id)`

A gravação da Inbox ocorre na mesma transação SQL que o processamento financeiro. Isso permite relacionar o registro da mensagem ao resultado da operação.

A Inbox e a idempotência financeira têm finalidades diferentes:

- A Inbox deduplica a mesma mensagem de transporte.
- A idempotência financeira impede que uma operação financeira seja aplicada novamente, mesmo que chegue em outra mensagem.

## 8. Mensageria SQS

O serviço utiliza duas filas FIFO, cada uma com sua dead-letter queue (DLQ):

| Fila | Direção | `MessageGroupId` | `MessageDeduplicationId` |
|---|---|---|---|
| `wager-transactions.fifo` | Provedores ou serviços enviam comandos ao Wallet Service | `walletId` | Recomenda-se `idempotencyKey` |
| `wallet-events.fifo` | Wallet Service publica eventos | `aggregateId` | `eventId` |

O agrupamento por carteira preserva a ordem de processamento das mensagens daquele grupo e permite paralelismo entre carteiras diferentes.

A deduplicação nativa da fila FIFO tem duração limitada. A aplicação continua dependendo da Inbox e da idempotência persistente no PostgreSQL para proteger o processamento financeiro.

### 8.1. Formato oficial do comando

O corpo da mensagem SQS utiliza um envelope com os seguintes campos:

- `messageId`: identificador estável da mensagem de negócio;
- `type`: tipo do evento ou comando;
- `occurredAt`: instante de ocorrência;
- `data`: conteúdo da operação financeira.

O tipo aceito pelo consumidor é `WagerTransactionRequested`.

Exemplo:

```json
{
  "messageId": "msg-456",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-18T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-456",
    "idempotencyKey": "provider-a:transaction-456",
    "playerId": "PLAYER_ID",
    "walletId": "WALLET_ID",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": {
      "amount": "25.00",
      "currency": "BRL"
    }
  }
}
```

Os identificadores de jogador e carteira do exemplo são ilustrativos e precisam ser substituídos por identificadores existentes no ambiente.

O objeto `data` também pode incluir campos como `referenceExternalTransactionId` e `correlationId`, quando aplicáveis.

### 8.2. Identidade da mensagem e deduplicação da Inbox

O consumidor utiliza o `messageId` do envelope como identidade persistente da mensagem.

A chave da Inbox é:

`(consumer_name, message_id)`

O `MessageDeduplicationId` do SQS não substitui o `messageId` do envelope. São identificadores utilizados em camadas diferentes.

O fluxo de processamento é:

1. Receber a mensagem SQS.
2. Decodificar e validar o envelope.
3. Validar o tipo de comando e os dados necessários.
4. Iniciar o processamento financeiro transacional.
5. Registrar a identidade da mensagem na Inbox.
6. Processar a operação financeira.
7. Persistir transação, saldo, ledger e eventos da Outbox, quando aplicáveis.
8. Marcar a mensagem como concluída na Inbox.
9. Confirmar a transação SQL.
10. Remover a mensagem da fila depois do commit.

A identidade da Inbox e o resultado financeiro precisam ser persistidos de maneira atômica. Se ocorrer uma falha antes do commit, as alterações da transação devem ser revertidas.

### 8.3. Cenários de reentrega

**Mesmo `messageId` no envelope**

A Inbox reconhece que a mensagem já foi processada e evita repetir o processamento financeiro.

**`messageId` diferente, mesma operação financeira**

A Inbox considera a mensagem nova. A idempotência financeira persistida no PostgreSQL deve impedir um segundo débito quando a chave e o conteúdo representam a mesma operação.

**Falha antes do commit**

A transação SQL é revertida. A mensagem pode ser entregue novamente e processada.

**Falha depois do commit, antes de remover a mensagem**

A mensagem pode ser entregue novamente. A Inbox reconhece o identificador já concluído e impede o reprocessamento financeiro.

**Deduplicação FIFO**

O SQS pode descartar mensagens repetidas com o mesmo `MessageDeduplicationId` durante a janela de deduplicação da fila. Esse comportamento não deve ser usado como única proteção contra duplicidade financeira.

### 8.4. Falhas e DLQ

Falhas transitórias podem resultar em nova tentativa de processamento, com ajuste da visibilidade da mensagem e backoff.

Mensagens inválidas ou incompatíveis com o contrato podem ser encaminhadas para a DLQ conforme o comportamento implementado pelo consumidor.

Rejeições de negócio, como saldo insuficiente, não devem ser confundidas com falhas técnicas. Quando a rejeição é persistida como resultado da operação, a mensagem pode ser confirmada sem repetir indefinidamente o mesmo comando.

O comportamento exato para conflitos de idempotência, carteira inexistente, erros de banco e falhas de infraestrutura deve ser validado nos testes do consumidor.

### 8.5. Publicador da Outbox

O publicador envia eventos persistidos na Outbox para a fila de eventos.

A publicação utiliza mecanismos de concorrência no banco, como `FOR UPDATE SKIP LOCKED`, e concessões temporárias para evitar que vários publicadores processem simultaneamente o mesmo registro.

O publicador deve preservar a ordem dos eventos de um mesmo agregado, conforme as regras implementadas.

Se ocorrer uma falha após o envio à fila, mas antes da marcação do evento como publicado no banco, o evento poderá ser enviado novamente. O identificador do evento deve permanecer estável para permitir deduplicação pelos consumidores.

### 8.6. Controle de acesso às filas

A fila de comandos é tratada como um canal confiável. O serviço não deve presumir que o conteúdo de uma mensagem SQS passou pela autenticação HTTP.

Em um ambiente AWS real, o acesso às filas deve ser restringido por IAM, limitando quais produtores podem enviar comandos e quais serviços podem consumir mensagens ou publicar eventos.

Se o ambiente LocalStack não aplicar as mesmas políticas de IAM da AWS, isso deve ser tratado como uma limitação do ambiente de desenvolvimento, não como uma garantia de segurança em produção.

## 9. Autenticação e autorização

O serviço utiliza Keycloak como provedor de identidade. A aplicação valida os JWTs localmente com base nas chaves públicas publicadas pelo JWKS.

Os principais elementos são:

- Validação da assinatura do token.
- Validação do emissor `iss`.
- Verificação da expiração `exp`.
- Identificação do cliente autorizado.
- Verificação dos papéis necessários para cada operação.

O cliente identificado pelo token determina o `providerId` autorizado nas chamadas dos provedores.

### Regras de acesso

- Provedores podem enviar e consultar apenas as próprias transações.
- O serviço interno pode executar as operações administrativas de carteira.
- Endpoints de saúde são públicos.
- Uma solicitação sem permissão não deve produzir efeitos financeiros.
- Os segredos usados no realm de desenvolvimento não devem ser reutilizados em produção.

A validação de audiência (`aud`), introspecção, revogação e rotação de segredos deve ser descrita como implementada apenas se houver suporte efetivo no código.

## 10. Composição e encerramento

O aplicativo utiliza Uber Fx para compor as dependências e gerenciar o ciclo de vida.

A inicialização inclui a leitura da configuração, a conexão com o banco, a construção dos serviços, o verificador de tokens e o servidor HTTP, além dos workers e componentes de mensageria habilitados.

O encerramento gracioso deve interromper a entrada de novos trabalhos, permitir a conclusão dos trabalhos em andamento dentro dos limites definidos e fechar os recursos na ordem apropriada.

A variável `MESSAGING_ENABLED` permite desabilitar os componentes de mensageria quando isso for necessário, conforme a configuração implementada.

## 11. Observabilidade

A aplicação utiliza logs estruturados e métricas no formato Prometheus.

Os logs podem incluir identificadores de correlação e de transação para facilitar o rastreamento de uma operação entre componentes. Credenciais e payloads sensíveis não devem ser registrados integralmente.

Endpoints de saúde:

- `/health/live`: indica se o processo está ativo.
- `/health/ready`: indica se as dependências necessárias estão disponíveis.

O endpoint `/metrics` expõe as métricas configuradas no serviço.

Entre as categorias documentadas estão:

| Métrica | Finalidade |
|---|---|
| `wallet_wager_results_total` | Contabilizar resultados das operações |
| `wallet_wager_duplicates_total` | Contabilizar duplicatas reconhecidas |
| `wallet_wager_duration_seconds` | Medir a duração do processamento |
| `wallet_concurrency_conflicts_total` | Contabilizar conflitos de concorrência |
| `wallet_pending_retries_total` | Contabilizar tentativas de resolução de referências |
| `wallet_sqs_messages_total` | Contabilizar resultados do consumo SQS |
| `wallet_sqs_dlq_total` | Contabilizar mensagens encaminhadas à DLQ pela aplicação |
| `wallet_outbox_published_total` | Contabilizar eventos publicados |
| `wallet_outbox_publish_failures_total` | Contabilizar falhas de publicação |
| `wallet_outbox_lag_seconds` | Medir a idade dos eventos pendentes |
| `wallet_outbox_unpublished` | Contabilizar eventos ainda não publicados |
| `wallet_reconciliation_divergences_total` | Contabilizar divergências identificadas pela reconciliação |

As métricas reais disponíveis devem ser verificadas no código. Mensagens movidas para a DLQ diretamente pelo redrive do SQS podem não ser contabilizadas como encaminhamentos realizados pela aplicação.

Em produção, o acesso a métricas e endpoints operacionais deve ser restringido conforme as necessidades de segurança do ambiente.

## 12. Códigos HTTP

A tabela a seguir resume os códigos documentados para a API. O comportamento efetivo deve permanecer sincronizado com os handlers.

| Situação | HTTP | Exemplo de código ou resultado |
|---|---:|---|
| Operação processada ou replay | `200` | `PROCESSED` |
| Operação aguardando referência | `202` | `PENDING_REFERENCE` |
| Carteira criada | `201` | Dados da carteira |
| Entrada inválida | `400` | `INVALID_REQUEST` |
| Token ausente ou inválido | `401` | `UNAUTHORIZED` |
| Acesso não autorizado | `403` | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Recurso não encontrado | `404` | `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND` |
| Conflito de idempotência ou transação | `409` | `IDEMPOTENCY_KEY_CONFLICT`, `EXTERNAL_TRANSACTION_CONFLICT` |
| Rejeição de negócio | `422` | `REJECTED` e `failureCode` |
| Dependência indisponível | `503` | `SERVICE_UNAVAILABLE` |

Os códigos de erro específicos e seus corpos de resposta devem ser confirmados no contrato da API antes de serem considerados definitivos.

## 13. Limitações conhecidas e itens não implementados

Os itens abaixo devem ser mantidos sincronizados com o código atual e com os testes realmente executados.

| Item | Situação documentada |
|---|---|
| Estado `FAILED` | Existe no domínio/schema, mas não deve ser descrito como gravado por um fluxo de execução sem evidência no código |
| Validação de audiência `aud` | Não implementada, conforme a documentação atual |
| Introspecção ou revogação de tokens | Não implementadas, conforme a documentação atual |
| Política IAM do SQS | Deve ser aplicada e validada no ambiente AWS real |
| Execução contra AWS real | Não deve ser considerada validada apenas por testes com LocalStack |
| OpenTelemetry | Não implementado, conforme a documentação atual |
| Ledger de partidas dobradas | Não implementado, conforme a documentação atual |
| Testes de carga | Não implementados, conforme a documentação atual |

## 14. Validação do ambiente

A documentação fornece comandos para executar os testes, mas a conclusão de que o projeto funciona integralmente em um ambiente depende da execução efetiva.

Para uma validação local, recomenda-se verificar:

1. Se o Docker Compose consegue resolver a configuração.
2. Se PostgreSQL, migrations, Keycloak e LocalStack iniciam corretamente.
3. Se a API fica disponível e os endpoints de saúde retornam os estados esperados.
4. Se a autenticação funciona com os clientes de desenvolvimento.
5. Se as operações financeiras respeitam saldo, regras de negócio e idempotência.
6. Se o consumidor processa o envelope oficial SQS.
7. Se a Inbox impede reprocessamento após uma reentrega.
8. Se a Outbox publica os eventos persistidos.
9. Se os testes de integração, os testes com Keycloak e os testes com LocalStack passam nos ambientes correspondentes.
10. Se as limitações documentadas continuam refletindo o código.

A validação contra PostgreSQL, LocalStack e Keycloak deve ser diferenciada da validação contra serviços AWS reais.

## 15. Referências internas

Os principais componentes relacionados às decisões descritas neste documento são:

- `internal/money`: representação monetária.
- `internal/wallet`: carteira e ledger.
- `internal/wagering`: regras de negócio das transações.
- `internal/app`: processamento e persistência.
- `internal/inbound`: consumidor SQS e Inbox.
- `internal/outbox`: publicação de eventos.
- `internal/messaging`: abstração de filas e adaptadores.
- `internal/auth`: autenticação e validação de JWT.
- `internal/httpapi`: endpoints HTTP.
- `internal/bootstrap`: inicialização e encerramento.
- `db/migrations`: constraints, índices, triggers e tabelas do banco.

Esses componentes, junto com os testes automatizados, são a referência para confirmar o comportamento real do serviço.