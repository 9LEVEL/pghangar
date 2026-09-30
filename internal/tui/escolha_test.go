package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

func banco(nome string) cadastro.Banco {
	return cadastro.Banco{Nome: nome, Tamanho: 1 << 20, Dono: "app", Codificacao: "UTF8", Conexoes: true}
}

// comBancos grava na conexão o que uma verificação teria visto.
func comBancos(t *testing.T, m *Model, conexao string, bancos ...cadastro.Banco) {
	t.Helper()
	if err := m.o.Cadastro.GravarInfo(context.Background(), conexao, cadastro.Info{VerificadaEm: time.Now(), Bancos: bancos}); err != nil {
		t.Fatal(err)
	}
	m.recarregar()
}

// seletorDaOrigem prepara os bancos da produção e abre o perfil novo com o foco no banco de origem.
func seletorDaOrigem(t *testing.T) *Model {
	t.Helper()
	m := novoTeste(t, &falsos{})
	fechado := banco("fechado")
	fechado.Conexoes = false
	comBancos(t, m, "prod", banco("loja"), banco("loja_hist"), banco("rh"), banco("postgres"), banco("loja__novo"),
		banco("loja__anterior_20260929_101010"), banco("loja__base"), fechado)
	comBancos(t, m, "homolog", banco("loja"))
	tecla(m, "a", "tab", "tab")
	if c := m.form.campos[m.form.foco]; c.chave != "origem_banco" {
		t.Fatalf("o foco deveria estar no banco de origem, está em %s", c.chave)
	}
	return m
}

func (m *Model) etiquetas(chave string) []string {
	var vs []string
	for _, it := range m.form.campo(chave).esc.visiveis(&m.form) {
		vs = append(vs, it.valor)
	}
	return vs
}

func TestSeletorDeBancosFiltraEMarca(t *testing.T) {
	m := seletorDaOrigem(t)
	// Ficam de fora o postgres e os bancos da ferramenta.
	if v := m.etiquetas("origem_banco"); !reflect.DeepEqual(v, []string{"loja", "loja_hist", "rh", "fechado"}) {
		t.Fatalf("etiquetas: %v", v)
	}
	if tela := m.View(); !strings.Contains(tela, "loja_hist") || !strings.Contains(tela, "digite para filtrar") {
		t.Fatalf("o seletor deveria aparecer aberto:\n%s", tela)
	}
	// O filtro ignora maiúsculas. O texto dele continua oferecido no fim, porque não é o nome exato
	// de um banco (no Postgres, as maiúsculas contam).
	digitar(m, "HIST")
	if v := m.etiquetas("origem_banco"); !reflect.DeepEqual(v, []string{"loja_hist", "HIST"}) {
		t.Fatalf("filtro: %v", v)
	}
	tecla(m, " ")
	c := m.form.campo("origem_banco")
	if !reflect.DeepEqual(c.esc.marcados, []string{"loja_hist"}) || c.esc.filtro != "" {
		t.Fatalf("marcado %v, filtro %q: o espaço marca e limpa o filtro", c.esc.marcados, c.esc.filtro)
	}
	if n := m.form.valor("nome"); n != "loja_hist-homolog" {
		t.Fatalf("o nome vem de <banco>-<destino>: %q", n)
	}
	tecla(m, "enter")
	if m.form.ativo {
		t.Fatalf("o formulário deveria fechar: %s", m.form.erro)
	}
	p, err := m.o.Cadastro.Perfil(context.Background(), "loja_hist-homolog")
	if err != nil || p.OrigemBanco != "loja_hist" || p.DestinoBanco != "loja_hist" {
		t.Fatalf("perfil %+v %v", p, err)
	}
}

func TestSeletorEscLimpaOFiltroAntesDeFechar(t *testing.T) {
	m := seletorDaOrigem(t)
	digitar(m, "zz")
	if v := m.etiquetas("origem_banco"); !reflect.DeepEqual(v, []string{"zz"}) {
		t.Fatalf("sem par, o filtro vira a etiqueta livre: %v", v)
	}
	tecla(m, "esc")
	if !m.form.ativo || m.form.campo("origem_banco").esc.filtro != "" {
		t.Fatal("o primeiro esc limpa o filtro e deixa o formulário aberto")
	}
	tecla(m, "esc")
	if m.form.ativo {
		t.Fatal("sem filtro, o esc fecha o formulário")
	}
}

func TestSeletorBancoFechadoEBancoForaDaLista(t *testing.T) {
	m := seletorDaOrigem(t)
	digitar(m, "fechado")
	tecla(m, " ")
	if len(m.form.campo("origem_banco").esc.marcados) != 0 {
		t.Fatal("um banco que não aceita conexões não pode ser marcado")
	}
	tecla(m, "esc")
	digitar(m, "novo_db")
	tecla(m, " ")
	tecla(m, "enter")
	if p, err := m.o.Cadastro.Perfil(context.Background(), "novo_db-homolog"); err != nil || p.OrigemBanco != "novo_db" {
		t.Fatalf("um nome fora da lista continua valendo: %+v %v", p, err)
	}
}

func TestSeletorSetasSaemNasPontas(t *testing.T) {
	m := seletorDaOrigem(t)
	tecla(m, "right", "right")
	if c := m.form.campo("origem_banco"); c.esc.cursor != 2 {
		t.Fatalf("cursor %d", c.esc.cursor)
	}
	// As quatro etiquetas cabem numa linha: ↑ e ↓ saem para os campos vizinhos.
	tecla(m, "up")
	if c := m.form.campos[m.form.foco]; c.chave != "origem" {
		t.Fatalf("↑ na primeira linha deveria ir à origem, foi a %s", c.chave)
	}
	tecla(m, "down", "down")
	if c := m.form.campos[m.form.foco]; c.chave != "destino" {
		t.Fatalf("↓ na última linha deveria ir ao destino, foi a %s", c.chave)
	}
}

