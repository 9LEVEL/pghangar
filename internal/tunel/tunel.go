// Package tunel abre o túnel SSH até o banco (docs/ESTRATEGIA.md §4): uma conexão SSH, um socket
// unix local e, para cada conexão aceita nele, um canal até o banco visto a partir do servidor SSH.
// O container monta o diretório do socket.
//
// O TLS com o banco é do túnel: no socket unix, o libpq e o pgx ignoram o sslmode, e o canal SSH
// só cifra até o servidor SSH. Quando o sslmode pede, o túnel negocia o TLS com o banco pelo canal
// e só então repassa o que o cliente mandou.
//
// O known_hosts é o da ferramenta e é estrito: um host desconhecido volta como *HostDesconhecido,
// com o fingerprint, para a tela perguntar; uma chave diferente da registrada bloqueia.
package tunel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Config é o que o túnel precisa saber.
type Config struct {
	Host       string // o servidor SSH
	Porta      int
	Usuario    string
	Chave      string // caminho da chave privada
	Frase      string // passphrase da chave, se ela tiver
	KnownHosts string
	Destino    string // host:porta do banco visto a partir do servidor SSH
	Prazo      time.Duration
	Keepalive  time.Duration
	// Salto, se houver, é o bastion por onde o SSH passa.
	Salto *Salto
	// Socket, se informado, é o arquivo do socket unix em que o túnel escuta (num diretório 700):
	// só o root chega nele. Sem ele, o túnel escuta numa porta TCP de 127.0.0.1, que qualquer
	// usuário local alcança (só os testes usam assim).
	Socket string
	// SSLMode é o sslmode da conexão, cumprido pelo túnel: prefer e require cifram sem conferir o
	// certificado, verify-ca confere a CA e verify-full confere também o nome do host do Destino.
	// Vazio ou disable repassa os bytes como vierem.
	SSLMode string
	// RaizesTLS são as CAs do verify-ca e do verify-full. Nil são as do sistema.
	RaizesTLS *x509.CertPool
}

func (c Config) endereco() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Porta)) }

// Salto é o host intermediário (bastion) até o servidor SSH. Usa a mesma chave e o mesmo
// known_hosts: a chave dele é conferida como a de qualquer servidor.
type Salto struct {
	Host    string
	Porta   int
	Usuario string
}

// HostDesconhecido é o servidor SSH que o known_hosts da ferramenta ainda não conhece.
type HostDesconhecido struct {
	Endereco    string
	Chave       ssh.PublicKey
	Fingerprint string
}

func (e *HostDesconhecido) Error() string {
	return fmt.Sprintf("o servidor SSH %s ainda não é conhecido (chave %s %s): confira o fingerprint e aceite", e.Endereco, e.Chave.Type(), e.Fingerprint)
}

// ErrChaveMudou é a chave de um host conhecido diferente da registrada.
var ErrChaveMudou = errors.New("A CHAVE DO SERVIDOR SSH MUDOU: pare e investigue antes de continuar")

// FalhaSSH diz em que ponto a conexão SSH parou: tcp, handshake, chave do host, autenticação, canal.
type FalhaSSH struct {
	Camada string
	Err    error
}

func (f *FalhaSSH) Error() string { return f.Err.Error() }
func (f *FalhaSSH) Unwrap() error { return f.Err }

// PrecisaFrase é a chave protegida por passphrase, sem a passphrase (ou com a errada).
type PrecisaFrase struct {
	Chave  string
	Errada bool
}

func (e *PrecisaFrase) Error() string {
	if e.Errada {
		return "a passphrase da chave " + e.Chave + " está errada"
	}
	return "a chave " + e.Chave + " tem passphrase: informe-a"
}

// Tunel é um túnel aberto.
type Tunel struct {
	cliente  *ssh.Client
	ouvinte  net.Listener
	destino  string
	sslmode  string
	tls      *tls.Config // nil sem TLS
	prazo    time.Duration
	fechado  chan struct{}
	fecharUm sync.Once
	mu       sync.Mutex
	erro     error
	conexoes sync.WaitGroup
}

