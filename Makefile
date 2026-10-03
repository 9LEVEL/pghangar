# pghangar: compilar, testar e instalar.
#
#   make                 compila em bin/pghangar
#   make testar          testes unitários
#   make integracao      testes de integração (containers presos em 127.0.0.1)
#   sudo make instalar   instala em /opt/pghangar e copia para /usr/local/bin
#   make release         os arquivos de uma release em dist/: o .tar.gz e o SHA256SUMS

VERSAO  ?= v0.4.0
OPT     ?= /opt/pghangar
BIN     ?= /usr/local/bin
LDFLAGS := -s -w -X main.versao=$(VERSAO)

.PHONY: compilar testar integracao instalar release

compilar:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pghangar ./cmd/pghangar

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
	install -m 755 bin/pghangar $(OPT)/pghangar
	install -m 755 bin/pghangar $(BIN)/pghangar
	@$(BIN)/pghangar versao

# Os arquivos de uma release: um .tar.gz com o binário por arquitetura e o SHA256SUMS. Os nomes são
# usados pelo install.sh e pelo pgrunway: não os mude.
ARQS ?= amd64
release:
	rm -rf dist && mkdir -p dist
	for a in $(ARQS); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -ldflags "$(LDFLAGS)" -o dist/pghangar ./cmd/pghangar && \
		tar -czf dist/pghangar_$(VERSAO)_linux_$$a.tar.gz -C dist pghangar && rm dist/pghangar || exit 1; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS
