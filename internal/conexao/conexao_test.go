package conexao

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

func conexaoTeste() cadastro.Conexao {
	return cadastro.Conexao{Nome: "dev", Tag: cadastro.TagDev, Acesso: cadastro.AcessoDireto, Host: "127.0.0.1", Porta: 5432,
		Usuario: "postgres", ModoSenha: cadastro.SenhaGuardar, Senha: "x", SSLMode: "prefer", BancoAdmin: "postgres"}
}

func TestDSNSemSenhaEComAspas(t *testing.T) {
	c := conexaoTeste()
	c.Usuario = "o'brien"
	d := DSN(c, &Ponte{Host: "127.0.0.1", Porta: 40001}, "minha loja", "pghangar/srv/p 1")
	if strings.Contains(d, "password") {
		t.Fatal("o DSN nunca leva senha")
	}
	for _, esperado := range []string{`host=127.0.0.1`, `port=40001`, `user='o\'brien'`, `dbname='minha loja'`, `application_name='pghangar/srv/p 1'`} {
		if !strings.Contains(d, esperado) {
			t.Errorf("faltou %s em %s", esperado, d)
		}
	}
	if valorDSN("") != "''" || valorDSN(`a\b`) != `'a\\b'` || valorDSN("a=b") != "'a=b'" {
		t.Fatal("valorDSN")
	}
}

// Pelo túnel, o TLS é do túnel: o cliente fala em claro com o socket, e a DSN diz isso, em vez de
// um require que o libpq ignoraria no socket.
func TestDSNPeloTunelSemTLSNoCliente(t *testing.T) {
	c := conexaoTeste()
	c.Acesso, c.SSLMode = cadastro.AcessoSSH, "require"
	if d := DSN(c, &Ponte{Host: "/tmp/tunel-1", Porta: 5432, Tunel: &tunel.Tunel{}}, "loja", ""); !strings.Contains(d, "sslmode=disable") {
		t.Fatalf("pelo túnel, o cliente não negocia TLS: %s", d)
	}
	if d := DSN(c, &Ponte{Host: "db.exemplo", Porta: 5432}, "loja", ""); !strings.Contains(d, "sslmode=require") {
		t.Fatalf("direto, vale o sslmode da conexão: %s", d)
	}
	if cfg := ConfigTunel(c, Ambiente{}, Segredos{}); cfg.SSLMode != "require" {
		t.Fatalf("o túnel precisa receber o sslmode: %q", cfg.SSLMode)
	}
}

// A ponte da origem leva a sessão só de leitura no DSN, que serve ao pg_dump e ao pgx.
func TestDSNSoLeitura(t *testing.T) {
	c := conexaoTeste()
	if d := DSN(c, &Ponte{Host: "db", Porta: 5432, SoLeitura: true}, "loja", ""); !strings.Contains(d, `options='-c default_transaction_read_only=on'`) {
		t.Fatalf("a origem começa só de leitura: %s", d)
	}
	if d := DSN(c, &Ponte{Host: "db", Porta: 5432}, "loja", ""); strings.Contains(d, "options") {
		t.Fatalf("o destino escreve: %s", d)
	}
}

func TestLinhaPgpassEscapa(t *testing.T) {
	c := conexaoTeste()
	c.Usuario = "u:1"
	l := LinhaPgpass(c, &Ponte{Host: "127.0.0.1", Porta: 5433}, `p:a\ss`)
	if l != `127.0.0.1:5433:*:u\:1:p\:a\\ss` {
		t.Fatal(l)
	}
}

func TestSenhaPorModo(t *testing.T) {
	c := conexaoTeste()
	if s, err := Senha(c, Segredos{}); err != nil || s != "x" {
		t.Fatal(s, err)
	}
	c.ModoSenha = cadastro.SenhaPerguntar
	if _, err := Senha(c, Segredos{}); err == nil {
		t.Fatal("perguntar sem senha informada")
	}
	var seg Segredos
	seg.GuardarSenha("dev", "y")
	if s, _ := Senha(c, seg); s != "y" {
		t.Fatal(s)
	}
	c.ModoSenha = cadastro.SenhaPgpass
	arq := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(arq, []byte("127.0.0.1:5432:*:postgres:do\\:pgpass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGPASSFILE", arq)
	if s, err := Senha(c, Segredos{}); err != nil || s != "do:pgpass" {
		t.Fatal(s, err)
	}
	c.Usuario = "outro"
	if _, err := Senha(c, Segredos{}); err == nil {
		t.Fatal("pgpass sem linha para o usuário")
	}
}

func TestConsultaBancosPorVersao(t *testing.T) {
	if !strings.Contains(ConsultaBancos(170000), "datlocale") || strings.Contains(ConsultaBancos(170000), "daticulocale") {
		t.Fatal("17")
	}
	if !strings.Contains(ConsultaBancos(160004), "daticulocale") || !strings.Contains(ConsultaBancos(160004), "daticurules") {
		t.Fatal("16")
	}
	if strings.Contains(ConsultaBancos(150000), "daticurules") {
		t.Fatal("15 não tem regras ICU")
	}
	if strings.Contains(ConsultaBancos(140000), "datlocprovider") {
		t.Fatal("14 não tem provedor")
	}
}

// As camadas de rede param no lugar certo, sem banco nenhum: só localhost.
func TestDiagnosticoCamadasDeRede(t *testing.T) {
	ctx := context.Background()
	c := conexaoTeste()

	// Porta fechada: o TCP para com "recusada".
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	porta := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	c.Porta = porta
	d := Diagnosticar(ctx, c, Ambiente{}, Segredos{})
	if d.Parou() != "TCP" || !strings.Contains(d.Info.Erro, "recusada") {
		t.Fatalf("%+v", d)
	}

	// Nome que não resolve (o domínio .invalid nunca resolve, pela RFC 2606).
	c.Host = "nao-existe.invalid"
	d = Diagnosticar(ctx, c, Ambiente{}, Segredos{})
	if d.Parou() != "DNS" {
		t.Fatalf("%+v", d)
	}

	// Socket inexistente.
	c.Host = t.TempDir()
	d = Diagnosticar(ctx, c, Ambiente{}, Segredos{})
	if d.Parou() != "Socket" {
		t.Fatalf("%+v", d)
	}

	// Algo escuta, mas não é Postgres: a camada do Postgres falha.
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l2.Close()
	go func() {
		for {
			x, err := l2.Accept()
			if err != nil {
				return
			}
			_ = x.Close()
		}
	}()
	c.Host, c.Porta = "127.0.0.1", l2.Addr().(*net.TCPAddr).Port
	d = Diagnosticar(ctx, c, Ambiente{}, Segredos{})
	if d.Parou() != "Postgres" {
		t.Fatalf("%+v", d)
	}

	// Senha no modo perguntar, sem senha: para na autenticação e avisa a tela.
	c.ModoSenha = cadastro.SenhaPerguntar
	d = Diagnosticar(ctx, c, Ambiente{}, Segredos{})
	if d.Parou() != "Autenticação" || !d.PrecisaSenha {
		t.Fatalf("%+v", d)
	}
}
