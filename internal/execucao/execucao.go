// Package execucao roda a cópia num processo separado da tela (docs/ESTRATEGIA.md §11): o mesmo
// binário, no subcomando "executar", solto do terminal (setsid). Fechar a tela, ou perder o SSH até
// o servidor de desenvolvimento, não interrompe a cópia.
//
// As senhas e passphrases informadas na hora vão para o processo pela entrada padrão, em JSON:
// nunca pela linha de comando, pelo ambiente ou pelo disco.
package execucao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/trava"
)

// Pedido é uma cópia confirmada.
type Pedido struct {
	Plano    motor.Plano
	Apagar   []string // os anteriores que o sysadmin marcou
	Segredos conexao.Segredos
}

// Lancador sobe o processo da execução.
type Lancador struct {
	Cadastro *cadastro.Cadastro
	Dir      local.Dir
	Binario  string // o executável; vazio é o próprio
}

func (l Lancador) binario() (string, error) {
	if l.Binario != "" {
		return l.Binario, nil
	}
	return os.Executable()
}

// Iniciar registra a execução e sobe o processo. Devolve o id.
func (l Lancador) Iniciar(ctx context.Context, p Pedido) (int64, error) {
	pl := p.Plano
	if pl.Bloqueado() {
		return 0, errors.New("o plano está bloqueado")
	}
	if !trava.Livre(l.Dir.Travas(), trava.Destino(pl.Perfil.Destino, pl.Destino.SystemID), pl.Perfil.DestinoBanco) {
		return 0, trava.ErrOcupado
	}
	pj, err := json.Marshal(pl)
	if err != nil {
		return 0, err
	}
	tipo := cadastro.TipoCopia
	switch {
	case pl.DumpGuardado != "":
		tipo = cadastro.TipoRestauracao
	case pl.Reset:
		tipo = cadastro.TipoReset
	case pl.Arquivo != nil:
		tipo = cadastro.TipoArquivo
	}
	id, err := l.Cadastro.NovaExecucao(ctx, cadastro.Execucao{
		Perfil: pl.Perfil.Nome, Tipo: tipo, Estado: cadastro.EstadoIniciando,
		Destino: pl.Perfil.Destino, Banco: pl.Perfil.DestinoBanco, Apagar: p.Apagar, Plano: string(pj),
		Operador: Operador(), DumpDir: pl.DumpGuardado,
	})
	if err != nil {
		return 0, err
	}
	if err := l.lancar(id, false, p.Segredos); err != nil {
		e, _ := l.Cadastro.Execucao(ctx, id)
		e.Estado, e.Fim, e.Mensagem = cadastro.EstadoErro, time.Now(), "o processo da execução não subiu: "+err.Error()
		_ = l.Cadastro.GravarExecucao(ctx, e)
		return id, err
	}
	return id, nil
}

// IniciarGrupo registra as cópias de um grupo, na fila, e sobe um processo que as roda uma de cada
// vez (para não carregar a origem com vários dumps ao mesmo tempo). Os anteriores não são apagados
// num grupo: a pergunta fica para cada cópia sozinha.
func (l Lancador) IniciarGrupo(ctx context.Context, planos []motor.Plano, seg conexao.Segredos) (string, []int64, error) {
	if len(planos) == 0 {
		return "", nil, errors.New("o grupo está vazio")
	}
	destinos := map[string]bool{}
	for _, pl := range planos {
		if pl.Bloqueado() {
			return "", nil, fmt.Errorf("o perfil %s está bloqueado", pl.Perfil.Nome)
		}
		k := trava.Destino(pl.Perfil.Destino, pl.Destino.SystemID) + "\x00" + pl.Perfil.DestinoBanco
		if destinos[k] {
			return "", nil, fmt.Errorf("dois perfis do grupo escrevem no mesmo banco (%s): tire um deles", pl.Perfil.DestinoBanco)
		}
		destinos[k] = true
	}
	grupo := fmt.Sprintf("g%d", time.Now().UnixNano())
	var ids []int64
	for _, pl := range planos {
		pj, err := json.Marshal(pl)
		if err != nil {
			return "", nil, err
		}
		id, err := l.Cadastro.NovaExecucao(ctx, cadastro.Execucao{
			Perfil: pl.Perfil.Nome, Tipo: cadastro.TipoCopia, Estado: cadastro.EstadoFila, Destino: pl.Perfil.Destino,
			Banco: pl.Perfil.DestinoBanco, Plano: string(pj), Operador: Operador(), Grupo: grupo,
		})
		if err != nil {
			return "", nil, err
		}
		ids = append(ids, id)
	}
	if err := l.lancarGrupo(grupo, ids[0], seg); err != nil {
		for _, id := range ids {
			if e, err2 := l.Cadastro.Execucao(ctx, id); err2 == nil {
				e.Estado, e.Fim, e.Mensagem = cadastro.EstadoErro, time.Now(), "o processo do grupo não subiu: "+err.Error()
				_ = l.Cadastro.GravarExecucao(ctx, e)
			}
		}
		return grupo, ids, err
	}
	return grupo, ids, nil
}

