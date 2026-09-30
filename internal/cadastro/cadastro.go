// Package cadastro guarda o que a ferramenta sabe: as conexões (com o que a última verificação
// viu), os perfis de cópia, as imagens baixadas, a configuração e o histórico das execuções. Fica
// num SQLite com permissão 600, dentro do diretório da ferramenta.
//
// As senhas das conexões ficam aqui (modo "guardar"). O arquivo usa secure_delete: uma senha
// trocada ou apagada não fica legível nas páginas livres.
package cadastro

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/9LEVEL/pghangar/internal/versoes"
)

// Tags de conexão. Um banco "prod" nunca é destino.
const (
	TagProd    = "prod"
	TagHomolog = "homolog"
	TagDev     = "dev"
)

var Tags = []string{TagDev, TagHomolog, TagProd}

// Acesso à conexão.
const (
	AcessoDireto = "direto"
	AcessoSSH    = "ssh"
)

var Acessos = []string{AcessoDireto, AcessoSSH}

// Modos da senha do banco.
const (
	SenhaGuardar   = "guardar"
	SenhaPerguntar = "perguntar"
	SenhaPgpass    = "pgpass"
)

var ModosSenha = []string{SenhaGuardar, SenhaPerguntar, SenhaPgpass}

var SSLModes = []string{"prefer", "disable", "require", "verify-ca", "verify-full"}

// Conexao é um servidor PostgreSQL. O banco é escolhido no perfil.
type Conexao struct {
	Nome   string
	Tag    string
	Acesso string

	// Com túnel: o servidor SSH. Host e Porta, abaixo, são então o banco visto a partir dele
	// (127.0.0.1:5432 quando o banco fica no mesmo host).
	SSHHost    string
	SSHPorta   int
	SSHUsuario string
	SSHChave   string // caminho da chave privada; vazio é a chave da ferramenta
	// Bastion (opcional): o SSH até o servidor passa por ele, com a mesma chave.
	SaltoHost    string
	SaltoPorta   int
	SaltoUsuario string

	Host       string
	Porta      int
	Usuario    string
	ModoSenha  string
	Senha      string // só no modo "guardar"
	SSLMode    string
	BancoAdmin string // o banco usado para as consultas do cluster

	Info Info
}

// Info é o que a última verificação viu. Gravada no cadastro na primeira conexão e relida a cada
// execução.
type Info struct {
	VerificadaEm time.Time     `json:"verificada_em"`
	Camada       string        `json:"camada,omitempty"` // onde parou; vazio quando chegou ao fim
	Erro         string        `json:"erro,omitempty"`
	VersaoNum    int           `json:"versao_num,omitempty"`
	Recuperacao  bool          `json:"recuperacao,omitempty"`
	SystemID     string        `json:"system_id,omitempty"`
	Superusuario bool          `json:"superusuario,omitempty"`
	Latencia     time.Duration `json:"latencia,omitempty"`
	Bancos       []Banco       `json:"bancos,omitempty"`
}

// Pronta diz se a última verificação chegou ao fim.
func (i Info) Pronta() bool { return !i.VerificadaEm.IsZero() && i.Erro == "" }

// Banco é um banco do servidor, como a verificação o viu.
type Banco struct {
	Nome        string `json:"nome"`
	Tamanho     int64  `json:"tamanho"`
	Dono        string `json:"dono"`
	Codificacao string `json:"codificacao"`
	Collate     string `json:"collate"`
	Ctype       string `json:"ctype"`
	Provedor    string `json:"provedor"` // c (libc), i (icu), b (builtin)
	Locale      string `json:"locale,omitempty"`
	RegrasICU   string `json:"regras_icu,omitempty"`
	Conexoes    bool   `json:"conexoes"` // datallowconn
}

func (c Conexao) Endereco() string {
	if strings.HasPrefix(c.Host, "/") {
		return fmt.Sprintf("%s (socket, porta %d)", c.Host, c.Porta)
	}
	return fmt.Sprintf("%s:%d", c.Host, c.Porta)
}

// Onde é o endereço para mostrar, com o túnel quando houver.
func (c Conexao) Onde() string {
	if c.Acesso == AcessoSSH {
		s := fmt.Sprintf("%s@%s:%d → %s", c.SSHUsuario, c.SSHHost, c.SSHPorta, c.Endereco())
		if c.SaltoHost != "" {
			s = fmt.Sprintf("%s@%s:%d → ", c.SaltoUsuario, c.SaltoHost, c.SaltoPorta) + s
		}
		return s
	}
	return c.Endereco()
}

// Perfil é uma cópia de um banco: a origem, o destino e as opções.
type Perfil struct {
	Nome         string
	Origem       string // nome da conexão
	OrigemBanco  string
	Destino      string // nome da conexão
	DestinoBanco string
	JobsDump     int
	JobsRestore  int
	SemDados     []string // padrões de tabelas que vêm só com a estrutura
	Script       string   // caminho de um .sql rodado no __novo antes da troca
	DirDumps     string   // vazio é o padrão

	Compressao  string   // do dump: zstd (padrão), lz4, gzip ou nenhuma
	Schemas     []string // só estes schemas (pg_dump -n); vazio é todos
	SchemasFora []string // sem estes schemas (-N)
	Tabelas     []string // só estas tabelas (-t)
	TabelasFora []string // sem estas tabelas (-T)

	ConferirLinhas bool // conta as linhas de cada tabela, no mesmo snapshot do dump
	Retomavel      bool // modo "link instável": dados em blocos por chave, retomáveis
	GuardarBase    bool // guarda <banco>__base para resetar o destino sem ir à origem
}

