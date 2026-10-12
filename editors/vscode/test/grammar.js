const fs = require('fs'), path = require('path');
const vsctm = require('vscode-textmate'), oniguruma = require('vscode-oniguruma');
const wasm = fs.readFileSync(require.resolve('vscode-oniguruma/release/onig.wasm')).buffer;
const lib = oniguruma.loadWASM(wasm).then(() => ({
  createOnigScanner: (p) => new oniguruma.OnigScanner(p), createOnigString: (s) => new oniguruma.OnigString(s) }));
const reg = new vsctm.Registry({ onigLib: lib, loadGrammar: async () =>
  vsctm.parseRawGrammar(fs.readFileSync(path.join(__dirname, '../syntaxes/gocpy.tmLanguage.json'), 'utf8'), 'g.json') });
const src = fs.readFileSync(process.argv[2], 'utf8');
reg.loadGrammar('source.gocpy').then((g) => {
  let st = vsctm.INITIAL;
  src.split('\n').forEach((line, i) => {
    const r = g.tokenizeLine(line, st);
    const parts = r.tokens.filter(t => line.slice(t.startIndex, t.endIndex).trim()).map(t =>
      `${line.slice(t.startIndex, t.endIndex)}=${t.scopes[t.scopes.length - 1].replace(/\.gocpy$/, '')}`);
    if (parts.length) console.log(i + 1 + ': ' + parts.join('  '));
    st = r.ruleStack;
  });
});
