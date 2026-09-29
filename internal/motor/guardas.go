package motor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
)

// guardaProd recusa escrever num servidor de produção. A tag é a primeira barreira; esta é a
// segunda: uma conexão sem a tag prod (cadastrada de novo, copiada, editada) que aponte para o
// mesmo servidor de uma conexão prod é recusada, pelo system_identifier ou pelo endereço.
func guardaProd(ctx context.Context, d Deps, c cadastro.Conexao, systemID string) error {
	if c.Tag == cadastro.TagProd {
		return fmt.Errorf("a conexão %s é prod: um banco prod nunca é destino", c.Nome)
	}
	cs, err := d.Cadastro.Conexoes(ctx)
	if err != nil {
		return err
	}
	for _, x := range cs {
		if x.Tag != cadastro.TagProd {
			continue
		}
		if systemID != "" && x.Info.SystemID == systemID {
			return fmt.Errorf("a conexão %s aponta para o mesmo servidor da conexão prod %s (system_identifier %s): um banco prod nunca é destino", c.Nome, x.Nome, systemID)
		}
		if mesmoEndereco(c, x) {
			return fmt.Errorf("a conexão %s tem o mesmo endereço da conexão prod %s: um banco prod nunca é destino", c.Nome, x.Nome)
		}
	}
	return nil
}

func ehLocal(h string) bool {
	h = strings.ToLower(h)
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// mesmoEndereco compara onde as duas conexões chegam: direto, pelo mesmo túnel, ou uma direta e a
// outra pelo SSH do mesmo host até o banco local dele.
func mesmoEndereco(a, b cadastro.Conexao) bool {
	eq := strings.EqualFold
	switch {
	case a.Acesso == cadastro.AcessoDireto && b.Acesso == cadastro.AcessoDireto:
		return eq(a.Host, b.Host) && a.Porta == b.Porta
	case a.Acesso == cadastro.AcessoSSH && b.Acesso == cadastro.AcessoSSH:
		return eq(a.SSHHost, b.SSHHost) && a.SSHPorta == b.SSHPorta && eq(a.Host, b.Host) && a.Porta == b.Porta
	case a.Acesso == cadastro.AcessoDireto:
		return eq(a.Host, b.SSHHost) && a.Porta == b.Porta && ehLocal(b.Host)
	default:
		return eq(b.Host, a.SSHHost) && a.Porta == b.Porta && ehLocal(a.Host)
	}
}

// bancosDePerfis são os bancos que algum perfil usa numa conexão (origem ou destino). Um banco
// assim nunca é tratado como "da ferramenta", mesmo que o nome pareça um __novo ou um __anterior.
func bancosDePerfis(ctx context.Context, d Deps, conexao string) (map[string]bool, error) {
	ps, err := d.Cadastro.Perfis(ctx)
	if err != nil {
		return nil, err
	}
	r := map[string]bool{}
	for _, p := range ps {
		if p.Destino == conexao {
			r[p.DestinoBanco] = true
		}
		if p.Origem == conexao {
			r[p.OrigemBanco] = true
		}
	}
	return r, nil
}

// conferirScript recusa um script que não seja do root ou que grupo e outros possam alterar: ele
// roda com poderes de superusuário no banco.
func conferirScript(caminho string) error {
	fi, err := os.Stat(caminho)
	if err != nil {
		return fmt.Errorf("script pós-restore: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("script pós-restore: %s é um diretório", caminho)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("script pós-restore: %s precisa ser do root (ele roda como superusuário no banco)", caminho)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("script pós-restore: %s pode ser alterado por grupo ou outros (%04o): rode `chmod go-w %s`", caminho, fi.Mode().Perm(), caminho)
	}
	return nil
}

// ConferirScript é a mesma conferência, para a tela avisar ao salvar o perfil.
func ConferirScript(caminho string) error { return conferirScript(caminho) }

// oid lê o oid de um banco (0 se não existir).
func oid(ctx context.Context, adm *pgx.Conn, nome string) (uint32, error) {
	var o uint32
	err := adm.QueryRow(ctx, `SELECT coalesce((SELECT oid FROM pg_database WHERE datname = $1), 0::oid)`, nome).Scan(&o)
	return o, err
}
