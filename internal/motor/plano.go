// Package motor faz a cópia (docs/ESTRATEGIA.md §7): o plano com as checagens, o dump, o banco
// __novo, o restore, os donos, as configurações do banco, o script pós-restore, o ANALYZE, a
// conferência e a troca de nomes. Também desfaz uma troca e apaga os bancos da ferramenta.
//
// Nada aqui apaga sem pergunta: os anteriores só saem quando o sysadmin os marca na confirmação, e
// o __novo de uma cópia que falhou fica.
package motor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/local"
	"github.com/9LEVEL/pghangar/internal/nomes"
	"github.com/9LEVEL/pghangar/internal/trava"
	"github.com/9LEVEL/pghangar/internal/tunel"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// Docker é o que o motor usa do Docker. Os testes trocam por um falso.
type Docker interface {
	Versao(ctx context.Context) (string, error)
	Existe(ctx context.Context, ref string) bool
	Rodar(ctx context.Context, e imagens.Execucao, linha imagens.Linha) (int, error)
}

// Deps é o que o motor precisa de fora.
type Deps struct {
	Cadastro *cadastro.Cadastro
	Dir      local.Dir
	Docker   Docker
	Amb      conexao.Ambiente
	Seg      conexao.Segredos
	Log      io.Writer // o log da execução; nil descarta
	Maquina  string    // o hostname, para o application_name

	// TravaObtida: quem chama já segura a trava do destino (o processo da execução). Sem isso,
	// o plano veria a própria trava como "outra cópia em andamento".
	TravaObtida bool

	// EsperasRede são as esperas antes de refazer o dump que caiu por rede (nil: 10 s, 30 s, 90 s).
	// Os testes encurtam.
	EsperasRede []time.Duration
}

func (d Deps) logf(formato string, args ...any) {
	if d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(formato, args...))
}

// Lado é a origem ou o destino como o plano os viu.
type Lado struct {
	Conexao      string         `json:"conexao"`
	Tag          string         `json:"tag"`
	Onde         string         `json:"onde"`
	VersaoNum    int            `json:"versao_num"`
	SystemID     string         `json:"system_id"`
	Recuperacao  bool           `json:"recuperacao"`
	Superusuario bool           `json:"superusuario"`
	Usuario      string         `json:"usuario"`
	Banco        string         `json:"banco"`
	Existe       bool           `json:"existe"`
	Info         cadastro.Banco `json:"info"`
}

// Anterior é um banco __anterior do destino.
type Anterior struct {
	Nome    string    `json:"nome"`
	Tamanho int64     `json:"tamanho"`
	Data    time.Time `json:"data"`
}

// Sessao é uma conexão ativa no banco de destino, que a troca vai derrubar.
type Sessao struct {
	PID       int    `json:"pid"`
	Usuario   string `json:"usuario"`
	Aplicacao string `json:"aplicacao"`
	Cliente   string `json:"cliente"`
	Estado    string `json:"estado"`
}

// Extensao é uma extensão do banco de origem.
type Extensao struct {
	Nome   string `json:"nome"`
	Versao string `json:"versao"`
}

// Plano é tudo o que a confirmação mostra, e o que a execução confere de novo antes de começar.
type Plano struct {
	Perfil     cadastro.Perfil `json:"perfil"`
	Origem     Lado            `json:"origem"`
	Destino    Lado            `json:"destino"`
	Imagem     int             `json:"imagem"`
	ImagemRef  string          `json:"imagem_ref"`
	Cliente    string          `json:"cliente"`
	DirDumps   string          `json:"dir_dumps"`
	Anteriores []Anterior      `json:"anteriores"`
	Novo       string          `json:"novo"`
	NovoExiste bool            `json:"novo_existe"`
	Sessoes    []Sessao        `json:"sessoes"`
	Extensoes  []Extensao      `json:"extensoes"`
	Tabelas    int             `json:"tabelas"` // para o progresso do dump
	// ObjetosGrandes é o número de large objects da origem (o modo link instável não os leva).
	ObjetosGrandes int      `json:"objetos_grandes,omitempty"`
	Contagem       Contagem `json:"contagem"`
	Citadas        []string `json:"citadas,omitempty"` // roles que a RLS e os user mappings da origem citam
	// DumpGuardado é o dump que uma restauração usa (vazio numa cópia da origem).
	DumpGuardado string `json:"dump_guardado,omitempty"`
	// Reset: o destino é recriado a partir da base, sem ir à origem.
	Reset bool `json:"reset,omitempty"`
	// Base é o <banco>__base do destino, quando existe: no reset, a fonte; numa cópia com "guardar
	// base", a que vai ser substituída.
	Base *Anterior `json:"base,omitempty"`
	// BaseDescricao é o comentário da base (de que cópia ela veio).
	BaseDescricao string `json:"base_descricao,omitempty"`
	// Retomar é o dump em blocos incompleto que esta cópia retoma (modo link instável).
	Retomar     string    `json:"retomar,omitempty"`
	RetomarInfo string    `json:"retomar_info,omitempty"`
	Bloqueios   []string  `json:"bloqueios"`
	Avisos      []string  `json:"avisos"`
	Notas       []string  `json:"notas"`
	CriadoEm    time.Time `json:"criado_em"`
}

