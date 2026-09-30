package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/motor"
)

func execucaoOK() cadastro.Execucao {
	pl, _ := json.Marshal(motor.Plano{Perfil: cadastro.Perfil{Nome: "loja"}, Destino: motor.Lado{Existe: false}})
	ini := time.Now().Add(-192 * time.Second)
	return cadastro.Execucao{ID: 3, Perfil: "loja", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoOK, EtapaNum: 12, Etapa: "Troca",
		Inicio: ini, Fim: ini.Add(192 * time.Second), Destino: "homolog", Banco: "loja", TamanhoDump: 1 << 30,
		Mensagem: "loja copiado de prod/loja", Plano: string(pl),
		Avisos: []string{"a extensão vector está na versão 0.8.6 na origem e na 0.8.1 no destino, mais antiga"},
		Notas:  []string{"o banco loja não existe no destino: vai ser criado"}}
}

// O estado vem primeiro, em bloco próprio; depois, o que pede atenção, separado do que é só
// informação.
func TestCartaoConcluidoSeparaEstadoAtencaoEInformacao(t *testing.T) {
	m := novoTeste(t, &falsos{})
	c := m.execA.cartao(m, execucaoOK(), 120)
	estado, atencao, info := strings.Index(c, "CÓPIA CONCLUÍDA"), strings.Index(c, "ATENÇÃO (1)"), strings.Index(c, "INFORMAÇÕES (1)")
	if estado < 0 || atencao < 0 || info < 0 || !(estado < atencao && atencao < info) {
		t.Fatalf("a ordem é estado, atenção, informações:\n%s", c)
	}
	for _, s := range []string{"em 3m12s", "dump de 1,0 GB", "o banco não existia no destino e foi criado", "1 ponto(s) de atenção abaixo"} {
		if !strings.Contains(c, s) {
			t.Fatalf("faltou %q no estado:\n%s", s, c)
		}
	}
	if got := resumoFim(execucaoOK()); got != "✔ #3 loja concluída em 3m12s, com 1 ponto(s) de atenção (aba 2)" {
		t.Fatalf("rodapé: %q", got)
	}
}

func TestCartaoEmAndamentoEComErro(t *testing.T) {
	m := novoTeste(t, &falsos{})
	e := execucaoOK()
	e.Estado, e.EtapaNum, e.Etapa, e.Feito, e.Total, e.Fim = cadastro.EstadoRodando, 5, "Restore", 29, 58, time.Time{}
	if c := m.execA.cartao(m, e, 120); !strings.Contains(c, "EM ANDAMENTO · etapa 5 de 12: Restore") || !strings.Contains(c, "50%") {
		t.Fatalf("em andamento:\n%s", c)
	}
	e.Estado, e.EtapaNum, e.Etapa, e.Mensagem = cadastro.EstadoErro, 4, "Criar __novo", "o disco encheu"
	c := m.execA.cartao(m, e, 120)
	if !strings.Contains(c, "FALHOU NA ETAPA Criar __novo") || !strings.Contains(c, "o disco encheu") || strings.Contains(c, "CONCLUÍDA") {
		t.Fatalf("com erro:\n%s", c)
	}
	if got := resumoFim(e); !strings.HasPrefix(got, "✖ #3 loja: falhou na etapa Criar __novo") {
		t.Fatalf("rodapé: %q", got)
	}
	e.Estado, e.Mensagem = cadastro.EstadoAguardando, "o restore teve 2 erro(s)"
	if c := m.execA.cartao(m, e, 120); !strings.Contains(c, "A TROCA ESPERA A SUA DECISÃO") || !strings.Contains(c, "trocar mesmo assim") {
		t.Fatalf("aguardando:\n%s", c)
	}
}

// Uma execução de antes das notas tem tudo em atenção, como era.
func TestCartaoDeExecucaoAntiga(t *testing.T) {
	m := novoTeste(t, &falsos{})
	e := execucaoOK()
	e.Notas = nil
	if c := m.execA.cartao(m, e, 120); strings.Contains(c, "INFORMAÇÕES") || !strings.Contains(c, "ATENÇÃO (1)") {
		t.Fatalf("antiga:\n%s", c)
	}
}