func TestVariosBancosViramVariosPerfis(t *testing.T) {
	m := seletorDaOrigem(t)
	digitar(m, "loja")
	tecla(m, "ctrl+a") // loja e loja_hist: os da ferramenta nem aparecem
	tecla(m, "esc")
	digitar(m, "rh")
	tecla(m, " ")
	if v := m.form.valores("origem_banco"); !reflect.DeepEqual(v, []string{"loja", "loja_hist", "rh"}) {
		t.Fatalf("marcados: %v", v)
	}
	for i, c := range m.form.campos {
		if (c.chave == "nome" || c.chave == "destino_banco") && m.form.aparece(i) {
			t.Fatalf("com vários bancos, o campo %s some: o nome e o destino vêm do banco", c.chave)
		}
	}
	tecla(m, "enter")
	if m.form.ativo {
		t.Fatalf("o formulário deveria fechar: %s", m.form.erro)
	}
	for _, b := range []string{"loja", "loja_hist", "rh"} {
		p, err := m.o.Cadastro.Perfil(context.Background(), b+"-homolog")
		if err != nil || p.OrigemBanco != b || p.DestinoBanco != b || p.Destino != "homolog" {
			t.Fatalf("perfil de %s: %+v %v", b, p, err)
		}
		if !m.perfisA.marcados[b+"-homolog"] {
			t.Fatalf("%s-homolog deveria sair marcado, para o enter copiar em fila", b)
		}
	}
	if !strings.Contains(m.status, "3 perfis") {
		t.Fatalf("status: %q", m.status)
	}
}

func TestVariosBancosComNomeRepetidoNaoSalvaNenhum(t *testing.T) {
	m := seletorDaOrigem(t)
	if err := m.o.Cadastro.SalvarPerfil(context.Background(), "", cadastro.Perfil{Nome: "rh-homolog", Origem: "prod", OrigemBanco: "rh",
		Destino: "homolog", DestinoBanco: "rh", JobsDump: 2, JobsRestore: 4}); err != nil {
		t.Fatal(err)
	}
	m.recarregar()
	digitar(m, "rh")
	tecla(m, " ")
	digitar(m, "loja_hist")
	tecla(m, " ", "enter")
	if !m.form.ativo || !strings.Contains(m.form.erro, "rh-homolog") {
		t.Fatalf("o nome repetido deveria barrar: %q", m.form.erro)
	}
	if _, err := m.o.Cadastro.Perfil(context.Background(), "loja_hist-homolog"); err == nil {
		t.Fatal("nenhum perfil deveria ser salvo")
	}
}

func TestEditarPerfilEscolheUmBancoSo(t *testing.T) {
	m := seletorDaOrigem(t)
	tecla(m, "esc", "e", "tab", "tab")
	c := m.form.campo("origem_banco")
	if !reflect.DeepEqual(c.esc.marcados, []string{"loja"}) || c.esc.multi {
		t.Fatalf("na edição, o seletor começa no banco do perfil e marca um só: %v %v", c.esc.marcados, c.esc.multi)
	}
	digitar(m, "rh")
	tecla(m, " ")
	if !reflect.DeepEqual(c.esc.marcados, []string{"rh"}) || m.form.valor("nome") != "loja" {
		t.Fatalf("o espaço troca o banco, e o nome fica: %v %q", c.esc.marcados, m.form.valor("nome"))
	}
	tecla(m, "enter")
	if p, err := m.o.Cadastro.Perfil(context.Background(), "loja"); err != nil || p.OrigemBanco != "rh" || p.DestinoBanco != "rh" {
		t.Fatalf("perfil %+v %v", p, err)
	}
}

func TestSeletorDoDestinoAvisaOQueESubstituido(t *testing.T) {
	m := seletorDaOrigem(t)
	digitar(m, "loja")
	tecla(m, "right", " ") // loja_hist, que não existe no destino
	tecla(m, "tab", "tab")
	d := m.form.campo("destino_banco")
	its := d.esc.visiveis(&m.form)
	if its[0].valor != "" || its[0].aviso || !strings.Contains(its[0].detalhe, "é criado") {
		t.Fatalf("o mesmo nome, que não existe no destino: %+v", its[0])
	}
	if len(its) != 2 || its[1].valor != "loja" || !its[1].aviso {
		t.Fatalf("os bancos do destino vêm na cor de aviso: %+v", its)
	}
}

func TestCtrlRReleOsBancos(t *testing.T) {
	m := seletorDaOrigem(t)
	tecla(m, "ctrl+r")
	if a := m.form.campo("origem_banco").esc.aviso; !strings.Contains(a, "lido(s) agora") {
		t.Fatalf("aviso do ctrl+r: %q", a)
	}
}

// Com o filtro aberto, o enter marca a etiqueta em destaque (como o espaço) e não salva sem ela.
func TestEnterComFiltroMarca(t *testing.T) {
	m := seletorDaOrigem(t)
	digitar(m, "rh")
	tecla(m, "enter")
	if !m.form.ativo || !reflect.DeepEqual(m.form.valores("origem_banco"), []string{"rh"}) {
		t.Fatalf("o enter com filtro marca e não salva: ativo=%v %v", m.form.ativo, m.form.valores("origem_banco"))
	}
	tecla(m, "enter")
	if m.form.ativo {
		t.Fatalf("o segundo enter salva: %s", m.form.erro)
	}
	if _, err := m.o.Cadastro.Perfil(context.Background(), "rh-homolog"); err != nil {
		t.Fatal(err)
	}
}
