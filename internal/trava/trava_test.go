package trava

import (
	"errors"
	"testing"
)

func TestUmaCopiaPorDestino(t *testing.T) {
	dir := t.TempDir()
	a, err := Obter(dir, "dev", "loja")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Obter(dir, "dev", "loja"); !errors.Is(err, ErrOcupado) {
		t.Fatalf("a segunda trava deveria falhar: %v", err)
	}
	if Livre(dir, "dev", "loja") {
		t.Fatal("deveria estar ocupada")
	}
	// Outro banco ou outra conexão não disputam a mesma trava.
	b, err := Obter(dir, "dev", "loja2")
	if err != nil {
		t.Fatal(err)
	}
	b.Soltar()
	a.Soltar()
	if !Livre(dir, "dev", "loja") {
		t.Fatal("deveria estar livre depois de soltar")
	}
	// "dev"+"xloja" e "devx"+"loja" não colidem.
	if Arquivo(dir, "dev", "xloja") == Arquivo(dir, "devx", "loja") {
		t.Fatal("colisão de nomes")
	}
	// O servidor manda: duas conexões com o mesmo system_identifier são o mesmo destino.
	if Destino("a", "123") != Destino("b", "123") || Destino("a", "") == Destino("b", "") {
		t.Fatal("Destino")
	}
}
