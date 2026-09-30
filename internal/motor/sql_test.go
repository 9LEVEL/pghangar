package motor

import (
	"strings"
	"testing"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

func TestSQLCriarBancoPorProvedorEVersao(t *testing.T) {
	libc := cadastro.Banco{Codificacao: "UTF8", Collate: "pt_BR.UTF-8", Ctype: "pt_BR.UTF-8", Provedor: "c"}
	s := SQLCriarBanco(`loja"x__novo`, "app", libc, 180000)
	for _, e := range []string{`CREATE DATABASE "loja""x__novo"`, `OWNER "app"`, `TEMPLATE template0`, `LC_COLLATE 'pt_BR.UTF-8'`, `LOCALE_PROVIDER libc`} {
		if !strings.Contains(s, e) {
			t.Errorf("faltou %q em %s", e, s)
		}
	}
	icu := cadastro.Banco{Codificacao: "UTF8", Collate: "C", Ctype: "C", Provedor: "i", Locale: "und-x-icu", RegrasICU: "&a<b"}
	if s := SQLCriarBanco("n", "d", icu, 160000); !strings.Contains(s, "ICU_LOCALE 'und-x-icu'") || !strings.Contains(s, "ICU_RULES '&a<b'") {
		t.Error(s)
	}
	if s := SQLCriarBanco("n", "d", icu, 150000); strings.Contains(s, "ICU_RULES") {
		t.Error("15 não tem ICU_RULES:", s)
	}
	b := cadastro.Banco{Codificacao: "UTF8", Collate: "C", Ctype: "C", Provedor: "b", Locale: "C.UTF-8"}
	if s := SQLCriarBanco("n", "d", b, 170000); !strings.Contains(s, "BUILTIN_LOCALE 'C.UTF-8'") {
		t.Error(s)
	}
	if s := SQLCriarBanco("n", "d", libc, 140000); strings.Contains(s, "LOCALE_PROVIDER") {
		t.Error("14 não tem LOCALE_PROVIDER:", s)
	}
	// Um literal com aspas não escapa do comando.
	mal := cadastro.Banco{Codificacao: "UTF8", Collate: "x'; DROP DATABASE prod; --", Ctype: "C", Provedor: "c"}
	if s := SQLCriarBanco("n", "d", mal, 180000); !strings.Contains(s, `'x''; DROP DATABASE prod; --'`) {
		t.Error(s)
	}
}

func TestDividirGUC(t *testing.T) {
	casos := map[string][]string{
		`"$user", public`:       {"$user", "public"},
		`app, "Esquema, Um", x`: {"app", "Esquema, Um", "x"},
		`"a""b"`:                {`a"b`},
		``:                      nil,
		`public`:                {"public"},
	}
	for in, esperado := range casos {
		got := DividirGUC(in)
		if strings.Join(got, "|") != strings.Join(esperado, "|") || len(got) != len(esperado) {
			t.Errorf("%q: %q, esperava %q", in, got, esperado)
		}
	}
}

func TestSQLConfiguracao(t *testing.T) {
	s, err := SQLConfiguracao("loja__novo", "", `search_path="$user", public, app`)
	if err != nil || s != `ALTER DATABASE "loja__novo" SET "search_path" TO '$user', 'public', 'app'` {
		t.Fatal(s, err)
	}
	s, _ = SQLConfiguracao("n", "app", "work_mem=64MB")
	if s != `ALTER ROLE "app" IN DATABASE "n" SET "work_mem" TO '64MB'` {
		t.Fatal(s)
	}
	s, _ = SQLConfiguracao("n", "", "myapp.tenant=o'x")
	if s != `ALTER DATABASE "n" SET "myapp.tenant" TO 'o''x'` {
		t.Fatal(s)
	}
	if _, err := SQLConfiguracao("n", "", "semigual"); err == nil {
		t.Fatal("configuração sem = deveria falhar")
	}
}

func TestDiferencas(t *testing.T) {
	d := Diferencas(Contagem{"tabelas": 3, "funções": 1}, Contagem{"tabelas": 3, "índices": 2})
	if strings.Join(d, "|") != "funções: origem 1, destino 0|índices: origem 0, destino 2" {
		t.Fatal(d)
	}
}

// As linhas reais do --verbose (PostgreSQL 18), para o progresso.
func TestLinhasDoVerbose(t *testing.T) {
	if m := reDumpTabela.FindStringSubmatch(`pg_dump: dumping contents of table "s.t1"`); m == nil || m[1] != "s.t1" {
		t.Fatal(m)
	}
	vistos := map[string]string{}
	for _, l := range []string{
		`pg_restore: processing item 219 TABLE t1`,
		`pg_restore: creating TABLE "public.t1"`,
		`pg_restore: launching item 3447 TABLE DATA t1`,
		`pg_restore: processing data for table "public.t1"`,
		`pg_restore: finished item 3447 TABLE DATA t1`,
		`pg_restore: finished item 3298 CONSTRAINT t2 t2_pkey`,
	} {
		if m := reRestoreItem.FindStringSubmatch(l); m != nil {
			vistos[m[1]] = m[2]
		}
	}
	if len(vistos) != 3 || vistos["3447"] != "TABLE DATA t1" {
		t.Fatal(vistos)
	}
	if m := reErrosIgnorado.FindStringSubmatch(`pg_restore: warning: errors ignored on restore: 3`); m == nil || m[1] != "3" {
		t.Fatal(m)
	}
}

func TestPadroesDoPgDump(t *testing.T) {
	casos := []struct {
		padrao, schema, tabela string
		casa                   bool
	}{
		{"public.log_*", "public", "log_eventos", true},
		{"public.log_*", "vendas", "log_eventos", false},
		{"log_eventos", "qualquer", "log_eventos", true},
		{"*.pedido", "vendas", "pedido", true},
		{"vendas.ped?do", "vendas", "pedido", true},
		{`"Vendas"."Pedido"`, "Vendas", "Pedido", true},
		{`Vendas.Pedido`, "vendas", "pedido", true},
		{`"Vendas".pedido`, "vendas", "pedido", false},
		{"a.b", "a", "bx", false},
		{"x.y+z", "x", "y+z", true},
	}
	for _, c := range casos {
		if casaTabela(c.padrao, c.schema, c.tabela) != c.casa {
			t.Errorf("%s contra %s.%s: esperava %v", c.padrao, c.schema, c.tabela, c.casa)
		}
	}
	if !casaSchema([]string{"app_*"}, "app_1") || casaSchema([]string{"app_*"}, "xapp_1") {
		t.Error("casaSchema")
	}
}

func TestDiferencasLinhas(t *testing.T) {
	o := map[string]int64{"public.a": 10, "public.log": 5, "public.fora": 7}
	d := map[string]int64{"public.a": 10, "public.log": 0}
	if difs := DiferencasLinhas(o, d, []string{"public.log"}); len(difs) != 0 {
		t.Fatal(difs)
	}
	d["public.a"] = 9
	if difs := DiferencasLinhas(o, d, nil); len(difs) != 2 {
		t.Fatal(difs) // a diverge, e log sem o padrão de sem dados também
	}
}

func TestArgsDump(t *testing.T) {
	p := Plano{Perfil: cadastro.Perfil{JobsDump: 3, Compressao: "", Schemas: []string{"vendas"}, TabelasFora: []string{"public.x"}, SemDados: []string{"public.log"}}}
	a := strings.Join(argsDump(p), " ")
	for _, e := range []string{"--jobs=3", "--compress=zstd", "--schema=vendas", "--exclude-table=public.x", "--extension=*", "--exclude-table-data=public.log"} {
		if !strings.Contains(a, e) {
			t.Errorf("faltou %s em %s", e, a)
		}
	}
	p.Perfil.Compressao, p.Perfil.Schemas = "nenhuma", nil
	if a := strings.Join(argsDump(p), " "); !strings.Contains(a, "--compress=none") || strings.Contains(a, "--extension") {
		t.Error(a)
	}
	if err := versoes.Conferir("pg_dump", argsDump(p), 16); err != nil {
		t.Error(err)
	}
}

// Uma extensão mais antiga no destino pede atenção; mais nova é só uma informação. O caso que
// motivou: vector 0.8.6 na origem e 0.8.1 no destino; pg_stat_statements 1.10 e 1.12.
func TestAvisoExtensaoPelaVersao(t *testing.T) {
	if txt, atencao := avisoExtensao("vector", "0.8.6", "0.8.1"); !atencao || !strings.Contains(txt, "mais antiga") {
		t.Fatalf("vector mais antiga no destino: %v %q", atencao, txt)
	}
	if txt, atencao := avisoExtensao("pg_stat_statements", "1.10", "1.12"); atencao || !strings.Contains(txt, "mais nova") {
		t.Fatalf("pg_stat_statements mais nova no destino: %v %q", atencao, txt)
	}
	if _, atencao := avisoExtensao("x", "1.0beta", "1.0"); !atencao {
		t.Fatal("sem dar para comparar, pede atenção")
	}
	for _, c := range []struct {
		a, b string
		quer int
	}{{"1.10", "1.9", 1}, {"0.8.1", "0.8.6", -1}, {"1.2", "1.2.0", 0}, {"2.0-1", "2.0-2", -1}} {
		if got, ok := compararVersoes(c.a, c.b); !ok || got != c.quer {
			t.Fatalf("%s × %s: %d %v", c.a, c.b, got, ok)
		}
	}
}
