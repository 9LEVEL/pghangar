package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/nomes"
)

type abaExecucoes struct{ cursor int }

func (a *abaExecucoes) atual(m *Model) (cadastro.Execucao, bool) {
	if a.cursor < 0 || a.cursor >= len(m.execucoes) {
		return cadastro.Execucao{}, false
	}
	return m.execucoes[a.cursor], true
}

func (a *abaExecucoes) dicas(m *Model) string {
	e, ok := a.atual(m)
	if !ok {
		return ""
	}
	ds := []string{dica("↑↓", "escolher"), dica("enter", "log")}
	switch {
	case !e.Terminou():
		ds = append(ds, dica("c", "cancelar"))
	case e.Estado == cadastro.EstadoAguardando:
		ds = append(ds, dica("t", "trocar mesmo assim"), dica("x", "não trocar"))
	}
	return juntarDicas(ds...)
}

func (a *abaExecucoes) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(m.execucoes)-1, 0))
	case "home", "g":
		a.cursor = 0
	case "end", "G":
		a.cursor = max(len(m.execucoes)-1, 0)
	case "enter", "l":
		if e, ok := a.atual(m); ok {
			id := e.ID
			m.jan.mostrarLog(m.largura, m.altura, fmt.Sprintf("Log da execução #%d (%s)", id, e.Perfil), func() string { return m.o.LerLog(id) })
		}
	case "c":
		e, ok := a.atual(m)
		if !ok || e.Terminou() {
			return nil
		}
		if e.Etapa == "Troca" {
			m.informar("A troca não é cancelada", "A troca de nomes leva segundos, e pará-la no meio deixaria o destino sem banco. Espere terminar; depois, se quiser voltar atrás, use desfazer na aba 5.")
			return nil
		}
		corpo := fmt.Sprintf("A cópia #%d (%s) para na etapa %s. O container é parado.\n\n", e.ID, e.Perfil, e.Etapa)
		if e.Grupo != "" {
			corpo = fmt.Sprintf("A cópia #%d (%s) é de um grupo: cancelar para a cópia em andamento e as que esperam na fila.\n\n", e.ID, e.Perfil)
		}
		switch {
		case e.BancoNovo != "" && (e.Etapa == "Criar __novo" || e.Etapa == "Restore"):
			corpo += fmt.Sprintf("O banco %s fica no destino, incompleto: apague-o na aba Anteriores. O banco %s não é tocado.", e.BancoNovo, e.Banco)
		case e.BancoNovo != "":
			corpo += fmt.Sprintf("O banco %s fica no destino, restaurado mas sem a troca: apague-o na aba Anteriores. O banco %s não é tocado.", e.BancoNovo, e.Banco)
		default:
			corpo += fmt.Sprintf("O banco de destino %s não foi tocado. O dump até aqui fica marcado como incompleto.", e.Banco)
		}
		m.conf.perguntar("Cancelar a cópia #"+fmt.Sprint(e.ID), corpo, func() tea.Cmd {
			if err := m.o.Cancelar(e); err != nil {
				m.erro("Não foi possível cancelar", err)
				return nil
			}
			m.status = fmt.Sprintf("cancelando #%d: o processo para o container e grava o fim", e.ID)
			return nil
		})
	case "t":
		e, ok := a.atual(m)
		if !ok || e.Estado != cadastro.EstadoAguardando {
			return nil
		}
		return a.confirmarTroca(m, e)
	case "x":
		e, ok := a.atual(m)
		if !ok || e.Estado != cadastro.EstadoAguardando {
			return nil
		}
		m.conf.perguntar("Não trocar", fmt.Sprintf("A execução #%d termina sem a troca: o banco %s continua como está.\n\nO banco %s fica no destino até você apagá-lo na aba Anteriores.", e.ID, e.Banco, e.BancoNovo), func() tea.Cmd {
			if err := m.o.Descartar(context.Background(), e.ID); err != nil {
				m.erro("Erro", err)
				return nil
			}
			m.recarregar()
			return nil
		})
	}
	return nil
}

