package motor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

// Correções: o que a cópia resolve sozinha no destino, se o sysadmin a deixar marcada na
// confirmação (docs/DECISOES.md, 2026-09-30). Só no destino, só no que é da própria cópia, e depois
// das guardas: nunca num servidor prod.

const (
	CorrecaoNovo  = "novo"  // apagar o __novo que sobrou de uma cópia anterior
	CorrecaoRoles = "roles" // criar no destino as roles que a RLS e os user mappings da origem citam
	CorrecaoFDW   = "fdw"   // tirar do banco copiado os user mappings dos servidores externos
)

// Correcao é um conserto que a cópia faz sozinha, se estiver marcada.
type Correcao struct {
	Tipo  string   `json:"tipo"`
	Texto string   `json:"texto"`
	SeNao string   `json:"se_nao"` // o que acontece sem ela
	Nomes []string `json:"nomes"`  // o __novo, as roles, os servidores externos
	// Marcada começa com o padrão do plano; a confirmação grava a escolha do sysadmin.
	Marcada bool `json:"marcada"`
	// Bloqueia: desmarcada, a cópia não começa, e o SeNao vira um bloqueio.
	Bloqueia bool `json:"bloqueia,omitempty"`
	// OID é o do __novo que a correção apaga: o nome é sempre o mesmo, e só o OID diz que é o
	// mesmo banco que o sysadmin viu.
	OID uint32 `json:"oid,omitempty"`
}

// BloqueiosAtivos são os bloqueios do plano e os das correções que, desmarcadas, impedem a cópia.
func (p Plano) BloqueiosAtivos() []string {
	bs := append([]string(nil), p.Bloqueios...)
	for _, c := range p.Correcoes {
		if c.Bloqueia && !c.Marcada {
			bs = append(bs, c.SeNao)
		}
	}
	return bs
}

func (p Plano) correcao(tipo string) (Correcao, bool) {
	for _, c := range p.Correcoes {
		if c.Tipo == tipo {
			return c, true
		}
	}
	return Correcao{}, false
}

// AdotarEscolhas passa ao plano refeito na execução o que o sysadmin marcou na confirmação. Uma
// correção só vale se foi marcada e se é a mesma que ele viu (os mesmos nomes; no __novo, o mesmo
// banco). Uma marcada que mudou não é aplicada nem esquecida: a execução para e pede outra
// confirmação.
func (p *Plano) AdotarEscolhas(confirmado Plano) error {
	for i := range p.Correcoes {
		c := &p.Correcoes[i]
		c.Marcada = false
		for _, x := range confirmado.Correcoes {
			if x.Tipo != c.Tipo || !x.Marcada {
				continue
			}
			if !mesmosNomes(x.Nomes, c.Nomes) || x.OID != c.OID {
				return fmt.Errorf("a correção \"%s\" mudou desde a confirmação (agora: %s): confirme de novo", x.Texto, c.Texto)
			}
			c.Marcada = true
		}
	}
	return nil
}

// DesmarcarCorrecoes tira todas as marcas: sem a tela (o cron), ninguém disse sim.
func (p *Plano) DesmarcarCorrecoes() {
	for i := range p.Correcoes {
		p.Correcoes[i].Marcada = false
	}
}

func mesmosNomes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// --- no plano ---------------------------------------------------------------------------------

// marcaDoNovo é o comentário que o __novo recebe ao nascer: a instalação e a execução.
func marcaDoNovo(instancia string, execucao int64) string {
	return fmt.Sprintf("pghangar: __novo da execução #%d (instância %s)", execucao, instancia)
}

var reMarcaDoNovo = regexp.MustCompile(`^pghangar: __novo da execução #(\d+) \(instância ([^)]+)\)`)

// lerMarcaDoNovo lê a instalação e a execução do comentário do __novo. ok é falso sem a marca
// (um __novo de uma versão anterior, criado à mão, ou cujo comentário foi trocado).
func lerMarcaDoNovo(comentario string) (instancia string, execucao int64, ok bool) {
	m := reMarcaDoNovo.FindStringSubmatch(comentario)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return m[2], n, err == nil
}

