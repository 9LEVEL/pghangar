package tunel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/9LEVEL/pghangar/internal/testessh"
)

// Com bastion: o SSH até o servidor passa por dentro dele, e as duas chaves de host são conferidas.
func TestTunelPeloBastion(t *testing.T) {
	cfg, alvo := preparar(t)
	pub, _ := ChavePublica(cfg.Chave)
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	bastion, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer bastion.Parar()
	bastion.Permitido = net.JoinHostPort(alvo.Host, strconv.Itoa(alvo.Porta)) // o permitopen do bastion
	cfg.Salto = &Salto{Host: bastion.Host, Porta: bastion.Porta, Usuario: "salto"}

	// Os dois hosts são desconhecidos: o bastion primeiro, depois o servidor.
	for i := 0; i < 2; i++ {
		_, err := Abrir(context.Background(), cfg)
		var hd *HostDesconhecido
		if !errors.As(err, &hd) {
			t.Fatalf("esperava host desconhecido (%d): %v", i, err)
		}
		if err := Aceitar(cfg.KnownHosts, hd); err != nil {
			t.Fatal(err)
		}
	}
	tn, err := Abrir(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tn.Porta()))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("pelo bastion\n"))
	if l, err := bufio.NewReader(c).ReadString('\n'); err != nil || l != "pelo bastion\n" {
		t.Fatalf("eco: %q %v", l, err)
	}
	_ = c.Close()
	tn.Fechar()

	// O bastion que não deixa passar para o servidor: a falha diz que foi no salto.
	bastion.Permitido = "127.0.0.1:1"
	_, err = Abrir(context.Background(), cfg)
	var f *FalhaSSH
	if !errors.As(err, &f) || f.Camada != "salto" {
		t.Fatalf("esperava falha no salto: %v", err)
	}
}

// eco é o "banco": devolve o que recebe.
func eco(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return l.Addr().String()
}

func preparar(t *testing.T) (Config, *testessh.Servidor) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	chave := filepath.Join(dir, "id_ed25519")
	if err := GerarChave(chave, "pghangar@teste"); err != nil {
		t.Fatal(err)
	}
	pub, err := ChavePublica(chave)
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}
	s, err := testessh.Novo(pk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Parar)
	return Config{Host: s.Host, Porta: s.Porta, Usuario: "pghangar", Chave: chave,
		KnownHosts: filepath.Join(dir, "known_hosts"), Destino: eco(t), Prazo: 3 * time.Second}, s
}

func TestHostDesconhecidoAceitarETunelFunciona(t *testing.T) {
	cfg, _ := preparar(t)
	ctx := context.Background()

	_, err := Abrir(ctx, cfg)
	var hd *HostDesconhecido
	if !errors.As(err, &hd) || !strings.HasPrefix(hd.Fingerprint, "SHA256:") {
		t.Fatalf("esperava host desconhecido, veio %v", err)
	}
	if err := Aceitar(cfg.KnownHosts, hd); err != nil {
		t.Fatal(err)
	}
	if err := Aceitar(cfg.KnownHosts, hd); err == nil {
		t.Fatal("aceitar duas vezes deveria ser recusado")
	}

	tn, err := Abrir(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Fechar()
	for i := 0; i < 3; i++ { // várias conexões pelo mesmo túnel, como o pg_dump -j
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tn.Porta()))
		if err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("olá %d\n", i)
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		linha, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || linha != msg {
			t.Fatalf("eco: %q %v", linha, err)
		}
		_ = c.Close()
	}
}

func aceitarDireto(t *testing.T, cfg Config) {
	t.Helper()
	_, err := Conectar(context.Background(), cfg)
	var hd *HostDesconhecido
	if !errors.As(err, &hd) {
		t.Fatalf("esperava host desconhecido: %v", err)
	}
	if err := Aceitar(cfg.KnownHosts, hd); err != nil {
		t.Fatal(err)
	}
}

func TestChaveDoHostMudouBloqueia(t *testing.T) {
	cfg, _ := preparar(t)
	// Um known_hosts com outra chave para o mesmo endereço.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	outra, _ := ssh.NewPublicKey(pub)
	end := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Porta))
	if err := Aceitar(cfg.KnownHosts, &HostDesconhecido{Endereco: end, Chave: outra}); err != nil {
		t.Fatal(err)
	}
	_, err := Abrir(context.Background(), cfg)
	var f *FalhaSSH
	if !errors.As(err, &f) || f.Camada != "chave do host" || !errors.Is(err, ErrChaveMudou) {
		t.Fatalf("esperava chave mudou, veio %v", err)
	}
}

