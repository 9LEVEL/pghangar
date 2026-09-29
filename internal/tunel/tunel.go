// Package tunel abre o túnel SSH até o banco (docs/ESTRATEGIA.md §4): uma conexão SSH, uma porta
// local efêmera em 127.0.0.1 e, para cada conexão aceita nela, um canal até o banco visto a partir
// do servidor SSH. O container, com --network host, conecta nessa porta.
//
// O known_hosts é o da ferramenta e é estrito: um host desconhecido volta como *HostDesconhecido,
// com o fingerprint, para a tela perguntar; uma chave diferente da registrada bloqueia.
package tunel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
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
	t := &Tunel{cliente: cliente, ouvinte: ouvinte, destino: c.Destino, fechado: make(chan struct{})}
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
			remoto, err := t.cliente.Dial("tcp", t.destino)
			if err != nil {
				return
			}
			defer remoto.Close()
			fim := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remoto, local); fim <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remoto); fim <- struct{}{} }()
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
