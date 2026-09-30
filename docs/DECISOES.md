# Decisões

Cada entrada diz o que foi decidido e por quê. Quando uma decisão muda, ela ganha uma entrada
nova, e a antiga continua aqui, marcada como substituída.

---

## 2026-09-29: Projeto separado do `copy-db-prod`

**Decisão do usuário.**

**Decidido:** o `copia-banco` é um projeto novo, em `/docker/copia-banco`. Os projetos vizinhos
(`/docker/copy-db-prod`, `/docker/pgtui`) servem só de referência e não são alterados.

**Por quê:** o `copy-db-prod` é o monitor de DR do 9level-id. Ele só lê os hosts e, por regra,
fica fora do caminho dos dados. Uma ferramenta que escreve em bancos tem outro propósito.

## 2026-09-29: Ferramenta de sysadmin: root e superusuário

**Decisão do usuário.**

**Decidido:** a ferramenta roda como root e conecta com um superusuário, na origem e no destino.
Ela não desenha roles de menor privilégio.

**Por quê:** é uma ferramenta de alto nível para o sysadmin, que responde pelos servidores.

**Consequência:** as checagens de privilégio e o caminho alternativo sem `CREATEDB` saem do
desenho. A troca por `__novo`/`__anterior` é sempre possível.

## 2026-09-29: Nada é apagado automaticamente

**Decisão do usuário.**

**Decidido:** nem dumps, nem bancos `__anterior`, nem bancos `__novo` que sobraram. Tudo fica até
alguém mandar apagar pela TUI.

**Por quê:** a proteção do servidor e o uso do disco são responsabilidade do sysadmin.

**A única exceção** é o `pgpass` temporário de cada execução. Ele é um segredo da própria
ferramenta, e não dado.

## 2026-09-29: Bancos `__anterior`: sempre perguntar

**Decisão do usuário.**

**Decidido:** cada cópia guarda o destino antigo como `<banco>__anterior_<AAAAMMDD_HHMMSS>`. Na
confirmação de cada cópia, a TUI lista os `__anterior` daquele destino e pergunta quais apagar,
**sem nenhum marcado**.

**Por quê:** sem apagamento automático, o disco do destino cresce a cada cópia. A pergunta faz a
decisão acontecer sempre, sem que ela seja tomada no lugar do sysadmin.

## 2026-09-29: `pg_dump` e `pg_restore` dentro de containers

**Decisão do usuário.**

**Decidido:**
- as imagens oficiais `postgres:16`, `17` e `18` ficam baixadas no servidor, e cada execução usa
  a que a regra de versões pede;
- os bancos ficam **fora** do Docker, no mesmo host ou em outros, e o container conecta com
  `--network host`.

**Por quê:** as três versões do cliente convivem sem instalar pacotes no servidor, e cada
execução fica isolada.

## 2026-09-29: As imagens: variante padrão, travadas pelo digest

**Recomendação aceita pelo usuário.**

**Decidido:**
- a variante é a padrão da imagem oficial (Debian);
- a TUI grava o digest de cada imagem baixada, e toda execução usa o digest gravado;
- **"atualizar imagens"** é uma ação manual, com confirmação, e a imagem antiga não é apagada;
- o registry é configurável, e um servidor sem internet usa `docker save` e `docker load`;
- **o Docker é obrigatório na v1.** Sem ele, a TUI não copia, e os clientes nativos não são usados.

**Por quê:** a mesma versão do cliente em todas as execuções, até que o sysadmin decida atualizar.
Com um só caminho de execução, a matriz de testes cobre tudo o que roda.

## 2026-09-29: Uma imagem, a da maior versão, faz o dump e o restore; descer é bloqueado

**Recomendação aceita pelo usuário.**

**Decidido:** a imagem da maior versão entre a origem e o destino roda o `pg_dump` e o
`pg_restore`. Copiar de uma versão mais nova para uma mais antiga (18 → 16, por exemplo) é
bloqueado.

**Por quê:**
- um `pg_restore` mais antigo não lê o arquivo de um `pg_dump` mais novo;
- a documentação do PostgreSQL indica o cliente mais novo para levar dados a uma versão mais nova;
- um dump não tem garantia de carregar numa versão mais antiga.

