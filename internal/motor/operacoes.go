package motor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/trava"
)

// TrocarDepois faz a troca de uma execução que parou em "aguardando", depois de o sysadmin decidir
// trocar mesmo assim (docs/DECISOES.md: um restore com erro não troca sem perguntar). Se a troca não
// acontecer por um problema passageiro (destino fora do ar, sessões que não caem), a execução volta
// a aguardar: a opção de trocar não se perde.
func TrocarDepois(ctx context.Context, d Deps, execID int64) error {
	e, err := d.Cadastro.Execucao(ctx, execID)
	if err != nil {
		return err
	}
	if e.Estado != cadastro.EstadoAguardando {
		return fmt.Errorf("a execução %d está %s, e não aguardando a troca", execID, e.Estado)
	}
	motivo := e.Mensagem
	if i := strings.Index(motivo, " (a tentativa de troca de "); i > 0 {
		motivo = motivo[:i]
	}
	r := &corrida{d: d, e: e}
	r.e.Estado, r.e.PID = cadastro.EstadoRodando, os.Getpid()
	r.etapa("Troca")
	// Como na cópia, a troca não obedece ao cancelamento.
	ctxTroca, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	err = r.trocarDepois(ctxTroca, motivo)
	if err != nil && !r.trocou && !errors.Is(err, errNovoPerdido) {
		r.e.Estado, r.e.Fim = cadastro.EstadoAguardando, time.Now()
		r.e.Mensagem = fmt.Sprintf("%s (a tentativa de troca de %s falhou: %v)", motivo, time.Now().Format("15:04"), err)
		d.logf("== a troca não aconteceu, e a execução continua aguardando: %v", err)
		r.gravar()
		r.avisar()
		return err
	}
	r.fim(ctx, err)
	return err
}

func (r *corrida) trocarDepois(ctx context.Context, motivo string) error {
	var p Plano
	if err := json.Unmarshal([]byte(r.e.Plano), &p); err != nil {
		return fmt.Errorf("plano ilegível: %w", err)
	}
	var err error
	if r.destino, _, err = abrirLado(ctx, r.d, r.e.Destino); err != nil {
		return err
	}
	defer r.destino.fechar()
	if err := guardaProd(ctx, r.d, r.destino.c, r.destino.c.Info.SystemID); err != nil {
		return err
	}
	if p.Destino.SystemID != "" && r.destino.c.Info.SystemID != p.Destino.SystemID {
		return errors.New("o servidor de destino mudou desde a cópia (system_identifier diferente): nada foi trocado")
	}
	ant, trocou, err := trocarNomes(ctx, r.d, r.destino, r.e.Banco, r.e.BancoNovo, p.Destino.Existe, r.e.NovoOID, time.Now())
	r.e.BancoAnterior, r.trocou = ant, trocou
	if err != nil {
		return err
	}
	r.e.Mensagem = fmt.Sprintf("%s trocado por decisão do sysadmin, apesar de: %s", r.e.Banco, motivo)
	if ant != "" {
		r.e.Mensagem += " O banco substituído ficou como " + ant + "."
	}
	return nil
}

// DaFerramenta são os bancos que a ferramenta deixou num destino: os anteriores e o __novo.
type DaFerramenta struct {
	Conexao    string
	Banco      string
	Existe     bool // o próprio banco de destino
	Anteriores []Anterior
	Novo       *Anterior
	Base       *Anterior
}

// comDestino abre o destino, confere que não é um servidor de produção e roda f com ele. Com
// travar, pega a trava do destino (pelo servidor, e não pelo nome da conexão).
func comDestino(ctx context.Context, d Deps, conexaoNome, banco string, travar bool, f func(l *lado) error) error {
	c, err := d.Cadastro.Conexao(ctx, conexaoNome)
	if err != nil {
		return err
	}
	if c.Tag == cadastro.TagProd {
		return fmt.Errorf("a conexão %s é prod: a ferramenta não mexe em bancos de uma conexão prod", c.Nome)
	}
	l, _, err := abrirLado(ctx, d, conexaoNome)
	if err != nil {
		return err
	}
	defer l.fechar()
	if err := guardaProd(ctx, d, l.c, l.c.Info.SystemID); err != nil {
		return err
	}
	if travar {
		t, err := trava.Obter(d.Dir.Travas(), trava.Destino(conexaoNome, l.c.Info.SystemID), banco)
		if err != nil {
			return err
		}
		defer t.Soltar()
	}
	return f(l)
}

