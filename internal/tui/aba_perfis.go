package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

type abaPerfis struct {
	cursor   int
	marcados map[string]bool // os perfis de um grupo
}

func (a *abaPerfis) atual(m *Model) (cadastro.Perfil, bool) {
	if a.cursor < 0 || a.cursor >= len(m.perfis) {
		return cadastro.Perfil{}, false
	}
	return m.perfis[a.cursor], true
}

func (a *abaPerfis) dicas(m *Model) string {
	if len(m.perfis) == 0 {
		return juntarDicas(dica("a", "novo perfil"))
	}
	if len(a.marcados) > 0 {
		return juntarDicas(dica("enter", fmt.Sprintf("copiar os %d marcados em fila", len(a.marcados))), dica("espaço", "marcar"), dica("esc", "desmarcar"))
	}
	return juntarDicas(dica("enter", "copiar"), dica("espaço", "marcar (grupo)"), dica("z", "resetar da base"), dica("a", "novo"), dica("e", "editar"), dica("d", "remover"))
}

func (a *abaPerfis) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(m.perfis)-1, 0))
	case "home", "g":
		a.cursor = 0
	case "end", "G":
		a.cursor = max(len(m.perfis)-1, 0)
	case " ":
		if p, ok := a.atual(m); ok {
			if a.marcados == nil {
				a.marcados = map[string]bool{}
			}
			a.marcados[p.Nome] = !a.marcados[p.Nome]
			if !a.marcados[p.Nome] {
				delete(a.marcados, p.Nome)
			}
			a.cursor = min(a.cursor+1, max(len(m.perfis)-1, 0))
		}
	case "esc":
		a.marcados = nil
	case "enter":
		if len(a.marcados) > 0 {
			var ns []string
			for n := range a.marcados {
				if _, ok := perfilPorNome(m, n); ok {
					ns = append(ns, n)
				}
			}
			return m.copiarGrupo(ns)
		}
		if p, ok := a.atual(m); ok {
			return m.copiar(p.Nome)
		}
	case "z":
		if p, ok := a.atual(m); ok {
			nome := p.Nome
			return m.executar(&tarefa{
				rotulo: "checando o destino e a base de " + nome,
				rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
					return m.o.PlanejarReset(ctx, nome, seg)
				},
				pronto: func(m *Model, v any, err error) tea.Cmd {
					if err != nil {
						m.erro("Não foi possível planejar o reset", err)
						return nil
					}
					return m.abrirPlano(v.(motor.Plano))
				},
			})
		}
	case "a":
		return a.formulario(m, cadastro.Perfil{JobsDump: 2, JobsRestore: 4}, "")
	case "e":
		if p, ok := a.atual(m); ok {
			return a.formulario(m, p, p.Nome)
		}
	case "d":
		if p, ok := a.atual(m); ok {
			m.conf.perguntar("Remover o perfil "+p.Nome, "O perfil sai do cadastro. O histórico das execuções, os dumps e os bancos no destino ficam.", func() tea.Cmd {
				if err := m.o.Cadastro.RemoverPerfil(context.Background(), p.Nome); err != nil {
					m.erro("Não foi possível remover", err)
					return nil
				}
				m.recarregar()
				m.status = "perfil " + p.Nome + " removido"
				return nil
			})
		}
	}
	return nil
}

// copiar é o "um ou dois cliques": checa tudo e abre o plano para a confirmação.
func (m *Model) copiar(nome string) tea.Cmd {
	return m.executar(&tarefa{
		rotulo: "checando a origem e o destino de " + nome,
		rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
			return m.o.Planejar(ctx, nome, seg)
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("Não foi possível checar "+nome, err)
				return nil
			}
			m.recarregar() // o plano atualiza a versão gravada das conexões
			return m.abrirPlano(v.(motor.Plano))
		},
	})
}