// Conectar abre só a conexão SSH (o diagnóstico usa para testar as camadas).
func Conectar(ctx context.Context, c Config) (*ssh.Client, error) {
	if c.Prazo <= 0 {
		c.Prazo = 10 * time.Second
	}
	auth, fechar, err := metodos(c)
	if err != nil {
		var pf *PrecisaFrase
		if errors.As(err, &pf) {
			return nil, pf
		}
		return nil, &FalhaSSH{Camada: "chave", Err: err}
	}
	// A conexão com o ssh-agent só serve para a autenticação.
	defer fechar()
	conferir, err := conferidor(c.KnownHosts)
	if err != nil {
		return nil, &FalhaSSH{Camada: "known_hosts", Err: err}
	}
	d := net.Dialer{Timeout: c.Prazo}
	if c.Salto == nil {
		end := c.endereco()
		conn, err := d.DialContext(ctx, "tcp", end)
		if err != nil {
			return nil, &FalhaSSH{Camada: "tcp", Err: fmt.Errorf("conectando no SSH %s: %w", end, err)}
		}
		return handshake(ctx, conn, end, c.Usuario, c, auth, conferir, "")
	}
	// Com salto: primeiro o bastion, depois o servidor, por um canal do bastion.
	endSalto := net.JoinHostPort(c.Salto.Host, strconv.Itoa(c.Salto.Porta))
	conn, err := d.DialContext(ctx, "tcp", endSalto)
	if err != nil {
		return nil, &FalhaSSH{Camada: "tcp", Err: fmt.Errorf("conectando no bastion %s: %w", endSalto, err)}
	}
	bastion, err := handshake(ctx, conn, endSalto, c.Salto.Usuario, c, auth, conferir, "bastion ")
	if err != nil {
		return nil, err
	}
	end := c.endereco()
	canal, err := bastion.Dial("tcp", end)
	if err != nil {
		_ = bastion.Close()
		return nil, &FalhaSSH{Camada: "salto", Err: fmt.Errorf("o bastion %s não abriu o caminho até o SSH %s: %w (confira o permitopen do authorized_keys no bastion)", endSalto, end, err)}
	}
	alvo, err := handshake(ctx, canal, end, c.Usuario, c, auth, conferir, "")
	if err != nil {
		_ = bastion.Close()
		return nil, err
	}
	// Fechado o servidor, fecha o bastion junto.
	go func() {
		_ = alvo.Wait()
		_ = bastion.Close()
	}()
	return alvo, nil
}

// handshake faz o SSH sobre uma conexão já aberta, com prazo, e explica a falha pela camada.
func handshake(ctx context.Context, conn net.Conn, end, usuario string, c Config, auth []ssh.AuthMethod, conferir ssh.HostKeyCallback, quem string) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:              usuario,
		Auth:              auth,
		HostKeyCallback:   conferir,
		HostKeyAlgorithms: algoritmosConhecidos(c.KnownHosts, end),
		Timeout:           c.Prazo,
	}
	// Sem prazo na conexão, um sshd que aceita e não responde prende o handshake para sempre.
	prazo := time.Now().Add(2 * c.Prazo)
	if p, ok := ctx.Deadline(); ok && p.Before(prazo) {
		prazo = p
	}
	_ = conn.SetDeadline(prazo)
	pronto := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-pronto:
		}
	}()
	cc, canais, pedidos, err := ssh.NewClientConn(conn, end, cfg)
	close(pronto)
	if err != nil {
		_ = conn.Close()
		var hd *HostDesconhecido
		switch {
		case errors.As(err, &hd):
			return nil, hd
		case errors.Is(err, ErrChaveMudou):
			return nil, &FalhaSSH{Camada: "chave do host", Err: fmt.Errorf("%s%s: %w", quem, end, ErrChaveMudou)}
		case strings.Contains(err.Error(), "unable to authenticate"):
			return nil, &FalhaSSH{Camada: "autenticação", Err: fmt.Errorf("o %sSSH %s recusou a chave (usuário %s): confira o authorized_keys", quem, end, usuario)}
		case ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded):
			return nil, &FalhaSSH{Camada: "handshake", Err: fmt.Errorf("o %sSSH %s não respondeu a tempo", quem, end)}
		}
		return nil, &FalhaSSH{Camada: "handshake", Err: fmt.Errorf("%sSSH %s: %w", quem, end, err)}
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, canais, pedidos), nil
}