// Trocar sobe o processo que faz só a troca de uma execução que aguarda a decisão.
func (l Lancador) Trocar(ctx context.Context, id int64, seg conexao.Segredos) error {
	e, err := l.Cadastro.Execucao(ctx, id)
	if err != nil {
		return err
	}
	if e.Estado != cadastro.EstadoAguardando {
		return fmt.Errorf("a execução %d não está aguardando a troca", id)
	}
	if !trava.Livre(l.Dir.Travas(), chaveTrava(e), e.Banco) {
		return trava.ErrOcupado
	}
	return l.lancar(id, true, seg)
}

// Descartar encerra uma execução que aguardava a troca, sem trocar. O __novo fica.
func (l Lancador) Descartar(ctx context.Context, id int64) error {
	e, err := l.Cadastro.Execucao(ctx, id)
	if err != nil {
		return err
	}
	if e.Estado != cadastro.EstadoAguardando {
		return fmt.Errorf("a execução %d não está aguardando a troca", id)
	}
	e.Estado, e.Fim = cadastro.EstadoErro, time.Now()
	e.Mensagem = fmt.Sprintf("o sysadmin não trocou (%s). O banco %s ficou no destino: apague-o na aba Anteriores quando quiser.", e.Mensagem, e.BancoNovo)
	return l.Cadastro.GravarExecucao(ctx, e)
}

func (l Lancador) lancar(id int64, trocar bool, seg conexao.Segredos) error {
	args := []string{"executar", "--dir", l.Dir.Raiz, "--execucao", strconv.FormatInt(id, 10)}
	if trocar {
		args = append(args, "--trocar")
	}
	return l.subir(args, l.Dir.Log(id), seg)
}

func (l Lancador) lancarGrupo(grupo string, primeiro int64, seg conexao.Segredos) error {
	return l.subir([]string{"executar", "--dir", l.Dir.Raiz, "--grupo", grupo}, l.Dir.Log(primeiro), seg)
}

func (l Lancador) subir(args []string, arqLog string, seg conexao.Segredos) error {
	bin, err := l.binario()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(arqLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	segredos, err := json.Marshal(seg)
	if err != nil {
		return err
	}
	cmd.Stdin = bytes.NewReader(segredos)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Recolhe o processo quando ele terminar (sem isso, ele vira zumbi enquanto a tela viver).
	go func() { _ = cmd.Wait() }()
	return nil
}

// chaveTrava é o servidor de destino da execução, pelo plano confirmado.
func chaveTrava(e cadastro.Execucao) string {
	var p motor.Plano
	_ = json.Unmarshal([]byte(e.Plano), &p)
	return trava.Destino(e.Destino, p.Destino.SystemID)
}

// Operador diz quem está confirmando: o usuário por trás do sudo (ou o login da sessão, pelo
// /proc/self/loginuid, que o sudo não troca) e o endereço de onde veio o SSH.
func Operador() string {
	u := os.Getenv("SUDO_USER")
	if u == "" {
		if b, err := os.ReadFile("/proc/self/loginuid"); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" && id != "4294967295" {
				if x, err := user.LookupId(id); err == nil {
					u = x.Username
				}
			}
		}
	}
	if u == "" {
		u = os.Getenv("USER")
	}
	if u == "" {
		u = "?"
	}
	if c := strings.Fields(os.Getenv("SSH_CONNECTION")); len(c) > 0 {
		u += " de " + c[0]
	}
	return u
}

