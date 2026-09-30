package tui

import (
	"context"
	"fmt"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
)

// abaAnteriores mostra, por destino dos perfis, os bancos que a ferramenta deixou: os __anterior
// (desfazer ou apagar) e o __novo que sobrou de uma cópia que não terminou.
type abaAnteriores struct {
	cursor    int
	carregado bool
	destinos  []destinoAnt
}

type destinoAnt struct {
	conexao, banco string
	perfis         []string
	dados          motor.DaFerramenta
	erro           string
}

// item é uma linha selecionável: um anterior ou um __novo.
type item struct {
	d       int
	nome    string
	tamanho int64
	novo    bool
	base    bool
}

func (a *abaAnteriores) itens() []item {
	var is []item
	for i, d := range a.destinos {
		for _, x := range d.dados.Anteriores {
			is = append(is, item{d: i, nome: x.Nome, tamanho: x.Tamanho})
		}
		if d.dados.Novo != nil {
			is = append(is, item{d: i, nome: d.dados.Novo.Nome, tamanho: d.dados.Novo.Tamanho, novo: true})
		}
		if d.dados.Base != nil {
			is = append(is, item{d: i, nome: d.dados.Base.Nome, tamanho: d.dados.Base.Tamanho, base: true})
		}
	}
	return is
}

type resultadoAnteriores []destinoAnt

func (a *abaAnteriores) carregar(m *Model) tea.Cmd {
	// Os destinos dos perfis, sem repetição.
	vistos := map[string]int{}
	var ds []destinoAnt
	for _, p := range m.perfis {
		k := p.Destino + "\x00" + p.DestinoBanco
		if i, ok := vistos[k]; ok {
			ds[i].perfis = append(ds[i].perfis, p.Nome)
			continue
		}
		vistos[k] = len(ds)
		ds = append(ds, destinoAnt{conexao: p.Destino, banco: p.DestinoBanco, perfis: []string{p.Nome}})
	}
	return m.executar(&tarefa{
		rotulo: "lendo os bancos da ferramenta nos destinos",
		rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
			out := make([]destinoAnt, len(ds))
			copy(out, ds)
			for i := range out {
				r, err := m.o.Listar(ctx, out[i].conexao, out[i].banco, seg)
				if p := perguntaDe(err); p != nil {
					return nil, err // a tela pergunta e lê tudo de novo
				}
				if err != nil {
					out[i].erro = err.Error()
					continue
				}
				out[i].dados = r
			}
			return resultadoAnteriores(out), nil
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("Erro lendo os destinos", err)
				return nil
			}
			a.destinos, a.carregado = v.(resultadoAnteriores), true
			a.cursor = limitar(a.cursor, 0, max(len(a.itens())-1, 0))
			return nil
		},
	})
}

func (a *abaAnteriores) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	is := a.itens()
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(is)-1, 0))
	case "r":
		return a.carregar(m)
	case "u":
		if a.cursor >= len(is) || is[a.cursor].novo || is[a.cursor].base {
			return nil
		}
		it := is[a.cursor]
		d := a.destinos[it.d]
		return a.desfazer(m, d, it)
	case "d":
		if a.cursor >= len(is) {
			return nil
		}
		it := is[a.cursor]
		d := a.destinos[it.d]
		corpo := fmt.Sprintf("%s, em %s (%s).\n\nApagado, ele não volta.", it.nome, d.conexao, motor.Tamanho(it.tamanho))
		if it.novo {
			corpo = fmt.Sprintf("%s é o banco de uma cópia que não terminou, em %s (%s). O banco %s não é tocado.\n\nApagado, ele não volta.",
				it.nome, d.conexao, motor.Tamanho(it.tamanho), d.banco)
		}
		if it.base {
			corpo = fmt.Sprintf("%s é a base de %s em %s (%s): sem ela, o reset (tecla z nos perfis) só volta a funcionar depois da próxima cópia.\n\nApagada, ela não volta.",
				it.nome, d.banco, d.conexao, motor.Tamanho(it.tamanho))
		}
		apagar := func() tea.Cmd {
			return m.executar(&tarefa{
				rotulo: "apagando " + it.nome,
				rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
					return nil, m.o.Apagar(ctx, d.conexao, d.banco, it.nome, seg)
				},
				pronto: func(m *Model, _ any, err error) tea.Cmd {
					if err != nil {
						m.erro("Não foi possível apagar", err)
						return nil
					}
					m.status = it.nome + " apagado"
					m.recarregarExecucoes()
					return a.carregar(m)
				},
			})
		}
		// Apagar não tem volta: num destino homolog, pede o nome do banco, como desfazer.
		if c, _ := m.conexao(d.conexao); c.Tag == cadastro.TagHomolog {
			return m.conf.perguntarCritico("Apagar "+it.nome, corpo, d.banco, apagar)
		}
		m.conf.perguntar("Apagar "+it.nome, corpo, apagar)
	}
	return nil
}