func (a *abaPerfis) formulario(m *Model, p cadastro.Perfil, antigo string) tea.Cmd {
	if len(m.conexoes) == 0 {
		m.informar("Primeiro as conexões", "Um perfil liga uma conexão de origem a uma de destino. Cadastre as conexões na aba 3 (tecla a).")
		return nil
	}
	var todas, destinos []string
	for _, c := range m.conexoes {
		todas = append(todas, c.Nome)
		if c.Tag != cadastro.TagProd {
			destinos = append(destinos, c.Nome)
		}
	}
	if len(destinos) == 0 {
		m.informar("Falta um destino", "Todas as conexões são prod, e um banco prod nunca é destino. Cadastre a conexão do banco de desenvolvimento ou homologação na aba 3.")
		return nil
	}
	if p.Origem == "" {
		for _, c := range m.conexoes {
			if c.Tag == cadastro.TagProd {
				p.Origem = c.Nome
				break
			}
		}
	}
	obrig := func(s string) error {
		if s == "" {
			return errors.New("obrigatório")
		}
		return nil
	}
	jobs := func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 32 {
			return errors.New("de 1 a 32")
		}
		return nil
	}
	arquivo := func(s string) error {
		if s == "" {
			return nil
		}
		if !strings.HasPrefix(s, "/") {
			return errors.New("use o caminho absoluto")
		}
		if _, err := os.Stat(s); err != nil {
			return errors.New("arquivo não encontrado")
		}
		return motor.ConferirScript(s)
	}
	diretorio := func(s string) error {
		if s != "" && !strings.HasPrefix(s, "/") {
			return errors.New("use o caminho absoluto")
		}
		return nil
	}
	campos := []campo{
		novoCampo("nome", "Nome", p.Nome, "Como o perfil aparece na lista: por exemplo, loja-homolog.", obrig),
		campoDeOpcao("origem", "Origem", "A conexão de onde o banco vem (normalmente a produção).", todas, p.Origem),
		novoCampo("origem_banco", "Banco de origem", p.OrigemBanco, "", obrig),
		campoDeOpcao("destino", "Destino", "Só aparecem conexões dev e homolog: um banco prod nunca é destino.", destinos, p.Destino),
		novoCampo("destino_banco", "Banco de destino", p.DestinoBanco, "", nil),
		novoCampo("jobs_dump", "Jobs no dump", strconv.Itoa(p.JobsDump), "Conexões paralelas na origem. Cada uma é uma consulta longa na produção: comece com 2.", jobs),
		novoCampo("jobs_restore", "Jobs no restore", strconv.Itoa(p.JobsRestore), "Conexões paralelas no destino, no restore e no ANALYZE.", jobs),
		novoCampo("sem_dados", "Tabelas sem dados", strings.Join(p.SemDados, ", "), "Padrões do pg_dump separados por vírgula (public.log_*, auditoria.*): essas tabelas vêm só com a estrutura.", nil),
		novoCampo("script", "Script pós-restore", p.Script, "Opcional: um .sql rodado no banco novo antes da troca (desligar jobs, trocar URLs). Roda com poderes de superusuário, e o que ele criar fica com o dono do destino.", arquivo),
		novoCampo("dir", "Diretório dos dumps", p.DirDumps, "Vazio é "+m.o.Dir.Dumps()+"/<perfil>. Os dumps nunca são apagados sozinhos.", diretorio),
		campoDeOpcao("compressao", "Compressão", "zstd é a mais rápida para o tamanho que dá; lz4 gasta menos CPU; gzip é a do pg_dump antigo.", cadastro.Compressoes, orDefault(p.Compressao, "zstd")),
		novoCampo("schemas", "Só os schemas", strings.Join(p.Schemas, ", "), "Vazio copia todos. Padrões do pg_dump separados por vírgula (vendas, app_*). As extensões vêm junto.", nil),
		novoCampo("schemas_fora", "Sem os schemas", strings.Join(p.SchemasFora, ", "), "Schemas que ficam de fora (padrões do pg_dump, por vírgula).", nil),
		novoCampo("tabelas", "Só as tabelas", strings.Join(p.Tabelas, ", "), "Vazio copia todas. Com esta lista, a conferência não compara os objetos (só as linhas, se ligadas).", nil),
		novoCampo("tabelas_fora", "Sem as tabelas", strings.Join(p.TabelasFora, ", "), "Tabelas que ficam de fora inteiras (estrutura e dados). Para levar só a estrutura, use \"Tabelas sem dados\".", nil),
		campoDeOpcao("conferir_linhas", "Conferir linhas", "sim: conta as linhas de cada tabela na origem, no mesmo snapshot do dump, e compara com o destino. É uma leitura completa de cada tabela da origem.", []string{"não", "sim"}, simNao(p.ConferirLinhas)),
		campoDeOpcao("retomavel", "Link instável", "sim: os dados vêm em blocos pela chave e, se o túnel cair, o dump continua de onde parou (até de uma cópia anterior que morreu). Sem snapshot único; não aceita a lista \"Só as tabelas\".", []string{"não", "sim"}, simNao(p.Retomavel)),
		campoDeOpcao("guardar_base", "Guardar base", "sim: cada cópia deixa <banco>__base no destino (fechado), para resetar o destino depois sem ir à origem (tecla z). Ocupa o tamanho do banco a mais.", []string{"não", "sim"}, simNao(p.GuardarBase)),
	}
	campos[2].ajudaDin = func(f *formulario) string { return bancosDe(m, f.valor("origem"), "O banco copiado.") }
	campos[2].sugestoes = func(f *formulario) []string { return nomesDeBancos(m, f.valor("origem")) }
	campos[4].sugestoes = func(f *formulario) []string { return nomesDeBancos(m, f.valor("destino")) }
	campos[4].ajudaDin = func(f *formulario) string {
		b := orDefault(f.valor("destino_banco"), f.valor("origem_banco"))
		return bancosDe(m, f.valor("destino"), "Vazio é o mesmo nome da origem. O banco é substituído a cada cópia (o antigo vira "+nomes.PrefixoAnteriores(orDefault(b, "banco"))+"…); se não existir, é criado.")
	}
	titulo := "Novo perfil"
	if antigo != "" {
		titulo = "Editar o perfil " + antigo
	}
	return m.form.abrir(titulo, "", campos, func(f *formulario) (tea.Cmd, error) {
		jd, _ := strconv.Atoi(f.valor("jobs_dump"))
		jr, _ := strconv.Atoi(f.valor("jobs_restore"))
		novo := cadastro.Perfil{
			Nome: f.valor("nome"), Origem: f.valor("origem"), OrigemBanco: f.valor("origem_banco"),
			Destino: f.valor("destino"), DestinoBanco: orDefault(f.valor("destino_banco"), f.valor("origem_banco")), JobsDump: jd, JobsRestore: jr,
			SemDados: strings.Split(f.valor("sem_dados"), ","), Script: f.valor("script"), DirDumps: f.valor("dir"),
			Compressao: f.valor("compressao"), Schemas: strings.Split(f.valor("schemas"), ","), SchemasFora: strings.Split(f.valor("schemas_fora"), ","),
			Tabelas: strings.Split(f.valor("tabelas"), ","), TabelasFora: strings.Split(f.valor("tabelas_fora"), ","),
			ConferirLinhas: f.valor("conferir_linhas") == "sim", GuardarBase: f.valor("guardar_base") == "sim",
			Retomavel: f.valor("retomavel") == "sim",
		}
		if err := m.o.Cadastro.SalvarPerfil(context.Background(), antigo, novo); err != nil {
			return nil, err
		}
		m.recarregar()
		for i, x := range m.perfis {
			if x.Nome == novo.Nome {
				a.cursor = i
			}
		}
		m.status = "perfil " + novo.Nome + " salvo: enter para copiar"
		return nil, nil
	})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func perfilPorNome(m *Model, n string) (cadastro.Perfil, bool) {
	for _, p := range m.perfis {
		if p.Nome == n {
			return p, true
		}
	}
	return cadastro.Perfil{}, false
}

