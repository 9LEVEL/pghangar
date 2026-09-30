package motor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

// id aspeia um identificador; lit aspeia um literal. Os dois seguem as regras do PostgreSQL com
// standard_conforming_strings ligado (o padrão desde o 9.1, e o pgx recusa o contrário).
func id(s string) string  { return pgx.Identifier{s}.Sanitize() }
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// SQLCriarBanco escreve o CREATE DATABASE do __novo com o locale e o dono pedidos. O provedor por
// banco existe desde o 15; as regras ICU, desde o 16; o builtin, desde o 17.
func SQLCriarBanco(nome, dono string, b cadastro.Banco, versaoDestino int) string {
	var s strings.Builder
	fmt.Fprintf(&s, "CREATE DATABASE %s WITH TEMPLATE template0 OWNER %s ENCODING %s LC_COLLATE %s LC_CTYPE %s",
		id(nome), id(dono), lit(b.Codificacao), lit(b.Collate), lit(b.Ctype))
	if versaoDestino >= 150000 {
		switch b.Provedor {
		case "i":
			s.WriteString(" LOCALE_PROVIDER icu")
			if b.Locale != "" {
				fmt.Fprintf(&s, " ICU_LOCALE %s", lit(b.Locale))
			}
			if b.RegrasICU != "" && versaoDestino >= 160000 {
				fmt.Fprintf(&s, " ICU_RULES %s", lit(b.RegrasICU))
			}
		case "b":
			if versaoDestino >= 170000 {
				s.WriteString(" LOCALE_PROVIDER builtin")
				if b.Locale != "" {
					fmt.Fprintf(&s, " BUILTIN_LOCALE %s", lit(b.Locale))
				}
			}
		default:
			s.WriteString(" LOCALE_PROVIDER libc")
		}
	}
	return s.String()
}

// listaGUC são as variáveis cujo valor é uma lista de identificadores (GUC_LIST_QUOTE no
// PostgreSQL). Numa ALTER DATABASE … SET, cada elemento vai como um literal separado; o valor
// inteiro num literal só viraria um schema chamado "a, b".
var listaGUC = map[string]bool{
	"search_path": true, "temp_tablespaces": true, "session_preload_libraries": true,
	"shared_preload_libraries": true, "local_preload_libraries": true, "unix_socket_directories": true,
}

// DividirGUC separa "a, \"b c\", d" nos elementos, tirando as aspas duplas como o PostgreSQL faz.
func DividirGUC(v string) []string {
	var out []string
	var cur strings.Builder
	aspas := false
	for i := 0; i < len(v); i++ {
		ch := v[i]
		switch {
		case aspas && ch == '"' && i+1 < len(v) && v[i+1] == '"':
			cur.WriteByte('"')
			i++
		case ch == '"':
			aspas = !aspas
		case ch == ',' && !aspas:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" || len(out) > 0 {
		out = append(out, s)
	}
	return out
}

// ValorGUC escreve o valor de um setconfig ("nome=valor") para o SET.
func ValorGUC(nome, valor string) string {
	if listaGUC[strings.ToLower(nome)] {
		partes := DividirGUC(valor)
		for i, p := range partes {
			partes[i] = lit(p)
		}
		if len(partes) == 0 {
			return "''"
		}
		return strings.Join(partes, ", ")
	}
	return lit(valor)
}

// SQLConfiguracao escreve o ALTER que reaplica um setconfig no banco novo (role vazia é o banco
// todo).
func SQLConfiguracao(banco, role, config string) (string, error) {
	i := strings.IndexByte(config, '=')
	if i <= 0 {
		return "", fmt.Errorf("configuração ilegível: %q", config)
	}
	nome, valor := config[:i], config[i+1:]
	if role == "" {
		return fmt.Sprintf("ALTER DATABASE %s SET %s TO %s", id(banco), id(nome), ValorGUC(nome, valor)), nil
	}
	return fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET %s TO %s", id(role), id(banco), id(nome), ValorGUC(nome, valor)), nil
}

// Contagem é o número de objetos por tipo, sem os que pertencem a extensões (esses nascem do
// CREATE EXTENSION dos dois lados). As restrições NOT NULL ficam de fora: a partir do 18 elas
// também aparecem em pg_constraint, e uma cópia 16 → 18 divergiria sem diferença nenhuma.
type Contagem map[string]int64

const consultaContagem = `
WITH ext AS (SELECT classid, objid FROM pg_depend WHERE deptype = 'e'),
     ns AS (SELECT oid FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema')
SELECT 'schemas', count(*) FROM pg_namespace n WHERE n.oid IN (SELECT oid FROM ns)
  AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_namespace'::regclass AND ext.objid = n.oid)
UNION ALL
SELECT CASE c.relkind WHEN 'r' THEN 'tabelas' WHEN 'p' THEN 'tabelas' WHEN 'v' THEN 'visões'
                      WHEN 'm' THEN 'visões materializadas' WHEN 'S' THEN 'sequências' ELSE 'índices' END, count(*)
  FROM pg_class c
 WHERE c.relnamespace IN (SELECT oid FROM ns) AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'i', 'I')
   AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_class'::regclass AND ext.objid = c.oid)
 GROUP BY 1
UNION ALL
SELECT 'funções', count(*) FROM pg_proc p WHERE p.pronamespace IN (SELECT oid FROM ns)
  AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_proc'::regclass AND ext.objid = p.oid)
UNION ALL
SELECT 'restrições', count(*) FROM pg_constraint k WHERE k.connamespace IN (SELECT oid FROM ns) AND k.contype <> 'n'
  AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_class'::regclass AND ext.objid = k.conrelid)
UNION ALL
SELECT 'gatilhos', count(*) FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
 WHERE NOT g.tgisinternal AND c.relnamespace IN (SELECT oid FROM ns)
UNION ALL
SELECT 'extensões', count(*) FROM pg_extension`

// contar conta os objetos. Com filtro de schemas, os dois lados contam o mesmo recorte (o __novo
// nasce com o public do template0, que a origem filtrada não leva).
func contar(ctx context.Context, conn *pgx.Conn, dentro, fora []string) (Contagem, error) {
	q := consultaContagem
	if len(dentro) > 0 || len(fora) > 0 {
		filtro := "nspname !~ '^pg_' AND nspname <> 'information_schema'"
		if len(dentro) > 0 {
			filtro += " AND nspname ~ " + lit(juntarRegex(dentro))
		}
		if len(fora) > 0 {
			filtro += " AND nspname !~ " + lit(juntarRegex(fora))
		}
		q = strings.Replace(q, "nspname !~ '^pg_' AND nspname <> 'information_schema'", filtro, 1)
	}
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	c := Contagem{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		c[k] += n
	}
	return c, rows.Err()
}

func juntarRegex(ps []string) string {
	var rs []string
	for _, p := range ps {
		rs = append(rs, "("+padraoRegex(p)+")")
	}
	return strings.Join(rs, "|")
}

// Diferencas lista o que diverge entre a origem e o destino, em ordem.
func Diferencas(origem, destino Contagem) []string {
	chaves := map[string]bool{}
	for k := range origem {
		chaves[k] = true
	}
	for k := range destino {
		chaves[k] = true
	}
	var ks []string
	for k := range chaves {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []string
	for _, k := range ks {
		if origem[k] != destino[k] {
			out = append(out, fmt.Sprintf("%s: origem %d, destino %d", k, origem[k], destino[k]))
		}
	}
	return out
}

// consultor é o que conta: uma conexão ou a transação do snapshot do dump.
type consultor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// tabelasComDados são as tabelas que guardam linhas (as partições, e não a tabela-mãe), sem as de
// extensões e as de sistema.
const tabelasComDados = `
SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
 ORDER BY 1, 2`

// contarLinhas conta as linhas de cada tabela (chave "schema.tabela").
func contarLinhas(ctx context.Context, c consultor, progresso func(i, n int, tabela string)) (map[string]int64, error) {
	rows, err := c.Query(ctx, tabelasComDados)
	if err != nil {
		return nil, err
	}
	var ts [][2]string
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			rows.Close()
			return nil, err
		}
		ts = append(ts, [2]string{s, t})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r := map[string]int64{}
	for i, t := range ts {
		nome := t[0] + "." + t[1]
		if progresso != nil {
			progresso(i+1, len(ts), nome)
		}
		var n int64
		if err := c.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{t[0], t[1]}.Sanitize()).Scan(&n); err != nil {
			return nil, fmt.Errorf("%s: %w", nome, err)
		}
		r[nome] = n
	}
	return r, nil
}

