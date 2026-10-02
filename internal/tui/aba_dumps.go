package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// abaDumps mostra a pasta de entrada (os arquivos de fora, para restaurar num dev ou homolog) e os
// dumps que as cópias guardaram. O cursor anda pelas duas listas, a da entrada primeiro.
type abaDumps struct {
	cursor    int
	entrada   []motor.ItemEntrada
	dumps     []motor.Dump
	carregado bool
}

type msgDumps struct {
	entrada []motor.ItemEntrada
	ds      []motor.Dump
}

func (a *abaDumps) carregar(m *Model) tea.Cmd {
	dir, perfis := m.o.Dir, m.perfis
	return func() tea.Msg { return msgDumps{entrada: motor.ListarEntrada(dir), ds: motor.ListarDumps(dir, perfis)} }
}

// receber troca as listas e mantém a seleção no mesmo item (um arquivo novo entra no topo).
func (a *abaDumps) receber(m *Model, msg msgDumps) {
	antes := a.caminhoAtual()
	a.entrada, a.dumps, a.carregado = msg.entrada, msg.ds, true
	for i := 0; i < a.total() && antes != ""; i++ {
		a.cursor = i
		if a.caminhoAtual() == antes {
			return
		}
	}
	a.cursor = limitar(a.cursor, 0, max(a.total()-1, 0))
}

func (a *abaDumps) caminhoAtual() string {
	switch it, d := a.atual(); {
	case it != nil:
		return it.Arquivo.Caminho
	case d != nil:
		return d.Caminho
	}
	return ""
}

// emUso diz qual execução em andamento usa o caminho (o arquivo de uma restauração, o dump de outra):
// apagá-lo derrubaria o restore no meio.
func emUso(m *Model, caminho string) (int64, bool) {
	for _, e := range m.execucoes {
		if e.Terminou() {
			continue
		}
		if e.DumpDir == caminho {
			return e.ID, true
		}
		var p motor.Plano
		if e.Tipo == cadastro.TipoArquivo && json.Unmarshal([]byte(e.Plano), &p) == nil && p.Arquivo != nil && p.Arquivo.Caminho == caminho {
			return e.ID, true
		}
	}
	return 0, false
}

func (a *abaDumps) total() int { return len(a.entrada) + len(a.dumps) }

// atual é o item do cursor: um arquivo da entrada ou um dump guardado.
func (a *abaDumps) atual() (*motor.ItemEntrada, *motor.Dump) {
	switch {
	case a.cursor < len(a.entrada):
		return &a.entrada[a.cursor], nil
	case a.cursor < a.total():
		return nil, &a.dumps[a.cursor-len(a.entrada)]
	}
	return nil, nil
}

func (a *abaDumps) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	it, d := a.atual()
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(a.total()-1, 0))
	case "R":
		return a.carregar(m)
	case "r":
		switch {
		case it != nil && it.Erro != "":
			m.informar("Não dá para restaurar", filepath.Base(it.Arquivo.Caminho)+": "+it.Erro)
		case it != nil:
			return a.formArquivo(m, it.Arquivo)
		case d != nil:
			return a.restaurarDump(m, *d)
		}
	case "d":
		if id, ok := emUso(m, a.caminhoAtual()); ok {
			m.informar("Em uso", fmt.Sprintf("A execução #%d, em andamento, usa %s: apagar agora derrubaria o restore no meio. Espere ela terminar.", id, a.caminhoAtual()))
			return nil
		}
		switch {
		case it != nil:
			arq := it.Arquivo
			corpo := fmt.Sprintf("%s\n\n%s, %s.\n\nO arquivo sai da pasta de entrada e não volta. Os bancos restaurados dele não são tocados.",
				arq.Caminho, orDefault(arq.Formato, "formato desconhecido"), motor.Tamanho(arq.Tamanho))
			m.conf.perguntar("Apagar o arquivo", corpo, func() tea.Cmd {
				if err := motor.ApagarArquivo(m.o.Dir, arq.Caminho); err != nil {
					m.erro("Não foi possível apagar", err)
					return nil
				}
				m.status = "arquivo apagado: " + arq.Caminho
				return a.carregar(m)
			})
		case d != nil:
			corpo := fmt.Sprintf("%s\n\n%s, do perfil %s (%s/%s), de %s.\n\nApagado, ele não volta.",
				d.Caminho, motor.Tamanho(d.Tamanho), d.Manifesto.Perfil, d.Manifesto.Origem.Conexao, d.Manifesto.Origem.Banco, d.Manifesto.Inicio.Format("02/01/2006 15:04"))
			caminho := d.Caminho
			m.conf.perguntar("Apagar o dump", corpo, func() tea.Cmd {
				if err := motor.ApagarDump(m.o.Dir, m.perfis, caminho); err != nil {
					m.erro("Não foi possível apagar", err)
					return nil
				}
				m.status = "dump apagado: " + caminho
				return a.carregar(m)
			})
		}
	}
	return nil
}

