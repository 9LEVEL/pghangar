package motor

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// O modo "link instável" (docs/ESTRATEGIA.md §16, inspirado no Dolly): o esquema sai pelo pg_dump
// --schema-only, e os dados, por tabela, em blocos pela chave, cada um num arquivo, com checkpoint.
// A queda do túnel não perde o que já veio: a origem é reaberta e o dump continua de onde parou.
// O preço é o mesmo do Dolly: não há um snapshot único (as tabelas são lidas em momentos
// diferentes), e uma chave estrangeira que não bata aparece como erro no restore.

// linhasPorBloco é o tamanho de cada bloco.
var linhasPorBloco = 50000

// TabelaBlocos é o plano de uma tabela e o ponto em que ela está.
type TabelaBlocos struct {
	Schema     string   `json:"schema"`
	Nome       string   `json:"nome"`
	Colunas    []string `json:"colunas"`     // sem as geradas e as apagadas
	Chave      []string `json:"chave"`       // vazio: a tabela vai inteira, num bloco só
	TiposChave []string `json:"tipos_chave"` // format_type de cada coluna da chave
	SemDados   bool     `json:"sem_dados"`
	Impressao  string   `json:"impressao"` // colunas, tipos e chave: se mudar, não se retoma

	Ultimo   []string `json:"ultimo,omitempty"` // a chave do fim do último bloco gravado
	Blocos   int      `json:"blocos"`
	Linhas   int64    `json:"linhas"`
	Completa bool     `json:"completa"`
}

func (t TabelaBlocos) qualificado() string { return pgx.Identifier{t.Schema, t.Nome}.Sanitize() }
func (t TabelaBlocos) chaveNome() string   { return t.Schema + "." + t.Nome }

func (t TabelaBlocos) colunasSQL() string {
	cs := make([]string, len(t.Colunas))
	for i, c := range t.Colunas {
		cs[i] = id(c)
	}
	return strings.Join(cs, ", ")
}

func (t TabelaBlocos) chaveSQL() string {
	cs := make([]string, len(t.Chave))
	for i, c := range t.Chave {
		cs[i] = id(c)
	}
	return strings.Join(cs, ", ")
}

// literalChave escreve (v1::tipo1, v2::tipo2) para comparar com a chave.
func (t TabelaBlocos) literalChave(vs []string) string {
	ls := make([]string, len(vs))
	for i, v := range vs {
		ls[i] = lit(v) + "::" + t.TiposChave[i]
	}
	return "(" + strings.Join(ls, ", ") + ")"
}

// EstadoBlocos é o checkpoint do dump em blocos (dados/estado.json).
type EstadoBlocos struct {
	Tabelas    []TabelaBlocos   `json:"tabelas"`
	Sequencias []Sequencia      `json:"sequencias,omitempty"`
	Completo   bool             `json:"completo"`
	Linhas     map[string]int64 `json:"-"`
}

// Sequencia é o valor de uma sequência no fim do dump.
type Sequencia struct {
	Schema string `json:"schema"`
	Nome   string `json:"nome"`
	Valor  *int64 `json:"valor"` // nulo: nunca foi usada
}

func caminhoEstado(trabalho string) string { return filepath.Join(trabalho, "dados", "estado.json") }

func lerEstado(trabalho string) (EstadoBlocos, error) {
	var e EstadoBlocos
	b, err := os.ReadFile(caminhoEstado(trabalho))
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(b, &e)
}

func gravarEstado(trabalho string, e EstadoBlocos) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	tmp := caminhoEstado(trabalho) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, caminhoEstado(trabalho))
}

// dirTabela é o diretório dos blocos de uma tabela, pelo nome dela (e não pela posição no plano:
// entre duas tentativas, uma tabela nova mudaria as posições e trocaria os blocos de tabela).
func dirTabela(trabalho string, t TabelaBlocos) string {
	h := sha256.Sum256([]byte(t.Schema + "\x00" + t.Nome))
	return filepath.Join(trabalho, "dados", hex.EncodeToString(h[:8]))
}

