package motor

// A restauração de um arquivo de fora (docs/ESTRATEGIA.md §17): um dump que a ferramenta não fez
// (um backup, um arquivo que alguém mandou), posto na pasta de entrada e restaurado num banco de
// uma conexão dev ou homolog. Daí em diante é uma cópia como as outras: o __novo com a role
// temporária, os donos, o ANALYZE e a troca, com o anterior e o desfazer. O arquivo só é lido.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// Os formatos de um arquivo de fora, reconhecidos pelo conteúdo (e não pela extensão).
const (
	ArqCustom    = "custom"    // pg_dump -Fc
	ArqTar       = "tar"       // pg_dump -Ft
	ArqDiretorio = "diretório" // pg_dump -Fd
	ArqSQL       = "sql"       // pg_dump em texto: vai pelo psql
	ArqSQLGz     = "sql.gz"    // o mesmo, comprimido com gzip
)

// dentroArquivo é onde o arquivo aparece no container, só para leitura.
const dentroArquivo = "/run/pghangar/arquivo"

// Arquivo é um arquivo da pasta de entrada, como o cabeçalho dele o descreve.
type Arquivo struct {
	Caminho    string    `json:"caminho"`
	Formato    string    `json:"formato"`
	Tamanho    int64     `json:"tamanho"`
	Modificado time.Time `json:"modificado"`
	// O que o arquivo diz de si; vazio quando não diz (um SQL escrito à mão).
	Banco       string    `json:"banco,omitempty"`       // o banco de onde o dump veio
	Servidor    string    `json:"servidor,omitempty"`    // a versão do servidor de onde ele veio
	PgDump      string    `json:"pg_dump,omitempty"`     // a versão do pg_dump que o gerou
	Criado      time.Time `json:"criado,omitempty"`      // quando o dump foi feito
	Codificacao string    `json:"codificacao,omitempty"` // o client_encoding de um SQL
	Cluster     bool      `json:"cluster,omitempty"`     // um pg_dumpall: o servidor inteiro
	// A identidade dele, que a execução confere (no diretório, a do toc.dat).
	Dispositivo uint64    `json:"dispositivo,omitempty"`
	Inode       uint64    `json:"inode,omitempty"`
	Mudanca     time.Time `json:"mudanca,omitempty"` // o ctime
}

// identidade guarda o dispositivo, o inode e o ctime.
func (a *Arquivo) identidade(fi os.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		a.Dispositivo, a.Inode, a.Mudanca = uint64(st.Dev), st.Ino, time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	}
}

// SQL diz se o arquivo vai pelo psql.
func (a Arquivo) SQL() bool { return a.Formato == ArqSQL || a.Formato == ArqSQLGz }

// Paralelo diz se o formato restaura com vários jobs: o tar e o SQL vão num só.
func (a Arquivo) Paralelo() bool { return a.Formato == ArqCustom || a.Formato == ArqDiretorio }

// Versao é a maior versão principal que o arquivo cita (a do servidor ou a do pg_dump), ou 0.
func (a Arquivo) Versao() int { return max(majorDe(a.Servidor), majorDe(a.PgDump)) }

// Origem descreve de onde o dump veio, com o que o arquivo diz: "do banco loja, PostgreSQL 17.2,
// pelo pg_dump 17.2, em 01/10/2026 03:00".
func (a Arquivo) Origem() string {
	var ps []string
	if a.Cluster {
		ps = append(ps, "pg_dumpall (o servidor inteiro)")
	}
	if a.Banco != "" {
		ps = append(ps, "do banco "+a.Banco)
	}
	if a.Servidor != "" {
		ps = append(ps, "PostgreSQL "+a.Servidor)
	}
	if a.PgDump != "" {
		ps = append(ps, "pelo pg_dump "+a.PgDump)
	}
	if !a.Criado.IsZero() {
		ps = append(ps, "em "+a.Criado.Format("02/01/2006 15:04"))
	}
	if len(ps) == 0 {
		return "o arquivo não diz de onde veio"
	}
	return strings.Join(ps, ", ")
}

// majorDe tira a versão principal de "17.2 (Debian 17.2-1)" (17) ou de "9.6.24" (9).
func majorDe(v string) int {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(v[:i])
	return n
}

