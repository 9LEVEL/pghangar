package motor

import (
	"strings"
	"testing"
)

func TestCorrecoesPadraoEBloqueio(t *testing.T) {
	var p Plano
	oferecerRoles(&p, []string{"auditor", "leitor"})
	oferecerFDW(&p, []string{"erp_prod"})
	p.Correcoes = append(p.Correcoes, Correcao{Tipo: CorrecaoNovo, Nomes: []string{"loja__novo"}, Marcada: true, Bloqueia: true, SeNao: "sobrou o loja__novo"})
	roles, _ := p.correcao(CorrecaoRoles)
	fdw, _ := p.correcao(CorrecaoFDW)
	if !roles.Marcada || fdw.Marcada || !strings.Contains(roles.Texto, "auditor, leitor") || !strings.Contains(fdw.SeNao, "erp_prod") {
		t.Fatalf("padrões: roles %+v, fdw %+v", roles, fdw)
	}
	if p.Bloqueado() {
		t.Fatal("com a correção do __novo marcada, a cópia pode começar")
	}
	p.Correcoes[2].Marcada = false
	if !p.Bloqueado() || p.BloqueiosAtivos()[0] != "sobrou o loja__novo" {
		t.Fatalf("desmarcada, ela bloqueia: %v", p.BloqueiosAtivos())
	}
	p.Correcoes[2].Marcada = true
	p.DesmarcarCorrecoes()
	for _, c := range p.Correcoes {
		if c.Marcada {
			t.Fatal("sem a tela, nenhuma correção fica marcada")
		}
	}
}

// Na execução, uma correção vale como foi confirmada, e só com os mesmos nomes.
func TestAdotarEscolhas(t *testing.T) {
	var confirmado, agora Plano
	oferecerRoles(&confirmado, []string{"b", "a"})
	oferecerFDW(&confirmado, []string{"erp"})
	confirmado.Correcoes[1].Marcada = true // o sysadmin marcou a do FDW
	oferecerRoles(&agora, []string{"a", "b"})
	oferecerFDW(&agora, []string{"erp"})
	if err := agora.AdotarEscolhas(confirmado); err != nil {
		t.Fatal(err)
	}
	roles, _ := agora.correcao(CorrecaoRoles)
	fdw, _ := agora.correcao(CorrecaoFDW)
	if !roles.Marcada {
		t.Fatal("a das roles, marcada pelo padrão, e com as mesmas roles em outra ordem, vale")
	}
	if !fdw.Marcada {
		t.Fatal("a mesma do FDW, marcada, vale")
	}
	// Apareceu outro servidor depois da confirmação: a marcada mudou, e a execução pede outra
	// confirmação em vez de seguir sem ela.
	agora.Correcoes[1].Nomes = []string{"erp", "crm"}
	if err := agora.AdotarEscolhas(confirmado); err == nil {
		t.Fatal("uma correção marcada que mudou pede outra confirmação")
	}
	// O __novo: o mesmo nome, outro banco (OID).
	var c1, c2 Plano
	c1.Correcoes = []Correcao{{Tipo: CorrecaoNovo, Nomes: []string{"x__novo"}, OID: 10, Marcada: true}}
	c2.Correcoes = []Correcao{{Tipo: CorrecaoNovo, Nomes: []string{"x__novo"}, OID: 11}}
	if err := c2.AdotarEscolhas(c1); err == nil {
		t.Fatal("outro __novo com o mesmo nome não é o que o sysadmin viu")
	}
	if err := agora.AdotarEscolhas(Plano{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range agora.Correcoes {
		if c.Marcada {
			t.Fatal("sem confirmação, nada vale")
		}
	}
}

func TestMarcaDoNovo(t *testing.T) {
	inst, id, ok := lerMarcaDoNovo(marcaDoNovo("a1b2c3", 42))
	if !ok || inst != "a1b2c3" || id != 42 {
		t.Fatalf("%q %d %v", inst, id, ok)
	}
	if _, _, ok := lerMarcaDoNovo("homolog da loja"); ok {
		t.Fatal("um comentário qualquer não é a marca")
	}
}
