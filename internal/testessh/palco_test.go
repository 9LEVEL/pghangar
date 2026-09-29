//go:build palco

// O palco para testar a tela à mão, só em localhost: três Postgres em containers presos em
// 127.0.0.1 e o servidor SSH de teste na frente da "produção". Escreve os endereços em
// $PALCO_ARQUIVO e fica no ar até o prazo do teste.
//
//	PALCO_DIR=/tmp/cb PALCO_ARQUIVO=/tmp/palco.env go test -tags palco ./internal/testessh -run TestPalco -timeout 90m
package testessh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

const senha = "palco-s3nha"

func subir(t *testing.T, nome string, versao int) int {
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", nome, "-e", "POSTGRES_PASSWORD="+senha,
		"-p", "127.0.0.1::5432", fmt.Sprintf("postgres:%d", versao)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", nome).Run() })
	out, _ = exec.Command("docker", "port", nome, "5432/tcp").Output()
	l := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	if !strings.HasPrefix(l, "127.0.0.1:") {
		t.Fatalf("precisa estar preso em 127.0.0.1: %s", l)
	}
	var porta int
	fmt.Sscanf(l[strings.LastIndex(l, ":")+1:], "%d", &porta)
	for i := 0; i < 120; i++ {
		c, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", senha, porta))
		if err == nil {
			_ = c.Close(context.Background())
			time.Sleep(2 * time.Second)
			return porta
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("não subiu")
	return 0
}

func exe(t *testing.T, porta int, banco string, cmds ...string) {
	c, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/%s?sslmode=disable", senha, porta, banco))
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

func TestPalco(t *testing.T) {
	dir, arq := os.Getenv("PALCO_DIR"), os.Getenv("PALCO_ARQUIVO")
	if dir == "" || arq == "" {
		t.Skip("defina PALCO_DIR e PALCO_ARQUIVO")
	}
	prod := subir(t, "copia-banco-palco-prod16", 16)
	homolog := subir(t, "copia-banco-palco-homolog18", 18)
	dev := subir(t, "copia-banco-palco-dev17", 17)

	exe(t, prod, "postgres", "CREATE DATABASE loja", "CREATE DATABASE financeiro")
	exe(t, prod, "loja",
		"CREATE EXTENSION pg_trgm", "CREATE SCHEMA vendas",
		"CREATE TABLE vendas.cliente (id serial PRIMARY KEY, nome text)",
		"CREATE TABLE vendas.pedido (id bigserial PRIMARY KEY, cliente int REFERENCES vendas.cliente, valor numeric)",
		"INSERT INTO vendas.cliente (nome) SELECT 'cliente ' || g FROM generate_series(1, 50000) g",
		"INSERT INTO vendas.pedido (cliente, valor) SELECT 1 + g % 50000, g % 700 FROM generate_series(1, 800000) g",
		"CREATE TABLE public.log_eventos AS SELECT g id, md5(g::text) msg FROM generate_series(1, 300000) g",
		"CREATE TABLE public.config (k text PRIMARY KEY, v text)", "INSERT INTO config VALUES ('url', 'https://loja.exemplo')")
	exe(t, homolog, "postgres", "CREATE ROLE app_homolog LOGIN PASSWORD 'x'", "CREATE DATABASE loja OWNER app_homolog",
		`ALTER DATABASE loja SET search_path TO "$user", public, vendas`)
	exe(t, homolog, "loja", "CREATE TABLE velha (x int)", "ALTER TABLE velha OWNER TO app_homolog")

	pub := filepath.Join(dir, "chaves", "id_ed25519.pub")
	srv, err := NovoCom(func(k ssh.PublicKey) bool {
		b, err := os.ReadFile(pub)
		if err != nil {
			return false
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(b)
		return err == nil && string(pk.Marshal()) == string(k.Marshal())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Parar()
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", prod)

	env := fmt.Sprintf("SENHA=%s\nPROD=%d\nHOMOLOG=%d\nDEV=%d\nSSH=%d\n", senha, prod, homolog, dev, srv.Porta)
	if err := os.WriteFile(arq, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Log("palco no ar:\n" + env)
	prazo, _ := t.Deadline()
	time.Sleep(time.Until(prazo) - 30*time.Second)
}