func arquivoBloco(trabalho string, t TabelaBlocos, bloco int) string {
	return filepath.Join(dirTabela(trabalho, t), fmt.Sprintf("%06d.copy.gz", bloco))
}

// planoDasTabelas lê as tabelas da origem, com as colunas e a chave de cada uma.
func planoDasTabelas(ctx context.Context, conn *pgx.Conn, pf cadastro.Perfil) ([]TabelaBlocos, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.oid, n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
		   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		 ORDER BY 2, 3`)
	if err != nil {
		return nil, err
	}
	type tab struct {
		oid          uint32
		schema, nome string
	}
	var ts []tab
	for rows.Next() {
		var t tab
		if err := rows.Scan(&t.oid, &t.schema, &t.nome); err != nil {
			rows.Close()
			return nil, err
		}
		if len(pf.Schemas) > 0 && !casaSchema(pf.Schemas, t.schema) {
			continue
		}
		if len(pf.SchemasFora) > 0 && casaSchema(pf.SchemasFora, t.schema) {
			continue
		}
		fora := false
		for _, p := range pf.TabelasFora {
			if casaTabela(p, t.schema, t.nome) {
				fora = true
			}
		}
		if !fora {
			ts = append(ts, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []TabelaBlocos
	for _, t := range ts {
		tb := TabelaBlocos{Schema: t.schema, Nome: t.nome}
		for _, p := range pf.SemDados {
			if casaTabela(p, t.schema, t.nome) {
				tb.SemDados = true
			}
		}
		cols, err := nomesDe(ctx, conn, `SELECT attname FROM pg_attribute WHERE attrelid = $1 AND attnum > 0 AND NOT attisdropped
			AND attgenerated = '' ORDER BY attnum`, t.oid)
		if err != nil {
			return nil, err
		}
		tb.Colunas = cols
		// A chave: a primária, ou um índice único válido, sem predicado nem expressão, em ordem
		// crescente e com todas as colunas NOT NULL. A primeira que servir, a primária antes.
		rows, err := conn.Query(ctx, `
			SELECT array_agg(a.attname ORDER BY k.ord), array_agg(format_type(a.atttypid, a.atttypmod) ORDER BY k.ord)
			  FROM pg_index i
			  CROSS JOIN LATERAL unnest(i.indkey::int2[], i.indoption::int2[]) WITH ORDINALITY AS k(attnum, opcao, ord)
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			 WHERE i.indrelid = $1 AND i.indisunique AND i.indisvalid AND i.indpred IS NULL AND i.indexprs IS NULL
			 GROUP BY i.indexrelid, i.indisprimary
			HAVING bool_and(a.attnotnull) AND bool_and(k.attnum > 0) AND bool_and(k.opcao & 1 = 0)
			 ORDER BY i.indisprimary DESC, count(*), i.indexrelid
			 LIMIT 1`, t.oid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			if err := rows.Scan(&tb.Chave, &tb.TiposChave); err != nil {
				rows.Close()
				return nil, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		h := sha256.Sum256([]byte(strings.Join(tb.Colunas, ",") + "|" + strings.Join(tb.Chave, ",") + "|" + strings.Join(tb.TiposChave, ",")))
		tb.Impressao = hex.EncodeToString(h[:8])
		out = append(out, tb)
	}
	return out, nil
}

// dumpBlocos faz o dump em blocos, retomando o que um dump anterior (incompleto) já trouxe.
func (r *corrida) dumpBlocos(ctx context.Context) error {
	p := r.p
	if err := os.MkdirAll(p.DirDumps, 0o700); err != nil {
		return err
	}
	var m Manifesto
	var estado EstadoBlocos
	retomando := false
	if p.Retomar != "" {
		if mm, err := LerManifesto(p.Retomar); err == nil {
			if e, err := lerEstado(p.Retomar); err == nil {
				r.trabalho, m, estado, retomando = p.Retomar, mm, e, true
				m.Execucao = r.e.ID
				r.d.logf("retomando o dump incompleto de %s (%s)", mm.Inicio.Format("02/01 15:04"), p.Retomar)
			}
		}
	}
	if !retomando {
		nome := time.Now().Format("20060102_150405")
		r.trabalho = filepath.Join(p.DirDumps, nome)
		if _, err := os.Stat(r.trabalho); err == nil {
			r.trabalho += fmt.Sprintf("_%d", r.e.ID)
		}
		if err := os.MkdirAll(filepath.Join(r.trabalho, "dados"), 0o700); err != nil {
			return err
		}
		m = Manifesto{Perfil: p.Perfil.Nome, Execucao: r.e.ID, Estado: DumpIncompleto, Inicio: time.Now(), Origem: p.Origem,
			Imagem: p.Imagem, ImagemRef: p.ImagemRef, Cliente: p.Cliente, SemDados: p.Perfil.SemDados, Contagem: p.Contagem,
			Filtrado: p.Perfil.Filtrado(), Formato: FormatoBlocos, Extensoes: p.Extensoes, Citadas: p.Citadas, Externos: p.Externos,
			Filtros: Filtros{Schemas: p.Perfil.Schemas, SchemasFora: p.Perfil.SchemasFora, Tabelas: p.Perfil.Tabelas, TabelasFora: p.Perfil.TabelasFora}}
		if err := EscreverManifesto(r.trabalho, m); err != nil {
			return err
		}
	}
	r.e.DumpDir = r.trabalho
	r.gravar()

	esperas := r.d.EsperasRede
	if esperas == nil {
		esperas = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second,
			60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}
	}
	for tentativa := 0; ; tentativa++ {
		err := r.dumpBlocosUmaVez(ctx, &estado, retomando)
		if err == nil {
			break
		}
		var rede *falhaRede
		if !errors.As(err, &rede) || tentativa >= len(esperas) || ctx.Err() != nil {
			return err
		}
		prontas := 0
		for _, t := range estado.Tabelas {
			if t.Completa {
				prontas++
			}
		}
		r.d.logf("o dump caiu (%v): retoma em %s, de onde parou (%d de %d tabelas prontas)", err, esperas[tentativa], prontas, len(estado.Tabelas))
		r.e.Notas = append(r.e.Notas, fmt.Sprintf("o dump caiu e retomou de onde parou (tentativa %d): %v", tentativa+2, err))
		r.gravar()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(esperas[tentativa]):
		}
		if err := r.reabrirOrigem(ctx); err != nil {
			var pg *Pergunta
			if errors.As(err, &pg) || errors.Is(err, errOrigemOutra) {
				return err
			}
			// A origem pode demorar a voltar: a próxima volta do laço tenta de novo.
			r.d.logf("a origem ainda não voltou: %v", err)
		}
		retomando = true
	}
	m.Estado, m.Fim = DumpCompleto, time.Now()
	m.Tamanho = TamanhoDir(r.trabalho)
	m.Linhas = map[string]int64{}
	for _, t := range estado.Tabelas {
		m.Linhas[t.chaveNome()] = t.Linhas
	}
	r.linhasOrigem = m.Linhas
	r.e.TamanhoDump = m.Tamanho
	r.gravar()
	r.d.logf("dump em blocos completo: %s em %s", Tamanho(m.Tamanho), r.trabalho)
	return EscreverManifesto(r.trabalho, m)
}

// dumpBlocosUmaVez é uma tentativa: o esquema, o plano das tabelas e os blocos que faltam.
func (r *corrida) dumpBlocosUmaVez(ctx context.Context, estado *EstadoBlocos, retomando bool) error {
	p := r.p
	if r.origem == nil || r.origem.admin == nil {
		return &falhaRede{errors.New("a origem não está aberta")}
	}
	// 1. O esquema (sempre de novo: é pequeno, e a estrutura pode ter mudado).
	app := fmt.Sprintf("pghangar/%s/%s", r.d.Maquina, p.Perfil.Nome)
	args := []string{"--schema-only", "--format=custom", "--verbose", "--file=" + dentroTrabalho + "/esquema.dump"}
	for _, x := range p.Perfil.Schemas {
		args = append(args, "--schema="+x)
	}
	for _, x := range p.Perfil.SchemasFora {
		args = append(args, "--exclude-schema="+x)
	}
	for _, x := range p.Perfil.TabelasFora {
		args = append(args, "--exclude-table="+x)
	}
	if len(p.Perfil.Schemas) > 0 {
		args = append(args, "--extension=*")
	}
	args = append(args, "--dbname="+conexaoDSN(r.origem, p.Origem.Banco, app))
	if err := versoes.Conferir("pg_dump", args, p.Imagem); err != nil {
		return err
	}
	cod, ultimas, err := r.rodar(ctx, "esquema", append([]string{"pg_dump"}, args...), nil, nil, nil)
	if err != nil {
		return err
	}
	if cod != 0 {
		e := falhaCliente("o pg_dump do esquema", cod, ultimas)
		if ehFalhaDeRede(ultimas) || (r.origem.ponte.Tunel != nil && r.origem.ponte.Tunel.Erro() != nil) {
			return &falhaRede{e}
		}
		return e
	}

	// 2. O plano das tabelas. Uma tabela já começada só é retomada se a estrutura for a mesma;
	// se mudou, ela recomeça (os blocos dela são refeitos do zero).
	conn, err := r.origem.conectar(ctx, r.d, p.Origem.Banco)
	if err != nil {
		return &falhaRede{err}
	}
	defer conn.Close(context.Background())
	novas, err := planoDasTabelas(ctx, conn, p.Perfil)
	if err != nil {
		return &falhaRede{err}
	}
	antes := map[string]TabelaBlocos{}
	for _, t := range estado.Tabelas {
		antes[t.chaveNome()] = t
	}
	for i := range novas {
		if a, ok := antes[novas[i].chaveNome()]; ok && retomando {
			if a.Impressao == novas[i].Impressao {
				novas[i].Ultimo, novas[i].Blocos, novas[i].Linhas, novas[i].Completa = a.Ultimo, a.Blocos, a.Linhas, a.Completa
			} else {
				r.d.logf("%s mudou de estrutura desde o dump anterior: recomeça do zero", novas[i].chaveNome())
			}
		}
	}
	estado.Tabelas = novas
	if err := gravarEstado(r.trabalho, *estado); err != nil {
		return err
	}

	// 3. Os blocos que faltam, em paralelo (jobs do dump), cada tabela numa conexão.
	faltam := 0
	for _, t := range estado.Tabelas {
		if !t.Completa {
			faltam++
		}
	}
	r.d.logf("%d tabela(s), %d a fazer", len(estado.Tabelas), faltam)
	var mu sync.Mutex
	feitas := len(estado.Tabelas) - faltam
	trabalhos := make(chan int)
	erros := make(chan error, len(estado.Tabelas))
	ctxPool, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < max(p.Perfil.JobsDump, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := r.origem.conectar(ctxPool, r.d, p.Origem.Banco)
			if err != nil {
				erros <- &falhaRede{err}
				cancel()
				return
			}
			defer c.Close(context.Background())
			andamento := func(item string) {
				mu.Lock()
				f := feitas
				mu.Unlock()
				r.progresso(f, len(estado.Tabelas), item, false)
			}
			for i := range trabalhos {
				if err := r.dumpTabela(ctxPool, c, estado, &mu, i, andamento); err != nil {
					erros <- err
					cancel()
					return
				}
				mu.Lock()
				feitas++
				f := feitas
				mu.Unlock()
				r.progresso(f, len(estado.Tabelas), estado.Tabelas[i].chaveNome(), false)
			}
		}()
	}
enviar:
	for i, t := range estado.Tabelas {
		if t.Completa {
			continue
		}
		select {
		case trabalhos <- i:
		case <-ctxPool.Done():
			break enviar
		}
	}
	close(trabalhos)
	wg.Wait()
	close(erros)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A causa: os workers parados pelo cancelamento dos outros não contam. Um erro que não é de
	// rede para de vez; os de rede fazem a próxima tentativa.
	var rede error
	for err := range erros {
		switch {
		case errors.Is(err, context.Canceled):
		case errors.As(err, new(*falhaRede)) || pgconnRede(err):
			rede = err
		default:
			return err
		}
	}
	if rede != nil {
		return &falhaRede{rede}
	}

	// 4. As sequências, no fim (o valor mais novo que se consegue sem snapshot único).
	seqs, err := sequencias(ctx, conn, p.Perfil)
	if err != nil {
		return &falhaRede{err}
	}
	estado.Sequencias, estado.Completo = seqs, true
	return gravarEstado(r.trabalho, *estado)
}

// pgconnRede reconhece um erro de conexão do pgx (a conexão caiu no meio de um COPY).
func pgconnRede(err error) bool {
	s := err.Error()
	for _, m := range []string{"conn closed", "unexpected EOF", "connection reset", "broken pipe", "EOF", "i/o timeout", "use of closed network connection"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// dumpTabela grava os blocos que faltam de uma tabela, com checkpoint a cada bloco.
func (r *corrida) dumpTabela(ctx context.Context, c *pgx.Conn, estado *EstadoBlocos, mu *sync.Mutex, i int, andamento func(string)) error {
	mu.Lock()
	t := estado.Tabelas[i]
	mu.Unlock()
	if err := os.MkdirAll(dirTabela(r.trabalho, t), 0o700); err != nil {
		return err
	}
	salvar := func(t TabelaBlocos) error {
		mu.Lock()
		defer mu.Unlock()
		estado.Tabelas[i] = t
		return gravarEstado(r.trabalho, *estado)
	}
	if t.SemDados {
		t.Completa = true
		return salvar(t)
	}
	for !t.Completa {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var cond string
		var fim []string
		completa := true
		if len(t.Chave) > 0 {
			// O fim deste bloco: a chave da linha n a partir do último.
			q := "SELECT " + textosDe(t.Chave) + " FROM " + t.qualificado()
			if t.Ultimo != nil {
				q += " WHERE (" + t.chaveSQL() + ") > " + t.literalChave(t.Ultimo)
			}
			q += fmt.Sprintf(" ORDER BY %s LIMIT 1 OFFSET %d", t.chaveSQL(), linhasPorBloco-1)
			vals := make([]*string, len(t.Chave))
			alvos := make([]any, len(vals))
			for k := range vals {
				alvos[k] = &vals[k]
			}
			err := c.QueryRow(ctx, q).Scan(alvos...)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			default:
				completa = false
				fim = make([]string, len(vals))
				for k, v := range vals {
					fim[k] = *v
				}
			}
			var partes []string
			if t.Ultimo != nil {
				partes = append(partes, "("+t.chaveSQL()+") > "+t.literalChave(t.Ultimo))
			}
			if fim != nil {
				partes = append(partes, "("+t.chaveSQL()+") <= "+t.literalChave(fim))
			}
			if len(partes) > 0 {
				cond = " WHERE " + strings.Join(partes, " AND ")
			}
			cond += " ORDER BY " + t.chaveSQL()
		}
		n, err := r.blocoCopy(ctx, c, t, cond, arquivoBloco(r.trabalho, t, t.Blocos+1))
		if err != nil {
			return err
		}
		t.Blocos++
		t.Linhas += n
		t.Ultimo = fim
		t.Completa = completa
		if err := salvar(t); err != nil {
			return err
		}
		andamento(fmt.Sprintf("%s: bloco %d, %d linhas", t.chaveNome(), t.Blocos, t.Linhas))
	}
	return nil
}

func textosDe(cs []string) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = id(c) + "::text"
	}
	return strings.Join(out, ", ")
}

// blocoCopy grava um bloco (COPY … TO STDOUT, comprimido) num arquivo, de forma atômica.
func (r *corrida) blocoCopy(ctx context.Context, c *pgx.Conn, t TabelaBlocos, cond, destino string) (int64, error) {
	tmp := destino + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	sql := "COPY (SELECT " + t.colunasSQL() + " FROM " + t.qualificado() + cond + ") TO STDOUT"
	tag, err := c.PgConn().CopyTo(ctx, gz, sql)
	if err2 := gz.Close(); err == nil {
		err = err2
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		_ = os.Remove(tmp) // o bloco pela metade não vale nada: o checkpoint não andou
		return 0, err
	}
	return tag.RowsAffected(), os.Rename(tmp, destino)
}

// sequencias lê o valor de cada sequência do recorte.
func sequencias(ctx context.Context, c *pgx.Conn, pf cadastro.Perfil) ([]Sequencia, error) {
	rows, err := c.Query(ctx, `SELECT schemaname, sequencename, last_value FROM pg_sequences
		WHERE schemaname !~ '^pg_' AND schemaname <> 'information_schema' ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sequencia
	for rows.Next() {
		var s Sequencia
		if err := rows.Scan(&s.Schema, &s.Nome, &s.Valor); err != nil {
			return nil, err
		}
		if len(pf.Schemas) > 0 && !casaSchema(pf.Schemas, s.Schema) || len(pf.SchemasFora) > 0 && casaSchema(pf.SchemasFora, s.Schema) {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// restoreBlocos restaura um dump em blocos: o pre-data, os blocos com COPY FROM (em paralelo), as
// sequências, o post-data e o refresh das visões materializadas.
func (r *corrida) restoreBlocos(ctx context.Context) (int, error) {
	estado, err := lerEstado(r.trabalho)
	if err != nil {
		return 0, fmt.Errorf("lendo o estado do dump em blocos: %w", err)
	}
	if !estado.Completo {
		return 0, errors.New("o dump em blocos não está completo")
	}
	erros := 0
	secao := func(nome string, jobs int) error {
		args := []string{fmt.Sprintf("--jobs=%d", jobs), "--verbose", "--no-owner", "--no-privileges", "--no-tablespaces",
			"--no-subscriptions", "--no-publications", "--role=" + r.role, "--section=" + nome,
			"--dbname=" + conexaoDSN(r.destino, r.e.BancoNovo, "pghangar"), dentroTrabalho + "/esquema.dump"}
		if err := versoes.Conferir("pg_restore", args, r.p.Imagem); err != nil {
			return err
		}
		ignorados := -1
		cod, ultimas, err := r.rodar(ctx, "restore-"+nome, append([]string{"pg_restore"}, args...), nil, nil, func(_, t string) {
			if mm := reErrosIgnorado.FindStringSubmatch(t); mm != nil {
				_, _ = fmt.Sscanf(mm[1], "%d", &ignorados)
			}
		})
		if err != nil {
			return err
		}
		switch {
		case cod == 0:
		case cod == 1 && ignorados > 0:
			erros += ignorados
		default:
			return falhaCliente("o pg_restore ("+nome+")", cod, ultimas)
		}
		return nil
	}
	if err := secao("pre-data", 1); err != nil {
		return 0, err
	}

	// Os dados: cada tabela numa conexão, em paralelo.
	total := len(estado.Tabelas)
	var mu sync.Mutex
	feitas := 0
	trabalhos := make(chan int)
	falhas := make(chan error, total)
	var wg sync.WaitGroup
	for w := 0; w < max(r.p.Perfil.JobsRestore, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
			if err != nil {
				for i := range trabalhos {
					falhas <- fmt.Errorf("%s: %w", estado.Tabelas[i].chaveNome(), err)
				}
				return
			}
			defer c.Close(context.Background())
			for i := range trabalhos {
				t := estado.Tabelas[i]
				if err := carregarTabela(ctx, c, r.trabalho, t); err != nil {
					falhas <- fmt.Errorf("%s: %w", t.chaveNome(), err)
					continue
				}
				mu.Lock()
				feitas++
				r.progresso(feitas, total, t.chaveNome(), false)
				mu.Unlock()
			}
		}()
	}
	for i := range estado.Tabelas {
		trabalhos <- i
	}
	close(trabalhos)
	wg.Wait()
	close(falhas)
	for err := range falhas {
		r.d.logf("erro carregando os dados: %v", err)
		erros++
	}

	// As sequências.
	adm, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
	if err != nil {
		return 0, err
	}
	for _, s := range estado.Sequencias {
		if s.Valor == nil {
			continue
		}
		if _, err := adm.Exec(ctx, `SELECT setval(format('%I.%I', $1::text, $2::text)::regclass, $3::bigint, true)`, s.Schema, s.Nome, *s.Valor); err != nil {
			r.d.logf("sequência %s.%s: %v", s.Schema, s.Nome, err)
			erros++
		}
	}
	_ = adm.Close(context.Background())

	if err := secao("post-data", max(r.p.Perfil.JobsRestore, 1)); err != nil {
		return 0, err
	}
	// O --schema-only não traz o conteúdo das visões materializadas: refresh, em ordem de
	// dependência (tenta de novo as que dependem de outra ainda vazia).
	if n, err := r.refreshMatviews(ctx); err != nil {
		return 0, err
	} else {
		erros += n
	}
	if erros > 0 {
		r.e.ErrosRestore = erros
		r.gravar()
	}
	return erros, nil
}

func carregarTabela(ctx context.Context, c *pgx.Conn, trabalho string, t TabelaBlocos) error {
	if t.SemDados || t.Blocos == 0 {
		return nil
	}
	sql := "COPY " + t.qualificado() + " (" + t.colunasSQL() + ") FROM STDIN"
	for b := 1; b <= t.Blocos; b++ {
		f, err := os.Open(arquivoBloco(trabalho, t, b))
		if err != nil {
			return err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return err
		}
		_, err = c.PgConn().CopyFrom(ctx, gz, sql)
		_ = gz.Close()
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *corrida) refreshMatviews(ctx context.Context) (int, error) {
	c, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	mvs, err := nomesDe(ctx, c, `SELECT format('%I.%I', schemaname, matviewname) FROM pg_matviews
		WHERE schemaname !~ '^pg_' ORDER BY 1`)
	if err != nil {
		return 0, err
	}
	pendentes := mvs
	for len(pendentes) > 0 {
		var resto []string
		var ultimo error
		for _, mv := range pendentes {
			if _, err := c.Exec(ctx, "REFRESH MATERIALIZED VIEW "+mv); err != nil {
				resto, ultimo = append(resto, mv), err
			}
		}
		if len(resto) == len(pendentes) {
			for _, mv := range resto {
				r.d.logf("refresh de %s: %v", mv, ultimo)
			}
			return len(resto), nil
		}
		pendentes = resto
	}
	return 0, nil
}

// conexaoDSN é o DSN de um lado (a ponte dele).
func conexaoDSN(l *lado, banco, app string) string {
	return dsnDe(l, banco, app)
}

// retomavel acha o dump em blocos incompleto mais recente do perfil, que dê para retomar: mesma
// origem (servidor e banco) e mesmo recorte.
func retomavel(dir string, p Plano) (string, string) {
	ms, _ := filepath.Glob(filepath.Join(dir, "*", "manifesto.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(ms)))
	for _, arq := range ms {
		d := filepath.Dir(arq)
		m, err := LerManifesto(d)
		if err != nil || m.Formato != FormatoBlocos || m.Estado == DumpCompleto {
			continue
		}
		if m.Origem.SystemID != p.Origem.SystemID || m.Origem.Banco != p.Origem.Banco {
			continue
		}
		f := Filtros{Schemas: p.Perfil.Schemas, SchemasFora: p.Perfil.SchemasFora, Tabelas: p.Perfil.Tabelas, TabelasFora: p.Perfil.TabelasFora}
		a, _ := json.Marshal(f)
		b, _ := json.Marshal(m.Filtros)
		if string(a) != string(b) {
			continue
		}
		e, err := lerEstado(d)
		if err != nil {
			continue
		}
		prontas := 0
		for _, t := range e.Tabelas {
			if t.Completa {
				prontas++
			}
		}
		return d, fmt.Sprintf("%d de %d tabelas prontas, de %s", prontas, len(e.Tabelas), m.Inicio.Format("02/01 15:04"))
	}
	return "", ""
}

func dsnDe(l *lado, banco, app string) string { return conexao.DSN(l.c, l.ponte, banco, app) }