// Compressões aceitas no dump (as imagens são todas 16+, que têm zstd e lz4).
var Compressoes = []string{"zstd", "lz4", "gzip", "nenhuma"}

// Filtrado diz se o perfil copia só parte do banco.
func (p Perfil) Filtrado() bool {
	return len(p.Schemas)+len(p.SchemasFora)+len(p.Tabelas)+len(p.TabelasFora) > 0
}

// Imagem é a imagem de uma versão, travada pelo digest.
type Imagem struct {
	Versao     int
	Referencia string // a tag baixada (postgres:18)
	Digest     string // postgres@sha256:…; é o que as execuções usam
	Cliente    string // a versão exata do pg_dump de dentro dela
	BaixadaEm  time.Time
}

// Config é a configuração geral.
type Config struct {
	Repositorio string // a imagem sem a tag: postgres, ou um espelho (registry.interna/postgres)
	Versoes     []int
	Webhook     string // aviso ao terminar: um POST em JSON (compatível com Slack, Mattermost, ntfy…)
}

// Instancia identifica esta instalação da ferramenta (6 hex aleatórios, gerados uma vez). Vai no
// nome da role temporária: duas instalações apontando para o mesmo servidor não se confundem.
func (c *Cadastro) Instancia(ctx context.Context) (string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, `SELECT valor FROM config WHERE chave = 'instancia'`).Scan(&v)
	if err == nil && v != "" {
		return v, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v = hex.EncodeToString(b)
	if _, err := c.db.ExecContext(ctx, `INSERT INTO config (chave, valor) VALUES ('instancia', ?) ON CONFLICT (chave) DO NOTHING`, v); err != nil {
		return "", err
	}
	err = c.db.QueryRowContext(ctx, `SELECT valor FROM config WHERE chave = 'instancia'`).Scan(&v)
	return v, err
}

// Estados de uma execução.
const (
	EstadoIniciando    = "iniciando"
	EstadoRodando      = "rodando"
	EstadoAguardando   = "aguardando" // o restore terminou com erro, ou a conferência divergiu: a troca espera o sysadmin
	EstadoOK           = "ok"
	EstadoErro         = "erro"
	EstadoCancelada    = "cancelada"
	EstadoInterrompida = "interrompida" // o processo morreu sem registrar o fim
	EstadoFila         = "na fila"      // de um grupo: espera a anterior terminar
)

// Tipos de execução.
const (
	TipoCopia       = "copia"
	TipoTroca       = "troca"       // só a troca, depois de o sysadmin decidir
	TipoRestauracao = "restauracao" // um dump guardado, restaurado de novo, sem ir à origem
	TipoReset       = "reset"       // o destino recriado a partir do <banco>__base
)

// Execucao é uma cópia, do começo ao fim, com o progresso de agora.
type Execucao struct {
	ID     int64
	Perfil string
	Tipo   string
	Estado string

	Etapa       string
	EtapaNum    int
	EtapasTotal int
	Feito       int
	Total       int
	Item        string

	Inicio       time.Time
	Fim          time.Time
	AtualizadaEm time.Time
	PID          int

	Destino       string // a conexão de destino
	Banco         string // o banco de destino
	DumpDir       string
	BancoNovo     string
	NovoOID       uint32 // o oid do __novo criado por esta execução: a troca adiada confere que é o mesmo
	BancoAnterior string
	ErrosRestore  int
	TamanhoDump   int64
	Mensagem      string
	Avisos        []string // o que pede atenção
	Notas         []string // o que vale saber, sem pedir atenção
	Apagar        []string // anteriores que o sysadmin marcou para apagar antes de começar
	Plano         string   // o plano confirmado, em JSON
	Operador      string   // quem confirmou: o usuário por trás do sudo, e de onde veio o SSH
	Grupo         string   // execuções de um grupo rodam em fila, uma de cada vez
}

// Terminou diz se a execução não vai mais mudar sozinha.
func (e Execucao) Terminou() bool {
	switch e.Estado {
	case EstadoIniciando, EstadoRodando, EstadoFila:
		return false
	}
	return true
}

type Cadastro struct{ db *sql.DB }

