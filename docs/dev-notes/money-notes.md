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

## Implementações existentes no ZIP inicial

- Estrutura inicial da aplicação em Go.
- Componente `internal/money/money.go`.
- Testes em `internal/money/money_test.go`.
- Migration inicial para a tabela `wallets`.
- Configuração inicial do PostgreSQL.
- Endpoints de health check.

## Componente Money

O tipo `Money` representa valores monetários usando centavos em `int64` e um código de moeda.

Foram implementados:

- Parse de valores decimais com duas casas.
- Construção a partir de centavos.
- Formatação decimal.
- Adição e subtração.
- Negação.
- Comparação.
- Verificação de overflow.
- Validação do formato de moeda.

## Estado dos testes

Pendente de confirmação pelo desenvolvedor:

- [ ] Executar `go test ./...`.
- [ ] Executar `go vet ./...`.
- [ ] Registrar os resultados reais.

Não considerar os testes aprovados até que os comandos tenham sido executados.

## Próximo objetivo

Após a aprovação dos testes do componente Money:

1. Implementar a entidade Wallet e suas regras de negócio.
2. Definir a persistência e as regras de consistência do saldo.
3. Implementar o endpoint de criação de carteiras (`POST /wallets`).
4. Criar testes unitários e de integração para a carteira.
5. Validar o comportamento contra os requisitos do desafio.

## Observação

Este documento descreve o estado pretendido a partir do ZIP inicial e das instruções de desenvolvimento disponíveis. Os arquivos do projeto e o README original devem ser consultados antes de considerar qualquer componente concluído.

## Decisões Arquiteturais Recentes

- **Integração do `database/sql` com `pgx`**: Optou-se por utilizar o driver padrão `database/sql` em conjunto com `github.com/jackc/pgx/v5/stdlib` para manter a simplicidade e a compatibilidade do repositório de carteiras com o PostgreSQL, aproveitando o pool de conexões inicializado no `main.go`.
- **Domínio de Moeda Unificado**: A entidade `Wallet` utiliza o tipo encapsulado `money.Money` para representar o saldo e a moeda, garantindo que operações de saldo e validações monetárias sigam as regras estritas de centavos em `int64` e verificação de overflow.