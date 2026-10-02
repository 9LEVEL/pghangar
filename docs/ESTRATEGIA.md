# Estratégia do `pghangar`

> **Estado: fases 1, 2 e 3 implementadas** (§16). O porquê de cada escolha fica em `docs/DECISOES.md`.

## 1. O que é

Uma ferramenta de sysadmin, com TUI, que copia um banco PostgreSQL de um servidor para outro
por dump e restore. O caso típico é puxar um banco da produção para um banco de desenvolvimento
ou homologação **que já existe**. As conexões e os perfis ficam salvos, e repetir uma cópia é
escolher o perfil e confirmar.

| | |
|---|---|
| **Onde roda** | num servidor de desenvolvimento, como **root** |
| **Usuário de banco** | **superusuário**, na origem e no destino |
| **Versões** | PostgreSQL **16, 17 e 18**, em toda combinação que mantém ou sobe a versão (§5) |
| **Como copia** | `pg_dump` e `pg_restore` oficiais, dentro dos containers `postgres:16`, `17` e `18` |
| **Onde ficam os bancos** | **fora do Docker**, no próprio servidor ou em outros hosts |
| **O que não faz** | anonimizar dados; apagar qualquer coisa sem perguntar |

## 2. Princípios

| princípio | o que significa na prática |
|---|---|
| **Nada é apagado sem pergunta** | Dumps, bancos `__anterior` e bancos parciais ficam até alguém mandar apagar pela TUI. A proteção do servidor é responsabilidade do sysadmin. As únicas exceções são artefatos da própria ferramenta: o `pgpass` temporário e a role temporária |
| **Um banco `prod` nunca é destino** | A regra fica no motor, não só na tela. Nenhuma opção a desliga |
| **A origem só é lida** | O motor só lê a origem (catálogo, `pg_dump`, contagens, `COPY … TO`), e toda sessão com ela começa com `default_transaction_read_only`: uma escrita por engano é recusada pelo próprio Postgres. Tudo o que escreve acontece no destino |
| **Os binários são os oficiais** | O motor orquestra `pg_dump`, `pg_restore` e `vacuumdb`, e não reimplementa nada |
| **Tudo é checado antes de tocar em algo** | As checagens (§7, etapa 1) rodam inteiras antes do primeiro comando que escreve |
| **"Sem resposta" não é "fora do ar"** | O diagnóstico diz em que camada parou (§3) |
| **Senha nunca na linha de comando** | Nem em argumento, nem em `docker run -e`, que aparece no `docker inspect` |
| **A cópia sobrevive à tela** | A cópia roda num processo separado. Fechar a TUI ou perder o SSH não a interrompe (§11) |

## 3. Conexões

Uma **conexão** é um servidor PostgreSQL. O banco é escolhido no perfil.

**Campos:**
- nome, **tag** (`prod`, `homolog` ou `dev`), host, porta, usuário, `sslmode`;
- senha: **guardar** (o padrão), **perguntar sempre** ou **usar o `~/.pgpass`**;
- acesso: **direto** ou **túnel SSH** (§4).

**Ao salvar, a TUI conecta e grava:**
- a versão (`server_version_num`);
- se o servidor é réplica (`pg_is_in_recovery()`);
- o `system_identifier` do cluster;
- os bancos, com tamanho, codificação e locale;
- se o usuário é superusuário. **Se não for, a TUI avisa.**

**A cada execução, tudo isso é relido.** Se a versão mudou (a produção foi do 16 para o 17, por
exemplo), a TUI avisa, atualiza o cadastro e escolhe a imagem pela versão de agora.

### Diagnóstico em camadas

| camada | o que aparece |
|---|---|
| DNS | "não resolve `db.exemplo`" |
| TCP (porta do banco ou do SSH) | "tempo esgotado" é diferente de "conexão recusada" |
| SSH, quando houver | "a chave do host MUDOU: pare e investigue" / "chave não autorizada" |
| O Postgres responde | "iniciando" / "em recuperação" |
| Autenticação | cada caso tem código de erro próprio: senha errada (`28P01`), recusado pelo `pg_hba` (`28000`), banco inexistente (`3D000`) |
| Pronto | versão, tamanho, latência, se é réplica, se a imagem da versão está baixada |

**Como o diagnóstico roda:**
- em segundo plano, com prazo por camada (10 s) e em paralelo;
- **sem travar a tela**;
- cada conexão mostra o estado com uma cor e a idade da última verificação.

## 4. Acesso: direto ou túnel SSH

**A produção não é alcançável pela rede (nem pela tailnet): chega-se a ela só por SSH.** Por isso,
o túnel é obrigatório e faz parte da fase 1.

- **Direto:** o `host:porta` é alcançável pela rede. É o caso dos bancos de desenvolvimento e de
  homologação: no próprio servidor ou em hosts da rede interna.
