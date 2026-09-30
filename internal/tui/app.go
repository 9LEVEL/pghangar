package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/tunel"
)

// Abas, na ordem das teclas 1 a 6.
const (
	AbaPerfis = iota
	AbaExecucoes
	AbaConexoes
	AbaDumps
	AbaAnteriores
	AbaAmbiente
)

var titulosAbas = []string{"Perfis", "Execuções", "Conexões", "Dumps", "Anteriores", "Ambiente"}

type Opcoes struct {
	Versao string
	Aba    int
	// IntervaloVerificacao é a verificação das conexões em segundo plano (padrão 60 s).
	IntervaloVerificacao time.Duration
	Servicos
}

// Rodar abre a tela e só volta quando ela fecha.
func Rodar(o Opcoes) error {
	_, err := tea.NewProgram(Novo(o), tea.WithAltScreen()).Run()
	return err
}

// Model é a tela inteira.
type Model struct {
	o       Opcoes
	maquina string
	usuario string

	largura, altura int
	aba             int
	quadro          int // anima o "ocupado"

	ajuda   bool
	ajudaVP viewport.Model
	status  string
	ocupado string // a tarefa em andamento, no rodapé

	form  formulario
	conf  confirmacao
	jan   janela
	plano *telaPlano
	grupo *telaGrupo

	// Os segredos informados nesta sessão (modo "perguntar" e passphrases). Só na memória.
	seg conexao.Segredos

	// o cadastro, relido depois de cada alteração
	conexoes  []cadastro.Conexao
	perfis    []cadastro.Perfil
	ultimas   map[string]cadastro.Execucao
	execucoes []cadastro.Execucao
	imagens   map[int]cadastro.Imagem
	cfg       cadastro.Config
	aprovados map[string]cadastro.DestinoAprovado

	perfisA     abaPerfis
	execA       abaExecucoes
	conexoesA   abaConexoes
	dumpsA      abaDumps
	anterioresA abaAnteriores
	ambienteA   abaAmbiente
}

func Novo(o Opcoes) *Model {
	if o.IntervaloVerificacao <= 0 {
		o.IntervaloVerificacao = time.Minute
	}
	maq, _ := os.Hostname()
	quem := "?"
	if u, err := user.Current(); err == nil {
		quem = u.Username
	}
	m := &Model{o: o, maquina: maq, usuario: quem, aba: limitar(o.Aba, 0, len(titulosAbas)-1), largura: 100, altura: 30}
	m.ajudaVP = viewport.New(60, 10)
	m.conexoesA.diag = map[string]conexao.Diagnostico{}
	m.conexoesA.verificando = map[string]bool{}
	m.recarregar()
	return m
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(tique(), m.verificarTodas(), m.ambienteA.carregar(m), m.entrarNaAba())
}

type msgTique struct{}

func tique() tea.Cmd { return tea.Tick(time.Second, func(time.Time) tea.Msg { return msgTique{} }) }

// recarregar lê o cadastro de novo. Um erro aqui aparece na janela: a tela não segue com dados
// velhos em silêncio.
func (m *Model) recarregar() {
	ctx := context.Background()
	c := m.o.Cadastro
	var err error
	falhou := func(o string, e error) {
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", o, e)
		}
	}
	var e error
	m.conexoes, e = c.Conexoes(ctx)
	falhou("conexões", e)
	m.perfis, e = c.Perfis(ctx)
	falhou("perfis", e)
	m.ultimas = map[string]cadastro.Execucao{}
	for _, p := range m.perfis {
		u, ok, e := c.UltimaDoPerfil(ctx, p.Nome)
		falhou("execuções", e)
		if ok {
			m.ultimas[p.Nome] = u
		}
	}
	m.execucoes, e = c.Execucoes(ctx, 200)
	falhou("execuções", e)
	m.imagens, e = c.Imagens(ctx)
	falhou("imagens", e)
	m.cfg, e = c.Config(ctx)
	falhou("configuração", e)
	m.aprovados, e = c.DestinosAprovados(ctx)
	falhou("destinos aprovados", e)
	m.perfisA.cursor = limitar(m.perfisA.cursor, 0, max(len(m.perfis)-1, 0))
	m.conexoesA.cursor = limitar(m.conexoesA.cursor, 0, max(len(m.conexoes)-1, 0))
	m.execA.cursor = limitar(m.execA.cursor, 0, max(len(m.execucoes)-1, 0))
	if err != nil {
		m.erro("Erro lendo o cadastro", err)
	}
}