// padraoRegex converte um padrão do pg_dump (como os do psql: * e ?, aspas para respeitar
// maiúsculas) numa expressão regular inteira. Sem aspas, o nome vai em minúsculas.
func padraoRegex(p string) string {
	var b strings.Builder
	aspas := false
	for _, r := range p {
		switch {
		case r == '"':
			aspas = !aspas
		case r == '*' && !aspas:
			b.WriteString(".*")
		case r == '?' && !aspas:
			b.WriteString(".")
		default:
			c := string(r)
			if !aspas {
				c = strings.ToLower(c)
			}
			b.WriteString(regexp.QuoteMeta(c))
		}
	}
	return "^" + b.String() + "$"
}

// casaTabela diz se "schema.tabela" casa com um padrão de tabela do pg_dump (sem ponto, vale
// qualquer schema).
func casaTabela(padrao, schema, tabela string) bool {
	ps, pt := "*", padrao
	if i := indicePonto(padrao); i >= 0 {
		ps, pt = padrao[:i], padrao[i+1:]
	}
	rs, e1 := regexp.Compile(padraoRegex(ps))
	rt, e2 := regexp.Compile(padraoRegex(pt))
	return e1 == nil && e2 == nil && rs.MatchString(schema) && rt.MatchString(tabela)
}

// indicePonto acha o ponto que separa schema e tabela, fora de aspas.
func indicePonto(p string) int {
	aspas := false
	for i, r := range p {
		switch {
		case r == '"':
			aspas = !aspas
		case r == '.' && !aspas:
			return i
		}
	}
	return -1
}

// casaSchema diz se o schema casa com algum dos padrões.
func casaSchema(padroes []string, schema string) bool {
	for _, p := range padroes {
		if re, err := regexp.Compile(padraoRegex(p)); err == nil && re.MatchString(schema) {
			return true
		}
	}
	return false
}

// DiferencasLinhas compara as linhas do __novo com as da origem. Só as tabelas que existem no
// __novo contam (um perfil com filtro leva só parte). Uma tabela "sem dados" tem que vir vazia.
func DiferencasLinhas(origem, destino map[string]int64, semDados []string) []string {
	var ks []string
	for k := range destino {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []string
	for _, k := range ks {
		o, ok := origem[k]
		if !ok {
			continue
		}
		i := indicePonto(k)
		esperado := o
		for _, p := range semDados {
			if casaTabela(p, k[:i], k[i+1:]) {
				esperado = 0
			}
		}
		if destino[k] != esperado {
			out = append(out, fmt.Sprintf("linhas de %s: origem %d, destino %d", k, esperado, destino[k]))
		}
	}
	return out
}