- **Túnel SSH:**
  - aberto pelo próprio binário (`golang.org/x/crypto/ssh`), sem depender do `ssh` do sistema;
  - escuta num **socket unix**, num diretório `700` só do root (`tmp/tunel-*`; com um `--dir`
    muito longo, `/tmp/tunel-*`, porque o caminho de um socket cabe em 108 bytes), e não numa porta
    de `127.0.0.1`, que qualquer usuário do servidor de desenvolvimento alcançaria. O container
    monta esse diretório;
  - **o TLS com o banco é do túnel.** No socket, o libpq ignora o `sslmode`, e o SSH só cifra até
    o servidor SSH. Quando o `sslmode` pede, o túnel negocia o TLS com o banco pelo canal SSH. O
    cliente fala em claro com o socket. O `verify-full` confere o certificado pelas CAs do sistema
    e contra o host do banco como cadastrado (o nome do certificado, e não `127.0.0.1`). O
    `verify-ca` não vale pelo túnel: sem a CA do servidor, conferir a cadeia sem o nome aceitaria
    qualquer certificado público. No `prefer`, como o libpq, uma recusa logo depois do TLS (um
    `pg_hba` só com `hostnossl`) faz o túnel tentar de novo em claro. O que o túnel não consegue (o
    TLS, o canal até o banco) chega ao cliente como um erro do protocolo, com o motivo;
  - **o destino do túnel é o banco visto a partir do servidor SSH:** `127.0.0.1:5432` quando o
    banco está no mesmo host, ou `db-interno:5432` quando o SSH entra num host e o banco fica em
    outro atrás dele;
  - um host de salto (bastion) é opcional e fica para a fase 3. **No caso normal, o SSH entra direto
    no servidor do banco de produção**, e o destino do túnel é `127.0.0.1:<porta do banco>`.

### O túnel durante uma cópia

- **Quem abre o túnel é o processo da execução (§11), e não a TUI.** Fechar a tela não derruba o
  túnel no meio do dump.
- **Uma conexão SSH por execução** leva as N+1 conexões do `pg_dump -j N`, cada uma num canal
  próprio.
- **O túnel manda keepalives** (`keepalive@openssh.com`) para não cair por inatividade em NAT ou
  firewall.
- **O túnel é necessário só durante o dump.** Ele fecha assim que o dump termina, e o restore
  roda no destino.
- **Se o túnel cair no meio do dump,** o dump falha, fica marcado `incompleto`, e **o destino não
  foi tocado**, porque o `__novo` só é criado depois do dump (fora a correção que apaga um `__novo`
  que sobrou, se o sysadmin a deixou marcada, §10).
- **O diagnóstico (§3) abre um túnel próprio e curto**, só para a verificação.

### Política de chaves SSH

**Uma chave por servidor de desenvolvimento.** Para revogar um servidor, basta apagar uma linha do
`authorized_keys` em cada host.

**No host da produção (o sysadmin faz uma vez; a TUI mostra o texto pronto):**
- um usuário Linux dedicado (`pghangar`), sem shell. O nome dele fica na conexão: um usuário
  criado antes com outro nome (como `copia-banco`) continua valendo;
- no `authorized_keys` dele, a chave **só abre o túnel até a porta do banco**:

  ```
  restrict,port-forwarding,permitopen="127.0.0.1:5432",command="/bin/false" ssh-ed25519 AAAA… pghangar@<servidor-dev>
  ```

  Ela não roda comando, não abre terminal e não alcança outra porta. Quem a roubar ainda precisa
  da senha do banco. O `permitopen` leva a porta real do banco (`5433`, por exemplo).

**No servidor de desenvolvimento, a TUI:**
- **gera a chave** ed25519 e mostra a linha pronta para o `authorized_keys`;
- também aceita **o caminho de uma chave que já existe**, e o `ssh-agent`;
- **com passphrase,** pede a passphrase na hora e a guarda só na memória;
- **sem passphrase,** aceita a chave, porque ela é restrita ao túnel;
- usa um **`known_hosts` próprio e estrito:**
  - na primeira conexão, mostra o fingerprint SHA256 do host e pede confirmação;
  - depois, uma chave diferente **bloqueia** a conexão.

## 5. Versões e imagens

### As regras do PostgreSQL

| regra | consequência |
|---|---|
| o `pg_dump` recusa servidor **mais novo** que ele | o cliente precisa ser da versão da origem ou mais novo |
| um `pg_restore` mais antigo **não lê** o arquivo de um `pg_dump` mais novo | o `pg_restore` 16 recusa o dump feito pelo 18 |
| o dump carrega em servidor de versão **igual ou mais nova**; em versão mais antiga, sem garantia | copiar "descendo" é arriscado |

### A regra da ferramenta

**Uma imagem só, a da maior versão entre a origem e o destino, faz o dump e o restore.**
**Copiar descendo de versão é bloqueado.**

| origem ↓ \ destino → | 16 | 17 | 18 |
|---|---|---|---|
| **16** | imagem 16 | imagem 17 | imagem 18 |
| **17** | bloqueado | imagem 17 | imagem 18 |
| **18** | bloqueado | bloqueado | imagem 18 |

**As opções que dependem da versão do cliente** (o `--filter`, por exemplo, só existe a partir do
17) ficam numa tabela única no código, coberta por teste.

### As imagens

- **O Docker é obrigatório.** Sem ele, o cadastro e o diagnóstico funcionam, e a cópia explica o
  que falta e não roda.
- **As imagens são as oficiais**, na variante padrão (Debian): `postgres:16`, `17` e `18`.
- **Ficam travadas pelo digest:**
  - a aba Ambiente baixa as três;
  - a TUI grava o digest e a versão exata de cada uma (`pg_dump --version` de dentro dela);
  - toda execução usa esse digest.
- **"Atualizar imagens"** baixa a tag de novo, mostra a versão atual e a nova, e pede
  confirmação. **A imagem antiga não é apagada.**
- **O registry é configurável:** Docker Hub por padrão, ou um espelho interno. Num servidor sem
  internet, basta um `docker save` numa máquina com internet e um `docker load` no servidor; a
  TUI reconhece a imagem carregada.
