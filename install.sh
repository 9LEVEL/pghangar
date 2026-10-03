#!/bin/sh
# Instalador do pghangar: baixa o binário de uma release do GitHub, confere o SHA-256 e instala em
# /opt/pghangar e /usr/local/bin, como o `make instalar`.
#
#   curl -fsSL https://pghangar.dev/install | sudo sh
#   curl -fsSL https://raw.githubusercontent.com/9LEVEL/pghangar/main/install.sh | sudo sh
#
# O pghangar roda como root (é uma ferramenta de sysadmin), e o instalador também. Para atualizar, o
# mesmo comando: uma cópia em andamento continua com o binário que já estava rodando, e os dados em
# /var/lib/pghangar ficam como estão.
#
# Variáveis:
#   PGHANGAR_VERSAO=vX.Y.Z   uma versão fixa (o padrão é a última release)
#
# Não compila nada: o pghangar é um binário estático (sem CGO). Precisa de Linux e, para copiar, do
# Docker, que roda o pg_dump e o pg_restore oficiais.

set -eu

REPO="9LEVEL/pghangar"
VERSAO="${PGHANGAR_VERSAO:-latest}"
OPT=/opt/pghangar
BIN=/usr/local/bin

# Cores só num terminal.
if [ -t 2 ]; then
	B=$(printf '\033[1m'); G=$(printf '\033[32m'); Y=$(printf '\033[33m'); R=$(printf '\033[31m'); N=$(printf '\033[0m')
else
	B=; G=; Y=; R=; N=
fi
diz()   { printf '%s %s\n' "${B}▸${N}" "$*" >&2; }
ok()    { printf '%s %s\n' "${G}✓${N}" "$*" >&2; }
aviso() { printf '%s %s\n' "${Y}!${N}" "$*" >&2; }
erro()  { printf '%s %s\n' "${R}✗${N}" "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || erro "o pghangar roda como root, e o instalador também: curl -fsSL https://pghangar.dev/install | sudo sh"

# --- o download: curl ou wget ---
if command -v curl >/dev/null 2>&1; then
	baixar() { curl -fsSL -o "$2" "$1"; }
	TEM_CURL=1
elif command -v wget >/dev/null 2>&1; then
	baixar() { wget -qO "$2" "$1"; }
	TEM_CURL=0
else
	erro "é preciso o curl ou o wget para baixar"
fi
command -v sha256sum >/dev/null 2>&1 || erro "é preciso o sha256sum (coreutils) para conferir o binário"

# --- o sistema e a arquitetura ---
[ "$(uname -s)" = Linux ] || erro "o pghangar roda em Linux, no servidor que recebe as cópias"
case "$(uname -m)" in
	x86_64 | amd64)  ARQ=amd64 ;;
	aarch64 | arm64) ARQ=arm64 ;;
	*) erro "processador $(uname -m) sem binário pronto. Compile do código: https://github.com/$REPO#instalar" ;;
esac

# --- a versão ---
if [ "$VERSAO" = latest ]; then
	diz "procurando a última release…"
	if [ "$TEM_CURL" = 1 ]; then
		# O redirecionamento do /releases/latest dá a tag, sem o limite da API.
		VERSAO=$(curl -sSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -E 's#.*/tag/##')
	else
		VERSAO=$(wget -qO- "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' || true)
	fi
fi
case "$VERSAO" in
	v*) : ;;
	*) erro "nenhuma release encontrada. As releases: https://github.com/$REPO/releases" ;;
esac
ok "versão ${B}$VERSAO${N} (linux/$ARQ)"

ARQUIVO="pghangar_${VERSAO}_linux_${ARQ}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSAO"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

diz "baixando $ARQUIVO…"
baixar "$BASE/$ARQUIVO" "$tmp/$ARQUIVO" 2>/dev/null ||
	erro "a $VERSAO não tem $ARQUIVO. As releases: https://github.com/$REPO/releases (ou compile do código)"

# --- o SHA-256: sem ele, nada é instalado ---
baixar "$BASE/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null || erro "a $VERSAO não tem o SHA256SUMS: sem ele, o binário não é conferido, e nada é instalado"
quer=$(awk -v a="$ARQUIVO" '$2 == a {print $1}' "$tmp/SHA256SUMS")
[ -n "$quer" ] || erro "o SHA256SUMS da $VERSAO não lista $ARQUIVO: nada é instalado"
veio=$(sha256sum "$tmp/$ARQUIVO" | awk '{print $1}')
[ "$quer" = "$veio" ] || erro "o SHA-256 não confere (esperado $quer, veio $veio): nada é instalado"
ok "SHA-256 conferido"

tar -xzf "$tmp/$ARQUIVO" -C "$tmp" pghangar || erro "o arquivo não tem o binário pghangar"

# --- a instalação: as duas cópias são o mesmo binário ---
install -d -m 755 "$OPT"
install -m 755 "$tmp/pghangar" "$OPT/pghangar"
install -m 755 "$tmp/pghangar" "$BIN/pghangar"
ok "instalado: $("$BIN/pghangar" versao) em $OPT e $BIN"

case ":$PATH:" in
	*":$BIN:"*) : ;;
	*) aviso "$BIN não está no PATH: rode $BIN/pghangar" ;;
esac

if ! command -v docker >/dev/null 2>&1; then
	aviso "sem o Docker: o pghangar roda o pg_dump e o pg_restore nos containers oficiais do PostgreSQL. Instale-o (https://docs.docker.com/engine/install/) ou use o pgrunway (https://pgrunway.dev), que instala os dois"
fi

printf '\n%s Pronto. Rode %s: na aba 6, %s baixa as imagens e %s gera a chave SSH; na aba 3, cadastre a produção e o destino; na aba 1, o perfil.\n' \
	"${G}✓${N}" "${B}sudo pghangar${N}" "${B}b${N}" "${B}g${N}" >&2
