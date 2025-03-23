import * as path from "path";
import {
  workspace,
  ExtensionContext,
  window,
  commands,
  TextDocument,
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

// Custom error handler
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

// Function to check if a YAML file is a Kro file
async function isKroFile(document: TextDocument): Promise<boolean> {
  if (document.languageId !== "yaml") {
    return false;
  }

  const text = document.getText();
  return text.includes("apiVersion: kro.run/v1alpha");
}

export function activate(context: ExtensionContext) {
  // Server executable path
  const serverPath = path.join(
    context.extensionPath,
    "..",
    "server",
    "kro-language-server"
  );

  // Log the server path for debugging
  outputChannel.appendLine(`Server path: ${serverPath}`);
  outputChannel.show();

  // Server options
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

  // Client options
  const clientOptions: LanguageClientOptions = {
    documentSelector: [{ scheme: "file", language: "yaml" }],
    synchronize: {
      fileEvents: workspace.createFileSystemWatcher("**/*.{yaml,yml}"),
    },
    outputChannel: outputChannel,
    revealOutputChannelOn: RevealOutputChannelOn.Never,
    errorHandler: customErrorHandler,
  };

  // Create and start client
  client = new LanguageClient(
    "kroLanguageServer",
    "KRO Language Server",
    serverOptions,
    clientOptions
  );

  // Register command to restart the server
  const restartCommand = commands.registerCommand("kro.restartServer", () => {
    outputChannel.appendLine("Manually restarting server...");
    if (client) {
      client.stop().then(() => {
        client.start();
        outputChannel.appendLine("Server restarted");
      });
    }
  });

  // Register a listener for when document is opened/changed to apply Kro-specific settings
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
        // This is a Kro YAML file that changed
        outputChannel.appendLine(
          `Kro file changed: ${event.document.uri.toString()}`
        );
      }
    })
  );

  // Start client and add to subscriptions
  client.start();
  context.subscriptions.push(client, restartCommand);
}

export function deactivate(): Thenable<void> | undefined {
  if (!client) {
    return undefined;
  }
  return client.stop();
}
