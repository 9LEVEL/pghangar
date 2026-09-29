package motor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
)

func TestAvisar(t *testing.T) {
	var recebido Aviso
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &recebido)
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	e := cadastro.Execucao{ID: 7, Perfil: "loja", Estado: cadastro.EstadoOK, Mensagem: "loja copiado", Destino: "homolog", Banco: "loja",
		Inicio: time.Now().Add(-3 * time.Minute), Fim: time.Now()}
	if err := Avisar(context.Background(), srv.URL+"/segredo", NovoAviso(e, "dev-01")); err != nil {
		t.Fatal(err)
	}
	if recebido.Execucao != 7 || !strings.Contains(recebido.Text, "loja copiado") || recebido.DuracaoS < 170 {
		t.Fatalf("%+v", recebido)
	}
	// Um erro não mostra a URL (ela costuma ser o segredo).
	err := Avisar(context.Background(), "http://127.0.0.1:1/segredo-xyz", NovoAviso(e, "x"))
	if err == nil || strings.Contains(err.Error(), "segredo-xyz") {
		t.Fatalf("o erro não pode levar a URL: %v", err)
	}
	ruim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer ruim.Close()
	if err := Avisar(context.Background(), ruim.URL, NovoAviso(e, "x")); err == nil {
		t.Fatal("500 deveria ser erro")
	}
}
