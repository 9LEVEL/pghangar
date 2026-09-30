//go:build integracao

// Testes de ponta a ponta do motor, com containers Postgres LOCAIS (presos em 127.0.0.1) e um
// servidor SSH dentro do próprio processo. Nenhum outro host é contatado.
//
//	go test -tags integracao ./internal/motor/ -v -timeout 30m
package motor

import (
	"context"
	dbsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/testessh"
	"github.com/9LEVEL/pghangar/internal/trava"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

const senhaTeste = "s3nh@:com'aspas\\e barra"

type pg struct {
	nome   string
	versao int
	porta  int
}

var (
	muPG     sync.Mutex
	servidor = map[string]*pg{}
)

// subir sobe (uma vez por processo) um Postgres local preso em 127.0.0.1.
func subir(t *testing.T, chave string, versao int) *pg {
	t.Helper()
	muPG.Lock()
	defer muPG.Unlock()
	if s, ok := servidor[chave]; ok {
		return s
	}
	nome := fmt.Sprintf("pghangar-teste-%s-%d", chave, rand.Intn(1_000_000))
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", nome, "-e", "POSTGRES_PASSWORD="+senhaTeste,
		"-p", "127.0.0.1::5432", fmt.Sprintf("postgres:%d", versao)).CombinedOutput()
	if err != nil {
		t.Fatalf("subindo %s: %v %s", nome, err, out)
	}
	out, err = exec.Command("docker", "port", nome, "5432/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	linha := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	if !strings.HasPrefix(linha, "127.0.0.1:") {
		t.Fatalf("o container de teste precisa estar preso em 127.0.0.1: %s", linha)
	}
	porta, _ := strconv.Atoi(linha[strings.LastIndex(linha, ":")+1:])
	s := &pg{nome: nome, versao: versao, porta: porta}
	// Espera aceitar conexões (o entrypoint reinicia o servidor uma vez).
	prazo := time.Now().Add(90 * time.Second)
	for {
		c, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", urlEsc(senhaTeste), porta))
		if err == nil {
			var ok int
			err = c.QueryRow(context.Background(), "SELECT 1").Scan(&ok)
			_ = c.Close(context.Background())
			if err == nil {
				time.Sleep(1500 * time.Millisecond)
				if c2, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", urlEsc(senhaTeste), porta)); err == nil {
					_ = c2.Close(context.Background())
					break
				}
			}
		}
		if time.Now().After(prazo) {
			t.Fatalf("%s não subiu: %v", nome, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	servidor[chave] = s
	return s
}

func abrirSQL(t *testing.T, p string) *dbsql.DB {
	t.Helper()
	db, err := dbsql.Open("sqlite", p+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func urlEsc(s string) string {
	r := strings.NewReplacer("%", "%25", "@", "%40", ":", "%3A", "'", "%27", "\\", "%5C", " ", "%20")
	return r.Replace(s)
}

func TestMain(m *testing.M) {
	cod := m.Run()
	muPG.Lock()
	for _, s := range servidor {
		// -v: a imagem do postgres declara VOLUME; um rm -f explícito passa na frente do --rm e o volume anônimo (GBs) fica órfão.
		_ = exec.Command("docker", "rm", "-fv", s.nome).Run()
	}
	muPG.Unlock()
	os.Exit(cod)
}

func sql(t *testing.T, s *pg, banco string, cmds ...string) {
	t.Helper()
	c := conectar(t, s, banco)
	defer c.Close(context.Background())
	for _, cmd := range cmds {
		if _, err := c.Exec(context.Background(), cmd); err != nil {
			t.Fatalf("%s/%s: %s: %v", s.nome, banco, cmd, err)
		}
	}
}

func conectar(t *testing.T, s *pg, banco string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname='%s' sslmode=disable", s.porta, strings.ReplaceAll(banco, "'", `\'`)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = senhaTeste
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("conectando em %s/%s: %v", s.nome, banco, err)
	}
	return c
}

func valor[T any](t *testing.T, s *pg, banco, q string, args ...any) T {
	t.Helper()
	c := conectar(t, s, banco)
	defer c.Close(context.Background())
	var v T
	if err := c.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s/%s: %s: %v", s.nome, banco, q, err)
	}
	return v
}

// semear cria o banco de origem com um pouco de tudo.
func semear(t *testing.T, s *pg, banco string, extra ...string) {
	sql(t, s, "postgres", fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, id(banco)), fmt.Sprintf(`CREATE DATABASE %s`, id(banco)))
	cmds := []string{
		`CREATE EXTENSION pg_trgm`, `CREATE EXTENSION citext`,
		`CREATE SCHEMA vendas`,
		`CREATE TABLE vendas.cliente (id serial PRIMARY KEY, nome text NOT NULL, email citext UNIQUE)`,
		`CREATE TABLE vendas.pedido (id bigserial PRIMARY KEY, cliente int REFERENCES vendas.cliente, valor numeric(12,2) CHECK (valor >= 0), criado timestamptz DEFAULT now())`,
		`CREATE INDEX pedido_trgm ON vendas.cliente USING gin (nome gin_trgm_ops)`,
		`INSERT INTO vendas.cliente (nome, email) SELECT 'cliente ' || g, 'c' || g || '@x.com' FROM generate_series(1, 2000) g`,
		`INSERT INTO vendas.pedido (cliente, valor) SELECT 1 + g % 2000, g % 500 FROM generate_series(1, 20000) g`,
		`CREATE VIEW vendas.resumo AS SELECT cliente, sum(valor) total FROM vendas.pedido GROUP BY cliente`,
		`CREATE MATERIALIZED VIEW vendas.top AS SELECT * FROM vendas.resumo ORDER BY total DESC LIMIT 10`,
		`CREATE FUNCTION vendas.carimbo() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.criado := now(); RETURN NEW; END $$`,
		`CREATE TRIGGER pedido_carimbo BEFORE INSERT ON vendas.pedido FOR EACH ROW EXECUTE FUNCTION vendas.carimbo()`,
		`CREATE TABLE public.log_eventos (id serial, msg text)`,
		`INSERT INTO public.log_eventos (msg) SELECT 'evento ' || g FROM generate_series(1, 5000) g`,
		// RLS com FORCE: só o superusuário passa (como no 9level-id).
		`CREATE TABLE public.segredo (org int, valor text)`,
		`ALTER TABLE public.segredo ENABLE ROW LEVEL SECURITY`, `ALTER TABLE public.segredo FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY por_org ON public.segredo USING (org = current_setting('app.org', true)::int)`,
		`INSERT INTO public.segredo VALUES (1, 'a'), (2, 'b')`,
		`COMMENT ON TABLE vendas.pedido IS 'pedidos da loja'`,
	}
	sql(t, s, banco, append(cmds, extra...)...)
}

// destinoHomolog cria o banco de destino que já existe, com dono próprio e configurações dele.
func destinoHomolog(t *testing.T, s *pg, banco string) {
	sql(t, s, "postgres",
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_homolog') THEN CREATE ROLE app_homolog LOGIN PASSWORD 'x'; END IF; END $$`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leitor') THEN CREATE ROLE leitor LOGIN; END IF; END $$`,
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, id(banco)),
		fmt.Sprintf(`CREATE DATABASE %s OWNER app_homolog`, id(banco)),
		fmt.Sprintf(`ALTER DATABASE %s SET search_path TO "$user", public, vendas`, id(banco)),
		fmt.Sprintf(`ALTER ROLE app_homolog IN DATABASE %s SET work_mem = '8MB'`, id(banco)),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, id(banco)),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO leitor`, id(banco)),
		fmt.Sprintf(`ALTER DATABASE %s CONNECTION LIMIT 50`, id(banco)),
		fmt.Sprintf(`COMMENT ON DATABASE %s IS 'homolog da loja'`, id(banco)),
	)
	sql(t, s, banco, `CREATE TABLE velha (x int)`, `INSERT INTO velha VALUES (42)`, `ALTER TABLE velha OWNER TO app_homolog`)
}