func Abrir(caminho string) (*Cadastro, error) {
	if caminho == "" {
		return nil, errors.New("caminho do cadastro vazio")
	}
	// Cria o arquivo com 600 antes do SQLite, para ele nunca existir com a umask do processo.
	f, err := os.OpenFile(caminho, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", caminho+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	c := &Cadastro{db: db}
	if err := c.criar(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, s := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(caminho+s, 0o600)
	}
	return c, nil
}

func (c *Cadastro) Fechar() error { return c.db.Close() }

func (c *Cadastro) criar(ctx context.Context) error {
	const ddl = `
		CREATE TABLE IF NOT EXISTS conexoes (
			nome        TEXT PRIMARY KEY,
			tag         TEXT NOT NULL CHECK (tag IN ('prod', 'homolog', 'dev')),
			acesso      TEXT NOT NULL CHECK (acesso IN ('direto', 'ssh')),
			ssh_host    TEXT NOT NULL DEFAULT '',
			ssh_porta   INTEGER NOT NULL DEFAULT 22,
			ssh_usuario TEXT NOT NULL DEFAULT '',
			ssh_chave   TEXT NOT NULL DEFAULT '',
			host        TEXT NOT NULL,
			porta       INTEGER NOT NULL DEFAULT 5432,
			usuario     TEXT NOT NULL,
			modo_senha  TEXT NOT NULL CHECK (modo_senha IN ('guardar', 'perguntar', 'pgpass')),
			senha       TEXT NOT NULL DEFAULT '',
			sslmode     TEXT NOT NULL DEFAULT 'prefer',
			banco_admin TEXT NOT NULL DEFAULT 'postgres',
			info        TEXT NOT NULL DEFAULT '{}'
		);
		CREATE TABLE IF NOT EXISTS perfis (
			nome          TEXT PRIMARY KEY,
			origem        TEXT NOT NULL REFERENCES conexoes(nome) ON UPDATE CASCADE,
			origem_banco  TEXT NOT NULL,
			destino       TEXT NOT NULL REFERENCES conexoes(nome) ON UPDATE CASCADE,
			destino_banco TEXT NOT NULL,
			jobs_dump     INTEGER NOT NULL DEFAULT 2,
			jobs_restore  INTEGER NOT NULL DEFAULT 4,
			sem_dados     TEXT NOT NULL DEFAULT '[]',
			script        TEXT NOT NULL DEFAULT '',
			dir_dumps     TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS imagens (
			versao     INTEGER PRIMARY KEY,
			referencia TEXT NOT NULL,
			digest     TEXT NOT NULL,
			cliente    TEXT NOT NULL,
			baixada_em TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS config (
			chave TEXT PRIMARY KEY,
			valor TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS execucoes (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			perfil         TEXT NOT NULL,
			tipo           TEXT NOT NULL,
			estado         TEXT NOT NULL,
			etapa          TEXT NOT NULL DEFAULT '',
			etapa_num      INTEGER NOT NULL DEFAULT 0,
			etapas_total   INTEGER NOT NULL DEFAULT 0,
			feito          INTEGER NOT NULL DEFAULT 0,
			total          INTEGER NOT NULL DEFAULT 0,
			item           TEXT NOT NULL DEFAULT '',
			inicio         TEXT NOT NULL,
			fim            TEXT NOT NULL DEFAULT '',
			atualizada_em  TEXT NOT NULL,
			pid            INTEGER NOT NULL DEFAULT 0,
			destino        TEXT NOT NULL DEFAULT '',
			banco          TEXT NOT NULL DEFAULT '',
			dump_dir       TEXT NOT NULL DEFAULT '',
			banco_novo     TEXT NOT NULL DEFAULT '',
			banco_anterior TEXT NOT NULL DEFAULT '',
			erros_restore  INTEGER NOT NULL DEFAULT 0,
			tamanho_dump   INTEGER NOT NULL DEFAULT 0,
			mensagem       TEXT NOT NULL DEFAULT '',
			avisos         TEXT NOT NULL DEFAULT '[]',
			apagar         TEXT NOT NULL DEFAULT '[]',
			plano          TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS execucoes_perfil ON execucoes (perfil, id);`
	if _, err := c.db.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS destinos_aprovados (
			system_id    TEXT PRIMARY KEY,
			conexao      TEXT NOT NULL,
			aprovado_em  TEXT NOT NULL,
			aprovado_por TEXT NOT NULL
		)`); err != nil {
		return err
	}
	// Colunas que vieram depois da primeira versão do cadastro.
	for _, c2 := range [][3]string{
		{"conexoes", "salto_host", "TEXT NOT NULL DEFAULT ''"},
		{"conexoes", "salto_porta", "INTEGER NOT NULL DEFAULT 22"},
		{"conexoes", "salto_usuario", "TEXT NOT NULL DEFAULT ''"},
		{"execucoes", "novo_oid", "INTEGER NOT NULL DEFAULT 0"},
		{"execucoes", "operador", "TEXT NOT NULL DEFAULT ''"},
		{"execucoes", "grupo", "TEXT NOT NULL DEFAULT ''"},
		{"execucoes", "notas", "TEXT NOT NULL DEFAULT '[]'"},
		{"perfis", "compressao", "TEXT NOT NULL DEFAULT 'zstd'"},
		{"perfis", "schemas", "TEXT NOT NULL DEFAULT '[]'"},
		{"perfis", "schemas_fora", "TEXT NOT NULL DEFAULT '[]'"},
		{"perfis", "tabelas", "TEXT NOT NULL DEFAULT '[]'"},
		{"perfis", "tabelas_fora", "TEXT NOT NULL DEFAULT '[]'"},
		{"perfis", "conferir_linhas", "INTEGER NOT NULL DEFAULT 0"},
		{"perfis", "retomavel", "INTEGER NOT NULL DEFAULT 0"},
		{"perfis", "guardar_base", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := c.garantirColuna(ctx, c2[0], c2[1], c2[2]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cadastro) garantirColuna(ctx context.Context, tabela, coluna, tipo string) error {
	rows, err := c.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, tabela)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		if n == coluna {
			return nil
		}
	}
	rows.Close()
	_, err = c.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tabela, coluna, tipo))
	return err
}

// --- conexões ---------------------------------------------------------------------------------

const colunasConexao = `nome, tag, acesso, ssh_host, ssh_porta, ssh_usuario, ssh_chave, host, porta, usuario,
	modo_senha, senha, sslmode, banco_admin, info, salto_host, salto_porta, salto_usuario`

type linha interface{ Scan(...any) error }

func lerConexao(l linha) (Conexao, error) {
	var x Conexao
	var info string
	err := l.Scan(&x.Nome, &x.Tag, &x.Acesso, &x.SSHHost, &x.SSHPorta, &x.SSHUsuario, &x.SSHChave, &x.Host, &x.Porta,
		&x.Usuario, &x.ModoSenha, &x.Senha, &x.SSLMode, &x.BancoAdmin, &info, &x.SaltoHost, &x.SaltoPorta, &x.SaltoUsuario)
	if err != nil {
		return x, err
	}
	if err := json.Unmarshal([]byte(info), &x.Info); err != nil {
		return x, fmt.Errorf("conexão %s: info ilegível: %w", x.Nome, err)
	}
	return x, nil
}

func (c *Cadastro) Conexoes(ctx context.Context) ([]Conexao, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+colunasConexao+` FROM conexoes ORDER BY nome`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cs []Conexao
	for rows.Next() {
		x, err := lerConexao(rows)
		if err != nil {
			return nil, err
		}
		cs = append(cs, x)
	}
	return cs, rows.Err()
}

var ErrNaoExiste = errors.New("não existe")

func (c *Cadastro) Conexao(ctx context.Context, nome string) (Conexao, error) {
	x, err := lerConexao(c.db.QueryRowContext(ctx, `SELECT `+colunasConexao+` FROM conexoes WHERE nome = ?`, nome))
	if errors.Is(err, sql.ErrNoRows) {
		return x, fmt.Errorf("conexão %q: %w", nome, ErrNaoExiste)
	}
	return x, err
}

// Validar confere os campos antes de gravar.
func (x Conexao) Validar() error {
	switch {
	case strings.TrimSpace(x.Nome) == "":
		return errors.New("o nome é obrigatório")
	case !contem(Tags, x.Tag):
		return fmt.Errorf("tag inválida: %q", x.Tag)
	case !contem(Acessos, x.Acesso):
		return fmt.Errorf("acesso inválido: %q", x.Acesso)
	case strings.TrimSpace(x.Host) == "":
		return errors.New("o host do banco é obrigatório")
	case x.Porta <= 0 || x.Porta > 65535:
		return errors.New("porta do banco inválida")
	case strings.TrimSpace(x.Usuario) == "":
		return errors.New("o usuário do banco é obrigatório")
	case !contem(ModosSenha, x.ModoSenha):
		return fmt.Errorf("modo de senha inválido: %q", x.ModoSenha)
	case !contem(SSLModes, x.SSLMode):
		return fmt.Errorf("sslmode inválido: %q", x.SSLMode)
	case strings.TrimSpace(x.BancoAdmin) == "":
		return errors.New("o banco administrativo é obrigatório (normalmente postgres)")
	}
	if x.Acesso == AcessoSSH {
		switch {
		case strings.TrimSpace(x.SSHHost) == "":
			return errors.New("com túnel, o host SSH é obrigatório")
		case x.SSHPorta <= 0 || x.SSHPorta > 65535:
			return errors.New("porta SSH inválida")
		case strings.TrimSpace(x.SSHUsuario) == "":
			return errors.New("com túnel, o usuário SSH é obrigatório")
		case strings.HasPrefix(x.Host, "/"):
			return errors.New("com túnel, o banco precisa de host e porta TCP (o socket não atravessa o túnel)")
		case x.SaltoHost != "" && (x.SaltoPorta <= 0 || x.SaltoPorta > 65535 || strings.TrimSpace(x.SaltoUsuario) == ""):
			return errors.New("com bastion, informe a porta e o usuário dele")
		}
	}
	return nil
}

// SalvarConexao grava uma conexão nova ou alterada. Trocar o nome leva junto os perfis que a
// usam. A senha vazia no modo "guardar" mantém a gravada; os outros modos não guardam senha.
func (c *Cadastro) SalvarConexao(ctx context.Context, antigo string, x Conexao) error {
	if err := x.Validar(); err != nil {
		return err
	}
	info, err := json.Marshal(x.Info)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if antigo != "" {
		var senha string
		err := tx.QueryRowContext(ctx, `SELECT senha FROM conexoes WHERE nome = ?`, antigo).Scan(&senha)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("conexão %q: %w", antigo, ErrNaoExiste)
		}
		if err != nil {
			return err
		}
		if x.ModoSenha == SenhaGuardar && x.Senha == "" {
			x.Senha = senha
		}
	}
	if x.ModoSenha != SenhaGuardar {
		x.Senha = ""
	}
	if antigo != "" && antigo != x.Nome {
		if _, err := tx.ExecContext(ctx, `UPDATE conexoes SET nome = ? WHERE nome = ?`, x.Nome, antigo); err != nil {
			return traduzir(err, "já existe uma conexão com o nome "+x.Nome)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO conexoes (`+colunasConexao+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (nome) DO UPDATE SET tag = excluded.tag, acesso = excluded.acesso, ssh_host = excluded.ssh_host,
			ssh_porta = excluded.ssh_porta, ssh_usuario = excluded.ssh_usuario, ssh_chave = excluded.ssh_chave,
			host = excluded.host, porta = excluded.porta, usuario = excluded.usuario, modo_senha = excluded.modo_senha,
			senha = excluded.senha, sslmode = excluded.sslmode, banco_admin = excluded.banco_admin, info = excluded.info,
			salto_host = excluded.salto_host, salto_porta = excluded.salto_porta, salto_usuario = excluded.salto_usuario`,
		x.Nome, x.Tag, x.Acesso, x.SSHHost, x.SSHPorta, x.SSHUsuario, x.SSHChave, x.Host, x.Porta, x.Usuario,
		x.ModoSenha, x.Senha, x.SSLMode, x.BancoAdmin, string(info), x.SaltoHost, x.SaltoPorta, x.SaltoUsuario)
	if err != nil {
		return err
	}
	// Uma conexão que virou prod não pode continuar como destino de um perfil.
	if x.Tag == TagProd {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM perfis WHERE destino = ?`, x.Nome).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("a conexão %s é destino de %d perfil(is): um banco prod nunca é destino", x.Nome, n)
		}
	}
	return tx.Commit()
}