func (m *Model) conexao(nome string) (cadastro.Conexao, bool) {
	for _, c := range m.conexoes {
		if c.Nome == nome {
			return c, true
		}
	}
	return cadastro.Conexao{}, false
}

func (m *Model) erro(titulo string, err error) {
	m.jan.mostrar(m.largura, m.altura, titulo, err.Error(), true)
}

func (m *Model) informar(titulo, corpo string) {
	m.jan.mostrar(m.largura, m.altura, titulo, corpo, false)
}

// --- tarefas ------------------------------------------------------------------------------------

// tarefa é uma ação demorada (rede, banco, Docker), rodada fora da tela. Se ela voltar pedindo uma
// senha, uma passphrase ou a aceitação de um servidor SSH, a tela pergunta e roda de novo.
type tarefa struct {
	rotulo string
	rodar  func(ctx context.Context, seg conexao.Segredos) (any, error)
	pronto func(m *Model, v any, err error) tea.Cmd
}

type msgTarefa struct {
	t   *tarefa
	v   any
	err error
}

func (m *Model) executar(t *tarefa) tea.Cmd {
	m.ocupado = t.rotulo
	seg := copiar(m.seg)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		v, err := t.rodar(ctx, seg)
		return msgTarefa{t: t, v: v, err: err}
	}
}

func copiar(s conexao.Segredos) conexao.Segredos {
	var c conexao.Segredos
	for k, v := range s.Senhas {
		c.GuardarSenha(k, v)
	}
	for k, v := range s.Frases {
		c.GuardarFrase(k, v)
	}
	return c
}

// perguntaDe tira do erro o que precisa ser perguntado.
func perguntaDe(err error) *motor.Pergunta {
	if err == nil {
		return nil
	}
	var p *motor.Pergunta
	var hd *tunel.HostDesconhecido
	var pf *tunel.PrecisaFrase
	var ps *conexao.PrecisaSenha
	switch {
	case errors.As(err, &p):
		return p
	case errors.As(err, &hd):
		return &motor.Pergunta{HostDesconhecido: hd}
	case errors.As(err, &pf):
		q := &motor.Pergunta{Frase: pf.Chave}
		if pf.Errada {
			q.Motivo = "a passphrase informada está errada"
		}
		return q
	case errors.As(err, &ps):
		return &motor.Pergunta{Conexao: ps.Conexao, Senha: true}
	}
	return nil
}

// perguntar abre a janela certa e, respondida, roda a tarefa de novo.
func (m *Model) perguntar(p *motor.Pergunta, t *tarefa) tea.Cmd {
	switch {
	case p.HostDesconhecido != nil:
		hd := p.HostDesconhecido
		corpo := fmt.Sprintf("O servidor SSH %s ainda não é conhecido por esta ferramenta.\n\n"+
			"Chave %s\n%s\n\n"+
			"Confira no próprio servidor antes de aceitar:\n  ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub\n\n"+
			"Aceita, a chave vai para %s. Se um dia ela mudar, a conexão é bloqueada.",
			hd.Endereco, hd.Chave.Type(), stValor.Render(hd.Fingerprint), tipoArquivoChave(hd.Chave.Type()), m.o.Dir.KnownHosts())
		m.conf.perguntar("Servidor SSH desconhecido", corpo, func() tea.Cmd {
			if err := m.o.AceitarHost(hd); err != nil {
				m.erro("Não foi possível gravar a chave", err)
				return nil
			}
			m.status = "chave de " + hd.Endereco + " aceita"
			return m.executar(t)
		})
		return nil
	case p.Frase != "":
		nota := frase(p.Motivo)
		return m.form.abrir("Passphrase da chave SSH", nota, []campo{
			campoDeSegredo("frase", "Passphrase", "A chave "+p.Frase+" é protegida. A passphrase fica só na memória, nesta sessão.", nil),
		}, func(f *formulario) (tea.Cmd, error) {
			m.seg.GuardarFrase(p.Frase, f.valor("frase"))
			return m.executar(t), nil
		})
	case p.Senha:
		nota := frase(p.Motivo)
		return m.form.abrir("Senha de "+p.Conexao, nota, []campo{
			campoDeSegredo("senha", "Senha", "A conexão "+p.Conexao+" pede a senha a cada sessão. Ela fica só na memória, e vai para o processo da cópia pela entrada padrão.", nil),
		}, func(f *formulario) (tea.Cmd, error) {
			m.seg.GuardarSenha(p.Conexao, f.valor("senha"))
			return m.executar(t), nil
		})
	}
	return nil
}

