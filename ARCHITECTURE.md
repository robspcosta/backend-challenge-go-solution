# Decisões de arquitetura

## Modelo financeiro

`Money` usa `int64` em centavos com moeda (BRL, USD ou EUR, todas com duas casas), sem ponto flutuante em parsing, cálculo, serialização ou persistência. No banco, valores e saldos são colunas `BIGINT` em centavos, acompanhadas da coluna `currency`. O intervalo representável é -92233720368547758.08 a 92233720368547758.07; overflow é verificado em parsing, soma, subtração e negação.

O parsing externo exige string decimal com exatamente duas casas e rejeita valor vazio, sinal, expoente, `NaN`/`Infinity`, escala diferente de dois e número JSON no lugar de string. Zeros à esquerda são aceitos e normalizados (`"01.00"` vira 100 centavos, serializado como `"1.00"`); o hash de idempotência usa sempre a forma normalizada. A serialização produz a string decimal canônica.

`Wallet` encapsula saldo e versão; operações rejeitadas não alteram seu estado. A versão começa em 1 e aumenta somente quando o saldo muda. Criação (`NewWallet`) e reidratação (`RestoreWallet`) são separadas; a reidratação valida o snapshot sem reaplicar movimentos. O pacote `domain` não importa Fx, HTTP, SQS nem pgx.

## Operações e máquina de estados

`Operation` admite BET, WIN, LOSS, REFUND e ROLLBACK; `OPENING` é rejeitado nas entradas externas. BET, WIN e reversões exigem valor positivo; LOSS exige zero. REFUND e ROLLBACK exigem `referenceExternalTransactionId`; WIN pode informá-lo.

`Transaction` começa em `PENDING` e valida as transições `PENDING -> PROCESSED | REJECTED | PENDING_REFERENCE` e `PENDING_REFERENCE -> PROCESSED | REJECTED`. Estados terminais não mudam, o que também é imposto por trigger no banco. Operações sem dependência concluem de forma síncrona na mesma transação SQL, sem commit intermediário de `PENDING`. Falhas transitórias de infraestrutura (timeout, lock timeout, banco ou SQS indisponível) fazem rollback e não deixam registro: o cliente HTTP recebe 503 e repete com a mesma identidade; a mensagem SQS volta à fila. O estado `FAILED` existe no schema, mas não é produzido (ver limitações).

## Transação SQL e concorrência

A persistência usa `pgx` v5 com SQL explícito. A aplicação depende das interfaces `Store` e `Session`; todas as chamadas de uma `Session` usam o mesmo `pgx.Tx` em READ COMMITTED, com `lock_timeout` de 3 s e `statement_timeout` de 8 s. A requisição HTTP tem prazo de 8 s. O rollback de limpeza usa contexto próprio com timeout.

A coordenação é por carteira: `SELECT ... FOR UPDATE` bloqueia somente a linha da carteira, e `UPDATE ... WHERE version=...` é uma defesa adicional contra lost update. Não há mutex local nem lock global; carteiras diferentes avançam em paralelo. Cada operação concluída grava estado, saldo, ledger, inbox (quando SQS) e outbox num único commit. A abertura positiva cria carteira, `OPENING`, crédito e eventos no mesmo commit; a abertura zero cria apenas a carteira.

Ordem de locks: o caminho síncrono bloqueia a carteira e então insere a transação nova; o worker de referências bloqueia a linha pendente (`SKIP LOCKED`) e então a carteira. Nenhum caminho bloqueia uma linha pendente enquanto segura a carteira, portanto não há ciclo.

## Schema e migrations

- Versão 1: carteira, transação e ledger. Constraints impõem saldo não negativo, unicidade de `(playerId, currency)`, vínculo carteira/jogador/moeda, unicidade por provedor + ID externo e por provedor + chave, um `OPENING` por carteira, `(walletId, transactionId)` único no ledger e a equação `balanceAfter = balanceBefore ± amount`. Triggers proíbem UPDATE, DELETE e TRUNCATE no ledger e mudanças em transações terminais. Triggers diferidos conferem, no commit, saldo e versão contra o ledger e exigem exatamente um lançamento para cada transação financeira processada.
- Versão 2: agendamento de referências pendentes, índice único de uma compensação bem-sucedida por referência e verificação no banco de referência, valor, identidade e direção do lançamento de reversão.
- Versão 3: inbox e outbox, ambas protegidas contra exclusão; o payload da outbox é imutável.
- Versão 4: role `wagering_runtime` com `SELECT/INSERT/UPDATE` apenas onde o fluxo exige e nenhum `DELETE`, `TRUNCATE`, DDL ou alteração do ledger.

