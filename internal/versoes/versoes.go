// Package versoes guarda a regra de versões da cópia (docs/ESTRATEGIA.md §5): uma imagem só, a da
// maior versão entre a origem e o destino, faz o dump e o restore; copiar descendo é bloqueado.
//
// Também guarda, num lugar só, a versão mínima do cliente para cada opção que o motor usa. Um
// teste garante que toda opção usada está aqui.
package versoes

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Padrao são as versões com imagem, enquanto o cadastro não disser outras.
var Padrao = []int{16, 17, 18}

// Major tira a versão principal do server_version_num (180006 → 18).
func Major(num int) int { return num / 10000 }

// Texto é o server_version_num legível (180006 → "18.6").
func Texto(num int) string {
	if num <= 0 {
		return "?"
	}
	return fmt.Sprintf("%d.%d", num/10000, num%10000)
}

// ErrDescendo é a cópia de uma versão mais nova para uma mais antiga.
var ErrDescendo = errors.New("copiar descendo de versão é bloqueado")

// Escolher devolve a versão da imagem que faz o dump e o restore: a maior entre as duas. Descer é
// bloqueado: um pg_restore mais antigo não lê o arquivo de um pg_dump mais novo, e um dump não tem
// garantia de carregar numa versão mais antiga.
func Escolher(origem, destino int, disponiveis []int) (int, error) {
	if origem <= 0 || destino <= 0 {
		return 0, errors.New("versão desconhecida: verifique as conexões")
	}
	if destino < origem {
		return 0, fmt.Errorf("%w: a origem é %d e o destino é %d", ErrDescendo, origem, destino)
	}
	img := max(origem, destino)
	for _, v := range disponiveis {
		if v == img {
			return img, nil
		}
	}
	return 0, fmt.Errorf("não há imagem configurada para a versão %d (configuradas: %s)", img, Lista(disponiveis))
}

// Lista escreve as versões como "16, 17, 18".
func Lista(vs []int) string {
	s := make([]string, len(vs))
	for i, v := range vs {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ", ")
}

// LerLista lê "16,17, 18" em ordem crescente, sem repetição.
func LerLista(s string) ([]int, error) {
	vistos := map[int]bool{}
	var vs []int
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		v, err := strconv.Atoi(p)
		if err != nil || v < 10 || v > 99 {
			return nil, fmt.Errorf("versão inválida: %q", p)
		}
		if !vistos[v] {
			vistos[v] = true
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return nil, errors.New("informe ao menos uma versão")
	}
	sort.Ints(vs)
	return vs, nil
}

// Opcao é uma opção de linha de comando dos clientes, com a versão em que ela apareceu.
type Opcao struct {
	Programa string // pg_dump, pg_restore, vacuumdb, psql
	Nome     string
	Desde    int
}

// Opcoes é a tabela única das opções que o motor usa. Uma opção fora dela é recusada.
var Opcoes = []Opcao{
	{"pg_dump", "--format=directory", 9},
	{"pg_dump", "--format=custom", 9},
	{"pg_dump", "--jobs", 9},
	{"pg_dump", "--verbose", 9},
	{"pg_dump", "--exclude-table-data", 9},
	{"pg_dump", "--file", 9},
	{"pg_dump", "--dbname", 9},
	{"pg_dump", "--compress", 16}, // com método (zstd, lz4): 16+
	{"pg_dump", "--schema", 9},
	{"pg_dump", "--exclude-schema", 9},
	{"pg_dump", "--table", 9},
	{"pg_dump", "--exclude-table", 9},
	{"pg_dump", "--extension", 14},
	{"pg_dump", "--snapshot", 9},
	{"pg_dump", "--schema-only", 9},
	{"pg_restore", "--section", 9},
	{"pg_restore", "--jobs", 9},
	{"pg_restore", "--verbose", 9},
	{"pg_restore", "--no-owner", 9},
	{"pg_restore", "--no-privileges", 9},
	{"pg_restore", "--no-tablespaces", 9},
	{"pg_restore", "--no-subscriptions", 11},
	{"pg_restore", "--no-publications", 11},
	{"pg_restore", "--role", 9},
	{"pg_restore", "--dbname", 9},
	{"pg_restore", "--list", 9},
	{"vacuumdb", "--analyze-only", 9},
	{"vacuumdb", "--jobs", 9},
	{"vacuumdb", "--dbname", 9},
	{"psql", "--no-psqlrc", 9},
	{"psql", "--quiet", 9},
	{"psql", "--set=ON_ERROR_STOP=1", 9},
	{"psql", "--single-transaction", 9},
	{"psql", "--file", 9},
	{"psql", "--dbname", 9},
}

// Suporta diz se o cliente da versão v tem a opção. Só o nome conta: "--jobs=4" é "--jobs".
func Suporta(programa, opcao string, v int) bool {
	// Primeiro a forma exata (--format=directory, --set=ON_ERROR_STOP=1); depois só o nome.
	for _, o := range Opcoes {
		if o.Programa == programa && o.Nome == opcao {
			return v >= o.Desde
		}
	}
	nome := opcao
	if i := strings.IndexByte(opcao, '='); i > 0 {
		nome = opcao[:i]
	}
	for _, o := range Opcoes {
		if o.Programa == programa && o.Nome == nome {
			return v >= o.Desde
		}
	}
	return false
}

// Conferir recusa um comando com alguma opção que o cliente não tem (ou que não está na tabela).
func Conferir(programa string, args []string, v int) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if !Suporta(programa, a, v) {
			return fmt.Errorf("%s %d não tem a opção %s (ou ela não está em versoes.Opcoes)", programa, v, a)
		}
	}
	return nil
}
