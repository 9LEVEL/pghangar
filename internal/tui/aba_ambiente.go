package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/imagens"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/tunel"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

// abaAmbiente: o Docker, as imagens, a chave SSH da ferramenta e o diretório.
type abaAmbiente struct {
	carregado  bool
	docker     string
	dockerErro string
	presentes  map[int]bool // a imagem travada ainda está no Docker local
	chavePub   string
	containers []imagens.Container
}

type msgAmbiente struct {
	docker     string
	dockerErro string
	presentes  map[int]bool
	chavePub   string
	containers []imagens.Container
}

func (a *abaAmbiente) carregar(m *Model) tea.Cmd {
	dk, is, chave := m.o.Docker, m.imagens, m.o.Dir.ChaveSSH()
	inst, _ := m.o.Cadastro.Instancia(context.Background())
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r := msgAmbiente{presentes: map[int]bool{}}
		if v, err := dk.Versao(ctx); err != nil {
			r.dockerErro = err.Error()
		} else {
			r.docker = v
			for v, i := range is {
				r.presentes[v] = dk.Existe(ctx, i.Digest)
			}
			r.containers, _ = dk.Rodando(ctx, inst)
		}
		if pub, err := tunel.ChavePublica(chave); err == nil {
			r.chavePub = pub
		}
		return r
	}
}

func (a *abaAmbiente) receber(m *Model, r msgAmbiente) {
	a.carregado = true
	a.docker, a.dockerErro, a.presentes, a.chavePub, a.containers = r.docker, r.dockerErro, r.presentes, r.chavePub, r.containers
}

// faltando são as versões configuradas sem imagem travada, ou com a imagem travada sumida.
func (a *abaAmbiente) faltando(m *Model) []int {
	var vs []int
	for _, v := range m.cfg.Versoes {
		i, ok := m.imagens[v]
		if !ok || (a.carregado && !a.presentes[v] && i.Digest != "") {
			vs = append(vs, v)
		}
	}
	return vs
}