func TestChaveRecusada(t *testing.T) {
	cfg, _ := preparar(t)
	aceitarDireto(t, cfg)
	outra := filepath.Join(t.TempDir(), "outra")
	if err := GerarChave(outra, "x"); err != nil {
		t.Fatal(err)
	}
	cfg.Chave = outra
	_, err := Abrir(context.Background(), cfg)
	var f *FalhaSSH
	if !errors.As(err, &f) || f.Camada != "autenticação" {
		t.Fatalf("esperava autenticação, veio %v", err)
	}
}

func TestDestinoInalcancavel(t *testing.T) {
	cfg, _ := preparar(t)
	aceitarDireto(t, cfg)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	fechado := l.Addr().String()
	_ = l.Close()
	cfg.Destino = fechado
	_, err := Abrir(context.Background(), cfg)
	var f *FalhaSSH
	if !errors.As(err, &f) || f.Camada != "canal" {
		t.Fatalf("esperava canal, veio %v", err)
	}
}

func TestSSHForaDoAr(t *testing.T) {
	cfg, s := preparar(t)
	s.Parar()
	_, err := Conectar(context.Background(), cfg)
	var f *FalhaSSH
	if !errors.As(err, &f) || f.Camada != "tcp" {
		t.Fatalf("esperava tcp, veio %v", err)
	}
}

func TestTunelFechaQuandoOServidorCai(t *testing.T) {
	cfg, s := preparar(t)
	aceitarDireto(t, cfg)
	tn, err := Abrir(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Parar()
	select {
	case <-tn.Fechado():
		if tn.Erro() == nil {
			t.Fatal("o túnel caiu sem motivo registrado")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("o túnel não percebeu a queda")
	}
}

func TestGerarChaveNaoSobrescreveEFicaCom600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chaves", "id_ed25519")
	if err := GerarChave(p, "c"); err != nil {
		t.Fatal(err)
	}
	if err := GerarChave(p, "c"); err == nil {
		t.Fatal("sobrescreveu a chave")
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("%o", fi.Mode().Perm())
	}
	precisa, err := PrecisaDeFrase(p)
	if err != nil || precisa {
		t.Fatal(precisa, err)
	}
}

func TestChaveComPassphrase(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	bloco, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "c", []byte("segredo"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, pem.EncodeToMemory(bloco), 0o600); err != nil {
		t.Fatal(err)
	}
	if precisa, err := PrecisaDeFrase(p); err != nil || !precisa {
		t.Fatal(precisa, err)
	}
	_, _, err = metodos(Config{Chave: p})
	var pf *PrecisaFrase
	if !errors.As(err, &pf) {
		t.Fatalf("esperava PrecisaFrase: %v", err)
	}
	if _, _, err := metodos(Config{Chave: p, Frase: "segredo"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = metodos(Config{Chave: p, Frase: "errada"})
	if !errors.As(err, &pf) || !pf.Errada {
		t.Fatalf("frase errada deveria voltar PrecisaFrase{Errada}: %v", err)
	}
}

func TestLinhaAuthorizedKeys(t *testing.T) {
	l := LinhaAuthorizedKeys("ssh-ed25519 AAAA c", "127.0.0.1:5433")
	if l != `restrict,port-forwarding,permitopen="127.0.0.1:5433",command="/bin/false" ssh-ed25519 AAAA c` {
		t.Fatal(l)
	}
}

func TestTunelNumSocketUnix(t *testing.T) {
	cfg, _ := preparar(t)
	aceitarDireto(t, cfg)
	dir, err := os.MkdirTemp("", "cb-teste-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg.Socket = filepath.Join(dir, ".s.PGSQL.5432")
	tn, err := Abrir(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tn.Porta() != 0 {
		t.Fatal("no socket unix, não há porta TCP")
	}
	fi, err := os.Stat(cfg.Socket)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("o socket deveria ter 600: %v %v", fi.Mode().Perm(), err)
	}
	c, err := net.Dial("unix", cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping\n"))
	if l, err := bufio.NewReader(c).ReadString('\n'); err != nil || l != "ping\n" {
		t.Fatalf("eco: %q %v", l, err)
	}
	_ = c.Close()
	tn.Fechar()
	if _, err := os.Stat(cfg.Socket); err == nil {
		t.Fatal("o socket deveria sumir ao fechar o túnel")
	}
}
