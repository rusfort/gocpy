// Gocpy for VS Code: language client for `gocpy lsp`, unsafe-block
// decorations, run/build commands and the Gocpy Dark theme prompt.
'use strict';

const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { LanguageClient, TransportKind } = require('vscode-languageclient/node');

const MIN_LSP = [0, 3, 0];

let client = null;
let output = null;
let unsafeDeco = null;
const unsafeTimers = new Map();

function extraPath() {
  const home = os.homedir();
  return ['/opt/homebrew/bin', '/usr/local/bin', '/usr/local/go/bin', path.join(home, 'go', 'bin'),
    path.join(home, '.local', 'bin'), '/usr/bin'];
}

function envWithPath() {
  const env = Object.assign({}, process.env);
  const parts = (env.PATH || '').split(path.delimiter).filter(Boolean);
  for (const p of extraPath()) if (!parts.includes(p)) parts.push(p);
  env.PATH = parts.join(path.delimiter);
  return env;
}

function findGocpy() {
  const cfg = vscode.workspace.getConfiguration('gocpy').get('path');
  if (cfg) return cfg.replace(/^~(?=$|\/)/, os.homedir());
  const exe = process.platform === 'win32' ? 'gocpy.exe' : 'gocpy';
  const dirs = (process.env.PATH || '').split(path.delimiter).concat(extraPath());
  for (const d of dirs) {
    const p = path.join(d, exe);
    try { fs.accessSync(p, fs.constants.X_OK); return p; } catch (_) { /* next */ }
  }
  return null;
}

function versionOf(bin) {
  try {
    const out = cp.execFileSync(bin, ['version'], { env: envWithPath(), timeout: 5000 }).toString();
    const m = out.match(/(\d+)\.(\d+)\.(\d+)/);
    return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
  } catch (_) {
    return null;
  }
}

function less(a, b) {
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] < b[i];
  return false;
}

async function startClient(context) {
  if (!vscode.workspace.getConfiguration('gocpy').get('languageServer.enabled')) return;
  const bin = findGocpy();
  if (!bin) {
    vscode.window.showWarningMessage(
      'Gocpy: компилятор gocpy не найден. Подсветка работает, но переходы, ссылки и автодополнение требуют gocpy 0.3.0 или новее.',
      'Как установить', 'Указать путь').then((pick) => {
      if (pick === 'Как установить') vscode.env.openExternal(vscode.Uri.parse('https://rusfort.github.io/gocpy/'));
      if (pick === 'Указать путь') vscode.commands.executeCommand('workbench.action.openSettings', 'gocpy.path');
    });
    return;
  }
  const ver = versionOf(bin);
  if (ver && less(ver, MIN_LSP)) {
    vscode.window.showWarningMessage(
      `Gocpy: найден gocpy ${ver.join('.')}, а языковому серверу нужен 0.3.0 или новее. Обновите: brew upgrade gocpy. Пока работает только подсветка.`);
    return;
  }
  const serverOptions = {
    run: { command: bin, args: ['lsp'], transport: TransportKind.stdio, options: { env: envWithPath() } },
    debug: { command: bin, args: ['lsp'], transport: TransportKind.stdio, options: { env: envWithPath() } },
  };
  const clientOptions = {
    documentSelector: [{ scheme: 'file', language: 'gocpy' }],
    synchronize: { fileEvents: vscode.workspace.createFileSystemWatcher('**/{*.gocpy,*.go,gocpy.yml,gocpy.sum}') },
    outputChannel: output,
  };
  client = new LanguageClient('gocpy', 'Gocpy', serverOptions, clientOptions);
  try {
    await client.start();
  } catch (e) {
    output.appendLine('gocpy lsp: ' + e);
    vscode.window.showErrorMessage('Gocpy: не удалось запустить языковой сервер: ' + e.message);
    client = null;
    return;
  }
  for (const ed of vscode.window.visibleTextEditors) scheduleUnsafe(ed.document);
}

async function stopClient() {
  if (client) {
    const c = client;
    client = null;
    try { await c.stop(); } catch (_) { /* ignore */ }
  }
}

// ---------------------------------------------------------------- unsafe blocks