// LerArquivo reconhece o formato do arquivo e lê o cabeçalho dele. Um dump em diretório (-Fd) é um
// diretório com o toc.dat.
func LerArquivo(caminho string) (Arquivo, error) {
	a := Arquivo{Caminho: caminho}
	fi, err := os.Stat(caminho)
	if err != nil {
		return a, err
	}
	a.Tamanho, a.Modificado = fi.Size(), fi.ModTime()
	if fi.IsDir() {
		a.Formato, a.Tamanho = ArqDiretorio, TamanhoDir(caminho)
		f, err := os.Open(filepath.Join(caminho, "toc.dat"))
		if err != nil {
			return a, errors.New("um diretório sem o toc.dat não é um dump do pg_dump em diretório (-Fd)")
		}
		defer f.Close()
		// O toc.dat é o último a ser escrito: a data dele é a do dump pronto.
		if tf, err := f.Stat(); err == nil {
			a.identidade(tf)
			if tf.ModTime().After(a.Modificado) {
				a.Modificado = tf.ModTime()
			}
		}
		return a, lerCabecalho(f, &a, 3)
	}
	a.identidade(fi)
	if a.Tamanho == 0 {
		return a, errors.New("o arquivo está vazio")
	}
	f, err := os.Open(caminho)
	if err != nil {
		return a, err
	}
	defer f.Close()
	ini := make([]byte, 512)
	n, _ := io.ReadFull(f, ini)
	ini = ini[:n]
	switch {
	case bytes.HasPrefix(ini, []byte("PGDMP")):
		a.Formato = ArqCustom
		return a, lerCabecalho(io.MultiReader(bytes.NewReader(ini), f), &a, 1)
	case n == 512 && string(ini[257:262]) == "ustar":
		a.Formato = ArqTar
		// O pg_dump -Ft põe o toc.dat primeiro; o arquivo seguinte do tar começa aqui.
		if nome := string(bytes.TrimRight(ini[:100], "\x00")); nome != "toc.dat" {
			return a, fmt.Errorf("um .tar que não é do pg_dump -Ft (o primeiro arquivo dele é %q, e não o toc.dat)", nome)
		}
		return a, lerCabecalho(f, &a, 3)
	case bytes.HasPrefix(ini, []byte{0x1f, 0x8b}):
		a.Formato = ArqSQLGz
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return a, err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return a, fmt.Errorf("o gzip não abre: %w", err)
		}
		defer gz.Close()
		// Dentro do gzip pode estar outra coisa que não SQL: um tar de um dump -Fd, um custom.
		dentro := make([]byte, 512)
		m, _ := io.ReadFull(gz, dentro)
		dentro = dentro[:m]
		switch {
		case bytes.HasPrefix(dentro, []byte("PGDMP")):
			return a, errors.New("um dump custom comprimido de novo com gzip: descomprima antes (gunzip), e ele restaura em paralelo")
		case m == 512 && string(dentro[257:262]) == "ustar":
			return a, errors.New("um .tar.gz: extraia antes (um dump -Fd vira o diretório; um -Ft, o .tar)")
		case bytes.Contains(dentro, []byte{0}):
			return a, errors.New("um gzip de um formato desconhecido: o pghangar lê do .gz só SQL puro")
		}
		return a, lerCabecalhoSQL(io.MultiReader(bytes.NewReader(dentro), gz), &a)
	case bytes.HasPrefix(ini, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return a, errors.New("comprimido com zstd: descomprima antes (zstd -d) ou gere com pg_dump -Fc, que restaura em paralelo")
	case bytes.HasPrefix(ini, []byte("BZh")), bytes.HasPrefix(ini, []byte{0xfd, '7', 'z', 'X', 'Z', 0}), bytes.HasPrefix(ini, []byte("PK\x03\x04")):
		return a, errors.New("comprimido com bzip2, xz ou zip: descomprima antes. Do SQL puro, o pghangar lê direto só o .gz")
	case !bytes.Contains(ini, []byte{0}):
		a.Formato = ArqSQL
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return a, err
		}
		return a, lerCabecalhoSQL(f, &a)
	}
	return a, errors.New("formato desconhecido: o pghangar restaura os dumps do pg_dump (custom, tar e diretório) e SQL puro, com ou sem gzip")
}

