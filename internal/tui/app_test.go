package tui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/crypto/ssh"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/execucao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

type dockerFalso struct{}

func (dockerFalso) Versao(context.Context) (string, error)                       { return "29.0", nil }
func (dockerFalso) Existe(context.Context, string) bool                          { return true }
func (dockerFalso) Baixar(context.Context, string, io.Writer) error              { return nil }
func (dockerFalso) Digest(context.Context, string) (string, error)               { return "postgres@sha256:abc", nil }
func (dockerFalso) VersaoCliente(context.Context, string) (string, error)        { return "18.6", nil }
func (dockerFalso) Rodando(context.Context, string) ([]imagens.Container, error) { return nil, nil }
func (dockerFalso) Parar(context.Context, string) error                          { return nil }

type falsos struct {
	plano     motor.Plano
	planoErr  func(seg conexao.Segredos) error
	iniciados []execucao.Pedido
	desfeitos []string
	apagados  []string
	segVistos []conexao.Segredos
}

func novoTeste(t *testing.T, f *falsos) *Model {
	t.Helper()
	dir := local.Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := dir.Preparar(); err != nil {
		t.Fatal(err)
	}
	cad, err := cadastro.Abrir(dir.Estado())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cad.Fechar() })
	ctx := context.Background()
	for _, c := range []cadastro.Conexao{
		{Nome: "prod", Tag: cadastro.TagProd, Acesso: cadastro.AcessoDireto, Host: "127.0.0.1", Porta: 5432, Usuario: "postgres", ModoSenha: cadastro.SenhaPerguntar, SSLMode: "prefer", BancoAdmin: "postgres"},
		{Nome: "homolog", Tag: cadastro.TagHomolog, Acesso: cadastro.AcessoDireto, Host: "127.0.0.1", Porta: 5433, Usuario: "postgres", ModoSenha: cadastro.SenhaGuardar, Senha: "x", SSLMode: "prefer", BancoAdmin: "postgres"},
	} {
		if err := cad.SalvarConexao(ctx, "", c); err != nil {
			t.Fatal(err)
		}
	}
	if err := cad.SalvarPerfil(ctx, "", cadastro.Perfil{Nome: "loja", Origem: "prod", OrigemBanco: "loja", Destino: "homolog", DestinoBanco: "loja", JobsDump: 2, JobsRestore: 4}); err != nil {
		t.Fatal(err)
	}
	s := Servicos{
		Cadastro: cad, Dir: dir, Docker: dockerFalso{},
		Planejar: func(_ context.Context, _ string, seg conexao.Segredos) (motor.Plano, error) {
			f.segVistos = append(f.segVistos, seg)
			if f.planoErr != nil {
				if err := f.planoErr(seg); err != nil {
					return motor.Plano{}, err
				}
			}
			return f.plano, nil
		},
		Diagnosticar: func(context.Context, cadastro.Conexao, conexao.Segredos) conexao.Diagnostico {
			return conexao.Diagnostico{Info: cadastro.Info{VerificadaEm: time.Now(), VersaoNum: 180006, Superusuario: true}}
		},
		Listar: func(_ context.Context, c, b string, _ conexao.Segredos) (motor.DaFerramenta, error) {
			return motor.DaFerramenta{Conexao: c, Banco: b, Existe: true, Anteriores: []motor.Anterior{{Nome: "loja__anterior_20260929_101010", Tamanho: 10}}}, nil
		},
		Desfazer: func(_ context.Context, _, _, a string, _ conexao.Segredos) (string, error) {
			f.desfeitos = append(f.desfeitos, a)
			return "loja__anterior_20260929_111111", nil
		},
		Apagar: func(_ context.Context, _, _, n string, _ conexao.Segredos) error {
			f.apagados = append(f.apagados, n)
			return nil
		},
		Iniciar: func(_ context.Context, p execucao.Pedido) (int64, error) {
			f.iniciados = append(f.iniciados, p)
			return 7, nil
		},
		Trocar:      func(context.Context, int64, conexao.Segredos) error { return nil },
		Descartar:   func(context.Context, int64) error { return nil },
		Cancelar:    func(cadastro.Execucao) error { return nil },
		Conferir:    func(context.Context) error { return nil },
		AceitarHost: func(*tunel.HostDesconhecido) error { return nil },
		GerarChave:  func() error { return nil },
		LerLog:      func(int64) string { return "log" },
	}
	m := Novo(Opcoes{Versao: "teste", Servicos: s})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	return m
}