// Bloqueado diz se a cópia não pode começar.
func (p Plano) Bloqueado() bool { return len(p.Bloqueios) > 0 }

// Confirmacao é o que o sysadmin digita para confirmar: o nome do banco num destino homolog; "y"
// num destino dev.
func (p Plano) Confirmacao() string {
	if p.Destino.Tag == cadastro.TagHomolog {
		return p.Destino.Banco
	}
	return ""
}

func (p *Plano) bloquear(f string, a ...any) { p.Bloqueios = append(p.Bloqueios, fmt.Sprintf(f, a...)) }
func (p *Plano) avisar(f string, a ...any)   { p.Avisos = append(p.Avisos, fmt.Sprintf(f, a...)) }

// Pergunta é o que a tela precisa perguntar antes de o plano seguir: uma senha, uma passphrase ou
// a chave de um servidor SSH desconhecido.
type Pergunta struct {
	Conexao          string
	Senha            bool
	Frase            string // o caminho da chave
	HostDesconhecido *tunel.HostDesconhecido
	Motivo           string // "a senha informada está errada", quando é para perguntar de novo
}

func (p *Pergunta) Error() string {
	switch {
	case p.HostDesconhecido != nil:
		return p.HostDesconhecido.Error()
	case p.Frase != "":
		return "informe a passphrase da chave " + p.Frase
	}
	return "informe a senha da conexão " + p.Conexao
}

// pergunta transforma os erros que pedem algo ao sysadmin numa Pergunta.
func pergunta(nome string, err error) error {
	var hd *tunel.HostDesconhecido
	var pf *tunel.PrecisaFrase
	var ps *conexao.PrecisaSenha
	switch {
	case errors.As(err, &hd):
		return &Pergunta{Conexao: nome, HostDesconhecido: hd}
	case errors.As(err, &pf):
		p := &Pergunta{Conexao: nome, Frase: pf.Chave}
		if pf.Errada {
			p.Motivo = "a passphrase informada está errada"
		}
		return p
	case errors.As(err, &ps):
		return &Pergunta{Conexao: nome, Senha: true}
	}
	return nil
}

// lado é uma ponta aberta: a ponte, a senha e a conexão com o banco administrativo.
type lado struct {
	c     cadastro.Conexao
	ponte *conexao.Ponte
	senha string
	admin *pgx.Conn
}

func (l *lado) fechar() {
	if l == nil {
		return
	}
	if l.admin != nil {
		_ = l.admin.Close(context.Background())
	}
	l.ponte.Fechar()
}

func (l *lado) conectar(ctx context.Context, d Deps, banco string) (*pgx.Conn, error) {
	return conexao.Conectar(ctx, l.c, l.ponte, l.senha, banco, d.Amb.Prazo)
}

// adminVivo devolve a conexão administrativa, reaberta se tiver caído: ela fica parada durante o
// dump e o restore, que podem levar horas (idle_session_timeout, reinício do servidor).
func (l *lado) adminVivo(ctx context.Context, d Deps) (*pgx.Conn, error) {
	if l.admin != nil {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := l.admin.Ping(c)
		cancel()
		if err == nil {
			return l.admin, nil
		}
		_ = l.admin.Close(context.Background())
		l.admin = nil
	}
	c, err := l.conectar(ctx, d, l.c.BancoAdmin)
	if err != nil {
		return nil, err
	}
	l.admin = c
	return c, nil
}

// abrirLado abre a ponte e a conexão administrativa, e relê o servidor (a versão gravada no
// cadastro é atualizada aqui: a que decide é a de agora).
func abrirLado(ctx context.Context, d Deps, nome string) (*lado, cadastro.Info, error) {
	c, err := d.Cadastro.Conexao(ctx, nome)
	if err != nil {
		return nil, cadastro.Info{}, err
	}
	senha, err := conexao.Senha(c, d.Seg)
	if err != nil {
		if p := pergunta(nome, err); p != nil {
			return nil, c.Info, p
		}
		return nil, c.Info, err
	}
	if c.Acesso == cadastro.AcessoSSH {
		if chave := conexao.Chave(c, d.Amb); chave != "" {
			if precisa, err := tunel.PrecisaDeFrase(chave); err == nil && precisa && d.Seg.Frases[chave] == "" {
				return nil, c.Info, &Pergunta{Conexao: nome, Frase: chave}
			}
		}
	}
	ponte, err := conexao.AbrirPonte(ctx, c, d.Amb, d.Seg)
	if err != nil {
		if p := pergunta(nome, err); p != nil {
			return nil, c.Info, p
		}
		return nil, c.Info, fmt.Errorf("%s: %w", nome, err)
	}
	l := &lado{c: c, ponte: ponte, senha: senha}
	l.admin, err = l.conectar(ctx, d, c.BancoAdmin)
	if err != nil {
		ponte.Fechar()
		// Uma senha informada na hora e errada volta como pergunta: a tela pede de novo.
		var f *conexao.FalhaPG
		if errors.As(err, &f) && f.Codigo == "28P01" && c.ModoSenha == cadastro.SenhaPerguntar {
			return nil, c.Info, &Pergunta{Conexao: nome, Senha: true, Motivo: "a senha informada está errada"}
		}
		return nil, c.Info, fmt.Errorf("%s: %w", nome, err)
	}
	anterior := c.Info
	info := cadastro.Info{VerificadaEm: time.Now()}
	if err := conexao.LerServidor(ctx, l.admin, &info); err != nil {
		l.fechar()
		return nil, anterior, fmt.Errorf("%s: lendo o servidor: %w", nome, err)
	}
	_ = d.Cadastro.GravarInfo(ctx, nome, info)
	l.c.Info = info
	return l, anterior, nil
}

