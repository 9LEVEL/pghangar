// pghangar: copia bancos PostgreSQL de um servidor para outro por dump e restore, com os
// clientes oficiais em containers. Ver docs/ESTRATEGIA.md.
//
//	pghangar [--dir D]                                   a tela
//	pghangar executar --dir D --execucao N [--trocar]    o processo de uma cópia (a tela sobe)
//	pghangar versao
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/execucao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/trava"
	"github.com/9LEVEL/pghangar/internal/tui"
)

// versao é gravada no build (-ldflags "-X main.versao=v0.1.0"); sem ela, vale o commit.
var versao = ""

func Versao() string {
	if versao != "" {
		return versao
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, sujo := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				sujo = s.Value == "true"
			}
		}
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if rev != "" {
			if sujo {
				rev += "+"
			}
			return "dev-" + rev
		}
	}
	return "dev"
}

func main() {
	if err := rodar(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pghangar:", err)
		var s *saida
		if errors.As(err, &s) {
			os.Exit(s.codigo)
		}
		os.Exit(1)
	}
}

// saida é um fim com código próprio: 2 é "a troca espera a decisão" (para o cron distinguir).
type saida struct {
	codigo int
	msg    string
}

func (s *saida) Error() string { return s.msg }

func rodar(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "versao", "--versao", "version", "--version":
			fmt.Println("pghangar", Versao())
			return nil
		case "ajuda", "--ajuda", "-h", "--help", "help":
			fmt.Print(ajuda)
			return nil
		case "executar":
			return executar(args[1:])
		case "rodar":
			return rodarPerfil(args[1:])
		}
	}
	fs := flag.NewFlagSet("pghangar", flag.ContinueOnError)
	dir := fs.String("dir", local.Padrao, "o diretório da ferramenta")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("comando desconhecido: %s (veja pghangar ajuda)", fs.Arg(0))
	}
	d, err := dirDe(*dir)
	if err != nil {
		return err
	}
	return tela(d)
}

// dirDe é o diretório da ferramenta em caminho absoluto: os volumes do Docker e o socket do túnel
// não aceitam caminho relativo.
func dirDe(s string) (local.Dir, error) {
	abs, err := filepath.Abs(s)
	if err != nil {
		return local.Dir{}, fmt.Errorf("--dir %s: %w", s, err)
	}
	return local.Dir{Raiz: abs}, nil
}

const ajuda = `pghangar: copia bancos PostgreSQL (16, 17, 18) por dump e restore, em containers.

  pghangar [--dir D]     abre a tela (o padrão de D é /var/lib/pghangar)
  pghangar rodar [--dir D] [--confirmar BANCO] PERFIL
                            copia sem a tela (para o cron): num destino homolog, --confirmar
                            com o nome do banco é obrigatório. Nunca apaga anteriores.
                            Sai com 0 (ok), 2 (a troca espera a decisão, pela tela) ou 1 (erro)
  pghangar versao        mostra a versão
  pghangar ajuda         esta ajuda

Roda como root. O subcomando "executar" é o processo de uma cópia: quem o sobe é a tela.
`

func abrir(d local.Dir) (*cadastro.Cadastro, error) {
	if err := local.ExigirRoot(); err != nil {
		return nil, err
	}
	if err := d.Preparar(); err != nil {
		return nil, err
	}
	return cadastro.Abrir(d.Estado())
}

func deps(d local.Dir, cad *cadastro.Cadastro, seg conexao.Segredos, log io.Writer) motor.Deps {
	host, _ := os.Hostname()
	return motor.Deps{Cadastro: cad, Dir: d, Docker: imagens.Docker{}, Amb: ambiente(d), Seg: seg, Log: log, Maquina: host}
}

func ambiente(d local.Dir) conexao.Ambiente {
	a := conexao.Ambiente{KnownHosts: d.KnownHosts(), DirTemp: d.Temp()}
	if _, err := os.Stat(d.ChaveSSH()); err == nil {
		a.ChavePadrao = d.ChaveSSH()
	}
	return a
}