// lerCabecalho lê o cabeçalho do pg_dump (o de pg_backup_archiver.c, ReadHead): a versão do
// formato, os tamanhos, o formato, a compressão, a data, o banco e as versões do servidor e do
// pg_dump. O formato gravado é 1 no custom e 3 no tar e no diretório (que grava "tar" no toc.dat).
func lerCabecalho(r io.Reader, a *Arquivo, formatoEsperado int) error {
	l := &leitorArq{r: bufio.NewReader(r)}
	mag := make([]byte, 5)
	if _, err := io.ReadFull(l.r, mag); err != nil || string(mag) != "PGDMP" {
		return errors.New("sem a marca PGDMP no começo: não é um dump do pg_dump")
	}
	vmaj, vmin, vrev := l.byte(), l.byte(), 0
	if vmaj > 1 || (vmaj == 1 && vmin > 0) {
		vrev = l.byte()
	}
	versao := (vmaj*256+vmin)*256 + vrev
	if versao < (1*256+10)*256 {
		return fmt.Errorf("o formato %d.%d é de um pg_dump antigo demais (de antes do PostgreSQL 8.4)", vmaj, vmin)
	}
	l.tamInt = l.byte()
	if l.tamInt < 1 || l.tamInt > 8 {
		return errors.New("o cabeçalho do dump está ilegível (tamanho de inteiro inválido)")
	}
	l.byte() // o tamanho dos offsets
	if f := l.byte(); f != formatoEsperado && l.err == nil {
		return fmt.Errorf("o cabeçalho diz outro formato (%d) para um arquivo %s", f, a.Formato)
	}
	if versao >= (1*256+15)*256 {
		l.byte() // o algoritmo de compressão
	} else {
		l.int() // o nível de compressão
	}
	seg, mi, h, dia, mes, ano := l.int(), l.int(), l.int(), l.int(), l.int(), l.int()
	l.int() // isdst
	a.Banco, a.Servidor, a.PgDump = l.str(), primeiraPalavra(l.str()), primeiraPalavra(l.str())
	if l.err != nil {
		return fmt.Errorf("o cabeçalho do dump está incompleto: %w", l.err)
	}
	if ano > 0 && mes >= 0 && mes < 12 {
		a.Criado = time.Date(ano+1900, time.Month(mes+1), dia, h, mi, seg, 0, time.Local)
	}
	return nil
}

// leitorArq lê os inteiros e as strings do formato do pg_dump: um byte de sinal e os bytes do
// número, do menos ao mais significativo; uma string é o tamanho (-1 é nula) e os bytes.
type leitorArq struct {
	r      *bufio.Reader
	tamInt int
	err    error
}

func (l *leitorArq) byte() int {
	b, err := l.r.ReadByte()
	if err != nil && l.err == nil {
		l.err = err
	}
	return int(b)
}

func (l *leitorArq) int() int {
	sinal := l.byte()
	v := 0
	for i := 0; i < l.tamInt; i++ {
		v |= l.byte() << (8 * i)
	}
	if sinal != 0 {
		v = -v
	}
	return v
}

func (l *leitorArq) str() string {
	n := l.int()
	if n < 0 || l.err != nil {
		return ""
	}
	if n > 1<<16 {
		l.err = errors.New("texto longo demais no cabeçalho")
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(l.r, b); err != nil && l.err == nil {
		l.err = err
	}
	return string(b)
}

func primeiraPalavra(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// lerCabecalhoSQL lê o começo de um SQL do pg_dump: as versões, a codificação e se é um pg_dumpall.
func lerCabecalhoSQL(r io.Reader, a *Arquivo) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	for i := 0; i < 300 && s.Scan(); i++ {
		t := s.Text()
		switch {
		case strings.HasPrefix(t, "-- PostgreSQL database cluster dump"):
			a.Cluster = true
		case strings.HasPrefix(t, "-- Dumped from database version "):
			a.Servidor = primeiraPalavra(strings.TrimPrefix(t, "-- Dumped from database version "))
		case strings.HasPrefix(t, "-- Dumped by pg_dump version "):
			a.PgDump = primeiraPalavra(strings.TrimPrefix(t, "-- Dumped by pg_dump version "))
		case a.Codificacao == "" && strings.HasPrefix(t, "SET client_encoding = '"):
			a.Codificacao = strings.TrimSuffix(strings.TrimPrefix(t, "SET client_encoding = '"), "';")
		}
	}
	// Um gzip que não abre direito, ou uma linha enorme logo no começo, aparece aqui; o fim do
	// arquivo não é erro.
	if err := s.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("lendo o começo do arquivo: %w", err)
	}
	return nil
}

// --- a pasta de entrada ---------------------------------------------------------------------------

// ItemEntrada é um arquivo (ou um dump em diretório) da pasta de entrada.
type ItemEntrada struct {
	Arquivo Arquivo
	Erro    string // por que ele não pode ser restaurado
}