func banco(info cadastro.Info, nome string) (cadastro.Banco, bool) {
	for _, b := range info.Bancos {
		if b.Nome == nome {
			return b, true
		}
	}
	return cadastro.Banco{}, false
}

// Planejar faz as checagens (docs/ESTRATEGIA.md §7, etapa 1) sem escrever nada. Os problemas
// viram bloqueios e avisos no plano; o erro é só para o que impede de planejar, e uma *Pergunta
// quando falta algo que o sysadmin informa.
func Planejar(ctx context.Context, d Deps, nomePerfil string) (Plano, error) {
	return planejar(ctx, d, nomePerfil, nil, "")
}

// PlanejarRestauracao planeja restaurar de novo um dump guardado, sem ir à origem: o destino é o do
// perfil do dump, e o que a origem tinha vem do manifesto.
func PlanejarRestauracao(ctx context.Context, d Deps, dumpDir string) (Plano, error) {
	m, err := LerManifesto(dumpDir)
	if err != nil {
		return Plano{}, err
	}
	if m.Estado != DumpCompleto {
		return Plano{}, fmt.Errorf("o dump de %s está %s: só um dump completo pode ser restaurado", m.Inicio.Format("02/01 15:04"), m.Estado)
	}
	return planejar(ctx, d, m.Perfil, &m, dumpDir)
}

// Filtros são as listas do perfil que recortam o dump (guardadas no manifesto).
type Filtros struct {
	Schemas     []string `json:"schemas,omitempty"`
	SchemasFora []string `json:"schemas_fora,omitempty"`
	Tabelas     []string `json:"tabelas,omitempty"`
	TabelasFora []string `json:"tabelas_fora,omitempty"`
}

