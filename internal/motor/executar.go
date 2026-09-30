package motor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// Etapas da cópia, na ordem. A conferência vem logo depois do restore: o script pós-restore pode
// criar e apagar objetos de propósito. O script roda antes dos donos, com a role temporária, e o
// que ele criar passa ao dono do destino junto com o resto.
var Etapas = []string{
	"Checagens", "Anteriores", "Dump", "Criar __novo", "Restore", "Conferência", "Script pós-restore",
	"Donos", "Configurações do banco", "ANALYZE", "Base", "Troca",
}

const (
	dentroTrabalho = "/trabalho"
	dentroPgpass   = "/run/pghangar/pgpass"
	dentroScript   = "/run/pghangar/script.sql"
)

// Manifesto descreve um dump guardado. Fica ao lado do dump, em manifesto.json.
type Manifesto struct {
	Formato   string    `json:"formato,omitempty"` // pg_dump (diretório) ou blocos (link instável)
	Perfil    string    `json:"perfil"`
	Execucao  int64     `json:"execucao"`
	Estado    string    `json:"estado"` // incompleto | completo
	Inicio    time.Time `json:"inicio"`
	Fim       time.Time `json:"fim,omitempty"`
	Origem    Lado      `json:"origem"`
	Imagem    int       `json:"imagem"`
	ImagemRef string    `json:"imagem_ref"`
	Cliente   string    `json:"cliente"`
	SemDados  []string  `json:"sem_dados,omitempty"`
	Tamanho   int64     `json:"tamanho"`
	// Para restaurar este dump de novo, depois: a conferência compara com o que a origem tinha.
	Contagem  Contagem         `json:"contagem,omitempty"`
	Linhas    map[string]int64 `json:"linhas,omitempty"`
	Filtrado  bool             `json:"filtrado,omitempty"`
	Filtros   Filtros          `json:"filtros,omitempty"`
	Extensoes []Extensao       `json:"extensoes,omitempty"`
	Citadas   []string         `json:"citadas,omitempty"`
	Externos  []string         `json:"externos,omitempty"` // os servidores externos com user mappings
}

// LerManifesto lê o manifesto de um dump guardado.
func LerManifesto(dir string) (Manifesto, error) {
	var m Manifesto
	b, err := os.ReadFile(filepath.Join(dir, "manifesto.json"))
	if err != nil {
		return m, fmt.Errorf("lendo o manifesto do dump: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("manifesto ilegível: %w", err)
	}
	return m, nil
}

// Formatos do dump guardado.
const (
	FormatoPgDump = "pg_dump"
	FormatoBlocos = "blocos"
)

const (
	DumpIncompleto = "incompleto"
	DumpCompleto   = "completo"
)

// corrida é uma execução em andamento.
type corrida struct {
	d        Deps
	e        cadastro.Execucao
	p        Plano
	origem   *lado
	destino  *lado
	pgpass   string
	trabalho string
	role     string
	roleViva bool
	// linhasOrigem é a contagem por tabela, no snapshot do dump (perfil com conferir linhas).
	linhasOrigem map[string]int64
	// formato é o do dump: pg_dump (diretório) ou blocos (link instável).
	formato  string
	trocou   bool // o __novo já é o banco de destino
	fdwFeito bool // a correção dos user mappings já rodou no __novo
	passados bool // os objetos da role temporária já foram para o dono
	mu       sync.Mutex
	ultimo   time.Time
}

// Executar roda a cópia da execução registrada e grava o fim dela (ok, aguardando, erro ou
// cancelada). O erro devolvido é só para o código de saída do processo.
func Executar(ctx context.Context, d Deps, execID int64) error {
	e, err := d.Cadastro.Execucao(ctx, execID)
	if err != nil {
		return err
	}
	r := &corrida{d: d, e: e}
	if r.e.Estado == cadastro.EstadoFila || r.e.Inicio.IsZero() {
		r.e.Inicio = time.Now() // na fila de um grupo, o início é quando a vez chega
	}
	r.e.Estado = cadastro.EstadoRodando
	r.e.PID = os.Getpid()
	r.gravar()
	err = r.copiar(ctx)
	r.fim(ctx, err)
	return err
}

// aguardar é o fim sem troca: o restore teve erro, ou a conferência divergiu.
type aguardar struct{ motivo string }

func (a *aguardar) Error() string { return a.motivo }

func (r *corrida) fim(ctx context.Context, err error) {
	r.e.Fim = time.Now()
	if r.e.DumpDir != "" && r.e.TamanhoDump == 0 {
		r.e.TamanhoDump = TamanhoDir(filepath.Join(r.e.DumpDir, "dump")) // o que um dump interrompido chegou a escrever
	}
	var ag *aguardar
	ficou := ""
	if r.e.BancoNovo != "" && !r.trocou {
		ficou = fmt.Sprintf("o banco %s ficou no destino (apague-o na aba Anteriores)", r.e.BancoNovo)
		if c, ok := r.p.correcao(CorrecaoFDW); ok && c.Marcada && !r.fdwFeito {
			ficou += ", e ele ainda guarda os user mappings dos servidores externos, que a correção tiraria"
		}
	}
	switch {
	case err == nil:
		r.e.Estado = cadastro.EstadoOK
	case errors.As(err, &ag):
		r.e.Estado = cadastro.EstadoAguardando
		r.e.Mensagem = ag.motivo
	case errors.Is(err, context.Canceled) || (ctx.Err() != nil && !r.trocou):
		r.e.Estado = cadastro.EstadoCancelada
		r.e.Mensagem = "cancelada pelo sysadmin na etapa " + r.e.Etapa
		if ficou != "" {
			r.e.Mensagem += "; " + ficou
		}
	default:
		r.e.Estado = cadastro.EstadoErro
		r.e.Mensagem = err.Error()
		if ficou != "" {
			r.e.Mensagem += " — " + ficou
		}
	}
	r.d.logf("== fim: %s — %s", r.e.Estado, r.e.Mensagem)
	r.gravar()
	r.avisar()
}

func (r *corrida) gravar() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.d.Cadastro.GravarExecucao(context.Background(), r.e); err != nil {
		r.d.logf("gravando a execução: %v", err)
	}
}

func (r *corrida) etapa(nome string) {
	r.e.Etapa = nome
	for i, e := range Etapas {
		if e == nome {
			r.e.EtapaNum = i + 1
		}
	}
	r.e.EtapasTotal = len(Etapas)
	r.e.Feito, r.e.Total, r.e.Item = 0, 0, ""
	r.d.logf("== %d/%d %s", r.e.EtapaNum, r.e.EtapasTotal, nome)
	r.gravar()
}

// progresso grava o andamento, no máximo a cada 300 ms (o pg_restore escreve milhares de linhas).
func (r *corrida) progresso(feito, total int, item string, forcar bool) {
	r.mu.Lock()
	r.e.Feito, r.e.Total, r.e.Item = feito, total, item
	agora := time.Now()
	if !forcar && agora.Sub(r.ultimo) < 300*time.Millisecond {
		r.mu.Unlock()
		return
	}
	r.ultimo = agora
	r.mu.Unlock()
	_ = r.d.Cadastro.Progresso(context.Background(), r.e.ID, feito, total, item)
}

