package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Os componentes em janela, como no pgtower: formulário, confirmação (simples com "y", crítica
// digitando uma palavra) e aviso com rolagem. Quem estiver aberto recebe as teclas primeiro.

// --- formulário -------------------------------------------------------------------------------

type tipoCampo int

const (
	campoTexto tipoCampo = iota
	campoSegredo
	campoOpcao
	campoEscolha
)

type campo struct {
	chave   string
	rotulo  string
	ajuda   string // aparece embaixo quando o campo está em foco
	tipo    tipoCampo
	entrada textinput.Model
	opcoes  []string
	sel     int
	validar func(string) error
	// visivel, se houver, esconde o campo conforme os outros (os do SSH só com acesso ssh).
	visivel func(f *formulario) bool
	// ajudaDin, se houver, troca a ajuda conforme os outros campos (os bancos da conexão escolhida).
	ajudaDin func(f *formulario) string
	// esc é o seletor de etiquetas do campoEscolha.
	esc escolha
	// auto, se houver, preenche o campo a partir dos outros (o nome do perfil) enquanto ninguém o
	// editar: autoValor é o último valor posto por ele.
	auto      func(f *formulario) string
	autoValor string
}

func novoCampo(chave, rotulo, valor, ajuda string, validar func(string) error) campo {
	ti := textinput.New()
	ti.CharLimit = 500
	ti.Width = 44
	ti.Prompt = ""
	ti.SetValue(valor)
	return campo{chave: chave, rotulo: rotulo, ajuda: ajuda, tipo: campoTexto, entrada: ti, validar: validar}
}

// campoDeSegredo nunca mostra o valor gravado: começa vazio.
func campoDeSegredo(chave, rotulo, ajuda string, validar func(string) error) campo {
	c := novoCampo(chave, rotulo, "", ajuda, validar)
	c.tipo = campoSegredo
	c.entrada.EchoMode = textinput.EchoPassword
	c.entrada.EchoCharacter = '•'
	return c
}

// campoDeEscolha é um campo de etiquetas (os bancos), com o seletor dado.
func campoDeEscolha(chave, rotulo, ajuda string, e escolha, validar func(string) error) campo {
	return campo{chave: chave, rotulo: rotulo, ajuda: ajuda, tipo: campoEscolha, esc: e, validar: validar}
}

func campoDeOpcao(chave, rotulo, ajuda string, opcoes []string, atual string) campo {
	c := campo{chave: chave, rotulo: rotulo, ajuda: ajuda, tipo: campoOpcao, opcoes: opcoes}
	for i, o := range opcoes {
		if o == atual {
			c.sel = i
		}
	}
	return c
}

func (c campo) valor() string {
	if c.tipo == campoEscolha {
		return strings.Join(c.esc.marcados, ",")
	}
	if c.tipo == campoOpcao {
		if len(c.opcoes) == 0 {
			return ""
		}
		return c.opcoes[c.sel]
	}
	if c.tipo == campoSegredo {
		return c.entrada.Value() // senha pode ter espaço nas pontas
	}
	return strings.TrimSpace(c.entrada.Value())
}

type resultadoForm int

const (
	formNada resultadoForm = iota
	formEnviar
	formCancelar
)

// geracao distingue um formulário do seguinte: quem envia pode abrir outro, e ele não pode ser
// fechado junto.
var geracao int

type formulario struct {
	geracao int
	ativo   bool
	titulo  string
	nota    string // aviso fixo no topo do formulário
	campos  []campo
	foco    int
	erro    string
	enviar  func(f *formulario) (tea.Cmd, error)
	largura int // a da tela, para o seletor de etiquetas saber as linhas
}

func (f *formulario) abrir(titulo, nota string, campos []campo, enviar func(f *formulario) (tea.Cmd, error)) tea.Cmd {
	geracao++
	*f = formulario{geracao: geracao, ativo: true, titulo: titulo, nota: nota, campos: campos, enviar: enviar}
	f.aplicarAutos()
	f.foco = f.proximo(-1, 1)
	return f.focar()
}

func (f *formulario) fechar() { *f = formulario{} }

func (f *formulario) aparece(i int) bool {
	return f.campos[i].visivel == nil || f.campos[i].visivel(f)
}

// proximo acha o próximo campo visível a partir de i, na direção d.
func (f *formulario) proximo(i, d int) int {
	n := len(f.campos)
	for k := 1; k <= n; k++ {
		j := ((i+d*k)%n + n) % n
		if f.aparece(j) {
			return j
		}
	}
	return 0
}

func (f *formulario) focar() tea.Cmd {
	var cmd tea.Cmd
	for i := range f.campos {
		if i == f.foco && f.campos[i].tipo != campoOpcao && f.campos[i].tipo != campoEscolha {
			cmd = f.campos[i].entrada.Focus()
		} else {
			f.campos[i].entrada.Blur()
		}
	}
	return cmd
}

func (f *formulario) mover(d int) tea.Cmd {
	f.foco = f.proximo(f.foco, d)
	return f.focar()
}

func (f *formulario) valor(chave string) string {
	if c := f.campo(chave); c != nil {
		return c.valor()
	}
	return ""
}

func (f *formulario) campo(chave string) *campo {
	for i := range f.campos {
		if f.campos[i].chave == chave {
			return &f.campos[i]
		}
	}
	return nil
}

// valores são as etiquetas marcadas de um campoEscolha.
func (f *formulario) valores(chave string) []string {
	if c := f.campo(chave); c != nil {
		return append([]string(nil), c.esc.marcados...)
	}
	return nil
}

// aplicarAutos refaz os campos automáticos que ninguém editou.
func (f *formulario) aplicarAutos() {
	for i := range f.campos {
		c := &f.campos[i]
		if c.auto == nil || c.entrada.Value() != c.autoValor {
			continue
		}
		v := c.auto(f)
		c.entrada.SetValue(v)
		c.entrada.CursorEnd()
		c.autoValor = v
	}
}

// validarTudo confere cada campo visível e leva o foco ao primeiro com erro.
func (f *formulario) validarTudo() bool {
	for i, c := range f.campos {
		if c.validar == nil || !f.aparece(i) {
			continue
		}
		if err := c.validar(c.valor()); err != nil {
			f.erro = c.rotulo + ": " + err.Error()
			f.foco = i
			f.focar()
			return false
		}
	}
	f.erro = ""
	return true
}

func (f *formulario) atualizar(msg tea.KeyMsg) (resultadoForm, tea.Cmd) {
	r, cmd := f.tratar(msg)
	if f.ativo {
		f.aplicarAutos()
	}
	return r, cmd
}

func (f *formulario) tratar(msg tea.KeyMsg) (resultadoForm, tea.Cmd) {
	cur := &f.campos[f.foco]
	if cur.tipo == campoEscolha {
		if r, cmd, ok := cur.esc.tecla(f, msg); ok {
			return r, cmd
		}
	}
	switch msg.String() {
	case "esc":
		f.fechar()
		return formCancelar, nil
	case "enter":
		return formEnviar, nil
	case "tab", "down":
		return formNada, f.mover(1)
	case "shift+tab", "up":
		return formNada, f.mover(-1)
	case "left":
		if cur.tipo == campoOpcao {
			cur.sel = (cur.sel - 1 + len(cur.opcoes)) % len(cur.opcoes)
			return formNada, nil
		}
	case "right", " ":
		if cur.tipo == campoOpcao {
			cur.sel = (cur.sel + 1) % len(cur.opcoes)
			return formNada, nil
		}
	}
	if cur.tipo == campoOpcao || cur.tipo == campoEscolha {
		return formNada, nil
	}
	var cmd tea.Cmd
	cur.entrada, cmd = cur.entrada.Update(msg)
	return formNada, cmd
}

// larguraForm é a largura do conteúdo do formulário na tela.
func larguraForm(largTela int) int { return limitar(largTela-8, 50, 92) }

func (f *formulario) view(w, h int) string {
	larg := larguraForm(w)
	titulo := faixa(corDestaque, f.titulo)
	rot := 0
	for _, c := range f.campos {
		rot = max(rot, lipgloss.Width(c.rotulo)+2)
	}
	var visiveis []int
	for i := range f.campos {
		if f.aparece(i) {
			visiveis = append(visiveis, i)
		}
	}
	// O seletor em foco ocupa várias linhas.
	var aberto []string
	if c := &f.campos[f.foco]; c.tipo == campoEscolha {
		aberto = c.esc.desenhar(f, larguraEtiquetas(w))
	}
	// Um formulário mais alto que a tela rola com o foco (os campos longe dele ficam de fora).
	cabe := max(h-16-len(aberto), 4)
	ini, fim := 0, len(visiveis)
	if len(visiveis) > cabe {
		pos := 0
		for k, i := range visiveis {
			if i == f.foco {
				pos = k
			}
		}
		ini, fim = janelaDeLinhas(len(visiveis), pos, cabe)
	}
	var linhas []string
	if ini > 0 {
		linhas = append(linhas, stDica.Render(fmt.Sprintf("  ↑ mais %d campo(s)", ini)))
	}
	for _, i := range visiveis[ini:fim] {
		c := &f.campos[i]
		r := stRotulo.Render(preencher("  "+c.rotulo, rot))
		if i == f.foco {
			r = stTecla.Render(preencher("› "+c.rotulo, rot))
		}
		v := c.entrada.View()
		switch c.tipo {
		case campoOpcao:
			v = stDica.Render("‹ ") + stValor.Render(c.valor()) + stDica.Render(" ›")
		case campoEscolha:
			v = c.esc.resumir(f, larg-rot-2)
			if i == f.foco {
				linhas = append(linhas, r)
				linhas = append(linhas, aberto...)
				continue
			}
		}
		linhas = append(linhas, r+"  "+v)
	}
	if fim < len(visiveis) {
		linhas = append(linhas, stDica.Render(fmt.Sprintf("  ↓ mais %d campo(s)", len(visiveis)-fim)))
	}
	corpo := titulo + "\n\n"
	if f.nota != "" {
		corpo += lipgloss.NewStyle().Width(larg).Foreground(corAviso).Render(f.nota) + "\n\n"
	}
	corpo += strings.Join(linhas, "\n")
	a := f.campos[f.foco].ajuda
	if d := f.campos[f.foco].ajudaDin; d != nil {
		a = d(f)
	}
	if a != "" {
		corpo += "\n\n" + lipgloss.NewStyle().Width(larg).Foreground(corApagada).Render(a)
	}
	teclas := "tab/↑↓ campos · ←→ opções · enter salvar · esc cancelar"
	if c := &f.campos[f.foco]; c.tipo == campoEscolha {
		teclas = c.esc.teclas()
	}
	corpo += "\n\n" + lipgloss.NewStyle().Width(larg).Inherit(stDica).Render(teclas)
	if f.erro != "" {
		corpo += "\n\n" + lipgloss.NewStyle().Width(larg).Inherit(stErro).Render(f.erro)
	}
	caixa := stModal.BorderForeground(corDestaque).Width(larg + 6).Render(corpo)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, caixa)
}