// ListarEntrada lê a pasta de entrada, do mais novo ao mais velho. Os ocultos (um upload em
// andamento, como os .part) ficam de fora.
func ListarEntrada(dir local.Dir) []ItemEntrada {
	es, err := os.ReadDir(dir.Entrada())
	if err != nil {
		return nil
	}
	var is []ItemEntrada
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a, err := LerArquivo(filepath.Join(dir.Entrada(), e.Name()))
		it := ItemEntrada{Arquivo: a}
		switch {
		case err != nil:
			it.Erro = err.Error()
		case a.Cluster:
			it.Erro = "um pg_dumpall (o servidor inteiro): restaure um banco só, gerado com pg_dump"
		}
		is = append(is, it)
	}
	sort.SliceStable(is, func(i, j int) bool { return is[i].Arquivo.Modificado.After(is[j].Arquivo.Modificado) })
	return is
}

// ApagarArquivo apaga um item da pasta de entrada: só dela, e só o que está direto nela. Um link sai
// sozinho (o arquivo para onde ele aponta fica), e um diretório só se for um dump do pg_dump.
func ApagarArquivo(dir local.Dir, caminho string) error {
	if !naEntrada(dir, caminho) {
		return fmt.Errorf("recusado: %s não está na pasta de entrada (%s)", caminho, dir.Entrada())
	}
	fi, err := os.Lstat(caminho)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if _, err := os.Stat(filepath.Join(caminho, "toc.dat")); err != nil {
			return fmt.Errorf("recusado: o diretório %s não é um dump do pg_dump (sem o toc.dat)", caminho)
		}
		return os.RemoveAll(caminho)
	}
	return os.Remove(caminho)
}

// naEntrada diz se o caminho é um item direto da pasta de entrada.
func naEntrada(dir local.Dir, caminho string) bool {
	c, err1 := filepath.Abs(caminho)
	e, err2 := filepath.Abs(dir.Entrada())
	return err1 == nil && err2 == nil && filepath.Dir(c) == e && filepath.Base(c) != "." && filepath.Base(c) != ".."
}

// --- o SQL puro (a conferência está em arquivo_sql.go) ------------------------------------------

// abrirArquivoSQL abre o SQL de um arquivo (descomprimido, se for .gz). conta é o quanto do arquivo em disco
// já foi lido, para o andamento.
func abrirArquivoSQL(a Arquivo) (r io.Reader, conta *contador, fechar func(), err error) {
	f, err := os.Open(a.Caminho)
	if err != nil {
		return nil, nil, nil, err
	}
	conta = &contador{r: f}
	if a.Formato != ArqSQLGz {
		return conta, conta, func() { _ = f.Close() }, nil
	}
	gz, err := gzip.NewReader(conta)
	if err != nil {
		_ = f.Close()
		return nil, nil, nil, fmt.Errorf("o gzip não abre: %w", err)
	}
	return gz, conta, func() { _ = gz.Close(); _ = f.Close() }, nil
}

// contador conta os bytes lidos (o andamento de um SQL é o quanto do arquivo já foi lido).
type contador struct {
	r io.Reader
	n atomic.Int64
}

func (c *contador) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// --- o plano ---------------------------------------------------------------------------------------

// PedidoArquivo é a restauração de um arquivo como o sysadmin a pede: o arquivo, a conexão e o banco
// de destino, e os jobs do restore e do ANALYZE.
type PedidoArquivo struct {
	Caminho string
	Conexao string
	Banco   string
	Jobs    int
	// confirmado é o plano que a execução refaz: o arquivo não é lido de novo antes do restore (o
	// filtro do restore recusa as mesmas linhas, e a identidade do arquivo é conferida), e o que a
	// leitura achou vem dele.
	confirmado *Plano
}

var (
	// As linhas do índice do pg_restore --list: "12; 3079 16385 EXTENSION - vector" e "3456; 0 0 USER
	// MAPPING - USER MAPPING app SERVER prodsrv postgres".
	reTocExtensao   = regexp.MustCompile(`^\d+;\s+\d+\s+\d+\s+EXTENSION\s+-\s+(\S+)`)
	reTocMapeamento = regexp.MustCompile(`^\d+;\s+\d+\s+\d+\s+USER MAPPING\s+-\s+USER MAPPING\s+.*\sSERVER\s+(\S+)`)
)