func (r *corrida) copiar(ctx context.Context) error {
	d := r.d
	r.etapa("Checagens")
	var confirmado Plano
	if r.e.Plano != "" {
		if err := json.Unmarshal([]byte(r.e.Plano), &confirmado); err != nil {
			return fmt.Errorf("plano confirmado ilegível: %w", err)
		}
	}
	restaurar := r.e.Tipo == cadastro.TipoRestauracao
	resetar := r.e.Tipo == cadastro.TipoReset
	var p Plano
	var err error
	switch {
	case restaurar:
		p, err = PlanejarRestauracao(ctx, d, r.e.DumpDir)
	case resetar:
		p, err = PlanejarReset(ctx, d, r.e.Perfil)
	default:
		p, err = Planejar(ctx, d, r.e.Perfil)
	}
	if err != nil {
		return fmt.Errorf("checagens: %w", err)
	}
	// As correções valem como o sysadmin as marcou na confirmação, e só as mesmas.
	if err := p.AdotarEscolhas(confirmado); err != nil {
		return err
	}
	if p.Bloqueado() {
		return fmt.Errorf("bloqueado: %s", strings.Join(p.BloqueiosAtivos(), "; "))
	}
	// O que o sysadmin confirmou é o que vai rodar: mesmo destino, mesmo servidor, mesma origem.
	if p.Destino.Conexao != r.e.Destino || p.Destino.Banco != r.e.Banco {
		return fmt.Errorf("o perfil mudou depois da confirmação (destino agora é %s/%s): confirme de novo", p.Destino.Conexao, p.Destino.Banco)
	}
	if confirmado.Destino.SystemID != "" && confirmado.Destino.SystemID != p.Destino.SystemID {
		return errors.New("o servidor de destino mudou desde a confirmação (system_identifier diferente): confirme de novo")
	}
	if confirmado.Origem.SystemID != "" && (confirmado.Origem.SystemID != p.Origem.SystemID || confirmado.Origem.Banco != p.Origem.Banco) {
		return errors.New("a origem mudou desde a confirmação: confirme de novo")
	}
	r.p = p
	r.e.Avisos = append([]string(nil), p.Avisos...)
	r.e.Notas = append([]string(nil), p.Notas...)
	r.e.BancoNovo = ""
	pj, _ := json.Marshal(p)
	r.e.Plano = string(pj)
	r.gravar()
	for _, a := range p.Avisos {
		d.logf("aviso: %s", a)
	}
	for _, a := range p.Notas {
		d.logf("nota: %s", a)
	}

	// Numa restauração, a origem não é aberta: o dump já está no disco. Num reset, nem ela nem o
	// Docker: o destino é recriado a partir da base, no próprio servidor.
	if !restaurar && !resetar {
		if r.origem, _, err = abrirOrigem(ctx, d, p.Perfil.Origem); err != nil {
			return fmt.Errorf("abrindo a origem: %w", err)
		}
		defer func() { r.origem.fechar() }()
	}
	if r.destino, _, err = abrirLado(ctx, d, p.Perfil.Destino); err != nil {
		return fmt.Errorf("abrindo o destino: %w", err)
	}
	defer r.destino.fechar()
	// O destino aberto para o trabalho é o mesmo servidor do plano (um DNS ou um VIP que mudou no
	// meio não leva a cópia a outro cluster).
	if sid := r.destino.c.Info.SystemID; p.Destino.SystemID != "" && sid != p.Destino.SystemID {
		return fmt.Errorf("o destino aberto é outro servidor (system_identifier %s, e não %s): confirme de novo", sid, p.Destino.SystemID)
	}
	if err := r.corrigirNovo(ctx); err != nil {
		return err
	}
	limparRolesOrfas(ctx, d, r.destino.admin)
	if resetar {
		return r.resetar(ctx)
	}
	if err := r.escreverPgpass(); err != nil {
		return err
	}
	// O pgpass temporário é apagado no fim: é um segredo da ferramenta. O caminho é lido na hora
	// (a repetição do dump o recria com outro nome).
	defer func() { _ = os.Remove(r.pgpass) }()

	if len(r.e.Apagar) > 0 {
		r.etapa("Anteriores")
		if err := r.apagarAnteriores(ctx); err != nil {
			return err
		}
	}

	if restaurar {
		m, err := LerManifesto(r.e.DumpDir)
		if err != nil {
			return err
		}
		r.trabalho, r.linhasOrigem, r.formato = r.e.DumpDir, m.Linhas, m.Formato
		d.logf("restaurando o dump guardado em %s (de %s)", r.e.DumpDir, m.Inicio.Format("02/01/2006 15:04"))
	} else {
		r.etapa("Dump")
		if p.Perfil.Retomavel {
			r.formato = FormatoBlocos
			if err := r.dumpBlocos(ctx); err != nil {
				return err
			}
		} else if err := r.dump(ctx); err != nil {
			return err
		}
		// A origem não é mais necessária: o túnel fecha aqui.
		r.origem.fechar()
		r.origem = nil
	}

	r.etapa("Criar __novo")
	if err := r.corrigirRoles(ctx); err != nil {
		return err
	}
	if err := r.criarNovo(ctx); err != nil {
		return err
	}
	defer r.soltarRole()

	r.etapa("Restore")
	erros, err := r.restore(ctx)
	if err != nil {
		return err
	}
	if err := r.corrigirFDW(ctx); err != nil {
		return err
	}

	r.etapa("Conferência")
	difs, err := r.conferir(ctx)
	if err != nil {
		return err
	}

	if p.Perfil.Script != "" {
		r.etapa("Script pós-restore")
		if err := r.script(ctx); err != nil {
			return err
		}
	}

	r.etapa("Donos")
	if err := r.donos(ctx); err != nil {
		return err
	}

	if p.Destino.Existe {
		r.etapa("Configurações do banco")
		if err := r.configuracoes(ctx); err != nil {
			return err
		}
	}

	r.etapa("ANALYZE")
	if err := r.analyze(ctx); err != nil {
		return err
	}

	if erros > 0 || len(difs) > 0 {
		var m []string
		if erros > 0 {
			m = append(m, fmt.Sprintf("o restore terminou com %d erro(s)", erros))
		}
		if len(difs) > 0 {
			m = append(m, "a conferência divergiu ("+strings.Join(difs, "; ")+")")
		}
		return &aguardar{motivo: strings.Join(m, " e ") + fmt.Sprintf(": a troca espera a sua decisão. O banco %s está pronto no destino; veja o log.", r.e.BancoNovo)}
	}

	if p.Perfil.GuardarBase {
		r.etapa("Base")
		if err := r.guardarBase(ctx); err != nil {
			return err
		}
	}

	r.etapa("Troca")
	// A troca não obedece ao cancelamento: parar entre os dois renames derrubaria o destino.
	ctxTroca, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	ant, trocou, err := trocarNomes(ctxTroca, d, r.destino, p.Destino.Banco, r.e.BancoNovo, p.Destino.Existe, r.e.NovoOID, time.Now())
	r.e.BancoAnterior, r.trocou = ant, trocou
	if err != nil {
		return err
	}
	r.e.Mensagem = fmt.Sprintf("%s copiado de %s/%s", p.Destino.Banco, p.Origem.Conexao, p.Origem.Banco)
	if restaurar {
		r.e.Mensagem = fmt.Sprintf("%s restaurado do dump de %s", p.Destino.Banco, filepath.Base(r.e.DumpDir))
	}
	if ant != "" {
		r.e.Mensagem += "; o banco substituído ficou como " + ant
	}
	return nil
}

