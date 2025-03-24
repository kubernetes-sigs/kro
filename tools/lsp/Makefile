# Makefile for building Kro LSP extension

.PHONY: all server client clean dirs install-deps install-go-deps cleanall

dirs:
	@mkdir -p server client/src client/out

all: dirs server client

server: dirs
	@echo "Building server..."
	go build -o server/kro-language-server ./server

client: dirs
	@echo "Building client..."
	cd client && npm install && npm run compile

clean:
	@echo "Cleaning up..."
	rm -f server/kro-language-server
	rm -rf client/node_modules client/out
	rm -f /tmp/kro-server-debug.log /tmp/kro-protocol-debug.log server-pid.txt
	find . -name "*.log" -type f -delete
	find . -name ".DS_Store" -type f -delete

cleanall: clean
	@echo "Performing deep clean..."
	rm -rf client/node_modules

# Install all Go dependencies
install-go-deps:
	@echo "Installing Go dependencies for Kro LSP..."
	go get github.com/tliron/glsp@v0.2.2
	go get github.com/tliron/commonlog@v0.2.19
	go get github.com/tliron/commonlog/simple
	go get gopkg.in/yaml.v3
	@echo "Installing Kubernetes packages..."
	go get k8s.io/apimachinery/pkg/apis/meta/v1/unstructured
	go get k8s.io/apimachinery/pkg/runtime
	go get k8s.io/kube-openapi/pkg/validation/spec
	@echo "Updating go.mod and go.sum..."
	go mod tidy
	@echo "Go dependencies installed successfully!"

# Install all dependencies (both Go and Node.js)
install-deps: install-go-deps
	@echo "Installing client dependencies..."
	cd client && npm install
	cd client && npm install --save-dev @types/node @types/vscode typescript
	@echo "All dependencies installed successfully" 