// GravarInfo guarda o que a verificação viu, sem tocar no resto da conexão.
func (c *Cadastro) GravarInfo(ctx context.Context, nome string, i Info) error {
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE conexoes SET info = ? WHERE nome = ?`, string(b), nome)
	return err
}

func (c *Cadastro) RemoverConexao(ctx context.Context, nome string) error {
	var perfis []string
	rows, err := c.db.QueryContext(ctx, `SELECT nome FROM perfis WHERE origem = ?1 OR destino = ?1 ORDER BY nome`, nome)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		perfis = append(perfis, p)
	}
	rows.Close()
	if len(perfis) > 0 {
		return fmt.Errorf("a conexão %s é usada pelos perfis: %s", nome, strings.Join(perfis, ", "))
	}
	r, err := c.db.ExecContext(ctx, `DELETE FROM conexoes WHERE nome = ?`, nome)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("conexão %q: %w", nome, ErrNaoExiste)
	}
	return nil
}

// --- perfis -----------------------------------------------------------------------------------

const colunasPerfil = `nome, origem, origem_banco, destino, destino_banco, jobs_dump, jobs_restore, sem_dados, script, dir_dumps,
	compressao, schemas, schemas_fora, tabelas, tabelas_fora, conferir_linhas, retomavel, guardar_base`

func lerPerfil(l linha) (Perfil, error) {
	var p Perfil
	var sem, sch, schF, tab, tabF string
	if err := l.Scan(&p.Nome, &p.Origem, &p.OrigemBanco, &p.Destino, &p.DestinoBanco, &p.JobsDump, &p.JobsRestore, &sem, &p.Script, &p.DirDumps,
		&p.Compressao, &sch, &schF, &tab, &tabF, &p.ConferirLinhas, &p.Retomavel, &p.GuardarBase); err != nil {
		return p, err
	}
	for _, x := range []struct {
		s string
		d *[]string
	}{{sem, &p.SemDados}, {sch, &p.Schemas}, {schF, &p.SchemasFora}, {tab, &p.Tabelas}, {tabF, &p.TabelasFora}} {
		if err := json.Unmarshal([]byte(x.s), x.d); err != nil {
			return p, fmt.Errorf("perfil %s: lista ilegível: %w", p.Nome, err)
		}
	}
	return p, nil
}

func (c *Cadastro) Perfis(ctx context.Context) ([]Perfil, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+colunasPerfil+` FROM perfis ORDER BY nome`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Perfil
	for rows.Next() {
		p, err := lerPerfil(rows)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

func (c *Cadastro) Perfil(ctx context.Context, nome string) (Perfil, error) {
	p, err := lerPerfil(c.db.QueryRowContext(ctx, `SELECT `+colunasPerfil+` FROM perfis WHERE nome = ?`, nome))
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("perfil %q: %w", nome, ErrNaoExiste)
	}
	return p, err
}

func (p Perfil) Validar() error {
	switch {
	case strings.TrimSpace(p.Nome) == "":
		return errors.New("o nome é obrigatório")
	case p.Origem == "" || p.Destino == "":
		return errors.New("escolha a origem e o destino")
	case strings.TrimSpace(p.OrigemBanco) == "" || strings.TrimSpace(p.DestinoBanco) == "":
		return errors.New("informe o banco de origem e o de destino")
	case p.JobsDump < 1 || p.JobsDump > 32 || p.JobsRestore < 1 || p.JobsRestore > 32:
		return errors.New("os jobs vão de 1 a 32")
	case p.Origem == p.Destino && p.OrigemBanco == p.DestinoBanco:
		return errors.New("a origem e o destino são o mesmo banco")
	}
	if p.Script != "" && !strings.HasPrefix(p.Script, "/") {
		return errors.New("o script pós-restore precisa de caminho absoluto")
	}
	if p.DirDumps != "" && !strings.HasPrefix(p.DirDumps, "/") {
		return errors.New("o diretório dos dumps precisa de caminho absoluto")
	}
	if p.Compressao != "" && !contem(Compressoes, p.Compressao) {
		return fmt.Errorf("compressão inválida: %q", p.Compressao)
	}
	if p.Retomavel && p.Filtrado() && len(p.Tabelas) > 0 {
		return errors.New("o modo link instável não aceita a lista de tabelas: use a de schemas")
	}
	return nil
}

// normalizar tira os itens vazios das listas (um campo vazio vira [""] no strings.Split).
func (p *Perfil) normalizar() {
	p.SemDados, p.Schemas, p.SchemasFora = limpar(p.SemDados), limpar(p.Schemas), limpar(p.SchemasFora)
	p.Tabelas, p.TabelasFora = limpar(p.Tabelas), limpar(p.TabelasFora)
}

// SalvarPerfil grava um perfil novo ou alterado, e recusa um destino prod.
func (c *Cadastro) SalvarPerfil(ctx context.Context, antigo string, p Perfil) error {
	p.normalizar()
	if err := p.Validar(); err != nil {
		return err
	}
	dest, err := c.Conexao(ctx, p.Destino)
	if err != nil {
		return err
	}
	if dest.Tag == TagProd {
		return fmt.Errorf("a conexão %s é prod: um banco prod nunca é destino", dest.Nome)
	}
	if _, err := c.Conexao(ctx, p.Origem); err != nil {
		return err
	}
	if p.Compressao == "" {
		p.Compressao = "zstd"
	}
	sem, err := json.Marshal(limpar(p.SemDados))
	if err != nil {
		return err
	}
	lista := func(ss []string) string { b, _ := json.Marshal(limpar(ss)); return string(b) }
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if antigo != "" && antigo != p.Nome {
		r, err := tx.ExecContext(ctx, `UPDATE perfis SET nome = ? WHERE nome = ?`, p.Nome, antigo)
		if err != nil {
			return traduzir(err, "já existe um perfil com o nome "+p.Nome)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return fmt.Errorf("perfil %q: %w", antigo, ErrNaoExiste)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execucoes SET perfil = ? WHERE perfil = ?`, p.Nome, antigo); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO perfis (`+colunasPerfil+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (nome) DO UPDATE SET origem = excluded.origem, origem_banco = excluded.origem_banco,
			destino = excluded.destino, destino_banco = excluded.destino_banco, jobs_dump = excluded.jobs_dump,
			jobs_restore = excluded.jobs_restore, sem_dados = excluded.sem_dados, script = excluded.script,
			dir_dumps = excluded.dir_dumps, compressao = excluded.compressao, schemas = excluded.schemas,
			schemas_fora = excluded.schemas_fora, tabelas = excluded.tabelas, tabelas_fora = excluded.tabelas_fora,
			conferir_linhas = excluded.conferir_linhas, retomavel = excluded.retomavel, guardar_base = excluded.guardar_base`,
		p.Nome, p.Origem, p.OrigemBanco, p.Destino, p.DestinoBanco, p.JobsDump, p.JobsRestore, string(sem), p.Script, p.DirDumps,
		p.Compressao, lista(p.Schemas), lista(p.SchemasFora), lista(p.Tabelas), lista(p.TabelasFora), p.ConferirLinhas, p.Retomavel, p.GuardarBase)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cadastro) RemoverPerfil(ctx context.Context, nome string) error {
	r, err := c.db.ExecContext(ctx, `DELETE FROM perfis WHERE nome = ?`, nome)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("perfil %q: %w", nome, ErrNaoExiste)
	}
	return nil
}