func (r *corrida) escreverPgpass() error {
	f, err := os.CreateTemp(r.d.Dir.Temp(), fmt.Sprintf("%d-*.pgpass", r.e.ID))
	if err != nil {
		return err
	}
	r.pgpass = f.Name()
	var linhas string
	for _, l := range []*lado{r.origem, r.destino} {
		if l == nil {
			continue // numa restauração, só o destino
		}
		linhas += conexao.LinhaPgpass(l.c, l.ponte, l.senha) + "\n"
		if l.ponte.Socket() {
			// Para o diretório de socket padrão, o libpq procura "localhost" no pgpass.
			linhas += conexao.LinhaPgpass(l.c, &conexao.Ponte{Host: "localhost", Porta: l.ponte.Porta}, l.senha) + "\n"
		}
	}
	if _, err := f.WriteString(linhas); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r *corrida) apagarAnteriores(ctx context.Context) error {
	existe := map[string]bool{}
	for _, a := range r.p.Anteriores {
		existe[a.Nome] = true
	}
	for i, nome := range r.e.Apagar {
		// Só um __anterior deste destino, e que ainda existe: nada mais sai por aqui.
		if _, ok := nomes.DataDoAnterior(r.p.Destino.Banco, nome); !ok || !existe[nome] {
			return fmt.Errorf("recusado apagar %s: não é um __anterior de %s que exista no destino", nome, r.p.Destino.Banco)
		}
		r.progresso(i, len(r.e.Apagar), nome, true)
		r.d.logf("apagando %s", nome)
		if _, err := r.destino.admin.Exec(ctx, "DROP DATABASE "+id(nome)+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("apagando %s: %w", nome, err)
		}
	}
	return nil
}

// rodar roda um cliente na imagem do plano, com o trabalho, o pgpass e os sockets montados.
func (r *corrida) rodar(ctx context.Context, sufixo string, comando []string, extra []imagens.Volume, amb map[string]string, linha func(fluxo, texto string)) (int, []string, error) {
	vols := []imagens.Volume{{Origem: r.pgpass, Destino: dentroPgpass, SoLeitura: true}}
	if r.trabalho != "" {
		vols = append(vols, imagens.Volume{Origem: r.trabalho, Destino: dentroTrabalho})
	}
	for _, l := range []*lado{r.origem, r.destino} {
		if l != nil && l.ponte.Socket() {
			vols = append(vols, imagens.Volume{Origem: l.ponte.Host, Destino: l.ponte.Host})
		}
	}
	vols = append(vols, extra...)
	env := map[string]string{"PGPASSFILE": dentroPgpass}
	for k, v := range amb {
		env[k] = v
	}
	e := imagens.Execucao{
		// O nome leva a instância: duas instalações no mesmo Docker têm, cada uma, a sua execução 1.
		Imagem: r.p.ImagemRef, Nome: fmt.Sprintf("pghangar-%s-%d-%s", r.instancia(), r.e.ID, sufixo),
		Rotulos: map[string]string{imagens.Rotulo: strconv.FormatInt(r.e.ID, 10), imagens.RotuloInstancia: r.instancia()},
		Volumes: vols, Ambiente: env, Comando: comando,
	}
	r.d.logf("$ docker %s", strings.Join(e.Args(), " "))
	var ultimas []string
	cod, err := r.d.Docker.Rodar(ctx, e, func(fluxo, texto string) {
		if r.d.Log != nil {
			fmt.Fprintf(r.d.Log, "    %s\n", texto)
		}
		ultimas = append(ultimas, texto)
		if len(ultimas) > 40 {
			ultimas = ultimas[1:]
		}
		if linha != nil {
			linha(fluxo, texto)
		}
	})
	return cod, ultimas, err
}

func (r *corrida) instancia() string {
	i, _ := r.d.Cadastro.Instancia(context.Background())
	return i
}

func falhaCliente(o string, cod int, ultimas []string) error {
	var rel []string
	for _, l := range ultimas {
		if strings.Contains(l, "error") || strings.Contains(l, "erro") || strings.Contains(l, "FATAL") || strings.Contains(l, "fatal") {
			rel = append(rel, strings.TrimSpace(l))
		}
	}
	if len(rel) == 0 && len(ultimas) > 0 {
		rel = ultimas[max(0, len(ultimas)-3):]
	}
	if len(rel) > 4 {
		rel = rel[len(rel)-4:]
	}
	return fmt.Errorf("%s saiu com código %d: %s", o, cod, strings.Join(rel, " | "))
}

var (
	reDumpTabela = regexp.MustCompile(`dumping contents of table "?([^"]+)"?`)
	// Cada item do índice aparece uma vez como "processing item" (a parte serial) ou "finished item"
	// (a parte paralela): contar os ids distintos dá o andamento contra o total do --list.
	reRestoreItem   = regexp.MustCompile(`^pg_restore: (?:processing|finished) item (\d+) (.+)$`)
	reErrosIgnorado = regexp.MustCompile(`errors ignored on restore: (\d+)`)
)

func (r *corrida) dump(ctx context.Context) error {
	p := r.p
	if err := os.MkdirAll(p.DirDumps, 0o700); err != nil {
		return err
	}
	nome := time.Now().Format("20060102_150405")
	r.trabalho = filepath.Join(p.DirDumps, nome)
	if _, err := os.Stat(r.trabalho); err == nil {
		r.trabalho += fmt.Sprintf("_%d", r.e.ID)
	}
	if err := os.Mkdir(r.trabalho, 0o700); err != nil {
		return err
	}
	r.e.DumpDir = r.trabalho
	r.gravar()
	m := Manifesto{Perfil: p.Perfil.Nome, Execucao: r.e.ID, Estado: DumpIncompleto, Inicio: time.Now(), Origem: p.Origem,
		Imagem: p.Imagem, ImagemRef: p.ImagemRef, Cliente: p.Cliente, SemDados: p.Perfil.SemDados, Contagem: p.Contagem,
		Filtrado: p.Perfil.Filtrado(), Formato: FormatoPgDump, Extensoes: p.Extensoes, Citadas: p.Citadas, Externos: p.Externos,
		Filtros: Filtros{Schemas: p.Perfil.Schemas, SchemasFora: p.Perfil.SchemasFora, Tabelas: p.Perfil.Tabelas, TabelasFora: p.Perfil.TabelasFora}}
	if err := EscreverManifesto(r.trabalho, m); err != nil {
		return err
	}

	// A queda do túnel (ou da rede) no meio do dump não derruba a cópia: o dump recomeça sozinho,
	// numa conexão nova, algumas vezes. A tentativa que caiu fica no disco (nada é apagado sozinho).
	esperas := r.d.EsperasRede
	if esperas == nil {
		esperas = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second}
	}
	for tentativa := 0; ; tentativa++ {
		err := r.dumpUmaVez(ctx)
		if err == nil {
			break
		}
		var rede *falhaRede
		if !errors.As(err, &rede) || tentativa >= len(esperas) || ctx.Err() != nil {
			return err
		}
		parcial := filepath.Join(r.trabalho, fmt.Sprintf("dump.incompleto-%d", tentativa+1))
		_ = os.Rename(filepath.Join(r.trabalho, "dump"), parcial)
		r.d.logf("o dump caiu (%v): nova tentativa em %s (%d de %d); o que ele escreveu ficou em %s", err, esperas[tentativa], tentativa+2, len(esperas)+1, parcial)
		r.e.Notas = append(r.e.Notas, fmt.Sprintf("o dump caiu e recomeçou (tentativa %d): %v", tentativa+2, err))
		r.gravar()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(esperas[tentativa]):
		}
		if err := r.reabrirOrigem(ctx); err != nil {
			return fmt.Errorf("reabrindo a origem depois da queda: %w", err)
		}
	}
	m.Estado, m.Fim = DumpCompleto, time.Now()
	m.Tamanho = TamanhoDir(filepath.Join(r.trabalho, "dump"))
	m.Linhas = r.linhasOrigem
	r.e.TamanhoDump = m.Tamanho
	r.gravar()
	r.d.logf("dump completo: %s em %s", Tamanho(m.Tamanho), r.trabalho)
	return EscreverManifesto(r.trabalho, m)
}

