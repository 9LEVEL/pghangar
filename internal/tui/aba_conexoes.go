package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/9LEVEL/pghangar/internal/cadastro"
	"github.com/9LEVEL/pghangar/internal/conexao"
	"github.com/9LEVEL/pghangar/internal/execucao"
	"github.com/9LEVEL/pghangar/internal/motor"
	"github.com/9LEVEL/pghangar/internal/versoes"
)

type abaConexoes struct {
	cursor      int
	diag        map[string]conexao.Diagnostico // o último diagnóstico desta sessão
	verificando map[string]bool
}

func (a *abaConexoes) atual(m *Model) (cadastro.Conexao, bool) {
	if a.cursor < 0 || a.cursor >= len(m.conexoes) {
		return cadastro.Conexao{}, false
	}
	return m.conexoes[a.cursor], true
}

func (a *abaConexoes) tecla(m *Model, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(m.conexoes)-1, 0))
	case "a":
		return a.formulario(m, cadastro.Conexao{Tag: semTag, Acesso: cadastro.AcessoDireto, SSHPorta: 22, SSHUsuario: "pghangar",
			Host: "127.0.0.1", Porta: 5432, Usuario: "postgres", ModoSenha: cadastro.SenhaGuardar, SSLMode: "prefer", BancoAdmin: "postgres"}, "")
	case "e", "enter":
		if c, ok := a.atual(m); ok {
			return a.formulario(m, c, c.Nome)
		}
	case "t":
		if c, ok := a.atual(m); ok {
			return a.testar(m, c.Nome, true)
		}
	case "b":
		if c, ok := a.atual(m); ok {
			a.mostrarBancos(m, c)
		}
	case "v":
		if c, ok := a.atual(m); ok {
			return a.aprovar(m, c)
		}
	case "d":
		if c, ok := a.atual(m); ok {
			m.conf.perguntar("Remover a conexão "+c.Nome, "A conexão sai do cadastro, com a senha guardada. Nada muda no servidor.", func() tea.Cmd {
				if err := m.o.Cadastro.RemoverConexao(context.Background(), c.Nome); err != nil {
					m.erro("Não foi possível remover", err)
					return nil
				}
				delete(a.diag, c.Nome)
				m.recarregar()
				m.status = "conexão " + c.Nome + " removida"
				return nil
			})
		}
	}
	return nil
}

// aprovar liga ou desliga o servidor da conexão como destino de cópias. Aprovar é de propósito:
// pede o nome da conexão digitado.
func (a *abaConexoes) aprovar(m *Model, c cadastro.Conexao) tea.Cmd {
	sid := c.Info.SystemID
	switch {
	case c.Tag == cadastro.TagProd:
		m.informar("prod nunca é destino", "A conexão "+c.Nome+" é prod: um banco prod nunca é destino, e não há como aprová-la.")
		return nil
	case sid == "":
		m.informar("Teste antes", "A aprovação é pelo servidor (system_identifier), e ele ainda não foi lido: tecle t para testar a conexão.")
		return nil
	}
	if d, ok := m.aprovados[sid]; ok {
		m.conf.perguntar("Revogar o destino", fmt.Sprintf("O servidor de %s deixa de ser destino aprovado (aprovado %s por %s). As cópias para ele passam a ser bloqueadas.", c.Nome, idade(d.AprovadoEm), d.AprovadoPor), func() tea.Cmd {
			if err := m.o.Cadastro.RevogarDestino(context.Background(), sid); err != nil {
				m.erro("Erro", err)
				return nil
			}
			m.recarregar()
			m.status = "destino revogado: " + c.Nome
			return nil
		})
		return nil
	}
	for _, x := range m.conexoes {
		if x.Tag == cadastro.TagProd && x.Info.SystemID == sid {
			m.informar("É a produção", fmt.Sprintf("O servidor de %s é o mesmo da conexão prod %s: ele nunca será destino.", c.Nome, x.Nome))
			return nil
		}
	}
	corpo := fmt.Sprintf("O servidor de %s (%s, system_identifier %s) passa a aceitar cópias: os bancos dos perfis que apontam para ele podem ser substituídos.\n\nA aprovação é do servidor: vale para qualquer conexão que chegue nele.", c.Nome, c.Onde(), sid)
	return m.conf.perguntarCritico("Aprovar como destino", corpo, c.Nome, func() tea.Cmd {
		if err := m.o.Cadastro.AprovarDestino(context.Background(), sid, c.Nome, execucao.Operador()); err != nil {
			m.erro("Erro", err)
			return nil
		}
		m.recarregar()
		m.status = "destino aprovado: " + c.Nome
		return nil
	})
}

