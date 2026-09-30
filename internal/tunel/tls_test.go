package tunel

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// certificados gera uma CA e um certificado de servidor assinado por ela, com os nomes dados (IP
// ou DNS).
func certificados(t *testing.T, nomes ...string) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	chaveCA, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	modeloCA := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA de teste"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	derCA, err := x509.CreateCertificate(rand.Reader, modeloCA, modeloCA, &chaveCA.PublicKey, chaveCA)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(derCA)
	chave, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	modelo := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: nomes[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, n := range nomes {
		if ip := net.ParseIP(n); ip != nil {
			modelo.IPAddresses = append(modelo.IPAddresses, ip)
		} else {
			modelo.DNSNames = append(modelo.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, modelo, ca, &chave.PublicKey, chaveCA)
	if err != nil {
		t.Fatal(err)
	}
	raizes := x509.NewCertPool()
	raizes.AddCert(ca)
	return raizes, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: chave}
}

// falsoPG é o "banco": responde ao pedido de TLS como o Postgres (S com certificado, N sem), lê o
// startup, pede SCRAM (com o PLUS, quando há TLS, como o servidor faz), aceita e manda uma linha:
// "tls" ou "claro", conforme o startup chegou.
func falsoPG(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
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
			go atenderPG(c, cert)
		}
	}()
	return l.Addr().String()
}

func atenderPG(c net.Conn, cert *tls.Certificate) {
	defer c.Close()
	var conn net.Conn = c
	cab := make([]byte, 8)
	if _, err := io.ReadFull(conn, cab); err != nil {
		return // o teste de caminho do Abrir abre e fecha sem mandar nada
	}
	modo := "claro"
	if binary.BigEndian.Uint32(cab[4:8]) == codigoSSLRequest {
		if cert == nil {
			_, _ = conn.Write([]byte{'N'})
		} else {
			_, _ = conn.Write([]byte{'S'})
			s := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}})
			if err := s.Handshake(); err != nil {
				return
			}
			conn, modo = s, "tls"
		}
		if _, err := io.ReadFull(conn, cab); err != nil {
			return
		}
	}
	resto := make([]byte, binary.BigEndian.Uint32(cab[0:4])-8)
	if _, err := io.ReadFull(conn, resto); err != nil {
		return
	}
	if binary.BigEndian.Uint32(cab[4:8]) != 196608 || !strings.Contains(string(resto), "user\x00ana\x00") {
		modo = "startup estragado"
	}
	mecanismos := "SCRAM-SHA-256\x00\x00"
	if modo == "tls" {
		mecanismos = "SCRAM-SHA-256-PLUS\x00" + mecanismos
	}
	_, _ = conn.Write(append(append(mensagemR(10, mecanismos), mensagemR(0, "")...), modo+"\n"...))
}

// mensagemR é uma mensagem de autenticação ('R') com o código e o resto.
func mensagemR(codigo uint32, resto string) []byte {
	corpo := append(binary.BigEndian.AppendUint32(nil, codigo), resto...)
	return append(binary.BigEndian.AppendUint32([]byte{'R'}, uint32(4+len(corpo))), corpo...)
}

func startup() []byte {
	corpo := binary.BigEndian.AppendUint32(nil, 196608)
	corpo = append(corpo, "user\x00ana\x00database\x00loja\x00\x00"...)
	return append(binary.BigEndian.AppendUint32(nil, uint32(4+len(corpo))), corpo...)
}

// conversar conecta no túnel como o libpq num socket: manda o startup em claro, lê a autenticação
// (os mecanismos SASL oferecidos) e a linha que vem depois. Um ErrorResponse volta como erro, com o
// SQLSTATE e a mensagem.
func conversar(t *testing.T, tn *Tunel, pedirTLS bool) (string, []string, error) {
	t.Helper()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tn.Porta()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	if pedirTLS {
		_, _ = c.Write(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), codigoSSLRequest))
		if b, err := r.ReadByte(); err != nil || b != 'N' {
			t.Fatalf("o túnel deveria recusar o TLS do cliente: %q %v", b, err)
		}
	}
	_, _ = c.Write(startup())
	var mecanismos []string
	for {
		cab := make([]byte, 5)
		if _, err := io.ReadFull(r, cab); err != nil {
			return "", nil, err
		}
		corpo := make([]byte, binary.BigEndian.Uint32(cab[1:5])-4)
		if _, err := io.ReadFull(r, corpo); err != nil {
			return "", nil, err
		}
		if cab[0] == 'E' {
			campos := map[byte]string{}
			for _, f := range strings.Split(strings.TrimRight(string(corpo), "\x00"), "\x00") {
				if f != "" {
					campos[f[0]] = f[1:]
				}
			}
			return "", nil, fmt.Errorf("%s %s: %s", campos['S'], campos['C'], campos['M'])
		}
		if cab[0] != 'R' {
			return "", nil, fmt.Errorf("mensagem inesperada %q", cab[0])
		}
		if binary.BigEndian.Uint32(corpo[:4]) != 10 {
			break // o AuthenticationOk
		}
		for _, m := range strings.Split(string(corpo[4:]), "\x00") {
			if m != "" {
				mecanismos = append(mecanismos, m)
			}
		}
	}
	l, err := r.ReadString('\n')
	return strings.TrimSpace(l), mecanismos, err
}