func simNao(b bool) string {
	if b {
		return "sim"
	}
	return "não"
}

// nomesDeBancos são os bancos que a última verificação viu na conexão, sem os da ferramenta.
func nomesDeBancos(m *Model, conexao string) []string {
	c, ok := m.conexao(conexao)
	if !ok {
		return nil
	}
	var ns []string
	for _, b := range c.Info.Bancos {
		if !strings.Contains(b.Nome, nomes.SufixoNovo) && !strings.Contains(b.Nome, nomes.MarcaAnterior) && b.Nome != "postgres" {
			ns = append(ns, b.Nome)
		}
	}
	return ns
}

// bancosDe lista os bancos que a última verificação viu na conexão, para a ajuda do campo.
func bancosDe(m *Model, conexao, prefixo string) string {
	c, ok := m.conexao(conexao)
	if !ok || len(c.Info.Bancos) == 0 {
		return prefixo + " (a conexão ainda não foi verificada: teste-a na aba 3 para ver os bancos.)"
	}
	var ns []string
	for _, b := range c.Info.Bancos {
		if !strings.Contains(b.Nome, nomes.SufixoNovo) && !strings.Contains(b.Nome, nomes.MarcaAnterior) {
			ns = append(ns, b.Nome)
		}
	}
	return prefixo + " Bancos em " + conexao + ": " + strings.Join(ns, ", ")
}