// frase põe maiúscula e ponto final num motivo.
func frase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func tipoArquivoChave(tipo string) string {
	switch {
	case strings.Contains(tipo, "ed25519"):
		return "ed25519"
	case strings.Contains(tipo, "ecdsa"):
		return "ecdsa"
	case strings.Contains(tipo, "rsa"):
		return "rsa"
	}
	return "*"
}

// --- mensagens ----------------------------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.largura, m.altura = msg.Width, msg.Height
		if m.plano != nil {
			m.plano.redimensionar(m)
		}
		return m, nil
	case tea.KeyMsg:
		return m, m.tecla(msg)
	case msgTique:
		return m, m.tiquetaque()
	case msgTarefa:
		m.ocupado = ""
		if p := perguntaDe(msg.err); p != nil {
			return m, m.perguntar(p, msg.t)
		}
		return m, msg.t.pronto(m, msg.v, msg.err)
	case msgVerificarTodas:
		return m, m.verificarTodas()
	case msgDiagFundo:
		m.conexoesA.receberFundo(m, msg)
		return m, nil
	case msgAmbiente:
		m.ambienteA.receber(m, msg)
		return m, nil
	case msgDumps:
		m.dumpsA.receber(m, msg)
		return m, nil
	}
	// o cursor piscando dos campos de texto
	var cmd tea.Cmd
	switch {
	case m.conf.ativa && m.conf.critica:
		m.conf.entrada, cmd = m.conf.entrada.Update(msg)
	case m.form.ativo:
		c := &m.form.campos[m.form.foco]
		c.entrada, cmd = c.entrada.Update(msg)
	case m.plano != nil:
		m.plano.entrada, cmd = m.plano.entrada.Update(msg)
	case m.grupo != nil:
		m.grupo.entrada, cmd = m.grupo.entrada.Update(msg)
	}
	return m, cmd
}

// tiquetaque: a cada segundo, relê as execuções (sem SSH nem banco: só o cadastro local) enquanto
// alguma estiver rodando, e atualiza o log aberto.
func (m *Model) tiquetaque() tea.Cmd {
	m.quadro++
	rodando := false
	for _, e := range m.execucoes {
		if !e.Terminou() {
			rodando = true
		}
	}
	if rodando || m.aba == AbaExecucoes || m.quadro%5 == 0 {
		antes := map[int64]string{}
		for _, e := range m.execucoes {
			antes[e.ID] = e.Estado
		}
		if m.quadro%10 == 0 && m.o.Conferir != nil {
			_ = m.o.Conferir(context.Background())
		}
		m.recarregarExecucoes()
		for _, e := range m.execucoes {
			if a, ok := antes[e.ID]; ok && a != e.Estado && e.Terminou() {
				m.status = fmt.Sprintf("#%d %s: %s", e.ID, e.Perfil, e.Estado)
			}
		}
	}
	if m.jan.ativa {
		m.jan.atualizarConteudo()
	}
	return tique()
}

func (m *Model) recarregarExecucoes() {
	ctx := context.Background()
	if es, err := m.o.Cadastro.Execucoes(ctx, 200); err == nil {
		m.execucoes = es
	}
	for _, p := range m.perfis {
		if u, ok, err := m.o.Cadastro.UltimaDoPerfil(ctx, p.Nome); err == nil && ok {
			m.ultimas[p.Nome] = u
		}
	}
	m.execA.cursor = limitar(m.execA.cursor, 0, max(len(m.execucoes)-1, 0))
}

func (m *Model) janelaAberta() bool {
	return m.jan.ativa || m.conf.ativa || m.form.ativo || m.ajuda || m.plano != nil || m.grupo != nil
}