func planejar(ctx context.Context, d Deps, nomePerfil string, guardado *Manifesto, dumpDir string) (Plano, error) {
	p := Plano{CriadoEm: time.Now()}
	perfil, err := d.Cadastro.Perfil(ctx, nomePerfil)
	if err != nil {
		return p, err
	}
	if guardado != nil {
		// O recorte e as tabelas sem dados são os do dump, e não os de agora do perfil.
		perfil.Schemas, perfil.SchemasFora, perfil.Tabelas, perfil.TabelasFora = guardado.Filtros.Schemas, guardado.Filtros.SchemasFora, guardado.Filtros.Tabelas, guardado.Filtros.TabelasFora
		perfil.SemDados = guardado.SemDados
		perfil.ConferirLinhas = guardado.Linhas != nil
		p.DumpGuardado = dumpDir
	}
	p.Perfil = perfil
	p.Novo = nomes.Novo(perfil.DestinoBanco)
	p.DirDumps = DirDumps(d.Dir, perfil)

	cDest, err := d.Cadastro.Conexao(ctx, perfil.Destino)
	if err != nil {
		return p, err
	}
	// A regra que não depende de nada: um banco prod nunca é destino.
	if cDest.Tag == cadastro.TagProd {
		p.bloquear("a conexão de destino %s é prod: um banco prod nunca é destino", cDest.Nome)
		return p, nil
	}
	if perfil.DestinoBanco == cDest.BancoAdmin || perfil.DestinoBanco == "template0" || perfil.DestinoBanco == "template1" {
		p.bloquear("o banco de destino %s é o banco administrativo ou um template: escolha outro", perfil.DestinoBanco)
		return p, nil
	}
	if _, err := d.Docker.Versao(ctx); err != nil {
		p.bloquear("%v", err)
	}

	// A origem: aberta numa cópia; lida do manifesto numa restauração.
	var origem *lado
	var infoO, antesOrigem cadastro.Info
	if guardado == nil {
		origem, antesOrigem, err = abrirLado(ctx, d, perfil.Origem)
		if err != nil {
			var pg *Pergunta
			if errors.As(err, &pg) {
				return p, pg
			}
			p.bloquear("origem inacessível: %v", err)
			return p, nil
		}
		defer origem.fechar()
		infoO = origem.c.Info
		p.Origem = Lado{Conexao: origem.c.Nome, Tag: origem.c.Tag, Onde: origem.c.Onde(), VersaoNum: infoO.VersaoNum, SystemID: infoO.SystemID,
			Recuperacao: infoO.Recuperacao, Superusuario: infoO.Superusuario, Usuario: origem.c.Usuario, Banco: perfil.OrigemBanco}
	} else {
		p.Origem = guardado.Origem
		infoO = cadastro.Info{VersaoNum: guardado.Origem.VersaoNum, SystemID: guardado.Origem.SystemID, Superusuario: true}
		p.Contagem, p.Extensoes, p.Citadas = guardado.Contagem, guardado.Extensoes, guardado.Citadas
	}
	destino, antesDestino, err := abrirLado(ctx, d, perfil.Destino)
	if err != nil {
		var pg *Pergunta
		if errors.As(err, &pg) {
			return p, pg
		}
		p.bloquear("destino inacessível: %v", err)
		return p, nil
	}
	defer destino.fechar()
	infoD := destino.c.Info
	p.Destino = Lado{Conexao: destino.c.Nome, Tag: destino.c.Tag, Onde: destino.c.Onde(), VersaoNum: infoD.VersaoNum, SystemID: infoD.SystemID,
		Recuperacao: infoD.Recuperacao, Superusuario: infoD.Superusuario, Usuario: destino.c.Usuario, Banco: perfil.DestinoBanco}
	for _, x := range []struct {
		nome  string
		antes cadastro.Info
		agora cadastro.Info
	}{{perfil.Origem, antesOrigem, infoO}, {destino.c.Nome, antesDestino, infoD}} {
		if x.antes.VersaoNum != 0 && versoes.Major(x.antes.VersaoNum) != versoes.Major(x.agora.VersaoNum) {
			p.avisar("a versão de %s mudou de %s para %s desde a última verificação: o cadastro foi atualizado", x.nome,
				versoes.Texto(x.antes.VersaoNum), versoes.Texto(x.agora.VersaoNum))
		}
	}

	// Um destino que seja o servidor de uma conexão prod (sem a tag, por engano) é recusado.
	if err := guardaProd(ctx, d, destino.c, infoD.SystemID); err != nil {
		p.bloquear("%v", err)
		return p, nil
	}
	if !d.TravaObtida && !trava.Livre(d.Dir.Travas(), trava.Destino(perfil.Destino, infoD.SystemID), perfil.DestinoBanco) {
		p.bloquear("há outra cópia em andamento para %s em %s", perfil.DestinoBanco, perfil.Destino)
		return p, nil
	}
	// A terceira barreira (depois da tag e do servidor da produção): o servidor de destino precisa
	// ter sido aprovado uma vez, de propósito, como destino de cópias.
	aprovados, err := d.Cadastro.DestinosAprovados(ctx)
	if err != nil {
		return p, err
	}
	if _, ok := aprovados[infoD.SystemID]; !ok || infoD.SystemID == "" {
		p.bloquear("o servidor de %s ainda não foi aprovado como destino de cópias: aprove-o uma vez na aba Conexões (tecla v)", destino.c.Nome)
	}

	// O mesmo cluster, visto por dois nomes, é a cópia em cima da própria origem.
	switch {
	case infoO.SystemID != "" && infoO.SystemID == infoD.SystemID:
		p.bloquear("a origem e o destino são o mesmo cluster (system_identifier %s)", infoO.SystemID)
	case infoO.SystemID == "" || infoD.SystemID == "":
		p.avisar("não foi possível comparar o system_identifier (falta superusuário?): confira que origem e destino são servidores diferentes")
	}
	if !infoD.Superusuario {
		p.bloquear("o usuário %s não é superusuário no destino: o restore precisa de superusuário", destino.c.Usuario)
	}
	if infoD.Recuperacao {
		p.bloquear("o destino %s é uma réplica (somente leitura)", destino.c.Nome)
	}
	if origem != nil && !infoO.Superusuario {
		p.avisar("o usuário %s não é superusuário na origem: tabelas com RLS e objetos restritos podem falhar no dump", origem.c.Usuario)
	}
	if origem != nil && infoO.Recuperacao {
		p.avisar("a origem é uma réplica: um dump longo pode ser cancelado por conflito de recuperação (max_standby_streaming_delay)")
	}

	// Versões e imagem. Numa restauração, o que manda é a versão do pg_dump que gerou o dump: um
	// pg_restore mais antigo não lê o arquivo dele.
	cfg, err := d.Cadastro.Config(ctx)
	if err != nil {
		return p, err
	}
	versaoOrigem := versoes.Major(infoO.VersaoNum)
	if guardado != nil {
		versaoOrigem = max(versaoOrigem, guardado.Imagem)
	}
	img, err := versoes.Escolher(versaoOrigem, versoes.Major(infoD.VersaoNum), cfg.Versoes)
	if err != nil {
		p.bloquear("%v", err)
	} else {
		p.Imagem = img
		is, err := d.Cadastro.Imagens(ctx)
		if err != nil {
			return p, err
		}
		i, ok := is[img]
		switch {
		case !ok:
			p.bloquear("a imagem da versão %d não foi baixada: baixe na aba Ambiente", img)
		case !d.Docker.Existe(ctx, i.Digest):
			p.bloquear("a imagem travada da versão %d (%s) não está mais no Docker local: baixe de novo na aba Ambiente", img, i.Digest)
		default:
			p.ImagemRef, p.Cliente = i.Digest, i.Cliente
			if imagens.MajorDoCliente(i.Cliente) != img {
				p.bloquear("a imagem da versão %d tem o cliente %s: baixe de novo na aba Ambiente", img, i.Cliente)
			}
		}
	}

	// Os bancos.
	var bo cadastro.Banco
	if guardado == nil {
		var ok bool
		if bo, ok = banco(infoO, perfil.OrigemBanco); !ok {
			p.bloquear("o banco %s não existe na origem %s", perfil.OrigemBanco, origem.c.Nome)
			return p, nil
		}
		p.Origem.Existe, p.Origem.Info = true, bo
	} else {
		bo = guardado.Origem.Info
	}
	if bd, ok := banco(infoD, perfil.DestinoBanco); ok {
		p.Destino.Existe, p.Destino.Info = true, bd
		if bo.Collate != bd.Collate || bo.Ctype != bd.Ctype || bo.Provedor != bd.Provedor || bo.Locale != bd.Locale {
			p.avisar("o locale difere: a origem usa %s, o destino %s. O __novo nasce com o do destino; a ordenação de textos pode mudar",
				descreverLocale(bo), descreverLocale(bd))
		}
		if bo.Codificacao != bd.Codificacao {
			p.avisar("a codificação difere: a origem é %s e o destino %s", bo.Codificacao, bd.Codificacao)
		}
	} else {
		p.Destino.Info = bo
		p.Destino.Info.Dono = destino.c.Usuario
		p.avisar("o banco %s não existe no destino: vai ser criado, com o locale da origem e o dono %s", perfil.DestinoBanco, destino.c.Usuario)
	}
	if _, ok := banco(infoD, p.Novo); ok {
		p.NovoExiste = true
		p.bloquear("sobrou o banco %s de uma cópia anterior no destino: apague-o na aba Anteriores antes de copiar", p.Novo)
	}

	// A origem por dentro: extensões, tabelas, a contagem e as roles citadas.
	if origem != nil {
		if pare, err := lerOrigem(ctx, d, &p, origem, perfil, infoO); err != nil || pare {
			return p, err
		}
	}

	// As roles que a origem cita (RLS, user mappings) e que não existem no destino.
	if len(p.Citadas) > 0 {
		existem, err := nomesDe(ctx, destino.admin, `SELECT rolname FROM pg_roles WHERE rolname = ANY($1)`, p.Citadas)
		if err != nil {
			return p, err
		}
		tem := map[string]bool{}
		for _, x := range existem {
			tem[x] = true
		}
		var faltam []string
		for _, x := range p.Citadas {
			if !tem[x] {
				faltam = append(faltam, x)
			}
		}
		if len(faltam) > 0 {
			p.avisar("as políticas de RLS (ou os user mappings) da origem citam as roles %s, que não existem no destino: o restore vai dar erro nesses objetos, e a troca vai esperar a sua decisão. Crie as roles no destino antes, para uma cópia limpa", strings.Join(faltam, ", "))
		}
	}

	// O destino: extensões disponíveis, anteriores e sessões.
	for _, e := range p.Extensoes {
		var padrao *string
		err := destino.admin.QueryRow(ctx, `SELECT default_version FROM pg_available_extensions WHERE name = $1`, e.Nome).Scan(&padrao)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			p.bloquear("a extensão %s (usada na origem) não está disponível no destino: instale-a no servidor de destino", e.Nome)
		case err != nil:
			return p, err
		case padrao != nil && *padrao != e.Versao:
			p.avisar("a extensão %s está na versão %s na origem e na %s no destino", e.Nome, e.Versao, *padrao)
		}
	}
	usados, err := bancosDePerfis(ctx, d, perfil.Destino)
	if err != nil {
		return p, err
	}
	if p.Anteriores, err = anteriores(ctx, destino.admin, perfil.DestinoBanco, usados); err != nil {
		return p, err
	}
	if usados[p.Novo] {
		p.bloquear("o banco %s, que seria o __novo, é usado por outro perfil: renomeie o banco de destino", p.Novo)
	}
	if p.Destino.Existe {
		if p.Sessoes, err = sessoes(ctx, destino.admin, perfil.DestinoBanco); err != nil {
			return p, err
		}
	}
	if perfil.GuardarBase {
		if err := lerBase(ctx, &p, destino.admin, perfil.DestinoBanco); err != nil {
			return p, err
		}
		if p.Base != nil {
			p.Notas = append(p.Notas, fmt.Sprintf("a base atual (%s, %s) vai ser SUBSTITUÍDA pela desta cópia, antes da troca", p.Base.Nome, Tamanho(p.Base.Tamanho)))
		} else {
			p.Notas = append(p.Notas, "esta cópia guarda "+nomes.BancoBase(perfil.DestinoBanco)+": depois, dá para resetar o destino sem ir à origem")
		}
	}
	if orfas, err := rolesOrfas(ctx, d, destino.admin); err == nil && len(orfas) > 0 {
		p.avisar("sobrou no destino a role temporária %s, de uma execução que não terminou: esta cópia a neutraliza e remove", strings.Join(orfas, ", "))
	}

	// O disco do destino, quando ele está neste servidor: o __novo ocupa perto do banco de origem.
	if destino.c.Acesso == cadastro.AcessoDireto && (ehLocal(destino.c.Host) || strings.HasPrefix(destino.c.Host, "/")) {
		var dd string
		if err := destino.admin.QueryRow(ctx, "SHOW data_directory").Scan(&dd); err == nil {
			if _, err := os.Stat(dd); err == nil {
				var st syscall.Statfs_t
				if syscall.Statfs(dd, &st) == nil {
					livre := int64(st.Bavail) * int64(st.Bsize)
					if livre < bo.Tamanho+bo.Tamanho/10 {
						p.avisar("o disco do destino (%s) tem %s livres, e o banco novo deve ocupar perto de %s: o restore pode encher o disco", dd, Tamanho(livre), Tamanho(bo.Tamanho))
					}
				}
			}
		}
	}

	if guardado != nil {
		p.Notas = append(p.Notas, fmt.Sprintf("restaura o dump de %s, feito da origem %s/%s com o pg_dump %s: a origem não é tocada",
			guardado.Inicio.Format("02/01/2006 15:04"), guardado.Origem.Conexao, guardado.Origem.Banco, guardado.Cliente))
		return p, nil
	}

	// O disco dos dumps.
	if livre, err := espacoLivre(p.DirDumps); err == nil && livre < bo.Tamanho {
		p.avisar("o diretório dos dumps (%s) tem %s livres e o banco de origem ocupa %s (o dump costuma ser menor, por ser comprimido)",
			p.DirDumps, Tamanho(livre), Tamanho(bo.Tamanho))
	}
	p.Notas = append(p.Notas,
		"o pg_dump segura um lock leve (ACCESS SHARE) em cada tabela da origem até o fim: uma migration na origem durante o dump fica esperando, e as consultas seguintes fazem fila atrás dela")
	if perfil.Filtrado() {
		var partes []string
		for _, x := range []struct {
			r string
			l []string
		}{{"só os schemas", perfil.Schemas}, {"sem os schemas", perfil.SchemasFora}, {"só as tabelas", perfil.Tabelas}, {"sem as tabelas", perfil.TabelasFora}} {
			if len(x.l) > 0 {
				partes = append(partes, x.r+" "+strings.Join(x.l, ", "))
			}
		}
		p.Notas = append(p.Notas, "o perfil copia só parte do banco: "+strings.Join(partes, "; ")+". O que ficar de fora e for citado pelo que vem (uma chave estrangeira, por exemplo) dá erro no restore")
	}
	if perfil.ConferirLinhas && !perfil.Retomavel {
		p.Notas = append(p.Notas, "a conferência conta as linhas de cada tabela na origem, no mesmo snapshot do dump: é uma leitura completa de cada tabela, depois do dump")
	}
	if perfil.Retomavel {
		p.Notas = append(p.Notas, "modo link instável: o esquema vai pelo pg_dump, e os dados em blocos pela chave, retomados de onde pararam se o túnel cair. Sem snapshot único: uma chave estrangeira que não bata aparece como erro no restore, e a troca espera a sua decisão")
		if p.Retomar, p.RetomarInfo = retomavel(p.DirDumps, p); p.Retomar != "" {
			p.Notas = append(p.Notas, "retoma o dump incompleto de "+p.RetomarInfo+" ("+p.Retomar+")")
		}
		if p.ObjetosGrandes > 0 {
			p.avisar("a origem tem %d large object(s): o modo link instável não os leva", p.ObjetosGrandes)
		}
	}
	return p, nil
}

