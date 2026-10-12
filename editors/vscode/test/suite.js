const vscode = require('vscode');
const path = require('path');
const assert = require('assert');
const fs = require('fs');
const log = (...a) => fs.appendFileSync('/tmp/suite.log', a.join(' ') + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
exports.run = async function () {
  log('start');
  await vscode.workspace.getConfiguration('gocpy').update('path', process.env.GOCPY_BIN || '/tmp/gocpy', vscode.ConfigurationTarget.Global);
  const ws = vscode.workspace.workspaceFolders[0].uri.fsPath;
  const doc = await vscode.workspace.openTextDocument(path.join(ws, 'main.gocpy'));
  await vscode.window.showTextDocument(doc);
  assert.strictEqual(doc.languageId, 'gocpy');
  log('opened');
  const ext = vscode.extensions.getExtension('rusfort.gocpy');
  await ext.activate();
  log('activated');
  let toks = null;
  for (let i = 0; i < 60 && !(toks && toks.data.length); i++) {
    await sleep(500);
    toks = await vscode.commands.executeCommand('vscode.provideDocumentSemanticTokens', doc.uri);
  }
  const legend = await vscode.commands.executeCommand('vscode.provideDocumentSemanticTokensLegend', doc.uri);
  log('legend', legend.tokenTypes.slice(0, 6).join(','), 'tokens', toks.data.length / 5);
  assert.ok(toks.data.length > 20);
  const pos = new vscode.Position(8, 15); // geo.New
  const defs = await vscode.commands.executeCommand('vscode.executeDefinitionProvider', doc.uri, pos);
  log('def', defs.map((d) => (d.uri || d.targetUri).fsPath + ':' + ((d.range || d.targetRange).start.line + 1)));
  assert.ok(defs.length && (defs[0].uri || defs[0].targetUri).fsPath.endsWith('geo.gocpy'));
  const hov = await vscode.commands.executeCommand('vscode.executeHoverProvider', doc.uri, pos);
  log('hover', hov[0].contents.map((c) => c.value).join('|').replace(/\n/g, ' '));
  const comp = await vscode.commands.executeCommand('vscode.executeCompletionItemProvider', doc.uri, new vscode.Position(13, 4));
  const labels = comp.items.map((i) => (typeof i.label === 'string' ? i.label : i.label.label));
  log('completion', labels.length, ['try', 'unsafe', 'geo', 'p', 'Exception'].filter((l) => labels.includes(l)).join(','));
  assert.ok(labels.includes('try') && labels.includes('p'));
  const refs = await vscode.commands.executeCommand('vscode.executeReferenceProvider', doc.uri, new vscode.Position(13, 18));
  log('refs X', refs.length);
  await vscode.workspace.getConfiguration('workbench').update('colorTheme', 'Default Light Modern', vscode.ConfigurationTarget.Global);
  await sleep(1000);
  await vscode.workspace.getConfiguration('workbench').update('colorTheme', process.env.THEME || 'Gocpy Dark', vscode.ConfigurationTarget.Global);
  log('theme', vscode.workspace.getConfiguration('workbench').get('colorTheme'));
  if (process.env.SHOT) {
    const d2 = await vscode.workspace.openTextDocument(path.join(ws, 'show', 'main.gocpy'));
    await vscode.window.showTextDocument(d2);
    if (process.env.SHOTLINE) { const ed = vscode.window.activeTextEditor; const r = new vscode.Range(Number(process.env.SHOTLINE), 0, Number(process.env.SHOTLINE), 0); ed.selection = new vscode.Selection(r.start, r.start); ed.revealRange(r, vscode.TextEditorRevealType.AtTop); }
    await vscode.commands.executeCommand('workbench.action.closeSidebar');
    await vscode.commands.executeCommand('workbench.action.closePanel');
    await vscode.commands.executeCommand('notifications.clearAll');
    await sleep(6000);
    require('child_process').execSync('import -window root ' + process.env.SHOT);
  }
  log('kind', vscode.window.activeColorTheme.kind);
  log('ALL OK');
};
