package motor

// O SQL puro de um arquivo de fora vai ao psql conferido comando a comando (docs/ESTRATEGIA.md
// §17). Um comando é o texto entre dois ";" de fora de string, de $$ e de comentário: pode ocupar
// várias linhas, e uma linha pode ter vários. Cada um é conferido pelas primeiras palavras.
//
// Não é um sandbox: o arquivo roda com superusuário no destino, como num restore à mão. A
// conferência tira o que não tem lugar no restore de um banco: o que sai dele (roles, outros
// bancos, a configuração do servidor) e os comandos do psql, que rodariam no container.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

// lexSQL acompanha se o texto está dentro de uma string, de um identificador entre aspas, de um $$
// ou de um comentário de bloco.
type lexSQL struct {
	aspas   byte // ' ou " aberta
	escape  bool // a string aberta é E'...': a barra escapa
	emDolar bool
	tag     string // a do $tag$ aberto
	bloco   int    // os /* */ abertos (aninham no PostgreSQL)
}

func (l *lexSQL) fora() bool { return l.aspas == 0 && !l.emDolar && l.bloco == 0 }

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// tagDolar lê o $tag$ no começo de s (s[0] é o $). Um $1 não é um.
func tagDolar(s string) (string, bool) {
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '$' {
			return s[1:i], true
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 || i > 1 && c >= '0' && c <= '9') {
			return "", false
		}
	}
	return "", false
}

// passo consome o que começa em s[i] (um caractere, uma aspa dobrada, um $tag$, um comentário de
// linha inteiro) e devolve onde o próximo começa.
func (l *lexSQL) passo(s string, i int) int {
	c := s[i]
	var prox byte
	if i+1 < len(s) {
		prox = s[i+1]
	}
	switch {
	case l.bloco > 0:
		switch {
		case c == '*' && prox == '/':
			l.bloco--
			return i + 2
		case c == '/' && prox == '*':
			l.bloco++
			return i + 2
		}
	case l.emDolar:
		if c == '$' && strings.HasPrefix(s[i+1:], l.tag+"$") {
			l.emDolar = false
			return i + len(l.tag) + 2
		}
	case l.aspas == '\'':
		switch {
		case l.escape && c == '\\':
			return min(i+2, len(s))
		case c == '\'' && prox == '\'':
			return i + 2
		case c == '\'':
			l.aspas = 0
		}
	case l.aspas == '"':
		switch {
		case c == '"' && prox == '"':
			return i + 2
		case c == '"':
			l.aspas = 0
		}
	default:
		switch {
		case c == '-' && prox == '-':
			return len(s)
		case c == '/' && prox == '*':
			l.bloco++
			return i + 2
		case c == '\'':
			l.aspas = '\''
			l.escape = i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !identByte(s[i-2]))
		case c == '"':
			l.aspas = '"'
		case c == '$' && (i == 0 || !identByte(s[i-1])):
			if tag, ok := tagDolar(s[i:]); ok {
				l.emDolar, l.tag = true, tag
				return i + len(tag) + 2
			}
		}
	}
	return i + 1
}

// O que acontece com um comando.
const (
	cmdPendente = iota // as primeiras palavras ainda não chegaram
	cmdPassa
	cmdPula // vira espaços: os comandos do banco de um pg_dump -C, as subscriptions, as publications
)