- **A lista de versões é configurável.** O 19 entra quando for lançado.

### Como o container roda

- `docker run --rm --network host`, com o nome `pghangar-<instância>-<execução>-<etapa>` e os
  labels `pghangar.execucao=<id>` e `pghangar.instancia=<instância>`. O label permite achar e
  parar containers órfãos, só desta instalação: outra instalação no mesmo Docker tem a sua própria
  execução 1.
- **O diretório do dump é montado** como volume.
- **As senhas entram por um `pgpass` temporário:**
  - o arquivo tem permissão `600` e é montado só para leitura (`PGPASSFILE`);
  - é apagado ao fim da execução (ou pela tela, se o processo morrer). Com a role temporária
    (§7), é o único que a ferramenta apaga sozinha: são artefatos dela, e não dados.
- **`PGAPPNAME=pghangar/<servidor>/<perfil>`:** quem olha o `pg_stat_activity` da produção
  vê de onde vem a carga.

## 6. Perfis

Um perfil é **uma cópia de um banco**. Vários bancos são vários perfis.

**Campos:**
- nome;
- **origem:** a conexão e o banco;
- **destino:** a conexão e o banco (que já existe, ou um novo);
- jobs paralelos (padrão 2 na origem);
- **tabelas sem dados:** padrões de nome cujas tabelas vêm só com a estrutura, sem as linhas
  (logs, auditoria), via `--exclude-table-data`;
- **script SQL pós-restore**, opcional (§7, etapa 8);
- o diretório dos dumps (padrão em §13).

**No formulário, os bancos se escolhem num seletor de etiquetas,** com a lista da última
verificação da conexão, sem digitar o nome:
- as letras filtram, o espaço marca e as setas andam entre as etiquetas. O `ctrl+r` verifica a
  conexão de novo e atualiza a lista;
- ficam de fora o `postgres` e os bancos da própria ferramenta (`__novo`, `__anterior`, `__base`).
  Um banco que não aceita conexões aparece apagado;
- um nome fora da lista continua valendo: o filtro sem par vira a etiqueta "usar …";
- no destino, a primeira etiqueta é "o mesmo nome da origem", e os bancos que já existem aparecem
  na cor de aviso, porque são substituídos;
- o nome do perfil vem preenchido com `<banco>-<destino>` até ser editado;
- **no perfil novo, marcar vários bancos cria um perfil por banco,** com as mesmas opções,
  chamados `<banco>-<destino>` e com o banco de destino de mesmo nome. Eles saem marcados como
  grupo, e o enter os copia em fila. Na edição, o seletor escolhe um banco só.

## 7. O motor: as etapas de uma cópia

As etapas, na ordem em que acontecem. A tela mostra as do motor (`motor.Etapas`: Checagens,
Anteriores, Dump, Criar `__novo`, Restore, Conferência, Script pós-restore, Donos, Configurações
do banco, ANALYZE, Base e Troca); a confirmação e a decisão acontecem na tela, entre elas. As que
não se aplicam a uma cópia aparecem puladas: sem anteriores marcados, sem script, destino que ainda
não existe, sem "guardar base".

1. **Checagens.** Nada é escrito até todas passarem. O processo da cópia refaz o plano e confere
   que o destino e a origem confirmados são os mesmos de agora (pelo `system_identifier`):
   - as conexões e as versões são relidas;
   - a combinação de versões é permitida, e a imagem necessária está baixada;
   - o destino não é `prod`, não é o banco administrativo nem um template, e origem e destino não
     são o mesmo cluster (pelo `system_identifier`, e não pelo hostname);
   - o usuário do destino é superusuário, e o destino não é réplica;
   - as extensões da origem estão disponíveis no destino (`pg_available_extensions`); versão
     diferente gera aviso;
   - diferenças de locale e de collation entre origem e destino geram aviso;
   - há espaço no diretório dos dumps, comparado com o tamanho do banco de origem;
   - a lista de bancos `__anterior` do destino, com tamanho (§8);
   - um `__novo` que sobrou de uma cópia anterior vira a correção que o apaga, quando é seguro
     (§10); senão, bloqueia;
   - as sessões ativas no destino, que vão ser derrubadas na troca;
   - a origem com event triggers, ou sem superusuário com tabelas com RLS, gera aviso ou bloqueio;
   - uma role temporária que sobrou de uma execução morta vira uma informação (a cópia a remove).
2. **Confirmação** (§10).
3. **Anteriores:** apaga os `__anterior` que o sysadmin marcou na confirmação, e só eles.
4. **Dump:**
   - `pg_dump -Fd -j N` para `dumps/<perfil>/<AAAAMMDD_HHMMSS>/dump`;
   - ao lado, um `manifesto.json`: origem, versões, imagem, início, fim, tamanho e estado
     (`incompleto` até o fim);
   - as tabelas sem dados saem aqui;
   - o túnel da origem fecha no fim do dump.
5. **Criar `<banco>__novo`** no destino, a partir do `template0`, com a codificação e o locale do
   banco de destino atual (ou os da origem, se o destino não existir). Ele nasce **fechado**: o dono
   é o superusuário da conexão, e `PUBLIC` não conecta. Durante o restore, tudo lá dentro é da role
   temporária de superusuário, e uma função `SECURITY DEFINER` vinda da origem seria um atalho para
   superusuário a qualquer login do servidor. Cria também a **role temporária**
   `pghangar_<instância>_<execução>`, `SUPERUSER NOLOGIN`.
