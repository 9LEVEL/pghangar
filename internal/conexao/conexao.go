// Package conexao chega até um banco cadastrado (direto ou pelo túnel SSH), resolve a senha e faz o
// diagnóstico em camadas (docs/ESTRATEGIA.md §3): "sem resposta" não é "fora do ar", e cada falha
// diz em que camada parou.
package conexao

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgpassfile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
	"github.com/9LEVEL/copia-banco/internal/tunel"
)

// Ambiente são os arquivos da ferramenta que a conexão usa.
type Ambiente struct {
	KnownHosts  string
	ChavePadrao string // a chave da ferramenta, usada quando a conexão não diz outra
	Prazo       time.Duration
	// DirTemp é onde o túnel cria o diretório (700) do socket unix dele. Vazio, o túnel escuta
	// numa porta TCP de 127.0.0.1 (só os testes do túnel usam assim).
	DirTemp string
}

func (a Ambiente) prazo() time.Duration {
	if a.Prazo <= 0 {
		return 10 * time.Second
	}
	return a.Prazo
}

// Segredos são as senhas e passphrases informadas na hora (modo "perguntar"). Nunca vão para o
// disco: a tela os guarda na memória, e o processo da execução os recebe pela entrada padrão.
type Segredos struct {
	Senhas map[string]string `json:"senhas,omitempty"` // por conexão
	Frases map[string]string `json:"frases,omitempty"` // por caminho de chave
}

func (s *Segredos) GuardarSenha(conexao, senha string) {
	if s.Senhas == nil {
		s.Senhas = map[string]string{}
	}
	s.Senhas[conexao] = senha
}

func (s *Segredos) GuardarFrase(chave, frase string) {
	if s.Frases == nil {
		s.Frases = map[string]string{}
	}
	s.Frases[chave] = frase
}

// PrecisaSenha é a conexão no modo "perguntar" sem a senha informada.
type PrecisaSenha struct{ Conexao string }

func (e *PrecisaSenha) Error() string { return "informe a senha da conexão " + e.Conexao }

// Chave é o caminho da chave SSH que a conexão usa.
func Chave(c cadastro.Conexao, a Ambiente) string {
	if c.SSHChave != "" {
		return c.SSHChave
	}
	return a.ChavePadrao
}

// Senha resolve a senha do banco pelo modo da conexão.
func Senha(c cadastro.Conexao, s Segredos) (string, error) {
	switch c.ModoSenha {
	case cadastro.SenhaGuardar:
		return c.Senha, nil
	case cadastro.SenhaPerguntar:
		if v, ok := s.Senhas[c.Nome]; ok {
			return v, nil
		}
		return "", &PrecisaSenha{Conexao: c.Nome}
	case cadastro.SenhaPgpass:
		arq := os.Getenv("PGPASSFILE")
		if arq == "" {
			casa, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			arq = filepath.Join(casa, ".pgpass")
		}
		pf, err := pgpassfile.ReadPassfile(arq)
		if err != nil {
			return "", fmt.Errorf("lendo %s: %w", arq, err)
		}
		if v := pf.FindPassword(c.Host, strconv.Itoa(c.Porta), "*", c.Usuario); v != "" {
			return v, nil
		}
		// O pgpass pode ter uma linha por banco: a do banco administrativo serve para todos.
		if v := pf.FindPassword(c.Host, strconv.Itoa(c.Porta), c.BancoAdmin, c.Usuario); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("o %s não tem senha para %s:%d usuário %s", arq, c.Host, c.Porta, c.Usuario)
	}
	return "", fmt.Errorf("modo de senha desconhecido: %q", c.ModoSenha)
}

// Ponte é o caminho até o banco: o próprio host e porta, ou 127.0.0.1 e a porta do túnel.
type Ponte struct {
	Host  string
	Porta int
	Tunel *tunel.Tunel
	dir   string // o diretório do socket do túnel, apagado ao fechar
}

// Fechar fecha o túnel, se houver, e apaga o diretório do socket dele.
func (p *Ponte) Fechar() {
	if p == nil {
		return
	}
	if p.Tunel != nil {
		p.Tunel.Fechar()
	}
	if p.dir != "" {
		_ = os.RemoveAll(p.dir)
	}
}

// Socket diz se a ponte é um socket unix (o diretório vai montado no container).
func (p *Ponte) Socket() bool { return strings.HasPrefix(p.Host, "/") }

// ConfigTunel monta a configuração do túnel da conexão.
func ConfigTunel(c cadastro.Conexao, a Ambiente, s Segredos) tunel.Config {
	chave := Chave(c, a)
	cfg := tunel.Config{
		Host: c.SSHHost, Porta: c.SSHPorta, Usuario: c.SSHUsuario, Chave: chave, Frase: s.Frases[chave],
		KnownHosts: a.KnownHosts, Destino: net.JoinHostPort(c.Host, strconv.Itoa(c.Porta)), Prazo: a.prazo(),
	}
	if c.SaltoHost != "" {
		cfg.Salto = &tunel.Salto{Host: c.SaltoHost, Porta: c.SaltoPorta, Usuario: c.SaltoUsuario}
	}
	return cfg
}