// Abrir abre o túnel: a conexão SSH, a porta local e o keepalive. Testa o caminho até o destino
// antes de devolver, para "o servidor SSH não alcança o banco" aparecer aqui, com a camada certa.
func Abrir(ctx context.Context, c Config) (*Tunel, error) {
	if c.Keepalive <= 0 {
		c.Keepalive = 30 * time.Second
	}
	if c.Prazo <= 0 {
		c.Prazo = 10 * time.Second
	}
	cfgTLS, err := configTLS(c.SSLMode, c.Destino, c.RaizesTLS)
	if err != nil {
		return nil, err
	}
	cliente, err := Conectar(ctx, c)
	if err != nil {
		return nil, err
	}
	teste, err := cliente.Dial("tcp", c.Destino)
	if err != nil {
		_ = cliente.Close()
		return nil, &FalhaSSH{Camada: "canal", Err: fmt.Errorf("o servidor SSH %s não abriu o caminho até %s: %w (confira o permitopen do authorized_keys e se o banco escuta nesse endereço)", c.endereco(), c.Destino, err)}
	}
	_ = teste.Close()

	var ouvinte net.Listener
	if c.Socket != "" {
		ouvinte, err = net.Listen("unix", c.Socket)
		if err == nil {
			err = os.Chmod(c.Socket, 0o600)
		}
	} else {
		ouvinte, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		if ouvinte != nil {
			_ = ouvinte.Close()
		}
		_ = cliente.Close()
		return nil, err
	}
	t := &Tunel{cliente: cliente, ouvinte: ouvinte, destino: c.Destino, sslmode: c.SSLMode, tls: cfgTLS, prazo: c.Prazo,
		fechado: make(chan struct{})}
	go t.aceitar()
	go t.manter(c.Keepalive)
	go func() {
		// Se a conexão SSH cair, o túnel fecha junto, e quem espera fica sabendo pelo Erro.
		err := cliente.Wait()
		t.falhar(fmt.Errorf("a conexão SSH caiu: %v", err))
	}()
	return t, nil
}

// Porta é a porta local do túnel, em 127.0.0.1 (0 quando ele escuta num socket unix).
func (t *Tunel) Porta() int {
	if a, ok := t.ouvinte.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Fechado avisa quando o túnel fecha.
func (t *Tunel) Fechado() <-chan struct{} { return t.fechado }

// Erro é o motivo do fechamento, se não foi o Fechar.
func (t *Tunel) Erro() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.erro
}

func (t *Tunel) Fechar() { t.falhar(nil) }

func (t *Tunel) falhar(err error) {
	t.fecharUm.Do(func() {
		t.mu.Lock()
		t.erro = err
		t.mu.Unlock()
		_ = t.ouvinte.Close()
		_ = t.cliente.Close()
		close(t.fechado)
	})
}

func (t *Tunel) aceitar() {
	for {
		local, err := t.ouvinte.Accept()
		if err != nil {
			return
		}
		t.conexoes.Add(1)
		go func() {
			defer t.conexoes.Done()
			defer local.Close()
			remoto, doBanco, filtrar, err := t.ligar(local)
			if err != nil {
				return
			}
			defer remoto.Close()
			fim := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remoto, local); fim <- struct{}{} }()
			go func() { _ = devolver(local, doBanco, filtrar); fim <- struct{}{} }()
			select {
			case <-fim:
			case <-t.fechado:
			}
		}()
	}
}

// manter manda keepalives: sem eles, um NAT ou firewall derruba a conexão parada entre as tabelas
// grandes. Três falhas seguidas fecham o túnel.
func (t *Tunel) manter(intervalo time.Duration) {
	tique := time.NewTicker(intervalo)
	defer tique.Stop()
	falhas := 0
	for {
		select {
		case <-t.fechado:
			return
		case <-tique.C:
			res := make(chan error, 1)
			go func() {
				_, _, err := t.cliente.SendRequest("keepalive@openssh.com", true, nil)
				res <- err
			}()
			select {
			case err := <-res:
				if err != nil {
					falhas++
				} else {
					falhas = 0
				}
			case <-time.After(intervalo):
				falhas++
			case <-t.fechado:
				return
			}
			if falhas >= 3 {
				t.falhar(errors.New("o servidor SSH parou de responder aos keepalives"))
				return
			}
		}
	}
}

