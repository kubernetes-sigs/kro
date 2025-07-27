.PHONY: build clean test

# Build the LSP server
build-server:
	cd server && go mod tidy && go build -o kro-lsp .

# Build the VS Code extension
build-client:
	cd client && npm install
	cd client && npm run compile

# Build everything
build: build-server build-client

# Separate build commands for convenience
build/server: build-server

build/client: build-client

# Clean build artifacts
clean:
	rm -f server/kro-lsp
	rm -rf client/out
	rm -rf client/node_modules
	rm -rf client/package-lock.json

# Install dependencies
install:
	cd server && go mod tidy
	cd client && npm install