// --- teste --------------------------------------------------------------------------------------

type resultadoDiag struct {
	nome string
	d    conexao.Diagnostico
}

// testar roda o diagnóstico. Na primeira vez (e sempre que chamado pela tecla t), mostra as camadas;
// a versão e o estado lidos vão para o cadastro.
func (a *abaConexoes) testar(m *Model, nome string, mostrar bool) tea.Cmd {
	return m.executar(&tarefa{
		rotulo: "testando " + nome,
		rodar: func(ctx context.Context, seg conexao.Segredos) (any, error) {
			c, err := m.o.Cadastro.Conexao(ctx, nome)
			if err != nil {
				return nil, err
			}
			d := m.o.Diagnosticar(ctx, c, seg)
			// O que falta perguntar volta como pergunta: a tela pergunta e testa de novo.
			switch {
			case d.HostDesconhecido != nil:
				return nil, &motor.Pergunta{Conexao: nome, HostDesconhecido: d.HostDesconhecido}
			case d.PrecisaFrase != "":
				return nil, &motor.Pergunta{Conexao: nome, Frase: d.PrecisaFrase}
			case d.PrecisaSenha:
				return nil, &motor.Pergunta{Conexao: nome, Senha: true}
			case d.SenhaErrada && c.ModoSenha == cadastro.SenhaPerguntar:
				// A senha desta sessão está errada: pergunta de novo, em vez de repeti-la.
				return nil, &motor.Pergunta{Conexao: nome, Senha: true, Motivo: "a senha informada está errada"}
			}
			return resultadoDiag{nome: nome, d: d}, nil
		},
		pronto: func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				m.erro("Não foi possível testar "+nome, err)
				return nil
			}
			r := v.(resultadoDiag)
			a.guardar(m, r.nome, r.d)
			if mostrar {
				titulo := nome + ": pronta"
				if r.d.Parou() != "" {
					titulo = nome + ": parou em " + r.d.Parou()
				}
				m.jan.mostrar(m.largura, m.altura, titulo, a.textoDiag(m, r.nome, r.d), r.d.Parou() != "")
			}
			return nil
		},
	})
}

// guardar grava o que o diagnóstico viu. Uma falha não apaga a versão e os bancos já conhecidos.
func (a *abaConexoes) guardar(m *Model, nome string, d conexao.Diagnostico) {
	a.diag[nome] = d
	if d.PrecisaSenha || d.PrecisaFrase != "" || d.HostDesconhecido != nil {
		return // falta algo desta sessão: não é um problema do servidor, e não vai para o cadastro
	}
	info := d.Info
	if d.Parou() != "" {
		if c, ok := m.conexao(nome); ok {
			velho := c.Info
			velho.VerificadaEm, velho.Camada, velho.Erro = d.Info.VerificadaEm, d.Info.Camada, d.Info.Erro
			info = velho
		}
	}
	_ = m.o.Cadastro.GravarInfo(context.Background(), nome, info)
	for i := range m.conexoes {
		if m.conexoes[i].Nome == nome {
			m.conexoes[i].Info = info
		}
	}
}

func (a *abaConexoes) textoDiag(m *Model, nome string, d conexao.Diagnostico) string {
	c, _ := m.conexao(nome)
	var b strings.Builder
	b.WriteString(seloTag(c.Tag) + " " + stValor.Render(nome) + stDica.Render(" · "+c.Onde()) + "\n\n")
	for _, x := range d.Camadas {
		s := stOk.Render("✔ ")
		if !x.OK {
			s = stPerigoV.Render("✖ ")
		}
		b.WriteString(quebrar(s+stValor.Render(preencher(x.Nome, 14)), stTexto.Render(x.Detalhe), 90) + "\n")
	}
	if d.Parou() == "" {
		i := d.Info
		b.WriteString("\n" + stRotulo.Render("system_identifier ") + stTexto.Render(orDefault(i.SystemID, "(não lido)")))
		if c.Tag != cadastro.TagProd && i.SystemID != "" {
			for _, x := range m.conexoes {
				if x.Tag == cadastro.TagProd && x.Info.SystemID == i.SystemID {
					b.WriteString("\n\n" + stPerigoV.Render(fmt.Sprintf("Este é o mesmo servidor da conexão prod %s. Esta conexão nunca será aceita como destino: se ela é a produção, marque a tag prod.", x.Nome)))
				}
			}
		}
		if !i.Superusuario {
			b.WriteString("\n\n" + stAvisoV.Render("O usuário não é superusuário: como destino, a cópia é bloqueada; como origem, tabelas com RLS falham."))
		}
		if i.Recuperacao {
			b.WriteString("\n\n" + stAvisoV.Render("É uma réplica: serve de origem (tira a carga do primário), nunca de destino."))
		}
	} else {
		b.WriteString("\n" + stDica.Render(explicarCamada(d.Parou())))
	}
	return b.String()
}

