package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// itemEscolha é uma etiqueta do seletor.
type itemEscolha struct {
	valor   string
	rotulo  string // o texto da etiqueta; vazio é o valor
	extra   string // ao lado do rótulo, em tom apagado (o tamanho)
	detalhe string // a linha embaixo das etiquetas, quando esta está em foco
	aviso   bool   // na cor de aviso (o banco do destino, que é substituído)
	apagado bool   // não pode ser marcada
	livre   bool   // fora da lista: o texto do filtro, ou um valor marcado que a lista não tem
}

func (i itemEscolha) texto() string {
	if i.rotulo != "" {
		return i.rotulo
	}
	return i.valor
}

// escolha é o seletor de etiquetas de um campo: as letras filtram, o espaço marca, e as setas andam
// pelas etiquetas. Com multi, marca várias; sem, a marcada troca.
type escolha struct {
	itens func(f *formulario) []itemEscolha
	multi bool
	// livre, se não vazio, é o rótulo (com um %q) da etiqueta que usa o texto do filtro como valor,
	// para um nome que a lista não tem.
	livre string
	// info vai no cabeçalho: de quando é a lista.
	info func(f *formulario) string
	// reler, se houver, é o ctrl+r: lê a lista de novo.
	reler func(f *formulario) tea.Cmd
	// resumo, se houver, vai depois das marcadas quando o seletor está fechado.
	resumo func(n int) string

	marcados []string
	filtro   string
	cursor   int
	aviso    string // o resultado do ctrl+r
}

// maxLinhasEscolha é quantas linhas de etiquetas aparecem de uma vez; o filtro encurta o resto.
const maxLinhasEscolha = 6

func (e *escolha) marcado(v string) bool {
	for _, m := range e.marcados {
		if m == v {
			return true
		}
	}
	return false
}

// visiveis são as etiquetas que passam no filtro. Um valor marcado que a lista não tem (a
// verificação é de antes dele) continua à vista, e o filtro sem par vira a etiqueta livre.
func (e *escolha) visiveis(f *formulario) []itemEscolha {
	todos := e.itens(f)
	conhecidos := map[string]bool{}
	for _, it := range todos {
		conhecidos[it.valor] = true
	}
	for _, v := range e.marcados {
		if !conhecidos[v] {
			todos = append(todos, itemEscolha{valor: v, detalhe: v + ": não visto na última verificação", livre: true})
			conhecidos[v] = true
		}
	}
	filtro := strings.ToLower(e.filtro)
	var r []itemEscolha
	for _, it := range todos {
		if strings.Contains(strings.ToLower(it.valor), filtro) || strings.Contains(strings.ToLower(it.texto()), filtro) {
			r = append(r, it)
		}
	}
	if e.livre != "" && e.filtro != "" && !conhecidos[e.filtro] {
		r = append(r, itemEscolha{valor: e.filtro, rotulo: fmt.Sprintf(e.livre, e.filtro), livre: true})
	}
	return r
}

// textos são os textos das etiquetas marcadas, na ordem da marcação.
func (e *escolha) textos(f *formulario) []string {
	rot := map[string]string{}
	for _, it := range e.itens(f) {
		rot[it.valor] = it.texto()
	}
	var r []string
	for _, v := range e.marcados {
		if t, ok := rot[v]; ok {
			r = append(r, t)
		} else {
			r = append(r, v)
		}
	}
	return r
}

