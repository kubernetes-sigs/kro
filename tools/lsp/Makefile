SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: lsp

.PHONY: lsp-server
lsp-server:
	cd server && go build -o kro-lsp .

.PHONY: lsp-client
lsp-client:
	cd client && npm run compile

.PHONY: lsp
lsp: lsp-client lsp-server

.PHONY: clean
clean:
	rm -f server/kro-lsp
	rm -rf client/out client/node_modules client/package-lock.json

.PHONY: install
install:
	cd client && npm install
	cd server && go mod tidy