// PlanejarArquivo planeja restaurar um arquivo da pasta de entrada num banco de uma conexão dev ou
// homolog. As guardas do destino são as de uma cópia; a origem é o arquivo.
func PlanejarArquivo(ctx context.Context, d Deps, pd PedidoArquivo) (Plano, error) {
	p := Plano{CriadoEm: time.Now()}
	if !naEntrada(d.Dir, pd.Caminho) {
		return p, fmt.Errorf("o arquivo precisa estar na pasta de entrada (%s): mova-o para lá", d.Dir.Entrada())
	}
	caminho, _ := filepath.Abs(pd.Caminho)
	if strings.HasPrefix(filepath.Base(caminho), ".") {
		return p, fmt.Errorf("%s é oculto (um upload em andamento?): renomeie-o sem o ponto quando ele estiver completo", filepath.Base(caminho))
	}
	a, err := LerArquivo(caminho)
	if err != nil {
		return p, fmt.Errorf("%s: %w", filepath.Base(caminho), err)
	}
	if pd.Banco == "" {
		return p, errors.New("informe o banco de destino")
	}
	if pd.Jobs < 1 || pd.Jobs > 32 {
		return p, fmt.Errorf("jobs: de 1 a 32 (veio %d)", pd.Jobs)
	}
	p.Arquivo = &a
	p.Perfil = cadastro.Perfil{Nome: filepath.Base(caminho), Destino: pd.Conexao, DestinoBanco: pd.Banco, JobsRestore: pd.Jobs}
	p.Novo = nomes.Novo(pd.Banco)
	switch {
	case len(pd.Banco) > nomes.Limite:
		p.bloquear("o nome %s passa de %d bytes, o limite do PostgreSQL", pd.Banco, nomes.Limite)
		return p, nil
	case nomes.PareceDaFerramenta(pd.Banco):
		p.bloquear("o nome %s tem a forma de um banco da ferramenta (__novo, __anterior, __base): escolha outro", pd.Banco)
		return p, nil
	}

	// O SQL é lido antes de abrir o destino: a leitura de um arquivo grande leva tempo, e a conexão
	// com o destino não fica parada esperando.
	var exts []string
	if c := pd.confirmado; c != nil {
		for _, e := range c.Extensoes {
			exts = append(exts, e.Nome)
		}
		p.Externos = c.Externos
	} else if a.SQL() {
		v, err := varrerSQL(ctx, a)
		if err != nil {
			return p, fmt.Errorf("lendo %s: %w", filepath.Base(caminho), err)
		}
		for _, x := range v.proibidos {
			p.bloquear("%s", x)
		}
		if v.nProibidos > len(v.proibidos) {
			p.bloquear("e mais %d comando(s) recusado(s): o restore de um SQL só aceita o que fica dentro do banco", v.nProibidos-len(v.proibidos))
		}
		if v.cortado != "" {
			p.bloquear("o arquivo parece incompleto: %s. Gere ou copie de novo", v.cortado)
		}
		switch {
		case v.nProibidos > 0 || v.banco == "":
		case v.criaBanco:
			p.notar("o arquivo cria o banco %s e se conecta a ele (pg_dump -C): esses comandos, e os de configuração do banco (ALTER DATABASE … SET, GRANT … ON DATABASE), ficam de fora. O restore vai para o __novo de %s", v.banco, pd.Banco)
		default:
			p.notar("o arquivo se conecta ao banco %s (\\connect): esse comando fica de fora, e o restore vai para o __novo de %s", v.banco, pd.Banco)
		}
		if v.subPub > 0 {
			p.notar("as subscriptions e as publications do arquivo (%d comando(s)) ficam de fora, como no --no-subscriptions e no --no-publications do pg_restore", v.subPub)
		}
		exts, p.Externos = v.extensoes, v.externos
	}

	destino, pare, err := abrirDestinoDoPlano(ctx, d, &p, pd.Conexao, pd.Banco)
	if err != nil || pare {
		return p, err
	}
	defer destino.fechar()
	infoD := destino.c.Info
	if _, err := d.Docker.Versao(ctx); err != nil {
		p.bloquear("%v", err)
	}

	// A versão: a do arquivo (um pg_restore mais antigo que o pg_dump não lê o arquivo dele, e
	// descer de versão é bloqueado), ou a do destino, se o arquivo não disser.
	fonte := a.Versao()
	if fonte == 0 {
		fonte = versoes.Major(infoD.VersaoNum)
		p.avisar("o arquivo não diz de que versão do PostgreSQL veio: o restore usa a do destino (%d). Se ele veio de uma versão mais nova, pode falhar", fonte)
	}
	if err := escolherImagem(ctx, d, &p, fonte, versoes.Major(infoD.VersaoNum)); err != nil {
		return p, err
	}
	if a.Cluster {
		p.bloquear("o arquivo é de um pg_dumpall (o servidor inteiro: roles e vários bancos): restaure um banco só, gerado com pg_dump")
	}

	// Os formatos do pg_restore: o índice diz as extensões e os user mappings.
	if !a.SQL() && pd.confirmado == nil {
		real, err := filepath.EvalSymlinks(a.Caminho)
		switch {
		case err != nil:
			p.bloquear("%s é um link quebrado: %v", filepath.Base(a.Caminho), err)
		case strings.ContainsAny(real, `:,"`):
			p.bloquear("o caminho de %s (%s) tem ':', ',' ou aspas, e o Docker não o monta: renomeie o arquivo", filepath.Base(a.Caminho), real)
		case p.ImagemRef != "":
			if exts, p.Externos, err = listarArquivo(ctx, d, &p, a); err != nil {
				p.bloquear("o pg_restore não leu o índice do arquivo: %v", err)
			}
		}
	}
	if len(exts) > 0 {
		tem, err := nomesDe(ctx, destino.admin, `SELECT name FROM pg_available_extensions WHERE name = ANY($1)`, exts)
		if err != nil {
			return p, err
		}
		disponivel := map[string]bool{}
		for _, x := range tem {
			disponivel[x] = true
		}
		for _, x := range exts {
			if !disponivel[x] {
				p.bloquear("a extensão %s (o arquivo a cria) não está disponível no destino: instale-a no servidor de destino", x)
			}
			p.Extensoes = append(p.Extensoes, Extensao{Nome: x})
		}
	}
	if len(p.Externos) > 0 {
		oferecerFDW(&p, p.Externos)
	}
	if a.SQL() && a.Codificacao != "" {
		cod := ""
		if bd, ok := banco(infoD, pd.Banco); ok {
			cod = bd.Codificacao
		} else if m, err := conexao.BancoModelo(ctx, destino.admin, infoD.VersaoNum); err == nil {
			cod = m.Codificacao
		}
		if cod != "" && !strings.EqualFold(cod, a.Codificacao) {
			p.avisar("o arquivo está em %s e o banco em %s: o psql converte, e um caractere sem par na codificação do banco vira erro", a.Codificacao, cod)
		}
	}

	// O banco de destino: substituído, se existe (o atual vira __anterior); criado, se não.
	if bd, ok := banco(infoD, pd.Banco); ok {
		p.Destino.Existe, p.Destino.Info = true, bd
	} else {
		m, err := conexao.BancoModelo(ctx, destino.admin, infoD.VersaoNum)
		if err != nil {
			return p, fmt.Errorf("lendo o template1 do destino: %w", err)
		}
		m.Nome, m.Tamanho, m.Dono, m.Conexoes = pd.Banco, 0, destino.c.Usuario, true
		p.Destino.Info = m
		p.notar("o banco %s não existe em %s: vai ser criado com a codificação e o locale do template1 (%s, %s) e o dono %s",
			pd.Banco, pd.Conexao, m.Codificacao, descreverLocale(m), destino.c.Usuario)
	}
	if _, ok := banco(infoD, p.Novo); ok {
		p.NovoExiste = true
		if err := oferecerApagarNovo(ctx, d, &p, destino.admin, pd.Conexao); err != nil {
			return p, err
		}
	}
	usados, err := bancosDePerfis(ctx, d, pd.Conexao)
	if err != nil {
		return p, err
	}
	if usados[p.Novo] {
		p.bloquear("o banco %s, que seria o __novo, é usado por um perfil: escolha outro banco de destino", p.Novo)
	}
	if ps, err := d.Cadastro.Perfis(ctx); err == nil {
		for _, x := range ps {
			if x.Destino == pd.Conexao && x.DestinoBanco == pd.Banco {
				p.notar("o banco %s é o destino do perfil %s: a próxima cópia dele substitui o que este arquivo trouxer", pd.Banco, x.Nome)
			}
			if x.Origem == pd.Conexao && x.OrigemBanco == pd.Banco {
				p.avisar("o banco %s é a ORIGEM do perfil %s: a troca derruba as sessões dele (um dump do perfil em andamento cai), e a próxima cópia do perfil leva o que este arquivo trouxer", pd.Banco, x.Nome)
			}
		}
	}
	if p.Anteriores, err = anteriores(ctx, destino.admin, pd.Banco, usados); err != nil {
		return p, err
	}
	if p.Destino.Existe {
		if p.Sessoes, err = sessoes(ctx, destino.admin, pd.Banco); err != nil {
			return p, err
		}
	}
	if orfas, err := rolesOrfas(ctx, d, destino.admin); err == nil && len(orfas) > 0 {
		p.notar("sobrou no destino a role temporária %s, de uma execução que não terminou: esta restauração a neutraliza e remove", strings.Join(orfas, ", "))
	}

	// O disco do destino, quando ele está neste servidor. Um arquivo comprimido cresce no restore.
	if destino.c.Acesso == cadastro.AcessoDireto && (ehLocal(destino.c.Host) || strings.HasPrefix(destino.c.Host, "/")) {
		var dd string
		if err := destino.admin.QueryRow(ctx, "SHOW data_directory").Scan(&dd); err == nil {
			var st syscall.Statfs_t
			if syscall.Statfs(dd, &st) == nil {
				if livre := int64(st.Bavail) * int64(st.Bsize); livre < 2*a.Tamanho {
					p.avisar("o disco do destino (%s) tem %s livres, e o arquivo tem %s: o banco restaurado costuma ocupar mais que o arquivo (comprimido, sem os índices)", dd, Tamanho(livre), Tamanho(a.Tamanho))
				}
			}
		}
	}

	p.notar("restaura o arquivo %s (%s, %s; %s): ele só é lido, e continua na pasta de entrada", filepath.Base(a.Caminho), a.Formato, Tamanho(a.Tamanho), a.Origem())
	switch {
	case a.SQL():
		p.notar("SQL puro: roda pelo psql, sem paralelismo, e os donos e as permissões vêm do arquivo como estão (uma role que não existe no destino vira erro, e a troca espera a sua decisão). O formato custom (pg_dump -Fc) restaura em paralelo e entrega tudo ao dono do destino")
	case a.Formato == ArqTar:
		p.notar("o formato tar não restaura em paralelo: o restore vai com um job, e o ANALYZE com %d", pd.Jobs)
	}
	p.notar("não há conferência com a origem (o arquivo não traz a contagem dela): o resultado mostra os erros do restore")
	p.notar("o arquivo roda com superusuário no destino, como num restore à mão: restaure só arquivos de fonte confiável")
	return p, nil
}