6. **Restore:** `pg_restore -j N --no-owner --no-privileges --no-tablespaces --no-subscriptions
   --no-publications --role=<role temporária>`.
   - Tudo nasce com a role temporária, que é superusuário: extensões, event triggers e o resto
     restauram sem depender do dono do destino.
   - As roles da produção não vão para o destino. Assinaturas e publicações também não.
   - **Um restore com erro não troca nada sem perguntar** (etapa 12).
7. **Conferência,** logo depois do restore (o script da etapa seguinte pode criar objetos de
   propósito): o número de objetos por tipo, sem os que pertencem a extensões, entre a origem e o
   `__novo`. As restrições NOT NULL ficam de fora: a partir do 18 elas também aparecem em
   `pg_constraint`, e uma cópia 16 → 18 divergiria sem diferença nenhuma.
8. **Script pós-restore**, se houver, **no `__novo`, antes da troca,** com `psql
   ON_ERROR_STOP=1` e a role temporária (`PGOPTIONS=-c role=…`): ele tem os poderes de
   superusuário, e o que ele criar passa ao dono na etapa seguinte. A aplicação nunca vê o banco
   com as configurações da produção: jobs ligados, URLs, e-mails. Um erro no script para a cópia.
9. **Donos:** `REASSIGN OWNED BY <role temporária> TO <dono do destino>`, `DROP OWNED` e
   `DROP ROLE`. Event triggers, que só um superusuário pode ter, ficam com o usuário da conexão
   quando o dono não é superusuário, e o mesmo vale para os foreign-data wrappers. Se a role não
   puder ser removida, ela perde o superusuário. Só então o `__novo` **passa ao dono do destino e
   abre** como um banco novo abriria (`CONNECT` e `TEMP` para `PUBLIC`).
10. **Reaplicar no `__novo` o que é do banco, e não do dump,** copiando do destino atual:
    - `ALTER DATABASE … SET`, inclusive o que vale por role (as listas, como `search_path`,
      elemento por elemento);
    - as permissões do banco (`REVOKE ALL FROM PUBLIC` e os `GRANT`s do destino atual);
    - o limite de conexões e o comentário.

    O destino continua com as configurações dele, e não com as da produção.
11. **`ANALYZE`** (`vacuumdb --analyze-only -j N`).
12. **Decisão:** se o restore teve erro ou a conferência divergiu, a execução para em
    **"aguardando decisão"**. O `__novo` fica pronto; o sysadmin lê o log e escolhe entre **trocar
    mesmo assim** e **não trocar** (o `__novo` fica para ele apagar).
13. **Troca:**
    - `ALLOW_CONNECTIONS false` no `__novo` e no destino, e as sessões derrubadas (até 10 s);
    - renomear `<banco>` para `<banco>__anterior_<AAAAMMDD_HHMMSS>`;
    - renomear `<banco>__novo` para `<banco>` e abrir as conexões.

    Se um passo falhar, os anteriores são desfeitos. O `__anterior` fica com
    `ALLOW_CONNECTIONS false`, para que nada escreva nele por engano.
14. **Histórico:** a execução é registrada com o resultado de cada etapa.

**Nomes longos:** o PostgreSQL corta nomes em 63 bytes. Se `<banco>__anterior_<data>` passar
disso, a base do nome é encurtada com um hash curto do nome original: a mesma entrada dá sempre a
mesma base, e a busca pelos anteriores continua exata.

**O que um processo morto deixa, e quem limpa:** um `kill -9` no meio da cópia não chega à
limpeza. A tela marca a execução como **interrompida** e apaga o `pgpass` temporário dela. A role
temporária que sobrou (já sem uso) é neutralizada e removida pela próxima cópia daquela instalação
para aquele servidor, ou quando o `__novo` que ela possui é apagado na aba Anteriores. Um container
que ficou rodando aparece como **órfão** na aba Ambiente.

## 8. Bancos `__anterior` e o desfazer

- **Nome:** `<banco>__anterior_<AAAAMMDD_HHMMSS>`, com `ALLOW_CONNECTIONS false`.
- **Nunca são apagados sozinhos, e a TUI sempre pergunta.** Na confirmação de cada cópia, ela
  lista os `__anterior` daquele destino, com tamanho e idade, e pergunta quais apagar antes de
  começar. **Nenhum vem marcado.** Apagar libera o disco antes do restore.
- **A aba Anteriores** lista os de todos os destinos. Nela: `u` desfaz, e `d` apaga, com
  confirmação.
- **Desfazer não perde nada:** o banco atual também vira `__anterior_<data>`, e o escolhido volta
  a ser o oficial, com conexões liberadas.
- **Um `__novo` de uma cópia que falhou ou foi cancelada fica.** A TUI pergunta se o apaga. A
  próxima cópia do mesmo destino não começa sem resolver isso.

## 9. Dumps

- **Onde ficam:** `dumps/<perfil>/<AAAAMMDD_HHMMSS>/`, cada um com o seu manifesto.
- **Nunca são apagados sozinhos.**
- **A aba Dumps:**
  - lista cada dump com tamanho, origem, versão e estado (`completo` ou `incompleto`);
  - **`r` restaura de novo**, sem fazer outro dump;
  - `d` apaga, com confirmação.
- **Restaurar um dump antigo segue a regra de versões do §5:** a imagem é a maior entre a versão
  do `pg_dump` que gerou o dump e a versão do destino.
