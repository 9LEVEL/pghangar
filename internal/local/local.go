// Package local cuida do diretório da ferramenta (docs/ESTRATEGIA.md §13): /var/lib/pghangar,
// ou o que --dir disser. Tudo fica dentro dele, e a ferramenta se recusa a abrir com permissões
// frouxas, como o OpenSSH: ali estão senhas de superusuário e dados de produção.
package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Padrao é o diretório quando --dir não é informado.
const Padrao = "/var/lib/pghangar"

type Dir struct{ Raiz string }

func (d Dir) Estado() string     { return filepath.Join(d.Raiz, "estado.db") }
func (d Dir) Chaves() string     { return filepath.Join(d.Raiz, "chaves") }
func (d Dir) ChaveSSH() string   { return filepath.Join(d.Chaves(), "id_ed25519") }
func (d Dir) KnownHosts() string { return filepath.Join(d.Raiz, "known_hosts") }
func (d Dir) Dumps() string      { return filepath.Join(d.Raiz, "dumps") }
func (d Dir) Entrada() string    { return filepath.Join(d.Raiz, "entrada") }
func (d Dir) Logs() string       { return filepath.Join(d.Raiz, "logs") }
func (d Dir) Travas() string     { return filepath.Join(d.Raiz, "travas") }
func (d Dir) Temp() string       { return filepath.Join(d.Raiz, "tmp") }

func (d Dir) Log(execucao int64) string {
	return filepath.Join(d.Logs(), fmt.Sprintf("%d.log", execucao))
}

// Preparar cria o que falta (700) e confere o que já existe: dono igual a quem roda, e nada
// legível por grupo ou outros.
func (d Dir) Preparar() error {
	if d.Raiz == "" {
		return errors.New("diretório da ferramenta vazio: informe --dir")
	}
	abs, err := filepath.Abs(d.Raiz)
	if err != nil {
		return err
	}
	d.Raiz = abs
	for _, p := range []string{d.Raiz, d.Chaves(), d.Dumps(), d.Entrada(), d.Logs(), d.Travas(), d.Temp()} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("criando %s: %w", p, err)
		}
		if err := Conferir(p); err != nil {
			return err
		}
	}
	for _, p := range []string{d.Estado(), d.KnownHosts(), d.ChaveSSH()} {
		if _, err := os.Stat(p); err == nil {
			if err := Conferir(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// Conferir recusa um arquivo ou diretório de outro dono, ou acessível por grupo ou outros.
func Conferir(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s pertence a outro usuário (uid %d): a ferramenta só abre o que é de quem a roda", p, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		modo := "600"
		if fi.IsDir() {
			modo = "700"
		}
		return fmt.Errorf("permissões frouxas em %s (%04o): rode `chmod %s %s`", p, fi.Mode().Perm(), modo, p)
	}
	return nil
}

// ExigirRoot recusa rodar sem root: a ferramenta é de sysadmin (docs/DECISOES.md).
func ExigirRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("o pghangar roda como root (é uma ferramenta de sysadmin): use sudo")
	}
	return nil
}

// Dentro diz se o caminho está dentro da raiz: a ferramenta só apaga dumps dentro do diretório
// deles.
func Dentro(raiz, caminho string) bool {
	r, err1 := filepath.Abs(raiz)
	c, err2 := filepath.Abs(caminho)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(r, c)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
