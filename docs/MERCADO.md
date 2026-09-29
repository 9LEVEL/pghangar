# O mercado

Pesquisa feita em 2026-09-29 (páginas, READMEs e releases; nada foi executado). **n.v.** = não
verificado numa fonte. Estrelas e datas são as do dia da consulta.

## Resumo

**Não achamos ferramenta que junte o que o copia-banco junta:**
- túnel SSH embutido com `known_hosts` estrito;
- a versão certa do cliente por imagem travada;
- a troca de nomes com desfazer num banco que já existe;
- a proteção de produção pelo `system_identifier`;
- a cópia num processo que sobrevive à queda do SSH;
- uma TUI para o operador.

Cada parte existe isolada em algum lugar:

| parte | onde existe | o limite |
|---|---|---|
| túnel SSH | Sling, Neosync | nenhum confere a chave do host, e o Neosync foi arquivado |
| versão por container | DBLab Engine, DBSnapper | o DBSnapper não trava pelo digest |
| troca por rename com volta | django-pgclone | preso ao Django |
| trava contra a produção | pgsync | só pela regra "o destino é localhost" |
| TUI | Dolly | não copia por túnel, não troca nomes; pgtower não copia |

**O nicho do copia-banco:** o refresh lógico, feito no lugar, de um banco específico num servidor
de homologação que já existe, on-premise, com a produção atrás de SSH. Sem Kubernetes, sem ZFS e
sem nuvem.

**Sinal do mercado:** vários produtos do nicho de "dados de produção em dev" foram abandonados ou
arquivados:
- Replibyte (sem release desde 2022);
- Neosync (arquivado em 08/2025);
- Snaplet (a empresa fechou em 08/2024);
- Tembo (saiu do Postgres gerenciado).

Os que ficam de pé são os que usam o `pg_dump`/`pg_restore` oficiais ou o storage (COW).

## Cópia e clonagem entre servidores

