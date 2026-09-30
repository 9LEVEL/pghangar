package cadastro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func abrir(t *testing.T) *Cadastro {
	t.Helper()
	c, err := Abrir(filepath.Join(t.TempDir(), "estado.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Fechar() })
	return c
}

func conexao(nome, tag string) Conexao {
	return Conexao{Nome: nome, Tag: tag, Acesso: AcessoDireto, Host: "10.0.0.1", Porta: 5432, Usuario: "postgres",
		ModoSenha: SenhaGuardar, Senha: "s3", SSLMode: "prefer", BancoAdmin: "postgres"}
}

func perfil(nome, origem, destino string) Perfil {
	return Perfil{Nome: nome, Origem: origem, OrigemBanco: "loja", Destino: destino, DestinoBanco: "loja", JobsDump: 2, JobsRestore: 4}
}

func TestArquivoNasceCom600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "estado.db")
	c, err := Abrir(p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Fechar()
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi.Mode().Perm(), err)
	}
}

func TestConexaoSenhaVaziaMantemAGravada(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	if err := c.SalvarConexao(ctx, "", conexao("prod", TagProd)); err != nil {
		t.Fatal(err)
	}
	x := conexao("prod", TagProd)
	x.Senha = ""
	x.Porta = 5433
	if err := c.SalvarConexao(ctx, "prod", x); err != nil {
		t.Fatal(err)
	}
	y, err := c.Conexao(ctx, "prod")
	if err != nil || y.Senha != "s3" || y.Porta != 5433 {
		t.Fatalf("%+v %v", y, err)
	}
	// Outro modo não guarda senha.
	y.ModoSenha = SenhaPerguntar
	if err := c.SalvarConexao(ctx, "prod", y); err != nil {
		t.Fatal(err)
	}
	if z, _ := c.Conexao(ctx, "prod"); z.Senha != "" {
		t.Fatal("o modo perguntar não pode guardar senha")
	}
}

