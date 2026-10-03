# Instruções para agentes

## O que é este repositório

`pghangar` é uma ferramenta de sysadmin, com TUI, que copia bancos PostgreSQL (16, 17 e 18)
por dump e restore, com `pg_dump` e `pg_restore` rodando em containers oficiais. O desenho está
em `docs/ESTRATEGIA.md`, e o porquê de cada escolha, em `docs/DECISOES.md`.

**Idioma:** português do Brasil em tudo: documentos, commits, mensagens da ferramenta e respostas
ao usuário.

## Regras

| regra | o que significa na prática |
|---|---|
| **Só se altera este diretório** | `/docker/copy-db-prod`, `/docker/pgtower` e os demais servem só de referência. Pode ler e copiar ideias; nunca editar, formatar ou gerar arquivos lá. A exceção é o site, `/docker/pghangar.dev`, quando pedirem, com as regras do `CLAUDE.md` de lá |
| **Nada é apagado sem pergunta** | Nenhum dump, banco `__anterior` ou `__novo` sai sem o sysadmin mandar. As exceções são o `pgpass` e a role temporária de cada execução; o `__novo` que sobrou só sai pela correção da cópia, com o SIM do sysadmin |
| **Um banco `prod` nunca é destino** | A regra fica no motor, com teste, e nenhuma opção a desliga |
| **A origem só é lida** | Nada escreve na origem. Toda sessão com ela passa por `abrirOrigem` (`internal/motor`) e começa com `default_transaction_read_only` |
| **Descer de versão é bloqueado** | Uma imagem só, a da maior versão entre a origem e o destino, faz o dump e o restore |
| **Senha nunca na linha de comando** | Nem em argumento, nem em `docker run -e`. Ela entra por um `pgpass` temporário montado |
| **Uma decisão nova vai para `docs/DECISOES.md`** | Com data, o que foi decidido e por quê. Uma decisão substituída continua lá, marcada |

## Código

Antes de dizer que terminou: `gofmt -l .` (saída vazia), `go vet ./...` e `go test ./...`. Uma
mudança no motor, na regra de versões ou no processo da execução roda também os de integração:

```bash
go test -tags integracao ./internal/motor/ ./internal/execucao/ -timeout 30m
```

**Testes só em localhost.** Os de integração sobem containers presos em `127.0.0.1` e o servidor
SSH de teste (`internal/testessh`). Nunca aponte teste nenhum para outro host. Para testar a tela
à mão, o "palco" (`internal/testessh/palco_test.go`, tag `palco`) sobe três Postgres e o SSH.

## Commits

Conventional commits em português: `feat(motor): ...`, `docs(estrategia): ...`. **Sem
`Co-Authored-By: Claude`** (nem "Generated with Claude Code"), como nos outros repositórios da 9Level.
Push e tag só quando pedirem.

## O repositório é público

Nada de um servidor real entra no código, nos testes, nos documentos ou nos commits: host, IP, usuário,
nome de banco, senha. Os exemplos usam a demonstração (`loja`, `prod-loja`, `127.0.0.1`).

## Release

`make release VERSAO=vX.Y.Z` (o `VERSAO` do `Makefile` também), tag assinada, `gh release create` com o
`dist/*`. Os nomes dos arquivos são usados pelo `install.sh` e pelo pgrunway: não os mude. Depois, o
site (`/docker/pghangar.dev`, com as regras dele) atualiza a versão.
