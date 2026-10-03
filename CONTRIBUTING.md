# Contribuir com o pghangar

Obrigado pelo interesse. Este documento diz como montar o ambiente, as regras do projeto e como propor
uma mudança.

## As regras de base

O pghangar copia bancos da produção. As promessas dele valem mais que qualquer recurso, e uma mudança
não pode abrir um caminho em volta delas:

- **A origem só é lida.** Toda sessão com ela começa só de leitura.
- **Um banco `prod` nunca é destino.** A regra fica no motor, com teste, e nenhuma opção a desliga.
- **Nada é apagado sem a pergunta** ao sysadmin.
- **Descer de versão é bloqueado.**
- **A senha nunca vai na linha de comando,** nem no ambiente de um container: ela entra por um
  `pgpass` temporário montado.

O desenho está em [`docs/ESTRATEGIA.md`](docs/ESTRATEGIA.md), e o porquê de cada escolha, em
[`docs/DECISOES.md`](docs/DECISOES.md). Uma decisão nova vai para lá, com a data.

**O idioma do projeto é o português do Brasil:** o código, os comentários, as mensagens da tela, os
documentos e os commits.

## Montar o ambiente

Precisa do **Go 1.27**, do **Docker** e de **root** (a ferramenta é de sysadmin, e se recusa a rodar
sem ele).

```bash
git clone https://github.com/9LEVEL/pghangar.git
cd pghangar
make                                    # compila em bin/pghangar
sudo ./bin/pghangar --dir /tmp/pghangar # a tela, num diretório de dados só para teste
```

Nunca abra a tela de desenvolvimento sobre um cadastro com servidores de verdade: ela verifica as
conexões sozinha.

## Testes

```bash
make testar      # gofmt, go vet e os testes unitários
make integracao  # os de integração: containers postgres:16/17/18 e um servidor SSH de teste
```

**Os testes só falam com `localhost`.** Os de integração sobem containers presos em `127.0.0.1` e um
servidor SSH dentro do próprio processo; precisam das imagens `postgres:16`, `17` e `18` baixadas.
Nunca aponte um teste para outro host.

Para testar a tela à mão, o **palco** sobe três Postgres e o SSH, também em `127.0.0.1`:

```bash
PALCO_DIR=/tmp/cb PALCO_ARQUIVO=/tmp/palco.env go test -tags palco ./internal/testessh -run TestPalco -timeout 90m
```

Antes de abrir um PR: `gofmt -l .` vazio, `go vet ./...` e `go test ./...` verdes. Uma mudança no
motor, na regra de versões ou no processo da execução roda também os de integração.

## Propor uma mudança

1. Para algo que não seja pequeno, abra uma issue antes: combinamos o caminho.
2. Faça uma branch a partir da `main`.
3. Commits no formato conventional commits, em português: `feat(motor): …`, `fix(tunel): …`,
   `docs(estrategia): …`.
4. Siga o estilo do código em volta: os mesmos nomes, a mesma densidade de comentários.
5. Uma mudança de comportamento vem com teste.
6. Abra o PR dizendo o que mudou e como você conferiu.

## Release

A versão fica na tag (o `-ldflags` a grava no binário) e no `VERSAO` do `Makefile`.

```bash
make release VERSAO=vX.Y.Z   # dist/: o .tar.gz do binário e o SHA256SUMS
git tag -s vX.Y.Z -m "vX.Y.Z: …" && git push origin main vX.Y.Z
gh release create vX.Y.Z dist/* --verify-tag --title "vX.Y.Z: …" --notes-file notas.md
```

Os nomes dos arquivos (`pghangar_vX.Y.Z_linux_amd64.tar.gz` e `SHA256SUMS`) são usados pelo
[`install.sh`](install.sh) e pelo pgrunway: não os mude. Depois, o site
([pghangar.dev](https://pghangar.dev)) atualiza a versão.

## Relatar um problema

Diga a versão (`pghangar versao`), a do PostgreSQL de cada lado e a do Docker, o que você fez, o que
esperava e o que aconteceu. O log da execução (aba 2, `l`, ou `/var/lib/pghangar/logs/<execução>.log`)
ajuda muito; tire dele o que for da sua rede.

## Segurança

Um problema de segurança (um caminho que escreva na origem, uma senha que vaze) não vai numa issue
pública: veja o [`SECURITY.md`](SECURITY.md).