// --- desenho ------------------------------------------------------------------------------------

func (a *abaPerfis) view(m *Model) string {
	w := m.largura
	if len(m.perfis) == 0 {
		return a.vazio(m)
	}
	var b strings.Builder
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" PERFIS (%d)", len(m.perfis))) + "\n")
	larNome := 6
	for _, p := range m.perfis {
		larNome = max(larNome, min(lipgloss.Width(p.Nome), 28))
	}
	altLista := max(m.alturaCorpo()-12, 3)
	ini, fim := janelaDeLinhas(len(m.perfis), a.cursor, altLista)
	for i := ini; i < fim; i++ {
		p := m.perfis[i]
		marca := "  "
		nome := stTexto.Render(preencher(truncar(p.Nome, 28), larNome))
		if i == a.cursor {
			marca = stSelecao.Render("▸ ")
			nome = stSelecao.Render(preencher(truncar(p.Nome, 28), larNome))
		}
		if a.marcados[p.Nome] {
			marca = stPerigoV.Render("✚ ")
		}
		rota := fmt.Sprintf("%s/%s → %s/%s", p.Origem, p.OrigemBanco, p.Destino, p.DestinoBanco)
		ult := stDica.Render("nunca copiado")
		if e, ok := m.ultimas[p.Nome]; ok {
			ult = resumoExecucao(e)
		}
		b.WriteString(" " + marca + nome + "  " + stRotulo.Render(preencher(truncar(rota, max(w/2-larNome, 20)), max(w/2-larNome, 20))) + "  " + ult + "\n")
	}
	if p, ok := a.atual(m); ok {
		b.WriteString("\n" + a.cartao(m, p, w))
	}
	return b.String()
}

func resumoExecucao(e cadastro.Execucao) string {
	s := seloEstado(e.Estado)
	if e.Estado == cadastro.EstadoFila {
		return s
	}
	if !e.Terminou() {
		s += stDica.Render(fmt.Sprintf(" · %s", e.Etapa))
		if e.Total > 0 {
			s += stDica.Render(fmt.Sprintf(" %d%%", e.Feito*100/e.Total))
		}
		return s
	}
	s += stDica.Render(" · " + idade(e.Inicio))
	if !e.Fim.IsZero() {
		s += stDica.Render(" · " + duracao(e.Fim.Sub(e.Inicio)))
	}
	if e.TamanhoDump > 0 {
		s += stDica.Render(" · " + motor.Tamanho(e.TamanhoDump))
	}
	return s
}