func (a *abaAmbiente) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "r":
		return a.carregar(m)
	case "b":
		if a.dockerErro != "" {
			m.erro("Sem Docker", errors.New(a.dockerErro))
			return nil
		}
		vs := a.faltando(m)
		if len(vs) == 0 {
			m.status = "as imagens de todas as versões configuradas já estão baixadas e travadas"
			return nil
		}
		var refs []string
		for _, v := range vs {
			refs = append(refs, imagens.Referencia(m.cfg.Repositorio, v))
		}
		m.conf.perguntar("Baixar as imagens", "Vão ser baixadas e travadas pelo digest:\n\n  "+strings.Join(refs, "\n  ")+
			"\n\nCada uma tem uns 150 MB. Travada, a imagem só muda quando você mandar.", func() tea.Cmd {
			return a.baixar(m, vs, 0)
		})
	case "u":
		if a.dockerErro != "" {
			m.erro("Sem Docker", errors.New(a.dockerErro))
			return nil
		}
		return a.atualizar(m)
	case "g":
		if arquivoExiste(m.o.Dir.ChaveSSH()) {
			m.status = "a chave já existe: " + m.o.Dir.ChaveSSH() + " (a ferramenta nunca sobrescreve)"
			return nil
		}
		m.conf.perguntar("Gerar a chave SSH", "Uma chave ed25519, sem passphrase, em "+m.o.Dir.ChaveSSH()+" (600).\n\n"+
			"Ela só abre o túnel até o banco: no servidor da produção, ela entra no authorized_keys com restrict, port-forwarding e permitopen (tecla l). Quem a roubar ainda precisa da senha do banco.", func() tea.Cmd {
			if err := m.o.GerarChave(); err != nil {
				m.erro("Não foi possível gerar a chave", err)
				return nil
			}
			m.status = "chave gerada: tecle l para ver a linha do authorized_keys"
			return a.carregar(m)
		})
	case "l":
		m.informar("Linhas do authorized_keys", a.linhas(m))
	case "w":
		if m.cfg.Webhook == "" {
			m.status = "sem webhook: configure com a tecla e"
			return nil
		}
		url := m.cfg.Webhook
		return m.executar(&tarefa{
			rotulo: "mandando um aviso de teste ao webhook",
			rodar: func(ctx context.Context, _ conexao.Segredos) (any, error) {
				maq, _ := os.Hostname()
				e := cadastro.Execucao{Perfil: "teste", Estado: cadastro.EstadoOK, Mensagem: "aviso de teste do pghangar", Inicio: time.Now(), Fim: time.Now()}
				return nil, motor.Avisar(ctx, url, motor.NovoAviso(e, maq))
			},
			pronto: func(m *Model, _ any, err error) tea.Cmd {
				if err != nil {
					m.erro("O webhook falhou", err)
					return nil
				}
				m.status = "aviso de teste enviado: confira no canal"
				return nil
			},
		})
	case "e":
		return a.configurar(m)
	case "p":
		var orfaos []imagens.Container
		for _, c := range a.containers {
			if !execucaoRodando(m, c.Execucao) {
				orfaos = append(orfaos, c)
			}
		}
		if len(orfaos) == 0 {
			m.status = "nenhum container órfão"
			return nil
		}
		var ns []string
		for _, c := range orfaos {
			ns = append(ns, c.Nome)
		}
		m.conf.perguntar("Parar os containers órfãos", "Containers da ferramenta sem execução em andamento:\n\n  "+strings.Join(ns, "\n  "), func() tea.Cmd {
			for _, c := range orfaos {
				if err := m.o.Docker.Parar(context.Background(), c.Nome); err != nil {
					m.erro("Não foi possível parar "+c.Nome, err)
					return nil
				}
			}
			return a.carregar(m)
		})
	}
	return nil
}

func execucaoRodando(m *Model, id string) bool {
	for _, e := range m.execucoes {
		if strconv.FormatInt(e.ID, 10) == id && !e.Terminou() {
			return true
		}
	}
	return false
}

type novidade struct {
	antes, depois cadastro.Imagem
}

// atualizar baixa a tag de cada versão de novo e mostra o que mudou. Só trava a nova com a
// confirmação; a imagem antiga fica no Docker (nada é apagado sozinho).
func (a *abaAmbiente) atualizar(m *Model) tea.Cmd {
	dk, cfg, atuais := m.o.Docker, m.cfg, m.imagens
	return m.executar(&tarefa{
		rotulo: "procurando imagens novas (docker pull)",
		rodar: func(_ context.Context, _ conexao.Segredos) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			var ns []novidade
			for _, v := range cfg.Versoes {
				ref := imagens.Referencia(cfg.Repositorio, v)
				if err := dk.Baixar(ctx, ref, io.Discard); err != nil {
					return nil, err
				}
				dg, err := dk.Digest(ctx, ref)
				if err != nil {
					return nil, err
				}
				if atual, ok := atuais[v]; ok && atual.Digest == dg {
					continue
				}
				cl, err := dk.VersaoCliente(ctx, dg)
				if err != nil {
					return nil, err
				}
				if imagens.MajorDoCliente(cl) != v {
					return nil, fmt.Errorf("%s tem o pg_dump %s, e não o %d", ref, cl, v)
				}
				ns = append(ns, novidade{antes: atuais[v], depois: cadastro.Imagem{Versao: v, Referencia: ref, Digest: dg, Cliente: cl, BaixadaEm: time.Now()}})
			}
			return ns, nil
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("Não foi possível atualizar", err)
				return nil
			}
			ns := v.([]novidade)
			if len(ns) == 0 {
				m.status = "as imagens travadas já são as mais novas das tags"
				return a.carregar(m)
			}
			var linhas []string
			for _, n := range ns {
				de := "não baixada"
				if n.antes.Digest != "" {
					de = "pg_dump " + n.antes.Cliente
				}
				linhas = append(linhas, fmt.Sprintf("  postgres:%d   %s  →  pg_dump %s", n.depois.Versao, de, n.depois.Cliente))
			}
			m.conf.perguntar("Travar as imagens novas", "Mudaram:\n\n"+strings.Join(linhas, "\n")+
				"\n\nConfirmando, as próximas cópias usam as novas. As antigas ficam no Docker (nada é apagado sozinho): para liberar espaço, `docker image prune`.", func() tea.Cmd {
				for _, n := range ns {
					if err := m.o.Cadastro.SalvarImagem(context.Background(), n.depois); err != nil {
						m.erro("Erro gravando a imagem", err)
						return nil
					}
				}
				m.recarregar()
				m.status = "imagens novas travadas"
				return a.carregar(m)
			})
			return nil
		},
	})
}

