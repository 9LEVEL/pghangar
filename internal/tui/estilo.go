// Package tui é a tela da ferramenta, no padrão do pgtower: cabeçalho com a versão, abas
// numeradas, rodapé com as teclas da aba, ajuda no "?", e janelas por cima para formulários,
// confirmações e avisos.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
)

// Paleta adaptável (terminal claro e escuro), a mesma do pgtower.
var (
	corDestaque = lipgloss.AdaptiveColor{Light: "#0b6bcb", Dark: "#4c9fff"}
	corApagada  = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#8a8f98"}
	corSutil    = lipgloss.AdaptiveColor{Light: "#9ca3af", Dark: "#5c6370"}
	corTexto    = lipgloss.AdaptiveColor{Light: "#1f2933", Dark: "#e6e6e6"}
	corOk       = lipgloss.AdaptiveColor{Light: "#0a7d33", Dark: "#4ec26b"}
	corAviso    = lipgloss.AdaptiveColor{Light: "#b45309", Dark: "#e0a92e"}
	corPerigo   = lipgloss.AdaptiveColor{Light: "#c02626", Dark: "#ff5c5c"}
	corBorda    = lipgloss.AdaptiveColor{Light: "#d1d5db", Dark: "#3a3f4b"}
	corSobre    = lipgloss.Color("#ffffff")
)

var (
	stTitulo    = lipgloss.NewStyle().Bold(true).Foreground(corSobre).Background(corDestaque).Padding(0, 1)
	stVersao    = lipgloss.NewStyle().Foreground(corSobre).Background(corApagada).Bold(true).Padding(0, 1)
	stAbaAtiva  = lipgloss.NewStyle().Bold(true).Foreground(corSobre).Background(corDestaque).Padding(0, 2)
	stAbaInat   = lipgloss.NewStyle().Foreground(corApagada).Padding(0, 2)
	stStatus    = lipgloss.NewStyle().Foreground(corApagada)
	stErro      = lipgloss.NewStyle().Foreground(corPerigo).Bold(true)
	stDica      = lipgloss.NewStyle().Foreground(corSutil)
	stTecla     = lipgloss.NewStyle().Foreground(corDestaque).Bold(true)
	stRotulo    = lipgloss.NewStyle().Foreground(corApagada)
	stValor     = lipgloss.NewStyle().Foreground(corTexto).Bold(true)
	stTexto     = lipgloss.NewStyle().Foreground(corTexto)
	stOk        = lipgloss.NewStyle().Foreground(corOk).Bold(true)
	stAvisoV    = lipgloss.NewStyle().Foreground(corAviso).Bold(true)
	stPerigoV   = lipgloss.NewStyle().Foreground(corPerigo).Bold(true)
	stModal     = lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2)
	stMarca     = lipgloss.NewStyle().Foreground(corDestaque).Bold(true)
	stCartao    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(corBorda).Padding(0, 1)
	stSelecao   = lipgloss.NewStyle().Foreground(corDestaque).Bold(true)
	stCabecalho = lipgloss.NewStyle().Foreground(corApagada).Bold(true)
)

// dica renderiza "tecla rótulo" para o rodapé.
func dica(tecla, rotulo string) string { return stTecla.Render(tecla) + " " + stDica.Render(rotulo) }

func juntarDicas(d ...string) string {
	var out []string
	for _, x := range d {
		if x != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, "   ")
}

// fundo é um selo com cor de fundo (cabeçalho, estados).
func fundo(c lipgloss.TerminalColor, t string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(corSobre).Background(c).Padding(0, 1).Render(t)
}

// seloTag pinta a tag da conexão: prod em vermelho, sempre.
func seloTag(tag string) string {
	switch tag {
	case cadastro.TagProd:
		return fundo(corPerigo, "PROD")
	case cadastro.TagHomolog:
		return fundo(corAviso, "HOMOLOG")
	}
	return fundo(corOk, "DEV")
}

// seloEstado pinta o estado de uma execução.
func seloEstado(estado string) string {
	switch estado {
	case cadastro.EstadoOK:
		return stOk.Render("✔ ok")
	case cadastro.EstadoRodando, cadastro.EstadoIniciando:
		return stSelecao.Render("⟳ " + estado)
	case cadastro.EstadoAguardando:
		return stAvisoV.Render("⚠ aguardando decisão")
	case cadastro.EstadoFila:
		return stDica.Render("⏳ na fila")
	case cadastro.EstadoCancelada:
		return stAvisoV.Render("■ cancelada")
	case cadastro.EstadoInterrompida:
		return stPerigoV.Render("✖ interrompida")
	}
	return stPerigoV.Render("✖ " + estado)
}

// idade escreve "há 3 min", "ontem 14:02", "29/09 14:02".
func idade(t time.Time) string {
	if t.IsZero() {
		return "nunca"
	}
	d := time.Since(t)
	hoje := time.Now().Truncate(24 * time.Hour)
	switch {
	case d < 0:
		return t.Format("02/01 15:04")
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return fmt.Sprintf("há %d min", int(d.Minutes()))
	case t.After(hoje) || d < 12*time.Hour:
		return "hoje " + t.Format("15:04")
	case d < 36*time.Hour:
		return "ontem " + t.Format("15:04")
	}
	return t.Format("02/01 15:04")
}

// duracao escreve 3m12s, 1h04m, 42s.
func duracao(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// intervalo escreve "1 min", "30 s".
func intervalo(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d s", int(d.Seconds()))
}

// barra desenha uma barra de progresso com a largura dada.
func barra(feito, total, largura int) string {
	if total <= 0 || largura <= 0 {
		return ""
	}
	cheio := min(largura, feito*largura/total)
	return stSelecao.Render(strings.Repeat("█", cheio)) + stDica.Render(strings.Repeat("░", largura-cheio))
}