// AbrirPonte abre o túnel quando a conexão pede um.
func AbrirPonte(ctx context.Context, c cadastro.Conexao, a Ambiente, s Segredos) (*Ponte, error) {
	if c.Acesso != cadastro.AcessoSSH {
		return &Ponte{Host: c.Host, Porta: c.Porta}, nil
	}
	return abrirTunel(ctx, c, a, s)
}

// PrefixoTunel é o começo do nome dos diretórios de socket dos túneis.
const PrefixoTunel = "tunel-"

// abrirTunel abre o túnel num socket unix, num diretório 700 só do root: ninguém mais no servidor
// chega na produção por ele. O container monta o diretório.
func abrirTunel(ctx context.Context, c cadastro.Conexao, a Ambiente, s Segredos) (*Ponte, error) {
	cfg := ConfigTunel(c, a, s)
	if a.DirTemp == "" {
		t, err := tunel.Abrir(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &Ponte{Host: "127.0.0.1", Porta: t.Porta(), Tunel: t}, nil
	}
	// O caminho de um socket unix cabe em 108 bytes: um diretório comprido demais usa o /tmp
	// (o MkdirTemp cria com 700 e nome imprevisível).
	base := a.DirTemp
	if len(base) > 60 {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, PrefixoTunel)
	if err != nil {
		return nil, err
	}
	const porta = 5432
	cfg.Socket = filepath.Join(dir, fmt.Sprintf(".s.PGSQL.%d", porta))
	t, err := tunel.Abrir(ctx, cfg)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Ponte{Host: dir, Porta: porta, Tunel: t, dir: dir}, nil
}

// DSN escreve a conexão no formato de palavras-chave do libpq, SEM senha: é o que vai na linha de
// comando do container. A senha vai pelo pgpass montado.
func DSN(c cadastro.Conexao, p *Ponte, banco, aplicacao string) string {
	par := func(k, v string) string { return k + "=" + valorDSN(v) }
	partes := []string{
		par("host", p.Host), par("port", strconv.Itoa(p.Porta)), par("user", c.Usuario),
		par("dbname", banco), par("sslmode", c.SSLMode),
	}
	if aplicacao != "" {
		partes = append(partes, par("application_name", aplicacao))
	}
	return strings.Join(partes, " ")
}

// valorDSN aspeia um valor do libpq: vazio, com espaço, aspas ou barra vai entre aspas simples,
// com \ e ' escapados.
func valorDSN(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n'\\=") {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// LinhaPgpass é a linha do pgpass para a ponte: host:porta:*:usuário:senha, com : e \ escapados.
func LinhaPgpass(c cadastro.Conexao, p *Ponte, senha string) string {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	return fmt.Sprintf("%s:%d:*:%s:%s", esc.Replace(p.Host), p.Porta, esc.Replace(c.Usuario), esc.Replace(senha))
}

// Conectar abre uma conexão pgx pela ponte.
func Conectar(ctx context.Context, c cadastro.Conexao, p *Ponte, senha, banco string, prazo time.Duration) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(DSN(c, p, banco, "copia-banco"))
	if err != nil {
		return nil, err
	}
	cfg.Password = senha
	if prazo <= 0 {
		prazo = 10 * time.Second
	}
	cfg.ConnectTimeout = prazo
	// Sem cache de comandos preparados: as consultas são poucas, e o catálogo muda entre elas
	// (bancos criados e renomeados).
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, explicarPG(c, banco, err)
	}
	return conn, nil
}

// FalhaPG é a falha na camada do Postgres, já explicada.
type FalhaPG struct {
	Codigo string
	Err    error
}

func (f *FalhaPG) Error() string { return f.Err.Error() }
func (f *FalhaPG) Unwrap() error { return f.Err }

func explicarPG(c cadastro.Conexao, banco string, err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		var msg string
		switch pe.Code {
		case "28P01":
			msg = fmt.Sprintf("senha errada para o usuário %s", c.Usuario)
		case "28000":
			msg = fmt.Sprintf("o servidor recusou a conexão (pg_hba.conf ou usuário inexistente): %s", pe.Message)
		case "3D000":
			msg = fmt.Sprintf("o banco %s não existe", banco)
		case "57P03":
			msg = "o servidor está iniciando, desligando ou em recuperação, e não aceita conexões agora"
		case "53300":
			msg = "o servidor está sem conexões livres (max_connections)"
		default:
			msg = fmt.Sprintf("%s (%s)", pe.Message, pe.Code)
		}
		return &FalhaPG{Codigo: pe.Code, Err: errors.New(msg)}
	}
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "timeout"):
		return &FalhaPG{Err: errors.New("o Postgres não respondeu a tempo")}
	case strings.Contains(s, "server refused TLS"):
		return &FalhaPG{Err: fmt.Errorf("o servidor não aceita TLS, e o sslmode é %s", c.SSLMode)}
	case strings.Contains(s, "connection refused"):
		return &FalhaPG{Err: errors.New("conexão recusada na porta do banco")}
	}
	return &FalhaPG{Err: err}
}
