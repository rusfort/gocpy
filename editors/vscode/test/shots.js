// Screenshots for the Marketplace page: SUITE=shots.js WS=<workspace> OUT=<dir>.
const vscode = require('vscode');
const path = require('path');
const fs = require('fs');
const { execSync } = require('child_process');
const log = (...a) => fs.appendFileSync('/tmp/shots.log', a.join(' ') + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const out = process.env.OUT || '/tmp/shots';

async function shot(name) {
  await vscode.commands.executeCommand('notifications.clearAll');
  await sleep(1500);
  execSync(`import -window root ${path.join(out, name + '.png')}`);
  log('shot', name);
}

function find(doc, text, off = 0) {
  const i = doc.getText().indexOf(text);
  if (i < 0) throw new Error('not found: ' + text);
  return doc.positionAt(i + off);
}

exports.run = async function () {
  fs.mkdirSync(out, { recursive: true });
  try {
    // no window manager under xvfb: size the window directly
    for (const id of execSync('xdotool search --onlyvisible --class code').toString().trim().split(/\s+/)) {
      execSync(`xdotool windowmove ${id} 0 0 windowsize ${id} ${process.env.SIZE || '1280 800'}`);
    }
  } catch (e) { log('xdotool', e.message); }
  await sleep(1500);
  const ws = vscode.workspace.workspaceFolders[0].uri.fsPath;
  const cfg = vscode.workspace.getConfiguration();
  await cfg.update('gocpy.path', process.env.GOCPY_BIN || '/tmp/gocpy', vscode.ConfigurationTarget.Global);
  await cfg.update('workbench.colorTheme', 'Default Light Modern', vscode.ConfigurationTarget.Global);
  await sleep(800);
  await cfg.update('workbench.colorTheme', 'Gocpy Dark', vscode.ConfigurationTarget.Global);
  const doc = await vscode.workspace.openTextDocument(path.join(ws, 'main.gocpy'));
  const ed = await vscode.window.showTextDocument(doc);
  await vscode.extensions.getExtension('rusfort.gocpy').activate();
  for (let i = 0; i < 60; i++) {
    const t = await vscode.commands.executeCommand('vscode.provideDocumentSemanticTokens', doc.uri);
    if (t && t.data.length) break;
    await sleep(500);
  }
  await vscode.commands.executeCommand('workbench.action.closeSidebar');
  await vscode.commands.executeCommand('workbench.action.closePanel');
  await vscode.commands.executeCommand('workbench.action.closeAuxiliaryBar');
  await cfg.update('editor.stickyScroll.enabled', false, vscode.ConfigurationTarget.Global);
  const esc = async () => { execSync('xdotool key Escape'); await sleep(400); };
  const at = (pos, top) => {
    ed.selection = new vscode.Selection(pos, pos);
    ed.revealRange(new vscode.Range(top, 0, top, 0), vscode.TextEditorRevealType.AtTop);
  };
  const line = (text) => find(doc, text).line;

  // 1. overview
  at(new vscode.Position(line('func main'), 0), line('@throws') - 2);
  await sleep(3000);
  await shot('overview');

  // 2. hover with documentation from another package
  at(find(doc, 'geo.Distance', 5), line('@throws') - 2);
  await vscode.commands.executeCommand('editor.action.showHover');
  await sleep(2500);
  await shot('hover');
  await esc();

  // 3. completion of members
  const after = line('p.Scale(2)');
  await ed.edit((e) => e.insert(new vscode.Position(after + 1, 0), '    geo.\n'));
  at(new vscode.Position(after + 1, 8), line('@throws') - 2);
  await sleep(1500);
  await vscode.commands.executeCommand('editor.action.triggerSuggest');
  await sleep(2000);
  if (process.env.TOGGLE) await vscode.commands.executeCommand('toggleSuggestionDetails');
  await sleep(1500);
  await shot('completion');
  await esc();
  await esc();
  await vscode.commands.executeCommand('undo');
  await sleep(500);

  // 4. diagnostics: strict rules
  const l2 = line('    total :=');
  await ed.edit((e) => e.insert(new vscode.Position(l2, 0), '    count := 10\n'));
  await sleep(2500);
  at(find(doc, 'count :=', 1), line('@throws') - 2);
  await vscode.commands.executeCommand('editor.action.showHover');
  await sleep(2000);
  await shot('diagnostics');
  await vscode.commands.executeCommand('undo');
  log('done');
};
