package execucao

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
	"github.com/9LEVEL/copia-banco/internal/local"
	"github.com/9LEVEL/copia-banco/internal/trava"
)

func TestLerSegredos(t *testing.T) {
	s, err := LerSegredos(strings.NewReader(`{"senhas":{"prod":"a:b\\c"},"frases":{"/k":"x"}}`))
	if err != nil || s.Senhas["prod"] != `a:b\c` || s.Frases["/k"] != "x" {
		t.Fatal(s, err)
	}
	if s, err := LerSegredos(strings.NewReader("  ")); err != nil || s.Senhas != nil {
		t.Fatal(s, err)
	}
	if _, err := LerSegredos(strings.NewReader("{")); err == nil {
		t.Fatal("JSON quebrado deveria falhar")
	}
}

func TestProcessoDaExecucao(t *testing.T) {
	// O próprio processo de teste não é o "executar" de execução nenhuma.
	if processoDaExecucao(os.Getpid(), 1) {
		t.Fatal("o teste não é uma execução")
	}
	if processoDaExecucao(1<<30, 1) {
		t.Fatal("pid inexistente")
	}
	if err := Cancelar(cadastro.Execucao{ID: 1, Estado: cadastro.EstadoRodando, PID: os.Getpid()}); err == nil {
		t.Fatal("cancelar um pid que não é da execução deveria ser recusado")
	}
	if err := Cancelar(cadastro.Execucao{ID: 1, Estado: cadastro.EstadoOK}); err == nil {
		t.Fatal("cancelar uma execução terminada")
	}
}

func TestConferirMarcaInterrompidaELimpaPgpass(t *testing.T) {
	ctx := context.Background()
	dir := local.Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := dir.Preparar(); err != nil {
		t.Fatal(err)
	}
	cad, err := cadastro.Abrir(dir.Estado())
	if err != nil {
		t.Fatal(err)
	}
	defer cad.Fechar()
	velha := time.Now().Add(-time.Hour)
	morta, _ := cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoRodando, Destino: "d", Banco: "b", Inicio: velha, BancoNovo: "b__novo"})
	e, _ := cad.Execucao(ctx, morta)
	e.PID = 1 << 30 // não existe
	_ = cad.GravarExecucao(ctx, e)
	subindo, _ := cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando, Destino: "d", Banco: "c"})
	// Uma execução cujo destino está travado (o processo segura a trava) não é tocada.
	presa, _ := cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: "p", Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoRodando, Destino: "d", Banco: "e", Inicio: velha})
	tr, err := trava.Obter(dir.Travas(), trava.Destino("d", ""), "e")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Soltar()
	for _, id := range []int64{morta, subindo} {
		_ = os.WriteFile(filepath.Join(dir.Temp(), strings.Join([]string{itoa(id), "x.pgpass"}, "-")), []byte("s"), 0o600)
	}

	if err := Conferir(ctx, cad, dir); err != nil {
		t.Fatal(err)
	}
	if e, _ := cad.Execucao(ctx, morta); e.Estado != cadastro.EstadoInterrompida || !strings.Contains(e.Mensagem, "b__novo") {
		t.Fatalf("%s %s", e.Estado, e.Mensagem)
	}
	if e, _ := cad.Execucao(ctx, subindo); e.Estado != cadastro.EstadoIniciando {
		t.Fatalf("a que acabou de subir não é interrompida: %s", e.Estado)
	}
	if e, _ := cad.Execucao(ctx, presa); e.Estado != cadastro.EstadoRodando {
		t.Fatalf("a que segura a trava não é interrompida: %s", e.Estado)
	}
	ms, _ := filepath.Glob(filepath.Join(dir.Temp(), "*.pgpass"))
	if len(ms) != 1 || !strings.HasPrefix(filepath.Base(ms[0]), itoa(subindo)+"-") {
		t.Fatalf("só o pgpass da execução viva fica: %v", ms)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
