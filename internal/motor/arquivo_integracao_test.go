//go:build integracao

package motor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

// dumpDeFora gera um arquivo com o pg_dump do próprio container (como um backup feito fora da
// ferramenta) e o põe na pasta de entrada.
func dumpDeFora(t *testing.T, a *ambiente, s *pg, nome string, args ...string) string {
	t.Helper()
	dentro := "/tmp/" + nome
	if out, err := exec.Command("docker", "exec", s.nome, "sh", "-c", "rm -rf "+dentro+" && pg_dump -U postgres "+strings.Join(args, " ")+" -f "+dentro).CombinedOutput(); err != nil {
		t.Fatalf("pg_dump %v: %v %s", args, err, out)
	}
	if strings.HasSuffix(nome, ".gz") {
		cru := strings.TrimSuffix(dentro, ".gz")
		if out, err := exec.Command("docker", "exec", s.nome, "sh", "-c", "mv "+dentro+" "+cru+" && gzip "+cru).CombinedOutput(); err != nil {
			t.Fatalf("gzip: %v %s", err, out)
		}
	}
	fora := filepath.Join(a.d.Dir.Entrada(), nome)
	if out, err := exec.Command("docker", "cp", "-q", s.nome+":"+dentro, fora).CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v %s", err, out)
	}
	return fora
}

// restaurarArquivo planeja, confirma (registra) e executa, como a tela faz.
func (a *ambiente) restaurarArquivo(t *testing.T, ctx context.Context, pd PedidoArquivo) (Plano, cadastro.Execucao) {
	t.Helper()
	p, err := PlanejarArquivo(ctx, a.d, pd)
	if err != nil {
		t.Fatalf("planejando %s: %v", filepath.Base(pd.Caminho), err)
	}
	if p.Bloqueado() {
		t.Fatalf("%s bloqueado: %v", filepath.Base(pd.Caminho), p.Bloqueios)
	}
	return p, a.executarArquivo(t, ctx, p)
}