// --- TLS com o banco --------------------------------------------------------------------------

// CodigoTLS é o SQLSTATE do erro que o túnel responde ao cliente quando não consegue o TLS que o
// sslmode pede (08001: não foi possível estabelecer a conexão).
const CodigoTLS = "08001"

// CodigoCanal é o SQLSTATE do erro que o túnel responde quando o servidor SSH não abre o canal até
// o banco (08006: falha na conexão).
const CodigoCanal = "08006"

const (
	codigoSSLRequest    = 80877103
	codigoGSSENCRequest = 80877104
	maxPacoteInicial    = 10000 // o MAX_STARTUP_PACKET_LENGTH do servidor
)

// configTLS monta o TLS do sslmode. Nil é sem TLS.
func configTLS(sslmode, destino string, raizes *x509.CertPool) (*tls.Config, error) {
	if sslmode == "" || sslmode == "disable" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(destino)
	if err != nil {
		return nil, fmt.Errorf("destino do túnel %q: %w", destino, err)
	}
	cfg := &tls.Config{ServerName: host, RootCAs: raizes, MinVersion: tls.VersionTLS12, NextProtos: []string{"postgresql"}}
	switch sslmode {
	case "prefer", "require":
		// Como o libpq: cifra, sem conferir o certificado.
		cfg.InsecureSkipVerify = true
	case "verify-ca":
		// Contra as CAs do sistema, conferir a cadeia sem o nome aceita qualquer certificado público
		// (o libpq recusa essa combinação). Só com a CA do servidor.
		if raizes == nil {
			return nil, errors.New("verify-ca pelo túnel só confere contra a CA do servidor, e as do sistema aceitariam qualquer certificado público: use verify-full (que confere também o nome) ou require")
		}
		// Confere a cadeia, e não o nome: o crypto/tls não tem esse meio-termo pronto.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(brutos [][]byte, _ [][]*x509.Certificate) error {
			return conferirCA(brutos, raizes)
		}
	case "verify-full":
	default:
		return nil, fmt.Errorf("sslmode desconhecido: %q", sslmode)
	}
	return cfg, nil
}

func conferirCA(brutos [][]byte, raizes *x509.CertPool) error {
	if len(brutos) == 0 {
		return errors.New("o servidor não mandou certificado")
	}
	certs := make([]*x509.Certificate, len(brutos))
	for i, b := range brutos {
		c, err := x509.ParseCertificate(b)
		if err != nil {
			return err
		}
		certs[i] = c
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{Roots: raizes, Intermediates: inter})
	return err
}

