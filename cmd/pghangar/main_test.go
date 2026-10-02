package main

import (
	"flag"
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

// O restaurar aceita as opções antes e depois do arquivo.
func TestArgumentosIntercalados(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	destino := fs.String("destino", "", "")
	banco := fs.String("banco", "", "")
	pos, err := argumentos(fs, []string{"--destino", "dev", "loja.dump", "--banco", "loja"})
	if err != nil || len(pos) != 1 || pos[0] != "loja.dump" || *destino != "dev" || *banco != "loja" {
		t.Fatalf("%v %v %q %q", pos, err, *destino, *banco)
	}
	if err := rodar([]string{"restaurar", "loja.dump"}); err == nil || !strings.Contains(err.Error(), "--destino") {
		t.Fatalf("sem destino e banco, o uso: %v", err)
	}
}