// tecla trata a tecla no seletor. ok falso devolve a tecla ao formulário (enter, tab, esc sem
// filtro).
func (e *escolha) tecla(f *formulario, k tea.KeyMsg) (resultadoForm, tea.Cmd, bool) {
	its := e.visiveis(f)
	e.cursor = limitar(e.cursor, 0, len(its)-1)
	tecla := k.String()
	// Com o filtro aberto, o enter marca a etiqueta em destaque, como o espaço, em vez de salvar o
	// formulário sem ela: o segundo enter salva.
	if tecla == "enter" && e.filtro != "" {
		tecla = " "
	}
	switch tecla {
	case "esc":
		if e.filtro == "" {
			return formNada, nil, false
		}
		// O esc limpa o filtro antes de fechar o formulário: um filtro não custa o formulário.
		e.filtro, e.cursor = "", 0
		return formNada, nil, true
	case "left":
		e.cursor = max(e.cursor-1, 0)
		return formNada, nil, true
	case "right":
		e.cursor = min(e.cursor+1, max(len(its)-1, 0))
		return formNada, nil, true
	case "up", "down":
		d := 1
		if tecla == "up" {
			d = -1
		}
		// Na primeira ou na última linha, as setas saem para o campo vizinho, como nos outros.
		pos := posicoes(e, its, larguraEtiquetas(f.largura))
		if len(its) == 0 {
			return formNada, f.mover(d), true
		}
		alvo := pos[e.cursor].linha + d
		melhor := -1
		for i, p := range pos {
			if p.linha == alvo && (melhor < 0 || abs(p.coluna-pos[e.cursor].coluna) < abs(pos[melhor].coluna-pos[e.cursor].coluna)) {
				melhor = i
			}
		}
		if melhor < 0 {
			return formNada, f.mover(d), true
		}
		e.cursor = melhor
		return formNada, nil, true
	case " ":
		if len(its) == 0 || its[e.cursor].apagado {
			return formNada, nil, true
		}
		v := its[e.cursor].valor
		switch {
		case !e.multi:
			e.marcados = []string{v}
		case e.marcado(v):
			e.desmarcar(v)
		default:
			e.marcados = append(e.marcados, v)
		}
		// Marcada pelo filtro, a etiqueta volta à lista inteira, com o foco nela: o próximo nome
		// começa do zero.
		if e.filtro != "" {
			e.filtro = ""
			for i, it := range e.visiveis(f) {
				if it.valor == v {
					e.cursor = i
				}
			}
		}
		return formNada, nil, true
	case "ctrl+a":
		if !e.multi {
			return formNada, nil, true
		}
		var marcaveis []string
		todos := true
		for _, it := range its {
			if it.apagado || (it.livre && !e.marcado(it.valor)) {
				continue
			}
			marcaveis = append(marcaveis, it.valor)
			todos = todos && e.marcado(it.valor)
		}
		for _, v := range marcaveis {
			switch {
			case todos:
				e.desmarcar(v)
			case !e.marcado(v):
				e.marcados = append(e.marcados, v)
			}
		}
		return formNada, nil, true
	case "ctrl+r":
		if e.reler == nil {
			return formNada, nil, true
		}
		e.aviso = ""
		return formNada, e.reler(f), true
	case "backspace":
		if r := []rune(e.filtro); len(r) > 0 {
			e.filtro, e.cursor = string(r[:len(r)-1]), 0
		}
		return formNada, nil, true
	}
	if k.Type == tea.KeyRunes && !k.Alt {
		e.filtro += string(k.Runes)
		e.cursor = 0
		return formNada, nil, true
	}
	return formNada, nil, false
}