func (a *abaDumps) restaurarDump(m *Model, d motor.Dump) tea.Cmd {
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
}

// formArquivo pede o destino de um arquivo da entrada: a conexão (só dev e homolog) e o banco, no
// seletor dos perfis. O banco sugerido é o de onde o dump veio, ou o nome do arquivo.
func (a *abaDumps) formArquivo(m *Model, arq motor.Arquivo) tea.Cmd {
	var destinos []string
	for _, c := range m.conexoes {
		if c.Tag != cadastro.TagProd {
			destinos = append(destinos, c.Nome)
		}
	}
	if len(destinos) == 0 {
		m.informar("Falta um destino", "Um arquivo só é restaurado numa conexão dev ou homolog. Cadastre a do banco de desenvolvimento ou homologação na aba 3.")
		return nil
	}
	nome := filepath.Base(arq.Caminho)
	var marcados []string
	if s := sugerirBanco(arq); s != "" {
		marcados = []string{s}
	}
	banco := campoDeEscolha("banco", "Banco de destino", "", escolha{
		itens: func(f *formulario) []itemEscolha { return itensRestaurar(m, f.valor("destino")) },
		livre: "usar %q (é criado)",
		info:  func(f *formulario) string { return infoBancos(m, f.valor("destino")) },
		reler: m.perfisA.relerBancos(m, "destino", "banco"),
		foraDaLista: func(f *formulario, v string) string {
			return v + " não existe em " + f.valor("destino") + " (na última verificação): é criado"
		},
		marcados: marcados,
	}, func(s string) error {
		if s == "" {
			return errors.New("marque o banco (espaço marca)")
		}
		return nil
	})
	banco.ajudaDin = func(f *formulario) string {
		return "Um banco que existe é substituído, e o atual vira <banco>__anterior_<data> (a aba 5 desfaz). Um nome novo é criado. Os que já existem aparecem na cor de aviso."
	}
	ajudaJobs := "Conexões paralelas no destino, no restore e no ANALYZE."
	if !arq.Paralelo() {
		ajudaJobs = "Conexões paralelas no ANALYZE. O restore do " + arq.Formato + " vai com uma só."
	}
	jobs := novoCampo("jobs", "Jobs", "4", ajudaJobs, func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 32 {
			return errors.New("de 1 a 32")
		}
		return nil
	})
	campos := []campo{
		campoDeOpcao("destino", "Destino", "Só aparecem conexões dev e homolog: um banco prod nunca é destino.", destinos, destinos[0]),
		banco,
		jobs,
	}
	nota := fmt.Sprintf("%s · %s · %s", arq.Formato, motor.Tamanho(arq.Tamanho), arq.Origem())
	cmd := m.form.abrir("Restaurar "+nome, nota, campos, func(f *formulario) (tea.Cmd, error) {
		// Com outra tarefa rodando, o formulário fica aberto: senão ele fecharia sem planejar nada.
		if m.ocupado != "" {
			return nil, errors.New("espere terminar: " + m.ocupado)
		}
		n, _ := strconv.Atoi(f.valor("jobs"))
		pd := motor.PedidoArquivo{Caminho: arq.Caminho, Conexao: f.valor("destino"), Banco: f.valor("banco"), Jobs: max(n, 1)}
		rotulo := "checando o destino para restaurar " + nome
		if arq.SQL() {
			rotulo = "lendo " + nome + " e checando o destino"
		}
		return m.executar(&tarefa{
			rotulo: rotulo,
			// A leitura de um SQL grande leva tempo: sem os 5 minutos de uma tarefa, e o esc para.
			prazo: 12 * time.Hour, cancelavel: true,
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				return m.o.PlanejarArquivo(ctx, pd, seg)
			},
			pronto: func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					m.erro("Não foi possível planejar a restauração", err)
					return nil
				}
				m.recarregar()
				return m.abrirPlano(v.(motor.Plano))
			},
		}), nil
	})
	// O cursor começa no banco sugerido, e o detalhe em destaque é o dele.
	if c := m.form.campo("banco"); c != nil && len(marcados) > 0 {
		for i, it := range c.esc.visiveis(&m.form) {
			if it.valor == marcados[0] {
				c.esc.cursor = i
			}
		}
	}
	return cmd
}

