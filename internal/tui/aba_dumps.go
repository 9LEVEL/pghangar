package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/9LEVEL/copia-banco/internal/conexao"
	"github.com/9LEVEL/copia-banco/internal/motor"
	"github.com/9LEVEL/copia-banco/internal/versoes"
)

type abaDumps struct {
	cursor    int
	dumps     []motor.Dump
	carregado bool
}

type msgDumps struct{ ds []motor.Dump }

func (a *abaDumps) carregar(m *Model) tea.Cmd {
	dir, perfis := m.o.Dir, m.perfis
	return func() tea.Msg { return msgDumps{ds: motor.ListarDumps(dir, perfis)} }
}

func (a *abaDumps) receber(m *Model, msg msgDumps) {
	a.dumps, a.carregado = msg.ds, true
	a.cursor = limitar(a.cursor, 0, max(len(a.dumps)-1, 0))
}

func (a *abaDumps) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(a.dumps)-1, 0))
	case "R":
		return a.carregar(m)
	case "r":
		if a.cursor >= len(a.dumps) {
			return nil
		}
		d := a.dumps[a.cursor]
		if d.Manifesto.Estado != motor.DumpCompleto {
			m.informar("Dump incompleto", "Só um dump completo pode ser restaurado.")
			return nil
		}
		return m.executar(&tarefa{
			rotulo: "checando o destino para restaurar " + filepath.Base(d.Caminho),
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				return m.o.PlanejarRestauracao(ctx, d.Caminho, seg)
			},
			pronto: func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					m.erro("Não foi possível planejar a restauração", err)
					return nil
				}
				m.recarregar()
				return m.abrirPlano(v.(motor.Plano))
			},
		})
	case "d":
		if a.cursor >= len(a.dumps) {
			return nil
		}
		d := a.dumps[a.cursor]
		corpo := fmt.Sprintf("%s\n\n%s, do perfil %s (%s/%s), de %s.\n\nApagado, ele não volta.",
			d.Caminho, motor.Tamanho(d.Tamanho), d.Manifesto.Perfil, d.Manifesto.Origem.Conexao, d.Manifesto.Origem.Banco, d.Manifesto.Inicio.Format("02/01/2006 15:04"))
		m.conf.perguntar("Apagar o dump", corpo, func() tea.Cmd {
			if err := motor.ApagarDump(m.o.Dir, m.perfis, d.Caminho); err != nil {
				m.erro("Não foi possível apagar", err)
				return nil
			}
			m.status = "dump apagado: " + d.Caminho
			return a.carregar(m)
		})
	}
	return nil
}

func (a *abaDumps) view(m *Model) string {
	if !a.carregado {
		return "\n" + stDica.Render("  lendo os dumps…")
	}
	if len(a.dumps) == 0 {
		return "\n" + stDica.Render("  Nenhum dump guardado. Cada cópia guarda o seu em "+m.o.Dir.Dumps()+"/<perfil>/<data>.")
	}
	var total int64
	for _, d := range a.dumps {
		total += d.Tamanho
	}
	var b strings.Builder
	livre := ""
	if l, err := espacoLivre(m.o.Dir.Dumps()); err == nil {
		livre = " · " + motor.Tamanho(l) + " livres no disco"
	}
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" DUMPS (%d, %s)", len(a.dumps), motor.Tamanho(total))) + stDica.Render(livre+" · nada é apagado sozinho") + "\n")
	altLista := max(m.alturaCorpo()-9, 3)
	ini, fim := janelaDeLinhas(len(a.dumps), a.cursor, altLista)
	for i := ini; i < fim; i++ {
		d := a.dumps[i]
		mf := d.Manifesto
		marca := "  "
		data := stTexto.Render(mf.Inicio.Format("02/01 15:04"))
		if i == a.cursor {
			marca, data = stSelecao.Render("▸ "), stSelecao.Render(mf.Inicio.Format("02/01 15:04"))
		}
		estado := stOk.Render("completo  ")
		if mf.Estado != motor.DumpCompleto {
			estado = stAvisoV.Render("incompleto")
		}
		b.WriteString(fmt.Sprintf(" %s%s  %s  %s  %s  %s\n", marca, data, estado, stTexto.Render(preencher(motor.Tamanho(d.Tamanho), 10)),
			stTexto.Render(preencher(truncar(mf.Perfil, 22), 22)), stDica.Render(mf.Origem.Conexao+"/"+mf.Origem.Banco)))
	}
	if a.cursor < len(a.dumps) {
		d := a.dumps[a.cursor]
		mf := d.Manifesto
		larg := limitar(m.largura-4, 40, 140)
		var c strings.Builder
		c.WriteString(stMarca.Render(d.Caminho) + "\n")
		c.WriteString(stRotulo.Render("origem    ") + stTexto.Render(fmt.Sprintf("%s/%s · PostgreSQL %s", mf.Origem.Conexao, mf.Origem.Banco, versoes.Texto(mf.Origem.VersaoNum))) + "\n")
		c.WriteString(stRotulo.Render("cliente   ") + stTexto.Render(fmt.Sprintf("postgres:%d (pg_dump %s)", mf.Imagem, mf.Cliente)) + "\n")
		c.WriteString(stRotulo.Render("execução  ") + stTexto.Render(fmt.Sprintf("#%d, %s", mf.Execucao, mf.Inicio.Format("02/01/2006 15:04:05"))))
		if len(mf.SemDados) > 0 {
			c.WriteString("\n" + stRotulo.Render("sem dados ") + stTexto.Render(strings.Join(mf.SemDados, ", ")))
		}
		b.WriteString("\n" + stCartao.Width(larg).Render(c.String()))
	}
	return b.String()
}

func arquivoExiste(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