func (a *abaExecucoes) confirmarTroca(m *Model, e cadastro.Execucao) tea.Cmd {
	var p motor.Plano
	_ = json.Unmarshal([]byte(e.Plano), &p)
	corpo := e.Mensagem + "\n\n"
	if p.Destino.Existe {
		corpo += fmt.Sprintf("Trocando, %s passa a ser o banco %s, e o banco atual vira %s<data>, fechado para conexões. As sessões em %s são derrubadas.",
			e.BancoNovo, e.Banco, nomes.PrefixoAnteriores(e.Banco), e.Banco)
	} else {
		corpo += fmt.Sprintf("Trocando, %s passa a ser o banco %s.", e.BancoNovo, e.Banco)
	}
	corpo += "\n\nVeja o log antes (enter na lista)."
	acao := func() tea.Cmd {
		id, destino, banco := e.ID, e.Destino, e.Banco
		return m.executar(&tarefa{
			rotulo: fmt.Sprintf("iniciando a troca da execução #%d", id),
			rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
				// Abre o destino antes: se faltar a senha desta sessão, a tela pergunta agora, e
				// não o processo da troca, que não tem a quem perguntar.
				if _, err := m.o.Listar(ctx, destino, banco, seg); err != nil {
					return nil, err
				}
				return nil, m.o.Trocar(ctx, id, seg)
			},
			pronto: func(m *Model, _ any, err error) tea.Cmd {
				if err != nil {
					m.erro("A troca não começou", err)
					return nil
				}
				m.recarregar()
				m.status = fmt.Sprintf("troca da execução #%d iniciada", id)
				return nil
			},
		})
	}
	if p.Destino.Tag == cadastro.TagHomolog {
		return m.conf.perguntarCritico("Trocar mesmo assim", corpo, e.Banco, acao)
	}
	m.conf.perguntar("Trocar mesmo assim", corpo, acao)
	return nil
}

// puladas são as etapas que esta cópia não roda: sem anteriores marcados, sem script, destino novo.
func puladas(e cadastro.Execucao) map[string]bool {
	r := map[string]bool{}
	if e.Tipo != cadastro.TipoTroca && len(e.Apagar) == 0 {
		r["Anteriores"] = true
	}
	if e.Tipo == cadastro.TipoRestauracao || e.Tipo == cadastro.TipoReset {
		r["Dump"] = true
	}
	if e.Tipo == cadastro.TipoReset {
		for _, x := range []string{"Restore", "Conferência", "Script pós-restore", "Donos", "ANALYZE", "Base"} {
			r[x] = true
		}
	}
	var p motor.Plano
	if json.Unmarshal([]byte(e.Plano), &p) == nil && p.Perfil.Nome != "" {
		if p.Perfil.Script == "" {
			r["Script pós-restore"] = true
		}
		if !p.Destino.Existe {
			r["Configurações do banco"] = true
		}
		if !p.Perfil.GuardarBase || e.Tipo != cadastro.TipoCopia {
			r["Base"] = true
		}
	}
	return r
}

// --- desenho ------------------------------------------------------------------------------------

func (a *abaExecucoes) view(m *Model) string {
	w := m.largura
	if len(m.execucoes) == 0 {
		return "\n" + stDica.Render("  Nenhuma execução ainda. Copie um perfil na aba 1 (enter).")
	}
	var b strings.Builder
	if e, ok := a.atual(m); ok {
		b.WriteString(a.cartao(m, e, w) + "\n")
	}
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" HISTÓRICO (%d)", len(m.execucoes))) + "\n")
	usadas := strings.Count(b.String(), "\n")
	altLista := max(m.alturaCorpo()-usadas-1, 3)
	ini, fim := janelaDeLinhas(len(m.execucoes), a.cursor, altLista)
	for i := ini; i < fim; i++ {
		e := m.execucoes[i]
		marca := "  "
		if i == a.cursor {
			marca = stSelecao.Render("▸ ")
		}
		id := fmt.Sprintf("#%-4d", e.ID)
		nome := preencher(truncar(e.Perfil, 22), 22)
		if i == a.cursor {
			id, nome = stSelecao.Render(id), stSelecao.Render(nome)
		} else {
			id, nome = stRotulo.Render(id), stTexto.Render(nome)
		}
		msg := e.Mensagem
		if !e.Terminou() {
			msg = ""
		}
		b.WriteString(" " + marca + id + " " + nome + " " + resumoExecucao(e) + "  " + stDica.Render(msg) + "\n")
	}
	return b.String()
}