// Listar lê os anteriores e o __novo de um banco de destino.
func Listar(ctx context.Context, d Deps, conexaoNome, bancoDestino string) (DaFerramenta, error) {
	r := DaFerramenta{Conexao: conexaoNome, Banco: bancoDestino}
	usados, err := bancosDePerfis(ctx, d, conexaoNome)
	if err != nil {
		return r, err
	}
	err = comDestino(ctx, d, conexaoNome, bancoDestino, false, func(l *lado) error {
		var err error
		if r.Anteriores, err = anteriores(ctx, l.admin, bancoDestino, usados); err != nil {
			return err
		}
		if r.Existe, err = existeBanco(ctx, l.admin, bancoDestino); err != nil {
			return err
		}
		var pl Plano
		if err := lerBase(ctx, &pl, l.admin, bancoDestino); err != nil {
			return err
		}
		if pl.Base != nil && !usados[pl.Base.Nome] {
			r.Base = pl.Base
		}
		novo := nomes.Novo(bancoDestino)
		if usados[novo] {
			return nil
		}
		var tam int64
		err = l.admin.QueryRow(ctx, `SELECT pg_database_size(oid) FROM pg_database WHERE datname = $1`, novo).Scan(&tam)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			r.Novo = &Anterior{Nome: novo, Tamanho: tam}
		}
		return nil
	})
	return r, err
}

// Desfazer põe um anterior de volta no lugar do banco. O banco atual também vira __anterior:
// desfazer não perde nada. Se um passo falhar, confere o estado real e devolve os nomes.
func Desfazer(ctx context.Context, d Deps, conexaoNome, bancoDestino, anterior string) (string, error) {
	if _, ok := nomes.DataDoAnterior(bancoDestino, anterior); !ok {
		return "", fmt.Errorf("%s não é um __anterior de %s", anterior, bancoDestino)
	}
	usados, err := bancosDePerfis(ctx, d, conexaoNome)
	if err != nil {
		return "", err
	}
	if usados[anterior] {
		return "", fmt.Errorf("recusado: %s é o banco de um perfil", anterior)
	}
	guardado := ""
	err = comDestino(ctx, d, conexaoNome, bancoDestino, true, func(l *lado) error {
		adm := l.admin
		if ok, err := existeBanco(ctx, adm, anterior); err != nil || !ok {
			return fmt.Errorf("o banco %s não existe mais", anterior)
		}
		existe, err := existeBanco(ctx, adm, bancoDestino)
		if err != nil {
			return err
		}
		if existe {
			if guardado, err = anteriorLivre(ctx, adm, bancoDestino, time.Now()); err != nil {
				return err
			}
			if err = conexoesDoBanco(ctx, adm, bancoDestino, false); err == nil {
				err = renomear(ctx, adm, bancoDestino, guardado)
			}
			if err != nil {
				_ = consertar(d, l, "devolver "+bancoDestino, func(c context.Context, a *pgx.Conn) error {
					if ok, e := existeBanco(c, a, bancoDestino); e != nil {
						return e
					} else if !ok {
						if e := renomear(c, a, guardado, bancoDestino); e != nil {
							return e
						}
					}
					return conexoesDoBanco(c, a, bancoDestino, true)
				})
				return err
			}
		}
		if err := renomear(ctx, adm, anterior, bancoDestino); err != nil {
			_ = consertar(d, l, "devolver "+bancoDestino, func(c context.Context, a *pgx.Conn) error {
				ok, e := existeBanco(c, a, bancoDestino)
				if e != nil {
					return e
				}
				if !ok && existe {
					if e := renomear(c, a, guardado, bancoDestino); e != nil {
						return e
					}
				}
				return conexoesDoBanco(c, a, bancoDestino, true)
			})
			return err
		}
		if err := conexoesDoBanco(ctx, adm, bancoDestino, true); err != nil {
			return consertar(d, l, "abrir "+bancoDestino, func(c context.Context, a *pgx.Conn) error { return conexoesDoBanco(c, a, bancoDestino, true) })
		}
		return nil
	})
	return guardado, err
}

