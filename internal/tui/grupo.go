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

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
)

// telaGrupo é a confirmação de um grupo de perfis: os planos lado a lado, as cópias em fila, uma de
// cada vez. Com um destino homolog no grupo, a confirmação é a palavra "copiar" digitada.
type telaGrupo struct {
	planos  []motor.Plano
	entrada textinput.Model
	vp      viewport.Model
	erro    string
}

const palavraGrupo = "copiar"

func (g *telaGrupo) homolog() bool {
	for _, p := range g.planos {
		if p.Destino.Tag == cadastro.TagHomolog {
			return true
		}
	}
	return false
}

func (g *telaGrupo) bloqueados() int {
	n := 0
	for _, p := range g.planos {
		if p.Bloqueado() {
			n++
		}
	}
	return n
}

// copiarGrupo planeja os perfis marcados (um de cada vez) e abre a confirmação do grupo.
func (m *Model) copiarGrupo(nomes []string) tea.Cmd {
	sort.Strings(nomes)
	return m.executar(&tarefa{
		rotulo: fmt.Sprintf("checando os %d perfis do grupo", len(nomes)),
		rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
			var ps []motor.Plano
			for _, n := range nomes {
				p, err := m.o.Planejar(ctx, n, seg)
				if err != nil {
					return nil, err // uma pergunta: a tela pergunta e checa o grupo todo de novo
				}
				ps = append(ps, p)
			}
			return ps, nil
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("Não foi possível checar o grupo", err)
				return nil
			}
			m.recarregar()
			g := &telaGrupo{planos: v.([]motor.Plano)}
			g.entrada = textinput.New()
			g.entrada.Prompt = "› "
			g.entrada.CharLimit = 32
			m.grupo = g
			if g.homolog() && g.bloqueados() == 0 {
				return g.entrada.Focus()
			}
			return nil
		},
	})
}

func (g *telaGrupo) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.grupo = nil
		m.status = "grupo não iniciado"
		return nil
	case "pgup", "pgdown", "up", "down":
		g.vp, _ = g.vp.Update(k)
		return nil
	case "enter", "y":
		if g.bloqueados() > 0 {
			g.erro = "há perfis bloqueados no grupo: desmarque-os (esc, espaço) ou resolva os bloqueios"
			return nil
		}
		if g.homolog() {
			if k.String() == "y" {
				break
			}
			if g.entrada.Value() != palavraGrupo {
				g.erro = "há destino homolog no grupo: para confirmar, digite " + palavraGrupo
				return nil
			}
		} else if k.String() != "y" {
			g.erro = "para copiar, tecle y"
			return nil
		}
		planos := g.planos
		m.grupo = nil
		return m.executar(&tarefa{
			rotulo: "iniciando o grupo",
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				_, ids, err := m.o.IniciarGrupo(ctx, planos, seg)
				return ids, err
			},
			pronto: func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					m.erro("O grupo não começou", err)
					return nil
				}
				m.perfisA.marcados = map[string]bool{}
				m.recarregar()
				m.aba = AbaExecucoes
				ids := v.([]int64)
				for i, e := range m.execucoes {
					if e.ID == ids[0] {
						m.execA.cursor = i
					}
				}
				m.status = fmt.Sprintf("grupo iniciado: %d cópias em fila, uma de cada vez, fora da tela", len(ids))
				return nil
			},
		})
	}
	if g.homolog() {
		g.erro = ""
		var cmd tea.Cmd
		g.entrada, cmd = g.entrada.Update(k)
		return cmd
	}
	return nil
}

func (g *telaGrupo) corpo(w int) string {
	var b strings.Builder
	for i, p := range g.planos {
		b.WriteString(fmt.Sprintf("%s %s\n", stMarca.Render(fmt.Sprintf("%d.", i+1)), stValor.Render(p.Perfil.Nome)))
		orig := fmt.Sprintf("%s/%s", p.Origem.Conexao, p.Origem.Banco)
		dest := seloTag(p.Destino.Tag) + " " + stTexto.Render(fmt.Sprintf("%s/%s", p.Destino.Conexao, p.Destino.Banco))
		b.WriteString(quebrar("   ", stDica.Render(orig+" → ")+dest, w) + "\n")
		if p.Destino.Existe {
			b.WriteString("   " + stPerigoV.Render("vai ser SUBSTITUÍDO") + stDica.Render(" (o atual vira "+nomes.PrefixoAnteriores(p.Destino.Banco)+"<data>)") + "\n")
		}
		for _, x := range p.Bloqueios {
			b.WriteString(quebrar(stPerigoV.Render("   ✖ "), stTexto.Render(x), w) + "\n")
		}
		for _, x := range p.Avisos {
			b.WriteString(quebrar(stAvisoV.Render("   ! "), stTexto.Render(x), w) + "\n")
		}
		for _, x := range p.Notas {
			b.WriteString(quebrar(stDica.Render("   · "), stDica.Render(x), w) + "\n")
		}
		if len(p.Anteriores) > 0 {
			var t int64
			for _, a := range p.Anteriores {
				t += a.Tamanho
			}
			b.WriteString(stDica.Render(fmt.Sprintf("   %d anterior(es) de %s ocupam %s: num grupo, nada é apagado (decida na aba 5)", len(p.Anteriores), p.Destino.Banco, motor.Tamanho(t))) + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (g *telaGrupo) view(m *Model) string {
	w := limitar(m.largura-6, 40, 150)
	titulo := faixa(corDestaque, fmt.Sprintf("Copiar %d perfis em fila", len(g.planos)))
	if g.bloqueados() > 0 {
		titulo = faixa(corPerigo, fmt.Sprintf("Copiar %d perfis: %d bloqueado(s)", len(g.planos), g.bloqueados()))
	}
	var rod strings.Builder
	rod.WriteString(stDica.Render("As cópias rodam uma de cada vez, fora da tela; uma que falhe não para as outras.") + "\n")
	switch {
	case g.bloqueados() > 0:
		rod.WriteString(dica("esc", "voltar") + stDica.Render("   desmarque os bloqueados (espaço) ou resolva os bloqueios"))
	case g.homolog():
		rod.WriteString(stRotulo.Render("Há destino homolog no grupo: para copiar, digite ") + stValor.Render(palavraGrupo) + "\n" + g.entrada.View() + "\n" +
			juntarDicas(dica("enter", "copiar"), dica("esc", "cancelar"), dica("↑↓", "rolar")))
	default:
		rod.WriteString(juntarDicas(dica("y", "copiar"), dica("esc", "cancelar"), dica("↑↓", "rolar")))
	}
	if g.erro != "" {
		rod.WriteString("\n" + stErro.Render(g.erro))
	}
	rodape := recortar(rod.String(), w)
	g.vp.Width = w
	g.vp.Height = max(m.altura-6-lipgloss.Height(rodape), 3)
	g.vp.SetContent(g.corpo(w))
	tela := titulo + "\n\n" + g.vp.View() + "\n\n" + rodape
	return lipgloss.NewStyle().Padding(0, 2).Render(ajustarAltura(recortar(tela, w+2), m.altura))
}