// LerSegredos lê os segredos da entrada padrão do processo da execução.
func LerSegredos(r io.Reader) (conexao.Segredos, error) {
	var s conexao.Segredos
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return s, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil
	}
	return s, json.Unmarshal(b, &s)
}

// Trabalhar é o processo da execução: pega a trava do destino e roda a cópia (ou só a troca).
func Trabalhar(ctx context.Context, d motor.Deps, id int64, trocar bool) error {
	e, err := d.Cadastro.Execucao(ctx, id)
	if err != nil {
		return err
	}
	t, err := trava.Obter(d.Dir.Travas(), chaveTrava(e), e.Banco)
	if err != nil {
		if !trocar {
			e.Estado, e.Fim, e.Mensagem = cadastro.EstadoErro, time.Now(), err.Error()
			_ = d.Cadastro.GravarExecucao(ctx, e)
		}
		return err
	}
	defer t.Soltar()
	d.TravaObtida = true
	if trocar {
		return motor.TrocarDepois(ctx, d, id)
	}
	return motor.Executar(ctx, d, id)
}

// TrabalharGrupo é o processo de um grupo: roda as cópias na ordem, uma de cada vez, cada uma com
// a trava do seu destino e o seu log. Uma que falha não para as outras; o cancelamento para a atual
// e as que esperam.
func TrabalharGrupo(ctx context.Context, d motor.Deps, grupo string, abrirLog func(id int64) (io.WriteCloser, error)) error {
	es, err := d.Cadastro.ExecucoesDoGrupo(ctx, grupo)
	if err != nil {
		return err
	}
	// Todas as da fila apontam para este processo: a tela as vê vivas enquanto esperam.
	for _, e := range es {
		if e.Estado == cadastro.EstadoFila {
			e.PID = os.Getpid()
			_ = d.Cadastro.GravarExecucao(ctx, e)
		}
	}
	var falhas int
	for _, e := range es {
		if e.Estado != cadastro.EstadoFila {
			continue
		}
		if ctx.Err() != nil {
			e.Estado, e.Fim, e.Mensagem = cadastro.EstadoCancelada, time.Now(), "cancelada pelo sysadmin antes de começar (estava na fila do grupo)"
			_ = d.Cadastro.GravarExecucao(context.Background(), e)
			continue
		}
		log, err := abrirLog(e.ID)
		if err != nil {
			return err
		}
		dd := d
		dd.Log = log
		if err := Trabalhar(ctx, dd, e.ID, false); err != nil {
			falhas++
		}
		_ = log.Close()
	}
	if falhas > 0 {
		return fmt.Errorf("%d cópia(s) do grupo não terminaram bem", falhas)
	}
	return nil
}

// Cancelar manda SIGTERM ao processo da execução. O processo para o container e grava "cancelada".
func Cancelar(e cadastro.Execucao) error {
	if e.Terminou() {
		return fmt.Errorf("a execução %d já terminou", e.ID)
	}
	if e.PID <= 0 {
		return errors.New("a execução ainda está iniciando: tente de novo em um instante")
	}
	if !processoDe(e) {
		return fmt.Errorf("o processo %d não é mais o da execução %d", e.PID, e.ID)
	}
	return syscall.Kill(e.PID, syscall.SIGTERM)
}

