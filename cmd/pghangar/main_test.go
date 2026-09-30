package main

import (
	"strings"
	"testing"
)

func TestComandos(t *testing.T) {
	if err := rodar([]string{"versao"}); err != nil {
		t.Fatal(err)
	}
	if err := rodar([]string{"ajuda"}); err != nil {
		t.Fatal(err)
	}
	if err := rodar([]string{"nao-existe"}); err == nil || !strings.Contains(err.Error(), "desconhecido") {
		t.Fatal(err)
	}
	if err := rodar([]string{"executar"}); err == nil || !strings.Contains(err.Error(), "--execucao") {
		t.Fatal(err)
	}
	if Versao() == "" {
		t.Fatal("versão vazia")
	}
}
