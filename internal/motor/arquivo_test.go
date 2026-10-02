package motor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/9LEVEL/pghangar/internal/local"
)

// Os arquivos de testdata/arquivos são dumps de verdade, pequenos, do pg_dump 16 e 18: o banco loja,
// com o pg_trgm, uma tabela e uma função cujo corpo tem um CREATE ROLE.
const fixtures = "testdata/arquivos"

func TestLerArquivoReconheceOsFormatos(t *testing.T) {
	for _, c := range []struct {
		nome, formato, servidor string
		versao                  int
		banco                   string
	}{
		{"custom16.dump", ArqCustom, "16", 16, "loja"},
		{"tar18.tar", ArqTar, "18.6", 18, "loja"},
		{"dir18", ArqDiretorio, "18.6", 18, "loja"},
		{"plain18.sql", ArqSQL, "18.6", 18, ""},
		{"plain18.sql.gz", ArqSQLGz, "18.6", 18, ""},
	} {
		t.Run(c.nome, func(t *testing.T) {
			a, err := LerArquivo(filepath.Join(fixtures, c.nome))
			if err != nil {
				t.Fatal(err)
			}
			if a.Formato != c.formato || !strings.HasPrefix(a.Servidor, c.servidor) || a.Versao() != c.versao || a.Banco != c.banco {
				t.Fatalf("%+v", a)
			}
			if a.PgDump != a.Servidor || a.Tamanho <= 0 || a.Cluster {
				t.Fatalf("%+v", a)
			}
			if !a.SQL() && (a.Criado.IsZero() || a.Criado.Year() != 2026) {
				t.Fatalf("a data do dump vem do cabeçalho: %v", a.Criado)
			}
			if a.SQL() && a.Codificacao != "UTF8" {
				t.Fatalf("codificação: %q", a.Codificacao)
			}
			if a.Paralelo() != (c.formato == ArqCustom || c.formato == ArqDiretorio) {
				t.Fatal("só o custom e o diretório restauram em paralelo")
			}
		})
	}
	a, err := LerArquivo(filepath.Join(fixtures, "dumpall18.sql"))
	if err != nil || !a.Cluster || a.Formato != ArqSQL {
		t.Fatalf("o pg_dumpall é reconhecido: %+v %v", a, err)
	}
}