var (
	// Os comandos do banco inteiro (que um pg_dump -C traz): o restore vai para o __novo, e eles
	// ficam de fora. O nome do banco é o primeiro grupo que casar.
	reComandoBanco = regexp.MustCompile(`(?i)^(?:CREATE DATABASE ("(?:[^"]|"")+"|[^\s;]+)|DROP DATABASE (?:IF EXISTS )?("(?:[^"]|"")+"|[^\s;]+)|` +
		`ALTER DATABASE ("(?:[^"]|"")+"|[^\s;]+)|COMMENT ON DATABASE ("(?:[^"]|"")+"|[^\s;]+)|` +
		`SECURITY LABEL (?:FOR \S+ )?ON DATABASE ("(?:[^"]|"")+"|[^\s;]+)|(?:GRANT|REVOKE) .*? ON DATABASE ("(?:[^"]|"")+"|[^\s;,]+))`)
	reCriaBanco = regexp.MustCompile(`(?i)^CREATE DATABASE\b`)
	// As subscriptions e as publications ficam de fora, como no --no-subscriptions e no
	// --no-publications do pg_restore: uma subscription leva a conexão (e a senha) da origem.
	reSubPub = regexp.MustCompile(`(?i)^(?:(?:CREATE|ALTER|DROP) (?:SUBSCRIPTION|PUBLICATION)|COMMENT ON (?:SUBSCRIPTION|PUBLICATION)|` +
		`SECURITY LABEL (?:FOR \S+ )?ON (?:SUBSCRIPTION|PUBLICATION))\b`)
	reCopy       = regexp.MustCompile(`(?i)^COPY\b`)
	reCopyStdin  = regexp.MustCompile(`(?i)\bFROM STDIN\b`)
	reCopyProg   = regexp.MustCompile(`(?i)\b(?:FROM|TO) PROGRAM\b`)
	reExtensao   = regexp.MustCompile(`(?i)^CREATE EXTENSION (?:IF NOT EXISTS )?("(?:[^"]|"")+"|[^\s;]+)`)
	reMapeamento = regexp.MustCompile(`(?i)^CREATE USER MAPPING (?:IF NOT EXISTS )?FOR (?:"(?:[^"]|"")+"|\S+) SERVER ("(?:[^"]|"")+"|[^\s;(]+)`)
	reUsuario    = regexp.MustCompile(`(?i)^(?:CREATE|ALTER|DROP) USER\b( MAPPING\b)?`)
	reObjeto     = regexp.MustCompile(`^-- (?:Data for )?Name: ([^;]+); Type: ([^;]+); Schema: ([^;]+);`)
	reRestrict   = regexp.MustCompile(`^\\(?:un)?restrict [A-Za-z0-9]+$`)
	// O \connect que o pg_dump -C escreve: o nome, entre aspas ou não, ou o dbname='…' de um nome que
	// precisa de aspas. Nada depois dele.
	reConnect    = regexp.MustCompile(`^\\(?:c|connect)\s+(?:-reuse-previous=on\s+"dbname='((?:[^'\\]|\\.)*)'"|"((?:[^"]|"")+)"|([^\s"\\;]+))\s*$`)
	proibicoesDe = []struct {
		re     *regexp.Regexp
		motivo string
	}{
		{regexp.MustCompile(`(?i)^(?:CREATE|ALTER|DROP) (?:ROLE|GROUP)\b`), "mexe nas roles do servidor"},
		{regexp.MustCompile(`(?i)^(?:CREATE|ALTER|DROP) TABLESPACE\b`), "mexe nos tablespaces do servidor"},
		{regexp.MustCompile(`(?i)^ALTER SYSTEM\b`), "muda a configuração do servidor"},
		{regexp.MustCompile(`(?i)^(?:REASSIGN|DROP) OWNED\b`), "mexe também em objetos de fora do banco"},
		{regexp.MustCompile(`(?i)^LOAD '`), "carrega uma biblioteca no servidor"},
	}
)

// As primeiras palavras dos comandos que a conferência olha. Um comando que começa com outra (um
// INSERT, um SELECT, um SET) passa assim que a primeira palavra fecha.
var conferidas = map[string]bool{"CREATE": true, "DROP": true, "ALTER": true, "COMMENT": true, "SECURITY": true,
	"GRANT": true, "REVOKE": true, "REASSIGN": true, "LOAD": true, "COPY": true}