**Descartado:** cada lado com a sua versão. Funciona quando a versão sobe, e falha na leitura do
arquivo quando ela desce.

## 2026-09-29: A versão é gravada no cadastro e relida a cada execução

**Decisão do usuário.**

**Decidido:** ao salvar uma conexão, a TUI grava a versão e o estado do servidor. A cada execução,
ela os relê. Se a versão mudou, a TUI avisa, atualiza o cadastro e escolhe a imagem pela versão de
agora.

**Por quê:** a versão gravada serve para mostrar e planejar. A que decide é a de agora: um servidor
pode ser atualizado entre duas cópias.

## 2026-09-29: O destino é substituído por troca de nomes

**Recomendação aceita pelo usuário.**

**Decidido:** o restore vai para `<banco>__novo`. No fim, o destino atual vira `__anterior` e o
`__novo` assume o nome.

**Por quê:**
- o destino continua no ar durante a cópia;
- uma falha no meio não estraga o destino;
- desfazer é trocar os nomes de novo.

**Descartados:**
- `--clean` em cima do banco existente, que deixa para trás os objetos que a origem não tem;
- `DROP` + `CREATE`, que não tem volta.

## 2026-09-29: Tudo nasce com o dono do destino

**Decidido pelo assistente,** dentro do "siga as recomendações" do usuário. É a opção (b) da
análise, recomendada desde a primeira resposta. **O método (`--role=<dono>` e extensões criadas
antes) foi SUBSTITUÍDO** pela entrada "O restore roda com uma role temporária", logo abaixo; o
resultado (tudo com o dono do destino) continua.

**Decidido:** o restore usa `--no-owner --no-privileges --role=<dono do destino>`. As extensões
são criadas antes, pelo superusuário. Assinaturas, publicações e tablespaces da produção não vão
para o destino.

**Por quê:**
- a aplicação do homolog continua dona do seu banco, e as migrations dela rodam;
- as roles e as senhas da produção não vão para o homolog;
- uma assinatura copiada tentaria replicar da produção.

**O preço:**
- objetos que só um superusuário cria (event triggers, funções em C) falham no restore. As
  checagens avisam antes;
- os `GRANT`s de outras roles do destino vão no script pós-restore.

## 2026-09-29: O restore roda com uma role temporária, e tudo passa ao dono no fim

**Decidido pelo assistente,** na implementação. **Substitui** o método da entrada anterior.

**Decidido:** a cópia cria `pghangar_<instância>_<execução>` (antes do nome novo, `copia_banco_…`),
`SUPERUSER NOLOGIN`, restaura com `--role` dela, roda o script pós-restore com ela, e no fim faz
`REASSIGN OWNED` para o dono do destino, `DROP OWNED` e `DROP ROLE`. Event triggers ficam com o
usuário da conexão quando o dono não é superusuário.

**Por quê:** restaurar direto como o dono (não superusuário) falharia em extensões não confiáveis,
event triggers e outros objetos restritos; criar as extensões antes exigiria prever os schemas
delas. Como superusuário, o restore passa inteiro, e o `REASSIGN` entrega tudo ao dono de uma vez,
inclusive o que o script criar. A role não tem login e ninguém é membro dela: só um superusuário
poderia assumi-la.

**A instância no nome:** cada instalação gera 6 hex aleatórios uma vez. Duas instalações (dois
servidores de dev) apontando para o mesmo servidor de destino nunca mexem na role uma da outra.

## 2026-09-29: O que um processo morto deixa, e quem limpa

**Decidido pelo assistente,** a partir do teste com `kill -9`.

**Decidido:**
- a tela marca a execução como **interrompida** e apaga o `pgpass` temporário dela;
- a role temporária que sobrou é **neutralizada** (`NOSUPERUSER NOLOGIN`) **e removida** pela próxima
  cópia daquela instalação para aquele servidor, ou quando o `__novo` que ela possui é apagado. As
  roles de execuções ainda em andamento (cópias para outros bancos do mesmo servidor) ficam de fora;
