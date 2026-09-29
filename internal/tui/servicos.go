package tui

import (
	"context"
	"io"
	"os"

	"github.com/9LEVEL/copia-banco/internal/cadastro"
	"github.com/9LEVEL/copia-banco/internal/conexao"
	"github.com/9LEVEL/copia-banco/internal/execucao"
	"github.com/9LEVEL/copia-banco/internal/imagens"
	"github.com/9LEVEL/copia-banco/internal/local"
	"github.com/9LEVEL/copia-banco/internal/motor"
	"github.com/9LEVEL/copia-banco/internal/tunel"
)

// Docker é o que a tela usa do Docker (a aba Ambiente).
type Docker interface {
	Versao(ctx context.Context) (string, error)
	Existe(ctx context.Context, ref string) bool
	Baixar(ctx context.Context, ref string, saida io.Writer) error
	Digest(ctx context.Context, ref string) (string, error)
	VersaoCliente(ctx context.Context, imagem string) (string, error)
	Rodando(ctx context.Context, instancia string) ([]imagens.Container, error)
	Parar(ctx context.Context, nome string) error
}

// Servicos é o que a tela usa de fora. Vem pronto do main; os testes trocam por falsos.
type Servicos struct {
	Cadastro *cadastro.Cadastro
	Dir      local.Dir
	Docker   Docker

	Planejar            func(ctx context.Context, perfil string, seg conexao.Segredos) (motor.Plano, error)
	PlanejarRestauracao func(ctx context.Context, dumpDir string, seg conexao.Segredos) (motor.Plano, error)
	PlanejarReset       func(ctx context.Context, perfil string, seg conexao.Segredos) (motor.Plano, error)
	Diagnosticar        func(ctx context.Context, c cadastro.Conexao, seg conexao.Segredos) conexao.Diagnostico
	Listar              func(ctx context.Context, conexao, banco string, seg conexao.Segredos) (motor.DaFerramenta, error)
	Desfazer            func(ctx context.Context, conexao, banco, anterior string, seg conexao.Segredos) (string, error)
	Apagar              func(ctx context.Context, conexao, banco, nome string, seg conexao.Segredos) error

	Iniciar      func(ctx context.Context, p execucao.Pedido) (int64, error)
	IniciarGrupo func(ctx context.Context, planos []motor.Plano, seg conexao.Segredos) (string, []int64, error)
	Trocar       func(ctx context.Context, id int64, seg conexao.Segredos) error
	Descartar    func(ctx context.Context, id int64) error
	Cancelar     func(e cadastro.Execucao) error
	Conferir     func(ctx context.Context) error

	AceitarHost func(hd *tunel.HostDesconhecido) error
	GerarChave  func() error
	LerLog      func(id int64) string
}

// ServicosReais liga a tela ao motor, ao Docker e ao processo das execuções.
func ServicosReais(cad *cadastro.Cadastro, d local.Dir, l execucao.Lancador, deps func(conexao.Segredos) motor.Deps) Servicos {
	amb := func() conexao.Ambiente { return deps(conexao.Segredos{}).Amb }
	host, _ := os.Hostname()
	return Servicos{
		Cadastro: cad, Dir: d, Docker: imagens.Docker{},
		Planejar: func(ctx context.Context, perfil string, seg conexao.Segredos) (motor.Plano, error) {
			return motor.Planejar(ctx, deps(seg), perfil)
		},
		PlanejarRestauracao: func(ctx context.Context, dir string, seg conexao.Segredos) (motor.Plano, error) {
			return motor.PlanejarRestauracao(ctx, deps(seg), dir)
		},
		PlanejarReset: func(ctx context.Context, perfil string, seg conexao.Segredos) (motor.Plano, error) {
			return motor.PlanejarReset(ctx, deps(seg), perfil)
		},
		Diagnosticar: func(ctx context.Context, c cadastro.Conexao, seg conexao.Segredos) conexao.Diagnostico {
			return conexao.Diagnosticar(ctx, c, amb(), seg)
		},
		Listar: func(ctx context.Context, c, b string, seg conexao.Segredos) (motor.DaFerramenta, error) {
			return motor.Listar(ctx, deps(seg), c, b)
		},
		Desfazer: func(ctx context.Context, c, b, a string, seg conexao.Segredos) (string, error) {
			return motor.Desfazer(ctx, deps(seg), c, b, a)
		},
		Apagar: func(ctx context.Context, c, b, n string, seg conexao.Segredos) error {
			return motor.Apagar(ctx, deps(seg), c, b, n)
		},
		Iniciar:      l.Iniciar,
		IniciarGrupo: l.IniciarGrupo,
		Trocar:       l.Trocar,
		Descartar:    l.Descartar,
		Cancelar:     execucao.Cancelar,
		Conferir:     func(ctx context.Context) error { return execucao.Conferir(ctx, cad, d) },
		AceitarHost:  func(hd *tunel.HostDesconhecido) error { return tunel.Aceitar(d.KnownHosts(), hd) },
		GerarChave:   func() error { return tunel.GerarChave(d.ChaveSSH(), "copia-banco@"+host) },
		LerLog:       func(id int64) string { return lerFim(d.Log(id), 512*1024) },
	}
}

// lerFim lê o fim de um arquivo (o log pode ser grande: milhares de linhas do pg_restore).
func lerFim(caminho string, limite int64) string {
	f, err := os.Open(caminho)
	if err != nil {
		return "(sem log: " + err.Error() + ")"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	ini := int64(0)
	if fi.Size() > limite {
		ini = fi.Size() - limite
	}
	b := make([]byte, fi.Size()-ini)
	if _, err := f.ReadAt(b, ini); err != nil && err != io.EOF {
		return err.Error()
	}
	s := string(b)
	if ini > 0 {
		s = "(… o começo do log foi omitido)\n" + s[indiceLinha(s):]
	}
	return s
}

func indiceLinha(s string) int {
	for i, c := range s {
		if c == '\n' {
			return i + 1
		}
	}
	return 0
}
