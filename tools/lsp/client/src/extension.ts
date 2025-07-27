import * as path from "path";
import { workspace, ExtensionContext } from "vscode";
import { LanguageClient, TransportKind } from "vscode-languageclient/node";

let client: LanguageClient;

export function activate(context: ExtensionContext) {
  // Get the server binary path
  let serverModule = process.env.KRO_SERVER_PATH;

  if (!serverModule) {
    serverModule = context.asAbsolutePath(path.join("..", "server", "kro-lsp"));
  }

  const serverOptions = {
    run: { command: serverModule, transport: TransportKind.stdio },
    debug: { command: serverModule, transport: TransportKind.stdio },
  };

  const clientOptions = {
    documentSelector: [{ scheme: "file", language: "kro-yaml" }],
    synchronize: {
      fileEvents: workspace.createFileSystemWatcher("**/*.{yaml,yml}"),
    },
  };

  // Language client
  client = new LanguageClient(
    "kroLanguageServer",
    "Kro Language Server",
    serverOptions,
    clientOptions
  );

  client
    .start()
    .then(() => {
      console.log("[KRO LSP] Client is ready");
    })
    .catch((err) => {
      console.log("KRO LSP] Failed to start client:", err);
    });
}

export function deactivate(): Thenable<void> | undefined {
  if (!client) {
    return undefined;
  }
  return client.stop();
}