// falhaRede é o dump que caiu por rede (o túnel, a conexão): vale tentar de novo.
type falhaRede struct{ err error }

func (f *falhaRede) Error() string { return f.err.Error() }
func (f *falhaRede) Unwrap() error { return f.err }

// errOrigemOutra: a origem reaberta depois de uma queda é outro servidor. A cópia para: continuar
// misturaria dois clusters no mesmo dump.
var errOrigemOutra = errors.New("a origem reaberta é outro servidor (system_identifier diferente)")

// marcasFixas são erros que não passam tentando de novo: senha, pg_hba, TLS recusado.
var marcasFixas = []string{
	"password authentication failed", "no pg_hba.conf entry", "o túnel não conseguiu TLS", "does not exist",
	"permission denied", "SSL is not enabled on the server", "server does not support SSL",
}

var marcasDeRede = []string{
	"server closed the connection unexpectedly", "could not connect to server", "connection to server",
	"no connection to the server", "SSL SYSCALL error", "Connection refused", "Connection reset",
	"could not receive data from server", "could not send data to server", "terminating connection due to administrator",
	"EOF detected", "túnel SSH caiu",
}

func ehFalhaDeRede(ultimas []string) bool {
	for _, l := range ultimas {
		for _, m := range marcasFixas {
			if strings.Contains(l, m) {
				return false
			}
		}
	}
	for _, l := range ultimas {
		for _, m := range marcasDeRede {
			if strings.Contains(l, m) {
				return true
			}
		}
	}
	return false
}

// reabrirOrigem abre de novo a ponte e a conexão com a origem (um túnel novo), e reescreve o
// pgpass, que tem o endereço da ponte.
func (r *corrida) reabrirOrigem(ctx context.Context) error {
	if r.origem != nil {
		r.origem.fechar()
	}
	var err error
	if r.origem, _, err = abrirOrigem(ctx, r.d, r.p.Perfil.Origem); err != nil {
		return err
	}
	if r.origem.c.Info.SystemID != r.p.Origem.SystemID && r.p.Origem.SystemID != "" {
		return errOrigemOutra
	}
	_ = os.Remove(r.pgpass)
	return r.escreverPgpass()
}

// argsDump são as opções do pg_dump do perfil: formato, jobs, compressão e filtros.
func argsDump(p Plano) []string {
	pf := p.Perfil
	args := []string{"--format=directory", fmt.Sprintf("--jobs=%d", pf.JobsDump), "--verbose", "--file=" + dentroTrabalho + "/dump"}
	switch pf.Compressao {
	case "", "zstd":
		args = append(args, "--compress=zstd")
	case "nenhuma":
		args = append(args, "--compress=none")
	default:
		args = append(args, "--compress="+pf.Compressao)
	}
	for _, x := range pf.Schemas {
		args = append(args, "--schema="+x)
	}
	for _, x := range pf.SchemasFora {
		args = append(args, "--exclude-schema="+x)
	}
	for _, x := range pf.Tabelas {
		args = append(args, "--table="+x)
	}
	for _, x := range pf.TabelasFora {
		args = append(args, "--exclude-table="+x)
	}
	// Com -n ou -t, o pg_dump deixa as extensões de fora; -e as traz de volta.
	if len(pf.Schemas) > 0 || len(pf.Tabelas) > 0 {
		args = append(args, "--extension=*")
	}
	for _, pat := range pf.SemDados {
		args = append(args, "--exclude-table-data="+pat)
	}
	return args
}

func (r *corrida) dumpUmaVez(ctx context.Context) error {
	p := r.p
	app := fmt.Sprintf("pghangar/%s/%s", r.d.Maquina, p.Perfil.Nome)
	args := argsDump(p)

	// A contagem de linhas usa o mesmo snapshot do dump: um snapshot exportado, que o pg_dump
	// adota (--snapshot). A transação fica aberta até a contagem, depois do dump.
	var snap pgx.Tx
	var snapConn *pgx.Conn
	if p.Perfil.ConferirLinhas {
		var err error
		if snapConn, err = r.origem.conectar(ctx, r.d, p.Origem.Banco); err != nil {
			return &falhaRede{err}
		}
		defer snapConn.Close(context.Background())
		if snap, err = snapConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}); err != nil {
			return &falhaRede{err}
		}
		defer func() { _ = snap.Rollback(context.Background()) }()
		var id string
		if err := snap.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&id); err != nil {
			return err
		}
		args = append(args, "--snapshot="+id)
		r.d.logf("snapshot exportado para o dump e a contagem de linhas: %s", id)
	}
	args = append(args, "--dbname="+conexao.DSN(r.origem.c, r.origem.ponte, p.Origem.Banco, app))
	if err := versoes.Conferir("pg_dump", args, p.Imagem); err != nil {
		return err
	}
	// O tamanho escrito até agora, a cada 2 s: numa tabela grande, a contagem de tabelas para.
	medindo := make(chan struct{})
	defer close(medindo)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-medindo:
				return
			case <-t.C:
				_ = r.d.Cadastro.TamanhoDump(context.Background(), r.e.ID, TamanhoDir(filepath.Join(r.trabalho, "dump")))
			}
		}
	}()
	feito := 0
	cod, ultimas, err := r.rodar(ctx, "dump", append([]string{"pg_dump"}, args...), nil, nil, func(_, t string) {
		if mm := reDumpTabela.FindStringSubmatch(t); mm != nil {
			feito++
			r.progresso(feito, max(p.Tabelas, feito), mm[1], false)
		}
	})
	if err != nil {
		return err
	}
	if cod != 0 {
		e := falhaCliente("o pg_dump", cod, ultimas)
		caiu := r.origem.ponte.Tunel != nil && r.origem.ponte.Tunel.Erro() != nil
		if caiu {
			e = fmt.Errorf("%w (o túnel SSH caiu: %v)", e, r.origem.ponte.Tunel.Erro())
		}
		if caiu || ehFalhaDeRede(ultimas) {
			return &falhaRede{e}
		}
		return e
	}
	r.progresso(feito, max(p.Tabelas, feito), "", true)
	if snap != nil {
		r.d.logf("contando as linhas na origem, no snapshot do dump")
		if r.linhasOrigem, err = contarLinhas(ctx, snap, func(i, n int, t string) { r.progresso(i, n, "contando "+t, false) }); err != nil {
			return &falhaRede{fmt.Errorf("contando as linhas da origem: %w", err)}
		}
	}
	return nil
}