// lerOrigem olha a origem por dentro: extensões, tabelas, a contagem e as roles citadas. pare diz
// que o plano já está bloqueado.
func lerOrigem(ctx context.Context, d Deps, p *Plano, origem *lado, perfil cadastro.Perfil, infoO cadastro.Info) (pare bool, err error) {
	cob, err := origem.conectar(ctx, d, perfil.OrigemBanco)
	if err != nil {
		p.bloquear("não foi possível abrir o banco de origem: %v", err)
		return true, nil
	}
	defer cob.Close(context.Background())
	if p.Extensoes, err = extensoes(ctx, cob); err != nil {
		return true, err
	}
	if p.Contagem, err = contar(ctx, cob, perfil.Schemas, perfil.SchemasFora); err != nil {
		return true, err
	}
	// Roles que a origem cita e que o --no-owner/--no-privileges não tira: as das políticas de RLS
	// e as dos user mappings. Sem elas no destino, o restore dá erro nesses objetos.
	if p.Citadas, err = nomesDe(ctx, cob, `SELECT DISTINCT r.rolname FROM pg_policy p CROSS JOIN LATERAL unnest(p.polroles) AS rid
		JOIN pg_roles r ON r.oid = rid UNION SELECT DISTINCT usename FROM pg_user_mappings WHERE umuser <> 0 ORDER BY 1`); err != nil {
		return true, err
	}
	if err := cob.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`).Scan(&p.Tabelas); err != nil {
		return true, err
	}
	_ = cob.QueryRow(ctx, `SELECT count(*) FROM pg_largeobject_metadata`).Scan(&p.ObjetosGrandes)
	var eventos int
	_ = cob.QueryRow(ctx, `SELECT count(*) FROM pg_event_trigger`).Scan(&eventos)
	if eventos > 0 {
		p.avisar("a origem tem %d event trigger(s): só um superusuário é dono deles, e eles ficam com o usuário da conexão no destino", eventos)
	}
	if !infoO.Superusuario {
		var rls int
		_ = cob.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relrowsecurity`).Scan(&rls)
		if rls > 0 {
			p.bloquear("a origem tem %d tabela(s) com RLS e o usuário %s não é superusuário: o dump falharia nelas", rls, origem.c.Usuario)
		}
	}
	return false, nil
}

