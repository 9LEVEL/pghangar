package conexao

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

// Camada é um passo do diagnóstico.
type Camada struct {
	Nome    string
	OK      bool
	Pulada  bool
	Detalhe string
}

// Diagnostico é o resultado das camadas, e o que a conexão viu quando chegou ao fim.
type Diagnostico struct {
	Camadas []Camada
	Info    cadastro.Info

	// O que a tela precisa perguntar para seguir.
	HostDesconhecido *tunel.HostDesconhecido
	PrecisaSenha     bool
	SenhaErrada      bool   // o servidor recusou a senha (28P01)
	PrecisaFrase     string // o caminho da chave
}

// Parou é a camada que falhou, ou vazio.
func (d Diagnostico) Parou() string { return d.Info.Camada }

func (d *Diagnostico) ok(nome, detalhe string) {
	d.Camadas = append(d.Camadas, Camada{Nome: nome, OK: true, Detalhe: detalhe})
}

func (d *Diagnostico) falha(nome string, err error) Diagnostico {
	d.Camadas = append(d.Camadas, Camada{Nome: nome, Detalhe: err.Error()})
	d.Info.Camada = nome
	d.Info.Erro = err.Error()
	return *d
}

// Diagnosticar percorre as camadas até o banco administrativo da conexão e lê o servidor.
func Diagnosticar(ctx context.Context, c cadastro.Conexao, a Ambiente, s Segredos) Diagnostico {
	d := Diagnostico{Info: cadastro.Info{VerificadaEm: time.Now()}}
	prazo := a.prazo()

	var ponte *Ponte
	if c.Acesso == cadastro.AcessoSSH {
		// O primeiro salto é o bastion, quando há um; o servidor SSH é testado por dentro dele.
		primeiro, porta, quem := c.SSHHost, c.SSHPorta, "SSH"
		if c.SaltoHost != "" {
			primeiro, porta, quem = c.SaltoHost, c.SaltoPorta, "bastion"
		}
		if err := resolver(ctx, primeiro); err != nil {
			return d.falha("DNS", err)
		}
		d.ok("DNS", primeiro)
		end := net.JoinHostPort(primeiro, strconv.Itoa(porta))
		if err := discar(ctx, end, prazo); err != nil {
			return d.falha("TCP", fmt.Errorf("%s %s: %w", quem, end, err))
		}
		d.ok("TCP", quem+" em "+end)

		chave := Chave(c, a)
		if chave != "" {
			if precisa, err := tunel.PrecisaDeFrase(chave); err == nil && precisa && s.Frases[chave] == "" {
				d.PrecisaFrase = chave
				return d.falha("SSH", &tunel.PrecisaFrase{Chave: chave})
			}
		}
		pt, err := abrirTunel(ctx, c, a, s)
		if err != nil {
			var hd *tunel.HostDesconhecido
			var fs *tunel.FalhaSSH
			var pf *tunel.PrecisaFrase
			switch {
			case errors.As(err, &hd):
				d.HostDesconhecido = hd
				return d.falha("SSH", err)
			case errors.As(err, &pf):
				d.PrecisaFrase = pf.Chave
				return d.falha("SSH", err)
			case errors.As(err, &fs) && fs.Camada == "canal":
				d.ok("SSH", "autenticado como "+c.SSHUsuario)
				return d.falha("Túnel", err)
			case errors.As(err, &fs) && fs.Camada == "salto":
				return d.falha("Bastion", err)
			}
			return d.falha("SSH", err)
		}
		defer pt.Fechar()
		d.ok("SSH", "autenticado como "+c.SSHUsuario)
		d.ok("Túnel", "até "+c.Endereco())
		ponte = pt
	} else {
		ponte = &Ponte{Host: c.Host, Porta: c.Porta}
		if ponte.Socket() {
			arq := fmt.Sprintf("%s/.s.PGSQL.%d", c.Host, c.Porta)
			if _, err := os.Stat(arq); err != nil {
				return d.falha("Socket", fmt.Errorf("não há socket em %s", arq))
			}
			d.ok("Socket", arq)
		} else {
			if err := resolver(ctx, c.Host); err != nil {
				return d.falha("DNS", err)
			}
			d.ok("DNS", c.Host)
			if err := discar(ctx, c.Endereco(), prazo); err != nil {
				return d.falha("TCP", err)
			}
			d.ok("TCP", c.Endereco())
		}
	}

	senha, err := Senha(c, s)
	if err != nil {
		var ps *PrecisaSenha
		if errors.As(err, &ps) {
			d.PrecisaSenha = true
		}
		return d.falha("Autenticação", err)
	}
	inicio := time.Now()
	conn, err := Conectar(ctx, c, ponte, senha, c.BancoAdmin, prazo)
	if err != nil {
		var f *FalhaPG
		if errors.As(err, &f) {
			switch f.Codigo {
			case "28P01", "28000":
				d.SenhaErrada = f.Codigo == "28P01"
				d.ok("Postgres", "respondeu")
				return d.falha("Autenticação", err)
			case "3D000":
				d.ok("Postgres", "respondeu")
				d.ok("Autenticação", c.Usuario)
				return d.falha("Banco", err)
			}
		}
		return d.falha("Postgres", err)
	}
	defer conn.Close(context.Background())
	d.Info.Latencia = time.Since(inicio)
	d.ok("Postgres", "respondeu")
	d.ok("Autenticação", c.Usuario)

	if err := LerServidor(ctx, conn, &d.Info); err != nil {
		return d.falha("Leitura", err)
	}
	t0 := time.Now()
	if _, err := conn.Exec(ctx, "SELECT 1"); err == nil {
		d.Info.Latencia = time.Since(t0)
	}
	lat := "<1 ms"
	if d.Info.Latencia >= time.Millisecond {
		lat = fmt.Sprintf("%d ms", d.Info.Latencia.Milliseconds())
	}
	detalhe := fmt.Sprintf("PostgreSQL %s, %d banco(s), latência %s", versaoTexto(d.Info.VersaoNum), len(d.Info.Bancos), lat)
	if d.Info.Recuperacao {
		detalhe += ", réplica"
	}
	if !d.Info.Superusuario {
		detalhe += ", SEM superusuário"
	}
	d.ok("Pronto", detalhe)
	return d
}