// --- confirmação ------------------------------------------------------------------------------

type resultadoConf int

const (
	confNada resultadoConf = iota
	confSim
	confNao
)

// confirmacao pede "y" nas ações comuns e, nas críticas, que se DIGITE a palavra exata (o nome do
// banco): ser root não dispensa a confirmação, só a torna rápida.
type confirmacao struct {
	ativa   bool
	critica bool
	titulo  string
	corpo   string
	palavra string
	erro    string
	entrada textinput.Model
	acao    func() tea.Cmd
	cancela func() tea.Cmd
}

func (c *confirmacao) perguntar(titulo, corpo string, acao func() tea.Cmd) {
	*c = confirmacao{ativa: true, titulo: titulo, corpo: corpo, acao: acao}
}

func (c *confirmacao) perguntarCritico(titulo, corpo, palavra string, acao func() tea.Cmd) tea.Cmd {
	ti := textinput.New()
	ti.CharLimit = 128
	ti.Width = 40
	ti.Prompt = "› "
	*c = confirmacao{ativa: true, critica: true, titulo: titulo, corpo: corpo, palavra: palavra, entrada: ti, acao: acao}
	return c.entrada.Focus()
}

func (c *confirmacao) fechar() { *c = confirmacao{} }

func (c *confirmacao) atualizar(msg tea.KeyMsg) (resultadoConf, tea.Cmd) {
	if c.critica {
		switch msg.Type {
		case tea.KeyEsc:
			return confNao, nil
		case tea.KeyEnter:
			if c.entrada.Value() == c.palavra {
				return confSim, nil
			}
			c.erro = "não confere: digite exatamente " + c.palavra
			return confNada, nil
		}
		c.erro = ""
		var cmd tea.Cmd
		c.entrada, cmd = c.entrada.Update(msg)
		return confNada, cmd
	}
	// Só "y" confirma: uma janela que aparece sozinha (um resultado que chegou) não pode ser
	// aceita por uma tecla de outra coisa, como o enter.
	switch strings.ToLower(msg.String()) {
	case "y":
		return confSim, nil
	case "n", "esc", "q":
		return confNao, nil
	}
	return confNada, nil
}

