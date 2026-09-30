// Package nomes dá os nomes dos bancos que a ferramenta cria no destino: o <banco>__novo, onde o
// restore acontece, e o <banco>__anterior_<AAAAMMDD_HHMMSS>, que guarda o destino substituído.
//
// O PostgreSQL corta identificadores em 63 bytes. Quando o nome do banco mais o sufixo passam
// disso, a base é encurtada com um hash curto do nome original: a mesma entrada dá sempre a mesma
// base, e a busca pelos anteriores de um banco continua exata.
package nomes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// Limite é o maior identificador do PostgreSQL (NAMEDATALEN - 1).
	Limite = 63

	SufixoNovo     = "__novo"
	MarcaAnterior  = "__anterior_"
	FormatoData    = "20060102_150405"
	tamanhoData    = len(FormatoData)
	tamanhoHash    = 6
	maiorSufixo    = len(MarcaAnterior) + tamanhoData // o anterior é o sufixo mais longo
	espacoDaBase   = Limite - maiorSufixo
	baseEncurtada  = espacoDaBase - 1 - tamanhoHash // "_" + hash
	prefixoInterno = "pghangar_"
)

// Base é o começo dos nomes derivados de um banco: o próprio nome, ou ele encurtado com hash.
func Base(banco string) string {
	if len(banco) <= espacoDaBase {
		return banco
	}
	soma := sha256.Sum256([]byte(banco))
	return cortar(banco, baseEncurtada) + "_" + hex.EncodeToString(soma[:])[:tamanhoHash]
}

// Novo é o banco onde o restore acontece.
func Novo(banco string) string { return Base(banco) + SufixoNovo }

// SufixoBase marca o banco base: a cópia guardada para resetar o destino sem ir à origem.
const SufixoBase = "__base"

// BancoBase é o banco base do destino.
func BancoBase(banco string) string { return Base(banco) + SufixoBase }

// BaseVelha é a base anterior enquanto a nova é criada: ela só é apagada depois.
func BaseVelha(banco string) string { return Base(banco) + SufixoBase + "_velha" }

// Anterior é o nome que o destino ganha na troca.
func Anterior(banco string, quando time.Time) string {
	return Base(banco) + MarcaAnterior + quando.Format(FormatoData)
}

// PrefixoAnteriores é o começo de todos os anteriores de um banco (para starts_with no catálogo).
func PrefixoAnteriores(banco string) string { return Base(banco) + MarcaAnterior }

// DataDoAnterior lê a data do nome de um anterior do banco. ok é falso se o nome não for um.
func DataDoAnterior(banco, nome string) (time.Time, bool) {
	p := PrefixoAnteriores(banco)
	if !strings.HasPrefix(nome, p) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(FormatoData, nome[len(p):], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// EhDaFerramenta diz se o nome é um __novo ou um __anterior do banco: só esses a ferramenta aceita
// apagar ou trocar.
func EhDaFerramenta(banco, nome string) bool {
	if nome == Novo(banco) || nome == BancoBase(banco) || nome == BaseVelha(banco) {
		return true
	}
	_, ok := DataDoAnterior(banco, nome)
	return ok
}

// RoleTemporaria é a role que faz o restore e depois passa tudo ao dono do destino. Leva a
// instância (cada instalação da ferramenta tem a sua): duas instalações apontando para o mesmo
// servidor nunca mexem na role uma da outra.
func RoleTemporaria(instancia string, execucao int64) string {
	return fmt.Sprintf("%s%s_%d", prefixoInterno, instancia, execucao)
}

// PrefixoRoles é o começo das roles temporárias da instância.
func PrefixoRoles(instancia string) string { return prefixoInterno + instancia + "_" }

// EhRoleTemporaria diz se o nome é exatamente o de uma role temporária da instância.
func EhRoleTemporaria(instancia, nome string) bool {
	p := PrefixoRoles(instancia)
	if instancia == "" || !strings.HasPrefix(nome, p) || len(nome) == len(p) {
		return false
	}
	for _, c := range nome[len(p):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// cortar corta em n bytes sem partir um caractere UTF-8.
func cortar(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
