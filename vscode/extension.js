// PARLEY v0 — VS Code extension: starts the LSP (python3 parley_lsp.py) via stdio.
const fs = require("fs");
const path = require("path");
const {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
  Executable,
} = require("vscode-languageclient/node");

function findServerScript(context, document) {
  const candidates = [
    path.join(context.extensionPath, "..", "..", "parley_lsp.py"),
    path.join(path.dirname(document.uri.fsPath), "parley_lsp.py"),
    path.join(process.env.HOME || "", "parley", "parley_lsp.py"),
  ];
  for (const c of candidates) {
    if (fs.existsSync(c)) return c;
  }
  return null;
}

function activate(context) {
  const open = (document) => {
    if (document.languageId !== "parley") return;
    const script = findServerScript(context, document);
    if (!script) {
      return void context.workspace.showInformationMessage(
        "PARLEY LSP: parley_lsp.py not found (expected at ~/parley/parley_lsp.py)");
    }
    const serverOptions = new Executable("python3", [script]);
    const clientOptions = new LanguageClientOptions({
      documentSelector: [{ language: "parley" }],
      diagnosticCollectionName: "parley",
    });
    const client = new LanguageClient(context, "parley", serverOptions, clientOptions);
    client.start();
    context.subscriptions.push(client);
  };
  context.workspace.onDidOpenTextDocument(open);
  context.workspace.onDidSaveTextDocument(open);
}

function deactivate() {}

module.exports = { activate, deactivate };
