# Estado do Desenvolvimento

## 1. Sobre o Projeto

**Wallet Service** é um serviço de backend desenvolvido em Go para gerenciamento de carteiras, processamento de operações financeiras e processamento de transações de apostas.

**Repositório do projeto:** https://github.com/Cakez777/wallet-service

**Repositório de referência:** https://github.com/junglegaming/backend-challenge-go

## 2. Objetivo Atual

Desenvolver uma implementação alinhada aos requisitos do desafio Backend Challenge da Jungle Gaming, priorizando:

- Integridade financeira.
- Atomicidade das operações.
- Idempotência.
- Segurança e autenticação.
- Processamento confiável de mensagens.
- Observabilidade.
- Testabilidade e manutenção do código.
- Clareza arquitetural.

A conformidade integral com os requisitos do desafio original ainda precisa ser confirmada por uma revisão sistemática dos requisitos e da implementação.

## 3. Tecnologias e Componentes

Tecnologias e componentes identificados no projeto:

- Go.
- PostgreSQL.
- Docker e Docker Compose.
- AWS SQS e LocalStack.
- Keycloak.
- Uber Fx.
- API HTTP.
- Padrões Inbox e Outbox.
- Testes unitários e de integração.
- Detector de condições de corrida do Go (`-race`).

## 4. Implementações Realizadas

### 4.1. Representação Monetária

- Implementação do pacote `internal/money`.
- Tipo monetário próprio utilizado pelas operações financeiras.
- Testes automatizados para as regras monetárias.
- Validação das operações e da representação dos valores.

### 4.2. Gerenciamento de Carteiras

- Entidade de carteira.
- Integração com o tipo `money.Money`.
- Camada de persistência.
- Serviços para gerenciamento de carteiras.
- Endpoint HTTP para criação de carteiras.
- Regras relacionadas ao saldo e à moeda.

### 4.3. Operações Financeiras

- Implementação de operações de depósito e saque.
- Persistência transacional com `sql.Tx`.
- Controle de concorrência no banco de dados.
- Mecanismos de idempotência para impedir a aplicação duplicada de operações financeiras.
- Persistência de informações relacionadas às movimentações financeiras.

### 4.4. Processamento de Apostas

- Componentes para processamento de operações de apostas.
- Integração com o fluxo financeiro.
- Persistência e regras de negócio relacionadas às operações.
- Testes automatizados para os componentes envolvidos.

### 4.5. Mensageria

- Componentes de entrada de mensagens em `internal/inbound`.
- Componentes de mensageria em `internal/messaging`.
- Processamento de mensagens de apostas.
- Mecanismos de deduplicação.
- Inbox para registrar mensagens recebidas e processadas.
- Outbox para apoiar a publicação confiável de eventos.

### 4.6. Formato das Mensagens de Apostas

Foi implementado suporte ao envelope de mensagens com os campos `messageId`, `type`, `occurredAt` e `data`.

O campo `data` contém os dados específicos da operação de aposta.

O consumidor utiliza o identificador da mensagem para controlar a deduplicação e manter o contexto do processamento.

O suporte ao envelope oficial foi registrado no commit:

`04178f9 fix(inbound): support official wager message envelope`

### 4.7. Idempotência e Deduplicação

O projeto diferencia dois mecanismos.

**Deduplicação da Inbox**

- Utiliza o identificador `messageId` do envelope recebido.
- Considera também a identificação do consumidor.
- Impede o processamento repetido da mesma mensagem pelo mesmo consumidor.

**Idempotência financeira**

- Protege a aplicação da mesma operação financeira contra duplicações.
- Permite distinguir a identidade de uma mensagem da identidade da operação financeira.
- Evita que mensagens diferentes referentes à mesma operação provoquem uma segunda aplicação financeira, conforme as regras implementadas e testadas.

Esses mecanismos têm objetivos distintos e não devem ser tratados como equivalentes.

### 4.8. API HTTP e Autenticação

- API HTTP no pacote `internal/httpapi`.
- Componentes de autenticação no pacote `internal/auth`.
- Inicialização e integração dos componentes da aplicação.
- Endpoints para operações de carteira e demais funcionalidades implementadas.

Foram executados testes de autenticação, autorização e isolamento entre provedores. A conformidade integral das rotas e dos respectivos contratos ainda depende da comparação final com os requisitos do desafio.

### 4.9. Observabilidade

- Componentes de métricas no pacote `internal/metrics`.
- Componentes de inicialização e integração no pacote `internal/bootstrap`.
- Endpoint de exposição de métricas validado no ambiente local.

A cobertura completa de observabilidade, os nomes das métricas e eventuais requisitos adicionais devem ser confirmados na implementação e na especificação do desafio.

## 5. Banco de Dados

O projeto utiliza PostgreSQL e migrations para estruturar o banco de dados.

A persistência abrange os componentes relacionados a carteiras, operações financeiras e processamento de apostas.

Os testes de integração foram executados utilizando PostgreSQL.

A revisão final ainda deve confirmar a correspondência entre todos os requisitos do desafio, as migrations, as restrições de integridade e os repositórios.

## 6. Testes e Validações

Em 13 de outubro de 2026, foram realizadas as seguintes validações.

### 6.1. Testes Automatizados

Comando:

`go test -count=1 ./...`

Resultado: os testes dos pacotes foram aprovados.

### 6.2. Testes de Integração

Comando:

`go test -tags integration -count=1 ./...`

Resultado: todos os pacotes testados passaram.

### 6.3. Detector de Condições de Corrida

A execução nativa no Windows não foi possível devido à ausência de um compilador GCC/CGO compatível.

A validação foi realizada em um contêiner Linux com Go e GCC.

Testes regulares:

`go test -race -count=1 ./...`

Resultado: todos os pacotes testados passaram, sem alertas de condições de corrida.

Testes de integração:

`go test -race -tags integration -count=1 ./...`

Resultado: todos os pacotes testados passaram, sem alertas de condições de corrida.

Esses resultados representam os cenários cobertos pelas suítes executadas e não constituem uma garantia absoluta de ausência de defeitos de concorrência.

### 6.4. Autenticação e Autorização

Foram executados testes relacionados a:

- Tokens ausentes ou inválidos.
- Permissões insuficientes.
- Isolamento de dados entre provedores.
- Restrições de acesso a endpoints protegidos.
- Indisponibilidade do provedor de identidade.
- Integração de autenticação com Keycloak.

Os cenários testados passaram. A revisão final ainda deve confirmar a conformidade de todos os requisitos de segurança do desafio.

### 6.5. Concorrência e Idempotência

Foram executados testes específicos para:

- Repetição de operações idempotentes.
- Requisições concorrentes sobre a mesma carteira.
- Operações simultâneas envolvendo carteiras independentes.
- Reversões concorrentes da mesma aposta.
- Concorrência entre processos.
- Reenvio de mensagens com a mesma identidade idempotente.

Os cenários selecionados passaram.

### 6.6. Falhas e Recuperação

Foram executados testes relacionados a:

- Recuperação após indisponibilidade do broker.
- Inicialização e encerramento controlado da aplicação.
- Falhas de processamento e reentrega de mensagens.
- Deduplicação e prevenção de saldo negativo.
- Encaminhamento de mensagens problemáticas para a DLQ.
- Publicação de eventos pela Outbox.
- Validação de configuração e dependências.

Os cenários de integração selecionados passaram.

### 6.7. Ambiente Docker Compose

Foram verificadas:

- Sintaxe e configuração do Docker Compose.
- Comunicação com o Docker Engine.
- Inicialização dos serviços.
- Disponibilidade da aplicação e das dependências.
- Respostas dos endpoints de saúde.
- Exposição do endpoint de métricas.

Os serviços PostgreSQL, Keycloak, LocalStack e aplicação foram validados no ambiente local.

### 6.8. Limites das Validações

Os resultados acima não comprovam, por si só:

- Funcionamento em uma conta AWS real.
- Aplicação efetiva de políticas IAM em produção.
- Conformidade integral com todos os requisitos do desafio original.
- Ausência absoluta de defeitos.
- Adequação a cargas de produção não testadas.

Esses pontos devem ser tratados na revisão final conforme os requisitos e o escopo da entrega.

## 7. Alterações Recentes

### Suporte ao Envelope de Mensagens

Foi implementado suporte ao envelope de mensagens de apostas, incluindo a identificação da mensagem e a extração dos dados de negócio.

Commit relacionado:

`04178f9 fix(inbound): support official wager message envelope`

### Atualização da Documentação

Os arquivos `README.md` e `ARCHITECTURE.md` foram atualizados e enviados ao repositório remoto.

Commit relacionado:

`e8e4dd6 docs: update project documentation`

## 8. Estado do Repositório

Último estado conhecido em 13 de outubro de 2026:

- Branch de trabalho: `main`.
- Último commit conhecido: `e8e4dd6 docs: update project documentation`.
- Os arquivos `docs/dev-notes/DEVELOPMENT_STATUS.md` e `docs/dev-notes/NEXT_STEPS.md` possuem alterações locais pendentes de revisão.
- O estado atual de sincronização com `origin/main` ainda deve ser confirmado.
- Nenhum commit final das alterações atuais foi confirmado.

Antes de criar um novo commit, verificar:

`git status --short`

`git diff --check`

`git diff`

## 9. Pendências de Validação

As principais pendências para concluir a revisão do projeto são:

- Comparar sistematicamente a implementação com os requisitos do desafio original.
- Confirmar os contratos e comportamentos esperados dos endpoints.
- Revisar as limitações descritas no `README.md` e no `ARCHITECTURE.md`.
- Confirmar se a validação do campo `aud` dos tokens JWT é exigida e se está implementada.
- Confirmar os requisitos de autorização, revogação e ciclo de vida dos tokens.
- Verificar se o estado `FAILED` faz parte de um fluxo executado ou se está apenas representado no domínio ou no banco de dados.
- Distinguir as validações realizadas no LocalStack daquelas que dependeriam de uma conta AWS real.
- Confirmar se OpenTelemetry, ledger de partidas dobradas e testes de carga são requisitos obrigatórios ou melhorias fora do escopo.
- Verificar se os comandos de execução documentados correspondem à configuração atual.
- Atualizar a documentação de acordo com as conclusões da revisão.

As validações de testes já executadas não precisam ser repetidas sem uma alteração de código ou uma justificativa técnica.

## 10. Procedimento de Desenvolvimento

As novas alterações devem ser implementadas em etapas verificáveis.

Sempre que aplicável, cada alteração deve incluir:

1. Análise do comportamento atual.
2. Definição do resultado esperado.
3. Implementação.
4. Testes automatizados.
5. Validação de regressões.
6. Atualização da documentação.
7. Criação de um commit descritivo após a conclusão de um marco coerente.

Uma funcionalidade não deve ser considerada concluída apenas por existir no código. Seu comportamento precisa ser validado por testes ou verificações apropriadas.

A prioridade atual é finalizar a revisão de requisitos e limitações antes de decidir por novas implementações.