type imagemBaixada struct {
	versao int
	img    cadastro.Imagem
}

// baixar baixa uma versão por vez, para o rodapé dizer qual está baixando.
func (a *abaAmbiente) baixar(m *Model, vs []int, i int) tea.Cmd {
	if i >= len(vs) {
		m.status = "imagens baixadas e travadas"
		return a.carregar(m)
	}
	v := vs[i]
	ref := imagens.Referencia(m.cfg.Repositorio, v)
	dk := m.o.Docker
	return m.executar(&tarefa{
		rotulo: fmt.Sprintf("baixando %s (%d de %d)", ref, i+1, len(vs)),
		rodar: func(ctx context.Context, _ conexao.Segredos) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := dk.Baixar(ctx, ref, io.Discard); err != nil {
				return nil, err
			}
			dg, err := dk.Digest(ctx, ref)
			if err != nil {
				return nil, err
			}
			cl, err := dk.VersaoCliente(ctx, dg)
			if err != nil {
				return nil, err
			}
			if imagens.MajorDoCliente(cl) != v {
				return nil, fmt.Errorf("%s tem o pg_dump %s, e não o %d: confira o repositório configurado", ref, cl, v)
			}
			return imagemBaixada{versao: v, img: cadastro.Imagem{Versao: v, Referencia: ref, Digest: dg, Cliente: cl, BaixadaEm: time.Now()}}, nil
		},
		pronto: func(m *Model, r any, err error) tea.Cmd {
			if err != nil {
				m.erro("Não foi possível baixar "+ref, err)
				return a.carregar(m)
			}
			if err := m.o.Cadastro.SalvarImagem(context.Background(), r.(imagemBaixada).img); err != nil {
				m.erro("Erro gravando a imagem", err)
				return nil
			}
			m.recarregar()
			return a.baixar(m, vs, i+1)
		},
	})
}

func (a *abaAmbiente) configurar(m *Model) tea.Cmd {
	return m.form.abrir("Configuração", "", []campo{
		novoCampo("repo", "Repositório", m.cfg.Repositorio, "A imagem sem a tag: postgres (Docker Hub), ou um espelho interno (registry.interna/postgres). A tag é a versão.", cadastro.ValidarRepositorio),
		novoCampo("versoes", "Versões", versoes.Lista(m.cfg.Versoes), "As versões com imagem, separadas por vírgula. Uma versão nova (19) entra aqui.", func(s string) error {
			_, err := versoes.LerLista(s)
			return err
		}),
		novoCampo("webhook", "Webhook", m.cfg.Webhook, "Aviso ao terminar cada execução: um POST em JSON, com o campo text (Slack, Mattermost e Teams mostram direto). Vazio desliga. A URL fica no cadastro e nunca vai para o log.", nil),
	}, func(f *formulario) (tea.Cmd, error) {
		vs, _ := versoes.LerLista(f.valor("versoes"))
		if err := m.o.Cadastro.SalvarConfig(context.Background(), cadastro.Config{Repositorio: f.valor("repo"), Versoes: vs, Webhook: f.valor("webhook")}); err != nil {
			return nil, err
		}
		m.recarregar()
		m.status = "configuração salva: tecle b para baixar o que falta"
		return a.carregar(m), nil
	})
}

