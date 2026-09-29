package versoes

import (
	"errors"
	"testing"
)

// As 9 combinações do docs/ESTRATEGIA.md §5.
func TestEscolherMatriz(t *testing.T) {
	casos := []struct {
		origem, destino, imagem int
		descendo                bool
	}{
		{16, 16, 16, false}, {16, 17, 17, false}, {16, 18, 18, false},
		{17, 16, 0, true}, {17, 17, 17, false}, {17, 18, 18, false},
		{18, 16, 0, true}, {18, 17, 0, true}, {18, 18, 18, false},
	}
	for _, c := range casos {
		img, err := Escolher(c.origem, c.destino, Padrao)
		if c.descendo {
			if !errors.Is(err, ErrDescendo) {
				t.Errorf("%d→%d: esperava bloqueio por descer, veio %v", c.origem, c.destino, err)
			}
			continue
		}
		if err != nil || img != c.imagem {
			t.Errorf("%d→%d: esperava imagem %d, veio %d (%v)", c.origem, c.destino, c.imagem, img, err)
		}
	}
}

func TestEscolherOrigemMaisAntigaQueAsImagens(t *testing.T) {
	// Uma origem 15 para um destino 16 usa a imagem 16: o pg_dump lê servidores mais antigos.
	if img, err := Escolher(15, 16, Padrao); err != nil || img != 16 {
		t.Fatalf("15→16: %d %v", img, err)
	}
	// Um destino 19 sem imagem configurada é recusado com a lista.
	if _, err := Escolher(18, 19, Padrao); err == nil {
		t.Fatal("19 sem imagem deveria ser recusado")
	}
	if _, err := Escolher(0, 18, Padrao); err == nil {
		t.Fatal("versão desconhecida deveria ser recusada")
	}
}

func TestMajorTexto(t *testing.T) {
	if Major(180006) != 18 || Major(160010) != 16 {
		t.Fatal("Major")
	}
	if Texto(170005) != "17.5" || Texto(0) != "?" {
		t.Fatal("Texto")
	}
}

func TestLerLista(t *testing.T) {
	vs, err := LerLista("18, 16,17 16")
	if err != nil || Lista(vs) != "16, 17, 18" {
		t.Fatalf("%v %v", vs, err)
	}
	for _, ruim := range []string{"", "x", "5", "16,abc"} {
		if _, err := LerLista(ruim); err == nil {
			t.Errorf("%q deveria falhar", ruim)
		}
	}
}

func TestConferirOpcoes(t *testing.T) {
	if err := Conferir("pg_restore", []string{"--jobs=4", "--no-subscriptions", "--role=x", "/dump"}, 16); err != nil {
		t.Fatal(err)
	}
	if err := Conferir("pg_restore", []string{"--no-subscriptions"}, 10); err == nil {
		t.Fatal("--no-subscriptions não existe no 10")
	}
	if err := Conferir("pg_dump", []string{"--filter=x"}, 18); err == nil {
		t.Fatal("opção fora da tabela deveria ser recusada")
	}
	if err := Conferir("psql", []string{"--set=ON_ERROR_STOP=1"}, 16); err != nil {
		t.Fatal(err)
	}
}
