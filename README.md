# Desafio de processamento financeiro

Especificação: `docs/CHALLENGE.md` (README oficial de `junglegaming/backend-challenge-go`, branch main). Em divergências, prevalece o repositório oficial. As decisões técnicas, interpretações e limitações estão em `ARCHITECTURE.md`.

## Pré-requisitos

- Docker Engine e Docker Compose v2 (o ambiente completo e os testes de integração rodam em containers).
- Go 1.27.2, somente para executar `go vet`/`go test` fora de containers. No Windows, `-race` exige GCC/MinGW e `CGO_ENABLED=1`; o serviço `checks` do Compose dispensa isso.
- `curl` para os exemplos em bash, ou PowerShell para os exemplos equivalentes.

## Variáveis de ambiente

O `docker-compose.yml` define todo o ambiente dos containers. O `.env.example` é apenas uma referência dos nomes e valores locais: nenhum serviço o lê, e `AWS_SHARED_CREDENTIALS_FILE` aponta para um caminho que só existe dentro do container. O Compose lê do `.env` somente `SQS_MESSAGE_HMAC_KEY`, caso você queira trocar o valor local.

| Variável | Uso |
| --- | --- |
| `DATABASE_URL` | Obrigatória. DSN do PostgreSQL; no Compose usa a role `wagering_runtime`. |
| `HTTP_ADDR` | Endereço HTTP da API; default `:8080`. |
| `SQS_ENDPOINT` | Endpoint SQS (LocalStack no Compose). **Vazio desliga consumidor, publisher da outbox e observador da DLQ**, e o readiness continua 200; uso previsto apenas em testes só de HTTP. |
| `AWS_REGION` | Região SQS; obrigatória com `SQS_ENDPOINT`. |
| `SQS_INPUT_QUEUE` | Fila de entrada FIFO (`wager-transactions.fifo`). A DLQ é derivada do nome: `<entrada sem .fifo>-dlq.fifo`. |
| `SQS_EVENT_QUEUE` | Fila FIFO de eventos de saída (`wager-events.fifo`). |
| `SQS_MESSAGE_HMAC_KEY` | Segredo HMAC compartilhado entre produtor e consumidor, mínimo de 32 bytes; obrigatório com `SQS_ENDPOINT`. |
| `AWS_SHARED_CREDENTIALS_FILE`, `AWS_PROFILE` | Credenciais IAM por papel, geradas pelo LocalStack em volumes separados (`consumer` no app, `producer` no produtor). Qualquer fonte padrão do AWS SDK também funciona. |
| `OIDC_ISSUER`, `OIDC_JWKS_URL`, `OIDC_AUDIENCE`, `OIDC_INTERNAL_CLIENT`, `OIDC_PROVIDER_CLIENTS` | Validação de JWT do Keycloak. Ou todas, ou nenhuma; sem elas a API nega os endpoints de negócio e o readiness fica 503. |
| `PRODUCER_HTTP_ADDR` | Endereço do produtor autenticado; default `:8082`. O produtor também usa `SQS_ENDPOINT`, `AWS_REGION`, `SQS_INPUT_QUEUE`, `SQS_MESSAGE_HMAC_KEY` e as variáveis `OIDC_*` exceto `OIDC_INTERNAL_CLIENT`. |

## Execução

```sh
docker compose up --build
```

O Compose inicia PostgreSQL, LocalStack e Keycloak, aplica as migrations com o serviço `migrate` e então inicia `app` (API, consumidor SQS, publisher da outbox e worker de referências) e `producer`. As portas 5432, 4566, 8080, 8081 e 8082 ficam vinculadas ao loopback.

- Filas: o script `localstack/init/queues.sh` cria automaticamente `wager-transactions.fifo` (visibility timeout 30 s, redrive para a DLQ após 5 recebimentos), `wager-transactions-dlq.fifo` e `wager-events.fifo`, além dos usuários IAM `wager-producer` e `wager-consumer` com chaves distintas.
- IdP: o Keycloak importa `keycloak/wagering-realm.json`, com os clientes `client_credentials` `provider-a`, `provider-b` e `wallet-internal`.
- Senhas e segredos do Compose (`local-only`, `local-only-runtime`, `*-local-only`, a chave HMAC padrão) são exclusivamente de desenvolvimento.