// --- imagens e configuração -------------------------------------------------------------------

func (c *Cadastro) Imagens(ctx context.Context) (map[int]Imagem, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT versao, referencia, digest, cliente, baixada_em FROM imagens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	is := map[int]Imagem{}
	for rows.Next() {
		var i Imagem
		var em string
		if err := rows.Scan(&i.Versao, &i.Referencia, &i.Digest, &i.Cliente, &em); err != nil {
			return nil, err
		}
		i.BaixadaEm = hora(em)
		is[i.Versao] = i
	}
	return is, rows.Err()
}

func (c *Cadastro) SalvarImagem(ctx context.Context, i Imagem) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO imagens (versao, referencia, digest, cliente, baixada_em) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (versao) DO UPDATE SET referencia = excluded.referencia, digest = excluded.digest,
			cliente = excluded.cliente, baixada_em = excluded.baixada_em`,
		i.Versao, i.Referencia, i.Digest, i.Cliente, texto(i.BaixadaEm))
	return err
}

func (c *Cadastro) Config(ctx context.Context) (Config, error) {
	cfg := Config{Repositorio: "postgres", Versoes: append([]int(nil), versoes.Padrao...)}
	rows, err := c.db.QueryContext(ctx, `SELECT chave, valor FROM config`)
	if err != nil {
		return cfg, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return cfg, err
		}
		switch k {
		case "repositorio":
			cfg.Repositorio = v
		case "versoes":
			vs, err := versoes.LerLista(v)
			if err != nil {
				return cfg, fmt.Errorf("config versoes: %w", err)
			}
			cfg.Versoes = vs
		case "webhook":
			cfg.Webhook = v
		}
	}
	return cfg, rows.Err()
}

func (c *Cadastro) SalvarConfig(ctx context.Context, cfg Config) error {
	if err := ValidarRepositorio(cfg.Repositorio); err != nil {
		return err
	}
	if len(cfg.Versoes) == 0 {
		return errors.New("informe ao menos uma versão")
	}
	if cfg.Webhook != "" && !strings.HasPrefix(cfg.Webhook, "https://") && !strings.HasPrefix(cfg.Webhook, "http://") {
		return errors.New("o webhook precisa começar com https:// (ou http://)")
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range map[string]string{"repositorio": cfg.Repositorio, "versoes": versoes.Lista(cfg.Versoes), "webhook": cfg.Webhook} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO config (chave, valor) VALUES (?, ?) ON CONFLICT (chave) DO UPDATE SET valor = excluded.valor`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DestinoAprovado é um servidor que o sysadmin aprovou, uma vez, como destino de cópias.
type DestinoAprovado struct {
	SystemID    string
	Conexao     string // a conexão pela qual foi aprovado (só para mostrar)
	AprovadoEm  time.Time
	AprovadoPor string
}