- **Um arquivo de fora** (um backup, um dump que alguém mandou) entra pela pasta de entrada, que a
  mesma aba lista em cima dos dumps: §17.

## 10. Confirmações e guardas

**Bloqueios fixos, sem opção para desligar:**
- um destino `prod`, ou **qualquer conexão que aponte para o servidor de uma conexão `prod`**,
  pelo `system_identifier` ou pelo endereço (a produção cadastrada de novo, sem a tag, por engano).
  O formulário de conexão não tem tag padrão: ela é sempre escolhida;
- origem e destino no mesmo cluster;
- cópia que desce de versão.

**A confirmação depende da tag do destino:**

| destino | confirmação |
|---|---|
| `dev` | `y` (o enter não confirma: o plano aparece sozinho quando fica pronto, e um enter dado para outra coisa não pode substituir o banco) |
| `homolog` | digitar o nome do banco de destino |

**A tela de confirmação mostra:**
- a origem: conexão, banco, versão e tamanho;
- o destino: os mesmos dados, e **"vai ser SUBSTITUÍDO"**;
- a imagem e o diretório do dump;
- os `__anterior` do destino, com a pergunta do §8;
- as sessões que vão ser derrubadas;
- o script pós-restore;
- **as correções,** abaixo.

**As correções: o que a cópia resolve sozinha no destino, se o sysadmin deixar marcado.** Elas
aparecem na confirmação, e o mesmo `y` (ou o nome do banco) que confirma a cópia as aplica. O espaço
marca ou desmarca cada uma, e uma correção desmarcada diz o que acontece sem ela.

| correção | padrão | quando |
|---|---|---|
| apagar o `__novo` que sobrou de uma cópia anterior | marcada; desmarcada, bloqueia a cópia. Só é oferecida quando é seguro: o `__novo` tem a marca desta instalação (o comentário que ele recebe ao nascer), a cópia dele já terminou e não espera a troca, e ninguém está conectado nele. Senão, bloqueia, como antes | antes de tudo |
| criar no destino as roles que a RLS e os user mappings da origem citam, **sem login e sem senha** | marcada | antes do restore |
| tirar do banco copiado os **user mappings** (as credenciais) dos servidores externos (`postgres_fdw` e afins) | **sempre desmarcada** | depois do restore, antes da troca |

- Só no destino e depois das guardas: nunca num servidor `prod`, nunca na origem. As roles criadas
  valem para o servidor inteiro e ficam depois da cópia. Instalar pacote ou reiniciar serviço fica
  de fora: vira um aviso com a orientação.
- Na execução, uma correção só vale como foi confirmada: os mesmos nomes e, no `__novo`, o mesmo
  banco (o OID). Uma marcada que mudou entre a confirmação e a execução para a cópia, que pede
  outra confirmação. Na hora de apagar o `__novo`, a execução confere de novo o OID e as sessões.
- Sem a tela (`pghangar rodar`, o cron), ninguém disse sim: nenhuma é aplicada.
- Cada correção aplicada vai para o log e para as INFORMAÇÕES da execução ("corrigido: …").
- No grupo, as correções seguem o padrão do plano, menos a do `__novo`: num grupo nada é apagado,
  e o `__novo` que sobrou bloqueia o perfil. A confirmação do grupo lista as correções.
- A restauração de um dump guardado oferece as mesmas, com a lista dos servidores externos
  guardada no manifesto do dump. A de um arquivo de fora oferece a do `__novo` e a dos user
  mappings (§17).

## 11. A execução separada da tela

- **A cópia roda num processo separado:** o mesmo binário, no subcomando `executar`, solto do
  terminal (`setsid`).
- **O processo grava** o estado no cadastro e o log em arquivo.
- **O processo é dono do túnel SSH** da origem (§4): ele abre, mantém e fecha o túnel.
- **A TUI só acompanha:** fecha e reabre sem afetar a cópia.
- **Uma cópia por destino** (o servidor, pelo `system_identifier`, e o banco): uma trava impede a
  segunda, mesmo que as duas usem conexões com nomes diferentes para o mesmo servidor.
- **Cancelar** para o container (pelo label) e marca a execução como cancelada. O `__novo` fica,
  e a TUI pergunta (§8).

**O progresso aparece por etapa:**
- no dump, as tabelas concluídas contra o total lido no catálogo da origem;
- no restore, os objetos concluídos contra o índice do dump (`pg_restore -l`).

**Sem a tela, para scripts e cron:** `pghangar rodar <perfil>`.
- Num destino `homolog`, é obrigatório `--confirmar <banco>`.
- **Nunca apaga nada:** um `__novo` que sobrou impede a execução, e isso é informado. Nenhuma
  correção é aplicada sem a tela.
- Ao começar, marca como interrompidas as execuções cujo processo morreu, como a tela faz: quem só
  usa o cron também tem o que sobrou limpo.

## 12. Cuidados com a produção

- **O `pg_dump` segura um lock leve** (`ACCESS SHARE`) em cada tabela até o fim. Uma migration na
  produção durante o dump fica esperando, e as consultas seguintes fazem fila atrás dela. A
  confirmação lembra isso, e os jobs na origem têm padrão 2.