func versaoTexto(n int) string { return fmt.Sprintf("%d.%d", n/10000, n%10000) }

// LerServidor lê a versão, o papel, o system_identifier, o superusuário e os bancos.
func LerServidor(ctx context.Context, conn *pgx.Conn, i *cadastro.Info) error {
	err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, pg_is_in_recovery(),
		coalesce((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false)`).Scan(&i.VersaoNum, &i.Recuperacao, &i.Superusuario)
	if err != nil {
		return err
	}
	// pg_control_system pode ser negado a quem não é superusuário: sem ele, a comparação de cluster
	// fica com o aviso, e o resto segue.
	var sid string
	if err := conn.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&sid); err == nil {
		i.SystemID = sid
	}
	bancos, err := Bancos(ctx, conn, i.VersaoNum)
	if err != nil {
		return err
	}
	i.Bancos = bancos
	return nil
}

// Bancos lista os bancos do servidor. As colunas de locale mudaram entre as versões: o 15 trouxe o
// provedor por banco, o 16 as regras ICU, e o 17 trocou daticulocale por datlocale.
func Bancos(ctx context.Context, conn *pgx.Conn, versao int) ([]cadastro.Banco, error) {
	rows, err := conn.Query(ctx, ConsultaBancos(versao))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []cadastro.Banco
	for rows.Next() {
		var b cadastro.Banco
		if err := rows.Scan(&b.Nome, &b.Tamanho, &b.Dono, &b.Codificacao, &b.Collate, &b.Ctype, &b.Provedor, &b.Locale, &b.RegrasICU, &b.Conexoes); err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}

// ConsultaBancos é a consulta dos bancos para a versão do servidor.
func ConsultaBancos(versao int) string {
	provedor, locale, regras := "'c'", "''", "''"
	switch {
	case versao >= 170000:
		provedor, locale, regras = "d.datlocprovider::text", "coalesce(d.datlocale, '')", "coalesce(d.daticurules, '')"
	case versao >= 160000:
		provedor, locale, regras = "d.datlocprovider::text", "coalesce(d.daticulocale, '')", "coalesce(d.daticurules, '')"
	case versao >= 150000:
		provedor, locale = "d.datlocprovider::text", "coalesce(d.daticulocale, '')"
	}
	return `SELECT d.datname, pg_database_size(d.oid), pg_get_userbyid(d.datdba), pg_encoding_to_char(d.encoding),
		d.datcollate, d.datctype, ` + provedor + `, ` + locale + `, ` + regras + `, d.datallowconn
		FROM pg_database d WHERE NOT d.datistemplate ORDER BY d.datname`
}

func resolver(ctx context.Context, host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(c, host); err != nil {
		return fmt.Errorf("não resolve %s", host)
	}
	return nil
}

func discar(ctx context.Context, endereco string, prazo time.Duration) error {
	d := net.Dialer{Timeout: prazo}
	c, err := d.DialContext(ctx, "tcp", endereco)
	if err != nil {
		var ne net.Error
		switch {
		case errors.As(err, &ne) && ne.Timeout():
			return fmt.Errorf("tempo esgotado em %s (sem resposta: não dá para dizer se está fora do ar)", endereco)
		case isRecusada(err):
			return fmt.Errorf("conexão recusada em %s (o host respondeu, mas nada escuta nessa porta)", endereco)
		}
		return err
	}
	return c.Close()
}

func isRecusada(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial" && contem(err.Error(), "refused")
}

func contem(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