// AprovarDestino registra o servidor (pelo system_identifier) como destino permitido.
func (c *Cadastro) AprovarDestino(ctx context.Context, systemID, conexao, quem string) error {
	if strings.TrimSpace(systemID) == "" {
		return errors.New("sem system_identifier: teste a conexão antes de aprovar")
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO destinos_aprovados (system_id, conexao, aprovado_em, aprovado_por) VALUES (?, ?, ?, ?)
		ON CONFLICT (system_id) DO UPDATE SET conexao = excluded.conexao, aprovado_em = excluded.aprovado_em, aprovado_por = excluded.aprovado_por`,
		systemID, conexao, texto(time.Now()), quem)
	return err
}

// RevogarDestino tira o servidor da lista.
func (c *Cadastro) RevogarDestino(ctx context.Context, systemID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM destinos_aprovados WHERE system_id = ?`, systemID)
	return err
}

// DestinosAprovados são os servidores aprovados, pelo system_identifier.
func (c *Cadastro) DestinosAprovados(ctx context.Context) (map[string]DestinoAprovado, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT system_id, conexao, aprovado_em, aprovado_por FROM destinos_aprovados`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r := map[string]DestinoAprovado{}
	for rows.Next() {
		var d DestinoAprovado
		var em string
		if err := rows.Scan(&d.SystemID, &d.Conexao, &em, &d.AprovadoPor); err != nil {
			return nil, err
		}
		d.AprovadoEm = hora(em)
		r[d.SystemID] = d
	}
	return r, rows.Err()
}

// ValidarRepositorio aceita o nome da imagem sem tag e sem digest: postgres, ou um espelho
// (registry.interna:5000/postgres). A tag é a versão, e quem a põe é a ferramenta.
func ValidarRepositorio(r string) error {
	ultimo := r[strings.LastIndex(r, "/")+1:]
	if strings.TrimSpace(r) == "" || strings.ContainsAny(r, " \t@") || strings.Contains(ultimo, ":") || ultimo == "" {
		return errors.New("repositório inválido: use o nome da imagem sem tag nem digest (postgres, ou registry.interna/postgres)")
	}
	return nil
}

// --- execuções --------------------------------------------------------------------------------

const colunasExecucao = `id, perfil, tipo, estado, etapa, etapa_num, etapas_total, feito, total, item, inicio, fim,
	atualizada_em, pid, destino, banco, dump_dir, banco_novo, banco_anterior, erros_restore, tamanho_dump, mensagem,
	avisos, apagar, plano, novo_oid, operador, grupo, notas`

func lerExecucao(l linha) (Execucao, error) {
	var e Execucao
	var ini, fim, atu, av, ap, nt string
	err := l.Scan(&e.ID, &e.Perfil, &e.Tipo, &e.Estado, &e.Etapa, &e.EtapaNum, &e.EtapasTotal, &e.Feito, &e.Total, &e.Item,
		&ini, &fim, &atu, &e.PID, &e.Destino, &e.Banco, &e.DumpDir, &e.BancoNovo, &e.BancoAnterior, &e.ErrosRestore,
		&e.TamanhoDump, &e.Mensagem, &av, &ap, &e.Plano, &e.NovoOID, &e.Operador, &e.Grupo, &nt)
	if err != nil {
		return e, err
	}
	e.Inicio, e.Fim, e.AtualizadaEm = hora(ini), hora(fim), hora(atu)
	_ = json.Unmarshal([]byte(av), &e.Avisos)
	_ = json.Unmarshal([]byte(ap), &e.Apagar)
	_ = json.Unmarshal([]byte(nt), &e.Notas)
	return e, nil
}

// NovaExecucao registra uma execução e devolve o id.
func (c *Cadastro) NovaExecucao(ctx context.Context, e Execucao) (int64, error) {
	agora := time.Now()
	if e.Inicio.IsZero() {
		e.Inicio = agora
	}
	av, _ := json.Marshal(nulo(e.Avisos))
	ap, _ := json.Marshal(nulo(e.Apagar))
	nt, _ := json.Marshal(nulo(e.Notas))
	r, err := c.db.ExecContext(ctx, `
		INSERT INTO execucoes (perfil, tipo, estado, inicio, atualizada_em, destino, banco, dump_dir, banco_novo,
			banco_anterior, mensagem, avisos, apagar, plano, operador, grupo, notas)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Perfil, e.Tipo, e.Estado, texto(e.Inicio), texto(agora), e.Destino, e.Banco, e.DumpDir, e.BancoNovo,
		e.BancoAnterior, e.Mensagem, string(av), string(ap), e.Plano, e.Operador, e.Grupo, string(nt))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// GravarExecucao regrava a execução inteira.