// Apagar apaga um banco da ferramenta (um __anterior ou o __novo) de um destino. Recusa qualquer
// outro nome, e um banco que algum perfil use. Fora da etapa "Anteriores", é o único DROP DATABASE
// da ferramenta, junto com a correção do __novo que sobrou (apagarBanco, nos dois).
func Apagar(ctx context.Context, d Deps, conexaoNome, bancoDestino, nome string) error {
	return comDestino(ctx, d, conexaoNome, bancoDestino, true, func(l *lado) error {
		_, err := apagarBanco(ctx, d, l, conexaoNome, bancoDestino, nome)
		return err
	})
}

// apagarBanco apaga um banco da ferramenta pelo lado já aberto (e já travado por quem chama).
// apagou diz se o DROP aconteceu, mesmo quando um passo depois dele falha.
func apagarBanco(ctx context.Context, d Deps, l *lado, conexaoNome, bancoDestino, nome string) (apagou bool, err error) {
	if !nomes.EhDaFerramenta(bancoDestino, nome) {
		return false, fmt.Errorf("recusado: %s não é um banco da ferramenta para %s", nome, bancoDestino)
	}
	usados, err := bancosDePerfis(ctx, d, conexaoNome)
	if err != nil {
		return false, err
	}
	if usados[nome] {
		return false, fmt.Errorf("recusado: %s é o banco de um perfil", nome)
	}
	adm, err := l.adminVivo(ctx, d)
	if err != nil {
		return false, err
	}
	if ok, err := existeBanco(ctx, adm, nome); err != nil || !ok {
		return false, fmt.Errorf("o banco %s não existe", nome)
	}
	if _, err := adm.Exec(ctx, "DROP DATABASE "+id(nome)+" WITH (FORCE)"); err != nil {
		return false, err
	}
	// Apagado o __novo, a role temporária que era dona dele pode sair.
	limparRolesOrfas(ctx, d, adm)
	if nome != nomes.Novo(bancoDestino) {
		return true, nil
	}
	// Uma execução que aguardava a troca deste __novo não tem mais o que trocar.
	es, err := d.Cadastro.Execucoes(ctx, 1000)
	if err != nil {
		return true, err
	}
	for _, e := range es {
		if e.Estado == cadastro.EstadoAguardando && e.Destino == conexaoNome && e.Banco == bancoDestino && e.BancoNovo == nome {
			e.Estado, e.Fim = cadastro.EstadoErro, time.Now()
			e.Mensagem = fmt.Sprintf("não trocada: o banco %s foi apagado (%s)", nome, e.Mensagem)
			if err := d.Cadastro.GravarExecucao(ctx, e); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// rolesOrfas lista as roles temporárias desta instância que sobraram no servidor: as de execuções
// que já terminaram (ou morreram). As de execuções em andamento, para outros bancos do mesmo
// servidor, ficam de fora.
func rolesOrfas(ctx context.Context, d Deps, adm *pgx.Conn) ([]string, error) {
	inst, err := d.Cadastro.Instancia(ctx)
	if err != nil {
		return nil, err
	}
	emUso := map[string]bool{}
	es, err := d.Cadastro.Execucoes(ctx, 1000)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !e.Terminou() {
			emUso[nomes.RoleTemporaria(inst, e.ID)] = true
		}
	}
	rows, err := adm.Query(ctx, `SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`, nomes.PrefixoRoles(inst))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rs []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		if nomes.EhRoleTemporaria(inst, r) && !emUso[r] {
			rs = append(rs, r)
		}
	}
	return rs, rows.Err()
}

// limparRolesOrfas neutraliza (sem superusuário, sem login) e remove as roles temporárias que
// sobraram. Uma que ainda seja dona de objetos (no __novo que sobrou) fica neutralizada, e sai
// quando o __novo for apagado.
func limparRolesOrfas(ctx context.Context, d Deps, adm *pgx.Conn) {
	rs, err := rolesOrfas(ctx, d, adm)
	if err != nil {
		d.logf("lendo as roles temporárias: %v", err)
		return
	}
	for _, r := range rs {
		if _, err := adm.Exec(ctx, "ALTER ROLE "+id(r)+" NOSUPERUSER NOLOGIN"); err != nil {
			d.logf("neutralizando a role %s: %v", r, err)
			continue
		}
		if _, err := adm.Exec(ctx, "DROP ROLE "+id(r)); err != nil {
			d.logf("a role %s ficou (sem superusuário e sem login): %v", r, err)
			continue
		}
		d.logf("role temporária %s, de uma execução que não terminou, removida", r)
	}
}

// --- dumps --------------------------------------------------------------------------------------

// Dump é um dump guardado.
type Dump struct {
	Caminho   string
	Manifesto Manifesto
	Tamanho   int64
}

// Raizes são os diretórios onde há dumps: o padrão e os dos perfis que escolheram outro.
func Raizes(dir local.Dir, perfis []cadastro.Perfil) []string {
	vistos := map[string]bool{dir.Dumps(): true}
	rs := []string{dir.Dumps()}
	for _, p := range perfis {
		if p.DirDumps != "" && !vistos[p.DirDumps] {
			vistos[p.DirDumps] = true
			rs = append(rs, p.DirDumps)
		}
	}
	return rs
}

// ListarDumps acha os dumps (os diretórios com manifesto.json), do mais novo ao mais velho.
func ListarDumps(dir local.Dir, perfis []cadastro.Perfil) []Dump {
	var ds []Dump
	vistos := map[string]bool{}
	add := func(caminho string) {
		if vistos[caminho] {
			return
		}
		b, err := os.ReadFile(filepath.Join(caminho, "manifesto.json"))
		if err != nil {
			return
		}
		var m Manifesto
		if json.Unmarshal(b, &m) != nil {
			return
		}
		vistos[caminho] = true
		ds = append(ds, Dump{Caminho: caminho, Manifesto: m, Tamanho: TamanhoDir(caminho)})
	}
	for _, raiz := range Raizes(dir, perfis) {
		// O padrão tem um nível por perfil; um diretório escolhido no perfil já é o do perfil.
		for _, padrao := range []string{filepath.Join(raiz, "*", "*"), filepath.Join(raiz, "*")} {
			ms, _ := filepath.Glob(padrao)
			for _, m := range ms {
				if fi, err := os.Stat(m); err == nil && fi.IsDir() {
					add(m)
				}
			}
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Manifesto.Inicio.After(ds[j].Manifesto.Inicio) })
	return ds
}

// ApagarDump apaga um dump, só se ele estiver dentro de um diretório de dumps e tiver o manifesto
// da ferramenta.
func ApagarDump(dir local.Dir, perfis []cadastro.Perfil, caminho string) error {
	dentro := false
	for _, r := range Raizes(dir, perfis) {
		if local.Dentro(r, caminho) {
			dentro = true
		}
	}
	if !dentro {
		return fmt.Errorf("recusado: %s está fora dos diretórios de dumps", caminho)
	}
	if _, err := os.Stat(filepath.Join(caminho, "manifesto.json")); err != nil {
		return fmt.Errorf("recusado: %s não tem o manifesto.json da ferramenta", caminho)
	}
	return os.RemoveAll(caminho)
}
