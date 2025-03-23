package main

import (
	"os"
	"os/exec"

	"github.com/kro-run/kro/tools/lsp/protocol"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	lspProtocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/tliron/glsp/server"

	_ "github.com/tliron/commonlog/simple"
)

// Language server name for LSP identification
const lsName = "kro-language-server"

var (
	version string = "0.0.1"
	handler lspProtocol.Handler
)

// main initializes and starts the KRO Language Server
func main() {
	commonlog.Configure(1, nil)
	log := commonlog.GetLogger("server")
	log.Infof("KRO Language Server starting...")

	// Configure LSP method handlers
	handler = lspProtocol.Handler{
		Initialize:  initialize,
		Initialized: initialized,
		Shutdown:    shutdown,
		TextDocumentDidOpen: func(context *glsp.Context, params *lspProtocol.DidOpenTextDocumentParams) error {
			log.Infof("Document opened: %s", params.TextDocument.URI)
			return protocol.TextDocumentDidOpen(context, params)
		},
		TextDocumentDidChange: func(context *glsp.Context, params *lspProtocol.DidChangeTextDocumentParams) error {
			log.Infof("Document changed: %s", params.TextDocument.URI)
			return protocol.TextDocumentDidChange(context, params)
		},
		TextDocumentDidClose:           protocol.TextDocumentDidClose,
		WorkspaceDidChangeWatchedFiles: protocol.DidChangeWatchedFiles,
		TextDocumentCodeAction:         protocol.HandleCodeAction,
	}

	log.Infof("Starting server...")

	// Support direct debugging mode if enabled
	if os.Getenv("KRO_DIRECT_DEBUG") == "true" {
		go func() {
			log.Infof("Signaling server is ready for direct debug connection...")
			exec.Command("/Users/himanshu/Desktop/open-source/kro/tools/lsp/debug-server.sh", "signal").Run()
		}()
	}

	server := server.NewServer(&handler, lsName, false)
	log.Infof("Running server on stdio...")
	server.RunStdio()
}

// initialize provides LSP server capabilities during client connection initialization
func initialize(context *glsp.Context, params *lspProtocol.InitializeParams) (any, error) {
	log := commonlog.GetLogger("server")
	log.Infof("Initializing server...")

	capabilities := handler.CreateServerCapabilities()

	// Configure text document synchronization
	openClose := true
	changeValue := lspProtocol.TextDocumentSyncKindFull
	capabilities.TextDocumentSync = &lspProtocol.TextDocumentSyncOptions{
		OpenClose: &openClose,
		Change:    &changeValue,
	}

	// Enable code action support for quick fixes
	codeActionProvider := true
	capabilities.CodeActionProvider = &codeActionProvider

	return lspProtocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &lspProtocol.InitializeResultServerInfo{
			Name:    lsName,
			Version: &version,
		},
	}, nil
}

// initialized handles post-initialization tasks
func initialized(context *glsp.Context, params *lspProtocol.InitializedParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Server initialized")
	return nil
}

// shutdown handles server shutdown requests
func shutdown(context *glsp.Context) error {
	log := commonlog.GetLogger("server")
	log.Infof("Server shutting down")
	return nil
}
