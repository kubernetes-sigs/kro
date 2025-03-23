import * as path from "path";
import {
  workspace,
  ExtensionContext,
  window,
  commands,
  TextDocument,
  TextDocumentChangeEvent,
  languages,
} from "vscode";
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
  State,
  ErrorAction,
  CloseAction,
  RevealOutputChannelOn,
  ErrorHandler,
} from "vscode-languageclient/node";

let client: LanguageClient;
let outputChannel = window.createOutputChannel("KRO Language Server");

/**
 * Custom error handler for the language client.
 * Logs errors to output channel and determines appropriate actions.
 */
const customErrorHandler: ErrorHandler = {
  error: (error, message, count) => {
    outputChannel.appendLine(`[Error] ${error.toString()}`);
    return { action: ErrorAction.Continue };
  },
  closed: () => {
    outputChannel.appendLine("[Info] Connection to server closed");
    return { action: CloseAction.Restart };
  },
};

/**
 * Determines if a document is a KRO resource file.
 * Checks for KRO API version strings in YAML files.
 */
async function isKroFile(document: TextDocument): Promise<boolean> {
  if (document.languageId !== "yaml") {
    return false;
  }

  const text = document.getText();
  return text.includes("apiVersion: kro.run/v1alpha");
}

/**
 * Activates the extension.
 * Sets up language client, event handlers, and command registrations.
 */
export function activate(context: ExtensionContext) {
  const serverPath = path.join(
    context.extensionPath,
    "..",
    "server",
    "kro-language-server"
  );

  outputChannel.appendLine(`Server path: ${serverPath}`);
  outputChannel.show();

  const serverOptions: ServerOptions = {
    run: {
      command: serverPath,
      transport: TransportKind.stdio,
    },
    debug: {
      command: serverPath,
      transport: TransportKind.stdio,
    },
  };

  const clientOptions: LanguageClientOptions = {
    documentSelector: [{ scheme: "file", language: "yaml" }],
    synchronize: {
      fileEvents: workspace.createFileSystemWatcher("**/*.{yaml,yml}"),
    },
    outputChannel: outputChannel,
    revealOutputChannelOn: RevealOutputChannelOn.Info,
    errorHandler: customErrorHandler,
  };

  client = new LanguageClient(
    "kroLanguageServer",
    "KRO Language Server",
    serverOptions,
    clientOptions
  );

  const diagnosticsDisposable = languages.onDidChangeDiagnostics((e) => {
    for (const uri of e.uris) {
      const diagnostics = languages.getDiagnostics(uri);
      outputChannel.appendLine(`Diagnostics for ${uri}: ${diagnostics.length}`);

      if (diagnostics.length > 0) {
        outputChannel.appendLine(
          `First diagnostic: ${diagnostics[0].message} [${diagnostics[0].severity}]`
        );
      }
    }
  });

  client.start();

  // Register server restart command
  const restartCommand = commands.registerCommand("kro.restartServer", () => {
    outputChannel.appendLine("Manually restarting server...");
    if (client) {
      client.stop().then(() => {
        client.start();
        outputChannel.appendLine("Server restarted");
      });
    }
  });

  // Track KRO YAML files
  context.subscriptions.push(
    workspace.onDidOpenTextDocument(async (document) => {
      if (await isKroFile(document)) {
        outputChannel.appendLine(
          `Detected Kro file: ${document.uri.toString()}`
        );
      }
    }),
    workspace.onDidChangeTextDocument(async (event) => {
      if (await isKroFile(event.document)) {
        outputChannel.appendLine(
          `Kro file changed: ${event.document.uri.toString()}`
        );
      }
    })
  );

  context.subscriptions.push(client, restartCommand, diagnosticsDisposable);
}

/**
 * Deactivates the extension.
 * Stops the language client if active.
 */
export function deactivate(): Thenable<void> | undefined {
  if (!client) {
    return undefined;
  }
  return client.stop();
}