- o plano avisa quando há uma role assim.

**Por quê:** a exceção ao "nada é apagado sem pergunta" vale para os artefatos da própria
ferramenta, e um segredo ou uma role de superusuário esquecidos são o pior que pode sobrar.

## 2026-09-29: A conferência vem logo depois do restore, sem NOT NULL

**Decidido pelo assistente,** a partir dos testes.

**Decidido:** a conferência roda antes do script pós-restore e não conta as restrições NOT NULL.

**Por quê:** o script cria objetos de propósito (uma tabela de configuração do homolog), o que
divergiria sempre. E, a partir do PostgreSQL 18, NOT NULL também aparece em `pg_constraint`: uma
cópia 16 → 18 divergiria sem diferença real (achado da matriz de versões).

## 2026-09-29: O nome do anterior leva os segundos

**Decidido pelo assistente.**

**Decidido:** `<banco>__anterior_<AAAAMMDD_HHMMSS>`. Se o nome já existir (desfazer logo depois de
uma troca), a ferramenta espera o segundo seguinte.

**Por quê:** com só os minutos, desfazer no mesmo minuto da cópia colidiria.

## 2026-09-29: As fases 2 e 3 e as melhorias depois do mercado

**Decisão do usuário** (fazer tudo); os detalhes abaixo foram **decididos pelo assistente** na
implementação.

- **Servidores de destino aprovados:** a terceira barreira. A aprovação é pelo `system_identifier`
  (vale para qualquer conexão que chegue no servidor), pede o nome da conexão digitado, e guarda
  quem aprovou. Sem ela, a cópia é bloqueada.
- **Quem rodou:** o `SUDO_USER`, ou o login de `/proc/self/loginuid` (que o sudo não troca), e o IP
  do `SSH_CONNECTION`.
- **Filtros:** com `-n` ou `-t`, o dump leva `--extension=*`, porque sem isso as extensões ficam
  de fora (conferido no `pg_dump` real). Com a lista de tabelas, a conferência não compara os
  objetos (só as linhas, se ligadas).
- **Contagem de linhas:** num snapshot exportado (`pg_export_snapshot`), que o `pg_dump` adota
  (`--snapshot`). Assim as duas veem o mesmo momento, e a contagem roda depois do dump.
- **Repetição do dump:** só para falhas de rede (túnel, conexão). A tentativa que caiu não é
  apagada: fica como `dump.incompleto-N`.
- **Atualizar imagens:** pede confirmação, e as antigas ficam no Docker.
- **Restaurar um dump guardado:** o destino é o do perfil do dump, e o recorte, as linhas e a versão
  vêm do manifesto. A imagem é a maior entre a do `pg_dump` que gerou o dump e a do destino.
- **`rodar`:** não pergunta, nunca apaga e tem códigos de saída próprios (0, 2 e 1).
- **Aviso por webhook:** POST em JSON com `text` (Slack, Mattermost, Teams). A URL nunca vai para o
  log nem para a tela inteira: o caminho costuma ser o segredo.
- **Bastion:** a mesma chave e o mesmo `known_hosts` (a chave do bastion também é conferida). As
  instruções do `authorized_keys` do bastion só deixam passar até o SSH do servidor.
- **Grupos:** em fila, num processo, sem apagar nada. A confirmação com homolog é `copiar` digitado.
- **Banco base:** criado do `__novo` antes da troca, e fechado. A base antiga é substituída com o
  aviso no plano. O reset é uma troca como as outras: tem anterior e desfazer.
- **Link instável:** sem `ctid` (uma tabela sem chave vai inteira), com retomada entre execuções. A
  consistência entre tabelas fica com o restore (uma chave estrangeira que falhe leva a execução a
  "aguardando decisão").

## 2026-09-29: Fase 3: dump retomável, inspirado no Dolly; o nome fica para depois

**Decisão do usuário.**

**Decidido:**
- a fase 3 ganha um dump retomável para link instável, inspirado no `internal/dump` do Dolly
  (github.com/VicenteOlmos/dolly, MIT). O esboço e os trade-offs estão em `docs/ESTRATEGIA.md` §16;