Aplicação e reversão estão em `migrations/apply.sql` e `migrations/revert.sql` (comandos no README).

## Idempotência

O hash é SHA-256 do JSON compacto, com chaves ordenadas pelo `encoding/json`, dos campos de negócio: provedor, ID externo, jogador, carteira, rodada, jogo, tipo, dinheiro normalizado e referência quando presente. `Idempotency-Key` e metadados de transporte ficam de fora; UUIDs de jogador e carteira são normalizados em minúsculas. HTTP e SQS calculam o mesmo hash.

As constraints UNIQUE por provedor + chave e provedor + ID externo arbitram disputas, com `INSERT ... ON CONFLICT DO NOTHING` seguido de nova leitura na mesma transação. Mesma chave e mesmo conteúdo devolvem o resultado persistido com `idempotentReplay: true`, inclusive o saldo observado no processamento original; mesma chave com conteúdo diferente devolve 409. O mesmo ID externo com outra chave e conteúdo equivalente devolve replay, sem reservar a segunda chave. Se o commit expirar, o resultado fica incerto para o cliente, que deve repetir a identidade original.

## Reversões

A referência é resolvida por `(providerId, referenceExternalTransactionId)` e precisa concordar em jogador, carteira, moeda e rodada. REFUND referencia uma BET processada e devolve exatamente o valor original. ROLLBACK referencia BET, WIN ou REFUND processado, pelo valor integral: ROLLBACK de BET credita; de WIN ou REFUND, debita. Débito reverso sem saldo é rejeitado com `REVERSAL_INSUFFICIENT_FUNDS`, distinto do `INSUFFICIENT_FUNDS` da BET. Valor diferente, origem inadequada ou identidade incompatível geram `INVALID_REFERENCE`.

Uma BET admite uma única compensação bem-sucedida: REFUND ou ROLLBACK. Depois de um REFUND, um ROLLBACK pode desfazer esse REFUND por débito, mas a BET não recebe uma segunda compensação; a tentativa é rejeitada com `ALREADY_REVERSED`. A aplicação verifica isso sob o lock da carteira, e o índice parcial único em `reference_transaction_id` para REFUND/ROLLBACK processados garante o mesmo entre instâncias. Rejeições ficam auditadas como transações terminais sem lançamento.

## Referências pendentes

Quando a referência não existe ou está em `PENDING`/`PENDING_REFERENCE`, a operação é gravada como `PENDING_REFERENCE` com o saldo observado, sem ledger, e emite `WagerTransactionPendingReference`. Um worker Fx consulta as pendências vencidas a cada 250 ms; cada item é retomado em nova transação, bloqueando sua linha com `FOR UPDATE SKIP LOCKED` e então a carteira, de modo que várias instâncias disputam o trabalho sem processá-lo duas vezes. As pendências estão no banco e sobrevivem a reinícios.

O backoff começa em 250 ms e dobra a cada tentativa (0,5 s, 1 s, 2 s, 4 s, 8 s, 16 s, 32 s). Com o limite de oito tentativas, uma referência que não chega leva à rejeição **cerca de 64 s após o aceite** (63,75 s de backoff, mais a granularidade de 250 ms do worker). O `reference_expires_at` de 30 minutos é apenas um teto, atingido somente se nenhuma instância processar a pendência nesse período; ao atingi-lo, a pendência é rejeitada sem reavaliar se a referência chegou nesse intervalo. Ao esgotar as tentativas, o código é `REFERENCE_NOT_FOUND` se a referência não existe, ou `REFERENCE_NOT_PROCESSED` se a origem continua pendente. Origem terminal sem sucesso é rejeitada imediatamente com `REFERENCE_NOT_PROCESSED`; referência incompatível, com `INVALID_REFERENCE`. Quando uma referência válida aparece, o worker conclui a reversão na mesma transação financeira e emite `Processed` ou `Rejected`.

## Identidade e autorização