func (a *abaAnteriores) desfazer(m *Model, d destinoAnt, it item) tea.Cmd {
	c, _ := m.conexao(d.conexao)
	corpo := fmt.Sprintf("%s volta a ser o banco %s em %s.\n\n", it.nome, d.banco, d.conexao)
	if d.dados.Existe {
		corpo += fmt.Sprintf("O banco %s de agora não se perde: ele vira %s<agora>, fechado para conexões. As sessões nele são derrubadas.",
			d.banco, nomes.PrefixoAnteriores(d.banco))
	}
	acao := func() tea.Cmd {
		return m.executar(&tarefa{
			rotulo: "desfazendo " + d.banco,
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				return m.o.Desfazer(ctx, d.conexao, d.banco, it.nome, seg)
			},
			pronto: func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					m.erro("Não foi possível desfazer", err)
					return nil
				}
				m.status = fmt.Sprintf("%s de volta em %s", d.banco, d.conexao)
				if g, _ := v.(string); g != "" {
					m.status += "; o que estava lá ficou como " + g
				}
				return a.carregar(m)
			},
		})
	}
	if c.Tag == cadastro.TagHomolog {
		return m.conf.perguntarCritico("Desfazer em "+d.conexao, corpo, d.banco, acao)
	}
	m.conf.perguntar("Desfazer em "+d.conexao, corpo, acao)
	return nil
}

func (a *abaAnteriores) view(m *Model) string {
	if len(m.perfis) == 0 {
		return "\n" + stDica.Render("  Sem perfis, sem destinos. Os bancos __anterior aparecem aqui depois da primeira cópia.")
	}
	if !a.carregado {
		return "\n" + stDica.Render("  lendo os destinos… (r para ler de novo)")
	}
	var b strings.Builder
	var total int64
	for _, it := range a.itens() {
		total += it.tamanho
	}
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" BANCOS DA FERRAMENTA NOS DESTINOS · %s no total", motor.Tamanho(total))) +
		stDica.Render(" · nada é apagado sozinho: decida aqui") + "\n")
	idx := 0
	cur := a.cursor
	for _, d := range a.destinos {
		c, _ := m.conexao(d.conexao)
		b.WriteString("\n " + seloTag(c.Tag) + " " + stValor.Render(d.conexao+" / "+d.banco) + stDica.Render("  perfis: "+strings.Join(d.perfis, ", ")) + "\n")
		if d.erro != "" {
			b.WriteString("   " + stPerigoV.Render("✖ ") + stDica.Render(d.erro) + "\n")
			continue
		}
		if len(d.dados.Anteriores) == 0 && d.dados.Novo == nil && d.dados.Base == nil {
			b.WriteString("   " + stDica.Render("nenhum") + "\n")
		}
		for _, x := range d.dados.Anteriores {
			b.WriteString(a.linha(idx == cur, x.Nome, x.Tamanho, stDica.Render(idade(x.Data))))
			idx++
		}
		if d.dados.Novo != nil {
			b.WriteString(a.linha(idx == cur, d.dados.Novo.Nome, d.dados.Novo.Tamanho, stAvisoV.Render("sobrou de uma cópia que não terminou: bloqueia a próxima cópia")))
			idx++
		}
		if d.dados.Base != nil {
			b.WriteString(a.linha(idx == cur, d.dados.Base.Nome, d.dados.Base.Tamanho, stDica.Render("base para o reset (tecla z nos perfis)")))
			idx++
		}
	}
	b.WriteString("\n" + stDica.Render("  u desfaz (o anterior volta, e o atual também vira anterior) · d apaga · os anteriores ficam fechados para conexões"))
	return b.String()
}

func (a *abaAnteriores) linha(sel bool, nome string, tam int64, extra string) string {
	marca := "   "
	n := stTexto.Render(nome)
	if sel {
		marca = stSelecao.Render(" ▸ ")
		n = stSelecao.Render(nome)
	}
	return marca + n + "  " + stTexto.Render(preencher(motor.Tamanho(tam), 10)) + " " + extra + "\n"
}

func espacoLivre(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