func (e *escolha) desmarcar(v string) {
	var r []string
	for _, m := range e.marcados {
		if m != v {
			r = append(r, m)
		}
	}
	e.marcados = r
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// larguraEtiquetas é a largura da área das etiquetas no formulário.
func larguraEtiquetas(largTela int) int { return larguraForm(largTela) - 4 }

const sepEtiquetas = "   "

// textoEtiqueta é a etiqueta sem cores: a marca, o rótulo e o extra.
func textoEtiqueta(e *escolha, it itemEscolha) string {
	s := "  " + it.texto()
	if e.marcado(it.valor) {
		s = "✔ " + it.texto()
	}
	if it.extra != "" {
		s += "  " + it.extra
	}
	return s
}

type posicao struct{ linha, coluna int }

// posicoes arruma as etiquetas em linhas corridas na largura: a mesma conta serve ao desenho e às
// setas.
func posicoes(e *escolha, its []itemEscolha, larg int) []posicao {
	pos := make([]posicao, len(its))
	linha, col := 0, 0
	for i, it := range its {
		w := min(lipgloss.Width(textoEtiqueta(e, it)), larg)
		if col > 0 && col+len(sepEtiquetas)+w > larg {
			linha, col = linha+1, 0
		}
		if col > 0 {
			col += len(sepEtiquetas)
		}
		pos[i] = posicao{linha, col}
		col += w
	}
	return pos
}

var stEtiquetaFoco = lipgloss.NewStyle().Foreground(corSobre).Background(corDestaque).Bold(true)

// desenhar é o seletor em foco: a linha do filtro, as etiquetas e o detalhe da etiqueta em foco.
func (e *escolha) desenhar(f *formulario, larg int) []string {
	its := e.visiveis(f)
	e.cursor = limitar(e.cursor, 0, len(its)-1)
	total := len(e.itens(f))

	filtro := stDica.Render("digite para filtrar")
	if e.filtro != "" {
		filtro = stValor.Render(e.filtro) + stTecla.Render("▏")
	}
	resumo := fmt.Sprintf("%d de %d", len(its), total)
	if e.multi && len(e.marcados) > 0 {
		resumo += fmt.Sprintf(" · %d marcado(s)", len(e.marcados))
	}
	if e.info != nil {
		if s := e.info(f); s != "" {
			resumo += " · " + s
		}
	}
	linhas := []string{"    " + filtro + "   " + stDica.Render(resumo), ""}

	if len(its) == 0 {
		linhas = append(linhas, "    "+stDica.Render("nenhum banco com esse filtro (backspace apaga, esc limpa)"))
	} else {
		pos := posicoes(e, its, larg)
		porLinha := map[int][]int{}
		ultima := 0
		for i, p := range pos {
			porLinha[p.linha] = append(porLinha[p.linha], i)
			ultima = max(ultima, p.linha)
		}
		ini, fim := janelaDeLinhas(ultima+1, pos[e.cursor].linha, maxLinhasEscolha)
		if ini > 0 {
			linhas = append(linhas, "    "+stDica.Render(fmt.Sprintf("↑ mais %d linha(s)", ini)))
		}
		for l := ini; l < fim; l++ {
			var partes []string
			for _, i := range porLinha[l] {
				partes = append(partes, e.etiqueta(its[i], i == e.cursor, larg))
			}
			linhas = append(linhas, "    "+strings.Join(partes, sepEtiquetas))
		}
		if fim <= ultima {
			linhas = append(linhas, "    "+stDica.Render(fmt.Sprintf("↓ mais %d linha(s)", ultima+1-fim)))
		}
		if d := its[e.cursor].detalhe; d != "" {
			linhas = append(append(linhas, ""), recuar(lipgloss.NewStyle().Width(larg).Foreground(corApagada).Render(d))...)
		}
	}
	if e.aviso != "" {
		linhas = append(linhas, recuar(lipgloss.NewStyle().Width(larg).Foreground(corAviso).Render(e.aviso))...)
	}
	return linhas
}

// recuar põe cada linha no recuo das etiquetas.
func recuar(s string) []string {
	var r []string
	for _, l := range strings.Split(s, "\n") {
		r = append(r, "    "+l)
	}
	return r
}

func (e *escolha) etiqueta(it itemEscolha, foco bool, larg int) string {
	if foco {
		return stEtiquetaFoco.Render(truncar(textoEtiqueta(e, it), larg))
	}
	nome := stTexto.Render(it.texto())
	switch {
	case it.apagado:
		nome = stDica.Render(it.texto())
	case it.aviso:
		nome = stAvisoV.Render(it.texto())
	case it.livre:
		nome = stTecla.Render(it.texto())
	case e.marcado(it.valor):
		nome = stValor.Render(it.texto())
	}
	marca := "  "
	if e.marcado(it.valor) {
		marca = stOk.Render("✔ ")
	}
	s := marca + nome
	if it.extra != "" {
		s += "  " + stDica.Render(it.extra)
	}
	return truncar(s, larg)
}

// resumir é o seletor fora de foco: as marcadas numa linha.
func (e *escolha) resumir(f *formulario, larg int) string {
	ts := e.textos(f)
	if len(ts) == 0 {
		return stDica.Render("nenhum (espaço marca)")
	}
	fim := ""
	switch {
	case e.resumo != nil:
		fim = e.resumo(len(ts))
	case len(ts) > 1:
		fim = fmt.Sprintf("(%d)", len(ts))
	}
	// O fim fica à vista mesmo quando os nomes não cabem.
	if fim != "" {
		fim = "  " + fim
	}
	return truncar(stValor.Render(strings.Join(ts, ", ")), max(larg-lipgloss.Width(fim), 10)) + stDica.Render(fim)
}

// teclasEscolha é a linha das teclas com o seletor em foco.
func (e *escolha) teclas() string {
	ts := []string{"espaço marca", "letras filtram", "←→↑↓ andam"}
	if e.multi {
		ts = append(ts, "ctrl+a marca os filtrados")
	}
	if e.reler != nil {
		ts = append(ts, "ctrl+r relê")
	}
	if e.filtro != "" {
		return strings.Join(append(ts, "enter marca", "esc limpa o filtro", "tab campos"), " · ")
	}
	return strings.Join(append(ts, "esc cancelar", "tab campos", "enter salvar"), " · ")
}