func TestProdNuncaEDestino(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	for _, x := range []Conexao{conexao("prod", TagProd), conexao("dev", TagDev)} {
		if err := c.SalvarConexao(ctx, "", x); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SalvarPerfil(ctx, "", perfil("errado", "dev", "prod")); err == nil {
		t.Fatal("perfil com destino prod deveria ser recusado")
	}
	if err := c.SalvarPerfil(ctx, "", perfil("certo", "prod", "dev")); err != nil {
		t.Fatal(err)
	}
	// Uma conexão que é destino não pode virar prod.
	if err := c.SalvarConexao(ctx, "dev", conexao("dev", TagProd)); err == nil {
		t.Fatal("destino de perfil virando prod deveria ser recusado")
	}
	if x, _ := c.Conexao(ctx, "dev"); x.Tag != TagDev {
		t.Fatal("a recusa deveria desfazer a gravação")
	}
}

func TestRenomearConexaoLevaOsPerfis(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	_ = c.SalvarConexao(ctx, "", conexao("prod", TagProd))
	_ = c.SalvarConexao(ctx, "", conexao("dev", TagDev))
	if err := c.SalvarPerfil(ctx, "", perfil("p", "prod", "dev")); err != nil {
		t.Fatal(err)
	}
	if err := c.SalvarConexao(ctx, "prod", conexao("producao", TagProd)); err != nil {
		t.Fatal(err)
	}
	p, err := c.Perfil(ctx, "p")
	if err != nil || p.Origem != "producao" {
		t.Fatalf("%+v %v", p, err)
	}
	if err := c.RemoverConexao(ctx, "producao"); err == nil || !strings.Contains(err.Error(), "p") {
		t.Fatalf("conexão em uso deveria ser recusada: %v", err)
	}
	if err := c.SalvarConexao(ctx, "dev", conexao("producao", TagDev)); err == nil {
		t.Fatal("nome repetido deveria ser recusado")
	}
}

func TestValidarConexaoTunel(t *testing.T) {
	x := conexao("p", TagProd)
	x.Acesso = AcessoSSH
	if err := x.Validar(); err == nil {
		t.Fatal("túnel sem host SSH")
	}
	x.SSHHost, x.SSHPorta, x.SSHUsuario = "prod.exemplo", 22, "pghangar"
	if err := x.Validar(); err != nil {
		t.Fatal(err)
	}
	// O TLS pelo túnel é do próprio túnel, que confere o certificado contra o host real do banco.
	x.SSLMode = "verify-full"
	if err := x.Validar(); err != nil {
		t.Fatalf("verify-full pelo túnel: %v", err)
	}
	x.SSLMode, x.Host = "require", "/var/run/postgresql"
	if err := x.Validar(); err == nil {
		t.Fatal("socket pelo túnel deveria ser recusado")
	}
}

func TestPerfilCampos(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	_ = c.SalvarConexao(ctx, "", conexao("prod", TagProd))
	_ = c.SalvarConexao(ctx, "", conexao("dev", TagDev))
	p := perfil("p", "prod", "dev")
	p.SemDados = []string{" public.log* ", "", "auditoria.*"}
	p.Script = "/root/pos.sql"
	if err := c.SalvarPerfil(ctx, "", p); err != nil {
		t.Fatal(err)
	}
	q, _ := c.Perfil(ctx, "p")
	if strings.Join(q.SemDados, "|") != "public.log*|auditoria.*" || q.Script != "/root/pos.sql" {
		t.Fatalf("%+v", q)
	}
	p.Script = "pos.sql"
	if err := c.SalvarPerfil(ctx, "p", p); err == nil {
		t.Fatal("script relativo deveria ser recusado")
	}
	mesmo := perfil("m", "dev", "dev")
	if err := c.SalvarPerfil(ctx, "", mesmo); err == nil {
		t.Fatal("origem igual ao destino deveria ser recusada")
	}
	if err := c.RemoverPerfil(ctx, "nada"); !errors.Is(err, ErrNaoExiste) {
		t.Fatal(err)
	}
}

func TestInfoEImagensEConfig(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	_ = c.SalvarConexao(ctx, "", conexao("dev", TagDev))
	i := Info{VerificadaEm: time.Now().Round(time.Second), VersaoNum: 180006, Superusuario: true,
		Bancos: []Banco{{Nome: "loja", Tamanho: 42, Provedor: "c"}}}
	if err := c.GravarInfo(ctx, "dev", i); err != nil {
		t.Fatal(err)
	}
	x, _ := c.Conexao(ctx, "dev")
	if x.Info.VersaoNum != 180006 || len(x.Info.Bancos) != 1 || !x.Info.Pronta() {
		t.Fatalf("%+v", x.Info)
	}

	cfg, err := c.Config(ctx)
	if err != nil || cfg.Repositorio != "postgres" || len(cfg.Versoes) != 3 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := c.SalvarConfig(ctx, Config{Repositorio: "registry.interna:5000/postgres", Versoes: []int{17, 18}}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = c.Config(ctx)
	if cfg.Repositorio != "registry.interna:5000/postgres" || len(cfg.Versoes) != 2 {
		t.Fatalf("%+v", cfg)
	}
	for _, ruim := range []string{"", "postgres:18", "postgres@sha256:x", "a b"} {
		if err := c.SalvarConfig(ctx, Config{Repositorio: ruim, Versoes: []int{18}}); err == nil {
			t.Errorf("repositório %q deveria ser recusado", ruim)
		}
	}

	if err := c.SalvarImagem(ctx, Imagem{Versao: 18, Referencia: "postgres:18", Digest: "postgres@sha256:abc", Cliente: "18.6", BaixadaEm: time.Now()}); err != nil {
		t.Fatal(err)
	}
	is, _ := c.Imagens(ctx)
	if is[18].Cliente != "18.6" {
		t.Fatalf("%+v", is)
	}
}

func TestExecucoes(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	id, err := c.NovaExecucao(ctx, Execucao{Perfil: "p", Tipo: TipoCopia, Estado: EstadoIniciando, Destino: "dev", Banco: "loja", Apagar: []string{"loja__anterior_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Progresso(ctx, id, 3, 10, "public.pedidos"); err != nil {
		t.Fatal(err)
	}
	e, err := c.Execucao(ctx, id)
	if err != nil || e.Feito != 3 || e.Item != "public.pedidos" || len(e.Apagar) != 1 || e.Terminou() {
		t.Fatalf("%+v %v", e, err)
	}
	e.Estado, e.Fim, e.Avisos, e.Notas = EstadoOK, time.Now(), []string{"x"}, []string{"n1", "n2"}
	if err := c.GravarExecucao(ctx, e); err != nil {
		t.Fatal(err)
	}
	u, ok, err := c.UltimaDoPerfil(ctx, "p")
	if err != nil || !ok || u.Estado != EstadoOK || !u.Terminou() || len(u.Avisos) != 1 || len(u.Notas) != 2 {
		t.Fatalf("%+v %v %v", u, ok, err)
	}
	if _, ok, _ := c.UltimaDoPerfil(ctx, "outro"); ok {
		t.Fatal("perfil sem execução")
	}
}

func TestPerfilListasVaziasDoFormulario(t *testing.T) {
	ctx := context.Background()
	c := abrir(t)
	_ = c.SalvarConexao(ctx, "", conexao("prod", TagProd))
	_ = c.SalvarConexao(ctx, "", conexao("dev", TagDev))
	p := perfil("p", "prod", "dev")
	// O formulário manda strings.Split("", ",") = [""] para cada lista vazia.
	p.Schemas, p.SchemasFora, p.Tabelas, p.TabelasFora, p.SemDados = []string{""}, []string{" "}, []string{""}, []string{""}, []string{""}
	p.Retomavel = true
	if err := c.SalvarPerfil(ctx, "", p); err != nil {
		t.Fatal(err)
	}
	q, _ := c.Perfil(ctx, "p")
	if q.Filtrado() || len(q.Tabelas) != 0 || !q.Retomavel || q.Compressao != "zstd" {
		t.Fatalf("%+v", q)
	}
	q.Tabelas = []string{"public.x"}
	if err := c.SalvarPerfil(ctx, "p", q); err == nil {
		t.Fatal("link instável com lista de tabelas deveria ser recusado")
	}
}