- o nome do produto continua "copia-banco" até o produto amadurecer. **pgdolly** saiu da lista, porque
  o Dolly já existe. **O nome foi SUBSTITUÍDO** pela entrada "O nome: pghangar", no fim.

**Por quê:** a revisão do Dolly mostrou que ele não cobre o túnel SSH, as várias versões por container,
a troca com desfazer e a proteção da produção. Mas a retomada dele resolve um risco real do nosso caso:
a produção só por SSH, num link longo.

## 2026-09-29: Correções da revisão adversarial

**Decidido pelo assistente,** depois de uma revisão independente do código (só leitura, sem
contatar host nenhum). Cada item veio de um cenário concreto.

- **Um destino que aponte para o servidor da produção é bloqueado, mesmo sem a tag prod.**
  - A comparação é pelo `system_identifier` das conexões prod e pelo endereço.
  - Vale na cópia, na troca adiada, em desfazer e em apagar.
  - O formulário de conexão não tem mais tag padrão: antes, ele vinha como dev.
  - **Por quê:** a produção cadastrada de novo por descuido (outro nome, "localhost" em vez do IP)
    seria destino, e a troca a tiraria do ar.
- **O `__novo` nasce fechado.** O dono é o superusuário da conexão, sem `PUBLIC`. Ele passa ao dono
  do destino e abre só na etapa "Donos".
  - **Por quê:** durante o restore, tudo é da role temporária de superusuário, e uma função
    `SECURITY DEFINER` vinda da produção daria superusuário a qualquer login do servidor de destino.
- **O script pós-restore precisa ser do root e não gravável por grupo e outros.**
  - **Por quê:** ele roda como superusuário no banco.
- **O túnel escuta num socket unix, num diretório 700,** e não numa porta de `127.0.0.1`.
  - **Por quê:** qualquer usuário local do servidor de desenvolvimento chegaria na produção pela
    porta.
- **A troca não obedece ao cancelamento e resiste a uma queda de conexão.** Se um passo falhar, ela
  confere o estado real, desfaz numa conexão nova e registra o anterior mesmo com erro. A tela não
  oferece cancelar durante a troca.
  - **Por quê:** parar entre os dois renames deixaria o destino sem banco.
- **As conexões administrativas são conferidas (ping) e reabertas antes de cada uso** depois do dump.
  - **Por quê:** elas ficam paradas por horas durante o restore (`idle_session_timeout`, reinício do
    servidor), e a role temporária podia sobrar como superusuário.
- **A troca adiada confere o OID do `__novo`.**
  - Apagar um `__novo` encerra a execução que o aguardava.
  - Um erro passageiro na troca adiada mantém a execução aguardando.
  - **Por quê:** um `__novo` apagado e recriado por outra cópia (incompleta) iria para o lugar do
    destino.
- **Num destino dev, só o `y` confirma a cópia; o enter não.**
  - **Por quê:** o plano aparece sozinho quando fica pronto, e um enter dado para fechar outra
    janela substituiria o banco.
- **Apagar um `__anterior` ou um `__novo` num destino homolog pede o nome do banco,** como desfazer.
- **A trava é pelo servidor (`system_identifier`) e pelo banco,** e não pelo nome da conexão.
- **Os containers levam a instância no nome e num label.** Duas instalações no mesmo Docker não
  colidem, e uma nunca para os containers da outra.
- **Um banco que algum perfil usa nunca é tratado como da ferramenta,** mesmo que o nome pareça um
  `__novo` ou um `__anterior`.
- **Menores:**
  - o dono de um foreign-data wrapper fica com o superusuário;
  - o `pgpass` ganha a linha `localhost` para o socket padrão;
  - a conexão com o ssh-agent é fechada depois do handshake.

## 2026-09-29: Um restore com erro não troca sem perguntar

**Decidido pelo assistente.**

**Decidido:** se o `pg_restore` terminar com erros, a troca não acontece sozinha. A TUI mostra os
erros, e o sysadmin decide entre trocar mesmo assim e parar. O `__novo` fica.