func descreverLocale(b cadastro.Banco) string {
	switch b.Provedor {
	case "i":
		return "ICU " + b.Locale
	case "b":
		return "builtin " + b.Locale
	}
	return b.Collate
}

func extensoes(ctx context.Context, conn *pgx.Conn) ([]Extensao, error) {
	rows, err := conn.Query(ctx, `SELECT extname, extversion FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY extname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var es []Extensao
	for rows.Next() {
		var e Extensao
		if err := rows.Scan(&e.Nome, &e.Versao); err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, rows.Err()
}

// anteriores lista os __anterior do banco no destino, pelo prefixo exato (starts_with, e não LIKE,
// que trataria o _ como curinga).
func anteriores(ctx context.Context, conn *pgx.Conn, bancoDestino string, usados map[string]bool) ([]Anterior, error) {
	rows, err := conn.Query(ctx, `SELECT datname, pg_database_size(oid) FROM pg_database WHERE starts_with(datname, $1) ORDER BY datname DESC`,
		nomes.PrefixoAnteriores(bancoDestino))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var as []Anterior
	for rows.Next() {
		var a Anterior
		if err := rows.Scan(&a.Nome, &a.Tamanho); err != nil {
			return nil, err
		}
		var ok bool
		if a.Data, ok = nomes.DataDoAnterior(bancoDestino, a.Nome); ok && !usados[a.Nome] {
			as = append(as, a)
		}
	}
	return as, rows.Err()
}

func sessoes(ctx context.Context, conn *pgx.Conn, b string) ([]Sessao, error) {
	rows, err := conn.Query(ctx, `SELECT pid, coalesce(usename, ''), coalesce(application_name, ''),
		coalesce(host(client_addr), 'local'), coalesce(state, '') FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()
		ORDER BY pid`, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ss []Sessao
	for rows.Next() {
		var s Sessao
		if err := rows.Scan(&s.PID, &s.Usuario, &s.Aplicacao, &s.Cliente, &s.Estado); err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, rows.Err()
}

// DirDumps é o diretório dos dumps do perfil.
func DirDumps(d local.Dir, p cadastro.Perfil) string {
	if p.DirDumps != "" {
		return p.DirDumps
	}
	return d.Dumps() + "/" + NomeArquivo(p.Nome)
}

var reNaoSeguro = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// NomeArquivo deixa um nome seguro para caminho: sem barra, sem espaço, sem "..".
func NomeArquivo(s string) string {
	s = reNaoSeguro.ReplaceAllString(s, "_")
	s = strings.Trim(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

func espacoLivre(dir string) (int64, error) {
	for p := dir; p != "/" && p != "."; p = parentDir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize), nil
		} else if !os.IsNotExist(err) {
			return 0, err
		}
	}
	return 0, errors.New("sem diretório")
}

func parentDir(p string) string {
	i := strings.LastIndex(strings.TrimRight(p, "/"), "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// Tamanho escreve bytes como 1,2 GB.
func Tamanho(b int64) string {
	const k = 1024
	u := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(b)
	i := 0
	for f >= k && i < len(u)-1 {
		f /= k
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return strings.Replace(fmt.Sprintf("%.1f %s", f, u[i]), ".", ",", 1)
}

// lerBase lê a base do destino (tamanho e comentário), se existir.
func lerBase(ctx context.Context, p *Plano, adm *pgx.Conn, bancoDestino string) error {
	var tam int64
	var coment *string
	base := nomes.BancoBase(bancoDestino)
	err := adm.QueryRow(ctx, `SELECT pg_database_size(oid), shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, base).Scan(&tam, &coment)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	p.Base = &Anterior{Nome: base, Tamanho: tam}
	if coment != nil {
		p.BaseDescricao = *coment
	}
	return nil
}