func latencia(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

func explicarCamada(c string) string {
	switch c {
	case "DNS":
		return "O nome não resolve neste servidor. Confira o nome, ou use o IP."
	case "TCP":
		return "\"Tempo esgotado\" não é \"fora do ar\": pode ser firewall ou rota. \"Recusada\" quer dizer que o host respondeu, mas nada escuta na porta."
	case "SSH":
		return "Confira o usuário SSH, a chave (aba 6, tecla l, mostra a linha do authorized_keys) e o known_hosts da ferramenta."
	case "Bastion":
		return "O bastion entrou, mas não abriu o caminho até o servidor SSH: confira o permitopen do authorized_keys no bastion (aba 6, tecla l)."
	case "Túnel":
		return "O SSH entrou, mas não abriu o caminho até o banco: confira o permitopen do authorized_keys e o endereço do banco visto do servidor SSH (normalmente 127.0.0.1)."
	case "Autenticação":
		return "Confira o usuário e a senha do banco, e o pg_hba.conf do servidor."
	case "Banco":
		return "O banco administrativo não existe: confira o campo \"banco administrativo\" (normalmente postgres)."
	}
	return ""
}

func (a *abaConexoes) mostrarBancos(m *Model, c cadastro.Conexao) {
	if len(c.Info.Bancos) == 0 {
		m.informar("Bancos de "+c.Nome, "A conexão ainda não foi verificada. Tecle t para testar.")
		return
	}
	var b strings.Builder
	b.WriteString(stDica.Render("Visto "+idade(c.Info.VerificadaEm)) + "\n\n")
	for _, x := range c.Info.Bancos {
		loc := x.Collate
		switch x.Provedor {
		case "i":
			loc = "ICU " + x.Locale
		case "b":
			loc = "builtin " + x.Locale
		}
		fechado := ""
		if !x.Conexoes {
			fechado = stAvisoV.Render(" fechado para conexões")
		}
		b.WriteString(fmt.Sprintf("%s  %s  %s%s\n", stValor.Render(preencher(truncar(x.Nome, 40), 40)), stTexto.Render(preencher(motor.Tamanho(x.Tamanho), 10)),
			stDica.Render(x.Dono+" · "+x.Codificacao+" · "+loc), fechado))
	}
	m.informar("Bancos de "+c.Nome, strings.TrimRight(b.String(), "\n"))
}

// --- verificação em segundo plano ---------------------------------------------------------------

type msgVerificarTodas struct{}

type msgDiagFundo struct {
	nome string
	d    conexao.Diagnostico
}

// verificarTodas testa as conexões em segundo plano, sem perguntar nada: uma senha que falta ou um
// servidor SSH desconhecido aparecem no estado, e o teste pela tecla t pergunta.
func (m *Model) verificarTodas() tea.Cmd {
	var cmds []tea.Cmd
	seg := copiar(m.seg)
	for _, c := range m.conexoes {
		if m.conexoesA.verificando[c.Nome] {
			continue
		}
		m.conexoesA.verificando[c.Nome] = true
		c := c
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			return msgDiagFundo{nome: c.Nome, d: m.o.Diagnosticar(ctx, c, seg)}
		})
	}
	cmds = append(cmds, tea.Tick(m.o.IntervaloVerificacao, func(time.Time) tea.Msg { return msgVerificarTodas{} }))
	return tea.Batch(cmds...)
}

func (a *abaConexoes) receberFundo(m *Model, msg msgDiagFundo) {
	delete(a.verificando, msg.nome)
	if _, ok := m.conexao(msg.nome); !ok {
		return
	}
	a.guardar(m, msg.nome, msg.d)
}

