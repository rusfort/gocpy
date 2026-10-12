const path = require('path');
const { runTests } = require('@vscode/test-electron');
runTests({
  version: process.env.VSC || '1.95.3',
  extensionDevelopmentPath: path.resolve(__dirname, '..'),
  extensionTestsPath: path.resolve(__dirname, 'suite.js'),
  launchArgs: [process.env.WS || '/tmp/vsws', '--disable-extensions', '--disable-gpu', '--no-sandbox'],
}).catch((e) => { console.error('FAILED', e); process.exit(1); });