// criarNovo cria o __novo FECHADO: dono é o superusuário da conexão, e PUBLIC não conecta. Durante
// o restore, tudo lá dentro pertence à role temporária de superusuário; uma função SECURITY DEFINER
// vinda da origem viraria um atalho para superusuário a qualquer login do servidor. Ele só abre na
// etapa "Donos", quando tudo já passou ao dono do destino.
func (r *corrida) criarNovo(ctx context.Context) error {
	p := r.p
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	sql := SQLCriarBanco(p.Novo, r.destino.c.Usuario, p.Destino.Info, p.Destino.VersaoNum)
	r.d.logf("%s", sql)
	if _, err := adm.Exec(ctx, sql); err != nil {
		return fmt.Errorf("criando %s: %w", p.Novo, err)
	}
	r.e.BancoNovo = p.Novo
	if r.e.NovoOID, err = oid(ctx, adm, p.Novo); err != nil {
		return err
	}
	r.gravar()
	if _, err := adm.Exec(ctx, "REVOKE ALL ON DATABASE "+id(p.Novo)+" FROM PUBLIC"); err != nil {
		return fmt.Errorf("fechando %s: %w", p.Novo, err)
	}
	inst, err := r.d.Cadastro.Instancia(ctx)
	if err != nil {
		return err
	}
	// A marca diz de que instalação e de que execução o __novo é: a correção que apaga um __novo
	// que sobrou só vale para os desta instalação, de uma cópia que já terminou.
	if _, err := adm.Exec(ctx, "COMMENT ON DATABASE "+id(p.Novo)+" IS "+lit(marcaDoNovo(inst, r.e.ID))); err != nil {
		return fmt.Errorf("marcando %s: %w", p.Novo, err)
	}
	r.role = nomes.RoleTemporaria(inst, r.e.ID)
	if _, err := adm.Exec(ctx, "CREATE ROLE "+id(r.role)+" SUPERUSER NOLOGIN"); err != nil {
		return fmt.Errorf("criando a role temporária %s: %w", r.role, err)
	}
	r.roleViva = true
	return nil
}

func (r *corrida) restore(ctx context.Context) (int, error) {
	if r.formato == FormatoBlocos {
		return r.restoreBlocos(ctx)
	}
	p := r.p
	// O total vem do índice do dump.
	total := 0
	cod, ultimas, err := r.rodar(ctx, "lista", []string{"pg_restore", "--list", dentroTrabalho + "/dump"}, nil, nil, func(fluxo, t string) {
		if fluxo == "saida" && t != "" && !strings.HasPrefix(t, ";") {
			total++
		}
	})
	if err != nil {
		return 0, err
	}
	if cod != 0 {
		return 0, falhaCliente("o pg_restore --list", cod, ultimas)
	}
	args := []string{fmt.Sprintf("--jobs=%d", p.Perfil.JobsRestore), "--verbose", "--no-owner", "--no-privileges",
		"--no-tablespaces", "--no-subscriptions", "--no-publications", "--role=" + r.role,
		"--dbname=" + conexao.DSN(r.destino.c, r.destino.ponte, r.e.BancoNovo, "pghangar"), dentroTrabalho + "/dump"}
	if err := versoes.Conferir("pg_restore", args, p.Imagem); err != nil {
		return 0, err
	}
	ignorados := -1
	vistos := map[string]bool{}
	var erros []string
	cod, ultimas, err = r.rodar(ctx, "restore", append([]string{"pg_restore"}, args...), nil, nil, func(_, t string) {
		if mm := reRestoreItem.FindStringSubmatch(t); mm != nil && !vistos[mm[1]] {
			vistos[mm[1]] = true
			r.progresso(min(len(vistos), max(total, 1)), max(total, 1), mm[2], false)
		}
		if strings.Contains(t, "pg_restore: error:") && len(erros) < 50 {
			erros = append(erros, t)
		}
		if mm := reErrosIgnorado.FindStringSubmatch(t); mm != nil {
			ignorados, _ = strconv.Atoi(mm[1])
		}
	})
	if err != nil {
		return 0, err
	}
	r.progresso(max(total, 1), max(total, 1), "", true)
	switch {
	case cod == 0:
		return 0, nil
	case cod == 1 && ignorados > 0:
		r.e.ErrosRestore = ignorados
		r.gravar()
		r.d.logf("o restore terminou com %d erro(s) ignorado(s):", ignorados)
		for _, e := range erros {
			r.d.logf("  %s", e)
		}
		return ignorados, nil
	}
	return 0, falhaCliente("o pg_restore", cod, ultimas)
}

func (r *corrida) script(ctx context.Context) error {
	s := r.p.Perfil.Script
	if err := conferirScript(s); err != nil {
		return err
	}
	args := []string{"--no-psqlrc", "--set=ON_ERROR_STOP=1", "--file=" + dentroScript,
		"--dbname=" + conexao.DSN(r.destino.c, r.destino.ponte, r.e.BancoNovo, "pghangar/script")}
	if err := versoes.Conferir("psql", args, r.p.Imagem); err != nil {
		return err
	}
	// Com a role temporária: o script tem os poderes de superusuário, e o que ele criar passa ao
	// dono do destino na etapa seguinte.
	cod, ultimas, err := r.rodar(ctx, "script", append([]string{"psql"}, args...),
		[]imagens.Volume{{Origem: s, Destino: dentroScript, SoLeitura: true}}, map[string]string{"PGOPTIONS": "-c role=" + r.role}, nil)
	if err != nil {
		return err
	}
	if cod != 0 {
		return falhaCliente("o script pós-restore", cod, ultimas)
	}
	return nil
}