// itensRestaurar são os bancos que já existem na conexão, em amarelo: escolhido, o banco é
// substituído, e o atual vira __anterior.
func itensRestaurar(m *Model, conexao string) []itemEscolha {
	c, _ := m.conexao(conexao)
	var its []itemEscolha
	for _, b := range c.Info.Bancos {
		if foraDoSeletor(b.Nome) || b.Nome == c.BancoAdmin {
			continue
		}
		its = append(its, itemEscolha{valor: b.Nome, extra: motor.Tamanho(b.Tamanho), aviso: true,
			detalhe: descreverBanco(b) + "\nexiste em " + conexao + ": é substituído, e o atual vira " + nomes.PrefixoAnteriores(b.Nome) + "<data>"})
	}
	return its
}

var (
	reExtensaoDump = regexp.MustCompile(`(?i)(\.sql\.gz|\.sql|\.dump|\.backup|\.bak|\.pgdump|\.custom|\.tar)$`)
	reNaoNome      = regexp.MustCompile(`[^a-z0-9_]+`)
)

// sugerirBanco é o banco de onde o dump veio, se o arquivo disser; senão, o nome do arquivo sem a
// extensão, em minúsculas e sem os caracteres que pediriam aspas.
func sugerirBanco(arq motor.Arquivo) string {
	serve := func(n string) bool {
		return n != "" && !nomes.PareceDaFerramenta(n) && n != "postgres" && n != "template0" && n != "template1"
	}
	if serve(arq.Banco) {
		return arq.Banco
	}
	n := strings.Trim(reNaoNome.ReplaceAllString(strings.ToLower(reExtensaoDump.ReplaceAllString(filepath.Base(arq.Caminho), "")), "_"), "_")
	if len(n) > 63 {
		n = n[:63]
	}
	if !serve(n) {
		return ""
	}
	return n
}

// truncarMeio corta o meio de um nome longo, que costuma ter o começo igual ao dos outros
// (backup_loja_…_2026-10-01.dump).
func truncarMeio(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 5 {
		return s
	}
	ini := (w - 1) / 2
	return string(r[:ini]) + "…" + string(r[len(r)-(w-1-ini):])
}

