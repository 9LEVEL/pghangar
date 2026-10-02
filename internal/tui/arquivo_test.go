package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
)

// naEntrada põe na pasta de entrada um dos dumps de verdade do motor.
func naEntrada(t *testing.T, m *Model, fixture, nome string, quando time.Time) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "motor", "testdata", "arquivos", fixture))
	if err != nil {
		t.Fatal(err)
	}
	c := filepath.Join(m.o.Dir.Entrada(), nome)
	if err := os.WriteFile(c, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(c, quando, quando)
	return c
}

func TestRestaurarArquivoDaEntrada(t *testing.T) {
	f := &falsos{}
	m := novoTeste(t, f)
	comBancos(t, m, "homolog", banco("loja"), banco("rh"))
	caminho := naEntrada(t, m, "custom16.dump", "loja_20261001.dump", time.Now())
	_ = os.WriteFile(filepath.Join(m.o.Dir.Entrada(), "lixo.bin"), []byte{0, 1}, 0o600)
	_ = os.Chtimes(filepath.Join(m.o.Dir.Entrada(), "lixo.bin"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	var pedido motor.PedidoArquivo
	m.o.PlanejarArquivo = func(_ context.Context, pd motor.PedidoArquivo, _ conexao.Segredos) (motor.Plano, error) {
		pedido = pd
		a, err := motor.LerArquivo(pd.Caminho)
		if err != nil {
			return motor.Plano{}, err
		}
		return motor.Plano{Arquivo: &a, Perfil: cadastro.Perfil{Nome: filepath.Base(pd.Caminho), Destino: pd.Conexao, DestinoBanco: pd.Banco, JobsRestore: pd.Jobs},
			Destino: motor.Lado{Conexao: pd.Conexao, Tag: cadastro.TagHomolog, Banco: pd.Banco, VersaoNum: 180000, Existe: true}, Imagem: 18, Cliente: "18.6"}, nil
	}

	tecla(m, "4")
	if !m.dumpsA.carregado || len(m.dumpsA.entrada) != 2 {
		t.Fatalf("a pasta de entrada: %+v", m.dumpsA.entrada)
	}
	if tela := m.View(); !strings.Contains(tela, "PASTA DE ENTRADA") || !strings.Contains(tela, "loja_20261001.dump") || !strings.Contains(tela, "do banco loja") {
		t.Fatalf("a entrada deveria aparecer:\n%s", tela)
	}
	// O arquivo que não é um dump não abre o formulário.
	tecla(m, "down", "r")
	if m.form.ativo || !m.jan.ativa {
		t.Fatal("um formato desconhecido avisa, e não abre o formulário")
	}
	tecla(m, "esc", "up", "r")
	if !m.form.ativo {
		t.Fatal("o formulário deveria abrir")
	}
	for _, c := range m.form.campos {
		if c.chave == "destino" {
			for _, o := range c.opcoes {
				if o == "prod" {
					t.Fatal("prod oferecido como destino")
				}
			}
		}
	}
	if v := m.form.valor("banco"); v != "loja" {
		t.Fatalf("o banco sugerido é o do cabeçalho do dump: %q", v)
	}
	tecla(m, "enter")
	if m.plano == nil {
		t.Fatalf("o plano deveria abrir: %s", m.status)
	}
	if pedido.Caminho != caminho || pedido.Conexao != "homolog" || pedido.Banco != "loja" || pedido.Jobs != 4 {
		t.Fatalf("pedido %+v", pedido)
	}
	if tela := m.View(); !strings.Contains(tela, "Restaurar o arquivo") || !strings.Contains(tela, "ARQUIVO") || !strings.Contains(tela, "pg_restore com 4 job") {
		t.Fatalf("o plano do arquivo:\n%s", tela)
	}
	digitar(m, "loja")
	tecla(m, "enter")
	if len(f.iniciados) != 1 || f.iniciados[0].Plano.Arquivo == nil {
		t.Fatalf("a restauração deveria começar: %+v", f.iniciados)
	}
	if !strings.Contains(m.status, "restauração #7 iniciada") {
		t.Fatalf("status: %q", m.status)
	}
}

func TestApagarArquivoDaEntrada(t *testing.T) {
	m := novoTeste(t, &falsos{})
	c := naEntrada(t, m, "plain18.sql.gz", "loja.sql.gz", time.Now())
	tecla(m, "4", "d")
	if !m.conf.ativa {
		t.Fatal("apagar pergunta")
	}
	tecla(m, "n")
	if _, err := os.Stat(c); err != nil {
		t.Fatal("o n não apaga")
	}
	tecla(m, "d", "y")
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		t.Fatal("o y apaga o arquivo")
	}
	if len(m.dumpsA.entrada) != 0 {
		t.Fatal("a lista deveria ser lida de novo")
	}
}

func TestSugerirBanco(t *testing.T) {
	for _, c := range []struct {
		arq  motor.Arquivo
		quer string
	}{
		{motor.Arquivo{Caminho: "/e/x.dump", Banco: "Loja"}, "Loja"},
		{motor.Arquivo{Caminho: "/e/Backup Loja 2026-10-01.sql.gz"}, "backup_loja_2026_10_01"},
		{motor.Arquivo{Caminho: "/e/loja__novo.dump"}, ""},
		{motor.Arquivo{Caminho: "/e/x.dump", Banco: "loja__novo"}, "x"},
		{motor.Arquivo{Caminho: "/e/postgres.dump", Banco: "postgres"}, ""},
		{motor.Arquivo{Caminho: "/e/template1.sql"}, ""},
	} {
		if v := sugerirBanco(c.arq); v != c.quer {
			t.Errorf("%+v: %q (esperava %q)", c.arq, v, c.quer)
		}
	}
}

// Um banco onde uma restauração de arquivo escreveu aparece na aba Anteriores, mesmo sem perfil.
func TestAnterioresDasRestauracoesDeArquivo(t *testing.T) {
	m := novoTeste(t, &falsos{})
	for _, e := range []cadastro.Execucao{
		{Perfil: "rh.dump", Tipo: cadastro.TipoArquivo, Estado: cadastro.EstadoOK, Destino: "homolog", Banco: "rh", BancoNovo: "rh__novo"},
		// Uma que parou nas checagens não escreveu nada: o banco dela não aparece.
		{Perfil: "x.dump", Tipo: cadastro.TipoArquivo, Estado: cadastro.EstadoErro, Destino: "homolog", Banco: "nada"},
	} {
		if _, err := m.o.Cadastro.NovaExecucao(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	tecla(m, "5")
	if len(m.anterioresA.destinos) != 2 {
		t.Fatalf("destinos: %+v", m.anterioresA.destinos)
	}
	if tela := m.View(); !strings.Contains(tela, "homolog / rh") || !strings.Contains(tela, "restaurado de arquivo") {
		t.Fatalf("o destino do arquivo deveria aparecer:\n%s", tela)
	}
}

// Com outra tarefa rodando, o enter no formulário não o perde: ele fica aberto, com o motivo.
func TestFormularioDoArquivoEsperaATarefa(t *testing.T) {
	m := novoTeste(t, &falsos{})
	naEntrada(t, m, "custom16.dump", "loja.dump", time.Now())
	tecla(m, "4", "r")
	if !m.form.ativo {
		t.Fatal("o formulário deveria abrir")
	}
	m.ocupado = "lendo os bancos de homolog"
	tecla(m, "enter")
	if !m.form.ativo || !strings.Contains(m.form.erro, "espere terminar") {
		t.Fatalf("o formulário fica, com o motivo: ativo=%v %q", m.form.ativo, m.form.erro)
	}
}

// Um arquivo sem sugestão de banco não marca uma etiqueta vazia.
func TestSemSugestaoNaoMarcaVazio(t *testing.T) {
	m := novoTeste(t, &falsos{})
	naEntrada(t, m, "plain18.sql", "loja__novo.sql", time.Now())
	tecla(m, "4", "r")
	if c := m.form.campo("banco"); c == nil || len(c.esc.marcados) != 0 {
		t.Fatalf("nada marcado: %+v", c.esc.marcados)
	}
}

// Um arquivo que uma restauração em andamento usa não se apaga.
func TestApagarArquivoEmUsoERecusado(t *testing.T) {
	m := novoTeste(t, &falsos{})
	c := naEntrada(t, m, "custom16.dump", "loja.dump", time.Now())
	pj := `{"arquivo":{"caminho":"` + c + `"}}`
	if _, err := m.o.Cadastro.NovaExecucao(context.Background(), cadastro.Execucao{Perfil: "loja.dump", Tipo: cadastro.TipoArquivo,
		Estado: cadastro.EstadoRodando, Destino: "homolog", Banco: "loja", Plano: pj}); err != nil {
		t.Fatal(err)
	}
	m.recarregar()
	tecla(m, "4", "d")
	if m.conf.ativa || !m.jan.ativa {
		t.Fatal("em uso: avisa, e não pergunta se apaga")
	}
	if _, err := os.Stat(c); err != nil {
		t.Fatal("o arquivo continua")
	}
}

// A leitura de um SQL grande antes do plano para com o esc.
func TestEscCancelaATarefaCancelavel(t *testing.T) {
	m := novoTeste(t, &falsos{})
	parou := make(chan struct{})
	cmd := m.executar(&tarefa{rotulo: "lendo x.sql", cancelavel: true,
		rodar: func(ctx context.Context, _ conexao.Segredos) (any, error) {
			<-ctx.Done()
			close(parou)
			return nil, ctx.Err()
		},
		pronto: func(*Model, any, error) tea.Cmd { return nil }})
	go cmd()
	if !strings.Contains(m.View(), "esc cancela") {
		t.Fatal("o rodapé diz que o esc cancela")
	}
	tecla(m, "esc")
	select {
	case <-parou:
	case <-time.After(2 * time.Second):
		t.Fatal("o esc deveria cancelar a tarefa")
	}
}