func (c *Cadastro) GravarExecucao(ctx context.Context, e Execucao) error {
	av, _ := json.Marshal(nulo(e.Avisos))
	ap, _ := json.Marshal(nulo(e.Apagar))
	nt, _ := json.Marshal(nulo(e.Notas))
	_, err := c.db.ExecContext(ctx, `
		UPDATE execucoes SET estado = ?, etapa = ?, etapa_num = ?, etapas_total = ?, feito = ?, total = ?, item = ?,
			inicio = ?, fim = ?, atualizada_em = ?, pid = ?, dump_dir = ?, banco_novo = ?, banco_anterior = ?, erros_restore = ?,
			tamanho_dump = ?, mensagem = ?, avisos = ?, apagar = ?, plano = ?, novo_oid = ?, notas = ?
		WHERE id = ?`,
		e.Estado, e.Etapa, e.EtapaNum, e.EtapasTotal, e.Feito, e.Total, e.Item, texto(e.Inicio), texto(e.Fim), texto(time.Now()), e.PID,
		e.DumpDir, e.BancoNovo, e.BancoAnterior, e.ErrosRestore, e.TamanhoDump, e.Mensagem, string(av), string(ap), e.Plano, e.NovoOID,
		string(nt), e.ID)
	return err
}

// Progresso grava só o andamento: é chamado a cada linha do pg_dump e do pg_restore.
func (c *Cadastro) Progresso(ctx context.Context, id int64, feito, total int, item string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE execucoes SET feito = ?, total = ?, item = ?, atualizada_em = ? WHERE id = ?`,
		feito, total, item, texto(time.Now()), id)
	return err
}

// TamanhoDump grava o tamanho do dump enquanto ele cresce (o pg_dump não diz quanto falta).
func (c *Cadastro) TamanhoDump(ctx context.Context, id, bytes int64) error {
	_, err := c.db.ExecContext(ctx, `UPDATE execucoes SET tamanho_dump = ?, atualizada_em = ? WHERE id = ?`, bytes, texto(time.Now()), id)
	return err
}

func (c *Cadastro) Execucao(ctx context.Context, id int64) (Execucao, error) {
	e, err := lerExecucao(c.db.QueryRowContext(ctx, `SELECT `+colunasExecucao+` FROM execucoes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("execução %d: %w", id, ErrNaoExiste)
	}
	return e, err
}