| ferramenta | licença · linguagem · atividade | como copia | várias versões | SSH | destino que já existe | proteção | retomada | interface |
|---|---|---|---|---|---|---|---|---|
| **pgcopydb** | PostgreSQL · C · ~1,56k★ · v0.18 (06/2026) | pg_dump/pg_restore para o schema + COPY paralelo; índices em paralelo; `--follow` (CDC) | binários do PATH, que "devem casar com a versão do destino" | não | `--drop-if-exists` | não achamos | `--resume`, mas perde o snapshot consistente | CLI |
| **pgsync** (ankane) | MIT · Ruby · ~3,5k★ · 0.8.1 (09/2025) | COPY por tabela, com WHERE e regras de mascaramento | n.v. | não (o README recomenda SSH/VPN por fora) | sobrescreve; `--truncate`/`--preserve` | **destino só localhost, a menos que `to_safe: true`** | `--in-batches` | CLI |
| **Dolly** | MIT · Go · criado em 07/2026 | NDJSON próprio; template, logical-stream, pg_basebackup | exige o cliente no PATH | não | `--replace` trunca as tabelas | só avisos | keyset + checkpoint | TUI + CLI |
| **pgclone** (extensão) | PostgreSQL (README) · C · 45★ · v4.4.2 (07/2026) | COPY via libpq, puxando de dentro do destino | PG 14 a 18 | não | error, skip, replace ou rename por objeto | **preflight só de leitura**: versão, conflitos, extensões, roles | checkpoint | SQL, background workers |
| **django-pgclone** | BSD-3 · Python · 54★ · 11/2025 | pg_dump → banco temporário → **troca por rename** | n.v. | não | troca; `--reversible` guarda `:pre`/`:post` | `ALLOW_RESTORE=False` por servidor | não | CLI do Django |
| **Sling CLI** | GPL-3.0 · Go · 911★ · v1.6.4 (09/2026) | ELT genérico, sempre por tabela temporária | n.v. | **sim** (`ssh_tunnel`) | full-refresh, truncate, incremental | não achamos | n.v. | CLI (UI paga) |
| **DBSnapper** | comercial (Pro US$ 300/mês) | pg_dump/pg_restore em **container** (`postgres:16-alpine`) | pela imagem | n.v. | sobrescreve | n.v. | n.v. | CLI + nuvem |
| **pg_dbmigrator** | BSD-3 · Rust · 3★ | dump/restore ou replicação lógica; confere a contagem de linhas | n.v. | não | `--no-owner --no-acl` | não | `.resume.json` | CLI |
| **Heroku `pg:copy`** | SaaS | gerenciado | gerenciado | — | apaga o destino inteiro | **o nome do destino digitado** | — | CLI |
| **pgAdmin** | PostgreSQL | pg_dump/pg_restore | **um caminho de binários por versão** | sim | clean ou CREATE | tags e cores por servidor | não | GUI |
| **DBeaver / DataGrip / Navicat** | CE aberto / comercial / comercial | transferência por JDBC, ou pg_dump | manual | sim / n.v. / sim | create, recreate ou truncate | não | não | GUI |
| **TablePlus** | comercial | pg_dump embutido | **o pg_dump 17 falha contra o PG 18** (issue #769, aberta) | n.v. | n.v. | n.v. | não | GUI |
| **pgbackweb / Databasus** | AGPL-3 / Apache-2 · Go | backup com pg_dump e agendamento | binários por versão (13 a 18 / 14 a 18) | não / ambíguo | restore no alvo | não | não | web |
| **TUIs:** lazysql, rainfrog, pgcli, harlequin, gobang, dblab, pgtower | diversas | **nenhuma copia bancos** | — | dblab tem `--ssh-*` | — | pgtower: badge prod; lazysql: `--read-only` | — | TUI |

## Dados de produção em dev/homolog

### Clones finos, branching e refresh

| ferramenta | abordagem | licença · preço | self-host · PG | destino | tempo |
|---|---|---|---|---|---|
| **DBLab Engine** (Postgres.ai) | clone COW (ZFS/LVM) sobre uma cópia lógica em container (`dockerImage`) ou física | Apache-2.0 · Go · ~2,7k★ · v4.2 (09/2026); SE a partir de US$ 62/mês | sim (Docker); PG 10 a 18 | clones numa instância dele (não num banco que já existe); **refresh agendado** num pool inativo e troca de pool | "1 TiB em ~10 s" |
| **Neon** | storage COW próprio | código Apache-2.0; SaaS | sem self-host de produção documentado; PG 14 a 18 | branch novo, "reset from parent" | "instantâneo" |
| **Xata OSS** | COW no storage + CloudNativePG; `xata clone` com anonimização | Apache-2.0 (desde 04/2026) | exige Kubernetes | branches | "TB em segundos" |
| **Supabase branching** | recria as migrations + seed | SaaS | não | branch | n.v. |
| **Aurora / Cloud SQL / Crunchy / Aiven** | COW do storage ou backup + WAL | nuvem | só na própria nuvem | cluster ou instância nova | segundos a minutos |
| **PG 18 `file_copy_method=clone`** + ZFS, Btrfs ou XFS | reflink no `CREATE DATABASE … TEMPLATE … STRATEGY FILE_COPY` | nativo | PG 18+, mesmo sistema de arquivos | **banco novo no mesmo servidor** | 6 GB em ~212 ms (contra ~67 s sem reflink) |
| **pgBackRest / Barman** | restore físico | MIT (pgBackRest) | a mesma versão major | sobrescreve o cluster inteiro | proporcional ao tamanho |
| **Delphix** (Perforce) | virtualização COW (VDB) com refresh, rewind e bookmark | comercial, preço não público | appliance; PG 15 a 17 | VDB novo, **nunca dentro de um cluster que já existe** | n.v. |
| **Redgate Clone / TDM** | imagem → containers diferenciais, com save/load de revisões | comercial (~US$ 9.600/TB/ano, n.v.) | Kubernetes; PG 11 a 17 | container novo | "< 60 s" |

### Subconjunto e anonimização

| ferramenta | abordagem | licença · atividade | observação |
|---|---|---|---|
| **Greenmask** | proxy compatível com pg_dump/pg_restore; transformadores determinísticos; subset | Apache-2.0 · Go · v0.2.24 (09/2026) | exige o pg_dump **da versão do servidor**, que é o que as nossas imagens resolvem. O SSH dele (storage SFTP) **não confere a chave do host** |
| **PostgreSQL Anonymizer** (Dalibo) | extensão com regras declarativas: mascaramento estático e dinâmico | PostgreSQL · Rust · 3.2 (PG 14 a 18) | poderia mascarar o `__novo` antes da troca; exige a extensão no destino |
| **Replibyte** | dump + subset + transformadores | GPL-3.0 · Rust | **parado** (release em 2022) |
| **Snaplet** | snapshot com subset | MIT | a empresa fechou em 2024; o `snapshot` foi arquivado |
| **Neosync** | sync + subset + mascaramento, com bastion SSH | MIT | **arquivado em 08/2025** |
| **Tonic Structural / Condenser** | mascaramento + subset | comercial (US$ 15 a 25 mil/ano, fonte de terceiros) / MIT, parado | — |
| **Jailer · pg_sample** | subset com integridade referencial | Apache-2.0 (Java, ativo) · Artistic (Perl) | — |
| **Synthesized · DATAMIMIC · DBSnapper** | geração e mascaramento | comerciais ou com edição community | — |

## Ideias para o copia-banco

A origem de cada uma está entre parênteses.

1. **Uma lista de servidores de destino permitidos** (pgsync `to_safe`), além da tag e do
   `system_identifier`: um servidor precisa ser aprovado uma vez como destino.
2. **Mais checagens no plano** (pgclone): espaço em disco no destino (dá para medir quando ele é
   local), roles referenciadas que faltam (as políticas de RLS, por exemplo) e a lista de objetos
   que vão ser copiados.
3. **Refresh agendado** (DBLab): `copia-banco rodar <perfil>` num timer do systemd, já previsto
   para a fase 2.
4. **Dump retomável** (Dolly, pgcopydb `--resume`, pg_dbmigrator): fase 3 (`docs/ESTRATEGIA.md`
   §16).
5. **"Resetar o homolog" sem ir à produção** (PG 18 clone, Neon "reset from parent", Redgate
   save/load): guardar um `<banco>__base` e recriar o homolog a partir dele em milissegundos, com
   reflink, ou em segundos a minutos, sem ele.
6. **Um modo rápido com pgcopydb:** COPY paralelo sem arquivo intermediário, índices em paralelo e
   `--follow` para encurtar a janela. Precisa de outra imagem.
7. **Anonimização opcional,** se um dia for necessária: Greenmask entre o dump e o restore, ou o
   Anonymizer no `__novo` antes da troca.
8. **Desligar o autovacuum no `__novo` durante o restore** (lição do fork do django-pgclone). A
   troca já cuida do resto (ALLOW_CONNECTIONS e derrubar sessões).
9. **Confirmação no estilo Heroku** ("WARNING: Destructive action…" com o nome digitado): já é o
   modelo do homolog.

**O que o mercado confirma nas nossas escolhas:**
- **imagens por versão:** o pgcopydb deixa a versão com o usuário, o TablePlus quebra com o PG 18 e
  o pgAdmin guarda um caminho por versão;
- **`known_hosts` estrito:** o Greenmask desliga a checagem da chave do host;
- **usar os clientes oficiais:** os projetos com formato próprio ou pipeline próprio foram os que
  mais morreram.

## Fontes

**Cópia e clonagem:**
- github.com/dimitri/pgcopydb · pgcopydb.readthedocs.io
- github.com/ankane/pgsync
- github.com/VicenteOlmos/dolly
- github.com/valehdba/pgclone
- github.com/Opus10/django-pgclone · django-pgclone.readthedocs.io · github.com/brand-meister/django-pgclone/pull/2
- github.com/slingdata-io/sling-cli · docs.slingdata.io
- github.com/isdaniel/pg_dbmigrator
- docs.dbsnapper.com
- github.com/heroku/cli (`pg/copy.ts`)
- pgadmin.org/docs · dbeaver.com/docs · jetbrains.com/help/datagrip · github.com/TablePlus/TablePlus-Windows/issues/769
- github.com/eduardolat/pgbackweb · github.com/databasus/databasus
- lazysql, rainfrog, pgcli, harlequin, gobang e dblab no GitHub

**Dados de produção em dev/homolog:**
- github.com/postgres-ai/database-lab-engine · postgres.ai/docs
- neon.com/docs · github.com/xataio/xata · supabase.com/docs
- docs.aws.amazon.com (Aurora clone) · docs.cloud.google.com (Cloud SQL clone)
- postgresql.org/docs/18/runtime-config-resource.html · boringsql.com/posts/instant-database-clones
- pgbackrest.org · docs.pgbarman.org
- help.delphix.com · documentation.red-gate.com
- gitlab.com/dalibo/postgresql_anonymizer · github.com/GreenmaskIO/greenmask
- github.com/Qovery/Replibyte · github.com/snaplet/snapshot · github.com/nucleuscloud/neosync
- tonic.ai · github.com/TonicAI/condenser · github.com/Wisser/Jailer · github.com/mla/pg_sample · dbsnapper.com