- **Toda sessão com a origem começa só de leitura** (`default_transaction_read_only=on`, pelo
  `options` da conexão): a conexão administrativa, o `pg_dump`, a contagem de linhas e a leitura em
  blocos. O diagnóstico também, em qualquer conexão. É uma rede contra um bug, e não contra quem
  mexer no código, porque uma sessão pode desligar o padrão. A garantia do lado do servidor é um
  usuário sem escrita na origem. O plano confere que a sessão ficou mesmo só de leitura e avisa se não ficou (um
  pooler na frente pode ignorar o `options`).
- **Com uma réplica disponível, puxar dela** tira a carga do primário. Um dump longo numa réplica
  pode ser cancelado por conflito de recuperação. O plano avisa disso antes da cópia.

## 13. Onde ficam as coisas

Tudo sob `/var/lib/pghangar` (a opção `--dir` troca o diretório). A ferramenta **exige root**
e **se recusa a abrir** se as permissões estiverem frouxas, como o OpenSSH.

| caminho | o quê | permissão |
|---|---|---|
| `estado.db` | conexões, senhas, perfis, imagens, histórico (SQLite, com `secure_delete`) | `600` |
| `chaves/` | chaves SSH geradas pela ferramenta | `700` / `600` |
| `known_hosts` | as chaves conferidas dos hosts SSH | `600` |
| `dumps/` | os dumps, por perfil | `700` |
| `entrada/` | os arquivos de fora, para restaurar num dev ou homolog (§17) | `700` |
| `logs/<execução>.log` | o log completo de cada execução | `600` |

## 14. Telas

Seguem o padrão do pgtower e do `9level-id-dr`: cabeçalho, abas numeradas, rodapé com as teclas
da aba, e `?` para a ajuda. O cabeçalho mostra o nome, a versão, o host e a execução em
andamento.

| aba | o que tem |
|---|---|
| **1 Perfis** | os perfis com o resultado da última cópia ("ontem 14:02 · OK · 3m12s · 1,2 GB"). **Enter copia** |
| **2 Execuções** | a cópia em andamento, com o progresso por etapa, e o histórico com o log |
| **3 Conexões** | as conexões com o estado de cada uma. `a` adiciona, `e` edita, `d` remove, `t` testa |
| **4 Dumps** | a pasta de entrada (os arquivos de fora) e os dumps guardados. `r` restaura (o arquivo num dev ou homolog; o dump de novo), `d` apaga |
| **5 Anteriores** | os bancos `__anterior`. `u` desfaz, `d` apaga |
| **6 Ambiente** | o Docker, as imagens (baixar e atualizar) e a chave SSH pública deste servidor |

**O cartão da execução vem em blocos,** do mais importante para o menos:
1. **o estado**, com uma faixa na cor dele: em andamento (a etapa e a barra), na fila, concluída
   (o que foi feito, quanto levou, o tamanho do dump, o que aconteceu com o banco que estava lá),
   esperando a decisão, cancelada ou com erro (a etapa e o motivo);
2. as etapas e os detalhes (operador, dump, `__novo`, anterior);
3. **ATENÇÃO:** o que pode dar errado ou mudar o resultado;
4. **INFORMAÇÕES:** o que vale saber, sem pedir atenção.

A tela do plano separa os mesmos dois níveis antes da confirmação. Quando uma execução termina, o
rodapé diz como ela terminou ("✔ #3 loja concluída em 3m12s").

## 15. Estrutura do código

**A stack:**
- Go, com `bubbletea`, `bubbles` e `lipgloss`;
- `pgx`, para as consultas de diagnóstico e de checagem;
- `modernc.org/sqlite`;
- `golang.org/x/crypto/ssh`;
- o Docker pela linha de comando (`docker`), sem o SDK.

| pacote | responsabilidade |
|---|---|
| `cmd/pghangar` | a linha de comando: `tui` (o padrão), `rodar`, `executar` |
| `internal/cadastro` | o SQLite: conexões, perfis, imagens, execuções |
| `internal/conexao` | conectar e fazer o diagnóstico em camadas |
| `internal/tunel` | o túnel SSH, as chaves e o `known_hosts` |
| `internal/versoes` | a regra de versões (§5) e a tabela de opções por versão |
| `internal/imagens` | baixar, conferir e rodar as imagens |
| `internal/motor` | as etapas do §7, cada uma emitindo eventos de progresso |
| `internal/execucao` | o processo separado, a trava, o log e o cancelamento |
| `internal/tui` | as telas |

**Os testes:**
- testes unitários;
- **testes de integração** com Postgres 16, 17 e 18 em containers: as **9 combinações** (6 que
  copiam e 3 bloqueadas), a troca e o desfazer, e o restore com extensões.

## 16. Fases

| fase | entrega |
|---|---|
| | **As fases 1, 2 e 3 estão prontas** (2026-09-29), testadas em localhost e, desde 2026-09-30, em uso contra um servidor real. Ficaram de fora da 3: o modo rápido com pgcopydb (exige outra imagem, e o link instável já cobre o problema principal) e a anonimização (não é necessária). |
| **1** | conexões com diagnóstico e leitura da versão; **túnel SSH com chaves, `known_hosts` e instruções para o host da produção**; imagens (baixar e travar); perfis com tabelas sem dados e script pós-restore; o motor completo; confirmações; `__anterior` com pergunta e desfazer; aba Dumps; execução separada; histórico |
| **1+** | as melhorias depois da pesquisa de mercado (`docs/MERCADO.md`): compressão zstd/lz4; filtro de schemas e tabelas (com `--extension=*`); **servidores de destino aprovados** (tecla `v`); checagens de disco local e de roles citadas pela RLS; quem rodou (o login por trás do sudo); os bancos por sugestão no formulário (`ctrl+n`; depois, o seletor com filtro do §6); o dump refeito sozinho quando o túnel cai |
| **2** | atualizar imagens (`u`, com confirmação; as antigas ficam); restaurar um dump guardado (aba 4, `r`, sem ir à origem); `pghangar rodar` para o cron; **contagem exata de linhas no mesmo snapshot do dump** (`pg_export_snapshot` + `pg_dump --snapshot`) |
| **3** | **dump retomável para link instável** (abaixo); aviso ao terminar por webhook; **grupos de perfis** em fila (espaço marca, enter copia); **bastion**; **banco base e "resetar da base"** (`z`) |
| **3+** | **restaurar um arquivo de fora** num dev ou homolog (§17), pela aba 4 ou pelo `pghangar restaurar` |

