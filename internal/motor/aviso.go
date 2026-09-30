package motor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/9LEVEL/pghangar/internal/cadastro"
)

// Aviso é o que o webhook recebe no fim de uma execução. O campo text é o que Slack, Mattermost e
// Teams mostram; o resto é para quem quiser tratar.
type Aviso struct {
	Text      string    `json:"text"`
	Execucao  int64     `json:"execucao"`
	Tipo      string    `json:"tipo"`
	Perfil    string    `json:"perfil"`
	Estado    string    `json:"estado"`
	Mensagem  string    `json:"mensagem"`
	Destino   string    `json:"destino"`
	Banco     string    `json:"banco"`
	Inicio    time.Time `json:"inicio"`
	Fim       time.Time `json:"fim"`
	DuracaoS  int64     `json:"duracao_s"`
	Maquina   string    `json:"maquina"`
	Operador  string    `json:"operador,omitempty"`
	Anterior  string    `json:"anterior,omitempty"`
	TamanhoMB int64     `json:"tamanho_dump_mb,omitempty"`
}

var simbolo = map[string]string{
	cadastro.EstadoOK: "✅", cadastro.EstadoAguardando: "⚠️", cadastro.EstadoCancelada: "⏹", cadastro.EstadoErro: "❌",
}

// NovoAviso monta o aviso de uma execução.
func NovoAviso(e cadastro.Execucao, maquina string) Aviso {
	dur := e.Fim.Sub(e.Inicio)
	return Aviso{
		Text: fmt.Sprintf("%s pghangar em %s: #%d %s → %s/%s: %s — %s (%s)", simbolo[e.Estado], maquina, e.ID, e.Perfil,
			e.Destino, e.Banco, e.Estado, e.Mensagem, dur.Round(time.Second)),
		Execucao: e.ID, Tipo: e.Tipo, Perfil: e.Perfil, Estado: e.Estado, Mensagem: e.Mensagem, Destino: e.Destino, Banco: e.Banco,
		Inicio: e.Inicio, Fim: e.Fim, DuracaoS: int64(dur.Seconds()), Maquina: maquina, Operador: e.Operador,
		Anterior: e.BancoAnterior, TamanhoMB: e.TamanhoDump / (1 << 20),
	}
}

// Avisar manda o aviso ao webhook. Um erro aqui nunca muda o resultado da cópia: é só registrado.
func Avisar(ctx context.Context, url string, a Aviso) error {
	if url == "" {
		return nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pghangar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// O erro do net/http leva a URL, que costuma ser um segredo (a do Slack é): fica de fora.
		return fmt.Errorf("o webhook não respondeu")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("o webhook respondeu %s", resp.Status)
	}
	return nil
}

// avisar manda o aviso do fim da execução, se houver webhook configurado.
func (r *corrida) avisar() {
	cfg, err := r.d.Cadastro.Config(context.Background())
	if err != nil || cfg.Webhook == "" {
		return
	}
	if err := Avisar(context.Background(), cfg.Webhook, NovoAviso(r.e, r.d.Maquina)); err != nil {
		r.d.logf("aviso ao terminar: %v", err)
		return
	}
	r.d.logf("aviso ao terminar enviado")
}