## Migrations

O serviço `migrate` executa `migrations/apply.sql`, que aplica as versões 1 a 4 em ordem, cada uma numa transação SQL com registro em `schema_migrations`, sob advisory lock. A versão 4 cria a role `wagering_runtime`, usada pela aplicação; o migrador usa `wagering`, proprietária das tabelas.

```sh
docker compose up -d postgres
docker compose run --rm migrate
```

Reversão das versões 4, 3, 2 e 1 (operação destrutiva). Pare a aplicação antes, para liberar as conexões da role runtime:

```sh
docker compose stop app producer
docker compose run --rm --entrypoint psql migrate -h postgres -U wagering -d wagering -v ON_ERROR_STOP=1 -f /migrations/revert.sql
```

## Autenticação

A API exige access token RS256 do Keycloak com emissor `http://localhost:8081/realms/wagering`, audiência `wagering-api`, expiração válida e `typ=Bearer`. O cliente é identificado pelo `azp`:

- `wallet-internal`: cria e lê carteiras, ledger, reconciliação e `/metrics`.
- `provider-a`, `provider-b`: enviam e consultam apenas as próprias transações; o `providerId` do corpo deve ser igual ao cliente autenticado, inclusive em replay.
- `/health/live` e `/health/ready` são públicos. Token ausente ou inválido retorna 401; cliente sem permissão retorna 403.

Os tokens expiram em cinco minutos. As chaves públicas são carregadas do JWKS na inicialização e recarregadas quando aparece um `kid` desconhecido.

Bash:

```sh
TOKEN_URL=http://localhost:8081/realms/wagering/protocol/openid-connect/token
token() { curl -s "$TOKEN_URL" -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$1-local-only" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'; }
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
PROVIDER_B=$(token provider-b)
```

PowerShell:

```powershell
$tokenUrl = 'http://localhost:8081/realms/wagering/protocol/openid-connect/token'
$internal = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{grant_type='client_credentials'; client_id='wallet-internal'; client_secret='wallet-internal-local-only'}).access_token
$providerA = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{grant_type='client_credentials'; client_id='provider-a'; client_secret='provider-a-local-only'}).access_token
$providerB = (Invoke-RestMethod -Method Post -Uri $tokenUrl -Body @{grant_type='client_credentials'; client_id='provider-b'; client_secret='provider-b-local-only'}).access_token
```

## API HTTP

- `POST /wallets`: cria uma carteira; saldo positivo gera `OPENING` e lançamento de crédito no mesmo commit.
- `GET /wallets/{walletId}`: saldo e versão.
- `GET /wallets/{walletId}/ledger?cursor=...&limit=50`: paginação por cursor opaco, ordenada por `(createdAt, id)`; `limit` de 1 a 100.
- `POST /wallets/{walletId}/reconciliation`: reconstrói o saldo a partir do ledger.
- `POST /wagering/transactions`: BET, WIN, LOSS, REFUND e ROLLBACK; exige `Idempotency-Key`.
- `GET /wagering/transactions/{transactionId}` e `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}`: status, código de falha e resultado original.
- `GET /health/live` e `GET /health/ready`: processo; PostgreSQL, fila SQS e chaves OIDC carregadas.

Exemplo em bash (usa os tokens da seção anterior):

