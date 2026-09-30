# pghangar

Uma ferramenta de sysadmin, com TUI, que copia um banco PostgreSQL de um servidor para outro por
dump e restore. O caso típico é puxar um banco da produção para um banco de desenvolvimento ou
homologação que já existe. As conexões e os perfis ficam salvos, e repetir uma cópia é escolher o
perfil e confirmar.

O nome: o hangar é onde o avião fica guardado e é preparado para voar, ao lado da torre (o
[pgtower](https://github.com/9LEVEL/pgtower)).

> **Estado: fases 1, 2 e 3 prontas** (docs/ESTRATEGIA.md §16), testadas em localhost: matriz
> 16/17/18, túnel SSH (e bastion), link instável retomável, grupos, base e reset, processo separado,
> cancelamento e processo morto. Em uso contra um servidor real desde 2026-09-30 (a produção por
> túnel SSH com TLS, para um dev).

| | |
|---|---|
| **Onde roda** | num servidor de desenvolvimento, como root |
| **Versões** | PostgreSQL 16, 17 e 18, na mesma versão ou subindo |
| **Como copia** | `pg_dump` e `pg_restore` oficiais, em containers `postgres:16`, `17` e `18` |
| **Acesso à produção** | só por túnel SSH, aberto pela própria ferramenta, com uma chave restrita ao túnel |
| **O destino** | substituído por troca de nomes. O antigo fica como `__anterior`, com desfazer |
| **O que não faz** | anonimizar dados; apagar qualquer coisa sem perguntar; escrever num banco `prod` |

## Compilar e rodar

```bash
sudo make instalar               # compila e instala em /opt/pghangar e /usr/local/bin (como o pgtower)
sudo pghangar                 # a tela, de qualquer lugar; os dados ficam em /var/lib/pghangar
sudo pghangar --dir /outro    # outro diretório de dados
pghangar versao
```

Para atualizar, é o mesmo `sudo make instalar`: as duas cópias são sempre o mesmo binário. Uma
cópia em andamento não é afetada: o processo dela continua com o binário que já estava rodando.

Pré-requisito: Docker no servidor. A primeira vez:

1. **aba 6:** `b` baixa as imagens `postgres:16`, `17` e `18` e trava pelo digest; `g` gera a
   chave SSH; `l` mostra as linhas prontas para o `authorized_keys` de cada servidor da produção;
2. **aba 3:** `a` cadastra a produção (tag `prod`, acesso `ssh`) e o destino (`homolog` ou `dev`).
   Ao salvar, a conexão é testada em camadas e a versão é lida;
3. **aba 1:** `a` cria o perfil. Os bancos se escolhem numa lista: digite parte do nome para
   filtrar, e o espaço marca. Marcar vários cria um perfil por banco, já marcados para copiar em
   fila. Daí em diante, copiar é **enter** e confirmar (`y` num destino dev; o nome do banco num
   homolog).

A cópia roda num processo separado: pode fechar a tela (ou cair o SSH) sem pará-la.

**Antes da primeira cópia para um servidor**, aprove-o como destino (aba 3, tecla `v`): sem isso, a
cópia é bloqueada.

## Sem a tela (cron)

```bash
pghangar rodar loja-dev                        # destino dev
pghangar rodar --confirmar loja loja-homolog   # destino homolog: o nome do banco é obrigatório
```

Sai com 0 (ok), 2 (a troca espera a decisão, pela tela) ou 1 (erro). Não pergunta nada (use senha
guardada ou pgpass, e aceite a chave do servidor SSH pela tela antes) e nunca apaga anteriores.
Um timer do systemd, por exemplo, toda madrugada:

```ini
# /etc/systemd/system/pghangar-loja.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/pghangar rodar --confirmar loja loja-homolog

# /etc/systemd/system/pghangar-loja.timer
[Timer]
OnCalendar=*-*-* 03:30
[Install]
WantedBy=timers.target
```

## Testar

```bash
go test ./...                                                  # unitários
go test -tags integracao ./internal/motor/ -timeout 30m        # motor: matriz 16/17/18, túnel, troca, desfazer
go test -tags integracao ./internal/execucao/ -timeout 30m     # o binário: processo separado, SIGTERM, kill -9
```

Os de integração sobem containers `postgres:16/17/18` presos em `127.0.0.1` e um servidor SSH de
teste dentro do processo: **nenhum outro host é contatado**. Precisam das imagens baixadas.

## Documentos

| | |
|---|---|
| **`docs/ESTRATEGIA.md`** | **o desenho: conexões, versões, imagens, o motor, a troca, as telas, os pacotes e as fases** |
| `docs/DECISOES.md` | o que foi decidido, e por quê |
| `docs/MERCADO.md` | o que existe no mercado, onde o pghangar se encaixa e as ideias de lá |
| `CLAUDE.md` | regras para agentes que trabalham aqui |