// processoDe confere que o pid da execução é o processo dela: o da própria execução, ou o do grupo.
func processoDe(e cadastro.Execucao) bool {
	if e.Grupo != "" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", e.PID))
		return err == nil && bytes.Contains(b, []byte("\x00executar\x00")) && bytes.Contains(b, []byte("\x00--grupo\x00"+e.Grupo+"\x00"))
	}
	if processoDaExecucao(e.PID, e.ID) {
		return true
	}
	// O pghangar rodar (o cron) roda a cópia no próprio processo: o perfil é o último argumento. O
	// pghangar restaurar também: um dos argumentos é o arquivo, e o "perfil" da execução é o nome dele.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", e.PID))
	if err != nil {
		return false
	}
	args := bytes.Split(bytes.TrimSuffix(b, []byte{0}), []byte{0})
	if e.Tipo == cadastro.TipoArquivo {
		if len(args) < 2 || string(args[1]) != "restaurar" {
			return false
		}
		for _, a := range args[2:] {
			if filepath.Base(string(a)) == e.Perfil {
				return true
			}
		}
		return false
	}
	return bytes.Contains(b, []byte("\x00rodar\x00")) && bytes.HasSuffix(b, []byte("\x00"+e.Perfil+"\x00"))
}

// processoDaExecucao confere pelo /proc que o pid é mesmo o "executar" daquela execução: um pid
// reaproveitado pelo sistema nunca recebe o sinal.
func processoDaExecucao(pid int, id int64) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("\x00executar\x00")) && bytes.Contains(b, []byte("\x00--execucao\x00"+strconv.FormatInt(id, 10)+"\x00"))
}

// Conferir marca como interrompidas as execuções cujo processo morreu sem gravar o fim.
func Conferir(ctx context.Context, cad *cadastro.Cadastro, dir local.Dir) error {
	es, err := cad.Execucoes(ctx, 100)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Terminou() {
			continue
		}
		if e.PID > 0 && processoDe(e) {
			continue
		}
		if e.PID == 0 && time.Since(e.Inicio) < 30*time.Second {
			continue // ainda subindo
		}
		if !trava.Livre(dir.Travas(), chaveTrava(e), e.Banco) {
			continue
		}
		msg := "o processo da execução terminou sem registrar o fim, na etapa " + e.Etapa + " (veja o log)"
		switch {
		case e.Etapa == "" && e.Grupo != "":
			msg = "o processo do grupo terminou antes desta cópia começar"
		case e.Etapa == "":
			msg = "o processo da execução terminou antes de a cópia começar (veja o log)"
		}
		if e.BancoNovo != "" {
			msg += fmt.Sprintf("; o banco %s pode ter ficado no destino", e.BancoNovo)
		}
		if _, err := cad.Interromper(ctx, e.ID, msg); err != nil {
			return err
		}
	}
	limparPgpass(ctx, cad, dir)
	return nil
}

// limparPgpass apaga o pgpass temporário de execuções que já não rodam (um processo morto com
// kill -9 não chega ao defer que o apagaria). É o segredo temporário da própria ferramenta.
func limparPgpass(ctx context.Context, cad *cadastro.Cadastro, dir local.Dir) {
	ms, _ := filepath.Glob(filepath.Join(dir.Temp(), "*.pgpass"))
	for _, m := range ms {
		base := filepath.Base(m)
		i := strings.IndexByte(base, '-')
		if i <= 0 {
			continue
		}
		id, err := strconv.ParseInt(base[:i], 10, 64)
		if err != nil {
			continue
		}
		// Só sai o pgpass de uma execução que terminou ou que não existe: um erro na leitura (o
		// cadastro ocupado) não apaga a senha de uma cópia viva.
		e, err := cad.Execucao(ctx, id)
		if (err != nil && !errors.Is(err, cadastro.ErrNaoExiste)) || (err == nil && !e.Terminou()) {
			continue
		}
		_ = os.Remove(m)
	}
	// O diretório do socket de um túnel cujo processo morreu: ninguém escuta mais nele.
	ds, _ := filepath.Glob(filepath.Join(dir.Temp(), conexao.PrefixoTunel+"*"))
	for _, d := range ds {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < time.Minute {
			continue
		}
		socks, _ := filepath.Glob(filepath.Join(d, ".s.PGSQL.*"))
		vivo := false
		for _, s := range socks {
			if c, err := net.DialTimeout("unix", s, 300*time.Millisecond); err == nil {
				_ = c.Close()
				vivo = true
			}
		}
		if !vivo {
			_ = os.RemoveAll(d)
		}
	}
}