// ligar abre o canal até o banco para uma conexão do cliente, e devolve o canal, o leitor do que
// vem do banco e se esse leitor passa pelo filtro da autenticação (com o TLS no túnel).
//
// Sem TLS, os bytes passam como vêm, e o canal só abre quando o cliente fala: uma conexão que abre e
// fecha (a sondagem do túnel vivo) não chega ao banco. Com TLS, o túnel lê antes o pacote inicial do
// cliente (ele fala primeiro), negocia o TLS com o banco e só então repassa o pacote. O que não sai
// (o TLS, o canal) chega ao cliente como um erro do protocolo com o motivo, e não só como uma
// conexão fechada.
func (t *Tunel) ligar(local net.Conn) (remoto net.Conn, doBanco io.Reader, filtrar bool, err error) {
	if t.tls == nil {
		primeiro := make([]byte, 1)
		_ = local.SetReadDeadline(time.Now().Add(t.prazo))
		if _, err := io.ReadFull(local, primeiro); err != nil {
			return nil, nil, false, err
		}
		_ = local.SetReadDeadline(time.Time{})
		if remoto, err = t.abrirCanal(local); err != nil {
			return nil, nil, false, err
		}
		if _, err := remoto.Write(primeiro); err != nil {
			_ = remoto.Close()
			return nil, nil, false, err
		}
		return remoto, remoto, false, nil
	}
	inicial, err := lerInicial(local, t.prazo)
	if err != nil {
		return nil, nil, false, err
	}
	if remoto, err = t.abrirCanal(local); err != nil {
		return nil, nil, false, err
	}
	seguro, err := t.negociar(remoto)
	if err != nil {
		if t.sslmode != "prefer" {
			responderErro(local, CodigoTLS, fmt.Sprintf("o túnel não conseguiu TLS com o banco %s (sslmode %s): %v", t.destino, t.sslmode, err))
			return nil, nil, false, err
		}
		// Como o libpq no prefer: se o TLS falha, tenta de novo sem ele.
		return t.emClaro(local, inicial)
	}
	if _, cifrado := seguro.(*tls.Conn); !cifrado {
		// prefer, e o banco sem TLS.
		if _, err := seguro.Write(inicial); err != nil {
			_ = seguro.Close()
			return nil, nil, false, err
		}
		return seguro, seguro, false, nil
	}
	if _, err := seguro.Write(inicial); err != nil {
		_ = seguro.Close()
		return nil, nil, false, err
	}
	if t.sslmode != "prefer" {
		return seguro, seguro, true, nil
	}
	// No prefer, como o libpq e o pgx: se o banco recusa logo de cara a conexão cifrada (um pg_hba
	// só com hostnossl para este caminho), tenta de novo sem TLS. A primeira mensagem decide.
	br := bufio.NewReader(seguro)
	parar := time.AfterFunc(t.prazo, func() { _ = seguro.Close() })
	msg, err := lerMensagem(br)
	if !parar.Stop() {
		err = errors.New("o banco não respondeu a tempo")
	}
	if err != nil {
		_ = seguro.Close()
		return nil, nil, false, err
	}
	if msg[0] == 'E' && campoDoErro(msg, 'C') == "28000" {
		_ = seguro.Close()
		return t.emClaro(local, inicial)
	}
	return seguro, io.MultiReader(bytes.NewReader(msg), br), true, nil
}

// abrirCanal abre o canal SSH até o banco. Se o servidor SSH não o abre, o cliente recebe o motivo.
func (t *Tunel) abrirCanal(local net.Conn) (net.Conn, error) {
	remoto, err := t.cliente.Dial("tcp", t.destino)
	if err != nil {
		responderErro(local, CodigoCanal, fmt.Sprintf("o servidor SSH não abriu o caminho até o banco %s: %v", t.destino, err))
		return nil, err
	}
	return remoto, nil
}

// emClaro refaz a conexão até o banco sem TLS (o prefer) e repassa o pacote inicial do cliente.
func (t *Tunel) emClaro(local net.Conn, inicial []byte) (net.Conn, io.Reader, bool, error) {
	remoto, err := t.abrirCanal(local)
	if err != nil {
		return nil, nil, false, err
	}
	if _, err := remoto.Write(inicial); err != nil {
		_ = remoto.Close()
		return nil, nil, false, err
	}
	return remoto, remoto, false, nil
}

// lerMensagem lê uma mensagem inteira do banco: o tipo, o tamanho e o corpo.
func lerMensagem(r io.Reader) ([]byte, error) {
	cab := make([]byte, 5)
	if _, err := io.ReadFull(r, cab); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(cab[1:5])
	if n < 4 || n > maxMensagemInicio {
		return nil, fmt.Errorf("mensagem do banco inválida na autenticação (%q, %d bytes)", cab[0], n)
	}
	msg := make([]byte, 1+n)
	copy(msg, cab)
	if _, err := io.ReadFull(r, msg[5:]); err != nil {
		return nil, err
	}
	return msg, nil
}

// campoDoErro lê um campo (o 'C', o SQLSTATE) de um ErrorResponse inteiro.
func campoDoErro(msg []byte, campo byte) string {
	for _, f := range bytes.Split(msg[5:], []byte{0}) {
		if len(f) > 0 && f[0] == campo {
			return string(f[1:])
		}
	}
	return ""
}

