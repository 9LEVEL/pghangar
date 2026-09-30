//go:build integracao

package motor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/testessh"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

// argsTLS são os argumentos do docker run de um Postgres com TLS e um pg_hba que, pela rede, só
// aceita hostssl. A chave precisa ser do usuário postgres e 600, então o container copia os
// arquivos antes de chamar o entrypoint.
func argsTLS(t *testing.T, versao int) []string {
	t.Helper()
	dir := t.TempDir()
	chaveCA, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	modeloCA := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA de teste"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	derCA, err := x509.CreateCertificate(crand.Reader, modeloCA, modeloCA, &chaveCA.PublicKey, chaveCA)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(derCA)
	chave, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	modelo := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(crand.Reader, modelo, ca, &chave.PublicKey, chaveCA)
	if err != nil {
		t.Fatal(err)
	}
	derChave, err := x509.MarshalECPrivateKey(chave)
	if err != nil {
		t.Fatal(err)
	}
	arquivos := map[string][]byte{
		"servidor.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"servidor.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: derChave}),
		// local: o entrypoint cria o banco pelo socket. Pela rede, só com TLS.
		"pg_hba.conf": []byte("local all all trust\nhostssl all all all scram-sha-256\n"),
	}
	for nome, b := range arquivos {
		if err := os.WriteFile(filepath.Join(dir, nome), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `set -e
mkdir -p /tls
install -o postgres -g postgres -m 600 /tls-origem/servidor.key /tls/
install -o postgres -g postgres -m 644 /tls-origem/servidor.crt /tls-origem/pg_hba.conf /tls/
exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tls/servidor.crt -c ssl_key_file=/tls/servidor.key -c hba_file=/tls/pg_hba.conf`
	return []string{"-v", dir + ":/tls-origem:ro", "--entrypoint", "bash", fmt.Sprintf("postgres:%d", versao), "-c", script}
}

// O banco de origem só aceita TLS pela rede (hostssl), como o pg_hba do caso que achou o problema.
// Pelo túnel, o cliente fala com um socket unix, onde o libpq e o pgx ignoram o sslmode, e quem
// negocia o TLS com o banco é o túnel. Antes disso, o servidor recusava a conexão com "no
// pg_hba.conf entry ... no encryption".
func TestCopiaPeloTunelComTLS(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o, d := subirTLS(t, "o18tls", 18), subir(t, "d18", 18)
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
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", o.porta)

	if err := a.cad.SalvarConexao(ctx, "", cadastro.Conexao{Nome: "prod", Tag: cadastro.TagProd, Acesso: cadastro.AcessoSSH,
		SSHHost: srv.Host, SSHPorta: srv.Porta, SSHUsuario: "pghangar", Host: "127.0.0.1", Porta: o.porta, Usuario: "postgres",
		ModoSenha: cadastro.SenhaGuardar, Senha: senhaTeste, SSLMode: "require", BancoAdmin: "postgres"}); err != nil {
		t.Fatal(err)
	}
	a.conexao(t, "dev", cadastro.TagDev, d)
	semear(t, o, "loja_tls")
	a.perfil(t, "p", "prod", "loja_tls", "dev", "loja_tls")

	_, err = Planejar(ctx, a.d, "p")
	var pg *Pergunta
	if !errors.As(err, &pg) || pg.HostDesconhecido == nil {
		t.Fatalf("esperava host desconhecido: %v", err)
	}
	if err := tunel.Aceitar(a.d.Amb.KnownHosts, pg.HostDesconhecido); err != nil {
		t.Fatal(err)
	}
	diag := func(mudar func(*cadastro.Conexao)) conexao.Diagnostico {
		cx, _ := a.cad.Conexao(ctx, "prod")
		if mudar != nil {
			mudar(&cx)
		}
		return conexao.Diagnosticar(ctx, cx, a.d.Amb, a.d.Seg)
	}
	pronto := func(dg conexao.Diagnostico) string { return dg.Camadas[len(dg.Camadas)-1].Detalhe }

	// 1. O diagnóstico chega ao fim, e o servidor confirma o TLS da sessão.
	if dg := diag(nil); dg.Parou() != "" || !strings.Contains(pronto(dg), "com TLS") {
		t.Fatalf("diagnóstico: %+v", dg)
	}

	// 2. A cópia inteira: o pg_dump, no container, passa pelo TLS do túnel. Sem ele, o pg_hba
	// recusaria.
	e := a.copiar(t, ctx, "p")
	if e.Estado != cadastro.EstadoOK {
		t.Fatalf("%s %s\n%s", e.Estado, e.Mensagem, a.log.String())
	}
	if n := valor[int64](t, d, "loja_tls", `SELECT count(*) FROM vendas.pedido`); n != 20000 {
		t.Fatalf("pedidos no destino: %d", n)
	}
	if strings.Contains(a.log.String(), senhaTeste) {
		t.Fatal("a senha apareceu no log")
	}

	// 3. Sem TLS, o erro do caso real.
	if dg := diag(func(c *cadastro.Conexao) { c.SSLMode = "disable" }); dg.Parou() != "Autenticação" || !strings.Contains(dg.Info.Erro, "no encryption") {
		t.Fatalf("sem TLS, o pg_hba deveria recusar: %+v", dg)
	}

	// 4. require num banco sem TLS para na camada TLS, com o motivo; prefer segue em claro, e o
	// diagnóstico diz.
	semTLS := subir(t, "o18", 18)
	srv.Permitido = fmt.Sprintf("127.0.0.1:%d", semTLS.porta)
	if dg := diag(func(c *cadastro.Conexao) { c.Porta = semTLS.porta }); dg.Parou() != "TLS" || !strings.Contains(dg.Info.Erro, "não aceita TLS") {
		t.Fatalf("require sem TLS no banco deveria parar na camada TLS: %+v", dg)
	}
	if dg := diag(func(c *cadastro.Conexao) { c.Porta, c.SSLMode = semTLS.porta, "prefer" }); dg.Parou() != "" || !strings.Contains(pronto(dg), "sem TLS") {
		t.Fatalf("prefer sem TLS no banco: %+v", dg)
	}
}
