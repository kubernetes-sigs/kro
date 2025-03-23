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

const lsName = "kro-language-server"

var (
	version string = "0.0.1"
	handler lspProtocol.Handler
)

func main() {
	// Configure logging
	commonlog.Configure(1, nil)
	log := commonlog.GetLogger("server")
	log.Infof("KRO Language Server starting...")

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

	// Signal that server is ready for debugging if in direct debug mode
	if os.Getenv("KRO_DIRECT_DEBUG") == "true" {
		go func() {
			log.Infof("Signaling server is ready for direct debug connection...")
			exec.Command("/Users/himanshu/Desktop/open-source/kro/tools/lsp/debug-server.sh", "signal").Run()
		}()
	}

	// Create a new server
	server := server.NewServer(&handler, lsName, false)

	log.Infof("Running server on stdio...")

	// Run on stdio
	server.RunStdio()
}

func initialize(context *glsp.Context, params *lspProtocol.InitializeParams) (any, error) {
	log := commonlog.GetLogger("server")
	log.Infof("Initializing server...")

	capabilities := handler.CreateServerCapabilities()

	openClose := true
	changeValue := lspProtocol.TextDocumentSyncKindFull

	// Set specific capabilities with manual pointer values
	capabilities.TextDocumentSync = &lspProtocol.TextDocumentSyncOptions{
		OpenClose: &openClose,
		Change:    &changeValue,
	}

	// Add code action provider capability
	// Using a simpler approach that avoids type issues
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

func initialized(context *glsp.Context, params *lspProtocol.InitializedParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Server initialized")
	return nil
}

func shutdown(context *glsp.Context) error {
	log := commonlog.GetLogger("server")
	log.Infof("Server shutting down")
	return nil
}