// negociar pede o TLS ao banco, como o libpq: o SSLRequest, a resposta de um byte e o handshake.
// Devolve o canal em claro quando o banco não tem TLS e o sslmode é prefer. Fecha o canal na falha.
func (t *Tunel) negociar(remoto net.Conn) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.prazo)
	defer cancel()
	// O canal SSH não aceita prazo: vencido o prazo, ele é fechado.
	parar := context.AfterFunc(ctx, func() { _ = remoto.Close() })
	conn, err := pedirTLS(ctx, remoto, t.tls, t.sslmode)
	if !parar() {
		err = errors.New("o banco não respondeu a tempo ao pedido de TLS")
	}
	if err != nil {
		_ = remoto.Close()
		return nil, err
	}
	return conn, nil
}

func pedirTLS(ctx context.Context, remoto net.Conn, cfg *tls.Config, sslmode string) (net.Conn, error) {
	pedido := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), codigoSSLRequest)
	if _, err := remoto.Write(pedido); err != nil {
		return nil, err
	}
	// Um byte só, lido direto do canal: nada que o servidor mande antes do handshake fica num buffer
	// para ser lido depois como se tivesse vindo cifrado.
	resp := make([]byte, 1)
	if _, err := io.ReadFull(remoto, resp); err != nil {
		return nil, fmt.Errorf("lendo a resposta ao pedido de TLS: %w", err)
	}
	switch resp[0] {
	case 'S':
		c := tls.Client(remoto, cfg)
		if err := c.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("handshake TLS: %w", err)
		}
		return c, nil
	case 'N':
		if sslmode == "prefer" {
			return remoto, nil
		}
		return nil, errors.New("o servidor não aceita TLS")
	}
	return nil, fmt.Errorf("resposta inesperada ao pedido de TLS: %q", resp[0])
}

// lerInicial lê o primeiro pacote do cliente: o startup ou um CancelRequest. Um pedido de TLS ou
// de GSSAPI do cliente recebe "não", porque a cifra é do túnel.
func lerInicial(local net.Conn, prazo time.Duration) ([]byte, error) {
	_ = local.SetReadDeadline(time.Now().Add(prazo))
	defer func() { _ = local.SetReadDeadline(time.Time{}) }()
	for range 3 {
		cab := make([]byte, 8)
		if _, err := io.ReadFull(local, cab); err != nil {
			return nil, err
		}
		n, codigo := binary.BigEndian.Uint32(cab[0:4]), binary.BigEndian.Uint32(cab[4:8])
		if n == 8 && (codigo == codigoSSLRequest || codigo == codigoGSSENCRequest) {
			if _, err := local.Write([]byte{'N'}); err != nil {
				return nil, err
			}
			continue
		}
		if n < 8 || n > maxPacoteInicial {
			return nil, fmt.Errorf("pacote inicial inválido (%d bytes)", n)
		}
		pacote := make([]byte, n)
		copy(pacote, cab)
		if _, err := io.ReadFull(local, pacote[8:]); err != nil {
			return nil, err
		}
		return pacote, nil
	}
	return nil, errors.New("o cliente insistiu em pedir cifra ao túnel")
}

// devolver copia para o cliente o que vem do banco. Com o TLS no túnel, filtra antes a
// autenticação (semChannelBinding).
func devolver(local io.Writer, doBanco io.Reader, filtrar bool) error {
	if filtrar {
		return semChannelBinding(local, doBanco)
	}
	_, err := io.Copy(local, doBanco)
	return err
}

// maxMensagemInicio limita as mensagens do banco que o túnel lê inteiras, até o fim da
// autenticação. Nessa fase, elas são pequenas.
const maxMensagemInicio = 1 << 20