// Execucoes devolve as mais recentes primeiro.
func (c *Cadastro) Execucoes(ctx context.Context, limite int) ([]Execucao, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+colunasExecucao+` FROM execucoes ORDER BY id DESC LIMIT ?`, limite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var es []Execucao
	for rows.Next() {
		e, err := lerExecucao(rows)
		if err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, rows.Err()
}

// ExecucoesDoGrupo são as execuções de um grupo, na ordem da fila.
func (c *Cadastro) ExecucoesDoGrupo(ctx context.Context, grupo string) ([]Execucao, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+colunasExecucao+` FROM execucoes WHERE grupo = ? ORDER BY id`, grupo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var es []Execucao
	for rows.Next() {
		e, err := lerExecucao(rows)
		if err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, rows.Err()
}

// UltimaDoPerfil é a execução de cópia mais recente do perfil. ok é falso se não houver.
func (c *Cadastro) UltimaDoPerfil(ctx context.Context, perfil string) (Execucao, bool, error) {
	e, err := lerExecucao(c.db.QueryRowContext(ctx, `SELECT `+colunasExecucao+` FROM execucoes WHERE perfil = ? AND tipo IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
		perfil, TipoCopia, TipoRestauracao, TipoReset))
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

// --- utilidades -------------------------------------------------------------------------------

const formatoHora = time.RFC3339Nano

func texto(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(formatoHora)
}

func hora(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(formatoHora, s)
	return t
}

func contem(lista []string, v string) bool {
	for _, x := range lista {
		if x == v {
			return true
		}
	}
	return false
}

func limpar(ss []string) []string {
	out := []string{}
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func nulo(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func traduzir(err error, repetido string) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New(repetido)
	}
	return err
}