func (c *confirmacao) view(w, h int) string {
	cor := corAviso
	if c.critica {
		cor = corPerigo
	}
	larg := limitar(w-8, 40, 86)
	corpo := faixa(cor, c.titulo) + "\n\n" + lipgloss.NewStyle().Width(larg).Render(c.corpo) + "\n\n"
	if c.critica {
		corpo += stRotulo.Render("Para confirmar, digite ") + stValor.Render(c.palavra) + "\n" + c.entrada.View() + "\n\n"
		if c.erro != "" {
			corpo += stErro.Render(c.erro) + "\n\n"
		}
		corpo += dica("enter", "confirmar") + "   " + dica("esc", "cancelar")
	} else {
		corpo += dica("y", "confirmar") + "   " + dica("n/esc", "cancelar")
	}
	caixa := stModal.BorderForeground(cor).Width(larg + 6).Render(corpo)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, caixa)
}

// --- janela -----------------------------------------------------------------------------------

// janela mostra uma mensagem longa (um erro inteiro, um diagnóstico, um log) com rolagem.
type janela struct {
	ativa     bool
	titulo    string
	perigo    bool
	vp        viewport.Model
	recarrega func() string // o log se atualiza enquanto a janela está aberta
	lido      string
}

func (j *janela) mostrar(w, h int, titulo, corpo string, perigo bool) {
	larg := limitar(w-12, 30, 120)
	conteudo := lipgloss.NewStyle().Width(larg).Render(corpo)
	vp := viewport.New(larg, limitar(lipgloss.Height(conteudo), 3, limitar(h-10, 3, 40)))
	vp.SetContent(conteudo)
	*j = janela{ativa: true, titulo: titulo, perigo: perigo, vp: vp}
}