// listarArquivo roda o pg_restore --list do arquivo na imagem do plano, sem rede (ele não conecta em
// nada): o índice diz as extensões que o restore vai criar e os user mappings que ele traz, e o
// pg_restore confere que lê o arquivo.
func listarArquivo(ctx context.Context, d Deps, p *Plano, a Arquivo) (exts, externos []string, err error) {
	inst, err := d.Cadastro.Instancia(ctx)
	if err != nil {
		return nil, nil, err
	}
	fonte, err := filepath.EvalSymlinks(a.Caminho)
	if err != nil {
		return nil, nil, err
	}
	args := []string{"--list", dentroArquivo}
	if err := versoes.Conferir("pg_restore", args, p.Imagem); err != nil {
		return nil, nil, err
	}
	e := imagens.Execucao{
		Imagem: p.ImagemRef, Nome: fmt.Sprintf("pghangar-%s-plano-%d", inst, time.Now().UnixNano()), SemRede: true,
		Rotulos: map[string]string{imagens.Rotulo: imagens.RotuloPlano, imagens.RotuloInstancia: inst},
		Volumes: []imagens.Volume{{Origem: fonte, Destino: dentroArquivo, SoLeitura: true, Existente: true}},
		Comando: append([]string{"pg_restore"}, args...),
	}
	var ultimas []string
	cod, err := d.Docker.Rodar(ctx, e, func(fluxo, t string) {
		if fluxo == "erro" {
			if len(ultimas) < 20 {
				ultimas = append(ultimas, t)
			}
			return
		}
		if m := reTocExtensao.FindStringSubmatch(t); m != nil {
			exts = anexar(exts, normIdent(m[1]))
		}
		if m := reTocMapeamento.FindStringSubmatch(t); m != nil {
			externos = anexar(externos, normIdent(m[1]))
		}
	})
	if err != nil {
		return nil, nil, err
	}
	if cod != 0 {
		return nil, nil, falhaCliente("o pg_restore --list", cod, ultimas)
	}
	return exts, externos, nil
}

