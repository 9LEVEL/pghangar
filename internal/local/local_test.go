package local

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepararCriaComPermissoesFechadas(t *testing.T) {
	d := Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := d.Preparar(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Raiz, d.Chaves(), d.Dumps(), d.Logs(), d.Travas(), d.Temp()} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", p, fi.Mode().Perm(), err)
		}
	}
}

func TestPrepararRecusaPermissoesFrouxas(t *testing.T) {
	d := Dir{Raiz: filepath.Join(t.TempDir(), "cb")}
	if err := d.Preparar(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d.Dumps(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Preparar(); err == nil {
		t.Fatal("dumps com 755 deveria ser recusado")
	}
	_ = os.Chmod(d.Dumps(), 0o700)
	if err := os.WriteFile(d.Estado(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Preparar(); err == nil {
		t.Fatal("estado.db com 644 deveria ser recusado")
	}
}

func TestDentro(t *testing.T) {
	casos := map[string]bool{
		"/var/lib/cb/dumps/p/1":   true,
		"/var/lib/cb/dumps":       false,
		"/var/lib/cb/dumps/../x":  false,
		"/var/lib/cb/dumpsx/p":    false,
		"/var/lib/cb/dumps/p/../": false,
		"/var/lib/cb/dumps/.x":    true,
		"/etc":                    false,
	}
	for c, esperado := range casos {
		if Dentro("/var/lib/cb/dumps", c) != esperado {
			t.Errorf("Dentro(%q) deveria ser %v", c, esperado)
		}
	}
}