**Por quê:** o `pg_restore` segue depois de um erro e termina com um banco incompleto. Trocar
automaticamente colocaria esse banco no lugar de um que funcionava.

## 2026-09-29: Acesso direto ou por túnel SSH restrito

**Recomendação aceita pelo usuário.**

**Decidido:**
- cada conexão escolhe entre o acesso direto e um túnel SSH;
- o túnel usa **uma chave por servidor de desenvolvimento**, com um usuário dedicado no host da
  produção, e a chave só abre o túnel até a porta do banco
  (`restrict,port-forwarding,permitopen=…,command="/bin/false"`);
- o `known_hosts` é próprio e estrito.

**Por quê:** mesmo numa ferramenta de root, a chave guardada no servidor de desenvolvimento fica
limitada ao túnel se vazar, e não custa nada. Para revogar um servidor, basta apagar uma linha.

## 2026-09-29: O túnel SSH é obrigatório e entra na fase 1

**Decisão do usuário.**

**Decidido:** o túnel SSH sai da fase 2 e entra na fase 1, com a geração de chaves, o
`known_hosts` e as instruções para o host da produção. O acesso direto continua, para os bancos
de desenvolvimento e homologação que a rede alcança.

**Por quê:** a produção não é alcançável pela rede, nem pela tailnet, a partir dos servidores de
desenvolvimento. Só se chega a ela por SSH. Sem o túnel, a ferramenta não copia nada da produção.

**Consequências de desenho:**
- o túnel é aberto pelo **processo da execução**, e não pela TUI: fechar a tela não derruba o
  túnel no meio do dump;
- o túnel manda keepalives e fica aberto só durante o dump;
- se o túnel cair, o dump fica `incompleto`, e o destino não foi tocado, porque o `__novo` só
  nasce depois do dump.

## 2026-09-29: SSH direto no servidor do banco; bastion fica para a fase 3

**Decisão do usuário.**

**Decidido:** no caso normal, o SSH entra direto no servidor do banco de produção, e o túnel vai
até `127.0.0.1:<porta do banco>` nesse servidor. O suporte a bastion continua na fase 3.

**Por quê:** normalmente não há um host intermediário. O modelo de conexão já prevê o destino do
túnel como "host:porta vistos a partir do servidor SSH", então o bastion entra depois sem mudar o
cadastro.

## 2026-09-29: Confirmação pela tag; `prod` nunca é destino

**Recomendação aceita pelo usuário.**

**Decidido:**
- um destino `dev` se confirma com Enter e `y`;
- um destino `homolog` pede o nome do banco digitado;
- um destino `prod` é bloqueado pelo motor, sem opção para liberar;
- origem e destino no mesmo cluster (pelo `system_identifier`) também são bloqueados.

**Por quê:** com superusuário, um perfil errado apagaria a produção. A regra não pode depender só
da tela.

## 2026-09-29: O escopo do perfil

**Recomendação aceita pelo usuário.**

**Decidido:**
- **um banco por perfil;**
- as tabelas sem dados e o script SQL pós-restore entram na v1;
- o script roda no `__novo`, **antes** da troca.

**Por quê:** com dados da produção, o homolog pode disparar jobs, e-mails e webhooks para clientes
reais. O script corrige isso antes de a aplicação ver o banco.

## 2026-09-29: A cópia roda fora da tela

**Recomendação aceita pelo usuário.**

**Decidido:** a cópia roda num processo separado, solto do terminal. A TUI acompanha e pode
fechar e reabrir sem afetá-la.

**Por quê:** a TUI é aberta por SSH no servidor de desenvolvimento. Uma queda dessa conexão não
pode matar uma cópia de 40 minutos.

## 2026-09-29: Tudo sob `/var/lib/pghangar` (era `/var/lib/copia-banco`)

**Decidido pelo assistente.** Na análise, a configuração ia para `/etc/copia-banco`.

**Decidido:** o cadastro, as chaves, o `known_hosts`, os dumps e os logs ficam sob
`/var/lib/pghangar`, e a opção `--dir` troca o diretório. A ferramenta exige root e recusa
permissões frouxas.

**Por quê:** a configuração fica toda no SQLite, e o `/etc` não teria o que guardar.