type ambiente struct {
	d   Deps
	cad *cadastro.Cadastro
	log *strings.Builder
}

func novoAmbiente(t *testing.T) *ambiente {
	t.Helper()
	dir := local.Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := dir.Preparar(); err != nil {
		t.Fatal(err)
	}
	cad, err := cadastro.Abrir(dir.Estado())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cad.Fechar() })
	dk := imagens.Docker{}
	ctx := context.Background()
	for _, v := range []int{16, 17, 18} {
		ref := imagens.Referencia("postgres", v)
		dg, err := dk.Digest(ctx, ref)
		if err != nil {
			t.Fatalf("a imagem %s precisa estar baixada: %v", ref, err)
		}
		cl, err := dk.VersaoCliente(ctx, dg)
		if err != nil {
			t.Fatal(err)
		}
		if err := cad.SalvarImagem(ctx, cadastro.Imagem{Versao: v, Referencia: ref, Digest: dg, Cliente: cl, BaixadaEm: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	log := &strings.Builder{}
	return &ambiente{cad: cad, log: log, d: Deps{Cadastro: cad, Dir: dir, Docker: dk,
		Amb: conexao.Ambiente{KnownHosts: dir.KnownHosts(), ChavePadrao: dir.ChaveSSH(), DirTemp: dir.Temp()}, Log: &travado{b: log}, Maquina: "teste"}}
}

// travado é um Writer seguro para as goroutines do motor.
type travado struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *travado) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (a *ambiente) conexao(t *testing.T, nome, tag string, s *pg) {
	t.Helper()
	c := cadastro.Conexao{Nome: nome, Tag: tag, Acesso: cadastro.AcessoDireto, Host: "127.0.0.1", Porta: s.porta, Usuario: "postgres",
		ModoSenha: cadastro.SenhaGuardar, Senha: senhaTeste, SSLMode: "disable", BancoAdmin: "postgres"}
	if err := a.cad.SalvarConexao(context.Background(), "", c); err != nil {
		t.Fatal(err)
	}
	if tag != cadastro.TagProd {
		a.aprovar(t, s, nome)
	}
}

// aprovar aprova o servidor como destino (o que o sysadmin faz uma vez, com a tecla v).
func (a *ambiente) aprovar(t *testing.T, s *pg, nome string) {
	t.Helper()
	sid := valor[string](t, s, "postgres", `SELECT system_identifier::text FROM pg_control_system()`)
	if err := a.cad.AprovarDestino(context.Background(), sid, nome, "teste"); err != nil {
		t.Fatal(err)
	}
}

func (a *ambiente) perfil(t *testing.T, nome, origem, bancoO, destino, bancoD string, mudar ...func(*cadastro.Perfil)) {
	t.Helper()
	p := cadastro.Perfil{Nome: nome, Origem: origem, OrigemBanco: bancoO, Destino: destino, DestinoBanco: bancoD,
		JobsDump: 2, JobsRestore: 2, SemDados: []string{"public.log_eventos"}}
	for _, f := range mudar {
		f(&p)
	}
	if err := a.cad.SalvarPerfil(context.Background(), "", p); err != nil {
		t.Fatal(err)
	}
}

// copiar planeja, confirma (registra) e executa, como a tela faz.
func (a *ambiente) copiar(t *testing.T, ctx context.Context, perfil string, apagar ...string) cadastro.Execucao {
	t.Helper()
	p, err := Planejar(ctx, a.d, perfil)
	if err != nil {
		t.Fatalf("planejando %s: %v", perfil, err)
	}
	if p.Bloqueado() {
		t.Fatalf("%s bloqueado: %v", perfil, p.Bloqueios)
	}
	return a.executar(t, ctx, p, apagar...)
}

func (a *ambiente) executar(t *testing.T, ctx context.Context, p Plano, apagar ...string) cadastro.Execucao {
	t.Helper()
	pj, _ := json.Marshal(p)
	id, err := a.cad.NovaExecucao(context.Background(), cadastro.Execucao{Perfil: p.Perfil.Nome, Tipo: cadastro.TipoCopia,
		Estado: cadastro.EstadoIniciando, Destino: p.Perfil.Destino, Banco: p.Perfil.DestinoBanco, Apagar: apagar, Plano: string(pj)})
	if err != nil {
		t.Fatal(err)
	}
	_ = Executar(ctx, a.d, id)
	e, err := a.cad.Execucao(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func conferirCopia(t *testing.T, a *ambiente, e cadastro.Execucao, origem, destino *pg, bancoO, bancoD string) {
	t.Helper()
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("estado %s: %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	// Os dados vieram, menos os da tabela sem dados.
	for _, q := range []string{`SELECT count(*) FROM vendas.cliente`, `SELECT count(*) FROM vendas.pedido`} {
		if o, d := valor[int64](t, origem, bancoO, q), valor[int64](t, destino, bancoD, q); o != d {
			t.Errorf("%s: origem %d, destino %d", q, o, d)
		}
	}
	if n := valor[int64](t, destino, bancoD, `SELECT count(*) FROM public.log_eventos`); n != 0 {
		t.Errorf("log_eventos deveria vir sem dados: %d linhas", n)
	}
	if n := valor[int64](t, destino, bancoD, `SELECT count(*) FROM public.segredo`); n != 2 {
		t.Errorf("a tabela com FORCE RLS deveria vir inteira (superusuário): %d", n)
	}
	// Tudo com o dono do homolog, e nada com a role temporária.
	donos := valor[string](t, destino, bancoD, `SELECT string_agg(DISTINCT pg_get_userbyid(relowner), ',') FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('vendas', 'public') AND c.relkind IN ('r','v','m','S')`)
	if donos != "app_homolog" {
		t.Errorf("donos: %s", donos)
	}
	if n := valor[int64](t, destino, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'copia\_banco\_%'`); n != 0 {
		t.Errorf("sobrou role temporária")
	}
	// As configurações do banco de homolog foram mantidas.
	cfg := valor[string](t, destino, "postgres", `SELECT string_agg(array_to_string(setconfig, ','), ';' ORDER BY setrole) FROM pg_db_role_setting
		WHERE setdatabase = (SELECT oid FROM pg_database WHERE datname = $1)`, bancoD)
	if !strings.Contains(cfg, `search_path="$user", public, vendas`) || !strings.Contains(cfg, "work_mem=8MB") {
		t.Errorf("configurações: %s", cfg)
	}
	// O homolog tirou o CONNECT de PUBLIC (o TEMP ficou) e deu a leitor: a cópia mantém isso.
	acl := valor[string](t, destino, "postgres", `SELECT string_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		|| ':' || a.privilege_type, ',' ORDER BY 1) FROM pg_database d, aclexplode(d.datacl) a WHERE d.datname = $1`, bancoD)
	if !strings.Contains(acl, "leitor:CONNECT") || strings.Contains(acl, "PUBLIC:CONNECT") || !strings.Contains(acl, "PUBLIC:TEMPORARY") {
		t.Errorf("acl: %s", acl)
	}
	if n := valor[int32](t, destino, "postgres", `SELECT datconnlimit FROM pg_database WHERE datname = $1`, bancoD); n != 50 {
		t.Errorf("limite: %d", n)
	}
	if c := valor[string](t, destino, "postgres", `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, bancoD); c != "homolog da loja" {
		t.Errorf("comentário: %s", c)
	}
	// O anterior guarda o banco velho, fechado para conexões.
	if e.BancoAnterior == "" {
		t.Fatal("sem anterior")
	}
	if ok := valor[bool](t, destino, "postgres", `SELECT datallowconn FROM pg_database WHERE datname = $1`, e.BancoAnterior); ok {
		t.Error("o anterior deveria estar fechado para conexões")
	}
	// O pgpass temporário sumiu, e a senha não foi para o log.
	if ms, _ := filepath.Glob(filepath.Join(a.d.Dir.Temp(), "*")); len(ms) != 0 {
		t.Errorf("sobrou arquivo temporário: %v", ms)
	}
	if strings.Contains(a.log.String(), senhaTeste) {
		t.Error("a senha apareceu no log")
	}
	// O dump ficou, com o manifesto completo.
	b, err := os.ReadFile(filepath.Join(e.DumpDir, "manifesto.json"))
	if err != nil || !strings.Contains(string(b), `"estado": "completo"`) {
		t.Errorf("manifesto: %s %v", b, err)
	}
}

// A matriz de versões: 6 cópias que sobem ou mantêm a versão, 3 bloqueadas.
func TestMatrizDeVersoes(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	for _, v := range []int{16, 17, 18} {
		o := subir(t, fmt.Sprintf("o%d", v), v)
		d := subir(t, fmt.Sprintf("d%d", v), v)
		a.conexao(t, fmt.Sprintf("prod%d", v), cadastro.TagProd, o)
		a.conexao(t, fmt.Sprintf("homolog%d", v), cadastro.TagHomolog, d)
		semear(t, o, "loja")
	}
	for _, vo := range []int{16, 17, 18} {
		for _, vd := range []int{16, 17, 18} {
			nome := fmt.Sprintf("p%d_%d", vo, vd)
			a.perfil(t, nome, fmt.Sprintf("prod%d", vo), "loja", fmt.Sprintf("homolog%d", vd), "loja")
			t.Run(nome, func(t *testing.T) {
				o, d := servidor[fmt.Sprintf("o%d", vo)], servidor[fmt.Sprintf("d%d", vd)]
				p, err := Planejar(ctx, a.d, nome)
				if err != nil {
					t.Fatal(err)
				}
				if vd < vo {
					if !p.Bloqueado() || !strings.Contains(strings.Join(p.Bloqueios, " "), "descendo") {
						t.Fatalf("%d→%d deveria ser bloqueado: %v", vo, vd, p.Bloqueios)
					}
					return
				}
				if p.Imagem != vd || p.Confirmacao() != "loja" {
					t.Fatalf("imagem %d (esperava %d), confirmação %q", p.Imagem, vd, p.Confirmacao())
				}
				destinoHomolog(t, d, "loja")
				e := a.executar(t, ctx, p)
				conferirCopia(t, a, e, o, d, "loja", "loja")
			})
		}
	}
}

func TestSegundaCopiaApagaAnteriorMarcadoEDesfazer(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "loja2")
	destinoHomolog(t, d, "loja2")
	a.perfil(t, "p", "prod", "loja2", "homolog", "loja2")

	e1 := a.copiar(t, ctx, "p")
	conferirCopia(t, a, e1, o, d, "loja2", "loja2")
	time.Sleep(1100 * time.Millisecond) // o nome do anterior tem segundos

	// A segunda cópia vê o anterior e apaga só o que foi marcado.
	p, err := Planejar(ctx, a.d, "p")
	if err != nil || len(p.Anteriores) != 1 || p.Anteriores[0].Nome != e1.BancoAnterior {
		t.Fatalf("%+v %v", p.Anteriores, err)
	}
	// Adversarial: marcar para apagar algo que não é anterior é recusado, e nada é tocado.
	eMal := a.executar(t, ctx, p, "postgres")
	if eMal.Estado != cadastro.EstadoErro || !strings.Contains(eMal.Mensagem, "recusado") {
		t.Fatalf("apagar 'postgres' deveria ser recusado: %s %s", eMal.Estado, eMal.Mensagem)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'postgres'`); n != 1 {
		t.Fatal("o banco postgres sumiu")
	}

	p, _ = Planejar(ctx, a.d, "p")
	e2 := a.executar(t, ctx, p, e1.BancoAnterior)
	if e2.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e2.Estado, e2.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname = $1`, e1.BancoAnterior); n != 0 {
		t.Fatal("o anterior marcado deveria ter sido apagado")
	}

	// Desfazer: o anterior da 2ª cópia é o banco da 1ª, que já era cópia. Pomos um marcador no
	// banco atual para ver que ele não se perde.
	sql(t, d, "loja2", `CREATE TABLE marcador (x int)`)
	guardado, err := Desfazer(ctx, a.d, "homolog", "loja2", e2.BancoAnterior)
	if err != nil {
		t.Fatal(err)
	}
	if n := valor[int64](t, d, "loja2", `SELECT count(*) FROM pg_tables WHERE tablename = 'marcador'`); n != 0 {
		t.Fatal("desfazer deveria trazer o banco anterior")
	}
	if valor[bool](t, d, "postgres", `SELECT datallowconn FROM pg_database WHERE datname = $1`, guardado) {
		t.Fatal("o banco guardado pelo desfazer deveria estar fechado para conexões")
	}
	sql(t, d, "postgres", "ALTER DATABASE "+id(guardado)+" ALLOW_CONNECTIONS true") // só para o teste olhar dentro
	if n := valor[int64](t, d, guardado, `SELECT count(*) FROM pg_tables WHERE tablename = 'marcador'`); n != 1 {
		t.Fatal("o banco desfeito deveria ter ficado guardado como anterior")
	}
	if !valor[bool](t, d, "postgres", `SELECT datallowconn FROM pg_database WHERE datname = 'loja2'`) {
		t.Fatal("o banco de volta deveria aceitar conexões")
	}

	// Apagar recusa o que não é da ferramenta.
	for _, nome := range []string{"loja2", "postgres", "loja2__anteriorx", "outra__novo"} {
		if err := Apagar(ctx, a.d, "homolog", "loja2", nome); err == nil {
			t.Errorf("apagar %s deveria ser recusado", nome)
		}
	}
	if err := Apagar(ctx, a.d, "homolog", "loja2", guardado); err != nil {
		t.Fatal(err)
	}
	l, err := Listar(ctx, a.d, "homolog", "loja2")
	if err != nil || !l.Existe || l.Novo != nil {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestGuardas(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	a.conexao(t, "mesmo", cadastro.TagDev, o) // o mesmo servidor da origem, com outro nome
	semear(t, o, "loja3")

	// O mesmo cluster por dois nomes.
	a.perfil(t, "mesmo", "prod", "loja3", "mesmo", "loja3_copia")
	if p, _ := Planejar(ctx, a.d, "mesmo"); !strings.Contains(strings.Join(p.Bloqueios, " "), "prod nunca é destino") {
		t.Fatalf("o servidor da produção por outro nome: %v", p.Bloqueios)
	}
	// Adversarial: a produção cadastrada de novo, sem a tag prod e com outro nome de host
	// ("localhost" em vez de 127.0.0.1). A origem é outro servidor. Só o system_identifier pega.
	disfarce := cadastro.Conexao{Nome: "disfarcada", Tag: cadastro.TagDev, Acesso: cadastro.AcessoDireto, Host: "localhost", Porta: o.porta,
		Usuario: "postgres", ModoSenha: cadastro.SenhaGuardar, Senha: senhaTeste, SSLMode: "disable", BancoAdmin: "postgres"}
	if err := a.cad.SalvarConexao(ctx, "", disfarce); err != nil {
		t.Fatal(err)
	}
	semear(t, d, "de_homolog")
	a.perfil(t, "contra-prod", "homolog", "de_homolog", "disfarcada", "loja3")
	p0, _ := Planejar(ctx, a.d, "contra-prod")
	if !strings.Contains(strings.Join(p0.Bloqueios, " "), "mesmo servidor da conexão prod") {
		t.Fatalf("produção disfarçada de dev passou: %v", p0.Bloqueios)
	}
	if err := Apagar(ctx, a.d, "disfarcada", "loja3", "loja3__novo"); err == nil || !strings.Contains(err.Error(), "prod") {
		t.Fatalf("apagar na produção disfarçada: %v", err)
	}

	// Outra cópia em andamento no destino.
	a.perfil(t, "p", "prod", "loja3", "homolog", "loja3")
	sid := valor[string](t, d, "postgres", `SELECT system_identifier::text FROM pg_control_system()`)
	tr, err := trava.Obter(a.d.Dir.Travas(), trava.Destino("homolog", sid), "loja3")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := Planejar(ctx, a.d, "p"); !strings.Contains(strings.Join(p.Bloqueios, " "), "em andamento") {
		t.Fatalf("trava: %v", p.Bloqueios)
	}
	tr.Soltar()

	// Um servidor de destino não aprovado bloqueia a cópia; aprovado de novo, ela segue.
	sidD := valor[string](t, d, "postgres", `SELECT system_identifier::text FROM pg_control_system()`)
	if err := a.cad.RevogarDestino(ctx, sidD); err != nil {
		t.Fatal(err)
	}
	if p, _ := Planejar(ctx, a.d, "p"); !strings.Contains(strings.Join(p.Bloqueios, " "), "aprovado como destino") {
		t.Fatalf("destino não aprovado: %v", p.Bloqueios)
	}
	a.aprovar(t, d, "homolog")
	if p, _ := Planejar(ctx, a.d, "p"); strings.Contains(strings.Join(p.Bloqueios, " "), "aprovado") {
		t.Fatalf("aprovado de novo: %v", p.Bloqueios)
	}

	// Banco que não existe na origem.
	a.perfil(t, "sem", "prod", "nao_existe", "homolog", "x")
	if p, _ := Planejar(ctx, a.d, "sem"); !strings.Contains(strings.Join(p.Bloqueios, " "), "não existe na origem") {
		t.Fatalf("banco inexistente: %v", p.Bloqueios)
	}

	// Adversarial: um cadastro adulterado com destino prod (a API recusa; aqui é SQL direto).
	db := abrirSQL(t, a.d.Dir.Estado())
	if _, err := db.Exec(`UPDATE conexoes SET tag = 'prod' WHERE nome = 'homolog'`); err != nil {
		t.Fatal(err)
	}
	p, _ := Planejar(ctx, a.d, "p")
	if !strings.Contains(strings.Join(p.Bloqueios, " "), "prod nunca é destino") {
		t.Fatalf("destino prod: %v", p.Bloqueios)
	}
	// Mesmo com um plano "confirmado" antes da adulteração, a execução recusa.
	e := a.executar(t, ctx, Plano{Perfil: p.Perfil})
	if e.Estado != cadastro.EstadoErro || !strings.Contains(e.Mensagem, "prod") {
		t.Fatalf("execução com destino prod: %s %s", e.Estado, e.Mensagem)
	}
	if err := Apagar(ctx, a.d, "homolog", "loja3", "loja3__novo"); err == nil || !strings.Contains(err.Error(), "prod") {
		t.Fatalf("apagar em conexão prod: %v", err)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname LIKE 'loja3%'`); n != 0 {
		t.Fatal("algo foi criado no destino prod")
	}
	if _, err := db.Exec(`UPDATE conexoes SET tag = 'homolog' WHERE nome = 'homolog'`); err != nil {
		t.Fatal(err)
	}

	// O destino mudou entre a confirmação e a execução.
	p, _ = Planejar(ctx, a.d, "p")
	p.Destino.SystemID = "123"
	e = a.executar(t, ctx, p)
	if e.Estado != cadastro.EstadoErro || !strings.Contains(e.Mensagem, "mudou") {
		t.Fatalf("destino trocado: %s %s", e.Estado, e.Mensagem)
	}
}

func TestRestoreComErroEsperaDecisao(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o17", 17), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "dev", cadastro.TagDev, d)
	// Uma política para uma role que só existe na origem: o restore falha nela.
	sql(t, o, "postgres", `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'so_na_prod') THEN CREATE ROLE so_na_prod; END IF; END $$`)
	semear(t, o, "loja4", `CREATE POLICY so_prod ON public.segredo TO so_na_prod USING (true)`)
	a.perfil(t, "p", "prod", "loja4", "dev", "loja4")

	e := a.copiar(t, ctx, "p")
	if e.Estado != cadastro.EstadoAguardando || e.ErrosRestore == 0 {
		t.Fatalf("%s %d %s\n%s", e.Estado, e.ErrosRestore, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'loja4'`); n != 0 {
		t.Fatal("a troca não deveria ter acontecido")
	}
	if n := valor[int64](t, d, "loja4__novo", `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatalf("o __novo deveria estar pronto: %d", n)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'copia\_banco\_%'`); n != 0 {
		t.Fatal("sobrou role temporária")
	}
	// Um destino dev confirma com y.
	var p Plano
	_ = json.Unmarshal([]byte(e.Plano), &p)
	if p.Confirmacao() != "" {
		t.Fatalf("dev confirma com y: %q", p.Confirmacao())
	}
	// O __novo, enquanto espera, já é do dono e está aberto como um banco novo; mas durante o
	// restore ele estava fechado (conferido no teste do binário).
	// Adversarial: o __novo some e outro com o mesmo nome aparece (outra cópia): a troca adiada
	// recusa, e nada muda no destino.
	sql(t, d, "postgres", `DROP DATABASE loja4__novo WITH (FORCE)`, `CREATE DATABASE loja4__novo`)
	if err := TrocarDepois(ctx, a.d, e.ID); err == nil || !strings.Contains(err.Error(), "recriado") {
		t.Fatalf("trocou um __novo que não é o desta execução: %v", err)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'loja4'`); n != 0 {
		t.Fatal("a troca recusada mexeu no destino")
	}
	if ex, _ := a.cad.Execucao(ctx, e.ID); ex.Estado != cadastro.EstadoErro {
		t.Fatalf("__novo perdido encerra a execução: %s", ex.Estado)
	}
	// Apagar o __novo encerra também uma execução que o aguardava.
	if err := Apagar(ctx, a.d, "dev", "loja4", "loja4__novo"); err != nil {
		t.Fatal(err)
	}
	e = a.copiar(t, ctx, "p")
	if e.Estado != cadastro.EstadoAguardando {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	if err := Apagar(ctx, a.d, "dev", "loja4", "loja4__novo"); err != nil {
		t.Fatal(err)
	}
	if ex, _ := a.cad.Execucao(ctx, e.ID); ex.Estado != cadastro.EstadoErro || !strings.Contains(ex.Mensagem, "apagado") {
		t.Fatalf("a execução que aguardava o __novo apagado: %s %s", ex.Estado, ex.Mensagem)
	}
	e = a.copiar(t, ctx, "p")
	if e.Estado != cadastro.EstadoAguardando {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	// O sysadmin decide trocar mesmo assim. O banco não existia: não há anterior.
	if err := TrocarDepois(ctx, a.d, e.ID); err != nil {
		t.Fatal(err)
	}
	e, _ = a.cad.Execucao(ctx, e.ID)
	if e.Estado != cadastro.EstadoOK || e.BancoAnterior != "" {
		t.Fatalf("%s %s %s", e.Estado, e.BancoAnterior, e.Mensagem)
	}
	if n := valor[int64](t, d, "loja4", `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatal(n)
	}
	// Trocar de novo é recusado.
	if err := TrocarDepois(ctx, a.d, e.ID); err == nil {
		t.Fatal("trocar duas vezes")
	}
}

func TestScriptPosRestore(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o16", 16), subir(t, "d16", 16)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "loja5", `CREATE EXTENSION postgres_fdw`,
		`CREATE SERVER outro FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'nao-existe.invalid', dbname 'x')`)
	destinoHomolog(t, d, "loja5")
	dir := t.TempDir()
	bom := filepath.Join(dir, "bom.sql")
	_ = os.WriteFile(bom, []byte("UPDATE vendas.cliente SET email = NULL WHERE id > 10;\nCREATE TABLE public.config_homolog (k text, v text);\nINSERT INTO public.config_homolog VALUES ('url', 'https://homolog');\n"), 0o600)
	ruim := filepath.Join(dir, "ruim.sql")
	_ = os.WriteFile(ruim, []byte("SELECT 1;\nSELECT * FROM tabela_que_nao_existe;\nCREATE TABLE nao_deveria (x int);\n"), 0o600)
	a.perfil(t, "bom", "prod", "loja5", "homolog", "loja5", func(p *cadastro.Perfil) { p.Script = bom })
	a.perfil(t, "ruim", "prod", "loja5", "homolog", "loja5b", func(p *cadastro.Perfil) { p.Script = ruim })

	e := a.copiar(t, ctx, "bom")
	conferirCopia(t, a, e, o, d, "loja5", "loja5")
	if dono := valor[string](t, d, "loja5", `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE relname = 'config_homolog'`); dono != "app_homolog" {
		t.Fatalf("o que o script cria vai para o dono do destino: %s", dono)
	}
	// O foreign-data wrapper só pode ser de superusuário: fica com o usuário da conexão; o servidor
	// estrangeiro vai para o dono.
	if dono := valor[string](t, d, "loja5", `SELECT pg_get_userbyid(fdwowner) FROM pg_foreign_data_wrapper WHERE fdwname = 'postgres_fdw'`); dono != "postgres" {
		t.Fatalf("dono do FDW: %s", dono)
	}
	if dono := valor[string](t, d, "loja5", `SELECT pg_get_userbyid(srvowner) FROM pg_foreign_server WHERE srvname = 'outro'`); dono != "app_homolog" {
		t.Fatalf("dono do servidor estrangeiro: %s", dono)
	}
	if n := valor[int64](t, d, "loja5", `SELECT count(*) FROM vendas.cliente WHERE email IS NOT NULL`); n != 10 {
		t.Fatal(n)
	}

	e = a.copiar(t, ctx, "ruim")
	if e.Estado != cadastro.EstadoErro || !strings.Contains(e.Mensagem, "script") || !strings.Contains(e.Mensagem, "loja5b__novo") {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'copia\_banco\_%'`); n != 0 {
		t.Fatal("sobrou role temporária depois do script com erro")
	}
	// O __novo ficou e bloqueia a próxima cópia até ser apagado.
	p, _ := Planejar(ctx, a.d, "ruim")
	if !p.NovoExiste || !p.Bloqueado() {
		t.Fatalf("o __novo que sobrou deveria bloquear: %v", p.Bloqueios)
	}
	if err := Apagar(ctx, a.d, "homolog", "loja5b", "loja5b__novo"); err != nil {
		t.Fatal(err)
	}
}

func TestNomesEstranhosEBancoNovo(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o17", 17), subir(t, "d17", 17)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "dev", cadastro.TagDev, d)
	origem := `Loja "Teste" ç'; DROP DATABASE postgres; --`
	destino := strings.Repeat("destino_muito_longo_", 3) // 60 bytes: os derivados são encurtados
	semear(t, o, origem)
	a.perfil(t, "estranho", "prod", origem, "dev", destino)
	e := a.copiar(t, ctx, "estranho")
	if e.Estado != cadastro.EstadoOK || e.BancoAnterior != "" {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, destino, `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatal(n)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'postgres'`); n != 1 {
		t.Fatal("injeção pelo nome do banco")
	}
	// Segunda cópia: o destino agora existe e vira anterior com nome encurtado.
	time.Sleep(1100 * time.Millisecond)
	e = a.copiar(t, ctx, "estranho")
	if e.Estado != cadastro.EstadoOK || len(e.BancoAnterior) > 63 || e.BancoAnterior == "" {
		t.Fatalf("%s %q %s", e.Estado, e.BancoAnterior, e.Mensagem)
	}
}

func TestCancelarNoDumpNaoTocaODestino(t *testing.T) {
	a := novoAmbiente(t)
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "grande", `CREATE TABLE public.volume AS SELECT g, md5(g::text) m FROM generate_series(1, 3000000) g`)
	destinoHomolog(t, d, "grande")
	a.perfil(t, "p", "prod", "grande", "homolog", "grande", func(p *cadastro.Perfil) { p.JobsDump = 1 })

	ctx, cancel := context.WithCancel(context.Background())
	p, err := Planejar(ctx, a.d, "p")
	if err != nil || p.Bloqueado() {
		t.Fatal(err, p.Bloqueios)
	}
	pj, _ := json.Marshal(p)
	id, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando,
		Destino: "homolog", Banco: "grande", Plano: string(pj)})
	go func() {
		for i := 0; i < 600; i++ {
			e, _ := a.cad.Execucao(context.Background(), id)
			if e.Etapa == "Dump" {
				time.Sleep(700 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	_ = Executar(ctx, a.d, id)
	e, _ := a.cad.Execucao(context.Background(), id)
	if e.Estado != cadastro.EstadoCancelada {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	inst, _ := a.cad.Instancia(context.Background())
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "label="+imagens.Rotulo+"="+strconv.FormatInt(id, 10), "--filter", "label="+imagens.RotuloInstancia+"="+inst, "--format", "{{.Names}}").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("container ficou: %s", out)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname LIKE 'grande\_\_%'`); n != 0 {
		t.Fatal("o destino foi tocado")
	}
	if n := valor[int64](t, d, "grande", `SELECT count(*) FROM velha`); n != 1 {
		t.Fatal("o banco de destino mudou")
	}
	b, _ := os.ReadFile(filepath.Join(e.DumpDir, "manifesto.json"))
	if !strings.Contains(string(b), `"estado": "incompleto"`) {
		t.Fatalf("o dump cancelado fica marcado incompleto: %s", b)
	}
	if ds := ListarDumps(a.d.Dir, nil); len(ds) != 1 || ds[0].Manifesto.Estado != DumpIncompleto {
		t.Fatalf("%+v", ds)
	}
}

// A produção só por SSH: a cópia inteira passa pelo túnel (servidor SSH de teste em 127.0.0.1).
func TestCopiaPeloTunelSSH(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	if err := tunel.GerarChave(a.d.Dir.ChaveSSH(), "pghangar@teste"); err != nil {
		t.Fatal(err)
	}
	pub, _ := tunel.ChavePublica(a.d.Dir.ChaveSSH())
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	srv, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Parar()
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", o.porta) // como o permitopen do authorized_keys

	c := cadastro.Conexao{Nome: "prod", Tag: cadastro.TagProd, Acesso: cadastro.AcessoSSH, SSHHost: srv.Host, SSHPorta: srv.Porta,
		SSHUsuario: "pghangar", Host: "127.0.0.1", Porta: o.porta, Usuario: "postgres", ModoSenha: cadastro.SenhaPerguntar,
		SSLMode: "prefer", BancoAdmin: "postgres"}
	if err := a.cad.SalvarConexao(ctx, "", c); err != nil {
		t.Fatal(err)
	}
	a.conexao(t, "dev", cadastro.TagDev, d)
	semear(t, o, "loja_tunel", `CREATE TABLE public.volume AS SELECT g, md5(g::text) m FROM generate_series(1, 2500000) g`)
	a.perfil(t, "p", "prod", "loja_tunel", "dev", "loja_tunel", func(p *cadastro.Perfil) { p.JobsDump = 1 })

	// 1. A senha no modo perguntar.
	_, err = Planejar(ctx, a.d, "p")
	var pg *Pergunta
	if !errors.As(err, &pg) || !pg.Senha {
		t.Fatalf("esperava pergunta de senha: %v", err)
	}
	a.d.Seg.GuardarSenha("prod", senhaTeste)
	// 2. O servidor SSH desconhecido.
	_, err = Planejar(ctx, a.d, "p")
	if !errors.As(err, &pg) || pg.HostDesconhecido == nil {
		t.Fatalf("esperava host desconhecido: %v", err)
	}
	if err := tunel.Aceitar(a.d.Amb.KnownHosts, pg.HostDesconhecido); err != nil {
		t.Fatal(err)
	}
	// 3. O diagnóstico em camadas pelo túnel.
	cx, _ := a.cad.Conexao(ctx, "prod")
	dg := conexao.Diagnosticar(ctx, cx, a.d.Amb, a.d.Seg)
	if dg.Parou() != "" {
		t.Fatalf("diagnóstico: %+v", dg)
	}
	var camadas []string
	for _, c := range dg.Camadas {
		camadas = append(camadas, c.Nome)
	}
	if strings.Join(camadas, ",") != "DNS,TCP,SSH,Túnel,Postgres,Autenticação,Pronto" {
		t.Fatalf("camadas: %v", camadas)
	}

	// 4a. O túnel cai no meio do dump e o servidor SSH volta: o dump recomeça sozinho e a cópia
	// termina. A tentativa que caiu fica no disco.
	a.d.EsperasRede = []time.Duration{time.Second, time.Second}
	{
		p, err := Planejar(ctx, a.d, "p")
		if err != nil || p.Bloqueado() {
			t.Fatal(err, p.Bloqueios)
		}
		pj, _ := json.Marshal(p)
		id0, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando,
			Destino: "dev", Banco: "loja_tunel", Plano: string(pj)})
		go func() {
			for i := 0; i < 600; i++ {
				e, _ := a.cad.Execucao(context.Background(), id0)
				if e.Etapa == "Dump" {
					time.Sleep(800 * time.Millisecond)
					srv.Derrubar()
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		_ = Executar(ctx, a.d, id0)
		e, _ := a.cad.Execucao(ctx, id0)
		if e.Estado != cadastro.EstadoOK || !strings.Contains(strings.Join(e.Avisos, " "), "recomeçou") {
			t.Fatalf("o dump deveria ter recomeçado sozinho: %s %s %v\n%s", e.Estado, e.Mensagem, e.Avisos, a.log.String())
		}
		if _, err := os.Stat(filepath.Join(e.DumpDir, "dump.incompleto-1")); err != nil {
			t.Fatal("a tentativa que caiu deveria ficar no disco")
		}
		time.Sleep(1100 * time.Millisecond)
		// Deixa o destino como estava para o resto do teste.
		sql(t, d, "postgres", "DROP DATABASE loja_tunel WITH (FORCE)")
	}

	// 4b. O servidor SSH cai de vez no meio do dump: as tentativas se esgotam, o dump falha, e o
	// destino não é tocado.
	p, err := Planejar(ctx, a.d, "p")
	if err != nil || p.Bloqueado() {
		t.Fatal(err, p.Bloqueios)
	}
	pj, _ := json.Marshal(p)
	id1, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando,
		Destino: "dev", Banco: "loja_tunel", Plano: string(pj)})
	go func() {
		for i := 0; i < 600; i++ {
			e, _ := a.cad.Execucao(context.Background(), id1)
			if e.Etapa == "Dump" {
				time.Sleep(800 * time.Millisecond)
				srv.Parar()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	_ = Executar(ctx, a.d, id1)
	e, _ := a.cad.Execucao(ctx, id1)
	if e.Estado != cadastro.EstadoErro || e.Etapa != "Dump" {
		t.Fatalf("a queda do túnel no dump deveria falhar a cópia: %s/%s %s", e.Estado, e.Etapa, e.Mensagem)
	}
	if n := valor[int64](t, d, "postgres", `SELECT count(*) FROM pg_database WHERE datname LIKE 'loja\_tunel%'`); n != 0 {
		t.Fatal("o destino foi tocado")
	}

	// 5. A cópia inteira pelo túnel (um servidor SSH novo, na mesma chave de host não: ele é outro,
	// então o known_hosts precisa aceitar a porta nova).
	srv2, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Parar()
	srv2.Permitido = srv.Permitido
	cx2, _ := a.cad.Conexao(ctx, "prod")
	cx2.SSHPorta = srv2.Porta
	if err := a.cad.SalvarConexao(ctx, "prod", cx2); err != nil {
		t.Fatal(err)
	}
	if _, err := Planejar(ctx, a.d, "p"); errors.As(err, &pg) && pg.HostDesconhecido != nil {
		if err := tunel.Aceitar(a.d.Amb.KnownHosts, pg.HostDesconhecido); err != nil {
			t.Fatal(err)
		}
	}
	e = a.copiar(t, ctx, "p")
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "loja_tunel", `SELECT count(*) FROM public.volume`); n != 2500000 {
		t.Fatal(n)
	}
	// O túnel escuta num socket unix (diretório 700), e não numa porta de 127.0.0.1 que qualquer
	// usuário local alcançaria.
	if !strings.Contains(a.log.String(), "/tunel-") || strings.Contains(a.log.String(), fmt.Sprintf("port=%d user=postgres dbname=loja_tunel ", o.porta)) {
		t.Fatal("o dump deveria ter ido pelo socket do túnel, e não direto")
	}
	if strings.Contains(a.log.String(), senhaTeste) {
		t.Fatal("a senha apareceu no log")
	}

	// 6. O permitopen: um destino fora dele é recusado na camada do túnel.
	cx, _ = a.cad.Conexao(ctx, "prod")
	cx.Porta = d.porta
	dg = conexao.Diagnosticar(ctx, cx, a.d.Amb, a.d.Seg)
	if dg.Parou() != "Túnel" {
		t.Fatalf("fora do permitopen deveria parar no túnel: %+v", dg)
	}
}

// As melhorias da etapa 1: compressão, filtro de schemas (as extensões vêm junto), tabelas fora,
// contagem de linhas no snapshot do dump e o aviso das roles que faltam.
func TestFiltrosCompressaoELinhas(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o17", 17), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "dev", cadastro.TagDev, d)
	semear(t, o, "loja6")
	a.perfil(t, "so-vendas", "prod", "loja6", "dev", "loja6_vendas", func(p *cadastro.Perfil) {
		p.Compressao, p.Schemas, p.ConferirLinhas, p.SemDados = "lz4", []string{"vendas"}, true, []string{"vendas.pedido"}
	})
	a.perfil(t, "sem-log", "prod", "loja6", "dev", "loja6_sem_log", func(p *cadastro.Perfil) {
		p.TabelasFora, p.ConferirLinhas, p.SemDados = []string{"public.log_eventos"}, true, nil
	})

	e := a.copiar(t, ctx, "so-vendas")
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if !strings.Contains(a.log.String(), "--compress=lz4") || !strings.Contains(a.log.String(), "--schema=vendas") || !strings.Contains(a.log.String(), "--extension=*") {
		t.Fatal("as opções do perfil não foram para o pg_dump")
	}
	if !strings.Contains(a.log.String(), "as linhas de") {
		t.Fatal("a contagem de linhas deveria ter rodado")
	}
	if n := valor[int64](t, d, "loja6_vendas", `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`); n != 0 {
		t.Fatalf("o schema public não deveria vir: %d tabelas", n)
	}
	if n := valor[int64](t, d, "loja6_vendas", `SELECT count(*) FROM vendas.cliente`); n != 2000 {
		t.Fatal(n)
	}
	if n := valor[int64](t, d, "loja6_vendas", `SELECT count(*) FROM vendas.pedido`); n != 0 {
		t.Fatal("pedido vem sem dados", n)
	}
	b, _ := os.ReadFile(filepath.Join(e.DumpDir, "manifesto.json"))
	if !strings.Contains(string(b), `"linhas"`) || !strings.Contains(string(b), `"filtrado": true`) {
		t.Fatalf("o manifesto guarda as linhas e o filtro: %s", b)
	}

	e = a.copiar(t, ctx, "sem-log")
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "loja6_sem_log", `SELECT count(*) FROM pg_tables WHERE tablename = 'log_eventos'`); n != 0 {
		t.Fatal("log_eventos ficou de fora inteira")
	}

	// Linhas que mudam no destino depois do restore (um script que apaga) aparecem na conferência:
	// a execução para, esperando a decisão.
	dir := t.TempDir()
	script := filepath.Join(dir, "apaga.sql")
	_ = os.WriteFile(script, []byte("DELETE FROM vendas.pedido WHERE cliente > 1990;\nDELETE FROM vendas.cliente WHERE id > 1990;"), 0o600)
	a.perfil(t, "script-apaga", "prod", "loja6", "dev", "loja6_apaga", func(p *cadastro.Perfil) {
		p.ConferirLinhas, p.Script = true, script
	})
	e = a.copiar(t, ctx, "script-apaga")
	if e.Estado != cadastro.EstadoOK {
		// A conferência roda ANTES do script: o que o script apaga de propósito não diverge.
		t.Fatalf("o script roda depois da conferência: %s %s", e.Estado, e.Mensagem)
	}

	// As roles que a RLS cita e o destino não tem: aviso no plano.
	sql(t, o, "postgres", `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auditor_prod') THEN CREATE ROLE auditor_prod; END IF; END $$`)
	sql(t, o, "loja6", `CREATE POLICY aud ON public.segredo TO auditor_prod USING (true)`)
	p, err := Planejar(ctx, a.d, "sem-log")
	if err != nil || !strings.Contains(strings.Join(p.Avisos, " "), "auditor_prod") {
		t.Fatalf("aviso das roles: %v %v", p.Avisos, err)
	}
}

func TestScriptPrecisaSerDoRoot(t *testing.T) {
	dir := t.TempDir()
	s := filepath.Join(dir, "x.sql")
	_ = os.WriteFile(s, []byte("SELECT 1;"), 0o666)
	_ = os.Chmod(s, 0o666)
	if err := ConferirScript(s); err == nil {
		t.Fatal("script gravável por outros deveria ser recusado")
	}
	_ = os.Chmod(s, 0o600)
	if err := ConferirScript(s); err != nil {
		t.Fatal(err)
	}
}

// Restaurar de novo um dump guardado: sem ir à origem, com o plano e a conferência do manifesto.
func TestRestaurarDumpGuardado(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o16", 16), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "loja7")
	destinoHomolog(t, d, "loja7")
	a.perfil(t, "p", "prod", "loja7", "homolog", "loja7", func(p *cadastro.Perfil) { p.ConferirLinhas = true })
	e1 := a.copiar(t, ctx, "p")
	conferirCopia(t, a, e1, o, d, "loja7", "loja7")

	// O banco de homolog muda (alguém mexeu); o dump de antes volta a valer.
	sql(t, d, "loja7", `DELETE FROM vendas.pedido WHERE id > 100`)
	// A origem fica inalcançável: a restauração não pode precisar dela.
	cx, _ := a.cad.Conexao(ctx, "prod")
	cx.Porta = 1
	if err := a.cad.SalvarConexao(ctx, "prod", cx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	p, err := PlanejarRestauracao(ctx, a.d, e1.DumpDir)
	if err != nil || p.Bloqueado() {
		t.Fatalf("%v %v", err, p.Bloqueios)
	}
	if p.Imagem != 18 || p.DumpGuardado != e1.DumpDir || len(p.Anteriores) != 1 {
		t.Fatalf("%+v", p)
	}
	pj, _ := json.Marshal(p)
	id, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoRestauracao, Estado: cadastro.EstadoIniciando,
		Destino: "homolog", Banco: "loja7", Plano: string(pj), DumpDir: e1.DumpDir})
	_ = Executar(ctx, a.d, id)
	e, _ := a.cad.Execucao(ctx, id)
	if e.Estado != cadastro.EstadoOK || !strings.Contains(e.Mensagem, "restaurado do dump") {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "loja7", `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatalf("o dump de antes deveria ter voltado: %d", n)
	}
	if strings.Count(a.log.String(), "pg_dump --format") != 1 {
		t.Fatal("a restauração não deveria rodar o pg_dump")
	}
	if !strings.Contains(a.log.String(), "as linhas de") {
		t.Fatal("a conferência das linhas vem do manifesto")
	}
	// Um dump incompleto não é restaurado.
	m, _ := LerManifesto(e1.DumpDir)
	m.Estado = DumpIncompleto
	_ = EscreverManifesto(e1.DumpDir, m)
	if _, err := PlanejarRestauracao(ctx, a.d, e1.DumpDir); err == nil {
		t.Fatal("dump incompleto não se restaura")
	}
}

// A base e o reset: a cópia guarda <banco>__base; depois, o destino volta ao estado da base sem ir
// à origem (nem ao Docker), com as configurações do banco de agora e desfazer.
func TestBaseEReset(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subir(t, "o18", 18), subir(t, "d18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	a.conexao(t, "homolog", cadastro.TagHomolog, d)
	semear(t, o, "loja8")
	destinoHomolog(t, d, "loja8")
	a.perfil(t, "p", "prod", "loja8", "homolog", "loja8", func(p *cadastro.Perfil) { p.GuardarBase = true })

	// Sem base ainda, o reset é bloqueado.
	if p, _ := PlanejarReset(ctx, a.d, "p"); !strings.Contains(strings.Join(p.Bloqueios, " "), "guardar base") {
		t.Fatalf("reset sem base: %v", p.Bloqueios)
	}
	e := a.copiar(t, ctx, "p")
	conferirCopia(t, a, e, o, d, "loja8", "loja8")
	if !valor[bool](t, d, "postgres", `SELECT NOT datallowconn FROM pg_database WHERE datname = 'loja8__base'`) {
		t.Fatal("a base fica fechada para conexões")
	}
	if c := valor[string](t, d, "postgres", `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = 'loja8__base'`); !strings.Contains(c, "cópia #") {
		t.Fatalf("comentário da base: %s", c)
	}

	// O homolog muda; o reset o traz de volta ao estado da base.
	sql(t, d, "loja8", `DELETE FROM vendas.pedido`, `CREATE TABLE lixo (x int)`)
	time.Sleep(1100 * time.Millisecond)
	// A origem nem precisa responder.
	cx, _ := a.cad.Conexao(ctx, "prod")
	cx.Porta = 1
	_ = a.cad.SalvarConexao(ctx, "prod", cx)
	p, err := PlanejarReset(ctx, a.d, "p")
	if err != nil || p.Bloqueado() || p.Base == nil || p.Confirmacao() != "loja8" {
		t.Fatalf("%v %v %+v", err, p.Bloqueios, p.Base)
	}
	pj, _ := json.Marshal(p)
	idReset, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoReset, Estado: cadastro.EstadoIniciando,
		Destino: "homolog", Banco: "loja8", Plano: string(pj)})
	_ = Executar(ctx, a.d, idReset)
	er, _ := a.cad.Execucao(ctx, idReset)
	if er.Estado != cadastro.EstadoOK || !strings.Contains(er.Mensagem, "resetado") || er.BancoAnterior == "" {
		t.Fatalf("%s %s\n%s", er.Estado, er.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "loja8", `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatalf("o reset deveria trazer os pedidos de volta: %d", n)
	}
	if n := valor[int64](t, d, "loja8", `SELECT count(*) FROM pg_tables WHERE tablename = 'lixo'`); n != 0 {
		t.Fatal("o reset deveria tirar a tabela lixo")
	}
	// As configurações do homolog continuam (o reset as reaplica), e o dono é o do homolog.
	cfg := valor[string](t, d, "postgres", `SELECT string_agg(array_to_string(setconfig, ','), ';') FROM pg_db_role_setting
		WHERE setdatabase = (SELECT oid FROM pg_database WHERE datname = 'loja8')`)
	if !strings.Contains(cfg, "search_path") {
		t.Fatalf("configurações depois do reset: %s", cfg)
	}
	if dono := valor[string](t, d, "postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'loja8'`); dono != "app_homolog" {
		t.Fatalf("dono depois do reset: %s", dono)
	}
	// O que estava antes do reset ficou como anterior (desfazer não perde nada).
	sql(t, d, "postgres", "ALTER DATABASE "+id(er.BancoAnterior)+" ALLOW_CONNECTIONS true")
	if n := valor[int64](t, d, er.BancoAnterior, `SELECT count(*) FROM pg_tables WHERE tablename = 'lixo'`); n != 1 {
		t.Fatal("o anterior do reset guarda o estado de antes")
	}
	// A base aparece em Listar e pode ser apagada.
	l, err := Listar(ctx, a.d, "homolog", "loja8")
	if err != nil || l.Base == nil {
		t.Fatalf("%+v %v", l, err)
	}
	if err := Apagar(ctx, a.d, "homolog", "loja8", "loja8__base"); err != nil {
		t.Fatal(err)
	}
}

// O modo link instável: dados em blocos pela chave, pelo túnel. A queda do túnel retoma de onde
// parou, e uma cópia cancelada é retomada pela seguinte.
func TestLinkInstavel(t *testing.T) {
	antes := linhasPorBloco
	linhasPorBloco = 20000
	defer func() { linhasPorBloco = antes }()
	a := novoAmbiente(t)
	a.d.EsperasRede = []time.Duration{time.Second, time.Second, time.Second}
	ctx := context.Background()
	o, d := subir(t, "o17", 17), subir(t, "d18", 18)
	if err := tunel.GerarChave(a.d.Dir.ChaveSSH(), "t"); err != nil {
		t.Fatal(err)
	}
	pub, _ := tunel.ChavePublica(a.d.Dir.ChaveSSH())
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	srv, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Parar()
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", o.porta)
	if err := a.cad.SalvarConexao(ctx, "", cadastro.Conexao{Nome: "prod", Tag: cadastro.TagProd, Acesso: cadastro.AcessoSSH, SSHHost: srv.Host,
		SSHPorta: srv.Porta, SSHUsuario: "c", Host: "127.0.0.1", Porta: o.porta, Usuario: "postgres", ModoSenha: cadastro.SenhaGuardar,
		Senha: senhaTeste, SSLMode: "prefer", BancoAdmin: "postgres"}); err != nil {
		t.Fatal(err)
	}
	a.conexao(t, "dev", cadastro.TagDev, d)
	semear(t, o, "loja9",
		`CREATE TABLE public.volume (id bigint PRIMARY KEY, m text)`,
		`INSERT INTO public.volume SELECT g, md5(g::text) FROM generate_series(1, 1500000) g`,
		`CREATE TABLE public.sem_chave AS SELECT g, md5(g::text) m FROM generate_series(1, 30000) g`,
		`CREATE TABLE public.chave_composta (a int NOT NULL, b text NOT NULL, v int, PRIMARY KEY (a, b))`,
		`INSERT INTO public.chave_composta SELECT g % 100, md5(g::text), g FROM generate_series(1, 45000) g`,
		`SELECT setval('vendas.pedido_id_seq', 987654)`)
	a.perfil(t, "p", "prod", "loja9", "dev", "loja9", func(p *cadastro.Perfil) {
		p.Retomavel, p.JobsDump, p.JobsRestore, p.SemDados = true, 2, 2, []string{"public.log_eventos"}
	})
	aceitar := func() {
		_, err := Planejar(ctx, a.d, "p")
		var pg *Pergunta
		if errors.As(err, &pg) && pg.HostDesconhecido != nil {
			if err := tunel.Aceitar(a.d.Amb.KnownHosts, pg.HostDesconhecido); err != nil {
				t.Fatal(err)
			}
		}
	}
	aceitar()

	// 1. Cancelada no meio do dump: o dump em blocos fica incompleto, com o que já veio.
	p, err := Planejar(ctx, a.d, "p")
	if err != nil || p.Bloqueado() {
		t.Fatal(err, p.Bloqueios)
	}
	cctx, cancel := context.WithCancel(ctx)
	pj, _ := json.Marshal(p)
	id1, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando, Destino: "dev", Banco: "loja9", Plano: string(pj)})
	go func() {
		for i := 0; i < 1200; i++ {
			e, _ := a.cad.Execucao(context.Background(), id1)
			if e.Etapa == "Dump" && strings.Contains(e.Item, "public.volume: bloco 3") {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	_ = Executar(cctx, a.d, id1)
	e1, _ := a.cad.Execucao(ctx, id1)
	if e1.Estado != cadastro.EstadoCancelada {
		t.Fatalf("%s %s", e1.Estado, e1.Mensagem)
	}

	// 2. A próxima cópia retoma o dump incompleto; e o túnel cai no meio, uma vez: retoma de novo.
	time.Sleep(1100 * time.Millisecond)
	p, err = Planejar(ctx, a.d, "p")
	if err != nil || p.Retomar != e1.DumpDir {
		t.Fatalf("a cópia seguinte deveria retomar %s: %q %v", e1.DumpDir, p.Retomar, err)
	}
	pj, _ = json.Marshal(p)
	id2, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando, Destino: "dev", Banco: "loja9", Plano: string(pj)})
	go func() {
		for i := 0; i < 1200; i++ {
			e, _ := a.cad.Execucao(context.Background(), id2)
			if e.Etapa == "Dump" && strings.Contains(e.Item, "bloco") {
				time.Sleep(300 * time.Millisecond)
				srv.Derrubar()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	_ = Executar(ctx, a.d, id2)
	e2, _ := a.cad.Execucao(ctx, id2)
	if e2.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e2.Estado, e2.Mensagem, a.log.String())
	}
	if !strings.Contains(a.log.String(), "retomando o dump incompleto") {
		t.Fatal("deveria ter retomado o dump da cópia cancelada")
	}
	if !strings.Contains(strings.Join(e2.Avisos, " "), "retomou de onde parou") {
		t.Fatalf("a queda do túnel deveria ter sido retomada: %v", e2.Avisos)
	}
	if e2.DumpDir != e1.DumpDir {
		t.Fatal("o dump retomado é o mesmo diretório")
	}

	// 3. Os dados, as sequências e a visão materializada, iguais à origem.
	for _, q := range []string{`SELECT count(*) FROM public.volume`, `SELECT count(*) FROM public.sem_chave`,
		`SELECT count(*) FROM public.chave_composta`, `SELECT count(*) FROM vendas.pedido`, `SELECT count(*) FROM vendas.top`,
		`SELECT sum(id) FROM public.volume`, `SELECT last_value FROM vendas.pedido_id_seq`} {
		if vo, vd := valor[int64](t, o, "loja9", q), valor[int64](t, d, "loja9", q); vo != vd {
			t.Errorf("%s: origem %d, destino %d", q, vo, vd)
		}
	}
	if n := valor[int64](t, d, "loja9", `SELECT count(*) FROM public.log_eventos`); n != 0 {
		t.Errorf("log_eventos vem sem dados: %d", n)
	}
	if n := valor[int64](t, d, "loja9", `SELECT count(*) FROM pg_indexes WHERE tablename = 'volume'`); n != 1 {
		t.Error("o índice da chave (post-data) deveria existir")
	}
	m, _ := LerManifesto(e2.DumpDir)
	if m.Formato != FormatoBlocos || m.Estado != DumpCompleto {
		t.Fatalf("%+v", m)
	}

	// 4. O dump em blocos guardado também se restaura de novo.
	time.Sleep(1100 * time.Millisecond)
	pr, err := PlanejarRestauracao(ctx, a.d, e2.DumpDir)
	if err != nil || pr.Bloqueado() {
		t.Fatal(err, pr.Bloqueios)
	}
	pj, _ = json.Marshal(pr)
	id3, _ := a.cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoRestauracao, Estado: cadastro.EstadoIniciando,
		Destino: "dev", Banco: "loja9", Plano: string(pj), DumpDir: e2.DumpDir})
	_ = Executar(ctx, a.d, id3)
	if e3, _ := a.cad.Execucao(ctx, id3); e3.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s", e3.Estado, e3.Mensagem)
	}
}
