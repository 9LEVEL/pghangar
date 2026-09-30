package nomes

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNomesCurtos(t *testing.T) {
	q := time.Date(2026, 9, 29, 14, 2, 7, 0, time.Local)
	if Novo("loja") != "loja__novo" {
		t.Fatal(Novo("loja"))
	}
	if a := Anterior("loja", q); a != "loja__anterior_20260929_140207" {
		t.Fatal(a)
	}
	d, ok := DataDoAnterior("loja", "loja__anterior_20260929_140207")
	if !ok || !d.Equal(q) {
		t.Fatal(d, ok)
	}
	if _, ok := DataDoAnterior("loja", "loja__anterior_x"); ok {
		t.Fatal("data inválida aceita")
	}
	if _, ok := DataDoAnterior("loja", "lojax__anterior_20260929_140207"); ok {
		t.Fatal("anterior de outro banco aceito")
	}
	if !EhDaFerramenta("loja", "loja__novo") || EhDaFerramenta("loja", "loja") || EhDaFerramenta("loja", "outra__novo") {
		t.Fatal("EhDaFerramenta")
	}
}

func TestNomesLongosCabemEm63Bytes(t *testing.T) {
	q := time.Now()
	for _, banco := range []string{
		strings.Repeat("a", 36), strings.Repeat("a", 37), strings.Repeat("a", 63),
		strings.Repeat("ç", 30), // 60 bytes, 2 por caractere
	} {
		for _, n := range []string{Novo(banco), Anterior(banco, q)} {
			if len(n) > Limite {
				t.Errorf("%q tem %d bytes", n, len(n))
			}
			if !utf8.ValidString(n) {
				t.Errorf("%q partiu um caractere", n)
			}
		}
		if _, ok := DataDoAnterior(banco, Anterior(banco, q)); !ok {
			t.Errorf("anterior de %q não é reconhecido", banco)
		}
	}
	// Dois nomes longos que só diferem no fim não colidem.
	a, b := strings.Repeat("x", 60)+"a", strings.Repeat("x", 60)+"b"
	if Base(a) == Base(b) {
		t.Fatal("bases iguais para nomes diferentes")
	}
	if Base(strings.Repeat("a", 36)) != strings.Repeat("a", 36) {
		t.Fatal("um nome que cabe não deve ser encurtado")
	}
}

func TestRoleTemporaria(t *testing.T) {
	if RoleTemporaria("a1b2c3", 42) != "pghangar_a1b2c3_42" || !EhRoleTemporaria("a1b2c3", "pghangar_a1b2c3_42") {
		t.Fatal(RoleTemporaria("a1b2c3", 42))
	}
	for _, n := range []string{"pghangar_a1b2c3_", "pghangar_a1b2c3_4x", "pghangar_ffffff_42", "pghangar_42", "postgres", "xpghangar_a1b2c3_1"} {
		if EhRoleTemporaria("a1b2c3", n) {
			t.Errorf("%s não é role temporária da instância", n)
		}
	}
	if EhRoleTemporaria("", "pghangar__1") {
		t.Error("sem instância, nada é da ferramenta")
	}
}

func TestBancoBase(t *testing.T) {
	if BancoBase("loja") != "loja__base" || !EhDaFerramenta("loja", "loja__base") || EhDaFerramenta("loja", "outra__base") {
		t.Fatal(BancoBase("loja"))
	}
	if len(BancoBase(strings.Repeat("x", 63))) > Limite {
		t.Fatal("a base de um nome longo cabe em 63 bytes")
	}
}