```sh
PLAYER=0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1
WALLET_ID=$(curl -s http://localhost:8080/wallets -H 'Content-Type: application/json' -H "Authorization: Bearer $INTERNAL" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

curl -i http://localhost:8080/wagering/transactions -H 'Content-Type: application/json' -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: bet-1' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"bet-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"game-1\",\"kind\":\"BET\",\"money\":{\"amount\":\"30.00\",\"currency\":\"BRL\"}}"

curl -i http://localhost:8080/wagering/transactions -H 'Content-Type: application/json' -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: refund-1' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"refund-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"game-1\",\"kind\":\"REFUND\",\"referenceExternalTransactionId\":\"bet-1\",\"money\":{\"amount\":\"30.00\",\"currency\":\"BRL\"}}"

curl http://localhost:8080/providers/provider-a/wagering/transactions/bet-1 -H "Authorization: Bearer $PROVIDER_A"
curl "http://localhost:8080/wallets/$WALLET_ID/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
curl -X POST "http://localhost:8080/wallets/$WALLET_ID/reconciliation" -H "Authorization: Bearer $INTERNAL"
```

Exemplo em PowerShell; substitua `WALLET_ID` pelo ID recebido na abertura:

```powershell
curl.exe -i http://localhost:8080/wallets -H 'Content-Type: application/json' -H "Authorization: Bearer $internal" -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"100.00","currency":"BRL"}}'

curl.exe -i http://localhost:8080/wagering/transactions -H 'Content-Type: application/json' -H "Authorization: Bearer $providerA" -H 'Idempotency-Key: bet-1' -d '{"providerId":"provider-a","externalTransactionId":"bet-1","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"WALLET_ID","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"30.00","currency":"BRL"}}'

curl.exe -i http://localhost:8080/wagering/transactions -H 'Content-Type: application/json' -H "Authorization: Bearer $providerA" -H 'Idempotency-Key: refund-1' -d '{"providerId":"provider-a","externalTransactionId":"refund-1","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"WALLET_ID","roundId":"round-1","gameId":"game-1","kind":"REFUND","referenceExternalTransactionId":"bet-1","money":{"amount":"30.00","currency":"BRL"}}'

curl.exe http://localhost:8080/wallets/WALLET_ID/ledger?limit=50 -H "Authorization: Bearer $internal"
curl.exe -X POST http://localhost:8080/wallets/WALLET_ID/reconciliation -H "Authorization: Bearer $internal"
```

| HTTP | Significado |
| --- | --- |
| 201 | Carteira criada |
| 200 | Operação concluída, replay ou consulta |
| 202 | Referência pendente (`PENDING_REFERENCE`), inclusive em replay |
| 400 | Entrada inválida: valor, moeda, tipo, UUID, corpo ou header |
| 401 | Access token ausente ou inválido |
| 403 | Cliente autenticado sem permissão para o recurso |
| 404 | Recurso não encontrado |
| 409 | Carteira duplicada, ou chave/ID externo já usado com conteúdo diferente |
| 422 | Rejeição de negócio, com `failureCode` e saldo persistidos |
| 503 | Indisponibilidade transitória; repita com a mesma identidade |

Códigos estáveis de rejeição: `INSUFFICIENT_FUNDS` (BET), `REVERSAL_INSUFFICIENT_FUNDS`, `NUMERIC_LIMIT`, `INVALID_REFERENCE`, `REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED` e `ALREADY_REVERSED`. Um `ROLLBACK` de REFUND ou WIN exige débito; sem saldo, é rejeitado e fica auditável.

### Reversão antes da referência

Uma reversão cuja referência ainda não existe recebe `202` com `status: "PENDING_REFERENCE"` e o saldo observado na aceitação. O worker tenta resolvê-la com backoff exponencial: 250 ms até a primeira retentativa e depois 0,5 s, 1 s, 2 s, 4 s, 8 s, 16 s e 32 s. Na oitava tentativa sem referência, a operação é rejeitada com `REFERENCE_NOT_FOUND` (ou `REFERENCE_NOT_PROCESSED`, se a origem ainda estiver pendente), **cerca de 64 s após o aceite**. O prazo de 30 minutos gravado em `reference_expires_at` é só um teto, atingido apenas se nenhuma instância processar a pendência nesse período. Consulte a transação pelo ID ou repita o pedido com a mesma chave para acompanhar o resultado.