// tecla: primeiro as janelas abertas, depois as teclas globais, depois a aba.
func (m *Model) tecla(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return tea.Quit
	}
	if k.String() != "?" {
		m.status = ""
	}
	switch {
	case m.jan.ativa:
		m.jan.atualizar(k)
		return nil
	case m.conf.ativa:
		r, cmd := m.conf.atualizar(k)
		switch r {
		case confSim:
			acao := m.conf.acao
			m.conf.fechar()
			if acao != nil {
				return acao()
			}
		case confNao:
			m.conf.fechar()
		}
		return cmd
	case m.form.ativo:
		m.form.largura = m.largura
		r, cmd := m.form.atualizar(k)
		if r == formEnviar && m.form.validarTudo() {
			g := m.form.geracao
			f := m.form
			c, err := f.enviar(&f)
			if err != nil {
				if m.form.geracao == g {
					m.form.erro = err.Error()
				} else {
					m.erro("Erro", err)
				}
				return nil
			}
			// Se o enviar abriu outra janela (uma confirmação, outro formulário), ela fica.
			if m.form.geracao == g && !m.conf.ativa {
				m.form.fechar()
			}
			return c
		}
		return cmd
	case m.plano != nil:
		return m.plano.tecla(m, k)
	case m.grupo != nil:
		return m.grupo.tecla(m, k)
	case m.ajuda:
		switch k.String() {
		case "esc", "?", "q", "enter":
			m.ajuda = false
		default:
			m.ajudaVP, _ = m.ajudaVP.Update(k)
		}
		return nil
	}

	switch k.String() {
	case "q":
		return tea.Quit
	case "?":
		m.abrirAjuda()
		return nil
	case "tab":
		m.aba = (m.aba + 1) % len(titulosAbas)
		return m.entrarNaAba()
	case "shift+tab":
		m.aba = (m.aba - 1 + len(titulosAbas)) % len(titulosAbas)
		return m.entrarNaAba()
	case "1", "2", "3", "4", "5", "6":
		m.aba = int(k.String()[0] - '1')
		return m.entrarNaAba()
	}

	switch m.aba {
	case AbaPerfis:
		return m.perfisA.tecla(m, k)
	case AbaExecucoes:
		return m.execA.tecla(m, k)
	case AbaConexoes:
		return m.conexoesA.tecla(m, k)
	case AbaDumps:
		return m.dumpsA.tecla(m, k)
	case AbaAnteriores:
		return m.anterioresA.tecla(m, k)
	case AbaAmbiente:
		return m.ambienteA.tecla(m, k)
	}
	return nil
}

// entrarNaAba carrega o que a aba mostra e não vem do cadastro.
func (m *Model) entrarNaAba() tea.Cmd {
	switch m.aba {
	case AbaDumps:
		return m.dumpsA.carregar(m)
	case AbaAnteriores:
		if !m.anterioresA.carregado {
			return m.anterioresA.carregar(m)
		}
	case AbaAmbiente:
		return m.ambienteA.carregar(m)
	}
	return nil
}

// --- desenho ------------------------------------------------------------------------------------

func (m *Model) View() string {
	w, h := m.largura, m.altura
	switch {
	case m.jan.ativa:
		return m.jan.view(w, h)
	case m.conf.ativa:
		return m.conf.view(w, h)
	case m.form.ativo:
		return m.form.view(w, h)
	case m.plano != nil:
		return m.plano.view(m)
	case m.grupo != nil:
		return m.grupo.view(m)
	case m.ajuda:
		return m.viewAjuda()
	}
	corpo := ""
	switch m.aba {
	case AbaPerfis:
		corpo = m.perfisA.view(m)
	case AbaExecucoes:
		corpo = m.execA.view(m)
	case AbaConexoes:
		corpo = m.conexoesA.view(m)
	case AbaDumps:
		corpo = m.dumpsA.view(m)
	case AbaAnteriores:
		corpo = m.anterioresA.view(m)
	case AbaAmbiente:
		corpo = m.ambienteA.view(m)
	}
	return m.cabecalho() + "\n" + m.barraAbas() + "\n" + ajustarAltura(recortar(corpo, w), m.alturaCorpo()) + "\n" + m.rodape()
}

// alturaCorpo: cabeçalho, abas e rodapé ocupam uma linha cada.
func (m *Model) alturaCorpo() int { return max(m.altura-3, 3) }

