package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/execucao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// telaPlano é a confirmação da cópia: tudo o que vai acontecer, os bloqueios, os avisos, os
// anteriores (com a pergunta de quais apagar) e a confirmação, que num destino homolog é o nome do
// banco digitado.
type telaPlano struct {
	p        motor.Plano
	marcados map[string]bool
	cursor   int
	focoAnt  bool // o foco está na lista dos anteriores (e não no campo da confirmação)
	entrada  textinput.Model
	vp       viewport.Model
	erro     string
}

func (m *Model) abrirPlano(p motor.Plano) tea.Cmd {
	t := &telaPlano{p: p, marcados: map[string]bool{}}
	t.entrada = textinput.New()
	t.entrada.Prompt = "› "
	t.entrada.CharLimit = 128
	t.entrada.Width = 40
	// Sem campo (dev) ou sem nada para digitar, o foco começa nos anteriores.
	t.focoAnt = p.Confirmacao() == "" && len(p.Anteriores) > 0
	m.plano = t
	t.redimensionar(m)
	if p.Confirmacao() != "" && !p.Bloqueado() {
		return t.entrada.Focus()
	}
	return nil
}

func (t *telaPlano) redimensionar(m *Model) {
	t.vp = viewport.New(limitar(m.largura-6, 40, 150), max(m.altura-9, 5))
	t.vp.SetContent(t.corpo(m))
}

func (t *telaPlano) apagar() []string {
	var ns []string
	for n, ok := range t.marcados {
		if ok {
			ns = append(ns, n)
		}
	}
	sort.Strings(ns)
	return ns
}

func (t *telaPlano) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	p := t.p
	switch k.String() {
	case "esc":
		m.plano = nil
		m.status = "cópia não iniciada"
		return nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		t.vp, _ = t.vp.Update(k)
		return nil
	case "tab", "shift+tab":
		if len(p.Anteriores) > 0 && p.Confirmacao() != "" {
			t.focoAnt = !t.focoAnt
			if t.focoAnt {
				t.entrada.Blur()
				return nil
			}
			return t.entrada.Focus()
		}
		return nil
	case "up", "down":
		if len(p.Anteriores) > 0 {
			if k.String() == "up" {
				t.cursor = max(t.cursor-1, 0)
			} else {
				t.cursor = min(t.cursor+1, len(p.Anteriores)-1)
			}
			t.vp.SetContent(t.corpo(m))
			return nil
		}
		t.vp, _ = t.vp.Update(k)
		return nil
	case " ":
		if len(p.Anteriores) > 0 && (t.focoAnt || p.Confirmacao() == "") {
			n := p.Anteriores[t.cursor].Nome
			t.marcados[n] = !t.marcados[n]
			t.vp.SetContent(t.corpo(m))
			return nil
		}
	case "enter", "y":
		if k.String() == "y" && p.Confirmacao() != "" && !t.focoAnt {
			break // no campo, o y é uma letra
		}
		// Num destino dev, só o y confirma: o plano aparece sozinho quando fica pronto, e um enter
		// dado para outra coisa não pode substituir o banco.
		if k.String() == "enter" && p.Confirmacao() == "" {
			if !p.Bloqueado() {
				t.erro = "para " + verbo(p) + ", tecle y"
			}
			return nil
		}
		if p.Bloqueado() {
			t.erro = "bloqueado: resolva os itens em vermelho e tente de novo"
			return nil
		}
		if c := p.Confirmacao(); c != "" && t.entrada.Value() != c {
			t.erro = "para confirmar, digite exatamente " + c
			if t.focoAnt {
				t.focoAnt = false
				return t.entrada.Focus()
			}
			return nil
		}
		return t.confirmar(m)
	}
	if p.Confirmacao() != "" && !t.focoAnt && !p.Bloqueado() {
		t.erro = ""
		var cmd tea.Cmd
		t.entrada, cmd = t.entrada.Update(k)
		return cmd
	}
	return nil
}

