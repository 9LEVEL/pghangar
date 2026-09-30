//go:build integracao

package motor

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

// As três correções num Postgres de verdade: a origem tem RLS citando uma role que o destino não
// tem e um servidor postgres_fdw com credenciais, e sobrou um __novo no destino. Com as três
// marcadas, a cópia apaga o __novo, cria a role sem login, tira os user mappings do banco copiado e
// termina ok, sem erro no restore. Sem a tela (o cron), nenhuma é aplicada, e o __novo bloqueia.
func TestCorrecoesNoDestino(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "dev", cadastro.TagDev, d)
	role := fmt.Sprintf("auditor_cor_%d", rand.Intn(1_000_000))
	semear(t, o, "loja_cor")
	sql(t, o, "postgres", "CREATE ROLE "+role)
	sql(t, o, "loja_cor",
		"CREATE POLICY aud ON public.segredo TO "+role+" USING (true)",
		`CREATE EXTENSION postgres_fdw`,
		`CREATE SERVER erp_prod FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host '127.0.0.1', port '1', dbname 'erp')`,
		`CREATE USER MAPPING FOR postgres SERVER erp_prod OPTIONS (user 'app', password 'segredo-do-erp')`,
		`CREATE USER MAPPING FOR PUBLIC SERVER erp_prod OPTIONS (user 'leitor', password 'outro-segredo')`,
	)
	sql(t, d, "postgres", `DROP DATABASE IF EXISTS loja_cor WITH (FORCE)`, `DROP DATABASE IF EXISTS loja_cor__novo WITH (FORCE)`,
		`CREATE DATABASE loja_cor__novo`)
	a.perfil(t, "cor", "prod", "loja_cor", "dev", "loja_cor")

	// Um __novo sem a marca desta instalação (de outra instalação, de uma versão anterior, criado à
	// mão) não é oferecido: bloqueia, como antes.
	p, err := Planejar(ctx, a.d, "cor")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.correcao(CorrecaoNovo); ok || !p.Bloqueado() || !strings.Contains(strings.Join(p.BloqueiosAtivos(), " "), "não dá para dizer") {
		t.Fatalf("um __novo sem a marca bloqueia: %+v %v", p.Correcoes, p.BloqueiosAtivos())
	}
	// Com a marca desta instalação, de uma cópia que já terminou: a correção vem marcada.
	inst, _ := a.cad.Instancia(ctx)
	sql(t, d, "postgres", "COMMENT ON DATABASE loja_cor__novo IS "+lit(marcaDoNovo(inst, 999999)))
	p, err = Planejar(ctx, a.d, "cor")
	if err != nil {
		t.Fatal(err)
	}
	novo, _ := p.correcao(CorrecaoNovo)
	roles, _ := p.correcao(CorrecaoRoles)
	fdw, _ := p.correcao(CorrecaoFDW)
	if !novo.Marcada || !novo.Bloqueia || !roles.Marcada || !mesmosNomes(roles.Nomes, []string{role}) || fdw.Marcada || !mesmosNomes(fdw.Nomes, []string{"erp_prod"}) {
		t.Fatalf("as correções e os padrões: %+v", p.Correcoes)
	}
	if p.Bloqueado() {
		t.Fatalf("com o __novo marcado, a cópia pode começar: %v", p.BloqueiosAtivos())
	}

	// Sem a tela, nenhuma correção: o __novo bloqueia.
	semTela := p
	semTela.Correcoes = append([]Correcao(nil), p.Correcoes...)
	semTela.DesmarcarCorrecoes()
	if !semTela.Bloqueado() {
		t.Fatal("sem as correções, o __novo que sobrou bloqueia")
	}

	// O sysadmin marca também a do FDW.
	for i := range p.Correcoes {
		p.Correcoes[i].Marcada = true
	}
	e := a.executar(t, ctx, p)
	if e.Estado != cadastro.EstadoOK || e.ErrosRestore != 0 {
		t.Fatalf("com as correções, a cópia termina limpa: %s %s (%d erros)\n%s", e.Estado, e.Mensagem, e.ErrosRestore, a.log.String())
	}
	notas := strings.Join(e.Notas, "\n")
	for _, s := range []string{"corrigido: apagado o loja_cor__novo", "corrigido: criada no destino a role " + role,
		"corrigido: tirado do banco copiado o user mapping de postgres no servidor externo erp_prod",
		"corrigido: tirado do banco copiado o user mapping de PUBLIC no servidor externo erp_prod"} {
		if !strings.Contains(notas, s) {
			t.Fatalf("faltou a nota %q:\n%s", s, notas)
		}
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_authid WHERE rolname = $1 AND NOT rolcanlogin AND rolpassword IS NULL`, role); n != 1 {
		t.Fatal("a role criada no destino é sem login e sem senha")
	}
	if n := valor[int64](t, d, "loja_cor", `SELECT count(*) FROM pg_user_mappings`); n != 0 {
		t.Fatalf("o banco copiado ficou com %d user mapping(s)", n)
	}
	if n := valor[int64](t, d, "loja_cor", `SELECT count(*) FROM pg_foreign_server WHERE srvname = 'erp_prod'`); n != 1 {
		t.Fatal("o servidor externo continua cadastrado, só sem as credenciais")
	}
	if n := valor[int64](t, d, "loja_cor", `SELECT count(*) FROM pg_policy WHERE polname = 'aud'`); n != 1 {
		t.Fatal("a política que cita a role veio")
	}
	// A origem não foi tocada: os user mappings continuam lá.
	if n := valor[int64](t, o, "loja_cor", `SELECT count(*) FROM pg_user_mappings`); n != 2 {
		t.Fatalf("a origem mudou: %d user mapping(s)", n)
	}
}