O IdP é o Keycloak, provisionado por importação de realm no Compose. Três clientes confidenciais usam `client_credentials`: `provider-a`, `provider-b` e `wallet-internal`. Um mapper acrescenta a audiência `wagering-api`. A aplicação não emite tokens nem armazena senhas. O emissor esperado (`localhost:8081`) e a URL interna de JWKS (`keycloak:8080`) são configurados separadamente, para que o `iss` público permaneça estável dentro dos containers.

O middleware valida assinatura RS256 com chave do JWKS, `kid`, emissor, audiência, `exp`, `iat`, `nbf` e `typ=Bearer`. As chaves são carregadas na inicialização e recarregadas quando surge um `kid` desconhecido, com intervalo mínimo de 5 s entre recargas. Sem configuração OIDC, os endpoints de negócio respondem 503 e o readiness falha.

O cliente é lido do `azp`, nunca do corpo. `wallet-internal` acessa apenas carteiras, ledger, reconciliação e métricas. Clientes de provedor constam numa allowlist e só enviam operações cujo `providerId` coincide com o `azp`. Na consulta por ID, a transação é carregada e seu provedor conferido antes de qualquer dado ser devolvido; na rota por provedor, o parâmetro é confrontado com o `azp`. A idempotência é escopada por provedor, então uma chave de outro provedor não dá acesso a replays alheios. O middleware nega por padrão qualquer rota não listada.

## Inbox, outbox e transporte

A inbox (`inbox_message`) tem chave primária `(consumer_name, message_id)` e guarda o SHA-256 do envelope JSON normalizado. A inserção ocorre no mesmo `pgx.Tx` da operação financeira. Uma reentrega idêntica segue para o replay financeiro persistido, sem novo movimento; o mesmo `messageId` com outro conteúdo é conflito permanente e chega à DLQ pelo redrive. A deduplicação FIFO do SQS é uma otimização, não a fonte de verdade.

Cada transição confirmada grava um snapshot de evento em `outbox_event` no mesmo commit. O publisher reivindica lotes com `FOR UPDATE SKIP LOCKED` e lease de 30 s, publica em `wager-events.fifo` fora da transação SQL e marca `published_at`; em erro, agenda retry exponencial (até 64 s) e guarda uma descrição curta. Lease vencido é retomado por outra instância. Entre o envio ao SQS e a marcação pode ocorrer republicação, sempre com o mesmo `eventId`, que os consumidores externos devem usar para deduplicar. `MessageGroupId` é a carteira; a outbox não promete ordem causal quando um evento antigo está em retry. Na entrada, `MessageGroupId=walletId` e `MessageDeduplicationId=messageId`.

O consumidor usa três goroutines, uma mensagem por chamada, long polling de 10 s e visibility timeout de 30 s. Buscar uma mensagem por vez evita que mensagens esperem em memória enquanto a visibilidade corre. A mensagem só é removida após o commit; rejeição de negócio confirmada também é terminal. Entrada inválida, conflito de hash e falhas repetidas recebem nova visibilidade de 2, 4 ou 8 s e vão para a DLQ após cinco recebimentos.

## Controle de acesso à mensageria

O gateway `cmd/producer` autentica o JWT de um provedor permitido, compara `azp` com `data.providerId`, valida o domínio e publica na fila de entrada; `wallet-internal` não é aceito como produtor. A identidade IAM `wager-producer` tem apenas `sqs:GetQueueUrl` e `sqs:SendMessage` na entrada. `wager-consumer` recebe, confirma e muda a visibilidade na entrada; envia e consulta atributos na DLQ; e envia na fila de eventos. Cada chave fica num volume Docker montado apenas no container do respectivo papel.

A assinatura do envelope contém `providerId`, `messageId`, o SHA-256 do JSON canônico de todo o envelope sem `signature` (inclusive tipo, horário, `data`, chave de idempotência e campos desconhecidos) e o HMAC-SHA-256 do vetor JSON `[providerId, messageId, payloadHash]`. O consumidor confere hash, MAC e identidades antes de chamar o caso de uso. Mensagens sem assinatura ou adulteradas são copiadas para a DLQ antes de serem removidas da origem; se a cópia falhar, a origem permanece. Quem obtiver o segredo HMAC pode forjar mensagens de qualquer provedor, então produção exige guarda e rotação coordenada do segredo.

