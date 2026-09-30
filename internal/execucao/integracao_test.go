//go:build integracao

// Ponta a ponta com o binário de verdade: o processo separado da execução, a senha que não vaza, o
// cancelamento, o processo morto e a limpeza. Só localhost: containers presos em 127.0.0.1 e o
// servidor SSH de teste dentro deste processo.
//
//	go test -tags integracao ./internal/execucao/ -v -timeout 30m
package execucao

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/testessh"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

const senha = "e2e-S3nh@-única-9f2c"

func subir(t *testing.T, nome string, versao int) int {
	t.Helper()
	// -v: a imagem do postgres declara VOLUME; um rm -f explícito passa na frente do --rm e o volume anônimo (GBs) fica órfão.
	_ = exec.Command("docker", "rm", "-fv", nome).Run()
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", nome, "-e", "POSTGRES_PASSWORD="+senha,
		"-p", "127.0.0.1::5432", fmt.Sprintf("postgres:%d", versao)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-fv", nome).Run() })
	out, _ = exec.Command("docker", "port", nome, "5432/tcp").Output()
	l := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	if !strings.HasPrefix(l, "127.0.0.1:") {
		t.Fatalf("o container de teste precisa estar preso em 127.0.0.1: %s", l)
	}
	porta, _ := strconv.Atoi(l[strings.LastIndex(l, ":")+1:])
	for i := 0; i < 180; i++ {
		if c, err := conectar(porta, "postgres"); err == nil {
			_ = c.Close(context.Background())
			time.Sleep(2 * time.Second)
			return porta
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("não subiu")
	return 0
}

func conectar(porta int, banco string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname=%s sslmode=disable", porta, banco))
	if err != nil {
		return nil, err
	}
	cfg.Password = senha
	return pgx.ConnectConfig(context.Background(), cfg)
}

func sql(t *testing.T, porta int, banco string, cmds ...string) {
	t.Helper()
	c, err := conectar(porta, banco)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	for _, s := range cmds {
		if _, err := c.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func contar(t *testing.T, porta int, banco, q string) int64 {
	t.Helper()
	c, err := conectar(porta, banco)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int64
	if err := c.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func esperar(t *testing.T, cad *cadastro.Cadastro, id int64, cond func(cadastro.Execucao) bool, prazo time.Duration) cadastro.Execucao {
	t.Helper()
	fim := time.Now().Add(prazo)
	for {
		e, err := cad.Execucao(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if cond(e) {
			return e
		}
		if time.Now().After(fim) {
			t.Fatalf("tempo esgotado esperando a execução %d: %+v", id, e)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestPontaAPontaComOBinario(t *testing.T) {
	ctx := context.Background()
	raiz, _ := filepath.Abs("../..")
	bin := filepath.Join(t.TempDir(), "pghangar")
	if out, err := exec.Command("go", "build", "-o", bin, filepath.Join(raiz, "cmd", "pghangar")).CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}

	origem := subir(t, "pghangar-e2e-o16", 16)
	destino := subir(t, "pghangar-e2e-d18", 18)
	sql(t, origem, "postgres", "CREATE DATABASE loja")
	sql(t, origem, "loja", "CREATE SCHEMA vendas",
		"CREATE TABLE vendas.pedido (id bigserial PRIMARY KEY, valor numeric)",
		"INSERT INTO vendas.pedido (valor) SELECT g % 500 FROM generate_series(1, 100000) g",
		"CREATE TABLE public.volume AS SELECT g, md5(g::text) m FROM generate_series(1, 3000000) g")
	sql(t, destino, "postgres", "CREATE ROLE app LOGIN", "CREATE DATABASE loja OWNER app")

	dir := local.Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := dir.Preparar(); err != nil {
		t.Fatal(err)
	}
	cad, err := cadastro.Abrir(dir.Estado())
	if err != nil {
		t.Fatal(err)
	}
	defer cad.Fechar()
	dk := imagens.Docker{}
	for _, v := range []int{16, 18} {
		ref := imagens.Referencia("postgres", v)
		dg, err := dk.Digest(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		cl, _ := dk.VersaoCliente(ctx, dg)
		_ = cad.SalvarImagem(ctx, cadastro.Imagem{Versao: v, Referencia: ref, Digest: dg, Cliente: cl, BaixadaEm: time.Now()})
	}
	if err := tunel.GerarChave(dir.ChaveSSH(), "e2e"); err != nil {
		t.Fatal(err)
	}
	pub, _ := tunel.ChavePublica(dir.ChaveSSH())
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	srv, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Parar()
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", origem)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(cad.SalvarConexao(ctx, "", cadastro.Conexao{Nome: "prod", Tag: cadastro.TagProd, Acesso: cadastro.AcessoSSH, SSHHost: srv.Host,
		SSHPorta: srv.Porta, SSHUsuario: "pghangar", Host: "127.0.0.1", Porta: origem, Usuario: "postgres",
		ModoSenha: cadastro.SenhaPerguntar, SSLMode: "prefer", BancoAdmin: "postgres"}))
	must(cad.SalvarConexao(ctx, "", cadastro.Conexao{Nome: "dev", Tag: cadastro.TagDev, Acesso: cadastro.AcessoDireto, Host: "127.0.0.1",
		Porta: destino, Usuario: "postgres", ModoSenha: cadastro.SenhaPerguntar, SSLMode: "disable", BancoAdmin: "postgres"}))
	must(cad.SalvarPerfil(ctx, "", cadastro.Perfil{Nome: "p", Origem: "prod", OrigemBanco: "loja", Destino: "dev", DestinoBanco: "loja", JobsDump: 1, JobsRestore: 2}))
	var sidDestino string
	{
		c, err := conectar(destino, "postgres")
		must(err)
		must(c.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&sidDestino))
		_ = c.Close(ctx)
	}
	must(cad.AprovarDestino(ctx, sidDestino, "dev", "teste"))

	var seg conexao.Segredos
	seg.GuardarSenha("prod", senha)
	seg.GuardarSenha("dev", senha)
	d := motor.Deps{Cadastro: cad, Dir: dir, Docker: dk, Amb: conexao.Ambiente{KnownHosts: dir.KnownHosts(), ChavePadrao: dir.ChaveSSH(), DirTemp: dir.Temp()}, Seg: seg, Maquina: "e2e"}
	planejar := func() motor.Plano {
		t.Helper()
		p, err := motor.Planejar(ctx, d, "p")
		var pg *motor.Pergunta
		if errors.As(err, &pg) && pg.HostDesconhecido != nil {
			must(tunel.Aceitar(dir.KnownHosts(), pg.HostDesconhecido))
			p, err = motor.Planejar(ctx, d, "p")
		}
		must(err)
		return p
	}
	l := Lancador{Cadastro: cad, Dir: dir, Binario: bin}

	// 1. A cópia pelo processo separado. A senha não aparece em argv, no ambiente nem no Docker.
	p := planejar()
	if p.Bloqueado() {
		t.Fatal(p.Bloqueios)
	}
	id, err := l.Iniciar(ctx, Pedido{Plano: p, Segredos: seg})
	must(err)
	e := esperar(t, cad, id, func(e cadastro.Execucao) bool { return e.PID > 0 }, 30*time.Second)
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", e.PID))
	environ, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", e.PID))
	if strings.Contains(string(cmdline), senha) || strings.Contains(string(environ), senha) {
		t.Fatal("a senha apareceu no argv ou no ambiente do processo")
	}
	if sid, _ := unix.Getsid(e.PID); sid != e.PID {
		t.Fatalf("o processo deveria ter sessão própria (setsid): sid %d, pid %d", sid, e.PID)
	}
	vistos := 0
	for i := 0; i < 200; i++ {
		out, _ := exec.Command("docker", "ps", "-q", "--filter", "label="+imagens.Rotulo+"="+strconv.FormatInt(id, 10), "--filter", "label="+imagens.RotuloInstancia+"="+instancia(t, cad)).Output()
		for _, c := range strings.Fields(string(out)) {
			insp, _ := exec.Command("docker", "inspect", c).Output()
			if len(insp) > 0 {
				vistos++
				if strings.Contains(string(insp), senha) {
					t.Fatal("a senha apareceu no docker inspect")
				}
			}
		}
		if cur, _ := cad.Execucao(ctx, id); cur.Terminou() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if vistos == 0 {
		t.Fatal("nenhum container inspecionado")
	}
	e = esperar(t, cad, id, func(e cadastro.Execucao) bool { return e.Terminou() }, 5*time.Minute)
	if e.Estado != cadastro.EstadoOK {
		b, _ := os.ReadFile(dir.Log(id))
		t.Fatalf("%s: %s\n%s", e.Estado, e.Mensagem, b)
	}
	if n := contar(t, destino, "loja", "SELECT count(*) FROM public.volume"); n != 3000000 {
		t.Fatal(n)
	}
	logb, _ := os.ReadFile(dir.Log(id))
	if strings.Contains(string(logb), senha) {
		t.Fatal("a senha apareceu no log")
	}
	if fi, _ := os.Stat(dir.Log(id)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("log com %o", fi.Mode().Perm())
	}

	// 2. Cancelar (SIGTERM) no dump: o destino não é tocado, e nada fica rodando.
	time.Sleep(1100 * time.Millisecond)
	p = planejar()
	id2, err := l.Iniciar(ctx, Pedido{Plano: p, Segredos: seg})
	must(err)
	e = esperar(t, cad, id2, func(e cadastro.Execucao) bool { return e.Etapa == "Dump" && e.PID > 0 }, time.Minute)
	time.Sleep(500 * time.Millisecond)
	must(Cancelar(e))
	e = esperar(t, cad, id2, func(e cadastro.Execucao) bool { return e.Terminou() }, time.Minute)
	if e.Estado != cadastro.EstadoCancelada {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	// O --rm remove o container logo depois de ele parar (o Docker faz isso depois do fim).
	for i := 0; ; i++ {
		out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}} {{.Status}}", "--filter", "label="+imagens.Rotulo+"="+strconv.FormatInt(id2, 10), "--filter", "label="+imagens.RotuloInstancia+"="+instancia(t, cad)).Output()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if i == 50 {
			b, _ := os.ReadFile(dir.Log(id2))
			t.Fatalf("container ficou depois do cancelamento: %s\n%s", out, b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := contar(t, destino, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'loja__novo'`); n != 0 {
		t.Fatal("o cancelamento no dump não pode deixar __novo")
	}
	// Cancelar de novo, ou uma execução que já terminou, é recusado.
	if err := Cancelar(e); err == nil {
		t.Fatal("cancelar uma execução terminada")
	}

	// 3. Processo morto (kill -9) no restore: a execução vira "interrompida", o __novo bloqueia a
	// próxima cópia, e apagá-lo leva junto a role temporária que sobrou.
	p = planejar()
	id3, err := l.Iniciar(ctx, Pedido{Plano: p, Segredos: seg})
	must(err)
	e = esperar(t, cad, id3, func(e cadastro.Execucao) bool { return e.Etapa == "Restore" }, 3*time.Minute)
	time.Sleep(700 * time.Millisecond)
	must(syscall.Kill(e.PID, syscall.SIGKILL))
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", e.PID)); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	must(Conferir(ctx, cad, dir))
	e, _ = cad.Execucao(ctx, id3)
	if e.Estado != cadastro.EstadoInterrompida || !strings.Contains(e.Mensagem, "loja__novo") {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	// Durante o restore, o __novo é do superusuário e PUBLIC não conecta nele: os objetos ainda são
	// da role temporária de superusuário.
	if n := contar(t, destino, "postgres", `SELECT count(*) FROM pg_database d, aclexplode(d.datacl) a
		WHERE d.datname = 'loja__novo' AND a.grantee = 0 AND a.privilege_type = 'CONNECT'`); n != 0 {
		t.Fatal("o __novo estava aberto a PUBLIC durante o restore")
	}
	if n := contar(t, destino, "postgres", `SELECT count(*) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
		WHERE d.datname = 'loja__novo' AND r.rolsuper`); n != 1 {
		t.Fatal("durante o restore, o dono do __novo é o superusuário da conexão")
	}
	inst, _ := cad.Instancia(ctx)
	role := fmt.Sprintf("pghangar_%s_%d", inst, id3)
	if n := contar(t, destino, "postgres", fmt.Sprintf(`SELECT count(*) FROM pg_roles WHERE rolname = '%s'`, role)); n != 1 {
		t.Fatalf("a role temporária deveria ter sobrado depois do kill -9 (é o caso que a limpeza cobre): %d", n)
	}
	p = planejar()
	if !p.NovoExiste || !p.Bloqueado() || !strings.Contains(strings.Join(p.Notas, " "), role) {
		t.Fatalf("o plano deveria bloquear pelo __novo e informar da role: %v %v", p.Bloqueios, p.Notas)
	}
	// O container órfão do restore: a aba Ambiente mostra; aqui, paramos como ela faria.
	out, _ := exec.Command("docker", "ps", "--format", "{{.Names}}", "--filter", "label="+imagens.Rotulo+"="+strconv.FormatInt(id3, 10), "--filter", "label="+imagens.RotuloInstancia+"="+instancia(t, cad)).Output()
	for _, n := range strings.Fields(string(out)) {
		must(dk.Parar(ctx, n))
	}
	must(motor.Apagar(ctx, d, "dev", "loja", "loja__novo"))
	if n := contar(t, destino, "postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'copia\_banco\_%'`); n != 0 {
		t.Fatal("a role temporária deveria sair junto com o __novo")
	}

	// 4. E a cópia seguinte funciona.
	time.Sleep(1100 * time.Millisecond)
	p = planejar()
	if p.Bloqueado() {
		t.Fatal(p.Bloqueios)
	}
	id4, err := l.Iniciar(ctx, Pedido{Plano: p, Segredos: seg})
	must(err)
	e = esperar(t, cad, id4, func(e cadastro.Execucao) bool { return e.Terminou() }, 5*time.Minute)
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	// 5. rodar, sem a tela (o cron): a senha "perguntar" não tem a quem perguntar.
	rodarCLI := func(args ...string) (string, int) {
		cmd := exec.Command(bin, append([]string{"rodar", "--dir", dir.Raiz}, args...)...)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode()
		}
		return string(out), 0
	}
	if out, cod := rodarCLI("p"); cod != 1 || !strings.Contains(out, "pede a senha a cada sessão") {
		t.Fatalf("rodar com senha perguntar: %d %s", cod, out)
	}
	for _, n := range []string{"prod", "dev"} {
		c, _ := cad.Conexao(ctx, n)
		c.ModoSenha, c.Senha = cadastro.SenhaGuardar, senha
		must(cad.SalvarConexao(ctx, n, c))
	}
	time.Sleep(1100 * time.Millisecond)
	if saidaCLI, cod := rodarCLI("p"); cod != 0 || !strings.Contains(saidaCLI, ": ok") {
		t.Fatalf("rodar: %d %s", cod, saidaCLI)
	}
	// Um destino homolog exige --confirmar com o nome do banco.
	c, _ := cad.Conexao(ctx, "dev")
	c.Tag = cadastro.TagHomolog
	must(cad.SalvarConexao(ctx, "dev", c))
	if out, cod := rodarCLI("p"); cod != 1 || !strings.Contains(out, "--confirmar loja") {
		t.Fatalf("homolog sem confirmar: %d %s", cod, out)
	}
	if out, cod := rodarCLI("--confirmar", "outro", "p"); cod != 1 {
		t.Fatalf("homolog com o nome errado: %d %s", cod, out)
	}
	time.Sleep(1100 * time.Millisecond)
	if out, cod := rodarCLI("--confirmar", "loja", "p"); cod != 0 {
		t.Fatalf("homolog confirmado: %d %s", cod, out)
	}

	// 6. Um grupo: duas cópias em fila, uma de cada vez, pelo processo do grupo.
	c, _ = cad.Conexao(ctx, "dev")
	c.Tag = cadastro.TagDev
	must(cad.SalvarConexao(ctx, "dev", c))
	must(cad.SalvarPerfil(ctx, "", cadastro.Perfil{Nome: "p2", Origem: "prod", OrigemBanco: "loja", Destino: "dev", DestinoBanco: "loja2", JobsDump: 1, JobsRestore: 2}))
	time.Sleep(1100 * time.Millisecond)
	p1, p2 := planejar(), func() motor.Plano {
		p, err := motor.Planejar(ctx, d, "p2")
		must(err)
		return p
	}()
	_, ids, err := l.IniciarGrupo(ctx, []motor.Plano{p1, p2}, seg)
	must(err)
	var fins []cadastro.Execucao
	for _, gid := range ids {
		fins = append(fins, esperar(t, cad, gid, func(e cadastro.Execucao) bool { return e.Terminou() }, 10*time.Minute))
	}
	for _, e := range fins {
		if e.Estado != cadastro.EstadoOK || e.Grupo == "" {
			t.Fatalf("grupo: #%d %s %s", e.ID, e.Estado, e.Mensagem)
		}
	}
	if fins[1].Inicio.Before(fins[0].Fim) {
		t.Fatal("as cópias do grupo deveriam rodar uma de cada vez")
	}
	if n := contar(t, destino, "loja2", "SELECT count(*) FROM public.volume"); n != 3000000 {
		t.Fatal(n)
	}
	// Dois perfis do grupo no mesmo banco de destino: recusado.
	if _, _, err := l.IniciarGrupo(ctx, []motor.Plano{p1, p1}, seg); err == nil {
		t.Fatal("dois perfis no mesmo destino num grupo deveriam ser recusados")
	}

	// 7. Cancelar o grupo: a cópia em andamento e a da fila param.
	time.Sleep(1100 * time.Millisecond)
	p1, p2 = planejar(), func() motor.Plano {
		p, err := motor.Planejar(ctx, d, "p2")
		must(err)
		return p
	}()
	_, ids, err = l.IniciarGrupo(ctx, []motor.Plano{p1, p2}, seg)
	must(err)
	primeira := esperar(t, cad, ids[0], func(e cadastro.Execucao) bool { return e.Etapa == "Dump" && e.PID > 0 }, time.Minute)
	if seg2, _ := cad.Execucao(ctx, ids[1]); seg2.Estado != cadastro.EstadoFila || seg2.PID != primeira.PID {
		t.Fatalf("a segunda espera na fila, do mesmo processo: %s pid %d/%d", seg2.Estado, seg2.PID, primeira.PID)
	}
	must(Conferir(ctx, cad, dir)) // a da fila não pode virar "interrompida" enquanto o grupo vive
	if seg2, _ := cad.Execucao(ctx, ids[1]); seg2.Estado != cadastro.EstadoFila {
		t.Fatalf("a da fila virou %s", seg2.Estado)
	}
	must(Cancelar(primeira))
	for _, gid := range ids {
		e := esperar(t, cad, gid, func(e cadastro.Execucao) bool { return e.Terminou() }, time.Minute)
		if e.Estado != cadastro.EstadoCancelada {
			t.Fatalf("#%d depois de cancelar o grupo: %s %s", e.ID, e.Estado, e.Mensagem)
		}
	}

	if ms, _ := filepath.Glob(filepath.Join(dir.Temp(), "*")); len(ms) != 0 {
		for _, id := range []int64{id2, id3, id4} {
			b, _ := os.ReadFile(dir.Log(id))
			t.Logf("log da execução %d:\n%s", id, b)
		}
		t.Fatalf("sobrou arquivo temporário: %v", ms)
	}
}

// instancia é a desta instalação: os filtros de container levam ela, porque outra suíte rodando ao
// mesmo tempo (outra instalação) também tem a sua execução 1, 2, 3.
func instancia(t *testing.T, cad *cadastro.Cadastro) string {
	t.Helper()
	i, err := cad.Instancia(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return i
}