// linhas é o texto para o sysadmin pôr nos servidores da produção.
func (a *abaAmbiente) linhas(m *Model) string {
	if a.chavePub == "" {
		return "A ferramenta ainda não tem chave SSH. Tecle g para gerar."
	}
	var b strings.Builder
	b.WriteString(stTexto.Render("A chave pública da ferramenta:") + "\n" + stValor.Render(a.chavePub) + "\n")
	ssh := 0
	for _, c := range m.conexoes {
		if c.Acesso != cadastro.AcessoSSH {
			continue
		}
		ssh++
		destino := fmt.Sprintf("%s:%d", c.Host, c.Porta)
		pub := a.chavePub
		if c.SSHChave != "" {
			if p, err := tunel.ChavePublica(c.SSHChave); err == nil {
				pub = p
			} else {
				b.WriteString("\n" + stAvisoV.Render(c.Nome+": a chave "+c.SSHChave+" é da conexão, e não foi possível ler a parte pública dela.") + "\n")
				continue
			}
		}
		u := c.SSHUsuario
		if c.SaltoHost != "" {
			su := c.SaltoUsuario
			b.WriteString(fmt.Sprintf("\n%s\n", stMarca.Render(fmt.Sprintf("# %s: no bastion %s, como root (só deixa passar até o SSH %s:%d)", c.Nome, c.SaltoHost, c.SSHHost, c.SSHPorta))))
			b.WriteString(stTexto.Render(fmt.Sprintf(`useradd --system --create-home --shell /usr/sbin/nologin %[1]s
install -d -m 700 -o %[1]s -g %[1]s ~%[1]s/.ssh
cat >> ~%[1]s/.ssh/authorized_keys <<'FIM'
%[2]s
FIM
chown %[1]s: ~%[1]s/.ssh/authorized_keys && chmod 600 ~%[1]s/.ssh/authorized_keys`, su, tunel.LinhaAuthorizedKeys(pub, fmt.Sprintf("%s:%d", c.SSHHost, c.SSHPorta)))) + "\n")
		}
		b.WriteString(fmt.Sprintf("\n%s\n", stMarca.Render(fmt.Sprintf("# %s: no servidor %s, como root", c.Nome, c.SSHHost))))
		b.WriteString(stTexto.Render(fmt.Sprintf(`useradd --system --create-home --shell /usr/sbin/nologin %[1]s
install -d -m 700 -o %[1]s -g %[1]s ~%[1]s/.ssh
cat >> ~%[1]s/.ssh/authorized_keys <<'FIM'
%[2]s
FIM
chown %[1]s: ~%[1]s/.ssh/authorized_keys && chmod 600 ~%[1]s/.ssh/authorized_keys`, u, tunel.LinhaAuthorizedKeys(pub, destino))) + "\n")
	}
	if ssh == 0 {
		b.WriteString("\n" + stDica.Render("Nenhuma conexão por SSH ainda. Cadastrada, a linha dela aparece aqui."))
	}
	b.WriteString("\n" + stDica.Render("A linha só permite o túnel até o banco (permitopen): nada de shell, comando ou outra porta. O sshd precisa de AllowTcpForwarding (o padrão é sim)."))
	return b.String()
}

