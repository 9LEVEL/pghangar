// Package testessh é um sshd mínimo para os testes: aceita uma chave, responde keepalive e abre
// canais direct-tcpip (o túnel). Escuta só em 127.0.0.1.
package testessh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

type Servidor struct {
	Host  string
	Porta int

	ouvinte  net.Listener
	mu       sync.Mutex
	conexoes []net.Conn
	// Permitido, se não vazio, é o único destino que o servidor abre (como o permitopen).
	Permitido string
}

// Novo sobe o servidor aceitando só a chave autorizada.
func Novo(autorizada ssh.PublicKey) (*Servidor, error) {
	return NovoCom(func(k ssh.PublicKey) bool { return string(k.Marshal()) == string(autorizada.Marshal()) })
}

// NovoCom sobe o servidor aceitando as chaves que aceitar() aprovar (o palco lê o arquivo da chave
// a cada login, para a chave poder ser gerada depois, pela tela).
func NovoCom(aceitar func(ssh.PublicKey) bool) (*Servidor, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hk, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if aceitar(k) {
				return nil, nil
			}
			return nil, errors.New("chave recusada")
		},
	}
	cfg.AddHostKey(hk)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Servidor{Host: "127.0.0.1", Porta: l.Addr().(*net.TCPAddr).Port, ouvinte: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conexoes = append(s.conexoes, c)
			s.mu.Unlock()
			go s.atender(c, cfg)
		}
	}()
	return s, nil
}

// Parar fecha o servidor e derruba as conexões abertas (simula a queda do SSH).
func (s *Servidor) Parar() {
	_ = s.ouvinte.Close()
	s.Derrubar()
}

// Derrubar fecha as conexões abertas sem parar de aceitar novas.
func (s *Servidor) Derrubar() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conexoes {
		_ = c.Close()
	}
	s.conexoes = nil
}

func (s *Servidor) atender(c net.Conn, cfg *ssh.ServerConfig) {
	_, canais, pedidos, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go func() {
		for p := range pedidos {
			if p.WantReply {
				_ = p.Reply(p.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	for nc := range canais {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "só direct-tcpip")
			continue
		}
		var d struct {
			Host   string
			Porta  uint32
			OHost  string
			OPorta uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &d); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "payload")
			continue
		}
		alvoEnd := net.JoinHostPort(d.Host, strconv.Itoa(int(d.Porta)))
		if s.Permitido != "" && alvoEnd != s.Permitido {
			_ = nc.Reject(ssh.Prohibited, "fora do permitopen")
			continue
		}
		alvo, err := net.Dial("tcp", alvoEnd)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			_ = alvo.Close()
			continue
		}
		go ssh.DiscardRequests(reqs)
		go func() { _, _ = io.Copy(ch, alvo); _ = ch.Close() }()
		go func() { _, _ = io.Copy(alvo, ch); _ = alvo.Close() }()
	}
}
