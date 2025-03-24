# Kro Language Server

This directory contains a Language Server Protocol (LSP) implementation for Kro YAML files. It provides editor features like validation, diagnostics, and in the future, completions and hover information.

## Structure

- `server/`: Go code for the language server
- `client/`: TypeScript code for the VS Code extension
- `protocol/`: Shared protocol code between server and client

### ✅ Current Implementation Status

- **Language Server Protocol (LSP) Infrastructure**

  - Basic LSP server structure in Go
  - VS Code client integration
  - Error reporting with proper locations in the document
  - Real-time diagnostics

- **YAML Validation** (In Progress)

  - Kro ResourceGraphDefinition schema validation
  - Basic syntax validation
  - Error reporting with line/column positions

- **Basic VS Code Extension**
  - Syntax highlighting for Kro YAML files (Implemented, but need to finalize syntax colors)
  - Extension activation for YAML files
  - Error diagnostics display

### ⏳ To Be Implemented

- **Auto-completion**

  - Schema field auto-completion
  - CEL expression auto-completion
  - Type and property suggestions

- **Enhanced Validation**

  - Integrated Kubernetes resource template validation
  - CEL expression validation
  - Integration with Kubeconform for Kubernetes validation

- **Developer Experience**
  - Hover documentation for schema fields
  - Jump to definition support
  - Code actions for quick fixes
  - Support for external schema sources

## Prerequisites

- Go 1.24+ (for server development)
- Node.js 14+ and npm (for client development)
- Visual Studio Code (for testing)

## Setup and Build Instructions

### Building from Source

1. Clone the repository:

   ```bash
   git clone https://github.com/kro-run/kro.git
   git checkout lsp
   cd kro/tools/lsp
   ```

2. Install all dependencies:

   ```bash
   make install-deps
   ```

3. Build both server and client components:
   ```bash
   make all
   ```

### Running the Extension Locally

#### Run in Development Mode

1. Open the `tools/lsp/client` directory in VS Code:

   ```bash
   cd /path/to/kro/tools/lsp/client
   code .
   ```

2. Navigate to the client directory in the VS Code Explorer

3. Press F5 or select "Run and Debug" from the sidebar and click the play button

4. This will launch a new VS Code window with the extension loaded

5. Open a Kro YAML file (with `apiVersion: kro.run/v1alpha1`) in the new window to test the extension
