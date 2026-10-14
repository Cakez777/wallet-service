# Próximas Etapas

## Objetivo

Concluir a validação do Wallet Service em relação ao desafio Backend Challenge da Jungle Gaming, identificar eventuais lacunas e preparar o projeto para entrega.

## Etapa 1 — Atualizar a documentação [CONCLUÍDA]

- [x] Atualizar o `README.md` para refletir o estado atual do projeto.
- [x] Atualizar o `ARCHITECTURE.md` para documentar a arquitetura e as decisões técnicas.
- [x] Atualizar os documentos de acompanhamento do desenvolvimento.

## Etapa 2 — Validar o ambiente Docker Compose [CONCLUÍDA]

- [x] Validar a sintaxe e a configuração do Docker Compose.
- [x] Confirmar que o Docker Engine está funcionando.
- [x] Iniciar os serviços necessários.
- [x] Confirmar que a aplicação, o PostgreSQL, o Keycloak e o LocalStack funcionam em conjunto.
- [x] Verificar a inicialização e a disponibilidade dos serviços.
- [x] Validar os endpoints de saúde e a exposição de métricas.

## Etapa 3 — Validar autenticação, autorização e isolamento entre provedores [CONCLUÍDA]

- [x] Executar testes de autenticação e autorização.
- [x] Testar o acesso permitido e negado aos endpoints protegidos.
- [x] Verificar o isolamento de dados entre provedores.
- [x] Validar cenários de credenciais inválidas e permissões insuficientes.
- [x] Executar testes de integração de tokens com Keycloak.

A conclusão desta etapa se limita aos cenários executados. A revisão integral dos requisitos de segurança permanece na etapa 7.

## Etapa 4 — Validar concorrência e idempotência [CONCLUÍDA]

- [x] Executar a suíte de testes de integração.
- [x] Validar operações concorrentes sobre a mesma carteira.
- [x] Validar operações simultâneas envolvendo carteiras independentes.
- [x] Testar requisições financeiras duplicadas.
- [x] Validar a deduplicação de mensagens e a idempotência financeira separadamente.
- [x] Testar cenários de concorrência entre processos.
- [x] Validar reversões concorrentes da mesma aposta.

Os cenários selecionados passaram. A cobertura integral dos requisitos financeiros será conferida na revisão final.

## Etapa 5 — Validar falhas e recuperação [CONCLUÍDA]

- [x] Executar testes de recuperação após falhas do broker.
- [x] Validar inicialização e encerramento controlado da aplicação.
- [x] Testar falhas e reentregas de mensagens no LocalStack.
- [x] Validar a deduplicação e a prevenção de saldo negativo nos cenários testados.
- [x] Validar o encaminhamento de mensagens problemáticas para a DLQ.
- [x] Validar a publicação de eventos pela Outbox.
- [x] Executar testes de configuração e dependências.

As validações de mensageria foram realizadas no ambiente local com LocalStack. Elas não substituem uma validação em uma conta AWS real.

## Etapa 6 — Executar testes com detector de condições de corrida [CONCLUÍDA]

- [x] Preparar um ambiente Linux compatível com `go test -race`.
- [x] Executar os testes regulares com o detector de condições de corrida.
- [x] Executar os testes de integração com o detector de condições de corrida.
- [x] Confirmar que as suítes executadas passaram sem alertas de condições de corrida.

Comandos executados:

`go test -race -count=1 ./...`

`go test -race -tags integration -count=1 ./...`

**Observação:** a execução foi realizada em um contêiner Linux com Go e GCC porque o ambiente Windows não possuía um compilador GCC/CGO compatível. Os testes passaram no ambiente utilizado.

## Etapa 7 — Resolver ou justificar as limitações documentadas [EM ANDAMENTO]

- [ ] Revisar as limitações descritas no `README.md` e no `ARCHITECTURE.md`.
- [ ] Comparar os requisitos do desafio original com a implementação existente.
- [ ] Confirmar se a validação do campo `aud` dos tokens JWT é necessária e se está implementada.
- [ ] Revisar os requisitos de autorização, revogação e ciclo de vida dos tokens.
- [ ] Verificar se o estado `FAILED` participa do fluxo real de processamento.
- [ ] Confirmar quais requisitos dependem de uma validação em AWS real.
- [ ] Determinar se OpenTelemetry é obrigatório para o desafio ou se pode permanecer como melhoria futura.
- [ ] Determinar se um ledger de partidas dobradas é exigido pelo desafio.
- [ ] Determinar se testes de carga são obrigatórios para a entrega.
- [ ] Corrigir lacunas de implementação que impeçam o atendimento dos requisitos.
- [ ] Documentar justificativas para limitações que permanecerem.
- [ ] Garantir que a documentação corresponda ao comportamento real do código.

Não implementar melhorias fora do escopo sem antes verificar sua necessidade em relação aos requisitos do desafio.

## Etapa 8 — Revisão final e preparação da entrega [PENDENTE]

- [ ] Revisar o repositório oficial do desafio e identificar seus requisitos explícitos.
- [ ] Montar uma lista de verificação requisito por requisito.
- [ ] Classificar cada requisito como atendido, parcialmente atendido, não atendido ou ainda não verificado.
- [ ] Corrigir eventuais lacunas obrigatórias identificadas.
- [ ] Executar os testes relevantes após eventuais alterações de código.
- [ ] Revisar a documentação e as instruções de execução.
- [ ] Executar `git diff --check`.
- [ ] Conferir `git status --short` e o diff completo.
- [ ] Confirmar que não existem alterações acidentais ou arquivos indevidos.
- [ ] Criar um commit descritivo para o marco final, depois de concluir as validações.
- [ ] Confirmar a sincronização com o repositório remoto.

## Critério de Conclusão

O projeto estará pronto para entrega quando:

1. Os requisitos obrigatórios do desafio tiverem sido revisados individualmente.
2. Os requisitos atendidos tiverem evidências verificáveis no código ou nos testes.
3. As lacunas obrigatórias tiverem sido corrigidas.
4. As limitações restantes estiverem documentadas e justificadas.
5. Os testes relevantes tiverem sido executados após as últimas alterações.
6. A documentação e os comandos de execução estiverem consistentes com o projeto.
7. O repositório estiver em um estado revisado e coerente com o marco de entrega.