// estadoConexao é o selo de estado da conexão: o que a última verificação viu.
func estadoConexao(m *Model, c cadastro.Conexao) string {
	d, temDiag := m.conexoesA.diag[c.Nome]
	i := c.Info
	switch {
	case temDiag && d.PrecisaSenha:
		return stDica.Render("● senha não informada nesta sessão")
	case temDiag && d.HostDesconhecido != nil:
		return stAvisoV.Render("● servidor SSH desconhecido: tecle t")
	case temDiag && d.PrecisaFrase != "":
		return stDica.Render("● passphrase não informada nesta sessão")
	case i.VerificadaEm.IsZero():
		return stDica.Render("● não verificada")
	case i.Erro != "":
		return stPerigoV.Render("● parou em "+i.Camada) + stDica.Render(" · "+idade(i.VerificadaEm))
	case !i.Superusuario:
		return stAvisoV.Render("● sem superusuário") + stDica.Render(" · "+idade(i.VerificadaEm))
	case i.Recuperacao:
		return stOk.Render("● réplica") + stDica.Render(" · "+idade(i.VerificadaEm))
	}
	return stOk.Render("● no ar") + stDica.Render(" · "+idade(i.VerificadaEm))
}

// --- formulário ---------------------------------------------------------------------------------

func (a *abaConexoes) formulario(m *Model, c cadastro.Conexao, antigo string) tea.Cmd {
	obrig := func(s string) error {
		if s == "" {
			return errors.New("obrigatório")
		}
		return nil
	}
	porta := func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 65535 {
			return errors.New("porta inválida")
		}
		return nil
	}
	ssh := func(f *formulario) bool { return f.valor("acesso") == cadastro.AcessoSSH }
	guardar := func(f *formulario) bool { return f.valor("modo_senha") == cadastro.SenhaGuardar }
	ajudaSenha := "Guardada no cadastro (600). Vazio mantém a gravada."
	if antigo == "" {
		ajudaSenha = "Guardada no cadastro (600), nunca mostrada."
	}
	campos := []campo{
		novoCampo("nome", "Nome", c.Nome, "Como a conexão aparece nos perfis: prod-loja, homolog.", obrig),
		campoDeOpcao("tag", "Tag", "Escolha com ←→. prod aparece em vermelho e NUNCA é destino (nem outra conexão para o mesmo servidor). homolog pede o nome do banco digitado para confirmar; dev, só y.", opcoesTag(c.Tag), c.Tag),
		campoDeOpcao("acesso", "Acesso", "direto: este servidor alcança o banco pela rede. ssh: pelo túnel, com a chave da ferramenta (aba 6).", cadastro.Acessos, c.Acesso),
		novoCampo("ssh_host", "Servidor SSH", c.SSHHost, "O host em que o SSH entra (normalmente o próprio servidor do banco).", obrig),
		novoCampo("ssh_porta", "Porta SSH", strconv.Itoa(max(c.SSHPorta, 22)), "", porta),
		novoCampo("ssh_usuario", "Usuário SSH", c.SSHUsuario, "O usuário dedicado, com a chave restrita ao túnel (aba 6, tecla l).", obrig),
		novoCampo("ssh_chave", "Chave SSH", c.SSHChave, "Vazio usa a chave da ferramenta ("+m.o.Dir.ChaveSSH()+"). Com passphrase, ela é pedida a cada sessão.", nil),
		novoCampo("salto_host", "Bastion", c.SaltoHost, "Opcional: o host de salto por onde o SSH passa até o servidor. A mesma chave; a chave dele também é conferida no known_hosts.", nil),
		novoCampo("salto_porta", "Porta do bastion", strconv.Itoa(max(c.SaltoPorta, 22)), "", porta),
		novoCampo("salto_usuario", "Usuário no bastion", orDefault(c.SaltoUsuario, "pghangar"), "", nil),
		novoCampo("host", "Host do banco", c.Host, "Direto: o endereço do banco (ou o diretório do socket, como /var/run/postgresql). Pelo túnel: o banco visto do servidor SSH, normalmente 127.0.0.1.", obrig),
		novoCampo("porta", "Porta do banco", strconv.Itoa(c.Porta), "", porta),
		novoCampo("usuario", "Usuário do banco", c.Usuario, "Um superusuário: a ferramenta é de sysadmin.", obrig),
		campoDeOpcao("modo_senha", "Senha", "guardar: no cadastro. perguntar: a cada sessão, só na memória. pgpass: do ~/.pgpass do root.", cadastro.ModosSenha, c.ModoSenha),
		campoDeSegredo("senha", "Senha do banco", ajudaSenha, nil),
		campoDeOpcao("sslmode", "sslmode", "Direto: require (ou verify-full, com certificado) quando o servidor tem TLS. Pelo túnel, o SSH já cifra o caminho, e verify-full não funciona por ele.", cadastro.SSLModes, c.SSLMode),
		novoCampo("banco_admin", "Banco administrativo", c.BancoAdmin, "O banco usado para ler o servidor e criar/renomear bancos (normalmente postgres).", obrig),
	}
	campos[1].validar = func(s string) error {
		if s == semTag {
			return errors.New("escolha a tag: prod, homolog ou dev")
		}
		return nil
	}
	for i := 3; i <= 6; i++ {
		campos[i].visivel = ssh
	}
	campos[7].visivel = ssh
	comSalto := func(f *formulario) bool { return ssh(f) && f.valor("salto_host") != "" }
	campos[8].visivel, campos[9].visivel = comSalto, comSalto
	campos[14].visivel = guardar
	titulo := "Nova conexão"
	if antigo != "" {
		titulo = "Editar a conexão " + antigo
	}
	return m.form.abrir(titulo, "", campos, func(f *formulario) (tea.Cmd, error) {
		sp, _ := strconv.Atoi(f.valor("ssh_porta"))
		pp, _ := strconv.Atoi(f.valor("porta"))
		n := cadastro.Conexao{
			Nome: f.valor("nome"), Tag: f.valor("tag"), Acesso: f.valor("acesso"),
			Host: f.valor("host"), Porta: pp, Usuario: f.valor("usuario"), ModoSenha: f.valor("modo_senha"),
			Senha: f.valor("senha"), SSLMode: f.valor("sslmode"), BancoAdmin: f.valor("banco_admin"), Info: c.Info,
		}
		if n.Acesso == cadastro.AcessoSSH {
			n.SSHHost, n.SSHPorta, n.SSHUsuario, n.SSHChave = f.valor("ssh_host"), sp, f.valor("ssh_usuario"), f.valor("ssh_chave")
			if h := f.valor("salto_host"); h != "" {
				bp, _ := strconv.Atoi(f.valor("salto_porta"))
				n.SaltoHost, n.SaltoPorta, n.SaltoUsuario = h, bp, f.valor("salto_usuario")
			}
			if n.SSHChave == "" && !arquivoExiste(m.o.Dir.ChaveSSH()) {
				return nil, errors.New("a ferramenta ainda não tem chave SSH: gere-a na aba 6 (tecla g), ou informe o caminho de uma")
			}
		}
		if n.ModoSenha == cadastro.SenhaGuardar && n.Senha == "" && antigo == "" {
			return nil, errors.New("informe a senha (ou escolha perguntar ou pgpass)")
		}
		// Mudou o endereço: o que foi visto antes não vale mais.
		if n.Host != c.Host || n.Porta != c.Porta || n.SSHHost != c.SSHHost || n.SaltoHost != c.SaltoHost {
			n.Info = cadastro.Info{}
		}
		if err := m.o.Cadastro.SalvarConexao(context.Background(), antigo, n); err != nil {
			return nil, err
		}
		if antigo != "" && antigo != n.Nome {
			delete(a.diag, antigo)
			if s, ok := m.seg.Senhas[antigo]; ok {
				m.seg.GuardarSenha(n.Nome, s)
			}
		}
		m.recarregar()
		for i, x := range m.conexoes {
			if x.Nome == n.Nome {
				a.cursor = i
			}
		}
		m.status = "conexão " + n.Nome + " salva"
		// Na primeira conexão (e a cada edição), já lê a versão e o estado.
		return a.testar(m, n.Nome, true), nil
	})
}