// PlanejarReset planeja recriar o destino do perfil a partir da base, sem ir à origem nem ao
// Docker. As guardas do destino são as mesmas de uma cópia.
func PlanejarReset(ctx context.Context, d Deps, nomePerfil string) (Plano, error) {
	p := Plano{CriadoEm: time.Now(), Reset: true}
	perfil, err := d.Cadastro.Perfil(ctx, nomePerfil)
	if err != nil {
		return p, err
	}
	p.Perfil, p.Novo = perfil, nomes.Novo(perfil.DestinoBanco)
	cDest, err := d.Cadastro.Conexao(ctx, perfil.Destino)
	if err != nil {
		return p, err
	}
	if cDest.Tag == cadastro.TagProd {
		p.bloquear("a conexão de destino %s é prod: um banco prod nunca é destino", cDest.Nome)
		return p, nil
	}
	if perfil.DestinoBanco == cDest.BancoAdmin || perfil.DestinoBanco == "template0" || perfil.DestinoBanco == "template1" {
		p.bloquear("o banco de destino %s é o banco administrativo ou um template: escolha outro", perfil.DestinoBanco)
		return p, nil
	}
	destino, _, err := abrirLado(ctx, d, perfil.Destino)
	if err != nil {
		var pg *Pergunta
		if errors.As(err, &pg) {
			return p, pg
		}
		p.bloquear("destino inacessível: %v", err)
		return p, nil
	}
	defer destino.fechar()
	infoD := destino.c.Info
	p.Destino = Lado{Conexao: destino.c.Nome, Tag: destino.c.Tag, Onde: destino.c.Onde(), VersaoNum: infoD.VersaoNum, SystemID: infoD.SystemID,
		Recuperacao: infoD.Recuperacao, Superusuario: infoD.Superusuario, Usuario: destino.c.Usuario, Banco: perfil.DestinoBanco}
	if err := guardaProd(ctx, d, destino.c, infoD.SystemID); err != nil {
		p.bloquear("%v", err)
		return p, nil
	}
	if !d.TravaObtida && !trava.Livre(d.Dir.Travas(), trava.Destino(perfil.Destino, infoD.SystemID), perfil.DestinoBanco) {
		p.bloquear("há outra cópia em andamento para %s em %s", perfil.DestinoBanco, perfil.Destino)
		return p, nil
	}
	aprovados, err := d.Cadastro.DestinosAprovados(ctx)
	if err != nil {
		return p, err
	}
	if _, ok := aprovados[infoD.SystemID]; !ok || infoD.SystemID == "" {
		p.bloquear("o servidor de %s ainda não foi aprovado como destino de cópias: aprove-o uma vez na aba Conexões (tecla v)", destino.c.Nome)
	}
	if !infoD.Superusuario {
		p.bloquear("o usuário %s não é superusuário no destino", destino.c.Usuario)
	}
	if infoD.Recuperacao {
		p.bloquear("o destino %s é uma réplica (somente leitura)", destino.c.Nome)
	}
	if err := lerBase(ctx, &p, destino.admin, perfil.DestinoBanco); err != nil {
		return p, err
	}
	if p.Base == nil {
		p.bloquear("não há %s no destino: ligue \"guardar base\" no perfil e faça uma cópia antes", nomes.BancoBase(perfil.DestinoBanco))
	}
	if bd, ok := banco(infoD, perfil.DestinoBanco); ok {
		p.Destino.Existe, p.Destino.Info = true, bd
	} else {
		p.Destino.Info.Dono = destino.c.Usuario
	}
	if _, ok := banco(infoD, p.Novo); ok {
		p.NovoExiste = true
		p.bloquear("sobrou o banco %s de uma cópia anterior no destino: apague-o na aba Anteriores antes", p.Novo)
	}
	usados, err := bancosDePerfis(ctx, d, perfil.Destino)
	if err != nil {
		return p, err
	}
	if p.Anteriores, err = anteriores(ctx, destino.admin, perfil.DestinoBanco, usados); err != nil {
		return p, err
	}
	if p.Destino.Existe {
		if p.Sessoes, err = sessoes(ctx, destino.admin, perfil.DestinoBanco); err != nil {
			return p, err
		}
	}
	p.Notas = append(p.Notas, "o destino é recriado a partir da base, no próprio servidor (CREATE DATABASE … TEMPLATE): a origem e o Docker não são usados. No PostgreSQL 18, com file_copy_method = clone num sistema de arquivos com reflink (XFS, Btrfs, ZFS), leva milissegundos")
	return p, nil
}