func (a *abaPerfis) cartao(m *Model, p cadastro.Perfil, w int) string {
	larg := limitar(w-4, 40, 140)
	linha := func(rot, val string) string { return quebrar(stRotulo.Render(preencher(rot, 10)), val, larg-2) + "\n" }
	lado := func(nome, banco string, destino bool) string {
		c, ok := m.conexao(nome)
		if !ok {
			return stPerigoV.Render(nome + " (conexão removida)")
		}
		s := seloTag(c.Tag) + " " + stValor.Render(nome) + stDica.Render(" · "+c.Onde())
		if c.Info.VersaoNum > 0 {
			s += stDica.Render(" · PG " + versoes.Texto(c.Info.VersaoNum))
		}
		s += " " + estadoConexao(m, c)
		s += "\n" + stRotulo.Render("banco ") + stValor.Render(banco)
		if destino {
			s += stDica.Render(" — substituído a cada cópia; o antigo vira " + nomes.PrefixoAnteriores(banco) + "<data>")
		}
		return s
	}
	var b strings.Builder
	b.WriteString(stMarca.Render(p.Nome) + "\n")
	b.WriteString(linha("origem", lado(p.Origem, p.OrigemBanco, false)))
	b.WriteString(linha("destino", lado(p.Destino, p.DestinoBanco, true)))
	op := fmt.Sprintf("jobs %d no dump e %d no restore · %s", p.JobsDump, p.JobsRestore, orDefault(p.Compressao, "zstd"))
	if p.Filtrado() {
		op += " · só parte do banco"
	}
	if p.ConferirLinhas {
		op += " · confere linhas"
	}
	if p.GuardarBase {
		op += " · guarda base (z reseta)"
	}
	if p.Retomavel {
		op += " · link instável (retomável)"
	}
	if len(p.SemDados) > 0 {
		op += " · sem dados: " + strings.Join(p.SemDados, ", ")
	}
	if p.Script != "" {
		op += " · script: " + p.Script
	}
	b.WriteString(linha("opções", stTexto.Render(op)))
	b.WriteString(linha("dumps", stTexto.Render(motor.DirDumps(m.o.Dir, p))))
	if e, ok := m.ultimas[p.Nome]; ok {
		s := fmt.Sprintf("#%d ", e.ID) + resumoExecucao(e)
		if e.Mensagem != "" {
			s += "\n" + stDica.Render(e.Mensagem)
		}
		b.WriteString(linha("última", s))
	}
	b.WriteString("\n" + stDica.Render("enter copia: a ferramenta checa tudo e mostra o plano antes de mexer em qualquer coisa."))
	return stCartao.Width(larg).Render(strings.TrimRight(b.String(), "\n"))
}

func (a *abaPerfis) vazio(m *Model) string {
	var passos []string
	ok := func(feito bool, texto string) string {
		if feito {
			return stOk.Render("  ✔ ") + stDica.Render(texto)
		}
		return stAvisoV.Render("  ○ ") + stTexto.Render(texto)
	}
	temImagem := len(m.imagens) > 0
	temProd, temDestino := false, false
	for _, c := range m.conexoes {
		if c.Tag == cadastro.TagProd {
			temProd = true
		} else {
			temDestino = true
		}
	}
	passos = append(passos,
		ok(temImagem, "baixe as imagens do Postgres (aba 6, tecla b)"),
		ok(temProd, "cadastre a conexão da produção (aba 3, tecla a); por SSH, gere a chave na aba 6 antes"),
		ok(temDestino, "cadastre a conexão do banco de desenvolvimento ou homologação (aba 3)"),
		ok(false, "crie o perfil: origem, destino e bancos (aqui, tecla a)"),
	)
	return "\n" + stMarca.Render("  Nenhum perfil ainda.") + "\n\n" + stTexto.Render("  Para a primeira cópia:") + "\n\n" + strings.Join(passos, "\n") +
		"\n\n" + stDica.Render("  Depois, copiar é escolher o perfil, enter e confirmar.")
}