// cartao é a execução escolhida, em blocos: o estado (o que está acontecendo, ou como terminou), as
// etapas, os detalhes, o que pede atenção e o que é só informação.
func (a *abaExecucoes) cartao(m *Model, e cadastro.Execucao, w int) string {
	larg := limitar(w-4, 40, 140)
	var b strings.Builder
	cab := stMarca.Render(fmt.Sprintf("#%d %s", e.ID, e.Perfil)) + "  " + seloEstado(e.Estado) + stDica.Render("  "+e.Destino+"/"+e.Banco)
	if !e.Inicio.IsZero() {
		fim := e.Fim
		if fim.IsZero() {
			fim = time.Now()
		}
		cab += stDica.Render(fmt.Sprintf("  · começou %s · %s", idade(e.Inicio), duracao(fim.Sub(e.Inicio))))
	}
	b.WriteString(cab + "\n\n")
	b.WriteString(faixaEstado(e, larg-2) + "\n\n")

	// As etapas, com a atual em destaque; as que não se aplicam a esta cópia aparecem puladas.
	pula := puladas(e)
	var et []string
	for i, nome := range motor.Etapas {
		n := i + 1
		switch {
		case pula[nome] && (n < e.EtapaNum || e.Estado == cadastro.EstadoOK):
			et = append(et, stDica.Render("– "+nome))
		case e.EtapaNum == 0:
			et = append(et, stDica.Render("· "+nome))
		case n < e.EtapaNum:
			et = append(et, stOk.Render("✔ ")+stDica.Render(nome))
		case n == e.EtapaNum && !e.Terminou():
			et = append(et, stSelecao.Render("▶ "+nome))
		case n == e.EtapaNum && e.Estado == cadastro.EstadoOK:
			et = append(et, stOk.Render("✔ ")+stDica.Render(nome))
		case n == e.EtapaNum && e.Estado == cadastro.EstadoAguardando:
			et = append(et, stAvisoV.Render("⚠ "+nome))
		case n == e.EtapaNum && e.Estado == cadastro.EstadoCancelada:
			et = append(et, stAvisoV.Render("■ "+nome))
		case n == e.EtapaNum:
			et = append(et, stPerigoV.Render("✖ "+nome))
		default:
			et = append(et, stDica.Render("· "+nome))
		}
	}
	b.WriteString(quebrar(stCabecalho.Render(preencher("ETAPAS", 10)), strings.Join(et, "  "), larg-2) + "\n")

	detalhe := func(rot, val string) { b.WriteString(quebrar(stRotulo.Render(preencher(rot, 10)), val, larg-2) + "\n") }
	if e.Operador != "" {
		detalhe("operador", stTexto.Render(e.Operador))
	}
	if e.DumpDir != "" {
		s := e.DumpDir
		if e.TamanhoDump > 0 {
			s += " (" + motor.Tamanho(e.TamanhoDump) + ")"
			if e.Etapa == "Dump" && !e.Terminou() {
				s += " escritos até agora"
			}
		}
		detalhe("dump", stTexto.Render(s))
	}
	if e.BancoNovo != "" && e.BancoAnterior == "" && e.Estado != cadastro.EstadoOK {
		detalhe("__novo", stTexto.Render(e.BancoNovo))
	}
	if e.BancoAnterior != "" {
		detalhe("anterior", stTexto.Render(e.BancoAnterior)+stDica.Render(" (aba 5 para desfazer ou apagar)"))
	}
	if len(e.Apagar) > 0 {
		detalhe("apagados", stTexto.Render(strings.Join(e.Apagar, ", ")))
	}

	// Primeiro o que pede atenção, depois o que é só informação: a execução antiga, que não
	// separava, tem tudo em atenção.
	if len(e.Avisos) > 0 {
		b.WriteString("\n" + stAvisoV.Render(fmt.Sprintf("ATENÇÃO (%d)", len(e.Avisos))) + "\n")
		for _, av := range e.Avisos {
			b.WriteString(quebrar(stAvisoV.Render(" ! "), stTexto.Render(av), larg-2) + "\n")
		}
	}
	if len(e.Notas) > 0 {
		b.WriteString("\n" + stCabecalho.Render(fmt.Sprintf("INFORMAÇÕES (%d)", len(e.Notas))) + "\n")
		for _, n := range e.Notas {
			b.WriteString(quebrar(stDica.Render(" · "), stDica.Render(n), larg-2) + "\n")
		}
	}
	return stCartao.Width(larg).Render(strings.TrimRight(b.String(), "\n"))
}

// oQueE é o nome do trabalho da execução e a terminação dele ("a" ou "o"): cópia concluída, reset
// concluído.
func oQueE(e cadastro.Execucao) (nome, g string) {
	switch e.Tipo {
	case cadastro.TipoRestauracao:
		return "restauração", "a"
	case cadastro.TipoReset:
		return "reset", "o"
	case cadastro.TipoTroca:
		return "troca", "a"
	}
	return "cópia", "a"
}