// semTag é a opção inicial de uma conexão nova: a tag é sempre escolhida, nunca herdada de um
// padrão (uma produção cadastrada como dev por descuido seria destino).
const semTag = "(escolha)"

func opcoesTag(atual string) []string {
	if atual == semTag {
		return append([]string{semTag}, cadastro.Tags...)
	}
	return cadastro.Tags
}

// --- desenho ------------------------------------------------------------------------------------

func (a *abaConexoes) view(m *Model) string {
	w := m.largura
	if len(m.conexoes) == 0 {
		return "\n" + stMarca.Render("  Nenhuma conexão.") + "\n\n" +
			stTexto.Render("  Tecle a para cadastrar. Uma conexão é um servidor PostgreSQL: a produção (tag prod, só origem),\n  e o banco de desenvolvimento ou homologação (dev ou homolog, destino).") + "\n\n" +
			stDica.Render("  A produção só por SSH? Gere a chave da ferramenta na aba 6 (tecla g) e ponha a linha do authorized_keys\n  (tecla l) no servidor antes.")
	}
	var b strings.Builder
	b.WriteString(stCabecalho.Render(fmt.Sprintf(" CONEXÕES (%d)", len(m.conexoes))) + stDica.Render(fmt.Sprintf("  · verificadas em segundo plano a cada %s", intervalo(m.o.IntervaloVerificacao))) + "\n")
	larNome := 6
	for _, c := range m.conexoes {
		larNome = max(larNome, min(len(c.Nome), 24))
	}
	for i, c := range m.conexoes {
		marca := "  "
		nome := stTexto.Render(preencher(truncar(c.Nome, 24), larNome))
		if i == a.cursor {
			marca = stSelecao.Render("▸ ")
			nome = stSelecao.Render(preencher(truncar(c.Nome, 24), larNome))
		}
		ver := "      "
		if c.Info.VersaoNum > 0 {
			ver = preencher("PG "+versoes.Texto(c.Info.VersaoNum), 8)
		}
		tag := preencher(seloTag(c.Tag), 9)
		b.WriteString(" " + marca + nome + " " + tag + " " + stRotulo.Render(preencher(truncar(c.Onde(), max(w/3, 24)), max(w/3, 24))) + " " + stTexto.Render(ver) + " " + estadoConexao(m, c) + "\n")
	}
	if c, ok := a.atual(m); ok {
		b.WriteString("\n" + a.cartao(m, c, w))
	}
	return b.String()
}