func TestLerArquivoRecusa(t *testing.T) {
	dir := t.TempDir()
	escrever := func(nome string, b []byte) string {
		c := filepath.Join(dir, nome)
		if err := os.WriteFile(c, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return c
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "outro.txt", Mode: 0o600, Size: 2})
	_, _ = tw.Write([]byte("oi"))
	_ = tw.Close()
	_ = os.Mkdir(filepath.Join(dir, "semtoc"), 0o700)
	for _, c := range []struct {
		caminho, erro string
	}{
		{escrever("vazio.sql", nil), "vazio"},
		{escrever("x.zst", []byte{0x28, 0xb5, 0x2f, 0xfd, 1, 2}), "zstd"},
		{escrever("x.xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0, 1}), "descomprima"},
		{escrever("x.bin", []byte{1, 0, 2, 0, 3}), "formato desconhecido"},
		{escrever("x.tar", tb.Bytes()), "toc.dat"},
		{escrever("x.dump", []byte("PGDMP\x01\x10\x00\x04")), "incompleto"},
		{filepath.Join(dir, "semtoc"), "toc.dat"},
	} {
		if _, err := LerArquivo(c.caminho); err == nil || !strings.Contains(err.Error(), c.erro) {
			t.Errorf("%s: %v (esperava %q)", filepath.Base(c.caminho), err, c.erro)
		}
	}
}

func varrer(t *testing.T, sql string) *varreduraSQL {
	t.Helper()
	v := &varreduraSQL{}
	for _, l := range strings.SplitAfter(sql, "\n") {
		if l != "" {
			v.ler(l)
		}
	}
	return v
}

func TestVarreduraDosDumpsDeVerdade(t *testing.T) {
	ler := func(nome string) *varreduraSQL {
		a, err := LerArquivo(filepath.Join(fixtures, nome))
		if err != nil {
			t.Fatal(err)
		}
		v, err := varrerSQL(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// O CREATE ROLE do corpo da função está dentro do $$: não conta.
	for _, n := range []string{"plain18.sql", "plain18.sql.gz"} {
		if v := ler(n); v.nProibidos != 0 || v.banco != "" || !reflect.DeepEqual(v.extensoes, []string{"pg_trgm"}) {
			t.Fatalf("%s: %v banco=%q ext=%v", n, v.proibidos, v.banco, v.extensoes)
		}
	}
	// O pg_dump -C: o banco loja é criado e conectado; esses comandos são pulados.
	if v := ler("plainc18.sql"); v.nProibidos != 0 || v.banco != "loja" || !v.criaBanco {
		t.Fatalf("-C: %v banco=%q", v.proibidos, v.banco)
	}
	// O pg_dumpall cria roles e passa por vários bancos.
	if v := ler("dumpall18.sql"); v.nProibidos == 0 || !strings.Contains(v.proibidos[0], "CREATE ROLE postgres") {
		t.Fatalf("pg_dumpall: %v", v.proibidos)
	}
}

func TestVarreduraSQL(t *testing.T) {
	for _, c := range []struct {
		nome, sql string
		proibida  string // um pedaço do primeiro motivo; vazio: nada recusado
	}{
		{"COPY com texto de comando nos dados", "COPY public.t (a) FROM stdin;\nCREATE ROLE x\n\\! ls\n\\.\nSELECT 1;\n", ""},
		{"string E'' com aspa escapada em várias linhas", "INSERT INTO t VALUES (E'a\\'\nCREATE ROLE x\n');\n", ""},
		{"comentário de bloco", "/* começo\nALTER SYSTEM SET x = 1;\n*/ SELECT 1;\n", ""},
		{"$tag$ com outra tag dentro", "CREATE FUNCTION f() AS $corpo$\n$$\nDROP ROLE x;\n$corpo$;\n", ""},
		{"user mapping", "CREATE USER MAPPING FOR app SERVER s;\nALTER USER MAPPING FOR app SERVER s OPTIONS (x 'y');\n", ""},
		{"restrict", "\\restrict abc\nSELECT 1;\n\\unrestrict abc\n", ""},
		{"$1 não abre um $$", "SELECT $1;\nCREATE ROLE x;\n", "roles"},
		{"create user", "CREATE USER app;\n", "roles"},
		{"alter system", "  ALTER SYSTEM SET work_mem = '1GB';\n", "configuração do servidor"},
		{"copy program", "COPY t FROM PROGRAM 'curl x';\n", "programa"},
		{"comando do psql", "\\set ON_ERROR_STOP 0\n", "\\set"},
		{"tablespace", "CREATE TABLESPACE t LOCATION '/x';\n", "tablespace"},
		{"reassign", "REASSIGN OWNED BY a TO b;\n", "fora do banco"},
		{"load", "LOAD 'x.so';\n", "biblioteca"},
		{"dois bancos", "CREATE DATABASE loja;\n\\connect loja\n\\connect outro\n", "outro"},
		{"grant em outro banco", "\\connect loja\nGRANT CONNECT ON DATABASE rh TO x;\n", "rh"},
	} {
		t.Run(c.nome, func(t *testing.T) {
			v := varrer(t, c.sql)
			switch {
			case c.proibida == "" && v.nProibidos > 0:
				t.Fatalf("recusou: %v", v.proibidos)
			case c.proibida != "" && (v.nProibidos == 0 || !strings.Contains(v.proibidos[0], c.proibida)):
				t.Fatalf("esperava recusar (%s): %v", c.proibida, v.proibidos)
			}
		})
	}
	// Os comandos do banco de um -C são do mesmo banco: o nome entre aspas e o do \connect batem.
	v := varrer(t, "DROP DATABASE IF EXISTS \"Loja\";\nCREATE DATABASE \"Loja\" WITH TEMPLATE = template0;\nALTER DATABASE \"Loja\" OWNER TO app;\n"+
		"\\connect -reuse-previous=on \"dbname='Loja'\"\nCOMMENT ON DATABASE \"Loja\" IS 'x';\nGRANT ALL ON DATABASE \"Loja\" TO app;\n")
	if v.nProibidos != 0 || v.banco != "Loja" {
		t.Fatalf("%v %q", v.proibidos, v.banco)
	}
}

// O filtro manda ao psql as mesmas linhas, com as do banco de um -C em branco (os números das linhas
// nos erros continuam os do arquivo).
func TestFiltroSQLPulaOsComandosDoBanco(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join(fixtures, "plainc18.sql"))
	if err != nil {
		t.Fatal(err)
	}
	saida, err := io.ReadAll(novoFiltroSQL(bytes.NewReader(orig)))
	if err != nil {
		t.Fatal(err)
	}
	lo, ls := strings.Split(string(orig), "\n"), strings.Split(string(saida), "\n")
	if len(lo) != len(ls) {
		t.Fatalf("o número de linhas mudou: %d → %d", len(lo), len(ls))
	}
	pulas := 0
	for i := range lo {
		if lo[i] == ls[i] {
			continue
		}
		if strings.TrimSpace(ls[i]) != "" {
			t.Fatalf("linha %d mudou: %q → %q", i+1, lo[i], ls[i])
		}
		pulas++
		if !strings.Contains(lo[i], "DATABASE loja") && lo[i] != `\connect loja` {
			t.Fatalf("linha %d pulada sem ser do banco: %q", i+1, lo[i])
		}
	}
	if pulas != 3 {
		t.Fatalf("esperava pular CREATE DATABASE, ALTER DATABASE e \\connect: %d", pulas)
	}
}

func TestFiltroSQLParaNaLinhaRecusada(t *testing.T) {
	f := novoFiltroSQL(strings.NewReader("SELECT 1;\nSELECT 2;\nALTER SYSTEM SET x = 1;\nSELECT 3;\n"))
	saida, err := io.ReadAll(f)
	var lr *linhaRecusada
	if !errors.As(err, &lr) || lr.linha != 3 || !strings.Contains(err.Error(), "linha 3") {
		t.Fatalf("%v", err)
	}
	if string(saida) != "SELECT 1;\nSELECT 2;\n" {
		t.Fatalf("o que vem depois da linha recusada não vai: %q", saida)
	}
	if !errors.As(f.erro(), &lr) {
		t.Fatal("o filtro guarda por que parou")
	}
	// Sem \n no fim, a última linha também vai.
	if b, err := io.ReadAll(novoFiltroSQL(strings.NewReader("SELECT 1;"))); err != nil || string(b) != "SELECT 1;" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestPastaDeEntrada(t *testing.T) {
	dir := local.Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := dir.Preparar(); err != nil {
		t.Fatal(err)
	}
	copiar := func(nome string, quando time.Time) string {
		b, err := os.ReadFile(filepath.Join(fixtures, nome))
		if err != nil {
			t.Fatal(err)
		}
		c := filepath.Join(dir.Entrada(), nome)
		if err := os.WriteFile(c, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(c, quando, quando)
		return c
	}
	agora := time.Now()
	velho := copiar("custom16.dump", agora.Add(-time.Hour))
	novo := copiar("plain18.sql", agora)
	_ = os.WriteFile(filepath.Join(dir.Entrada(), ".subindo.part"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir.Entrada(), "lixo.bin"), []byte{0, 1}, 0o600)
	_ = os.Chtimes(filepath.Join(dir.Entrada(), "lixo.bin"), agora.Add(-2*time.Hour), agora.Add(-2*time.Hour))
	is := ListarEntrada(dir)
	if len(is) != 3 || is[0].Arquivo.Caminho != novo || is[1].Arquivo.Caminho != velho || is[2].Erro == "" {
		t.Fatalf("%+v", is)
	}
	// Apagar: só o que está direto na pasta de entrada.
	if err := ApagarArquivo(dir, filepath.Join(fixtures, "plain18.sql")); err == nil {
		t.Fatal("fora da pasta de entrada não se apaga")
	}
	if err := ApagarArquivo(dir, filepath.Join(dir.Entrada(), "..", "estado.db")); err == nil {
		t.Fatal("o caminho com .. não sai da pasta de entrada")
	}
	_ = os.Mkdir(filepath.Join(dir.Entrada(), "pasta"), 0o700)
	if err := ApagarArquivo(dir, filepath.Join(dir.Entrada(), "pasta")); err == nil {
		t.Fatal("um diretório que não é dump não se apaga")
	}
	alvo := filepath.Join(t.TempDir(), "alvo.sql")
	_ = os.WriteFile(alvo, []byte("SELECT 1;"), 0o600)
	link := filepath.Join(dir.Entrada(), "link.sql")
	if err := os.Symlink(alvo, link); err != nil {
		t.Fatal(err)
	}
	if err := ApagarArquivo(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alvo); err != nil {
		t.Fatal("apagar o link não apaga o arquivo para onde ele aponta")
	}
	if err := ApagarArquivo(dir, novo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(novo); !os.IsNotExist(err) {
		t.Fatal("o arquivo deveria ter saído")
	}
}

func TestMesmoArquivo(t *testing.T) {
	a := &Arquivo{Formato: ArqCustom, Tamanho: 10, Modificado: time.Unix(100, 0)}
	b := *a
	if err := mesmoArquivo(a, &b); err != nil {
		t.Fatal(err)
	}
	b.Tamanho = 11
	if err := mesmoArquivo(a, &b); err == nil {
		t.Fatal("outro tamanho é outro arquivo")
	}
	if _, err := pedidoDoPlano(Plano{}); err == nil {
		t.Fatal("um plano sem o arquivo não vira pedido")
	}
}

// Os ataques e os falsos positivos que a revisão adversarial achou na conferência por linha.
func TestVarreduraPorComando(t *testing.T) {
	for _, c := range []struct {
		nome, sql string
		proibida  string
	}{
		{"segundo comando na mesma linha", "SELECT 1; CREATE ROLE x SUPERUSER;\n", "roles"},
		{"comando do psql depois de um ;", "SELECT 1; \\c postgres\nCREATE TABLE x ();\n", "psql"},
		{"comando do psql no meio de um comando", "SELECT 1 \\gexec\n", "psql"},
		{"BOM antes de um comando", "\ufeffCREATE ROLE x;\n", "roles"},
		{"cauda depois do \\unrestrict", "\\unrestrict abc \\! cat /run/pghangar/pgpass\n", "psql"},
		{"comando em duas linhas", "CREATE\nROLE x;\n", "roles"},
		{"ALTER SYSTEM em minúsculas e com comentário", "/* x */ alter   system set work_mem = '1GB';\n", "configuração"},
		{"coluna chamada load", "CREATE TABLE public.metricas (\n    id integer,\n    load numeric\n);\n", ""},
		{"COPY em duas linhas não deixa os dados serem lidos como SQL", "COPY t (a)\nFROM stdin;\nCREATE ROLE x\n\\N\n\\.\nSELECT 1;\n", ""},
		{"comando depois de um COPY em duas linhas é conferido", "COPY t (a)\nFROM stdin;\n1\n\\.\nCREATE ROLE x;\n", "roles"},
		{"COPY com muitas colunas", "COPY public.t (" + strings.Repeat("coluna_comprida, ", 60) + "fim) FROM stdin;\n\\N\tx'y\n\\.\nSELECT 1;\n", ""},
		{"COPY TO PROGRAM em duas linhas", "COPY t\nTO PROGRAM 'curl x';\n", "programa"},
		{"INSERT enorme sem espaços", "INSERT INTO t VALUES ('" + strings.Repeat("ab", 3<<20) + "');\n", ""},
		{"\\connect com ponto e vírgula", "\\connect loja;\n", "psql"},
	} {
		t.Run(c.nome, func(t *testing.T) {
			v := varrer(t, c.sql)
			switch {
			case c.proibida == "" && v.nProibidos > 0:
				t.Fatalf("recusou: %v", v.proibidos)
			case c.proibida != "" && (v.nProibidos == 0 || !strings.Contains(v.proibidos[0], c.proibida)):
				t.Fatalf("esperava recusar (%s): %v", c.proibida, v.proibidos)
			}
		})
	}
}

// Um comando do banco pulado sai inteiro, mesmo em várias linhas; o que vem depois dele na mesma
// linha continua.
func TestFiltroPulaComandoInteiro(t *testing.T) {
	ler := func(sql string) string {
		b, err := io.ReadAll(novoFiltroSQL(strings.NewReader(sql)))
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		return string(b)
	}
	if s := ler("CREATE DATABASE loja;\nCOMMENT ON DATABASE loja IS 'linha1\nlinha2';\nSELECT 1;\n"); strings.Contains(s, "linha2") ||
		strings.Count(s, "\n") != 4 || !strings.HasSuffix(s, "SELECT 1;\n") {
		t.Fatalf("o comentário em duas linhas sai inteiro: %q", s)
	}
	if s := ler("DROP DATABASE IF EXISTS loja; SELECT 2;\n"); strings.Contains(s, "DROP") || !strings.HasSuffix(s, " SELECT 2;\n") {
		t.Fatalf("o resto da linha continua: %q", s)
	}
	if s := ler("CREATE SUBSCRIPTION s CONNECTION 'host=prod password=x' PUBLICATION p;\nALTER SUBSCRIPTION s OWNER TO app;\nSELECT 3;\n"); strings.Contains(s, "password") || !strings.HasSuffix(s, "SELECT 3;\n") {
		t.Fatalf("a subscription sai: %q", s)
	}
	if s := ler("\ufeff\\connect loja\nSELECT 4;\n"); strings.Contains(s, "connect") || strings.Contains(s, "\ufeff") || !strings.HasSuffix(s, "SELECT 4;\n") {
		t.Fatalf("o BOM sai e o \\connect é pulado: %q", s)
	}
	// O plano vê o banco do -C, o servidor externo dos user mappings e as subscriptions.
	v := varrer(t, "CREATE DATABASE loja;\n\\connect loja\nCREATE USER MAPPING FOR app SERVER \"Prod Srv\" OPTIONS (\n    password 'x'\n);\nCREATE PUBLICATION p;\n")
	if v.banco != "loja" || !v.criaBanco || !reflect.DeepEqual(v.externos, []string{"Prod Srv"}) || v.subPub != 1 || v.nProibidos != 0 {
		t.Fatalf("%+v", v)
	}
}

// Um SQL cortado (um scp interrompido, o disco cheio) não troca: o plano bloqueia, e o filtro para.
func TestSQLCortado(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join(fixtures, "plain18.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, c := range []struct {
		nome   string
		corte  int
		motivo string
	}{
		{"no meio do COPY", bytes.Index(orig, []byte("\\.\n")), "COPY"},
		{"sem a linha final", bytes.Index(orig, []byte("-- PostgreSQL database dump complete")), "linha final"},
	} {
		caminho := filepath.Join(dir, "cortado.sql")
		if err := os.WriteFile(caminho, orig[:c.corte], 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := LerArquivo(caminho)
		if err != nil {
			t.Fatal(err)
		}
		v, err := varrerSQL(context.Background(), a)
		if err != nil || !strings.Contains(v.cortado, c.motivo) {
			t.Errorf("%s: o plano vê o corte: %q %v", c.nome, v.cortado, err)
		}
		_, err = io.ReadAll(novoFiltroSQL(bytes.NewReader(orig[:c.corte])))
		if err == nil || !strings.Contains(err.Error(), "incompleto") {
			t.Errorf("%s: o filtro para no fim: %v", c.nome, err)
		}
	}
	// O arquivo inteiro passa.
	if _, err := io.ReadAll(novoFiltroSQL(bytes.NewReader(orig))); err != nil {
		t.Fatal(err)
	}
}

// Um gzip que não tem SQL dentro (um .tar.gz de um dump -Fd, um custom comprimido de novo) é
// recusado na leitura, e não vai ao psql como texto.
func TestGzipQueNaoESQL(t *testing.T) {
	dir := t.TempDir()
	comprimir := func(nome string, b []byte) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(b)
		_ = gz.Close()
		c := filepath.Join(dir, nome)
		_ = os.WriteFile(c, buf.Bytes(), 0o600)
		return c
	}
	custom, _ := os.ReadFile(filepath.Join(fixtures, "custom16.dump"))
	tarb, _ := os.ReadFile(filepath.Join(fixtures, "tar18.tar"))
	for _, c := range []struct{ caminho, erro string }{
		{comprimir("x.dump.gz", custom), "custom"},
		{comprimir("x.tar.gz", tarb), ".tar.gz"},
		{comprimir("x.bin.gz", []byte{1, 0, 2}), "desconhecido"},
	} {
		if _, err := LerArquivo(c.caminho); err == nil || !strings.Contains(err.Error(), c.erro) {
			t.Errorf("%s: %v", filepath.Base(c.caminho), err)
		}
	}
}

// A identidade do arquivo: reescrever com o mesmo tamanho e repor a data muda o ctime.
func TestMesmoArquivoPeloCtime(t *testing.T) {
	c := filepath.Join(t.TempDir(), "x.sql")
	_ = os.WriteFile(c, []byte("SELECT 1;\n"), 0o600)
	a, err := LerArquivo(c)
	if err != nil || a.Inode == 0 || a.Mudanca.IsZero() {
		t.Fatalf("%+v %v", a, err)
	}
	time.Sleep(20 * time.Millisecond)
	_ = os.WriteFile(c, []byte("SELECT 2;\n"), 0o600)
	_ = os.Chtimes(c, a.Modificado, a.Modificado)
	b, _ := LerArquivo(c)
	if b.Tamanho != a.Tamanho || !b.Modificado.Equal(a.Modificado) {
		t.Fatal("o teste precisa do mesmo tamanho e da mesma data")
	}
	if err := mesmoArquivo(&a, &b); err == nil {
		t.Fatal("o conteúdo trocado com a data reposta é outro arquivo")
	}
}