func (a *abaDumps) view(m *Model) string {
	if !a.carregado {
		return "\n" + stDica.Render("  lendo a pasta de entrada e os dumps…")
	}
	cartao := a.cartao(m)
	// As duas listas dividem o que sobra do cartão: a dos dumps fica com até a metade, e a da
	// entrada com o resto.
	alt := max(m.alturaCorpo()-lipgloss.Height(cartao)-6, 2)
	altEntrada := limitar(len(a.entrada), 1, max(alt-limitar(len(a.dumps), 1, max(alt/2, 1)), 1))
	altDumps := max(alt-altEntrada, 1)
	var b strings.Builder

	// A pasta de entrada.
	var tamEntrada int64
	for _, it := range a.entrada {
		tamEntrada += it.Arquivo.Tamanho
	}
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" PASTA DE ENTRADA (%d, %s)", len(a.entrada), motor.Tamanho(tamEntrada))) +
		stDica.Render(" · "+m.o.Dir.Entrada()+" · arquivos para restaurar num dev ou homolog") + "\n")
	if len(a.entrada) == 0 {
		b.WriteString(stDica.Render("   vazia: copie para lá um dump do pg_dump (custom, tar ou diretório) ou um .sql / .sql.gz, e tecle R") + "\n")
	}
	ini, fim := janelaDeLinhas(len(a.entrada), min(a.cursor, max(len(a.entrada)-1, 0)), altEntrada)
	for i := ini; i < fim; i++ {
		it := a.entrada[i]
		arq := it.Arquivo
		marca, nome := "  ", preencher(truncarMeio(filepath.Base(arq.Caminho), 30), 30)
		if i == a.cursor {
			marca, nome = stSelecao.Render("▸ "), stSelecao.Render(nome)
		} else {
			nome = stTexto.Render(nome)
		}
		desc := stDica.Render(arq.Origem())
		if it.Erro != "" {
			desc = stAvisoV.Render(it.Erro)
		}
		b.WriteString(fmt.Sprintf(" %s%s  %s  %s  %s  %s\n", marca, stTexto.Render(arq.Modificado.Format("02/01 15:04")), nome,
			stTexto.Render(preencher(orDefault(arq.Formato, "?"), 9)), stTexto.Render(preencher(motor.Tamanho(arq.Tamanho), 10)), desc))
	}

	// Os dumps guardados.
	var total int64
	for _, d := range a.dumps {
		total += d.Tamanho
	}
	livre := ""
	if l, err := espacoLivre(m.o.Dir.Dumps()); err == nil {
		livre = " · " + motor.Tamanho(l) + " livres no disco"
	}
	b.WriteString("\n" + stCabecalho.Render(fmt.Sprintf(" DUMPS GUARDADOS (%d, %s)", len(a.dumps), motor.Tamanho(total))) + stDica.Render(livre+" · nada é apagado sozinho") + "\n")
	if len(a.dumps) == 0 {
		b.WriteString(stDica.Render("   nenhum: cada cópia guarda o seu em "+m.o.Dir.Dumps()+"/<perfil>/<data>") + "\n")
	}
	ini, fim = janelaDeLinhas(len(a.dumps), max(a.cursor-len(a.entrada), 0), altDumps)
	for i := ini; i < fim; i++ {
		d := a.dumps[i]
		mf := d.Manifesto
		marca := "  "
		data := stTexto.Render(mf.Inicio.Format("02/01 15:04"))
		if i+len(a.entrada) == a.cursor {
			marca, data = stSelecao.Render("▸ "), stSelecao.Render(mf.Inicio.Format("02/01 15:04"))
		}
		estado := stOk.Render("completo  ")
		if mf.Estado != motor.DumpCompleto {
			estado = stAvisoV.Render("incompleto")
		}
		b.WriteString(fmt.Sprintf(" %s%s  %s  %s  %s  %s\n", marca, data, estado, stTexto.Render(preencher(motor.Tamanho(d.Tamanho), 10)),
			stTexto.Render(preencher(truncar(mf.Perfil, 22), 22)), stDica.Render(mf.Origem.Conexao+"/"+mf.Origem.Banco)))
	}
	if cartao != "" {
		b.WriteString("\n" + cartao)
	}
	return b.String()
}

// cartao descreve o item do cursor.
func (a *abaDumps) cartao(m *Model) string {
	larg := limitar(m.largura-4, 40, 140)
	var c strings.Builder
	switch it, d := a.atual(); {
	case it != nil:
		arq := it.Arquivo
		c.WriteString(stMarca.Render(arq.Caminho) + "\n")
		c.WriteString(stRotulo.Render("formato   ") + stTexto.Render(orDefault(arq.Formato, "?")+" · "+motor.Tamanho(arq.Tamanho)) + "\n")
		c.WriteString(stRotulo.Render("origem    ") + stTexto.Render(arq.Origem()))
		if it.Erro != "" {
			c.WriteString("\n" + stRotulo.Render("restaurar ") + stAvisoV.Render(it.Erro))
		} else {
			c.WriteString("\n" + stRotulo.Render("restaurar ") + stDica.Render("r: escolha a conexão dev ou homolog e o banco; o banco que estiver lá vira __anterior"))
		}
	case d != nil:
		mf := d.Manifesto
		c.WriteString(stMarca.Render(d.Caminho) + "\n")
		c.WriteString(stRotulo.Render("origem    ") + stTexto.Render(fmt.Sprintf("%s/%s · PostgreSQL %s", mf.Origem.Conexao, mf.Origem.Banco, versoes.Texto(mf.Origem.VersaoNum))) + "\n")
		c.WriteString(stRotulo.Render("cliente   ") + stTexto.Render(fmt.Sprintf("postgres:%d (pg_dump %s)", mf.Imagem, mf.Cliente)) + "\n")
		c.WriteString(stRotulo.Render("execução  ") + stTexto.Render(fmt.Sprintf("#%d, %s", mf.Execucao, mf.Inicio.Format("02/01/2006 15:04:05"))))
		if len(mf.SemDados) > 0 {
			c.WriteString("\n" + stRotulo.Render("sem dados ") + stTexto.Render(strings.Join(mf.SemDados, ", ")))
		}
	default:
		return ""
	}
	return stCartao.Width(larg).Render(c.String())
}

func arquivoExiste(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