func TestTunelComTLS(t *testing.T) {
	raizes, cert := certificados(t, "127.0.0.1", "localhost")
	_, certOutroNome := certificados(t, "db.outro.exemplo")
	outrasRaizes, _ := certificados(t, "127.0.0.1")
	// Um certificado com o nome errado, mas da CA confiável.
	raizesNome, certNomeErrado := certificados(t, "db.outro.exemplo")

	casos := []struct {
		nome     string
		cert     *tls.Certificate // nil: o banco não tem TLS
		sslmode  string
		raizes   *x509.CertPool
		pedeTLS  bool // o cliente pede TLS ao túnel (o túnel diz não)
		quer     string
		querErro string
	}{
		{nome: "require com TLS no banco", cert: &cert, sslmode: "require", quer: "tls"},
		{nome: "require sem TLS no banco", sslmode: "require", querErro: "não aceita TLS"},
		{nome: "prefer com TLS no banco", cert: &cert, sslmode: "prefer", quer: "tls"},
		{nome: "prefer sem TLS no banco", sslmode: "prefer", quer: "claro"},
		{nome: "disable repassa em claro", cert: &cert, sslmode: "disable", quer: "claro"},
		{nome: "vazio repassa em claro", cert: &cert, sslmode: "", quer: "claro"},
		{nome: "require não confere o certificado", cert: &certOutroNome, sslmode: "require", quer: "tls"},
		{nome: "verify-full com a CA e o nome", cert: &cert, sslmode: "verify-full", raizes: raizes, quer: "tls"},
		{nome: "verify-full com o nome errado", cert: &certNomeErrado, sslmode: "verify-full", raizes: raizesNome, querErro: "certificate"},
		{nome: "verify-full com outra CA", cert: &cert, sslmode: "verify-full", raizes: outrasRaizes, querErro: "certificate"},
		{nome: "verify-ca não confere o nome", cert: &certNomeErrado, sslmode: "verify-ca", raizes: raizesNome, quer: "tls"},
		{nome: "verify-ca com outra CA", cert: &cert, sslmode: "verify-ca", raizes: outrasRaizes, querErro: "certificate"},
		{nome: "verify-full sem TLS no banco", sslmode: "verify-full", raizes: raizes, querErro: "não aceita TLS"},
		{nome: "o cliente pede TLS ao túnel", cert: &cert, sslmode: "require", pedeTLS: true, quer: "tls"},
	}
	cfg, _ := preparar(t)
	aceitarDireto(t, cfg)
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			cfg := cfg
			cfg.Destino, cfg.SSLMode, cfg.RaizesTLS = falsoPG(t, c.cert), c.sslmode, c.raizes
			tn, err := Abrir(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer tn.Fechar()
			// Duas conexões seguidas: cada uma negocia o seu TLS.
			for i := 0; i < 2; i++ {
				got, mecanismos, err := conversar(t, tn, c.pedeTLS)
				if c.querErro != "" {
					if err == nil || !strings.Contains(err.Error(), c.querErro) || !strings.Contains(err.Error(), "FATAL "+CodigoTLS) {
						t.Fatalf("esperava o erro %q do túnel, veio %q %v", c.querErro, got, err)
					}
					continue
				}
				if err != nil || got != c.quer {
					t.Fatalf("esperava %q, veio %q %v", c.quer, got, err)
				}
				// O cliente fala em claro com o túnel: o channel binding (PLUS) nunca chega a ele.
				if strings.Join(mecanismos, ",") != "SCRAM-SHA-256" {
					t.Fatalf("mecanismos oferecidos ao cliente: %v", mecanismos)
				}
			}
		})
	}
}

// O banco que aceita o pedido de TLS e não responde ao handshake: o prazo fecha o canal, e o
// cliente recebe o erro em vez de ficar preso.
func TestTunelTLSComBancoMudo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				cab := make([]byte, 8)
				if _, err := io.ReadFull(c, cab); err != nil {
					return
				}
				_, _ = c.Write([]byte{'S'})
				_, _ = io.Copy(io.Discard, c) // e mais nada
			}()
		}
	}()
	cfg, _ := preparar(t)
	aceitarDireto(t, cfg)
	cfg.Destino, cfg.SSLMode, cfg.Prazo = l.Addr().String(), "require", time.Second
	tn, err := Abrir(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Fechar()
	_, _, err = conversar(t, tn, false)
	if err == nil || !strings.Contains(err.Error(), "a tempo") {
		t.Fatalf("esperava o prazo do TLS: %v", err)
	}
}

func TestSSLModeDesconhecido(t *testing.T) {
	cfg, _ := preparar(t)
	cfg.SSLMode = "allow"
	if _, err := Abrir(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "sslmode desconhecido") {
		t.Fatalf("esperava recusa do sslmode: %v", err)
	}
}