// mostrarLog abre a janela no fim do texto, e a recarrega a cada segundo.
func (j *janela) mostrarLog(w, h int, titulo string, ler func() string) {
	larg := limitar(w-8, 30, 200)
	vp := viewport.New(larg, limitar(h-8, 3, 200))
	*j = janela{ativa: true, titulo: titulo, vp: vp, recarrega: ler}
	j.atualizarConteudo()
	j.vp.GotoBottom()
}

func (j *janela) atualizarConteudo() {
	if j.recarrega == nil {
		return
	}
	novo := j.recarrega()
	if novo == j.lido {
		return // o log não mudou: não refaz a quebra de linhas (ele pode ter milhares)
	}
	j.lido = novo
	noFim := j.vp.AtBottom()
	j.vp.SetContent(lipgloss.NewStyle().Width(j.vp.Width).Render(novo))
	if noFim {
		j.vp.GotoBottom()
	}
}

func (j *janela) atualizar(msg tea.KeyMsg) {
	switch msg.String() {
	case "esc", "enter", "q":
		j.ativa = false
		return
	case "g", "home":
		j.vp.GotoTop()
		return
	case "G", "end":
		j.vp.GotoBottom()
		return
	}
	j.vp, _ = j.vp.Update(msg)
}

func (j *janela) view(w, h int) string {
	cor := corDestaque
	if j.perigo {
		cor = corPerigo
	}
	d := dica("esc", "fechar")
	if j.vp.TotalLineCount() > j.vp.Height {
		d += "   " + dica("↑↓ pgup pgdn", "rolar") + "   " + dica("g/G", "início/fim")
	}
	caixa := stModal.BorderForeground(cor).Render(faixa(cor, j.titulo) + "\n\n" + j.vp.View() + "\n\n" + d)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, caixa)
}

// --- utilidades -------------------------------------------------------------------------------

// faixa é o título colorido das janelas.
func faixa(cor lipgloss.TerminalColor, texto string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(corSobre).Background(cor).Padding(0, 1).Render(" " + texto + " ")
}

// ajustarAltura prende o corpo da aba em exatamente h linhas: mais alto empurraria o cabeçalho para
// fora da tela; mais baixo deixaria o rodapé no meio dela.
func ajustarAltura(s string, h int) string {
	l := strings.Split(s, "\n")
	if len(l) > h {
		l = l[:h]
	}
	for len(l) < h {
		l = append(l, "")
	}
	return strings.Join(l, "\n")
}

// recortar corta cada linha na largura, para nada quebrar e desalinhar a tela.
func recortar(s string, w int) string {
	l := strings.Split(s, "\n")
	for i := range l {
		if lipgloss.Width(l[i]) > w {
			l[i] = truncar(l[i], w)
		}
	}
	return strings.Join(l, "\n")
}

func preencher(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncar corta na largura de exibição, com reticências. Preserva as cores (ANSI).
func truncar(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w-1).Render(s) + "…"
}

func limitar(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	return min(max(v, lo), hi)
}

// quebrar escreve "prefixo texto" quebrando o texto na largura, com as linhas seguintes alinhadas
// depois do prefixo.
func quebrar(prefixo, texto string, w int) string {
	pw := lipgloss.Width(prefixo)
	resto := max(w-pw, 20)
	l := strings.Split(lipgloss.NewStyle().Width(resto).Render(texto), "\n")
	for i := range l {
		l[i] = strings.TrimRight(l[i], " ")
		if i > 0 {
			l[i] = strings.Repeat(" ", pw) + l[i]
		}
	}
	return prefixo + strings.Join(l, "\n")
}

// janelaDeLinhas devolve as linhas visíveis de uma lista com cursor: a janela acompanha o cursor.
func janelaDeLinhas(total, cursor, altura int) (ini, fim int) {
	if altura <= 0 || total <= altura {
		return 0, total
	}
	ini = max(0, min(cursor-altura/2, total-altura))
	return ini, ini + altura
}