// semChannelBinding repassa as mensagens do banco até o fim da autenticação, tirando o
// SCRAM-SHA-256-PLUS da lista de mecanismos. Com TLS, o banco oferece o SCRAM com channel binding,
// que amarra a autenticação ao TLS do cliente. Aqui o TLS termina no túnel, e o cliente fala em
// claro com o socket: o libpq recusa a oferta ("server offered SCRAM-SHA-256-PLUS authentication
// over a non-SSL connection"). Sem o PLUS, ele autentica com o SCRAM-SHA-256. Depois da autenticação
// (ou de um erro), é cópia direta.
func semChannelBinding(local io.Writer, remoto io.Reader) error {
	br := bufio.NewReader(remoto)
	for {
		msg, err := lerMensagem(br)
		if err != nil {
			return err
		}
		cab, corpo := msg[:5], msg[5:]
		autenticacao := -1
		if cab[0] == 'R' && len(corpo) >= 4 {
			autenticacao = int(binary.BigEndian.Uint32(corpo[:4]))
		}
		if autenticacao == 10 { // AuthenticationSASL: a lista de mecanismos
			corpo = semPlus(corpo)
			binary.BigEndian.PutUint32(cab[1:5], uint32(4+len(corpo)))
		}
		// A mensagem sai montada numa fatia nova: o corpo pode ter encolhido.
		if _, err := local.Write(append(append([]byte(nil), cab...), corpo...)); err != nil {
			return err
		}
		if cab[0] == 'E' || autenticacao == 0 { // o erro ou o AuthenticationOk
			break
		}
	}
	_, err := io.Copy(local, br)
	return err
}

// semPlus tira o SCRAM-SHA-256-PLUS da AuthenticationSASL: o código 10 e os nomes, cada um com o
// seu zero, e um zero no fim.
func semPlus(corpo []byte) []byte {
	novo := append([]byte(nil), corpo[:4]...)
	for _, m := range strings.Split(string(corpo[4:]), "\x00") {
		if m != "" && m != "SCRAM-SHA-256-PLUS" {
			novo = append(append(novo, m...), 0)
		}
	}
	return append(novo, 0)
}

// responderErro manda ao cliente um ErrorResponse FATAL, como o servidor faria: o pg_dump e o pgx
// mostram a mensagem. Ela sai sem caracteres de controle: o texto de um certificado entra nela, e um
// NUL forjaria outros campos (o código do erro), e um ESC chegaria ao terminal.
func responderErro(local net.Conn, codigo, msg string) {
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, msg)
	var corpo []byte
	for _, c := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", codigo}, {"M", msg}} {
		corpo = append(corpo, c[0]...)
		corpo = append(corpo, c[1]...)
		corpo = append(corpo, 0)
	}
	corpo = append(corpo, 0)
	pacote := binary.BigEndian.AppendUint32([]byte{'E'}, uint32(4+len(corpo)))
	_ = local.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = local.Write(append(pacote, corpo...))
}

// --- chaves -----------------------------------------------------------------------------------

func metodos(c Config) ([]ssh.AuthMethod, func(), error) {
	var ms []ssh.AuthMethod
	fechar := func() {}
	if c.Chave != "" {
		dados, err := os.ReadFile(c.Chave)
		if err != nil {
			return nil, fechar, fmt.Errorf("lendo a chave %s: %w", c.Chave, err)
		}
		assinante, err := ssh.ParsePrivateKey(dados)
		var faltaFrase *ssh.PassphraseMissingError
		if errors.As(err, &faltaFrase) {
			if c.Frase == "" {
				return nil, fechar, &PrecisaFrase{Chave: c.Chave}
			}
			assinante, err = ssh.ParsePrivateKeyWithPassphrase(dados, []byte(c.Frase))
			if errors.Is(err, x509.IncorrectPasswordError) {
				return nil, fechar, &PrecisaFrase{Chave: c.Chave, Errada: true}
			}
		}
		if err != nil {
			return nil, fechar, fmt.Errorf("chave %s: %w", c.Chave, err)
		}
		ms = append(ms, ssh.PublicKeys(assinante))
	}
	// O ssh-agent entra depois da chave: uma chave com passphrase pode estar carregada nele.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ms = append(ms, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
			fechar = func() { _ = conn.Close() }
		}
	}
	if len(ms) == 0 {
		return nil, fechar, errors.New("nenhuma chave: gere a chave da ferramenta (aba Ambiente) ou informe o caminho de uma")
	}
	return ms, fechar, nil
}

// PrecisaDeFrase diz se a chave tem passphrase.
func PrecisaDeFrase(caminho string) (bool, error) {
	dados, err := os.ReadFile(caminho)
	if err != nil {
		return false, err
	}
	_, err = ssh.ParsePrivateKey(dados)
	var faltaFrase *ssh.PassphraseMissingError
	if errors.As(err, &faltaFrase) {
		return true, nil
	}
	return false, err
}