func (a *ambiente) executarArquivo(t *testing.T, ctx context.Context, p Plano) cadastro.Execucao {
	t.Helper()
	pj, _ := json.Marshal(p)
	id, err := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: p.Perfil.Nome, Tipo: cadastro.TipoArquivo, Estado: cadastro.EstadoIniciando,
		Destino: p.Perfil.Destino, Banco: p.Perfil.DestinoBanco, Plano: string(pj)})
	if err != nil {
		t.Fatal(err)
	}
	_ = Executar(ctx, a.d, id)
	e, err := a.cad.Execucao(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRestaurarArquivo(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o16", 16), subir(t, "d18", 18)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "loja8")
	destinoHomolog(t, d, "loja8")
	pedidos := valor[int64](t, o, "loja8", `SELECT count(*) FROM vendas.pedido`)
	contar := func(banco string) int64 { return valor[int64](t, d, banco, `SELECT count(*) FROM vendas.pedido`) }

	t.Run("custom num banco que existe", func(t *testing.T) {
		c := dumpDeFora(t, a, o, "loja8.dump", "-Fc", "loja8")
		p, e := a.restaurarArquivo(t, ctx, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8", Jobs: 2})
		if p.Imagem != 18 || p.Confirmacao() != "loja8" || p.Arquivo.Banco != "loja8" || len(p.Extensoes) != 2 {
			t.Fatalf("plano: imagem %d, confirmação %q, %+v, extensões %v", p.Imagem, p.Confirmacao(), p.Arquivo, p.Extensoes)
		}
		if e.Estado != cadastro.EstadoOK || !strings.Contains(e.Mensagem, "restaurado do arquivo loja8.dump") || e.BancoAnterior == "" {
			t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
		}
		if n := contar("loja8"); n != pedidos {
			t.Fatalf("pedidos: %d, esperava %d", n, pedidos)
		}
		// Tudo com o dono do homolog; as configurações do banco de antes ficaram.
		donos := valor[string](t, d, "loja8", `SELECT string_agg(DISTINCT pg_get_userbyid(relowner), ',') FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('vendas', 'public') AND c.relkind IN ('r','v','m','S')`)
		if donos != "app_homolog" {
			t.Errorf("donos: %s", donos)
		}
		if c := valor[string](t, d, "postgres", `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = 'loja8'`); c != "homolog da loja" {
			t.Errorf("comentário: %s", c)
		}
		if aberto := valor[bool](t, d, "postgres", `SELECT datallowconn FROM pg_database WHERE datname = $1`, e.BancoAnterior); aberto {
			t.Error("o anterior fica fechado para conexões")
		}
		if _, err := os.Stat(c); err != nil {
			t.Error("o arquivo continua na pasta de entrada")
		}
	})

	t.Run("diretório num banco novo", func(t *testing.T) {
		c := dumpDeFora(t, a, o, "loja8_dir", "-Fd", "-j", "2", "loja8")
		_, e := a.restaurarArquivo(t, ctx, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8_dir", Jobs: 2})
		if e.Estado != cadastro.EstadoOK || e.BancoAnterior != "" {
			t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
		}
		if n := contar("loja8_dir"); n != pedidos {
			t.Fatalf("pedidos: %d", n)
		}
		if dono := valor[string](t, d, "postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'loja8_dir'`); dono != "postgres" {
			t.Errorf("o banco novo é do usuário da conexão: %s", dono)
		}
	})

	t.Run("SQL com -C não escreve no banco de mesmo nome", func(t *testing.T) {
		sql(t, d, "loja8", `CREATE TABLE marca_viva (x int)`)
		antes := contar("loja8")
		c := dumpDeFora(t, a, o, "loja8.sql.gz", "-C", "loja8")
		p, e := a.restaurarArquivo(t, ctx, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8_sql", Jobs: 2})
		if !strings.Contains(strings.Join(p.Notas, " "), "pg_dump -C") {
			t.Errorf("o plano avisa do -C: %v", p.Notas)
		}
		if e.Estado != cadastro.EstadoOK {
			t.Fatalf("%s %s (erros %d)\n%s", e.Estado, e.Mensagem, e.ErrosRestore, a.log.String())
		}
		if n := contar("loja8_sql"); n != pedidos {
			t.Fatalf("pedidos: %d", n)
		}
		// O CREATE DATABASE loja8 e o \connect loja8 ficaram de fora: o loja8 vivo não foi tocado.
		if n := contar("loja8"); n != antes {
			t.Fatalf("o SQL escreveu no loja8 vivo: %d pedidos, eram %d", n, antes)
		}
		if !valor[bool](t, d, "loja8", `SELECT to_regclass('public.marca_viva') IS NOT NULL`) {
			t.Fatal("o loja8 vivo foi substituído")
		}
	})

	t.Run("SQL com role que falta espera a decisão", func(t *testing.T) {
		sql(t, o, "postgres", `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'so_na_origem') THEN CREATE ROLE so_na_origem; END IF; END $$`)
		sql(t, o, "loja8", `ALTER TABLE vendas.cliente OWNER TO so_na_origem`)
		c := dumpDeFora(t, a, o, "loja8_role.sql", "loja8")
		_, e := a.restaurarArquivo(t, ctx, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8_role", Jobs: 2})
		if e.Estado != cadastro.EstadoAguardando || e.ErrosRestore == 0 || !strings.Contains(e.Mensagem, "loja8_role__novo") {
			t.Fatalf("%s %s (erros %d)\n%s", e.Estado, e.Mensagem, e.ErrosRestore, a.log.String())
		}
		if err := TrocarDepois(ctx, a.d, e.ID); err != nil {
			t.Fatal(err)
		}
		if n := contar("loja8_role"); n != pedidos {
			t.Fatalf("pedidos: %d", n)
		}
		// As mensagens do servidor vêm em inglês: num servidor em português, o ERRO não seria contado.
		if !strings.Contains(a.log.String(), "lc_messages=C") {
			t.Error("o psql do restore deveria pedir as mensagens em inglês")
		}
	})

	t.Run("user mappings saem com a correção", func(t *testing.T) {
		sql(t, o, "postgres", `DROP DATABASE IF EXISTS loja8_fdw WITH (FORCE)`, `CREATE DATABASE loja8_fdw`)
		sql(t, o, "loja8_fdw", `CREATE EXTENSION postgres_fdw`,
			`CREATE SERVER outra_prod FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'prod.invalido', dbname 'x')`,
			`CREATE USER MAPPING FOR PUBLIC SERVER outra_prod OPTIONS (user 'u', password 'segredo')`,
			`CREATE TABLE t (id int)`)
		for _, x := range []struct{ nome, banco string }{{"fdw.dump", "loja8_fdwc"}, {"fdw.sql", "loja8_fdws"}} {
			args := []string{"loja8_fdw"}
			if strings.HasSuffix(x.nome, ".dump") {
				args = []string{"-Fc", "loja8_fdw"}
			}
			c := dumpDeFora(t, a, o, x.nome, args...)
			p, err := PlanejarArquivo(ctx, a.d, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: x.banco, Jobs: 2})
			if err != nil || p.Bloqueado() {
				t.Fatalf("%s: %v %v", x.nome, err, p.Bloqueios)
			}
			marcou := false
			for i, cr := range p.Correcoes {
				if cr.Tipo == CorrecaoFDW && reflect.DeepEqual(cr.Nomes, []string{"outra_prod"}) {
					if cr.Marcada {
						t.Fatalf("%s: a correção dos user mappings vem desmarcada", x.nome)
					}
					p.Correcoes[i].Marcada, marcou = true, true
				}
			}
			if !marcou {
				t.Fatalf("%s: a correção dos user mappings deveria ser oferecida: %+v", x.nome, p.Correcoes)
			}
			e := a.executarArquivo(t, ctx, p)
			if e.Estado != cadastro.EstadoOK {
				t.Fatalf("%s: %s %s\n%s", x.nome, e.Estado, e.Mensagem, a.log.String())
			}
			if n := valor[int64](t, d, x.banco, `SELECT count(*) FROM pg_user_mappings`); n != 0 {
				t.Errorf("%s: os user mappings deveriam ter saído: %d", x.nome, n)
			}
		}
	})

	t.Run("o arquivo que muda depois da confirmação não roda", func(t *testing.T) {
		c := filepath.Join(a.d.Dir.Entrada(), "loja8.dump")
		p, err := PlanejarArquivo(ctx, a.d, PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8_muda", Jobs: 2})
		if err != nil || p.Bloqueado() {
			t.Fatalf("%v %v", err, p.Bloqueios)
		}
		agora := time.Now().Add(time.Minute)
		_ = os.Chtimes(c, agora, agora)
		e := a.executarArquivo(t, ctx, p)
		if e.Estado != cadastro.EstadoErro || !strings.Contains(e.Mensagem, "mudou desde a confirmação") {
			t.Fatalf("%s %s", e.Estado, e.Mensagem)
		}
		if valor[bool](t, d, "postgres", `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname LIKE 'loja8_muda%')`) {
			t.Fatal("nada deveria ter sido criado")
		}
	})

	t.Run("bloqueios", func(t *testing.T) {
		a.conexao(t, "prod", cadastro.TagProd, o)
		c := filepath.Join(a.d.Dir.Entrada(), "loja8.dump")
		for _, x := range []struct {
			pd   PedidoArquivo
			quer string
		}{
			{PedidoArquivo{Caminho: c, Conexao: "prod", Banco: "loja8", Jobs: 2}, "prod"},
			{PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "postgres", Jobs: 2}, "administrativo"},
			{PedidoArquivo{Caminho: c, Conexao: "homolog", Banco: "loja8__novo", Jobs: 2}, "forma de um banco da ferramenta"},
		} {
			p, err := PlanejarArquivo(ctx, a.d, x.pd)
			if err != nil || !strings.Contains(strings.Join(p.Bloqueios, " "), x.quer) {
				t.Errorf("%+v: %v %v (esperava %q)", x.pd, err, p.Bloqueios, x.quer)
			}
		}
		// Descer de versão: um dump do pg_dump 18 num servidor 16.
		a.conexao(t, "dev16", cadastro.TagDev, subir(t, "d16", 16))
		c18 := dumpDeFora(t, a, d, "loja8_18.dump", "-Fc", "loja8")
		if p, err := PlanejarArquivo(ctx, a.d, PedidoArquivo{Caminho: c18, Conexao: "dev16", Banco: "loja8", Jobs: 2}); err != nil || !strings.Contains(strings.Join(p.Bloqueios, " "), "descendo") {
			t.Errorf("18 → 16: %v %v", err, p.Bloqueios)
		}
		// Um SQL que sai do banco é recusado no plano, mesmo no segundo comando de uma linha.
		ruim := filepath.Join(a.d.Dir.Entrada(), "ruim.sql")
		_ = os.WriteFile(ruim, []byte("SELECT 1;\nSELECT 2; ALTER SYSTEM SET work_mem = '1GB';\n"), 0o600)
		if p, err := PlanejarArquivo(ctx, a.d, PedidoArquivo{Caminho: ruim, Conexao: "homolog", Banco: "ruim", Jobs: 1}); err != nil || !strings.Contains(strings.Join(p.Bloqueios, " "), "linha 2") {
			t.Errorf("SQL ruim: %v %v", err, p.Bloqueios)
		}
		// Fora da pasta de entrada, nem planeja.
		fora := filepath.Join(t.TempDir(), "x.dump")
		_ = os.WriteFile(fora, []byte("x"), 0o600)
		if _, err := PlanejarArquivo(ctx, a.d, PedidoArquivo{Caminho: fora, Conexao: "homolog", Banco: "x", Jobs: 2}); err == nil || !strings.Contains(err.Error(), "pasta de entrada") {
			t.Errorf("fora da entrada: %v", err)
		}
	})

	// Nada de segredo ou de role temporária sobrando.
	if ms, _ := filepath.Glob(filepath.Join(a.d.Dir.Temp(), "*")); len(ms) != 0 {
		t.Errorf("sobrou arquivo temporário: %v", ms)
	}
	if strings.Contains(a.log.String(), senhaTeste) {
		t.Error("a senha apareceu no log")
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'pghangar\_%'`); n != 0 {
		t.Errorf("sobrou role temporária")
	}
}