// Os comandos que só se decidem no fim: o resto deles muda o que são (o PROGRAM e o FROM stdin de
// um COPY, o ON DATABASE de um GRANT). A cabeça deles não é cortada.
var decideNoFim = map[string]bool{"COPY": true, "GRANT": true, "REVOKE": true, "SECURITY": true}

// limiteRetido é o maior comando pendente que fica retido (as primeiras palavras dele não chegaram).
const limiteRetido = 4 << 20

// varreduraSQL lê um SQL puro como o restore vai ler, linha a linha, e devolve o que vai ao psql:
// as linhas como estão, menos os comandos pulados (que viram espaços: o número das linhas nos
// erros continua o do arquivo). Um comando ainda pendente fica retido até se decidir.
type varreduraSQL struct {
	lex   lexSQL
	linha int

	// O comando em andamento.
	noMeio     bool
	estado     int
	cabeca     strings.Builder // as primeiras palavras, sem os comentários e com os espaços juntados
	limite     int             // até onde a cabeça cresce: 400 bytes, ou o comando inteiro, se ele decide no fim
	linhaCmd   int
	copyDepois bool // um COPY … FROM stdin terminou nesta linha: os dados vêm na seguinte
	emCopy     bool

	saida  []byte // o que vai ao psql, retido enquanto o comando está pendente
	inicio int    // onde o comando pendente começa na saída

	// O que a leitura achou.
	banco      string // o banco que os comandos de banco citam: um só (o do pg_dump -C)
	criaBanco  bool
	extensoes  []string
	externos   []string // os servidores externos com user mappings
	subPub     int      // as subscriptions e publications puladas
	proibidos  []string // os primeiros, com a linha
	nProibidos int
	recusa     *linhaRecusada // a primeira recusa ainda não entregue
	objeto     string         // o objeto em que o restore está, pelos comentários do pg_dump
	dump       bool           // o cabeçalho do pg_dump
	dumpFim    bool           // a linha final do pg_dump
	cortado    string         // por que o arquivo parece cortado (no fim da leitura)
}

// linhaRecusada é o comando que parou o restore.
type linhaRecusada struct {
	linha         int
	texto, motivo string
}

func (l *linhaRecusada) Error() string {
	return fmt.Sprintf("a linha %d do arquivo foi recusada: %s (%s)", l.linha, truncarSQL(l.texto), l.motivo)
}

func truncarSQL(t string) string {
	if len(t) > 100 {
		return t[:100] + "…"
	}
	return t
}

func ehEspaco(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
}