func executar(args []string) error {
	fs := flag.NewFlagSet("executar", flag.ContinueOnError)
	dir := fs.String("dir", local.Padrao, "o diretório da ferramenta")
	id := fs.Int64("execucao", 0, "o id da execução")
	grupo := fs.String("grupo", "", "roda as cópias de um grupo, em fila")
	trocar := fs.Bool("trocar", false, "só a troca de uma execução que aguarda a decisão")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 && *grupo == "" {
		return errors.New("informe --execucao ou --grupo")
	}
	d, err := dirDe(*dir)
	if err != nil {
		return err
	}
	cad, err := abrir(d)
	if err != nil {
		return err
	}
	defer cad.Fechar()
	seg, err := execucao.LerSegredos(os.Stdin)
	if err != nil {
		return fmt.Errorf("lendo os segredos: %w", err)
	}
	// A tela pode fechar e o SSH cair: o processo segue. Só o cancelamento (SIGTERM) o para.
	signal.Ignore(syscall.SIGHUP)
	ctx, parar := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer parar()
	abrirLog := func(n int64) (io.WriteCloser, error) {
		return os.OpenFile(d.Log(n), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if *grupo != "" {
		return execucao.TrabalharGrupo(ctx, deps(d, cad, seg, nil), *grupo, abrirLog)
	}
	log, err := abrirLog(*id)
	if err != nil {
		return err
	}
	defer log.Close()
	fmt.Fprintf(log, "pghangar %s: processo %d da execução %d\n", Versao(), os.Getpid(), *id)
	return execucao.Trabalhar(ctx, deps(d, cad, seg, log), *id, *trocar)
}

func tela(d local.Dir) error {
	cad, err := abrir(d)
	if err != nil {
		return err
	}
	defer cad.Fechar()
	lanc := execucao.Lancador{Cadastro: cad, Dir: d}
	_ = execucao.Conferir(context.Background(), cad, d)
	return tui.Rodar(tui.Opcoes{Versao: Versao(), Servicos: tui.ServicosReais(cad, d, lanc, func(seg conexao.Segredos) motor.Deps {
		return deps(d, cad, seg, nil)
	})})
}

// rodarPerfil copia sem a tela, no primeiro plano: é o que o cron (ou um timer do systemd) chama.
// Não pergunta nada: o que precisaria de pergunta (senha a cada sessão, passphrase, servidor SSH
// desconhecido) vira erro, e se resolve pela tela.
func rodarPerfil(args []string) error {
	fs := flag.NewFlagSet("rodar", flag.ContinueOnError)
	dir := fs.String("dir", local.Padrao, "o diretório da ferramenta")
	confirmar := fs.String("confirmar", "", "o nome do banco de destino (obrigatório num destino homolog)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("use: pghangar rodar [--confirmar BANCO] PERFIL")
	}
	nome := fs.Arg(0)
	d, err := dirDe(*dir)
	if err != nil {
		return err
	}
	cad, err := abrir(d)
	if err != nil {
		return err
	}
	defer cad.Fechar()
	// Sem a tela aberta, ninguém mais limpa o que um rodar morto deixou (a execução "rodando", o
	// pgpass): esta chamada limpa.
	_ = execucao.Conferir(context.Background(), cad, d)
	signal.Ignore(syscall.SIGHUP)
	ctx, parar := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer parar()

	dp := deps(d, cad, conexao.Segredos{}, nil)
	p, err := motor.Planejar(ctx, dp, nome)
	var pg *motor.Pergunta
	if errors.As(err, &pg) {
		switch {
		case pg.HostDesconhecido != nil:
			return fmt.Errorf("o servidor SSH %s ainda não é conhecido: aceite a chave dele pela tela (aba 3, tecla t) antes de agendar", pg.HostDesconhecido.Endereco)
		case pg.Frase != "":
			return fmt.Errorf("a chave %s tem passphrase: sem a tela, não há a quem perguntar. Use a chave da ferramenta ou o ssh-agent", pg.Frase)
		default:
			return fmt.Errorf("a conexão %s pede a senha a cada sessão: para rodar sem a tela, use o modo guardar ou pgpass", pg.Conexao)
		}
	}
	if err != nil {
		return err
	}
	// Sem a tela, ninguém disse sim às correções: nenhuma é aplicada.
	p.DesmarcarCorrecoes()
	if p.Bloqueado() {
		return fmt.Errorf("bloqueado:\n  - %s", strings.Join(p.BloqueiosAtivos(), "\n  - "))
	}
	for _, c := range p.Correcoes {
		if !c.Bloqueia {
			fmt.Println("aviso (correção só pela tela):", c.SeNao)
		}
	}
	if c := p.Confirmacao(); c != "" && *confirmar != c {
		return fmt.Errorf("o destino é homolog: para substituir o banco %s sem a tela, passe --confirmar %s", c, c)
	}
	for _, a := range p.Avisos {
		fmt.Println("aviso:", a)
	}
	for _, a := range p.Notas {
		fmt.Println("nota:", a)
	}
	t, err := trava.Obter(d.Travas(), trava.Destino(p.Perfil.Destino, p.Destino.SystemID), p.Perfil.DestinoBanco)
	if err != nil {
		return err
	}
	defer t.Soltar()
	pj, _ := json.Marshal(p)
	id, err := cad.NovaExecucao(ctx, cadastro.Execucao{Perfil: nome, Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoIniciando,
		Destino: p.Perfil.Destino, Banco: p.Perfil.DestinoBanco, Plano: string(pj), Operador: execucao.Operador() + " (rodar)"})
	if err != nil {
		return err
	}
	log, err := os.OpenFile(d.Log(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	fmt.Printf("execução #%d de %s: log em %s\n", id, nome, d.Log(id))
	dp.Log, dp.TravaObtida = log, true
	_ = motor.Executar(ctx, dp, id)
	e, err := cad.Execucao(context.Background(), id)
	if err != nil {
		return err
	}
	fmt.Printf("execução #%d: %s — %s\n", id, e.Estado, e.Mensagem)
	// Os anteriores que ficaram: os de antes e o desta cópia, se ela trocou.
	anteriores, total := len(p.Anteriores), int64(0)
	for _, a := range p.Anteriores {
		total += a.Tamanho
	}
	if e.BancoAnterior != "" {
		anteriores++
		total += p.Destino.Info.Tamanho
	}
	if anteriores > 0 {
		fmt.Printf("lembrete: %d banco(s) __anterior de %s ocupam %s no destino; sem a tela, nada é apagado (aba 5)\n", anteriores, p.Perfil.DestinoBanco, motor.Tamanho(total))
	}
	switch e.Estado {
	case cadastro.EstadoOK:
		return nil
	case cadastro.EstadoAguardando:
		return &saida{codigo: 2, msg: "a troca espera a sua decisão: abra a tela (aba 2)"}
	}
	return &saida{codigo: 1, msg: "a cópia não terminou: " + e.Estado}
}