func (r *corrida) donos(ctx context.Context) error {
	if err := r.passarDonos(ctx); err != nil {
		return err
	}
	r.soltarRole()
	if r.roleViva {
		return fmt.Errorf("a role temporária %s não pôde ser removida (veja o log)", r.role)
	}
	// Agora nada lá dentro é de superusuário: o banco passa ao dono e abre como um banco novo
	// abriria. As permissões do destino atual, se ele tiver, vêm na etapa seguinte.
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	for _, c := range []string{
		"ALTER DATABASE " + id(r.e.BancoNovo) + " OWNER TO " + id(r.dono()),
		"GRANT CONNECT, TEMPORARY ON DATABASE " + id(r.e.BancoNovo) + " TO PUBLIC",
	} {
		r.d.logf("%s", c)
		if _, err := adm.Exec(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	return nil
}

// dono é o dono do banco de destino atual, ou o usuário da conexão se o destino ainda não existe.
func (r *corrida) dono() string {
	if r.p.Destino.Info.Dono != "" {
		return r.p.Destino.Info.Dono
	}
	return r.destino.c.Usuario
}

// passarDonos entrega ao dono do destino tudo o que a role temporária criou no __novo.
func (r *corrida) passarDonos(ctx context.Context) error {
	if !r.roleViva || r.passados {
		return nil
	}
	dono := r.dono()
	conn, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var super bool
	if err := conn.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = $1`, dono).Scan(&super); err != nil {
		return fmt.Errorf("lendo o dono %s: %w", dono, err)
	}
	if !super {
		// O dono de um event trigger precisa ser superusuário: esses ficam com o usuário da conexão.
		rows, err := conn.Query(ctx, `SELECT evtname FROM pg_event_trigger WHERE evtowner = (SELECT oid FROM pg_roles WHERE rolname = $1)`, r.role)
		if err != nil {
			return err
		}
		var evs []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			evs = append(evs, n)
		}
		rows.Close()
		for _, ev := range evs {
			if _, err := conn.Exec(ctx, "ALTER EVENT TRIGGER "+id(ev)+" OWNER TO "+id(r.destino.c.Usuario)); err != nil {
				return err
			}
		}
		// Um foreign-data wrapper (o do postgres_fdw, por exemplo) também só pode ser de superusuário.
		fdws, err := nomesDe(ctx, conn, `SELECT fdwname FROM pg_foreign_data_wrapper WHERE fdwowner = (SELECT oid FROM pg_roles WHERE rolname = $1)`, r.role)
		if err != nil {
			return err
		}
		for _, f := range fdws {
			if _, err := conn.Exec(ctx, "ALTER FOREIGN DATA WRAPPER "+id(f)+" OWNER TO "+id(r.destino.c.Usuario)); err != nil {
				return err
			}
		}
	}
	r.d.logf("REASSIGN OWNED BY %s TO %s", r.role, dono)
	if _, err := conn.Exec(ctx, "REASSIGN OWNED BY "+id(r.role)+" TO "+id(dono)); err != nil {
		return fmt.Errorf("passando os objetos para %s: %w", dono, err)
	}
	if _, err := conn.Exec(ctx, "DROP OWNED BY "+id(r.role)); err != nil {
		return err
	}
	r.passados = true
	return nil
}

func nomesDe(ctx context.Context, conn *pgx.Conn, q string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ns []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		ns = append(ns, n)
	}
	return ns, rows.Err()
}

// soltarRole remove a role temporária. Se não der (objetos que sobraram), tira dela o
// superusuário: uma role superusuária esquecida é o pior que pode sobrar. Roda até cancelado.
func (r *corrida) soltarRole() {
	if !r.roleViva || r.destino == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if r.e.BancoNovo != "" {
		if err := r.passarDonos(ctx); err != nil {
			r.d.logf("passando os donos na limpeza: %v", err)
		}
	}
	err := consertar(r.d, r.destino, "remover a role "+r.role, func(c context.Context, a *pgx.Conn) error {
		_, err := a.Exec(c, "DROP ROLE "+id(r.role))
		return err
	})
	if err != nil {
		if err2 := consertar(r.d, r.destino, "tirar o superusuário da role "+r.role, func(c context.Context, a *pgx.Conn) error {
			_, err := a.Exec(c, "ALTER ROLE "+id(r.role)+" NOSUPERUSER NOLOGIN")
			return err
		}); err2 != nil {
			r.d.logf("A ROLE %s CONTINUA SUPERUSUÁRIO: %v (a próxima cópia para este servidor tenta de novo)", r.role, err2)
		}
		return
	}
	r.roleViva = false
	r.d.logf("role temporária %s removida", r.role)
}

// configuracoes reaplica no __novo o que é do banco e não vem no dump.
func (r *corrida) configuracoes(ctx context.Context) error {
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	b, novo := r.p.Destino.Banco, r.e.BancoNovo
	var cmds []string

	rows, err := adm.Query(ctx, `SELECT coalesce(ro.rolname, ''), s.setconfig FROM pg_db_role_setting s
		LEFT JOIN pg_roles ro ON ro.oid = s.setrole WHERE s.setdatabase = (SELECT oid FROM pg_database WHERE datname = $1)`, b)
	if err != nil {
		return err
	}
	for rows.Next() {
		var role string
		var cfgs []string
		if err := rows.Scan(&role, &cfgs); err != nil {
			rows.Close()
			return err
		}
		for _, c := range cfgs {
			s, err := SQLConfiguracao(novo, role, c)
			if err != nil {
				rows.Close()
				return err
			}
			cmds = append(cmds, s)
		}
	}
	rows.Close()

	var aclNula bool
	var limite int
	var comentario *string
	if err := adm.QueryRow(ctx, `SELECT datacl IS NULL, datconnlimit, shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, b).
		Scan(&aclNula, &limite, &comentario); err != nil {
		return err
	}
	if !aclNula {
		cmds = append(cmds, "REVOKE ALL ON DATABASE "+id(novo)+" FROM PUBLIC")
		rows, err := adm.Query(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(a.grantee)) END,
			a.privilege_type, a.is_grantable FROM pg_database d, aclexplode(d.datacl) a WHERE d.datname = $1`, b)
		if err != nil {
			return err
		}
		for rows.Next() {
			var quem, priv string
			var opcao bool
			if err := rows.Scan(&quem, &priv, &opcao); err != nil {
				rows.Close()
				return err
			}
			s := fmt.Sprintf("GRANT %s ON DATABASE %s TO %s", priv, id(novo), quem)
			if opcao && quem != "PUBLIC" {
				s += " WITH GRANT OPTION"
			}
			cmds = append(cmds, s)
		}
		rows.Close()
	}
	if limite != -1 {
		cmds = append(cmds, fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT %d", id(novo), limite))
	}
	if comentario != nil {
		cmds = append(cmds, fmt.Sprintf("COMMENT ON DATABASE %s IS %s", id(novo), lit(*comentario)))
	}
	for i, c := range cmds {
		r.progresso(i+1, len(cmds), "", false)
		r.d.logf("%s", c)
		if _, err := adm.Exec(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	return nil
}

func (r *corrida) analyze(ctx context.Context) error {
	args := []string{"--analyze-only", fmt.Sprintf("--jobs=%d", r.p.Perfil.JobsRestore),
		"--dbname=" + conexao.DSN(r.destino.c, r.destino.ponte, r.e.BancoNovo, "pghangar")}
	if err := versoes.Conferir("vacuumdb", args, r.p.Imagem); err != nil {
		return err
	}
	cod, ultimas, err := r.rodar(ctx, "analyze", append([]string{"vacuumdb"}, args...), nil, nil, nil)
	if err != nil {
		return err
	}
	if cod != 0 {
		return falhaCliente("o vacuumdb", cod, ultimas)
	}
	return nil
}

func (r *corrida) conferir(ctx context.Context) ([]string, error) {
	pf := r.p.Perfil
	conn, err := r.destino.conectar(ctx, r.d, r.e.BancoNovo)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	var difs []string
	// Com filtro de tabelas, o recorte da origem não se reproduz pela contagem do catálogo: só as
	// linhas (se pedidas) são comparadas.
	if len(pf.Tabelas) > 0 || len(pf.TabelasFora) > 0 {
		r.d.logf("conferência: com filtro de tabelas, os objetos não são comparados")
	} else if r.p.Contagem == nil {
		r.d.logf("conferência: o dump não guardou a contagem da origem; os objetos não são comparados")
	} else {
		c, err := contar(ctx, conn, pf.Schemas, pf.SchemasFora)
		if err != nil {
			return nil, err
		}
		difs = Diferencas(r.p.Contagem, c)
		if len(difs) == 0 {
			r.d.logf("conferência: %d tipos de objeto iguais na origem e no destino", len(c))
		}
	}
	if r.linhasOrigem != nil {
		r.d.logf("conferência: contando as linhas no destino")
		destino, err := contarLinhas(ctx, conn, func(i, n int, t string) { r.progresso(i, n, t, false) })
		if err != nil {
			return nil, err
		}
		dl := DiferencasLinhas(r.linhasOrigem, destino, pf.SemDados)
		if len(dl) == 0 {
			r.d.logf("conferência: as linhas de %d tabela(s) batem com a origem, no snapshot do dump", len(destino))
		}
		difs = append(difs, dl...)
	}
	for _, x := range difs {
		r.d.logf("conferência: %s", x)
	}
	return difs, nil
}

// --- troca --------------------------------------------------------------------------------------

func existeBanco(ctx context.Context, adm *pgx.Conn, nome string) (bool, error) {
	var ok bool
	err := adm.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, nome).Scan(&ok)
	return ok, err
}

// derrubar encerra as sessões de um banco, até não sobrar nenhuma (ou 10 s).
func derrubar(ctx context.Context, adm *pgx.Conn, banco string) error {
	for i := 0; i < 40; i++ {
		var restam int
		if err := adm.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid()) t`, banco).Scan(&restam); err != nil {
			return err
		}
		if restam == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("as sessões do banco %s não encerraram em 10 s", banco)
}

// anteriorLivre dá o nome do __anterior de agora. O nome tem segundos: se já existir um com o mesmo
// (desfazer logo depois de uma troca), espera o segundo seguinte.
func anteriorLivre(ctx context.Context, adm *pgx.Conn, banco string, agora time.Time) (string, error) {
	for i := 0; i < 3; i++ {
		nome := nomes.Anterior(banco, agora)
		ok, err := existeBanco(ctx, adm, nome)
		if err != nil {
			return "", err
		}
		if !ok {
			return nome, nil
		}
		espera := agora.Truncate(time.Second).Add(time.Second).Sub(time.Now()) + 10*time.Millisecond
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(max(espera, 10*time.Millisecond)):
		}
		agora = time.Now()
	}
	return "", fmt.Errorf("não foi possível escolher um nome livre para o anterior de %s", banco)
}