func versao(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func (m *Model) cabecalho() string {
	esq := stTitulo.Render(" pghangar ") + stVersao.Render(versao(m.o.Versao))
	maq := lipgloss.NewStyle().Bold(true).Foreground(corSobre).Background(corApagada).Padding(0, 1).Render(m.maquina)
	// O diretório é o que sai primeiro quando falta espaço; o host fica: é ele que diz onde se está.
	dir := m.seloAndamento() + " " + maq + stStatus.Render(" "+m.usuario+" · "+m.o.Dir.Raiz+" ")
	if lipgloss.Width(esq)+lipgloss.Width(dir) > m.largura {
		dir = m.seloAndamento() + " " + maq + stStatus.Render(" "+m.usuario+" ")
	}
	if lipgloss.Width(esq)+lipgloss.Width(dir) > m.largura {
		dir = m.seloAndamento() + " "
	}
	vao := max(m.largura-lipgloss.Width(esq)-lipgloss.Width(dir), 1)
	return esq + strings.Repeat(" ", vao) + dir
}

// seloAndamento mostra a cópia em andamento (ou a que espera decisão), em qualquer aba.
func (m *Model) seloAndamento() string {
	var rodando, aguardando []cadastro.Execucao
	fila := 0
	for _, e := range m.execucoes {
		switch {
		case e.Estado == cadastro.EstadoFila:
			fila++
		case !e.Terminou():
			rodando = append(rodando, e)
		case e.Estado == cadastro.EstadoAguardando:
			aguardando = append(aguardando, e)
		}
	}
	switch {
	case len(rodando) == 1:
		e := rodando[0]
		t := fmt.Sprintf("⟳ #%d %s · %s", e.ID, e.Perfil, e.Etapa)
		if e.Total > 0 {
			t += fmt.Sprintf(" %d%%", e.Feito*100/e.Total)
		}
		if fila > 0 {
			t += fmt.Sprintf(" · %d na fila", fila)
		}
		return fundo(corDestaque, t)
	case len(rodando) > 1:
		return fundo(corDestaque, fmt.Sprintf("⟳ %d cópias em andamento", len(rodando)))
	case len(aguardando) > 0:
		return fundo(corAviso, fmt.Sprintf("⚠ %d aguardando decisão", len(aguardando)))
	}
	return fundo(corOk, "pronto")
}

func (m *Model) barraAbas() string {
	var p []string
	for i, t := range titulosAbas {
		rot := fmt.Sprintf("%d %s", i+1, t)
		if i == m.aba {
			p = append(p, stAbaAtiva.Render(rot))
		} else {
			p = append(p, stAbaInat.Render(rot))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, p...)
}

var quadros = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *Model) rodape() string {
	if m.ocupado != "" {
		return truncar(stSelecao.Render(quadros[m.quadro%len(quadros)]+" "+m.ocupado+"…"), m.largura)
	}
	if m.status != "" {
		return truncar(stStatus.Render(m.status), m.largura)
	}
	var daAba string
	switch m.aba {
	case AbaPerfis:
		daAba = m.perfisA.dicas(m)
	case AbaExecucoes:
		daAba = m.execA.dicas(m)
	case AbaConexoes:
		daAba = juntarDicas(dica("a", "adicionar"), dica("e", "editar"), dica("t", "testar"), dica("v", "aprovar destino"), dica("b", "bancos"), dica("d", "remover"))
	case AbaDumps:
		daAba = juntarDicas(dica("↑↓", "escolher"), dica("r", "restaurar de novo"), dica("d", "apagar"), dica("R", "recarregar"))
	case AbaAnteriores:
		daAba = juntarDicas(dica("u", "desfazer"), dica("d", "apagar"), dica("r", "recarregar"))
	case AbaAmbiente:
		daAba = juntarDicas(dica("b", "baixar"), dica("u", "atualizar imagens"), dica("g", "gerar chave"), dica("l", "authorized_keys"), dica("e", "configurar"))
	}
	global := juntarDicas(dica("1-6", "abas"), dica("?", "ajuda"), dica("q", "sair"))
	return truncar(daAba+stDica.Render("   │   ")+global, m.largura)
}

// --- ajuda --------------------------------------------------------------------------------------

var atalhos = [][2]string{
	{"Global", ""},
	{"1-6  tab", "trocar de aba"},
	{"?", "esta ajuda"},
	{"q  ctrl+c", "sair (as cópias em andamento continuam: elas rodam fora da tela)"},
	{"", ""},
	{"1 Perfis", ""},
	{"enter", "copiar: checa tudo, mostra o plano e pede a confirmação (dev: y; homolog: o nome do banco)"},
	{"espaço", "marcar perfis para um grupo: enter copia os marcados em fila, uma cópia de cada vez (esc desmarca)"},
	{"z", "resetar o destino a partir da base (<banco>__base), sem ir à origem: perfis com \"guardar base\""},
	{"a / e / d", "adicionar / editar / remover um perfil"},
	{"", ""},
	{"2 Execuções", ""},
	{"enter  l", "ver o log (atualiza sozinho)"},
	{"c", "cancelar a cópia em andamento"},
	{"t", "trocar mesmo assim (quando o restore teve erro ou a conferência divergiu)"},
	{"x", "não trocar: encerra a execução e deixa o __novo no destino"},
	{"", ""},
	{"3 Conexões", ""},
	{"a / e / d", "adicionar / editar / remover (ao salvar, a conexão é testada e a versão é lida)"},
	{"t", "testar: o diagnóstico em camadas (DNS, TCP, SSH, túnel, Postgres, autenticação)"},
	{"b", "os bancos do servidor, com o tamanho"},
	{"v", "aprovar (ou revogar) o servidor como destino de cópias: sem aprovação, nenhuma cópia escreve nele"},
	{"", ""},
	{"4 Dumps", ""},
	{"r", "restaurar o dump de novo no destino do perfil, sem ir à origem (mesmo plano e mesma confirmação da cópia)"},
	{"d", "apagar um dump (nada é apagado sozinho)"},
	{"R", "ler os dumps de novo"},
	{"", ""},
	{"5 Anteriores", ""},
	{"u", "desfazer: o anterior volta a ser o banco, e o atual também vira anterior"},
	{"d", "apagar um __anterior ou um __novo que sobrou"},
	{"", ""},
	{"6 Ambiente", ""},
	{"b", "baixar as imagens que faltam (postgres:16, 17, 18) e travar pelo digest"},
	{"u", "atualizar: baixar as tags de novo e, se mudaram, travar as novas (com confirmação; as antigas ficam)"},
	{"g", "gerar a chave SSH da ferramenta"},
	{"l", "as linhas prontas para o authorized_keys de cada conexão SSH"},
	{"e", "o repositório das imagens, as versões e o webhook do aviso ao terminar"},
	{"w", "mandar um aviso de teste ao webhook"},
	{"", ""},
	{"Janelas", ""},
	{"tab  ↑↓", "próximo campo"},
	{"←→  espaço", "trocar a opção"},
	{"enter", "salvar"},
	{"esc", "cancelar"},
	{"y / n", "confirmar / cancelar"},
}

func (m *Model) abrirAjuda() {
	w := limitar(m.largura-10, 40, 96)
	var b strings.Builder
	for _, a := range atalhos {
		switch {
		case a[0] == "" && a[1] == "":
			b.WriteString("\n")
		case a[1] == "":
			b.WriteString(stMarca.Render(a[0]) + "\n")
		default:
			b.WriteString(quebrar("  "+stTecla.Render(preencher(a[0], 12)), stDica.Render(a[1]), w) + "\n")
		}
	}
	m.ajudaVP = viewport.New(w, 3)
	m.ajudaVP.SetContent(strings.TrimRight(b.String(), "\n"))
	// A caixa inteira cabe na tela: a rolagem fica com o que sobra do cabeçalho da ajuda.
	fixo := lipgloss.Height(m.caixaAjuda()) - m.ajudaVP.Height
	m.ajudaVP.Height = limitar(m.altura-fixo, 3, 60)
	m.ajuda = true
}

func (m *Model) viewAjuda() string {
	return lipgloss.Place(m.largura, m.altura, lipgloss.Center, lipgloss.Center, m.caixaAjuda())
}

func (m *Model) caixaAjuda() string {
	w := m.ajudaVP.Width
	larg := lipgloss.NewStyle().Width(w)
	ident := stTitulo.Render(" pghangar ") + stVersao.Render(versao(m.o.Versao))
	desc := larg.Foreground(corApagada).Render("Copia bancos PostgreSQL (16, 17, 18) por dump e restore, com os clientes oficiais em containers. " +
		"O destino é trocado por nome: o antigo vira __anterior, e nada é apagado sem pergunta.")
	onde := larg.Render(stRotulo.Render("host ") + stValor.Render(m.maquina) + stRotulo.Render("   usuário ") + stValor.Render(m.usuario) +
		stRotulo.Render("   diretório ") + stValor.Render(m.o.Dir.Raiz))
	regua := stDica.Render(strings.Repeat("─", w))
	rod := stDica.Render("esc fechar")
	if m.ajudaVP.TotalLineCount() > m.ajudaVP.Height {
		rod += stDica.Render(" · ↑↓ rolar")
	}
	conteudo := ident + "\n\n" + desc + "\n" + onde + "\n" + regua + "\n\n" + faixa(corDestaque, "Atalhos") + "\n\n" + m.ajudaVP.View() + "\n\n" + rod
	return stModal.BorderForeground(corDestaque).Render(conteudo)
}