// rodar executa o comando e as mensagens que ele gera, sem esperar os tiques (que dormem).
func rodar(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	res := make(chan tea.Msg, 1)
	go func() { res <- cmd() }()
	select {
	case msg := <-res:
		switch x := msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range x {
				rodar(m, c)
			}
		case msgTique, msgVerificarTodas:
		default:
			_, next := m.Update(msg)
			rodar(m, next)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func tecla(m *Model, ks ...string) {
	for _, k := range ks {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "ctrl+a":
			msg = tea.KeyMsg{Type: tea.KeyCtrlA}
		case "ctrl+r":
			msg = tea.KeyMsg{Type: tea.KeyCtrlR}
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := m.Update(msg)
		rodar(m, cmd)
	}
}

func digitar(m *Model, s string) {
	for _, r := range s {
		tecla(m, string(r))
	}
}

func planoHomolog() motor.Plano {
	return motor.Plano{
		Perfil:  cadastro.Perfil{Nome: "loja", Origem: "prod", OrigemBanco: "loja", Destino: "homolog", DestinoBanco: "loja", JobsDump: 2, JobsRestore: 4},
		Origem:  motor.Lado{Conexao: "prod", Tag: cadastro.TagProd, Banco: "loja", VersaoNum: 160000, Existe: true},
		Destino: motor.Lado{Conexao: "homolog", Tag: cadastro.TagHomolog, Banco: "loja", VersaoNum: 180000, Existe: true},
		Imagem:  18, Cliente: "18.6", DirDumps: "/x",
		Anteriores: []motor.Anterior{{Nome: "loja__anterior_20260929_101010", Tamanho: 1}, {Nome: "loja__anterior_20260928_101010", Tamanho: 2}},
	}
}

func TestHomologExigeONomeDigitado(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter") // copiar o perfil
	if m.plano == nil {
		t.Fatal("o plano deveria abrir")
	}
	tecla(m, "y", "enter") // y é letra no campo; enter com "y" não confirma
	digitar(m, "lojx")
	tecla(m, "enter")
	if len(f.iniciados) != 0 {
		t.Fatal("confirmou com o nome errado")
	}
	if !strings.Contains(m.plano.erro, "loja") {
		t.Fatalf("deveria dizer o que digitar: %q", m.plano.erro)
	}
	// Corrige o texto: apaga e digita o nome certo.
	for i := 0; i < 10; i++ {
		tecla(m, "\x7f")
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	digitar(m, "loja")
	tecla(m, "enter")
	if len(f.iniciados) != 1 || len(f.iniciados[0].Apagar) != 0 {
		t.Fatalf("iniciados: %+v", f.iniciados)
	}
	if f.iniciados[0].Segredos.Senhas["prod"] != "s" {
		t.Fatal("os segredos da sessão vão para o processo da cópia")
	}
	if m.aba != AbaExecucoes {
		t.Fatal("depois de iniciar, a tela vai para as execuções")
	}
}

func TestMarcarAnterioresParaApagar(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter")
	// Com o foco no campo, espaço é texto e não marca nada.
	tecla(m, " ")
	if len(m.plano.apagar()) != 0 {
		t.Fatal("espaço no campo não pode marcar")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	tecla(m, "tab", "down", " ", "tab") // marca o segundo e volta ao campo
	digitar(m, "loja")
	tecla(m, "enter")
	if len(f.iniciados) != 1 || !reflect.DeepEqual(f.iniciados[0].Apagar, []string{"loja__anterior_20260928_101010"}) {
		t.Fatalf("%+v", f.iniciados)
	}
}

func TestDevConfirmaComY(t *testing.T) {
	p := planoHomolog()
	p.Destino.Tag = cadastro.TagDev
	f := &falsos{plano: p}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter", "x", "n", "enter")
	if len(f.iniciados) != 0 {
		t.Fatal("tecla qualquer (ou enter) confirmou num destino dev")
	}
	tecla(m, " ", "y") // marca o primeiro anterior e confirma
	if len(f.iniciados) != 1 || !reflect.DeepEqual(f.iniciados[0].Apagar, []string{"loja__anterior_20260929_101010"}) {
		t.Fatalf("%+v", f.iniciados)
	}
}

func TestPlanoBloqueadoNaoInicia(t *testing.T) {
	p := planoHomolog()
	p.Bloqueios = []string{"copiar descendo de versão é bloqueado"}
	f := &falsos{plano: p}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter")
	digitar(m, "loja")
	tecla(m, "enter", "y", "enter")
	if len(f.iniciados) != 0 {
		t.Fatal("plano bloqueado iniciou")
	}
	if !strings.Contains(m.View(), "bloqueado") {
		t.Fatal("a tela deveria dizer que está bloqueado")
	}
	tecla(m, "esc")
	if m.plano != nil {
		t.Fatal("esc fecha o plano")
	}
}

func TestSenhaPerguntadaENaoGuardada(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	f.planoErr = func(seg conexao.Segredos) error {
		if seg.Senhas["prod"] == "" {
			return &motor.Pergunta{Conexao: "prod", Senha: true}
		}
		return nil
	}
	m := novoTeste(t, f)
	tecla(m, "enter")
	if !m.form.ativo || !strings.Contains(m.form.titulo, "prod") {
		t.Fatalf("deveria pedir a senha de prod: %+v", m.form.titulo)
	}
	digitar(m, "s3gr:do")
	tecla(m, "enter")
	if m.plano == nil {
		t.Fatal("com a senha, o plano abre")
	}
	if m.seg.Senhas["prod"] != "s3gr:do" {
		t.Fatal("a senha fica na sessão")
	}
	c, _ := m.o.Cadastro.Conexao(context.Background(), "prod")
	if c.Senha != "" {
		t.Fatal("a senha perguntada nunca vai para o cadastro")
	}
}

func TestHostDesconhecidoPedeY(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	aceitos := 0
	f.planoErr = func(conexao.Segredos) error {
		if aceitos == 0 {
			return &motor.Pergunta{Conexao: "prod", HostDesconhecido: &tunel.HostDesconhecido{Endereco: "10.0.0.1:22", Chave: chaveQualquer(t), Fingerprint: "SHA256:xyz"}}
		}
		return nil
	}
	m := novoTeste(t, f)
	m.o.AceitarHost = func(*tunel.HostDesconhecido) error { aceitos++; return nil }
	tecla(m, "enter")
	if !m.conf.ativa || !strings.Contains(m.conf.corpo, "SHA256:xyz") {
		t.Fatal("deveria mostrar o fingerprint")
	}
	tecla(m, "enter") // enter não aceita uma chave de host
	if aceitos != 0 {
		t.Fatal("enter aceitou a chave")
	}
	tecla(m, "y")
	if aceitos != 1 || m.plano == nil {
		t.Fatalf("aceitos %d, plano %v", aceitos, m.plano != nil)
	}
}

func TestPerfilNuncaOfereceProdComoDestino(t *testing.T) {
	m := novoTeste(t, &falsos{})
	tecla(m, "a")
	for _, c := range m.form.campos {
		if c.chave == "destino" {
			for _, o := range c.opcoes {
				if o == "prod" {
					t.Fatal("prod oferecido como destino")
				}
			}
			return
		}
	}
	t.Fatal("sem campo destino")
}

func TestDesfazerEmHomologPedeONome(t *testing.T) {
	f := &falsos{}
	m := novoTeste(t, f)
	tecla(m, "5")
	if !m.anterioresA.carregado {
		t.Fatal("a aba deveria carregar")
	}
	tecla(m, "u")
	if !m.conf.ativa || !m.conf.critica {
		t.Fatal("desfazer em homolog é crítico")
	}
	tecla(m, "y", "enter")
	if len(f.desfeitos) != 0 {
		t.Fatal("y não basta em homolog")
	}
	tecla(m, "esc")
	tecla(m, "u")
	digitar(m, "loja")
	tecla(m, "enter")
	if !reflect.DeepEqual(f.desfeitos, []string{"loja__anterior_20260929_101010"}) {
		t.Fatalf("%v", f.desfeitos)
	}
	// Apagar não tem volta: em homolog, pede o nome do banco também (y e enter não bastam).
	tecla(m, "d", "y", "enter")
	if len(f.apagados) != 0 {
		t.Fatal("y/enter não confirmam apagar em homolog")
	}
	tecla(m, "esc", "d")
	digitar(m, "loja")
	tecla(m, "enter")
	if len(f.apagados) != 1 {
		t.Fatal("o nome digitado confirma apagar")
	}
}

func TestErroDoPlanoAparece(t *testing.T) {
	f := &falsos{planoErr: func(conexao.Segredos) error { return errors.New("perfil sumiu") }}
	m := novoTeste(t, f)
	tecla(m, "enter")
	if !m.jan.ativa || !strings.Contains(m.View(), "perfil sumiu") {
		t.Fatal("o erro deveria aparecer numa janela")
	}
}

// Nenhuma tela quebra, em nenhum tamanho: o corpo cabe e o cabeçalho fica no topo.
func TestTamanhos(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	m := novoTeste(t, f)
	ctx := context.Background()
	id, _ := m.o.Cadastro.NovaExecucao(ctx, cadastro.Execucao{Perfil: "loja", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoRodando,
		Destino: "homolog", Banco: "loja", Inicio: time.Now()})
	e, _ := m.o.Cadastro.Execucao(ctx, id)
	e.Etapa, e.EtapaNum, e.EtapasTotal, e.Feito, e.Total, e.Item = "Restore", 5, 11, 30, 90, "TABLE DATA vendas.pedido"
	_ = m.o.Cadastro.GravarExecucao(ctx, e)
	m.recarregar()
	for _, tam := range [][2]int{{40, 10}, {80, 24}, {120, 36}, {220, 60}} {
		m.Update(tea.WindowSizeMsg{Width: tam[0], Height: tam[1]})
		for aba := 0; aba < len(titulosAbas); aba++ {
			m.aba = aba
			v := m.View()
			linhas := strings.Split(v, "\n")
			if len(linhas) != tam[1] {
				t.Errorf("%dx%d aba %d: %d linhas", tam[0], tam[1], aba, len(linhas))
			}
			if !strings.Contains(linhas[0], "pghangar") {
				t.Errorf("%dx%d aba %d: o cabeçalho saiu do topo", tam[0], tam[1], aba)
			}
		}
		if !strings.Contains(m.cabecalho(), "#") && tam[0] >= 80 {
			t.Errorf("%dx%d: o cabeçalho deveria mostrar a cópia em andamento", tam[0], tam[1])
		}
	}
	// O plano e a ajuda cabem na altura.
	m.seg.GuardarSenha("prod", "s")
	m.aba = AbaPerfis
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	tecla(m, "enter")
	if n := len(strings.Split(m.View(), "\n")); n > 24 {
		t.Errorf("o plano tem %d linhas em 24", n)
	}
	if !strings.Contains(m.View(), "loja__anterior_20260929_101010") {
		t.Error("os anteriores precisam estar à vista no plano")
	}
	tecla(m, "esc", "?")
	if n := len(strings.Split(m.View(), "\n")); n != 24 || !strings.Contains(m.View(), "╔") {
		t.Errorf("a ajuda precisa caber inteira: %d linhas", n)
	}
}

func chaveQualquer(t *testing.T) ssh.PublicKey {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestConexaoNovaExigeATag(t *testing.T) {
	m := novoTeste(t, &falsos{})
	tecla(m, "3", "a")
	digitar(m, "nova")
	tecla(m, "enter")
	if !m.form.ativo || !strings.Contains(m.form.erro, "tag") {
		t.Fatalf("a tag precisa ser escolhida: %q", m.form.erro)
	}
	if _, ok := m.conexao("nova"); ok {
		t.Fatal("gravou sem tag")
	}
}

// As correções aparecem marcadas pelo padrão, o espaço desmarca, e o que vai para a execução é o
// que ficou marcado. A do __novo, desmarcada, bloqueia.
func TestPlanoComCorrecoes(t *testing.T) {
	p := planoHomolog()
	p.Destino.Tag = cadastro.TagDev
	p.Anteriores = nil
	p.Correcoes = []motor.Correcao{
		{Tipo: motor.CorrecaoNovo, Texto: "apagar o loja__novo", SeNao: "sobrou o loja__novo", Nomes: []string{"loja__novo"}, Marcada: true, Bloqueia: true},
		{Tipo: motor.CorrecaoRoles, Texto: "criar a role auditor", SeNao: "o restore dá erro", Nomes: []string{"auditor"}, Marcada: true},
		{Tipo: motor.CorrecaoFDW, Texto: "tirar os user mappings de erp", SeNao: "o destino entra no erp", Nomes: []string{"erp"}},
	}
	f := &falsos{plano: p}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter")
	if m.plano == nil {
		t.Fatal("o plano deveria abrir")
	}
	if v := m.View(); !strings.Contains(v, "CORREÇÕES (3)") || !strings.Contains(v, "[✔]") || !strings.Contains(v, "sem ela:") {
		t.Fatalf("as correções deveriam aparecer:\n%s", v)
	}
	tecla(m, " ", "y") // desmarca a do __novo: bloqueia
	if len(f.iniciados) != 0 || !strings.Contains(m.plano.erro, "bloqueado") {
		t.Fatalf("sem a correção do __novo, a cópia não começa: %q", m.plano.erro)
	}
	tecla(m, " ", "down", "down", " ", "y") // marca de novo, e marca a do FDW
	if len(f.iniciados) != 1 {
		t.Fatal("a cópia deveria começar")
	}
	var marcadas []string
	for _, c := range f.iniciados[0].Plano.Correcoes {
		if c.Marcada {
			marcadas = append(marcadas, c.Tipo)
		}
	}
	if strings.Join(marcadas, ",") != "novo,roles,fdw" {
		t.Fatalf("as marcas confirmadas: %v", marcadas)
	}
}

// Achado da revisão: num dev sem correções e sem anteriores (a primeira cópia de um banco), o y
// tem de confirmar.
func TestDevSemListasConfirmaComY(t *testing.T) {
	p := planoHomolog()
	p.Destino.Tag, p.Anteriores, p.Correcoes = cadastro.TagDev, nil, nil
	f := &falsos{plano: p}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, "enter", "y")
	if len(f.iniciados) != 1 {
		t.Fatal("o y deveria confirmar a cópia num dev sem listas")
	}
}

// Uma tarefa de cada vez, e um plano que chega com outra janela aberta não a substitui.
func TestUmaTarefaDeCadaVezEPlanoNaoAbrePorCima(t *testing.T) {
	p := planoHomolog()
	f := &falsos{plano: p}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	m.ocupado = "outra tarefa"
	tecla(m, "enter")
	if len(f.segVistos) != 0 || !strings.Contains(m.status, "espere") {
		t.Fatalf("com uma tarefa rodando, outra não começa: %q", m.status)
	}
	m.ocupado = ""
	tecla(m, "a") // o formulário do perfil novo
	m.abrirPlano(p)
	if m.plano != nil || !strings.Contains(m.status, "outra janela aberta") {
		t.Fatalf("o plano não abre por cima do formulário: %q", m.status)
	}
}

// Um grupo de um perfil só é uma cópia comum, com a confirmação dela.
func TestGrupoDeUmPerfilECopiaComum(t *testing.T) {
	f := &falsos{plano: planoHomolog()}
	m := novoTeste(t, f)
	m.seg.GuardarSenha("prod", "s")
	tecla(m, " ", "up", "enter")
	if m.grupo != nil || m.plano == nil {
		t.Fatal("um perfil marcado sozinho abre o plano da cópia, e não o do grupo")
	}
}