// Local fallback: find `unsafe {` and its matching brace, skipping strings and comments.
function scanUnsafe(doc) {
  const s = doc.getText();
  const out = [];
  const re = /\bunsafe\s*\{/g;
  let m;
  while ((m = re.exec(s))) {
    if (inCommentOrString(s, m.index)) continue;
    let depth = 0;
    for (let i = m.index + m[0].length - 1; i < s.length; i++) {
      const c = s[i];
      if (c === '"' || c === '\'' || c === '`') {
        const q = c;
        for (i++; i < s.length && s[i] !== q && (q === '`' || s[i] !== '\n'); i++) if (s[i] === '\\' && q !== '`') i++;
        continue;
      }
      if (c === '/' && s[i + 1] === '/') { while (i < s.length && s[i] !== '\n') i++; continue; }
      if (c === '/' && s[i + 1] === '*') { const e = s.indexOf('*/', i + 2); i = e < 0 ? s.length : e + 1; continue; }
      if (c === '{') depth++;
      if (c === '}' && --depth === 0) {
        out.push(new vscode.Range(doc.positionAt(m.index), doc.positionAt(i + 1)));
        break;
      }
    }
  }
  return out;
}

function inCommentOrString(s, at) {
  const ls = s.lastIndexOf('\n', at - 1) + 1;
  const before = s.slice(ls, at);
  let q = null;
  for (let i = 0; i < before.length; i++) {
    const c = before[i];
    if (q) { if (c === '\\') i++; else if (c === q) q = null; continue; }
    if (c === '"' || c === '\'' || c === '`') q = c;
    else if (c === '/' && before[i + 1] === '/') return true;
  }
  return q !== null;
}

function scheduleUnsafe(doc) {
  if (!doc || doc.languageId !== 'gocpy') return;
  const key = doc.uri.toString();
  clearTimeout(unsafeTimers.get(key));
  unsafeTimers.set(key, setTimeout(() => applyUnsafe(doc), 250));
}

function applyUnsafe(doc) {
  const enabled = vscode.workspace.getConfiguration('gocpy').get('unsafeHighlight');
  const ranges = enabled ? scanUnsafe(doc) : [];
  for (const ed of vscode.window.visibleTextEditors) {
    if (ed.document === doc) ed.setDecorations(unsafeDeco, ranges);
  }
}

// ---------------------------------------------------------------- commands

function runInTerminal(sub) {
  const ed = vscode.window.activeTextEditor;
  if (!ed || ed.document.languageId !== 'gocpy') {
    vscode.window.showInformationMessage('Gocpy: откройте файл .gocpy');
    return;
  }
  const bin = findGocpy() || 'gocpy';
  ed.document.save().then(() => {
    const dir = path.dirname(ed.document.uri.fsPath);
    let term = vscode.window.terminals.find((t) => t.name === 'Gocpy');
    if (!term) term = vscode.window.createTerminal({ name: 'Gocpy', env: { PATH: envWithPath().PATH } });
    term.show(true);
    const q = (p) => (/[\s"'$`\\]/.test(p) ? JSON.stringify(p) : p);
    term.sendText(`${q(bin)} ${sub} ${q(dir)}`);
  });
}

async function suggestTheme(context) {
  const cfg = vscode.workspace.getConfiguration('gocpy');
  if (!cfg.get('suggestTheme') || context.globalState.get('gocpy.themeSuggested')) return;
  const current = vscode.workspace.getConfiguration('workbench').get('colorTheme');
  if (current === 'Gocpy Dark') return;
  await context.globalState.update('gocpy.themeSuggested', true);
  const pick = await vscode.window.showInformationMessage(
    'Gocpy: включить тему Gocpy Dark с цветами как в Gocpy IDE?', 'Включить', 'Не сейчас');
  if (pick === 'Включить') await useTheme();
}

async function useTheme() {
  await vscode.workspace.getConfiguration('workbench').update('colorTheme', 'Gocpy Dark', vscode.ConfigurationTarget.Global);
}

// ---------------------------------------------------------------- activate

async function activate(context) {
  output = vscode.window.createOutputChannel('Gocpy');
  unsafeDeco = vscode.window.createTextEditorDecorationType({
    backgroundColor: new vscode.ThemeColor('gocpy.unsafeBackground'),
    isWholeLine: true,
    rangeBehavior: vscode.DecorationRangeBehavior.ClosedClosed,
  });
  context.subscriptions.push(output, unsafeDeco,
    vscode.commands.registerCommand('gocpy.restartServer', async () => { await stopClient(); await startClient(context); }),
    vscode.commands.registerCommand('gocpy.run', () => runInTerminal('run')),
    vscode.commands.registerCommand('gocpy.build', () => runInTerminal('build')),
    vscode.commands.registerCommand('gocpy.check', () => runInTerminal('check')),
    vscode.commands.registerCommand('gocpy.useTheme', useTheme),
    vscode.workspace.onDidChangeTextDocument((e) => scheduleUnsafe(e.document)),
    vscode.workspace.onDidOpenTextDocument(scheduleUnsafe),
    vscode.window.onDidChangeVisibleTextEditors((eds) => eds.forEach((ed) => scheduleUnsafe(ed.document))),
    vscode.workspace.onDidChangeConfiguration(async (e) => {
      if (e.affectsConfiguration('gocpy.unsafeHighlight')) vscode.window.visibleTextEditors.forEach((ed) => scheduleUnsafe(ed.document));
      if (e.affectsConfiguration('gocpy.path') || e.affectsConfiguration('gocpy.languageServer.enabled')) {
        await stopClient();
        await startClient(context);
      }
    }),
  );
  vscode.window.visibleTextEditors.forEach((ed) => scheduleUnsafe(ed.document));
  suggestTheme(context);
  startClient(context); // do not block activation on the server or on prompts
}

function deactivate() {
  return stopClient();
}

module.exports = { activate, deactivate };
