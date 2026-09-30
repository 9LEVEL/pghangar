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
	// Um perfil é um banco. No perfil novo, marcar vários bancos cria um perfil por banco, com as
	// mesmas opções (docs/DECISOES.md, 2026-09-30).
	varios := func(f *formulario) bool { return antigo == "" && len(f.valores("origem_banco")) > 1 }
	origemMarcados, destinoMarcados := []string(nil), []string{""}
	if p.OrigemBanco != "" {
		origemMarcados = []string{p.OrigemBanco}
	}
	if p.DestinoBanco != "" && p.DestinoBanco != p.OrigemBanco {
		destinoMarcados = []string{p.DestinoBanco}
	}
	nome := novoCampo("nome", "Nome", p.Nome, "Como o perfil aparece na lista. Vem preenchido com <banco>-<destino> até você editá-lo.", obrig)
	nome.visivel = func(f *formulario) bool { return !varios(f) }
	if antigo == "" {
		nome.auto = func(f *formulario) string {
			if bs := f.valores("origem_banco"); len(bs) == 1 {
				return bs[0] + "-" + f.valor("destino")
			}
			return ""
		}
	}
	origemBanco := campoDeEscolha("origem_banco", "Banco de origem", "", escolha{
		itens: func(f *formulario) []itemEscolha { return itensOrigem(m, f.valor("origem")) },
		multi: antigo == "",
		livre: "usar %q (não visto na última verificação)",
		info:  func(f *formulario) string { return infoBancos(m, f.valor("origem")) },
		reler: a.relerBancos(m, "origem", "origem_banco"),
		resumo: func(n int) string {
			if n > 1 {
				return fmt.Sprintf("→ %d perfis, um por banco", n)
			}
			return ""
		},
		marcados: origemMarcados,
	}, func(s string) error {
		if s == "" {
			return errors.New("marque o banco (espaço marca)")
		}
		return nil
	})
	origemBanco.ajudaDin = func(f *formulario) string {
		if bs := f.valores("origem_banco"); len(bs) > 1 {
			var ns []string
			for _, b := range bs {
				ns = append(ns, b+"-"+f.valor("destino"))
			}
			return fmt.Sprintf("%d bancos marcados: vira um perfil por banco, com as mesmas opções (%s). O banco de destino tem o mesmo nome do de origem.", len(bs), strings.Join(ns, ", "))
		}
		return "O banco copiado."
	}
	destinoBanco := campoDeEscolha("destino_banco", "Banco de destino", "", escolha{
		itens: func(f *formulario) []itemEscolha {
			return itensDestino(m, f.valor("destino"), f.valores("origem_banco"))
		},
		livre:    "usar %q (é criado na primeira cópia)",
		info:     func(f *formulario) string { return infoBancos(m, f.valor("destino")) },
		reler:    a.relerBancos(m, "destino", "destino_banco"),
		marcados: destinoMarcados,
	}, nil)
	destinoBanco.visivel = func(f *formulario) bool { return !varios(f) }
	destinoBanco.ajudaDin = func(f *formulario) string {
		return "O banco é substituído a cada cópia, e o antigo vira <banco>__anterior_<data>. Se não existir, é criado. Os que já existem aparecem na cor de aviso."
	}
	campos := []campo{
		nome,
		campoDeOpcao("origem", "Origem", "A conexão de onde o banco vem (normalmente a produção).", todas, p.Origem),
		origemBanco,
		campoDeOpcao("destino", "Destino", "Só aparecem conexões dev e homolog: um banco prod nunca é destino.", destinos, p.Destino),
		destinoBanco,
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
	titulo := "Novo perfil"
	if antigo != "" {
		titulo = "Editar o perfil " + antigo
	}
	return m.form.abrir(titulo, "", campos, func(f *formulario) (tea.Cmd, error) {
		jd, _ := strconv.Atoi(f.valor("jobs_dump"))
		jr, _ := strconv.Atoi(f.valor("jobs_restore"))
		base := cadastro.Perfil{
			Origem: f.valor("origem"), Destino: f.valor("destino"), JobsDump: jd, JobsRestore: jr,
			SemDados: strings.Split(f.valor("sem_dados"), ","), Script: f.valor("script"), DirDumps: f.valor("dir"),
			Compressao: f.valor("compressao"), Schemas: strings.Split(f.valor("schemas"), ","), SchemasFora: strings.Split(f.valor("schemas_fora"), ","),
			Tabelas: strings.Split(f.valor("tabelas"), ","), TabelasFora: strings.Split(f.valor("tabelas_fora"), ","),
			ConferirLinhas: f.valor("conferir_linhas") == "sim", GuardarBase: f.valor("guardar_base") == "sim",
			Retomavel: f.valor("retomavel") == "sim",
		}
		bancos := f.valores("origem_banco")
		if varios(f) {
			return nil, a.salvarVarios(m, base, bancos)
		}
		novo := base
		novo.Nome, novo.OrigemBanco = f.valor("nome"), bancos[0]
		novo.DestinoBanco = orDefault(f.valor("destino_banco"), bancos[0])
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

// salvarVarios cria um perfil por banco, com as mesmas opções: <banco>-<destino>, e o banco de
// destino com o mesmo nome. Um nome que já existe barra todos, antes de gravar qualquer um. Os
// perfis salvos ficam marcados, e o enter os copia em fila.
func (a *abaPerfis) salvarVarios(m *Model, base cadastro.Perfil, bancos []string) error {
	var ps []cadastro.Perfil
	var repetidos []string
	for _, b := range bancos {
		p := base
		p.Nome, p.OrigemBanco, p.DestinoBanco = b+"-"+base.Destino, b, b
		if _, ok := perfilPorNome(m, p.Nome); ok {
			repetidos = append(repetidos, p.Nome)
		}
		ps = append(ps, p)
	}
	if len(repetidos) > 0 {
		return fmt.Errorf("já existem os perfis %s: desmarque esses bancos, ou crie-os um de cada vez, com outro nome", strings.Join(repetidos, ", "))
	}
	var salvos []string
	for _, p := range ps {
		if err := m.o.Cadastro.SalvarPerfil(context.Background(), "", p); err != nil {
			m.recarregar()
			if len(salvos) > 0 {
				return fmt.Errorf("%s: %w (os anteriores já foram salvos: %s)", p.Nome, err, strings.Join(salvos, ", "))
			}
			return fmt.Errorf("%s: %w", p.Nome, err)
		}
		salvos = append(salvos, p.Nome)
	}
	m.recarregar()
	a.marcados = map[string]bool{}
	for _, n := range salvos {
		a.marcados[n] = true
	}
	for i, x := range m.perfis {
		if x.Nome == salvos[0] {
			a.cursor = i
		}
	}
	m.status = fmt.Sprintf("%d perfis salvos e marcados: enter copia em fila", len(salvos))
	return nil
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

// --- os bancos no seletor ---------------------------------------------------------------------

// foraDoSeletor são os bancos que não aparecem: o postgres e os da própria ferramenta (__novo,
// __anterior, __base). Os templates a verificação já não lê.
func foraDoSeletor(nome string) bool {
	return nome == "postgres" || strings.Contains(nome, nomes.SufixoNovo) || strings.Contains(nome, nomes.MarcaAnterior) ||
		strings.HasSuffix(nome, nomes.SufixoBase)
}

func descreverBanco(b cadastro.Banco) string {
	s := fmt.Sprintf("%s · %s · dono %s · %s", b.Nome, motor.Tamanho(b.Tamanho), b.Dono, b.Codificacao)
	if b.Collate != "" {
		s += " · " + b.Collate
	}
	return s
}

// itensOrigem são os bancos que a última verificação viu na conexão de origem.
func itensOrigem(m *Model, conexao string) []itemEscolha {
	c, ok := m.conexao(conexao)
	if !ok {
		return nil
	}
	var its []itemEscolha
	for _, b := range c.Info.Bancos {
		if foraDoSeletor(b.Nome) {
			continue
		}
		it := itemEscolha{valor: b.Nome, extra: motor.Tamanho(b.Tamanho), detalhe: descreverBanco(b)}
		var usam []string
		for _, p := range m.perfis {
			if p.Origem == conexao && p.OrigemBanco == b.Nome {
				usam = append(usam, p.Nome)
			}
		}
		if len(usam) > 0 {
			it.detalhe += "\njá é a origem de: " + strings.Join(usam, ", ")
		}
		if !b.Conexoes {
			it.apagado = true
			it.detalhe += "\nnão aceita conexões (datallowconn desligado)"
		}
		its = append(its, it)
	}
	return its
}

// itensDestino são "o mesmo nome da origem" e os bancos que já existem no destino, em amarelo:
// escolhido, o banco é substituído a cada cópia.
func itensDestino(m *Model, conexao string, origem []string) []itemEscolha {
	c, _ := m.conexao(conexao)
	existe := map[string]bool{}
	var its []itemEscolha
	for _, b := range c.Info.Bancos {
		existe[b.Nome] = true
		if foraDoSeletor(b.Nome) {
			continue
		}
		its = append(its, itemEscolha{valor: b.Nome, extra: motor.Tamanho(b.Tamanho), aviso: true,
			detalhe: descreverBanco(b) + "\nexiste em " + conexao + ": é substituído a cada cópia, e o antigo vira " + nomes.PrefixoAnteriores(b.Nome) + "<data>"})
	}
	mesmo := itemEscolha{valor: "", rotulo: "o mesmo nome da origem", detalhe: "o banco de destino tem o nome do de origem"}
	if len(origem) == 1 {
		o := origem[0]
		if existe[o] {
			mesmo.aviso = true
			mesmo.detalhe = o + " existe em " + conexao + ": é substituído a cada cópia, e o antigo vira " + nomes.PrefixoAnteriores(o) + "<data>"
		} else {
			mesmo.detalhe = o + " não existe em " + conexao + ": é criado na primeira cópia"
		}
	}
	return append([]itemEscolha{mesmo}, its...)
}

// infoBancos diz de quando é a lista de bancos da conexão.
func infoBancos(m *Model, conexao string) string {
	c, ok := m.conexao(conexao)
	switch {
	case !ok:
		return ""
	case c.Info.VerificadaEm.IsZero():
		return "a conexão ainda não foi verificada: ctrl+r verifica"
	case c.Info.Erro != "":
		return "a última verificação parou em " + c.Info.Camada + ": a lista é de antes"
	}
	return "lida " + idade(c.Info.VerificadaEm)
}

// relerBancos é o ctrl+r do seletor: verifica a conexão de novo, sem perguntar nada (o formulário
// ficaria para trás). O que faltar, a aba 3 pergunta.
func (a *abaPerfis) relerBancos(m *Model, chaveConexao, chaveCampo string) func(f *formulario) tea.Cmd {
	return func(f *formulario) tea.Cmd {
		nome, g := f.valor(chaveConexao), f.geracao
		return m.executar(&tarefa{
			rotulo: "lendo os bancos de " + nome,
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				c, err := m.o.Cadastro.Conexao(ctx, nome)
				if err != nil {
					return nil, err
				}
				return resultadoDiag{nome: nome, d: m.o.Diagnosticar(ctx, c, seg)}, nil
			},
			pronto: func(m *Model, v any, err error) tea.Cmd {
				var aviso string
				if err != nil {
					aviso = err.Error()
				} else {
					r := v.(resultadoDiag)
					m.conexoesA.guardar(m, r.nome, r.d)
					switch {
					case r.d.PrecisaSenha || r.d.PrecisaFrase != "" || r.d.HostDesconhecido != nil:
						aviso = "a verificação de " + nome + " precisa da senha, da passphrase ou de aceitar o servidor SSH: teste a conexão na aba 3 (tecla t)"
					case r.d.Parou() != "":
						aviso = "a verificação de " + nome + " parou em " + r.d.Parou() + ": " + r.d.Info.Erro
					default:
						aviso = fmt.Sprintf("%s: %d banco(s) lido(s) agora", nome, len(r.d.Info.Bancos))
					}
				}
				if m.form.ativo && m.form.geracao == g {
					if c := m.form.campo(chaveCampo); c != nil {
						c.esc.aviso = aviso
					}
				}
				return nil
			},
		})
	}
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