## Mensageria

### Produtor autenticado

O serviço `producer` (porta 8082) recebe o envelope da seção 10 de `docs/CHALLENGE.md` em `POST /wagering/messages` com `Authorization: Bearer <token de provedor>`. Ele valida o JWT, exige `data.providerId` igual ao `azp`, valida o tipo e os dados de domínio, acrescenta `signature` (HMAC) e publica na fila de entrada com `MessageGroupId=walletId` e `MessageDeduplicationId=messageId`. Responde 202 com `messageId` depois que o SQS aceita a publicação; 401 para token ausente ou inválido, 403 para provedor diferente, 400 para mensagem inválida e 503 para SQS indisponível.

```sh
curl -i http://localhost:8082/wagering/messages -H 'Content-Type: application/json' -H "Authorization: Bearer $PROVIDER_A" \
  -d "{\"messageId\":\"msg-1\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"data\":{\"providerId\":\"provider-a\",\"externalTransactionId\":\"sqs-bet-1\",\"idempotencyKey\":\"provider-a:sqs-bet-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-2\",\"gameId\":\"game-1\",\"kind\":\"BET\",\"money\":{\"amount\":\"1.00\",\"currency\":\"BRL\"}}}"

sleep 2
curl http://localhost:8080/providers/provider-a/wagering/transactions/sqs-bet-1 -H "Authorization: Bearer $PROVIDER_A"
curl "http://localhost:8080/wallets/$WALLET_ID" -H "Authorization: Bearer $INTERNAL"
```

Em sequência com os exemplos da API, o saldo final da carteira é `99.00` (100.00 − 30.00 + 30.00 − 1.00), consumido pelo app a partir da fila.

### Consumidor

Três goroutines consomem a fila de entrada, uma mensagem por chamada, com long polling de 10 s e visibility timeout de 30 s. Antes de tocar no caso de uso, o consumidor confere hash, HMAC e identidade assinada; mensagem sem assinatura ou adulterada é copiada para a DLQ e só depois removida da entrada. A chave financeira vem de `data.idempotencyKey`; a inbox deduplica por `(consumerName, messageId)` e hash do envelope. HTTP e SQS usam o mesmo caso de uso.

A mensagem só é removida depois do commit de inbox, estado financeiro, ledger e outbox; rejeições de negócio confirmadas também são removidas. Falhas, entradas inválidas e `messageId` reutilizado com outro conteúdo permanecem na fila, com nova visibilidade de 2, 4 e até 8 s; após cinco recebimentos, o redrive as move para a DLQ. No encerramento, o worker para de buscar, cancela o trabalho não confirmado (a transação SQL sofre rollback) e libera a visibilidade da mensagem.

### Publicação de eventos

O publisher reivindica até dez eventos por vez com `FOR UPDATE SKIP LOCKED` e lease de 30 s, e publica em `wager-events.fifo` com `MessageGroupId=aggregateId` (carteira) e `MessageDeduplicationId=eventId`. Em erro, grava tentativas, último erro e próximo envio com backoff exponencial de até 64 s. Lease vencido é retomado por outra instância. Uma falha entre o envio ao SQS e a marcação no Postgres pode gerar republicação com o mesmo `eventId`; consumidores devem deduplicar por ele.

Envelope: `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` (quando originado de SQS), `occurredAt` UTC, `version: 1` e `data`. Tipos e gatilhos:

- `WagerTransactionProcessed`: operação concluída, inclusive `LOSS` e abertura positiva.
- `WagerTransactionRejected`: rejeição de negócio, com `failureCode`.
- `WagerTransactionPendingReference`: espera por referência.
- `WalletBalanceChanged`: mudança efetiva de saldo, com `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter` e `walletVersion`.

Abertura com saldo zero não emite eventos.

## Reconciliação e observabilidade

