// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