func renomear(ctx context.Context, adm *pgx.Conn, de, para string) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = derrubar(ctx, adm, de); err != nil {
			return err
		}
		if _, err = adm.Exec(ctx, "ALTER DATABASE "+id(de)+" RENAME TO "+id(para)); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("renomeando %s para %s: %w", de, para, err)
}

func conexoesDoBanco(ctx context.Context, adm *pgx.Conn, banco string, permitir bool) error {
	_, err := adm.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS %t", id(banco), permitir))
	return err
}

// consertar roda um conserto numa conexão viva: a de agora pode ter caído no meio do passo que
// falhou. Tenta duas vezes, com prazo próprio (o cancelamento não interrompe um conserto).
func consertar(d Deps, l *lado, oque string, f func(ctx context.Context, adm *pgx.Conn) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	for i := 0; i < 2; i++ {
		var adm *pgx.Conn
		if adm, err = l.adminVivo(ctx, d); err == nil {
			if err = f(ctx, adm); err == nil {
				return nil
			}
		}
		d.logf("consertando (%s): %v", oque, err)
	}
	return err
}

// errNovoPerdido é o __novo que sumiu ou foi trocado por outro: nada pode ser trocado.
var errNovoPerdido = errors.New("o __novo desta execução não existe mais")

// trocarNomes põe o __novo no lugar do banco: o banco vira __anterior (fechado para conexões) e o
// __novo assume o nome. Não obedece ao cancelamento (são segundos, e parar no meio derrubaria o
// destino). Se um passo falhar, confere o estado real e desfaz o que já tinha sido feito.
// trocou diz se o __novo já é o banco, mesmo quando volta erro.
func trocarNomes(ctx context.Context, d Deps, l *lado, banco, novo string, esperaExistir bool, novoOID uint32, agora time.Time) (anterior string, trocou bool, err error) {
	adm, err := l.adminVivo(ctx, d)
	if err != nil {
		return "", false, err
	}
	o, err := oid(ctx, adm, novo)
	if err != nil {
		return "", false, err
	}
	if o == 0 {
		return "", false, fmt.Errorf("%w: o banco %s não existe no destino, e nada foi trocado", errNovoPerdido, novo)
	}
	if novoOID != 0 && o != novoOID {
		return "", false, fmt.Errorf("%w: o banco %s foi apagado e recriado por outra cópia, e nada foi trocado", errNovoPerdido, novo)
	}
	// A marca da ferramenta no __novo não vai para o banco de destino. Se as configurações do banco
	// deram a ele o comentário do destino, ela já saiu.
	var comentario string
	if err := adm.QueryRow(ctx, `SELECT coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = $1`, novo).Scan(&comentario); err != nil {
		return "", false, err
	}
	if _, _, marcado := lerMarcaDoNovo(comentario); marcado {
		if _, err := adm.Exec(ctx, "COMMENT ON DATABASE "+id(novo)+" IS NULL"); err != nil {
			return "", false, err
		}
	}
	existe, err := existeBanco(ctx, adm, banco)
	if err != nil {
		return "", false, err
	}
	if existe != esperaExistir {
		return "", false, fmt.Errorf("o banco %s %s desde o plano: confirme de novo", banco, map[bool]string{true: "passou a existir", false: "deixou de existir"}[existe])
	}
	// O __novo sem ninguém conectado (nem o autovacuum) para poder ser renomeado.
	if err := conexoesDoBanco(ctx, adm, novo, false); err != nil {
		return "", false, err
	}
	if existe {
		if anterior, err = anteriorLivre(ctx, adm, banco, agora); err != nil {
			return "", false, err
		}
		if err = conexoesDoBanco(ctx, adm, banco, false); err == nil {
			d.logf("%s: conexões fechadas, derrubando as sessões", banco)
			err = renomear(ctx, adm, banco, anterior)
		}
		if err != nil {
			// O banco pode ter sido renomeado antes de a resposta chegar: confere e devolve.
			_ = consertar(d, l, "devolver "+banco, func(c context.Context, a *pgx.Conn) error {
				if ok, e := existeBanco(c, a, banco); e != nil {
					return e
				} else if !ok {
					if e := renomear(c, a, anterior, banco); e != nil {
						return e
					}
				}
				if e := conexoesDoBanco(c, a, banco, true); e != nil {
					return e
				}
				return conexoesDoBanco(c, a, novo, true)
			})
			return "", false, err
		}
		d.logf("%s → %s", banco, anterior)
	}
	if err = renomear(ctx, adm, novo, banco); err != nil {
		// Pode ter acontecido mesmo assim (a conexão caiu depois do servidor renomear).
		aconteceu := false
		_ = consertar(d, l, "conferir a troca", func(c context.Context, a *pgx.Conn) error {
			ob, e := oid(c, a, banco)
			if e != nil {
				return e
			}
			if ob == o {
				aconteceu = true
				return conexoesDoBanco(c, a, banco, true)
			}
			if existe && ob == 0 {
				if e := renomear(c, a, anterior, banco); e != nil {
					return e
				}
				if e := conexoesDoBanco(c, a, banco, true); e != nil {
					return e
				}
			}
			return conexoesDoBanco(c, a, novo, true)
		})
		if aconteceu {
			d.logf("%s → %s (confirmado depois da falha: %v)", novo, banco, err)
			return anterior, true, nil
		}
		return "", false, err
	}
	d.logf("%s → %s", novo, banco)
	if err := conexoesDoBanco(ctx, adm, banco, true); err != nil {
		if e2 := consertar(d, l, "abrir "+banco, func(c context.Context, a *pgx.Conn) error { return conexoesDoBanco(c, a, banco, true) }); e2 != nil {
			return anterior, true, fmt.Errorf("a troca aconteceu, mas o banco %s ficou fechado para conexões: rode ALTER DATABASE %s ALLOW_CONNECTIONS true (%v)", banco, id(banco), e2)
		}
	}
	return anterior, true, nil
}

