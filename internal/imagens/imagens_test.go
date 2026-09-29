package imagens

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestArgs(t *testing.T) {
	e := Execucao{
		Imagem: "postgres@sha256:abc", Nome: "copia-banco-7-dump",
		Rotulos:  map[string]string{Rotulo: "7"},
		Volumes:  []Volume{{Origem: "/var/lib/copia-banco/dumps/p", Destino: "/dump"}, {Origem: "/tmp/x.pgpass", Destino: "/run/copia-banco/pgpass", SoLeitura: true}},
		Ambiente: map[string]string{"PGPASSFILE": "/run/copia-banco/pgpass"},
		Comando:  []string{"pg_dump", "--jobs=2"},
	}
	a := strings.Join(e.Args(), " ")
	for _, esperado := range []string{
		"run --rm --pull never --network host --name copia-banco-7-dump",
		"--label copia-banco.execucao=7",
		"--volume /tmp/x.pgpass:/run/copia-banco/pgpass:ro",
		"--env PGPASSFILE=/run/copia-banco/pgpass",
		"postgres@sha256:abc pg_dump --jobs=2",
	} {
		if !strings.Contains(a, esperado) {
			t.Errorf("faltou %q em %q", esperado, a)
		}
	}
}

func TestReferenciaEMajor(t *testing.T) {
	if Referencia("registry.interna:5000/postgres", 17) != "registry.interna:5000/postgres:17" {
		t.Fatal("Referencia")
	}
	if MajorDoCliente("18.6") != 18 || MajorDoCliente("16") != 16 {
		t.Fatal("MajorDoCliente")
	}
	if m := reVersaoCliente.FindStringSubmatch("pg_dump (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+1)"); m == nil || m[1] != "18.6" {
		t.Fatal(m)
	}
}

// Os testes abaixo usam o Docker local, se houver, e só a imagem postgres:18 já baixada.
func docker(t *testing.T) Docker {
	t.Helper()
	d := Docker{}
	if _, err := d.Versao(context.Background()); err != nil {
		t.Skip("sem Docker: ", err)
	}
	if !d.Existe(context.Background(), "postgres:18") {
		t.Skip("sem a imagem postgres:18 local")
	}
	return d
}

func TestDigestEVersaoCliente(t *testing.T) {
	d := docker(t)
	ctx := context.Background()
	dg, err := d.Digest(ctx, "postgres:18")
	if err != nil || !(strings.Contains(dg, "@sha256:") || strings.HasPrefix(dg, "sha256:")) {
		t.Fatal(dg, err)
	}
	v, err := d.VersaoCliente(ctx, dg)
	if err != nil || MajorDoCliente(v) != 18 {
		t.Fatal(v, err)
	}
}

func TestRodarCodigoELinhas(t *testing.T) {
	d := docker(t)
	var linhas []string
	cod, err := d.Rodar(context.Background(), Execucao{Imagem: "postgres:18", Nome: "copia-banco-teste-codigo",
		Comando: []string{"sh", "-c", "echo um; echo dois >&2; exit 3"}}, func(f, l string) { linhas = append(linhas, f+":"+l) })
	if err != nil || cod != 3 {
		t.Fatal(cod, err)
	}
	j := strings.Join(linhas, "|")
	if !strings.Contains(j, "saida:um") || !strings.Contains(j, "erro:dois") {
		t.Fatal(j)
	}
}

func TestRodarCanceladoParaOContainer(t *testing.T) {
	d := docker(t)
	ctx, cancel := context.WithCancel(context.Background())
	nome := "copia-banco-teste-cancelar"
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()
	_, err := d.Rodar(ctx, Execucao{Imagem: "postgres:18", Nome: nome, Rotulos: map[string]string{Rotulo: "teste"},
		Comando: []string{"sleep", "60"}}, nil)
	if err == nil {
		t.Fatal("cancelado deveria voltar erro")
	}
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+nome, "--format", "{{.Names}}").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("o container ficou para trás: %s", out)
	}
}