func (a *abaAmbiente) view(m *Model) string {
	larg := limitar(m.largura-4, 40, 140)
	var b strings.Builder
	sec := func(t string) { b.WriteString("\n" + stCabecalho.Render(" "+t) + "\n") }

	sec("DOCKER")
	switch {
	case !a.carregado:
		b.WriteString("   " + stDica.Render("verificando…") + "\n")
	case a.dockerErro != "":
		b.WriteString("   " + stPerigoV.Render("✖ "+a.dockerErro) + "\n   " + stDica.Render("O Docker é obrigatório: sem ele, a ferramenta não copia.") + "\n")
	default:
		b.WriteString("   " + stOk.Render("✔ ") + stTexto.Render("Docker "+a.docker) + "\n")
	}

	sec("IMAGENS · repositório " + m.cfg.Repositorio)
	for _, v := range m.cfg.Versoes {
		i, ok := m.imagens[v]
		ref := preencher(imagens.Referencia(m.cfg.Repositorio, v), 22)
		switch {
		case !ok:
			b.WriteString("   " + stAvisoV.Render("○ ") + stTexto.Render(ref) + stAvisoV.Render("não baixada (tecla b)") + "\n")
		case a.carregado && a.dockerErro == "" && !a.presentes[v]:
			b.WriteString("   " + stPerigoV.Render("✖ ") + stTexto.Render(ref) + stPerigoV.Render("a imagem travada sumiu do Docker (tecla b)") + "\n")
		default:
			dg := i.Digest
			if j := strings.Index(dg, "sha256:"); j >= 0 && len(dg) > j+19 {
				dg = dg[j : j+19]
			}
			b.WriteString("   " + stOk.Render("✔ ") + stTexto.Render(ref) + stTexto.Render(preencher("pg_dump "+i.Cliente, 14)) + stDica.Render(dg+" · baixada "+idade(i.BaixadaEm)) + "\n")
		}
	}

	sec("CHAVE SSH DA FERRAMENTA")
	if a.chavePub == "" {
		b.WriteString("   " + stAvisoV.Render("○ ") + stTexto.Render("ainda não gerada (tecla g). É ela que abre o túnel até a produção.") + "\n")
	} else {
		b.WriteString("   " + stOk.Render("✔ ") + stTexto.Render(m.o.Dir.ChaveSSH()) + "\n   " + stDica.Render(truncar(a.chavePub, larg-4)) + "\n")
		b.WriteString("   " + stDica.Render("tecla l: as linhas prontas para o authorized_keys de cada conexão SSH") + "\n")
	}
	kh := m.o.Dir.KnownHosts()
	n := 0
	if s, err := os.ReadFile(kh); err == nil {
		n = strings.Count(string(s), "\n")
	}
	b.WriteString("   " + stDica.Render(fmt.Sprintf("known_hosts da ferramenta: %s (%d servidor(es) aceito(s))", kh, n)) + "\n")

	sec("AVISO AO TERMINAR")
	if m.cfg.Webhook == "" {
		b.WriteString("   " + stDica.Render("sem webhook (tecla e para configurar)") + "\n")
	} else {
		u := m.cfg.Webhook
		if i := strings.Index(u, "://"); i > 0 {
			if j := strings.Index(u[i+3:], "/"); j > 0 {
				u = u[:i+3+j] + "/…" // o caminho costuma ser o segredo
			}
		}
		b.WriteString("   " + stOk.Render("✔ ") + stTexto.Render(u) + stDica.Render("  · tecla w: mandar um aviso de teste") + "\n")
	}

	sec("DIRETÓRIO")
	b.WriteString("   " + stTexto.Render(m.o.Dir.Raiz) + stDica.Render("  · estado.db, dumps/, logs/, chaves/ · tudo 700/600, só root") + "\n")

	if len(a.containers) > 0 {
		sec("CONTAINERS DA FERRAMENTA")
		for _, c := range a.containers {
			orf := ""
			if !execucaoRodando(m, c.Execucao) {
				orf = stAvisoV.Render("  órfão (tecla p para parar)")
			}
			b.WriteString("   " + stTexto.Render(c.Nome) + stDica.Render("  "+c.Estado) + orf + "\n")
		}
	}
	return b.String()
}