### Sem a tela: `pghangar rodar`

`pghangar rodar [--confirmar BANCO] PERFIL` roda uma cópia no primeiro plano, para o cron ou um
timer do systemd.
- **Não pergunta nada:** senha "perguntar", passphrase e servidor SSH desconhecido viram erro, e se
  resolvem pela tela antes.
- **Num destino homolog, `--confirmar` com o nome do banco é obrigatório.**
- **Nunca apaga anteriores:** ao fim, lembra quantos há e quanto ocupam.
- **Sai com 0** (ok), **2** (a troca espera a decisão, pela tela) ou **1** (erro).

### Grupos

Na aba 1, o espaço marca perfis; enter planeja todos e mostra o plano do grupo. As cópias rodam **em
fila, uma de cada vez**, num único processo separado (para não carregar a origem com vários dumps ao
mesmo tempo). Uma que falha não para as outras. Cancelar para a atual e as que esperam. Num grupo,
nada é apagado (a pergunta dos anteriores fica para cada cópia sozinha, e a correção do `__novo`
que sobrou fica desmarcada), e dois perfis no mesmo banco de destino são recusados. Com um destino
homolog no grupo, a confirmação é `copiar` digitado. Um perfil marcado sozinho é uma cópia comum,
com a confirmação dela.

### Banco base e reset

Um perfil com "guardar base" deixa, a cada cópia, `<banco>__base` no destino: criado do `__novo`
antes da troca (`CREATE DATABASE … TEMPLATE … STRATEGY FILE_COPY`), fechado para conexões, com o
comentário de que cópia veio. A base anterior é substituída, e o plano diz isso antes da
confirmação. Ela só é apagada depois de a nova existir: enquanto isso, fica como
`<banco>__base_velha`, e se a nova falha (o disco cheio), a anterior volta e a troca espera a
decisão do sysadmin, com o `__novo` pronto. Um `__base` que é o banco de um perfil nunca é
substituído. A tecla `z` recria o destino a partir da base, **sem ir à origem nem ao Docker**: o
`__novo` vem da base, ganha as configurações do destino de agora e troca de nome como numa cópia (com
desfazer). No PostgreSQL 18, com `file_copy_method = clone` num sistema de arquivos com reflink, isso
leva milissegundos.

### Fase 3: dump retomável (inspirado no Dolly), implementado

**O problema:** a produção só é alcançável por SSH, muitas vezes por um link longo. Hoje, se o túnel
cai no meio do dump de um banco grande, o `pg_dump` falha e a cópia recomeça do zero. O `pg_dump`
não sabe retomar.

**Como o Dolly resolve** (`github.com/VicenteOlmos/dolly`, MIT, `internal/dump`; lido em
2026-09-29):
- **Blocos por chave:** cada tabela sai em blocos pela chave primária, ou por um índice único `NOT NULL`
  válido (sem predicado nem expressão), com paginação por chave (`WHERE (k1, k2) > ($1, $2) ORDER BY
  k1, k2 LIMIT n`) e nunca por `OFFSET`.
- **Tabela sem chave segura:** cai no `ctid`, com aviso de que um `VACUUM` ou um `UPDATE` pode pular ou
  repetir linhas. Uma opção recusa esse caso.
- **Checkpoint por tabela:** a última chave gravada e uma impressão digital do plano (índice, colunas,
  opclass, collation). Se o plano mudou, a retomada é recusada.
- **Tentativas:** backoff exponencial (com limite de 30 s), refazendo só o bloco em andamento.
- **O preço declarado:** não há um snapshot único. Cada reconexão vê um momento diferente da origem.

**Como ficou aqui** (`internal/motor/blocos.go`): um modo **"link instável"** por perfil, que troca
só o dump dos dados:
1. **O esquema:** `pg_dump --schema-only`, que é pequeno e rápido, dividido nas seções `pre-data` e
   `post-data` do `pg_restore`.
2. **Os dados:** por tabela, em blocos por chave, com `COPY (SELECT … WHERE chave > … ORDER BY chave
   LIMIT n) TO STDOUT`. Cada bloco é um arquivo, com checkpoint por tabela, e o túnel é reaberto
   sozinho a cada queda, com backoff.
3. **O restore:** `pre-data` no `__novo`; os blocos com `COPY FROM`; `post-data` (índices, restrições e
   chaves estrangeiras); as sequências com o valor lido no fim.
4. **A consistência:** sem snapshot único, uma chave estrangeira pode falhar no `post-data`. As saídas,
   a decidir:
   - **Dump de uma réplica com o replay pausado** (`pg_wal_replay_pause()`): todas as reconexões veem o
     mesmo momento, e a consistência volta;
   - ou as chaves estrangeiras como `NOT VALID`, com o relatório das violações na conferência.