`POST /wallets/{walletId}/reconciliation` (token `wallet-internal`) lê saldo e ledger numa transação `REPEATABLE READ READ ONLY` e devolve `storedBalance`, `calculatedBalance`, `difference` (armazenado menos reconstruído), `consistent` e `checkedEntries`. Divergências geram log estruturado e incrementam uma métrica, sem alterar o saldo.

`GET /metrics` (token `wallet-internal`) expõe, no formato Prometheus, resultados por status, duplicatas, retries de consumo e publicação, mensagens confirmadas, conflitos de versão, divergências de reconciliação, histograma de latência, profundidade da DLQ e atraso da outbox. Os dois últimos são atualizados a cada cinco segundos. Os logs são JSON e não incluem tokens, segredos ou payloads financeiros completos.

## Testes

### Estáticos e unitários

```sh
gofmt -l .
go vet ./...
go vet -tags integration ./...
go test ./...
go test -race ./...
```

`gofmt -l .` deve produzir saída vazia. Para rodar `gofmt`, `go vet ./...` e `go test -race ./...` em container Linux, sem Go local:

```sh
docker compose run --rm checks
```

### Integração (caminho principal)

A suíte usa PostgreSQL, LocalStack e Keycloak reais em containers. Cada teste cria seu próprio schema e, quando precisa, filas FIFO temporárias. O teste `TestSQSConsumerAndPublisher` usa a fila real `wager-transactions.fifo`; por isso, **pare `app` e `producer` antes de rodar a integração**, ou o app consumirá as mensagens do teste e as enviará para a DLQ real.

```sh
docker compose up -d --wait postgres localstack keycloak
docker compose stop app producer   # somente se a stack completa estiver em execução
docker compose run --rm integration
```

O serviço `integration` executa `go test -race -tags integration -count=1 ./...` numa imagem Linux com compilador C e monta as credenciais IAM dos dois papéis. Para ver cada teste:

```sh
docker compose run --rm integration go test -race -tags integration -count=1 -v ./...
```

Alternativa fora de containers (exige Go e, no Windows, GCC para `-race`). Nesse caminho `TestBrokerRolePoliciesOnLocalStack` é ignorado, porque depende das credenciais montadas no container `integration`:

```sh
TEST_DATABASE_URL='postgres://wagering:local-only@localhost:5432/wagering?sslmode=disable' TEST_SQS_ENDPOINT='http://localhost:4566' TEST_OIDC_ISSUER='http://localhost:8081/realms/wagering' TEST_OIDC_JWKS_URL='http://localhost:8081/realms/wagering/protocol/openid-connect/certs' TEST_OIDC_TOKEN_URL='http://localhost:8081/realms/wagering/protocol/openid-connect/token' AWS_REGION=us-east-1 AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test SQS_MESSAGE_HMAC_KEY=local-only-message-signing-key-change-me go test -race -tags integration -count=1 ./...
```

```powershell
$env:TEST_DATABASE_URL='postgres://wagering:local-only@localhost:5432/wagering?sslmode=disable'
$env:TEST_SQS_ENDPOINT='http://localhost:4566'
$env:TEST_OIDC_ISSUER='http://localhost:8081/realms/wagering'
$env:TEST_OIDC_JWKS_URL='http://localhost:8081/realms/wagering/protocol/openid-connect/certs'
$env:TEST_OIDC_TOKEN_URL='http://localhost:8081/realms/wagering/protocol/openid-connect/token'
$env:AWS_REGION='us-east-1'; $env:AWS_ACCESS_KEY_ID='test'; $env:AWS_SECRET_ACCESS_KEY='test'
$env:SQS_MESSAGE_HMAC_KEY='local-only-message-signing-key-change-me'
go test -race -tags integration -count=1 ./...
```

`TEST_DATABASE_URL` precisa de permissão para criar schemas e roles. Nunca a aponte para um banco com dados que queira preservar.

### Concorrência, múltiplas instâncias e falhas

Todos fazem parte da suíte de integração acima; para rodar um cenário isolado, acrescente `-run <Teste>` ao comando do serviço `integration`.

