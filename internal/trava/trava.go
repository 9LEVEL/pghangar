// Package trava garante uma cópia por destino (docs/ESTRATEGIA.md §11): um flock num arquivo por
// conexão e banco de destino. O kernel solta a trava quando o processo morre, então uma execução
// que caiu não deixa o destino preso.
package trava

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrOcupado é o destino com outra cópia (ou troca) em andamento.
var ErrOcupado = errors.New("há outra cópia em andamento para este destino")

// Trava é uma trava obtida.
type Trava struct{ f *os.File }

// Destino é a chave do servidor de destino: o system_identifier, quando se sabe, e o nome da conexão
// só na falta dele. Duas conexões para o mesmo servidor, ou uma conexão renomeada no meio de uma
// cópia, disputam a mesma trava.
func Destino(conexao, systemID string) string {
	if systemID != "" {
		return "sid:" + systemID
	}
	return "con:" + conexao
}

func Arquivo(dir, destino, banco string) string {
	s := sha256.Sum256([]byte(destino + "\x00" + banco))
	return filepath.Join(dir, hex.EncodeToString(s[:12])+".lock")
}

// Obter pega a trava sem esperar.
func Obter(dir, destino, banco string) (*Trava, error) {
	f, err := os.OpenFile(Arquivo(dir, destino, banco), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrOcupado, banco)
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Trava{f: f}, nil
}

// Livre diz se ninguém segura a trava agora.
func Livre(dir, destino, banco string) bool {
	t, err := Obter(dir, destino, banco)
	if err != nil {
		return false
	}
	t.Soltar()
	return true
}

func (t *Trava) Soltar() {
	if t != nil && t.f != nil {
		_ = syscall.Flock(int(t.f.Fd()), syscall.LOCK_UN)
		_ = t.f.Close()
		t.f = nil
	}
}