// --- a execução ------------------------------------------------------------------------------------

// pedidoDoPlano refaz o pedido de um plano de arquivo confirmado (a execução planeja de novo).
func pedidoDoPlano(p Plano) (PedidoArquivo, error) {
	if p.Arquivo == nil {
		return PedidoArquivo{}, errors.New("o plano confirmado não tem o arquivo: confirme de novo")
	}
	return PedidoArquivo{Caminho: p.Arquivo.Caminho, Conexao: p.Perfil.Destino, Banco: p.Perfil.DestinoBanco, Jobs: p.Perfil.JobsRestore, confirmado: &p}, nil
}

// mesmoArquivo confere que o arquivo é o que o sysadmin confirmou: o mesmo inode, com o mesmo
// tamanho, a mesma data e o mesmo ctime (que ninguém volta atrás: reescrever o conteúdo e repor a
// data com touch muda o ctime).
func mesmoArquivo(confirmado, agora *Arquivo) error {
	if confirmado == nil || agora == nil || confirmado.Formato != agora.Formato || confirmado.Tamanho != agora.Tamanho ||
		!confirmado.Modificado.Equal(agora.Modificado) || confirmado.Inode != agora.Inode || confirmado.Dispositivo != agora.Dispositivo ||
		!confirmado.Mudanca.Equal(agora.Mudanca) {
		return errors.New("o arquivo mudou desde a confirmação: confirme de novo")
	}
	return nil
}