func (t *telaPlano) confirmar(m *Model) tea.Cmd {
	p, apagar := t.p, t.apagar()
	m.plano = nil
	return m.executar(&tarefa{
		rotulo: "iniciando a cópia de " + p.Perfil.Nome,
		rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
			return m.o.Iniciar(ctx, execucao.Pedido{Plano: p, Apagar: apagar, Segredos: seg})
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("A cópia não começou", err)
				return nil
			}
			id := v.(int64)
			m.recarregar()
			m.aba = AbaExecucoes
			for i, e := range m.execucoes {
				if e.ID == id {
					m.execA.cursor = i
				}
			}
			m.status = fmt.Sprintf("cópia #%d iniciada: ela roda fora da tela, e pode fechar a tela sem pará-la", id)
			return nil
		},
	})
}

// --- desenho ------------------------------------------------------------------------------------

func (t *telaPlano) corpo(m *Model) string {
	p := t.p
	w := t.vp.Width
	var b strings.Builder
	secao := func(rot, val string) {
		b.WriteString(quebrar(stCabecalho.Render(preencher(rot, 11)), val, w) + "\n")
	}
	versao := func(n int) string {
		if n == 0 {
			return ""
		}
		return stDica.Render(" · PostgreSQL " + versoes.Texto(n))
	}

	if p.Reset {
		if p.Base != nil {
			secao("BASE", stValor.Render(p.Base.Nome)+stDica.Render(" · "+motor.Tamanho(p.Base.Tamanho)+" · "+p.BaseDescricao))
		}
	} else if p.DumpGuardado != "" {
		secao("DUMP", stValor.Render(p.DumpGuardado)+stDica.Render(fmt.Sprintf(" · da origem %s/%s · a origem não é tocada", p.Origem.Conexao, p.Origem.Banco)))
	} else if p.Origem.Conexao != "" {
		o := seloTag(p.Origem.Tag) + " " + stValor.Render(p.Origem.Conexao) + stDica.Render(" · "+p.Origem.Onde) + versao(p.Origem.VersaoNum)
		o += "\n" + stRotulo.Render("banco ") + stValor.Render(p.Origem.Banco)
		if p.Origem.Existe {
			o += stDica.Render(" · " + motor.Tamanho(p.Origem.Info.Tamanho))
		}
		if p.Origem.Recuperacao {
			o += stDica.Render(" · réplica")
		}
		secao("ORIGEM", o)
	}
	if p.Destino.Conexao != "" {
		d := seloTag(p.Destino.Tag) + " " + stValor.Render(p.Destino.Conexao) + stDica.Render(" · "+p.Destino.Onde) + versao(p.Destino.VersaoNum)
		d += "\n" + stRotulo.Render("banco ") + stValor.Render(p.Destino.Banco)
		if p.Destino.Existe {
			d += stDica.Render(" · "+motor.Tamanho(p.Destino.Info.Tamanho)) + " " + stPerigoV.Render("vai ser SUBSTITUÍDO") +
				stDica.Render(" (o atual vira "+nomes.PrefixoAnteriores(p.Destino.Banco)+"<data>, fechado para conexões)")
		} else {
			d += " " + stAvisoV.Render("não existe: vai ser criado")
		}
		secao("DESTINO", d)
	}
	if p.Imagem > 0 && !p.Reset {
		secao("IMAGEM", stTexto.Render(fmt.Sprintf("postgres:%d", p.Imagem))+stDica.Render(fmt.Sprintf(" · cliente %s · faz o dump e o restore", p.Cliente)))
	}
	if p.DumpGuardado == "" && !p.Reset {
		secao("DUMP", stTexto.Render(p.DirDumps+"/<data>")+stDica.Render(" · fica guardado (nada é apagado sozinho)"))
	}
	var ops []string
	ops = append(ops, fmt.Sprintf("jobs %d no dump, %d no restore, %s", p.Perfil.JobsDump, p.Perfil.JobsRestore, orDefault(p.Perfil.Compressao, "zstd")))
	if p.Perfil.ConferirLinhas {
		ops = append(ops, "confere as linhas")
	}
	if p.Perfil.Retomavel {
		ops = append(ops, "link instável (em blocos, retomável)")
	}
	if len(p.Perfil.SemDados) > 0 {
		ops = append(ops, "sem dados: "+strings.Join(p.Perfil.SemDados, ", "))
	}
	if p.Perfil.Script != "" {
		ops = append(ops, "script pós-restore: "+p.Perfil.Script)
	}
	if !p.Reset {
		secao("OPÇÕES", stTexto.Render(strings.Join(ops, " · ")))
	}
	if len(p.Contagem) > 0 {
		var cs []string
		for _, k := range []string{"schemas", "tabelas", "visões", "visões materializadas", "sequências", "índices", "funções", "gatilhos", "extensões"} {
			if n := p.Contagem[k]; n > 0 {
				cs = append(cs, fmt.Sprintf("%d %s", n, k))
			}
		}
		secao("CONTEÚDO", stDica.Render(strings.Join(cs, " · ")))
	}
	if len(p.Sessoes) > 0 {
		var ss []string
		for i, s := range p.Sessoes {
			if i == 4 {
				ss = append(ss, fmt.Sprintf("e mais %d", len(p.Sessoes)-4))
				break
			}
			ss = append(ss, fmt.Sprintf("%s@%s (%s)", s.Usuario, s.Cliente, orDefault(s.Aplicacao, "?")))
		}
		secao("SESSÕES", stAvisoV.Render(fmt.Sprintf("%d conexão(ões) no destino serão derrubadas na troca: ", len(p.Sessoes)))+stDica.Render(strings.Join(ss, ", ")))
	}

	if len(p.Bloqueios) > 0 {
		b.WriteString("\n" + stPerigoV.Render("BLOQUEADO") + "\n")
		for _, x := range p.Bloqueios {
			b.WriteString(quebrar(stPerigoV.Render("  ✖ "), stTexto.Render(x), w) + "\n")
		}
	}
	if len(p.Avisos) > 0 {
		b.WriteString("\n" + stAvisoV.Render(fmt.Sprintf("ATENÇÃO (%d)", len(p.Avisos))) + "\n")
		for _, x := range p.Avisos {
			b.WriteString(quebrar(stAvisoV.Render("  ! "), stTexto.Render(x), w) + "\n")
		}
	}
	if len(p.Notas) > 0 {
		b.WriteString("\n" + stCabecalho.Render(fmt.Sprintf("INFORMAÇÕES (%d)", len(p.Notas))) + "\n")
		for _, x := range p.Notas {
			b.WriteString(quebrar(stDica.Render("  · "), stDica.Render(x), w) + "\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// anteriores é a lista dos __anterior do destino, fora da rolagem: ela fica sempre à vista, porque
// a cópia sempre pergunta o que fazer com eles.
func (t *telaPlano) anteriores(w, linhas int) string {
	p := t.p
	if len(p.Anteriores) == 0 {
		return ""
	}
	var total int64
	for _, a := range p.Anteriores {
		total += a.Tamanho
	}
	ativo := t.focoAnt || p.Confirmacao() == ""
	var b strings.Builder
	cab := fmt.Sprintf("ANTERIORES DE %s NO DESTINO: %d, %s", p.Destino.Banco, len(p.Anteriores), motor.Tamanho(total))
	b.WriteString(stCabecalho.Render(cab) + stDica.Render(" · marque com espaço os que quer APAGAR antes de começar") + "\n")
	ini, fim := janelaDeLinhas(len(p.Anteriores), t.cursor, linhas)
	if ini > 0 {
		b.WriteString(stDica.Render(fmt.Sprintf("    ↑ mais %d", ini)) + "\n")
	}
	for i := ini; i < fim; i++ {
		a := p.Anteriores[i]
		caixa := stDica.Render("[ ]")
		if t.marcados[a.Nome] {
			caixa = stPerigoV.Render("[x]")
		}
		cur := "  "
		nome := stTexto.Render(a.Nome)
		if i == t.cursor && ativo {
			cur, nome = stSelecao.Render("▸ "), stSelecao.Render(a.Nome)
		}
		b.WriteString(fmt.Sprintf("%s%s %s  %s %s\n", cur, caixa, nome, stDica.Render(preencher(motor.Tamanho(a.Tamanho), 9)), stDica.Render(idade(a.Data))))
	}
	if fim < len(p.Anteriores) {
		b.WriteString(stDica.Render(fmt.Sprintf("    ↓ mais %d", len(p.Anteriores)-fim)) + "\n")
	}
	return recortar(strings.TrimRight(b.String(), "\n"), w)
}

func (t *telaPlano) view(m *Model) string {
	p := t.p
	w := t.vp.Width
	acao := "Copiar "
	switch {
	case p.DumpGuardado != "":
		acao = "Restaurar o dump de novo: "
	case p.Reset:
		acao = "Resetar da base: "
	}
	titulo := faixa(corDestaque, acao+p.Perfil.Nome)
	if p.Bloqueado() {
		titulo = faixa(corPerigo, acao+p.Perfil.Nome+": bloqueado")
	}
	var rod strings.Builder
	if l := t.anteriores(w, limitar(m.altura/5, 2, 6)); l != "" {
		rod.WriteString(l + "\n\n")
	}
	apagar := t.apagar()
	if len(apagar) > 0 {
		var tam int64
		for _, a := range p.Anteriores {
			if t.marcados[a.Nome] {
				tam += a.Tamanho
			}
		}
		rod.WriteString(stPerigoV.Render(fmt.Sprintf("Antes de começar, %d anterior(es) serão APAGADOS (%s).", len(apagar), motor.Tamanho(tam))) + "\n")
	}
	switch {
	case p.Bloqueado():
		rod.WriteString(dica("esc", "voltar") + stDica.Render("   a cópia não pode começar: resolva os itens em vermelho"))
	case p.Confirmacao() != "":
		rod.WriteString(stRotulo.Render("Destino homolog: para "+verbo(p)+", digite o nome do banco ") + stValor.Render(p.Confirmacao()) + "\n" + t.entrada.View() + "\n")
		ds := []string{dica("enter", verbo(p)), dica("esc", "cancelar")}
		if len(p.Anteriores) > 0 {
			if t.focoAnt {
				ds = append(ds, dica("↑↓", "escolher"), dica("espaço", "marcar"), dica("tab", "nome"))
			} else {
				ds = append(ds, dica("tab", "marcar anteriores"))
			}
		}
		rod.WriteString(juntarDicas(ds...))
	default:
		ds := []string{dica("y", verbo(p)), dica("esc", "cancelar")}
		if len(p.Anteriores) > 0 {
			ds = append(ds, dica("↑↓", "escolher"), dica("espaço", "marcar"))
		}
		rod.WriteString(juntarDicas(ds...))
	}
	if t.vp.TotalLineCount() > t.vp.Height {
		rod.WriteString("   " + dica("pgup pgdn", "rolar"))
	}
	if t.erro != "" {
		rod.WriteString("\n" + stErro.Render(t.erro))
	}
	rodape := recortar(rod.String(), w)
	// O corpo encolhe para o rodapé caber.
	t.vp.Height = max(m.altura-6-lipgloss.Height(rodape), 3)
	t.vp.SetContent(t.corpo(m))
	tela := titulo + "\n\n" + t.vp.View() + "\n\n" + rodape
	return lipgloss.NewStyle().Padding(0, 2).Render(ajustarAltura(recortar(tela, w+2), m.altura))
}

// verbo é a ação do plano, para a confirmação.
func verbo(p motor.Plano) string {
	switch {
	case p.Reset:
		return "resetar"
	case p.DumpGuardado != "":
		return "restaurar"
	}
	return "copiar"
}