// GerarChave cria a chave ed25519 da ferramenta, sem passphrase: ela só abre o túnel, e quem a
// roubar ainda precisa da senha do banco (docs/ESTRATEGIA.md §4). Recusa sobrescrever.
func GerarChave(caminho, comentario string) error {
	if _, err := os.Stat(caminho); err == nil {
		return fmt.Errorf("%s já existe: a ferramenta não sobrescreve chaves", caminho)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	bloco, err := ssh.MarshalPrivateKey(priv, comentario)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(caminho), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(caminho, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, bloco); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	linha := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comentario + "\n"
	return os.WriteFile(caminho+".pub", []byte(linha), 0o600)
}

// ChavePublica lê a parte pública de uma chave privada (sem passphrase) ou do .pub ao lado.
func ChavePublica(caminho string) (string, error) {
	if b, err := os.ReadFile(caminho + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	dados, err := os.ReadFile(caminho)
	if err != nil {
		return "", err
	}
	s, err := ssh.ParsePrivateKey(dados)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))), nil
}

// LinhaAuthorizedKeys é a linha que o sysadmin põe no authorized_keys do usuário dedicado no
// servidor da produção: a chave só abre o túnel até o banco.
func LinhaAuthorizedKeys(chavePublica, destino string) string {
	return fmt.Sprintf(`restrict,port-forwarding,permitopen="%s",command="/bin/false" %s`, destino, chavePublica)
}

// --- known_hosts ------------------------------------------------------------------------------

func conferidor(arquivo string) (ssh.HostKeyCallback, error) {
	if arquivo == "" {
		return nil, errors.New("known_hosts não configurado")
	}
	if err := garantir(arquivo); err != nil {
		return nil, err
	}
	kh, err := knownhosts.New(arquivo)
	if err != nil {
		return nil, fmt.Errorf("lendo %s: %w", arquivo, err)
	}
	return func(nome string, remoto net.Addr, chave ssh.PublicKey) error {
		err := kh(nome, remoto, chave)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return &HostDesconhecido{Endereco: nome, Chave: chave, Fingerprint: ssh.FingerprintSHA256(chave)}
			}
			return ErrChaveMudou
		}
		return err
	}, nil
}

func garantir(arquivo string) error {
	f, err := os.OpenFile(arquivo, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Aceitar grava a chave do host no known_hosts da ferramenta, depois de o sysadmin conferir o
// fingerprint. Só grava host desconhecido: uma chave que mudou nunca é trocada por aqui.
func Aceitar(arquivo string, hd *HostDesconhecido) error {
	if err := garantir(arquivo); err != nil {
		return err
	}
	kh, err := knownhosts.New(arquivo)
	if err != nil {
		return err
	}
	remoto, _ := net.ResolveTCPAddr("tcp", hd.Endereco)
	if remoto == nil {
		remoto = &net.TCPAddr{}
	}
	var ke *knownhosts.KeyError
	if err := kh(hd.Endereco, remoto, hd.Chave); err == nil || !errors.As(err, &ke) || len(ke.Want) > 0 {
		return fmt.Errorf("%s já está no known_hosts: nada foi gravado", hd.Endereco)
	}
	f, err := os.OpenFile(arquivo, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hd.Endereco)}, hd.Chave))
	return err
}

// algoritmosConhecidos lê do known_hosts os tipos de chave do host. Sem isso, o Go negocia o tipo
// que prefere (ECDSA antes de Ed25519), e um host conhecido só pela Ed25519 pareceria ter mudado.
func algoritmosConhecidos(arquivo, endereco string) []string {
	f, err := os.Open(arquivo)
	if err != nil {
		return nil
	}
	defer f.Close()
	norm := knownhosts.Normalize(endereco)
	var algos []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		campos := strings.Fields(s.Text())
		if len(campos) < 3 || strings.HasPrefix(campos[0], "#") || strings.HasPrefix(campos[0], "@") {
			continue
		}
		achou := false
		for _, h := range strings.Split(campos[0], ",") {
			if h == norm {
				achou = true
			}
		}
		if !achou {
			continue
		}
		switch campos[1] {
		case ssh.KeyAlgoRSA:
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			algos = append(algos, campos[1])
		}
	}
	return algos
}
