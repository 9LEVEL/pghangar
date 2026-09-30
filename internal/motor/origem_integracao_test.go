//go:build integracao

package motor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
)

// A origem é aberta só de leitura: uma escrita, pela conexão administrativa ou pela do banco, é
// recusada pelo próprio Postgres, e o DSN do pg_dump leva o mesmo. O destino continua escrevendo.
func TestOrigemSoLeitura(t *testing.T) {
	a := novoAmbiente(t)
	ctx := context.Background()
	o := subir(t, "o18", 18)
	a.conexao(t, "prod", cadastro.TagProd, o)
	semear(t, o, "loja_leitura")

	l, _, err := abrirOrigem(ctx, a.d, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer l.fechar()
	recusada := func(c *pgx.Conn, q string) {
		t.Helper()
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "25006" {
			t.Fatalf("%s deveria ser recusado (read_only_sql_transaction): %v", q, err)
		}
	}
	recusada(l.admin, "CREATE DATABASE nao_deveria")
	c, err := l.conectar(ctx, a.d, "loja_leitura")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	recusada(c, "CREATE TABLE public.nao_deveria (x int)")
	recusada(c, "INSERT INTO public.log_eventos (msg) VALUES ('nao deveria')")
	recusada(c, "DELETE FROM public.log_eventos")
	if n := valor[int64](t, o, "loja_leitura", `SELECT count(*) FROM public.log_eventos`); n != 5000 {
		t.Fatalf("a origem mudou: %d eventos", n)
	}
	if d := conexao.DSN(l.c, l.ponte, "loja_leitura", ""); !strings.Contains(d, "default_transaction_read_only=on") {
		t.Fatalf("o DSN do pg_dump precisa do options: %s", d)
	}

	ld, _, err := abrirLado(ctx, a.d, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer ld.fechar()
	if ld.ponte.SoLeitura {
		t.Fatal("o lado do destino não pode ser só de leitura")
	}
}