// ler lê uma linha (com o \n) e devolve o que pode ir ao psql agora, e a recusa, se houver.
func (v *varreduraSQL) ler(s string) (string, error) {
	v.linha++
	if v.linha == 1 {
		s = strings.TrimPrefix(s, "\ufeff") // o psql pularia o BOM; a conferência também
	}
	if v.emCopy {
		if strings.TrimSpace(s) == `\.` {
			v.emCopy = false
		}
		v.escrever(s, false)
		return v.soltar(), v.entregar()
	}
	t := strings.TrimSpace(s)
	if !v.noMeio && v.lex.fora() {
		switch {
		case strings.HasPrefix(t, `\`):
			v.comandoPsql(s, t)
			return v.soltar(), v.entregar()
		case strings.HasPrefix(t, "--"):
			v.comentario(t)
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		fora := v.lex.fora()
		comentario := v.lex.bloco > 0 || fora && i+1 < len(s) && (c == '-' && s[i+1] == '-' || c == '/' && s[i+1] == '*')
		if fora && !v.noMeio && !comentario && !ehEspaco(c) {
			if c == '\\' {
				v.recusar(t, "um comando do psql no meio da linha, que rodaria no container")
				v.escrever(s[i:], true)
				break
			}
			v.comecar()
		}
		if fora && v.noMeio && c == '\\' {
			v.recusar(t, "um comando do psql no meio de um comando, que rodaria no container")
			v.escrever(s[i:], true)
			break
		}
		j := v.lex.passo(s, i)
		pedaco := s[i:j]
		if v.noMeio {
			if comentario {
				v.juntar(" ")
			} else {
				v.juntar(pedaco)
			}
			v.escrever(pedaco, v.estado == cmdPula)
			if v.estado == cmdPendente && v.lex.fora() {
				v.decidirCedo() // fora de aspas: nenhum nome fica cortado na cabeça
			}
			if fora && c == ';' {
				v.terminar()
			}
		} else {
			v.escrever(pedaco, false)
		}
		i = j
	}
	if v.copyDepois {
		v.emCopy, v.copyDepois = true, false
	}
	if v.estado == cmdPendente && v.noMeio && len(v.saida)-v.inicio > limiteRetido {
		v.recusar(t, "um comando longo demais para conferir")
		v.decidir(cmdPula)
	}
	return v.soltar(), v.entregar()
}

// fim fecha a leitura: o comando que ficou sem ";" (o psql o roda no fim do arquivo) e se o arquivo
// parece cortado (cortado fica com o motivo).
func (v *varreduraSQL) fim() (string, error) {
	if v.noMeio {
		v.terminar()
	}
	saida := v.soltar()
	switch {
	case v.emCopy:
		v.cortado = "termina no meio dos dados de uma tabela (um COPY sem o \\.)"
	case !v.lex.fora():
		v.cortado = "termina dentro de uma string, de um $$ ou de um comentário"
	case v.dump && !v.dumpFim:
		v.cortado = "não tem a linha final do pg_dump (-- PostgreSQL database dump complete)"
	}
	if err := v.entregar(); err != nil {
		return saida, err
	}
	if v.cortado != "" {
		return saida, errors.New("o arquivo está incompleto: " + v.cortado)
	}
	return saida, nil
}

func (v *varreduraSQL) comentario(t string) {
	switch {
	case t == "-- PostgreSQL database dump":
		v.dump = true
	case t == "-- PostgreSQL database dump complete":
		v.dumpFim = true
	}
	if m := reObjeto.FindStringSubmatch(t); m != nil {
		o := m[1]
		if m[3] != "-" {
			o = m[3] + "." + o
		}
		v.objeto = m[2] + " " + o
	}
}

// comandoPsql trata uma linha que é um comando do psql. Passam só o \restrict e o \unrestrict do
// pg_dump, como estão, e o \connect do banco de um pg_dump -C, que é pulado.
func (v *varreduraSQL) comandoPsql(s, t string) {
	if reRestrict.MatchString(t) {
		v.escrever(s, false)
		return
	}
	if m := reConnect.FindStringSubmatch(t); m != nil {
		var alvo string
		switch {
		case m[1] != "":
			alvo = strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(m[1])
		case m[2] != "":
			alvo = strings.ReplaceAll(m[2], `""`, `"`)
		default:
			alvo = m[3]
		}
		if motivo := v.citar(alvo, false); motivo != "" {
			v.recusar(t, motivo)
		}
		v.escrever(s, true)
		return
	}
	nome := strings.TrimPrefix(strings.Fields(t)[0], `\`)
	v.recusar(t, "um comando do psql (\\"+nome+"), que rodaria no container")
	v.escrever(s, true)
}

func (v *varreduraSQL) comecar() {
	v.noMeio, v.estado, v.linhaCmd, v.inicio, v.limite = true, cmdPendente, v.linha, len(v.saida), 400
	v.cabeca.Reset()
}

// juntar põe o texto na cabeça do comando, com os espaços juntados.
func (v *varreduraSQL) juntar(p string) {
	for i := 0; i < len(p) && v.cabeca.Len() < v.limite; i++ {
		c := p[i]
		if ehEspaco(c) {
			if b := v.cabeca.String(); b == "" || b[len(b)-1] == ' ' {
				continue
			}
			c = ' '
		}
		v.cabeca.WriteByte(c)
	}
}

// decidirCedo decide o comando assim que dá, para não reter o resto dele (o corpo de um CREATE
// TABLE, um INSERT enorme): um comando que a conferência não olha, logo na primeira palavra; os
// outros, nas primeiras oito palavras (ou nos primeiros 400 bytes). Os que se decidem no fim esperam
// o ";".
func (v *varreduraSQL) decidirCedo() {
	h := v.cabeca.String()
	n := 0
	for n < len(h) && (h[n] >= 'A' && h[n] <= 'Z' || h[n] >= 'a' && h[n] <= 'z' || h[n] == '_') {
		n++
	}
	if n == len(h) {
		return // a primeira palavra ainda não fechou
	}
	primeira := strings.ToUpper(h[:n])
	switch {
	case !conferidas[primeira]:
		v.decidir(v.classificar(h))
	case decideNoFim[primeira]:
		v.limite = 1 << 20
	case len(strings.Fields(h)) >= 8 || v.cabeca.Len() >= v.limite:
		v.decidir(v.classificar(h))
	}
}

// terminar fecha o comando no ";" (ou no fim do arquivo).
func (v *varreduraSQL) terminar() {
	h := strings.TrimSpace(v.cabeca.String())
	if v.estado == cmdPendente {
		v.decidir(v.classificar(h))
	}
	if v.estado == cmdPassa && reCopy.MatchString(h) && reCopyStdin.MatchString(h) {
		v.copyDepois = true
	}
	v.noMeio = false
}

// classificar diz o que fazer com o comando pelas primeiras palavras, e anota o que ele cria.
func (v *varreduraSQL) classificar(h string) int {
	if m := reComandoBanco.FindStringSubmatch(h); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				if motivo := v.citar(normIdent(g), reCriaBanco.MatchString(h)); motivo != "" {
					v.recusar(h, motivo)
				}
				return cmdPula
			}
		}
	}
	if reSubPub.MatchString(h) {
		v.subPub++
		return cmdPula
	}
	if m := reUsuario.FindStringSubmatch(h); m != nil && m[1] == "" {
		v.recusar(h, "mexe nas roles do servidor")
		return cmdPula
	}
	for _, p := range proibicoesDe {
		if p.re.MatchString(h) {
			v.recusar(h, p.motivo)
			return cmdPula
		}
	}
	if reCopy.MatchString(h) && reCopyProg.MatchString(h) {
		v.recusar(h, "roda um programa no servidor")
		return cmdPula
	}
	if m := reExtensao.FindStringSubmatch(h); m != nil {
		v.extensoes = anexar(v.extensoes, normIdent(m[1]))
	}
	if m := reMapeamento.FindStringSubmatch(h); m != nil {
		v.externos = anexar(v.externos, normIdent(m[1]))
	}
	return cmdPassa
}

// decidir fixa o estado do comando; um comando pulado vira espaços desde o começo dele.
func (v *varreduraSQL) decidir(estado int) {
	v.estado = estado
	if estado == cmdPula {
		for i := v.inicio; i < len(v.saida); i++ {
			if v.saida[i] != '\n' {
				v.saida[i] = ' '
			}
		}
	}
}

// citar registra o banco de um comando de banco: o primeiro vale (é o do pg_dump -C), e um outro é
// recusado (um pg_dumpall, ou um script de vários bancos). Devolve o motivo da recusa.
func (v *varreduraSQL) citar(banco string, cria bool) string {
	if v.banco == "" {
		v.banco = banco
	}
	if banco != v.banco {
		return fmt.Sprintf("cita o banco %s, além de %s: restaure um banco só", banco, v.banco)
	}
	v.criaBanco = v.criaBanco || cria
	return ""
}

func (v *varreduraSQL) recusar(texto, motivo string) {
	linha := v.linha
	if v.noMeio {
		linha = v.linhaCmd
	}
	v.nProibidos++
	if len(v.proibidos) < 5 {
		v.proibidos = append(v.proibidos, fmt.Sprintf("linha %d: %s (%s)", linha, truncarSQL(texto), motivo))
	}
	if v.recusa == nil {
		v.recusa = &linhaRecusada{linha: linha, texto: texto, motivo: motivo}
	}
}

// entregar devolve a recusa uma vez só.
func (v *varreduraSQL) entregar() error {
	if r := v.recusa; r != nil {
		v.recusa = nil
		return r
	}
	return nil
}

// escrever põe o texto na saída; em branco, só os \n ficam.
func (v *varreduraSQL) escrever(p string, branco bool) {
	if !branco {
		v.saida = append(v.saida, p...)
		return
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '\n' {
			v.saida = append(v.saida, '\n')
		} else {
			v.saida = append(v.saida, ' ')
		}
	}
}

// soltar devolve a saída pronta: tudo, menos o comando pendente.
func (v *varreduraSQL) soltar() string {
	fim := len(v.saida)
	if v.noMeio && v.estado == cmdPendente {
		fim = v.inicio
	}
	s := string(v.saida[:fim])
	v.saida = append(v.saida[:0], v.saida[fim:]...)
	v.inicio = 0
	return s
}

// normIdent é o nome como o PostgreSQL o lê: entre aspas, como está; sem aspas, em minúsculas.
func normIdent(s string) string {
	s = strings.TrimRight(s, ";,")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

func anexar(l []string, x string) []string {
	for _, y := range l {
		if y == x {
			return l
		}
	}
	return append(l, x)
}

// varrerSQL lê o SQL inteiro antes do plano, com as regras do restore. O erro é o do arquivo (ou o
// cancelamento); as recusas e um arquivo cortado ficam na varredura.
func varrerSQL(ctx context.Context, a Arquivo) (*varreduraSQL, error) {
	r, _, fechar, err := abrirArquivoSQL(a)
	if err != nil {
		return nil, err
	}
	defer fechar()
	v := &varreduraSQL{}
	br := bufio.NewReaderSize(r, 1<<20)
	for n := 0; ; n++ {
		if n%10000 == 0 && ctx.Err() != nil {
			return v, ctx.Err()
		}
		linha, err := br.ReadString('\n')
		if len(linha) > 0 {
			_, _ = v.ler(linha)
		}
		if err == io.EOF {
			_, _ = v.fim()
			return v, nil
		}
		if err != nil {
			return v, err
		}
	}
}

// filtroSQL é o SQL que vai para o psql, pela varredura. Uma recusa para tudo: o resto do arquivo
// não vai. Um arquivo que termina cortado também para, e a execução falha antes da troca.
type filtroSQL struct {
	br   *bufio.Reader
	v    varreduraSQL
	pend []byte
	mu   sync.Mutex
	err  error
}

func novoFiltroSQL(r io.Reader) *filtroSQL { return &filtroSQL{br: bufio.NewReaderSize(r, 1<<20)} }

func (f *filtroSQL) Read(p []byte) (int, error) {
	for len(f.pend) == 0 {
		if err := f.erro(); err != nil {
			return 0, err
		}
		linha, err := f.br.ReadString('\n')
		f.mu.Lock()
		if len(linha) > 0 {
			saida, rec := f.v.ler(linha)
			if rec != nil {
				f.err = rec
			} else {
				f.pend = append(f.pend, saida...)
			}
		}
		switch {
		case f.err != nil:
		case err == io.EOF:
			saida, fimErr := f.v.fim()
			f.pend = append(f.pend, saida...)
			f.err = io.EOF
			if fimErr != nil {
				f.err = fimErr
			}
		case err != nil:
			f.err = err
		}
		f.mu.Unlock()
	}
	n := copy(p, f.pend)
	f.pend = f.pend[n:]
	return n, nil
}

// erro é por que o filtro parou: io.EOF no fim do arquivo.
func (f *filtroSQL) erro() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *filtroSQL) objeto() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v.objeto
}