O Compose define `ENFORCE_IAM=1`, mas a [documentação do LocalStack](https://docs.localstack.cloud/aws/developer-tools/security-testing/iam-policy-enforcement/) informa que a aplicação de políticas IAM depende da edição. Na edição gratuita usada aqui, operações fora das políticas são aceitas. `TestBrokerRolePoliciesOnLocalStack` é, portanto, um teste diagnóstico: comprova credenciais distintas por papel e registra em log se as negativas foram aplicadas, mas não falha quando não foram. O isolamento efetivo depende do HMAC e das validações de domínio, e a política precisa ser comprovada no broker de destino.

O caminho completo com as credenciais de cada container é reproduzido pelos exemplos do README (seção "Produtor autenticado"): token real do Keycloak, abertura de carteira de BRL 100.00, BET de BRL 1.00 pelo produtor e saldo final de BRL 99.00 lido na API após o consumo.

## Composição e encerramento

O Fx compõe configuração, pool pgx, repositórios, casos de uso, autenticação, workers de mensageria, worker de referências e servidor HTTP, com `fx.Module`, `fx.Provide` e `fx.Invoke`. Os hooks de início seguem a ordem auth → pool (com ping) → mensageria (resolve as filas) → worker de referências → HTTP; o listener só abre depois de o banco responder.

No encerramento, a ordem é inversa: o servidor HTTP para de aceitar requisições e conclui as em andamento; os workers têm o contexto cancelado e o hook aguarda o término das goroutines; o pool fecha por último. O trabalho SQS não confirmado sofre rollback e a mensagem tem a visibilidade liberada para reentrega. `fx.StopTimeout` é de 20 s, e o Compose concede 30 s ao app.

Com `SQS_ENDPOINT` vazio, consumidor, publisher da outbox e observador da DLQ não são registrados, e o readiness não verifica SQS (continua 200). Esse modo é usado apenas em testes que exercitam só HTTP; em execução normal, `SQS_ENDPOINT` deve estar definido.

## Reconciliação e métricas

A reconciliação exige o cliente `wallet-internal`. Uma transação `REPEATABLE READ READ ONLY` lê o saldo armazenado e a soma de todos os lançamentos, inclusive `OPENING`, na mesma visão MVCC. O banco soma em `NUMERIC`; a conversão para `Money` verifica os limites de `int64`. Divergência gera log estruturado e incrementa uma métrica, sem alterar a carteira. Se a soma ou a diferença exceder o intervalo de `Money`, a API responde 503.

`GET /metrics` exige o mesmo cliente e usa apenas labels de status conhecidos, sem IDs, tokens ou payloads. As métricas cobrem resultados, replays, retries de consumo e publicação, mensagens confirmadas, conflitos de versão, divergências, latência, profundidade da DLQ e idade do evento mais antigo não publicado. As duas últimas são atualizadas a cada cinco segundos. Os contadores são por processo; um coletor deve agregar as instâncias.

## Estratégia de testes

Os testes de integração usam PostgreSQL, LocalStack e Keycloak reais em containers, cada um num schema isolado e, quando há SQS, em filas FIFO temporárias. Alguns pontos sobre o alcance de cada um:

- `TestThreeIndependentProcesses` compila o servidor com `-race` e sobe três processos independentes contra o mesmo banco, com tokens reais. Os processos rodam **somente HTTP** (`SQS_ENDPOINT` vazio). Prova a disputa 80.00 + 80.00 sobre 100.00, 50 envios repetidos distribuídos entre os processos e uma terceira carteira em paralelo.
- `TestConcurrentHTTPAndSQSSameOperation` segura o lock da linha da carteira numa conexão separada até que o consumidor SQS e o handler HTTP estejam ambos esperando por ele, com a mesma chave e o mesmo ID externo. As duas vias passam pela consulta de idempotência antes de qualquer commit. O teste roda duas vezes, alternando qual via chega primeiro, e confirma um único débito, replay na outra via e saldo igual à soma do ledger.
- `TestConsumerKilledAfterCommitBeforeDelete` usa um processo filho que recebe a mensagem real do LocalStack, confirma inbox, transação, saldo, ledger e outbox, e é morto antes de `DeleteMessage`. O filho chama o caso de uso (`Service.ProcessMessage`) diretamente, **não o handler de produção do consumidor**. A retomada, sim, é feita por uma aplicação Fx completa, que recebe a reentrega, faz replay sem novo débito e confirma a mensagem.
- `TestPublisherRecoversAfterSendBeforeMark` simula o publisher interrompido após `SendMessage` e antes de `MarkPublished`, vencendo o lease diretamente no schema temporário para não esperar 30 s. Uma nova aplicação retoma o registro, preserva o `eventId` e o marca após nova tentativa.
- `TestBrokerRolePoliciesOnLocalStack` é diagnóstico (ver "Controle de acesso à mensageria").

## Medição de carga

`cmd/loadtest` usa o contrato HTTP e tokens reais do Keycloak, sem inserir linhas diretamente no banco. Cada requisição tem `externalTransactionId` e `Idempotency-Key` exclusivos, então o teste mede operações novas. As carteiras são distribuídas por índice para exercitar locks por linha. O relatório classifica 409 como conflito e também como erro HTTP, soma erros HTTP e de rede em `errors`, calcula o throughput sobre a janela das BETs e os percentis por *nearest rank*. A abertura das carteiras e a espera de seis segundos para ler a gauge da outbox ficam fora da janela.

Medição de referência, reproduzível com o comando do README (`docker compose run --rm ... integration go run ./cmd/loadtest -api http://app:8080 ... -requests 120 -workers 12 -wallets 8`) contra o app do Compose, em Docker Desktop sobre WSL2 com 12 CPUs e 8 GB: duas execuções consecutivas registraram 120 respostas 200, nenhum erro ou conflito e atraso da outbox de 0 s na amostra; throughput de 968 e 485 req/s; p50 de 9,1 e 18,4 ms; p95 de 33,0 e 55,7 ms; p99 de 42,2 e 64,7 ms. A variação entre as duas execuções mostra que é uma medição de fumaça, não uma meta de capacidade: duração curta, histórico pequeno e um único processo de aplicação limitam qualquer extrapolação. A gauge de cinco segundos não registra necessariamente o pico da outbox.

## Limitações, interpretações e trabalho não concluído

### Interpretações adotadas

- A especificação não vincula carteiras a provedores. Qualquer provedor autorizado pode movimentar qualquer carteira se conhecer `walletId` e `playerId`; o isolamento entre provedores vale para transações, chaves de idempotência, referências e consultas.
- Zeros à esquerda no valor (`"01.00"`) são aceitos e normalizados antes do hash.
- Uma operação com carteira inexistente (404 no HTTP) ou com jogador/moeda incompatíveis com a carteira (400 no HTTP) não é gravada como rejeição auditável; no SQS, ambos os casos seguem retry e DLQ.
- WIN pode referenciar uma BET da mesma rodada, sem limite de quantidade nem conferência de valor.

### Trabalho não concluído

- O estado `FAILED` não é produzido: falhas de infraestrutura são tratadas como transitórias (rollback e nova tentativa) e não há registro auditável de falha permanente.
- Não há construtor de domínio validado para o lançamento do ledger; `balanceAfter = balanceBefore ± amount` é garantido pelo CHECK e pelos triggers do banco, e o tipo Go do lançamento é apenas um DTO de leitura.
- Não há teste unitário da abertura interna (`OPENING`) com seus metadados e eventos; ela é coberta pelos testes de integração.

### Limitações e riscos

- Os triggers diferidos somam todo o ledger da carteira a cada movimento, o que simplifica a auditoria mas encarece históricos grandes. O schema não verifica o encadeamento `balanceBefore`/`balanceAfter` entre lançamentos sucessivos.
- O cursor do ledger ordena por `(created_at, id)` sem congelar um snapshot contra inserções futuras.
- O IAM do LocalStack gratuito não aplica negativas de política; em AWS, as políticas equivalentes precisam ser aplicadas e testadas, sem credenciais administrativas nos serviços.
- Access tokens não são introspectados a cada requisição: um token emitido vale até expirar (cinco minutos no ambiente local), mesmo que o cliente seja desabilitado.
- A fila de eventos não deduplica além da janela FIFO; o consumidor externo deve usar o `eventId`. Uma publicação bloqueada por mais de 30 s pode perder o lease e ser republicada.
- A senha da role runtime na migration 4 é exclusiva do ambiente local; produção deve provisionar a credencial por um sistema de segredos e restringir o acesso de rede ao Postgres.

## Uso de IA

O projeto foi desenvolvido com apoio de assistentes de IA (Codex e Claude), sob direção do autor, que definiu o escopo, revisou as decisões e executou as verificações.