// oferecerApagarNovo faz do __novo que sobrou uma correção que o apaga, marcada. Ela só é oferecida
// quando é seguro: o __novo é desta instalação, de uma cópia que já terminou e que não espera a
// decisão da troca, ninguém está conectado nele, e ele não é o banco de um perfil. Em qualquer
// outro caso, a cópia fica bloqueada, e quem decide é o sysadmin (a aba 2 ou a aba Anteriores).
func oferecerApagarNovo(ctx context.Context, d Deps, p *Plano, adm *pgx.Conn, conexaoNome string) error {
	usados, err := bancosDePerfis(ctx, d, conexaoNome)
	if err != nil {
		return err
	}
	if usados[p.Novo] {
		p.bloquear("sobrou o banco %s no destino, e ele é o banco de um perfil: a ferramenta não o apaga", p.Novo)
		return nil
	}
	var o uint32
	var comentario string
	var sessoes int
	if err := adm.QueryRow(ctx, `SELECT d.oid, coalesce(shobj_description(d.oid, 'pg_database'), ''),
		(SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname) FROM pg_database d WHERE d.datname = $1`, p.Novo).
		Scan(&o, &comentario, &sessoes); err != nil {
		return err
	}
	inst, err := d.Cadastro.Instancia(ctx)
	if err != nil {
		return err
	}
	deOutro := fmt.Sprintf("sobrou o banco %s no destino, e não dá para dizer que é uma sobra desta instalação: confira e apague-o na aba Anteriores", p.Novo)
	instNovo, execNovo, marcado := lerMarcaDoNovo(comentario)
	switch {
	case !marcado || instNovo != inst:
		p.bloquear("%s", deOutro)
		return nil
	case sessoes > 0:
		p.bloquear("o banco %s está em uso agora (%d sessão(ões)): pode ser uma cópia em andamento. Espere ou confira na aba 2", p.Novo, sessoes)
		return nil
	}
	if e, err := d.Cadastro.Execucao(ctx, execNovo); err == nil && (!e.Terminou() || e.Estado == cadastro.EstadoAguardando) {
		p.bloquear("o banco %s é da cópia #%d, que está %s: decida na aba 2", p.Novo, e.ID, e.Estado)
		return nil
	}
	// Uma cópia que espera a decisão da troca dele, pelo nome da conexão ou pelo servidor.
	es, err := d.Cadastro.Execucoes(ctx, 1000)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Estado != cadastro.EstadoAguardando || e.BancoNovo != p.Novo {
			continue
		}
		var pe Plano
		_ = json.Unmarshal([]byte(e.Plano), &pe)
		if e.Destino == conexaoNome || (pe.Destino.SystemID != "" && pe.Destino.SystemID == p.Destino.SystemID) {
			p.bloquear("o banco %s espera a decisão da cópia #%d: decida na aba 2 (trocar ou não)", p.Novo, e.ID)
			return nil
		}
	}
	p.Correcoes = append(p.Correcoes, Correcao{Tipo: CorrecaoNovo, Nomes: []string{p.Novo}, OID: o, Marcada: true, Bloqueia: true,
		Texto: fmt.Sprintf("apagar o %s, que sobrou da cópia #%d", p.Novo, execNovo),
		SeNao: fmt.Sprintf("sobrou o banco %s da cópia #%d no destino: marque a correção para apagá-lo, ou apague-o na aba Anteriores", p.Novo, execNovo)})
	return nil
}

func oferecerRoles(p *Plano, faltam []string) {
	qual := "a role " + faltam[0]
	if len(faltam) > 1 {
		qual = "as roles " + strings.Join(faltam, ", ")
	}
	texto := fmt.Sprintf("criar no destino %s, sem login e sem senha: as políticas de RLS (ou os user mappings) da origem as citam. Roles valem para o servidor inteiro, e ficam depois da cópia", qual)
	// Uma role de user mapping faz o mapping, com a credencial, chegar ao destino.
	if len(p.Externos) > 0 {
		texto += ". Com elas, os user mappings dos servidores externos também chegam, com as credenciais, se a correção deles ficar desmarcada"
	}
	p.Correcoes = append(p.Correcoes, Correcao{Tipo: CorrecaoRoles, Nomes: faltam, Marcada: true, Texto: texto,
		SeNao: fmt.Sprintf("sem %s no destino, o restore dá erro nesses objetos, e a troca vai esperar a sua decisão", qual)})
}