| Cenário | Teste |
| --- | --- |
| Mesma aposta 50 vezes em paralelo; replay após novo pool | `TestSameBet50Times` |
| Duas apostas de 80.00 sobre 100.00 | `TestTwoBets80On100` |
| Carteiras distintas em paralelo | `TestIndependentWalletDoesNotWaitForLockedWallet` |
| Três processos independentes (somente HTTP; SQS desligado nos filhos) | `TestThreeIndependentProcesses` |
| Mesma operação por HTTP e SQS **ao mesmo tempo** | `TestConcurrentHTTPAndSQSSameOperation` |
| Mesma operação por HTTP e depois por SQS | `TestHTTPCommitReplayedBySQS` |
| Processo morto após o commit e antes de `DeleteMessage` | `TestConsumerKilledAfterCommitBeforeDelete` |
| Publisher interrompido após o envio e antes da marcação | `TestPublisherRecoversAfterSendBeforeMark` |
| Dois publishers disputando a outbox e recuperação de lease | `TestInboxOutboxAtomicityAndRecovery` |
| Reversão antes da referência, reinício e expiração | `TestReferenceArrivesAfterRefundAndRestart`, `TestReferenceExpiry`, `TestTwoWorkersOnePendingReference` |
| Mensagens sem assinatura ou adulteradas na DLQ real | `TestUnsignedAndTamperedMessagesGoToRealDLQ` |
| Privilégios da role runtime | `TestRuntimeRoleMigrationAndPrivileges` |

`TestBrokerRolePoliciesOnLocalStack` é diagnóstico: comprova chaves IAM distintas por papel, mas não falha se o LocalStack aceitar operações fora das políticas, o que acontece na edição gratuita mesmo com `ENFORCE_IAM=1`. A política pretendida está em `localstack/init/queues.sh`.

### Teste de carga

Com a stack completa em execução, pelo container (sem Go local):

```sh
docker compose run --rm -e LOAD_INTERNAL_CLIENT_SECRET=wallet-internal-local-only -e LOAD_PROVIDER_CLIENT_SECRET=provider-a-local-only integration \
  go run ./cmd/loadtest -api http://app:8080 -token-url http://keycloak:8080/realms/wagering/protocol/openid-connect/token -requests 120 -workers 12 -wallets 8
```

Ou no host, com Go: defina `LOAD_INTERNAL_CLIENT_SECRET` e `LOAD_PROVIDER_CLIENT_SECRET` e execute `go run ./cmd/loadtest -requests 120 -workers 12 -wallets 8` (por padrão usa `http://127.0.0.1:8080` e o token endpoint local). A ferramenta obtém tokens reais, cria as carteiras antes da medição e envia BETs únicas de BRL 0.01; a saída JSON traz requisições, status HTTP, erros, conflitos, throughput, p50/p95/p99 e atraso da outbox. A metodologia e uma medição de referência estão em `ARCHITECTURE.md`.

Para um ensaio isolado em schema e filas temporários, iniciando a aplicação pelo Fx:

```sh
docker compose run --rm integration go test -race -tags 'integration load' -count=1 ./internal/app -run TestLoadHarnessRealContainers -v
```

### Verificação automatizada

`.github/workflows/verify.yml` valida o Compose, constrói as imagens `app` e `producer`, inicia PostgreSQL, LocalStack e Keycloak, aplica as migrations, executa `gofmt`, `go vet -tags integration`, a suíte de integração com `-race` e o ensaio de carga, e remove containers e volumes ao final. `docker compose config --quiet` valida o Compose localmente.

## Limitações

As limitações, interpretações adotadas e o trabalho não concluído estão em `ARCHITECTURE.md`, seção "Limitações, interpretações e trabalho não concluído".

## Uso de IA

O projeto foi desenvolvido com apoio de assistentes de IA (Codex e Claude), sob direção do autor, que definiu o escopo, revisou as decisões e executou as verificações.