5. **Sem chave segura:** o `ctid` só com opt-in explícito no perfil, como no Dolly.

**As escolhas da implementação:**
- **Sem `ctid`:** uma tabela sem chave segura vai inteira, num bloco só, e retoma no nível da tabela.
  Não há o risco de pular ou repetir linhas.
- **Retomar depois de o processo morrer:** a próxima cópia do mesmo perfil retoma o dump em blocos
  incompleto (a mesma origem e o mesmo recorte). Uma tabela cuja estrutura mudou recomeça do zero.
  Os blocos ficam num diretório por tabela, pelo nome, e não pela posição.
- **A consistência:** sem snapshot único. Uma chave estrangeira que não bata aparece como erro no
  restore, e a troca espera a decisão do sysadmin. A réplica com replay pausado ficou de fora: um
  processo morto deixaria a réplica parada.
- **O que não vai:** os large objects (o plano avisa).
- **A conferência** compara as linhas do `__novo` com as que o dump trouxe.

**O passo intermediário também existe:** no modo normal (`pg_dump -Fd`), o dump que cai por rede é
refeito sozinho (3 tentativas, com backoff), e a tentativa que caiu fica no disco como
`dump.incompleto-N`.

## 17. Restaurar um arquivo de fora

**O caso:** um backup, ou um dump que alguém mandou, precisa virar um banco de desenvolvimento ou
de homologação. Sem a ferramenta, é um `pg_restore` à mão, sem as guardas de uma cópia.

**Como funciona:**
1. **O arquivo vai para a pasta de entrada,** `/var/lib/pghangar/entrada` (`sudo mv`; um link
   também vale). Um upload direto para lá (`scp`, `cp` de outro disco) escreve no nome final e
   aparece na lista antes de terminar: copie com um nome oculto (`.loja.dump.part`) e renomeie no
   fim, ou use `rsync`, que já faz assim. Os ocultos ficam de fora da lista e são recusados. A aba 4
   lista a pasta em cima dos dumps, com o formato, o tamanho e o que o cabeçalho diz (o banco de onde
   veio, as versões e a data).
2. **`r` no arquivo** pede a conexão (só dev e homolog), o banco (o seletor dos perfis; vem
   sugerido o banco do cabeçalho, ou o nome do arquivo) e os jobs (no tar e no SQL, só o ANALYZE os
   usa). A leitura de um SQL grande antes do plano pode levar minutos: o esc a cancela.
3. **O plano** faz as guardas de uma cópia no destino (§10), escolhe a imagem pela versão do arquivo
   (§5), confere as extensões que o arquivo cria e, num SQL, lê o arquivo inteiro com as regras
   abaixo. A confirmação é a de uma cópia: `y` num dev, o nome do banco num homolog.
4. **A execução** é a de uma cópia sem o dump e sem a conferência: o `__novo` com a role temporária,
   o restore, os donos, as configurações do banco de agora, o ANALYZE e a troca. O banco que estava
   lá vira `__anterior` (a aba 5 desfaz ou apaga). O arquivo só é lido e continua na pasta.

| formato | reconhecido por | restaura com |
|---|---|---|
| custom (`-Fc`) | `PGDMP` no começo | `pg_restore`, em paralelo |
| diretório (`-Fd`) | um diretório com o `toc.dat` | `pg_restore`, em paralelo |
| tar (`-Ft`) | um tar que começa com o `toc.dat` | `pg_restore`, um job |
| SQL puro, com ou sem gzip | texto, ou o gzip dele | `psql`, pela entrada padrão, um job |

**O SQL puro** vai ao `psql` pela entrada padrão do container, já descomprimido, conferido comando
a comando (o texto entre dois `;` de fora de string, de `$$` e de comentário; os dados de um `COPY`
não são conferidos):
- **recusado:** roles, tablespaces, `ALTER SYSTEM`, `REASSIGN`/`DROP OWNED`, `LOAD`, `COPY … PROGRAM`,
  um segundo banco e qualquer comando do `psql`, em qualquer lugar da linha. Passam só o
  `\restrict`/`\unrestrict` do pg_dump e o `\connect` do banco de um `-C`, sozinhos na linha;
- **pulado:** os comandos do próprio banco de um `pg_dump -C`, as subscriptions e as publications
  (viram espaços, e o número das linhas nos erros continua o do arquivo);
- **cortado:** um `COPY` sem o `\.`, uma string aberta no fim ou um dump sem a linha final do
  pg_dump bloqueiam o plano e param o restore antes da troca;
- **como estão:** os donos e as permissões. Uma role que falta vira erro, e a troca espera a
  decisão (as mensagens do servidor vêm em inglês, para os erros serem contados). O formato custom
  entrega tudo ao dono do destino e restaura em paralelo: é o melhor para quem gera o arquivo.

Os user mappings de um arquivo (do índice ou dos `CREATE USER MAPPING`) têm a correção de uma
cópia (§10), desmarcada. Não é um sandbox: o arquivo roda com superusuário no destino, como num
restore à mão, e o plano lembra de restaurar só arquivos de fonte confiável.

**Sem a tela:** `pghangar restaurar --destino CONEXAO --banco BANCO [--jobs N] [--confirmar BANCO]
ARQUIVO`, em que ARQUIVO é o nome na pasta de entrada. As regras e as saídas são as do `rodar`.
