# copia-banco: compilar, testar e instalar.
#
#   make                 compila em bin/copia-banco
#   make testar          testes unitários
#   make integracao      testes de integração (containers presos em 127.0.0.1)
#   sudo make instalar   instala em /opt/copia-banco e copia para /usr/local/bin

VERSAO  ?= v0.2.0
OPT     ?= /opt/copia-banco
BIN     ?= /usr/local/bin
LDFLAGS := -s -w -X main.versao=$(VERSAO)

.PHONY: compilar testar integracao instalar

compilar:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/copia-banco ./cmd/copia-banco

testar:
	gofmt -l . | (! grep .)
	go vet ./...
	go test ./...

integracao:
	go test -tags integracao ./internal/motor/ ./internal/execucao/ -timeout 40m

# As duas cópias são sempre o mesmo binário: /opt é a instalação, /usr/local/bin é para rodar de
# qualquer lugar (como o pgtower).
instalar: compilar
	install -d -m 755 $(OPT)
	install -m 755 bin/copia-banco $(OPT)/copia-banco
	install -m 755 bin/copia-banco $(BIN)/copia-banco
	@$(BIN)/copia-banco versao