// oferecerFDW: os user mappings guardam as credenciais dos servidores externos (postgres_fdw e
// afins). Copiados para o dev, eles deixam o dev ler e escrever nesses servidores, que muitas vezes
// são outra produção. A correção vem sempre desmarcada: há quem precise deles no dev.
func oferecerFDW(p *Plano, servidores []string) {
	qual := "do servidor externo " + servidores[0]
	if len(servidores) > 1 {
		qual = "dos servidores externos " + strings.Join(servidores, ", ")
	}
	p.Correcoes = append(p.Correcoes, Correcao{Tipo: CorrecaoFDW, Nomes: servidores,
		Texto: fmt.Sprintf("tirar do banco copiado os user mappings (as credenciais) %s: o destino fica sem acesso a eles", qual),
		SeNao: fmt.Sprintf("o banco copiado leva as credenciais %s: o destino pode ler e escrever neles", qual)})
}

// --- na execução ------------------------------------------------------------------------------

// corrigido registra uma correção aplicada: no log e nas notas da execução.
func (r *corrida) corrigido(f string, a ...any) {
	s := fmt.Sprintf(f, a...)
	r.d.logf("correção: %s", s)
	r.e.Notas = append(r.e.Notas, "corrigido: "+s)
	r.gravar()
}

// corrigirNovo apaga o __novo que sobrou, antes de qualquer outra coisa. A trava do destino já é
// desta execução. Na hora, confere de novo que é o mesmo banco (o OID) e que ninguém está
// conectado nele; o apagar confere que ele é da ferramenta e de nenhum perfil.
func (r *corrida) corrigirNovo(ctx context.Context) error {
	c, ok := r.p.correcao(CorrecaoNovo)
	if !ok || !c.Marcada {
		return nil
	}
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	for _, n := range c.Nomes {
		var o uint32
		var sessoes int
		if err := adm.QueryRow(ctx, `SELECT d.oid, (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname)
			FROM pg_database d WHERE d.datname = $1`, n).Scan(&o, &sessoes); err != nil {
			return fmt.Errorf("correção (apagar %s): %w", n, err)
		}
		if o != c.OID || sessoes > 0 {
			return fmt.Errorf("correção (apagar %s): o banco mudou ou está em uso desde a confirmação; nada foi apagado. Confirme de novo", n)
		}
		apagou, err := apagarBanco(ctx, r.d, r.destino, r.p.Perfil.Destino, r.p.Destino.Banco, n)
		if apagou {
			r.corrigido("apagado o %s, que sobrou de uma cópia anterior", n)
		}
		if err != nil {
			return fmt.Errorf("correção (apagar %s): %w", n, err)
		}
	}
	return nil
}

// corrigirRoles cria no destino as roles que faltam, sem login e sem senha: as políticas passam a
// existir, e ninguém entra por elas.
func (r *corrida) corrigirRoles(ctx context.Context) error {
	c, ok := r.p.correcao(CorrecaoRoles)
	if !ok || !c.Marcada {
		return nil
	}
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	for _, role := range c.Nomes {
		var existe bool
		if err := adm.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&existe); err != nil {
			return err
		}
		if existe {
			continue
		}
		if _, err := adm.Exec(ctx, "CREATE ROLE "+id(role)+" NOLOGIN"); err != nil {
			return fmt.Errorf("correção (criar a role %s): %w", role, err)
		}
		r.corrigido("criada no destino a role %s, sem login e sem senha", role)
	}
	return nil
}

// corrigirFDW tira do __novo, antes da troca, os user mappings dos servidores externos marcados. Os
// servidores continuam cadastrados, mas sem as credenciais.
func (r *corrida) corrigirFDW(ctx context.Context) error {
	c, ok := r.p.correcao(CorrecaoFDW)
	if !ok || !c.Marcada {
		return nil
	}
	conn, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT srvname, umuser = 0, coalesce(usename, '') FROM pg_user_mappings
		WHERE srvname = ANY($1) ORDER BY 1, 3`, c.Nomes)
	if err != nil {
		return err
	}
	type mapeamento struct {
		servidor, usuario string
		publico           bool
	}
	var ms []mapeamento
	for rows.Next() {
		var m mapeamento
		if err := rows.Scan(&m.servidor, &m.publico, &m.usuario); err != nil {
			rows.Close()
			return err
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range ms {
		quem, texto := id(m.usuario), m.usuario
		if m.publico {
			quem, texto = "PUBLIC", "PUBLIC"
		}
		if _, err := conn.Exec(ctx, "DROP USER MAPPING IF EXISTS FOR "+quem+" SERVER "+id(m.servidor)); err != nil {
			return fmt.Errorf("correção (user mapping de %s em %s): %w", texto, m.servidor, err)
		}
		r.corrigido("tirado do banco copiado o user mapping de %s no servidor externo %s", texto, m.servidor)
	}
	r.fdwFeito = true
	return nil
}