// restoreArquivo restaura o arquivo no __novo, com a role temporária: pelo pg_restore (custom, tar e
// diretório, montados só para leitura) ou pelo psql (SQL puro, pela entrada padrão).
func (r *corrida) restoreArquivo(ctx context.Context) (int, error) {
	a := *r.p.Arquivo
	if a.SQL() {
		return r.psqlArquivo(ctx, a)
	}
	fonte, err := filepath.EvalSymlinks(a.Caminho)
	if err != nil {
		return 0, err
	}
	jobs := r.p.Perfil.JobsRestore
	if !a.Paralelo() {
		jobs = 1 // o tar não restaura em paralelo; o ANALYZE, sim
	}
	return r.pgRestore(ctx, dentroArquivo, jobs, []imagens.Volume{{Origem: fonte, Destino: dentroArquivo, SoLeitura: true, Existente: true}})
}

// psqlArquivo manda o SQL ao psql pela entrada padrão, conferido e descomprimido. O psql segue
// depois de um erro (como num restore à mão), e os erros são contados: com algum, a troca espera a
// decisão do sysadmin. As mensagens do servidor vêm em inglês (lc_messages=C): num servidor em
// português, o "ERROR" viria "ERRO", e nenhum erro seria contado.
func (r *corrida) psqlArquivo(ctx context.Context, a Arquivo) (int, error) {
	fonte, conta, fechar, err := abrirArquivoSQL(a)
	if err != nil {
		return 0, err
	}
	defer fechar()
	filtro := novoFiltroSQL(fonte)
	args := []string{"--no-psqlrc", "--quiet", "--file=-", "--dbname=" + conexao.DSN(r.destino.c, r.destino.ponte, r.e.BancoNovo, "pghangar")}
	if err := versoes.Conferir("psql", args, r.p.Imagem); err != nil {
		return 0, err
	}
	// O andamento é o quanto do arquivo (em disco, comprimido ou não) já foi lido, em MB.
	const mb = 1 << 20
	total := int(max(a.Tamanho/mb, 1))
	medindo := make(chan struct{})
	defer close(medindo)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-medindo:
				return
			case <-t.C:
				r.progresso(min(int(conta.n.Load()/mb), total), total, filtro.objeto(), false)
			}
		}
	}()
	erros := 0
	cod, ultimas, err := r.rodarCom(ctx, "restore", append([]string{"psql"}, args...), nil,
		map[string]string{"PGOPTIONS": "-c role=" + r.role + " -c lc_messages=C"}, filtro, func(fluxo, t string) {
			if fluxo == "erro" && (strings.Contains(t, "ERROR:") || strings.Contains(t, ": error:")) {
				erros++
			}
		})
	if ferr := filtro.erro(); ferr != nil && !errors.Is(ferr, io.EOF) {
		return 0, fmt.Errorf("o restore parou: %w", ferr)
	}
	if err != nil {
		return 0, err
	}
	if cod != 0 {
		return 0, falhaCliente("o psql", cod, ultimas)
	}
	r.progresso(total, total, "", true)
	if erros > 0 {
		r.e.ErrosRestore = erros
		r.gravar()
		r.d.logf("o restore terminou com %d erro(s) (as linhas com ERROR acima)", erros)
	}
	return erros, nil
}