## 2026-09-29: Sem anonimização

**Decisão do usuário.**

**Decidido:** a cópia leva os dados como estão.

## 2026-09-29: O nome: pghangar

**Decisão do usuário,** entre as opções do assistente.

**Decidido:** a ferramenta passa a se chamar **pghangar**: o binário, o módulo Go
(`github.com/9LEVEL/pghangar`), os diretórios (`/opt/pghangar`, `/var/lib/pghangar`), a role
temporária (`pghangar_…`), os containers e labels (`pghangar-…`, `pghangar.execucao`), o
`application_name`, o repositório e a pasta do código (`/docker/pghangar`). As entradas acima
que citam só o caminho ou o prefixo foram atualizadas; as que registram o nome da época ficam como
estavam.

**Por quê:** o hangar é onde o avião fica guardado e é preparado para voar, e a ferramenta faz isso
com o banco: guarda dumps, anteriores e o banco base, e prepara as cópias. Faz par com o pgtower (a
torre fica ao lado do hangar), e a palavra é a mesma em português e em inglês. Os outros nomes
caíram: **pgferry** já é um projeto ativo de migração para Postgres, **pgshuttle** é uma "resumable
PostgreSQL database copy" (a mesma proposta) e **pgdolly** confundiria com o Dolly.

**A instalação que já existia:** o cadastro de `/var/lib/copia-banco` foi copiado para
`/var/lib/pghangar`, sem apagar o antigo. Uma conexão guarda o usuário SSH dela, então o usuário
`copia-banco` já criado num servidor continua valendo; o nome novo é só a sugestão do formulário.

## 2026-09-30: O túnel negocia o TLS com o banco

**Recomendação aceita pelo usuário.**

**Decidido:**
- quando o `sslmode` pede TLS, **o túnel negocia o TLS com o banco** pelo canal SSH (o
  `SSLRequest` e o handshake) e só então repassa o que o cliente mandou. O cliente (o `pg_dump` no
  container, o pgx do diagnóstico) fala em claro com o socket, e a DSN pelo túnel leva
  `sslmode=disable`;
- `prefer` e `require` cifram sem conferir o certificado. `verify-ca` e `verify-full` conferem
  pelas CAs do sistema, e o `verify-full` confere o nome contra o host do banco cadastrado. O
  `verify-full` pelo túnel, antes recusado, passa a valer;
- quando o TLS que o `sslmode` pede não sai, o túnel responde ao cliente com um erro `FATAL`
  (`08001`) que diz o motivo, e o diagnóstico para na camada **TLS**. Quando chega ao fim, o
  diagnóstico diz se a sessão está com TLS;
- o túnel tira o `SCRAM-SHA-256-PLUS` da lista de mecanismos que o banco oferece, e o cliente
  autentica com o `SCRAM-SHA-256`.

**Por quê:** a premissa anterior, "o SSH já cifra o caminho", estava errada. Ela não estava
registrada aqui, só no código e na ajuda do formulário. O SSH cifra só até o servidor SSH, e dali
até o banco a conexão segue pela rede interna. No socket unix, o libpq e o pgx ignoram o
`sslmode`: o `require` da conexão não fazia nada, e a cópia ia em claro entre o servidor SSH e o
banco. Um `pg_hba` só com `hostssl` recusava a conexão ("no pg_hba.conf entry … no encryption"), e
foi assim que o problema apareceu, na primeira conexão real. Voltar a uma porta TCP, para o
próprio cliente negociar o TLS, reabriria o túnel a qualquer usuário local (a decisão do socket
unix, acima).

**O channel binding:** com TLS, o banco oferece o `SCRAM-SHA-256-PLUS`, que amarra a autenticação
ao TLS do cliente. Como o TLS termina no túnel, o libpq, que fala em claro, recusa a oferta ("server
offered SCRAM-SHA-256-PLUS authentication over a non-SSL connection"). Sem o PLUS, perde-se só essa
amarração, que protege contra um intermediário no TLS. Aqui, o intermediário é o próprio túnel, e o
trecho do cliente até ele é o socket só do root.