func (a *abaConexoes) cartao(m *Model, c cadastro.Conexao, w int) string {
	larg := limitar(w-4, 40, 140)
	var b strings.Builder
	b.WriteString(seloTag(c.Tag) + " " + stMarca.Render(c.Nome) + "  " + estadoConexao(m, c) + "\n")
	linha := func(r, v string) { b.WriteString(quebrar(stRotulo.Render(preencher(r, 10)), v, larg-2) + "\n") }
	if c.Acesso == cadastro.AcessoSSH {
		chave := orDefault(c.SSHChave, "a da ferramenta")
		linha("túnel", stTexto.Render(fmt.Sprintf("%s@%s:%d, chave %s", c.SSHUsuario, c.SSHHost, c.SSHPorta, chave)))
		if c.SaltoHost != "" {
			linha("bastion", stTexto.Render(fmt.Sprintf("%s@%s:%d", c.SaltoUsuario, c.SaltoHost, c.SaltoPorta)))
		}
		linha("banco", stTexto.Render(c.Endereco()+" (visto do servidor SSH)"))
	} else {
		linha("banco", stTexto.Render(c.Endereco()))
	}
	linha("usuário", stTexto.Render(fmt.Sprintf("%s · senha: %s · sslmode %s · admin %s", c.Usuario, c.ModoSenha, c.SSLMode, c.BancoAdmin)))
	if c.Tag != cadastro.TagProd {
		if d, ok := m.aprovados[c.Info.SystemID]; ok && c.Info.SystemID != "" {
			linha("destino", stOk.Render("✔ aprovado")+stDica.Render(fmt.Sprintf(" %s por %s", idade(d.AprovadoEm), d.AprovadoPor)))
		} else {
			linha("destino", stAvisoV.Render("não aprovado")+stDica.Render(" · as cópias para este servidor ficam bloqueadas até a aprovação (tecla v)"))
		}
	}
	i := c.Info
	if i.VersaoNum > 0 {
		s := "PostgreSQL " + versoes.Texto(i.VersaoNum)
		if i.Superusuario {
			s += " · superusuário"
		}
		if i.Recuperacao {
			s += " · réplica"
		}
		s += fmt.Sprintf(" · %d banco(s)", len(i.Bancos))
		if i.Latencia > 0 {
			s += " · latência " + latencia(i.Latencia)
		}
		linha("servidor", stTexto.Render(s))
	}
	if i.Erro != "" {
		linha("parou em", stPerigoV.Render(i.Camada)+" "+stTexto.Render(i.Erro))
	}
	if d, ok := a.diag[c.Nome]; ok && len(d.Camadas) > 0 {
		var cs []string
		for _, x := range d.Camadas {
			if x.OK {
				cs = append(cs, stOk.Render("✔ ")+stDica.Render(x.Nome))
			} else {
				cs = append(cs, stPerigoV.Render("✖ "+x.Nome))
			}
		}
		linha("camadas", strings.Join(cs, "  "))
	}
	return stCartao.Width(larg).Render(strings.TrimRight(b.String(), "\n"))
}
