# Política de segurança

## Versões com correção

Só a **última release** recebe correções de segurança. A correção sai numa release nova; para
atualizar, rode o instalador de novo:

```bash
curl -fsSL https://pghangar.dev/install | sudo sh
```

## Como relatar uma vulnerabilidade

**Não abra uma issue pública.** Relate em particular pelo GitHub:
**[Relatar uma vulnerabilidade](https://github.com/9LEVEL/pghangar/security/advisories/new)** (a aba
*Security* deste repositório). Só você e os mantenedores veem o relato.

Inclua o que puder:

- a versão do pghangar (`pghangar versao`), a do PostgreSQL e a do Docker;
- os passos para reproduzir;
- o que um atacante ganha, e o que ele precisa antes (um login no banco, acesso à máquina, um
  arquivo na pasta de entrada, …).

A primeira resposta chega **em até 7 dias**.

## Depois do relato

1. Confirmamos o problema com você e combinamos a gravidade.
2. Corrigimos em particular e publicamos uma release nova.
3. Publicamos um aviso de segurança no GitHub, pedindo um CVE quando couber, com o crédito a você,
   se você quiser.

## Escopo

**Dentro:** o binário `pghangar`, o [`install.sh`](install.sh) e o instalador servido em
<https://pghangar.dev/install>. Interessam em especial as promessas da ferramenta
([`docs/ESTRATEGIA.md`](docs/ESTRATEGIA.md)):

- um caminho que **escreva na origem** ou que leve uma cópia a um **servidor prod**;
- algo **apagado sem a pergunta** ao sysadmin;
- uma **senha** que vá para a linha de comando, para o ambiente de um container, para o log ou para o
  disco fora do cadastro;
- o **túnel SSH** aceitando um servidor que não confere com o `known_hosts`.

**Fora:** vulnerabilidades do próprio PostgreSQL, do Docker, do OpenSSH ou do terminal; a configuração
de um servidor. Relate-as aos projetos delas, a menos que o pghangar as piore.

**Um arquivo de fora restaurado** (aba 4, `pghangar restaurar`) roda com superusuário no destino, como
num restore à mão: a conferência do SQL tira o que não tem lugar no restore de um banco, mas não é um
sandbox. Um SQL que passe pela conferência e escreva fora do banco restaurado é bem-vindo como relato.
