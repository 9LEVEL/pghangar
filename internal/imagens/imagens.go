// Package imagens roda os clientes do PostgreSQL dentro das imagens oficiais, pela linha de
// comando do Docker (docs/ESTRATEGIA.md §5): baixar, travar pelo digest e rodar com --network host.
// O Docker é obrigatório: sem ele, a ferramenta não copia.
package imagens

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rotulo marca os containers da ferramenta, com o id da execução; RotuloInstancia, com a instância
// (outra instalação no mesmo Docker tem os containers dela, que esta nunca para).
const (
	Rotulo          = "copia-banco.execucao"
	RotuloInstancia = "copia-banco.instancia"
)

// Docker fala com o Docker pela linha de comando.
type Docker struct{ Binario string }

func (d Docker) bin() string {
	if d.Binario == "" {
		return "docker"
	}
	return d.Binario
}

func (d Docker) saida(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Versao é a versão do servidor Docker. Um erro aqui quer dizer que não há Docker utilizável.
func (d Docker) Versao(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(d.bin()); err != nil {
		return "", errors.New("o Docker não está instalado (o comando docker não foi encontrado)")
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := d.saida(c, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", fmt.Errorf("o Docker não respondeu: %w", err)
	}
	return v, nil
}

// Referencia é a tag de uma versão: postgres:18.
func Referencia(repositorio string, versao int) string {
	return repositorio + ":" + strconv.Itoa(versao)
}

// Baixar faz o docker pull, mandando o andamento para saida.
func (d Docker) Baixar(ctx context.Context, ref string, saida io.Writer) error {
	cmd := exec.CommandContext(ctx, d.bin(), "pull", ref)
	cmd.Stdout, cmd.Stderr = saida, saida
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker pull %s: %w", ref, err)
	}
	return nil
}

// Existe diz se a imagem está no Docker local.
func (d Docker) Existe(ctx context.Context, ref string) bool {
	_, err := d.saida(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return err == nil
}

// Digest devolve a referência travada da imagem: repositório@sha256 quando o Docker sabe de onde
// ela veio, ou o id da imagem (sha256:…) quando ela entrou por docker load.
func (d Docker) Digest(ctx context.Context, ref string) (string, error) {
	out, err := d.saida(ctx, "image", "inspect", "--format", "{{json .RepoDigests}} {{.Id}}", ref)
	if err != nil {
		return "", err
	}
	i := strings.LastIndex(out, " ")
	if i < 0 {
		return "", fmt.Errorf("resposta inesperada do docker image inspect: %q", out)
	}
	var digests []string
	_ = json.Unmarshal([]byte(out[:i]), &digests)
	id := out[i+1:]
	repo := ref
	if j := strings.LastIndex(ref, ":"); j > strings.LastIndex(ref, "/") {
		repo = ref[:j]
	}
	for _, dg := range digests {
		if strings.HasPrefix(dg, repo+"@") || strings.HasPrefix(dg, "docker.io/library/"+repo+"@") {
			return dg, nil
		}
	}
	if len(digests) > 0 {
		return digests[0], nil
	}
	return id, nil
}

var reVersaoCliente = regexp.MustCompile(`\(PostgreSQL\) (\d+(?:\.\d+)?)`)

// VersaoCliente roda pg_dump --version de dentro da imagem.
func (d Docker) VersaoCliente(ctx context.Context, imagem string) (string, error) {
	out, err := d.saida(ctx, "run", "--rm", "--pull", "never", "--network", "none", imagem, "pg_dump", "--version")
	if err != nil {
		return "", err
	}
	m := reVersaoCliente.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("versão do pg_dump ilegível: %q", out)
	}
	return m[1], nil
}

// MajorDoCliente tira a versão principal de "18.6".
func MajorDoCliente(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return n
}

// Volume é um diretório ou arquivo do host montado no container.
type Volume struct {
	Origem    string
	Destino   string
	SoLeitura bool
}

// Execucao é um comando rodado numa imagem.
type Execucao struct {
	Imagem   string
	Nome     string
	Rotulos  map[string]string
	Volumes  []Volume
	Ambiente map[string]string // nunca segredos: a senha vai no pgpass montado
	Comando  []string
}

// Args é a linha de comando do docker run. Um teste confere que nenhuma senha aparece aqui.
func (e Execucao) Args() []string {
	a := []string{"run", "--rm", "--pull", "never", "--network", "host", "--name", e.Nome}
	for _, k := range ordenar(e.Rotulos) {
		a = append(a, "--label", k+"="+e.Rotulos[k])
	}
	for _, v := range e.Volumes {
		m := v.Origem + ":" + v.Destino
		if v.SoLeitura {
			m += ":ro"
		}
		a = append(a, "--volume", m)
	}
	for _, k := range ordenar(e.Ambiente) {
		a = append(a, "--env", k+"="+e.Ambiente[k])
	}
	a = append(a, e.Imagem)
	return append(a, e.Comando...)
}

func ordenar(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Linha recebe cada linha da saída do container: fluxo é "saida" ou "erro".
type Linha func(fluxo, texto string)

// Rodar roda o comando e devolve o código de saída dele. Com o contexto cancelado, para o
// container (docker stop): matar só o cliente docker deixaria o container rodando.
func (d Docker) Rodar(ctx context.Context, e Execucao, linha Linha) (int, error) {
	if e.Nome == "" {
		return -1, errors.New("container sem nome: sem ele, não dá para parar no cancelamento")
	}
	cmd := exec.Command(d.bin(), e.Args()...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	var wg sync.WaitGroup
	// As duas saídas são lidas em paralelo, mas quem recebe as linhas as recebe uma de cada vez:
	// o motor conta, acumula e grava o progresso sem precisar de trava própria.
	var vez sync.Mutex
	ler := func(r io.Reader, fluxo string) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for s.Scan() {
			if linha != nil {
				vez.Lock()
				linha(fluxo, s.Text())
				vez.Unlock()
			}
		}
	}
	wg.Add(2)
	go ler(out, "saida")
	go ler(errp, "erro")

	fim := make(chan error, 1)
	go func() {
		wg.Wait()
		fim <- cmd.Wait()
	}()
	select {
	case err := <-fim:
		return codigo(err)
	case <-ctx.Done():
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = d.saida(c, "stop", "--time", "5", e.Nome)
		cancel()
		<-fim
		return -1, ctx.Err()
	}
}

func codigo(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Container é um container da ferramenta rodando.
type Container struct {
	Nome     string
	Execucao string
	Estado   string
}

// Rodando lista os containers desta instalação (os órfãos aparecem aqui).
func (d Docker) Rodando(ctx context.Context, instancia string) ([]Container, error) {
	out, err := d.saida(ctx, "ps", "--filter", "label="+RotuloInstancia+"="+instancia, "--format", `{{.Names}}	{{.Label "`+Rotulo+`"}}	{{.Status}}`)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, l := range strings.Split(out, "\n") {
		p := strings.SplitN(l, "\t", 3)
		if len(p) == 3 {
			cs = append(cs, Container{Nome: p[0], Execucao: p[1], Estado: p[2]})
		}
	}
	return cs, nil
}

// Parar para um container da ferramenta pelo nome.
func (d Docker) Parar(ctx context.Context, nome string) error {
	_, err := d.saida(ctx, "stop", "--time", "5", nome)
	return err
}