// faixaEstado é o bloco do estado, com uma faixa na cor dele: em andamento, na fila, concluída,
// esperando a decisão, cancelada ou com erro. É o que a pessoa lê primeiro.
func faixaEstado(e cadastro.Execucao, larg int) string {
	nome, g := oQueE(e)
	var p motor.Plano
	_ = json.Unmarshal([]byte(e.Plano), &p)
	cor := corDestaque
	var titulo string
	var linhas []string
	switch e.Estado {
	case cadastro.EstadoFila:
		cor, titulo = corApagada, "⏳ NA FILA DO GRUPO"
		linhas = append(linhas, stDica.Render("começa quando a anterior do grupo terminar"))
	case cadastro.EstadoIniciando, cadastro.EstadoRodando:
		titulo = "▶ EM ANDAMENTO"
		if e.EtapaNum > 0 {
			titulo += fmt.Sprintf(" · etapa %d de %d: %s", e.EtapaNum, len(motor.Etapas), e.Etapa)
		}
		if e.Total > 0 {
			pct := e.Feito * 100 / max(e.Total, 1)
			linhas = append(linhas, fmt.Sprintf("%s %s %s", barra(e.Feito, e.Total, limitar(larg-40, 10, 60)), stValor.Render(fmt.Sprintf("%3d%%", pct)),
				stDica.Render(fmt.Sprintf("%d/%d  %s", e.Feito, e.Total, truncar(e.Item, 50)))))
		}
	case cadastro.EstadoOK:
		cor, titulo = corOk, strings.ToUpper("✔ "+nome+" concluíd"+g)
		if e.Mensagem != "" {
			linhas = append(linhas, stTexto.Render(e.Mensagem))
		}
		var fatos []string
		if !e.Fim.IsZero() && !e.Inicio.IsZero() {
			fatos = append(fatos, "em "+duracao(e.Fim.Sub(e.Inicio)))
		}
		if e.TamanhoDump > 0 && e.Tipo == cadastro.TipoCopia {
			fatos = append(fatos, "dump de "+motor.Tamanho(e.TamanhoDump))
		}
		switch {
		case e.BancoAnterior != "":
			fatos = append(fatos, "o banco que estava lá virou "+e.BancoAnterior+" (a aba 5 desfaz ou apaga)")
		case e.Tipo == cadastro.TipoCopia && p.Perfil.Nome != "" && !p.Destino.Existe:
			fatos = append(fatos, "o banco não existia no destino e foi criado")
		}
		if len(fatos) > 0 {
			linhas = append(linhas, stDica.Render(strings.Join(fatos, " · ")))
		}
		if n := len(e.Avisos); n > 0 {
			linhas = append(linhas, stAvisoV.Render(fmt.Sprintf("%d ponto(s) de atenção abaixo", n)))
		}
	case cadastro.EstadoAguardando:
		cor, titulo = corAviso, "⚠ A TROCA ESPERA A SUA DECISÃO"
		linhas = append(linhas, stTexto.Render(e.Mensagem),
			dica("enter", "ver o log")+"   "+dica("t", "trocar mesmo assim")+"   "+dica("x", "não trocar"))
	case cadastro.EstadoCancelada:
		cor, titulo = corAviso, strings.ToUpper("■ "+nome+" cancelad"+g+" na etapa ")+e.Etapa
		linhas = append(linhas, stTexto.Render(e.Mensagem))
	default:
		cor, titulo = corPerigo, "✖ FALHOU NA ETAPA "+e.Etapa
		if e.Estado == cadastro.EstadoInterrompida {
			titulo = "✖ INTERROMPIDA NA ETAPA " + e.Etapa
		}
		linhas = append(linhas, stTexto.Render(e.Mensagem), dica("enter", "ver o log"))
	}
	corpo := lipgloss.NewStyle().Foreground(cor).Bold(true).Render(titulo)
	for _, l := range linhas {
		if l != "" {
			corpo += "\n" + l
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(cor).
		PaddingLeft(1).Width(larg).Render(corpo)
}

// resumoFim é o aviso do rodapé quando uma execução termina.
func resumoFim(e cadastro.Execucao) string {
	nome, g := oQueE(e)
	id := fmt.Sprintf("#%d %s", e.ID, e.Perfil)
	switch e.Estado {
	case cadastro.EstadoOK:
		s := fmt.Sprintf("✔ %s concluíd%s", id, g)
		if !e.Fim.IsZero() && !e.Inicio.IsZero() {
			s += " em " + duracao(e.Fim.Sub(e.Inicio))
		}
		if n := len(e.Avisos); n > 0 {
			s += fmt.Sprintf(", com %d ponto(s) de atenção (aba 2)", n)
		}
		return s
	case cadastro.EstadoAguardando:
		return fmt.Sprintf("⚠ %s: a troca espera a sua decisão (aba 2)", id)
	case cadastro.EstadoCancelada:
		return fmt.Sprintf("■ %s: %s cancelad%s", id, nome, g)
	}
	return fmt.Sprintf("✖ %s: falhou na etapa %s (aba 2)", id, e.Etapa)
}