// TamanhoDir soma os arquivos de um diretório.
func TamanhoDir(dir string) int64 {
	var t int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			t += fi.Size()
		}
		return nil
	})
	return t
}

// EscreverManifesto grava o manifesto.json do dump (600).
func EscreverManifesto(dir string, m Manifesto) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".manifesto.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifesto.json"))
}

// --- banco base e reset -------------------------------------------------------------------------

// estrategiaCopia é o STRATEGY do CREATE DATABASE … TEMPLATE: FILE_COPY copia os arquivos (e, no
// 18 com file_copy_method = clone num sistema de arquivos com reflink, clona em milissegundos).
// Antes do 15, não há a opção.
func estrategiaCopia(versao int) string {
	if versao >= 150000 {
		return " STRATEGY FILE_COPY"
	}
	return ""
}

// guardarBase cria <banco>__base a partir do __novo, antes da troca: fechado para conexões, dono o
// superusuário da conexão. A base anterior é substituída (o plano mostrou isso, e o sysadmin
// confirmou).
func (r *corrida) guardarBase(ctx context.Context) error {
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	b := r.p.Destino.Banco
	base, novo := nomes.BancoBase(b), r.e.BancoNovo
	usados, err := bancosDePerfis(ctx, r.d, r.p.Perfil.Destino)
	if err != nil {
		return err
	}
	if usados[base] {
		return fmt.Errorf("o banco %s é o banco de um perfil: a base não o substitui", base)
	}
	// A base antiga sai do caminho com outro nome e só é apagada depois de a nova existir: se a
	// nova falhar (o disco cheio), a antiga volta, e a troca espera a decisão do sysadmin.
	velha := ""
	if ok, err := existeBanco(ctx, adm, base); err != nil {
		return err
	} else if ok {
		velha = nomes.BaseVelha(b)
		if ok, err := existeBanco(ctx, adm, velha); err != nil {
			return err
		} else if ok {
			// Uma sobra de uma substituição que morreu no meio.
			if _, err := adm.Exec(ctx, "DROP DATABASE "+id(velha)+" WITH (FORCE)"); err != nil {
				return fmt.Errorf("apagando %s: %w", velha, err)
			}
		}
		r.d.logf("substituindo a base anterior %s (confirmado no plano)", base)
		if err := conexoesDoBanco(ctx, adm, base, false); err != nil {
			return err
		}
		if err := derrubar(ctx, adm, base); err != nil {
			return err
		}
		if err := renomear(ctx, adm, base, velha); err != nil {
			return fmt.Errorf("guardando a base anterior: %w", err)
		}
	}
	falhou := func(err error) error {
		msg := fmt.Sprintf("a base não foi guardada (%v)", err)
		if velha != "" {
			if e := renomear(ctx, adm, velha, base); e != nil {
				msg += fmt.Sprintf("; a base anterior ficou como %s (%v)", velha, e)
			} else {
				msg += "; a base anterior foi mantida"
			}
		}
		_ = conexoesDoBanco(ctx, adm, novo, true)
		return &aguardar{motivo: msg + fmt.Sprintf(": a troca espera a sua decisão. O banco %s está pronto no destino.", novo)}
	}
	// O modelo não pode ter ninguém conectado (nem o autovacuum).
	if err := conexoesDoBanco(ctx, adm, novo, false); err != nil {
		return falhou(err)
	}
	if err := derrubar(ctx, adm, novo); err != nil {
		return falhou(err)
	}
	cmds := []string{
		"CREATE DATABASE " + id(base) + " TEMPLATE " + id(novo) + estrategiaCopia(r.p.Destino.VersaoNum) + " OWNER " + id(r.destino.c.Usuario),
		"REVOKE ALL ON DATABASE " + id(base) + " FROM PUBLIC",
		"ALTER DATABASE " + id(base) + " ALLOW_CONNECTIONS false",
		"COMMENT ON DATABASE " + id(base) + " IS " + lit(fmt.Sprintf("pghangar: base de %s, da cópia #%d de %s/%s em %s",
			b, r.e.ID, r.p.Origem.Conexao, r.p.Origem.Banco, time.Now().Format("2006-01-02 15:04"))),
	}
	for i, c := range cmds {
		r.d.logf("%s", c)
		if _, err := adm.Exec(ctx, c); err != nil {
			if i == 0 {
				return falhou(err)
			}
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	if velha != "" {
		if _, err := adm.Exec(ctx, "DROP DATABASE "+id(velha)+" WITH (FORCE)"); err != nil {
			r.d.logf("a base anterior ficou como %s: %v (apague-a na aba Anteriores)", velha, err)
		}
	}
	return nil
}

// resetar recria o destino a partir da base: o __novo vem da base (CREATE DATABASE … TEMPLATE),
// ganha as configurações do banco de agora, e troca de nome como numa cópia.
func (r *corrida) resetar(ctx context.Context) error {
	if len(r.e.Apagar) > 0 {
		r.etapa("Anteriores")
		if err := r.apagarAnteriores(ctx); err != nil {
			return err
		}
	}
	r.etapa("Criar __novo")
	adm, err := r.destino.adminVivo(ctx, r.d)
	if err != nil {
		return err
	}
	b := r.p.Destino.Banco
	base, novo := nomes.BancoBase(b), r.p.Novo
	stmt := "CREATE DATABASE " + id(novo) + " TEMPLATE " + id(base) + estrategiaCopia(r.p.Destino.VersaoNum) + " OWNER " + id(r.dono())
	r.d.logf("%s", stmt)
	inicio := time.Now()
	if _, err := adm.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("criando %s a partir da base: %w", novo, err)
	}
	r.d.logf("%s criado a partir de %s em %s", novo, base, time.Since(inicio).Round(time.Millisecond))
	r.e.BancoNovo = novo
	if r.e.NovoOID, err = oid(ctx, adm, novo); err != nil {
		return err
	}
	r.gravar()
	// A base é fechada; o banco novo nasce aberto como um banco novo, com as permissões e as
	// configurações do destino de agora.
	if r.p.Destino.Existe {
		r.etapa("Configurações do banco")
		if err := r.configuracoes(ctx); err != nil {
			return err
		}
	}
	r.etapa("Troca")
	ctxTroca, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	ant, trocou, err := trocarNomes(ctxTroca, r.d, r.destino, b, novo, r.p.Destino.Existe, r.e.NovoOID, time.Now())
	r.e.BancoAnterior, r.trocou = ant, trocou
	if err != nil {
		return err
	}
	r.e.Mensagem = fmt.Sprintf("%s resetado a partir de %s", b, base)
	if ant != "" {
		r.e.Mensagem += "; o banco substituído ficou como " + ant
	}
	return nil
}
