// Run terminal.js against a minimal fake DOM to surface runtime errors.
const fs = require('fs');
const el = () => ({ value: '', options: [], style: {}, classList: { contains: () => false }, addEventListener() {},
  appendChild() {}, querySelectorAll: () => [], getContext: () => ({ fillRect() {}, drawImage() {},
  fillText() {}, createImageData: (w, h) => ({ data: new Uint8ClampedArray(w * h * 4) }), putImageData() {} }),
  set innerHTML(v) {}, focus() {}, clientWidth: 1000, clientHeight: 700, offsetHeight: 30, width: 300, height: 150 });
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || el()), createElement: () => el(),
  addEventListener() {}, querySelector: () => null, querySelectorAll: () => [],
  body: { appendChild() {}, removeChild() {} }, activeElement: null };
global.window = { addEventListener() {}, innerHeight: 800, isSecureContext: true };
global.navigator = { platform: 'MacIntel', userAgent: 'test' };
// argv[3] (optional): JSON object preloaded into localStorage
const store = process.argv[3] ? JSON.parse(process.argv[3]) : {};
global.localStorage = { getItem: (k) => (k in store ? store[k] : null), setItem(k, v) { store[k] = v; } };
global.location = { protocol: 'http:', host: 'x' };
global.WebSocket = function () { this.readyState = 0; };
global.WebSocket.OPEN = 1;
global.setInterval = () => 0;
try {
  new Function(fs.readFileSync(process.argv[2], 'utf8'))();
  console.log('ran OK');
  // the values the page restored, for the Go test
  const v = (id) => (els[id] ? els[id].value : undefined);
  console.log('VALUES ' + JSON.stringify({ host: v('host-input'), port: v('port-input'),
    model: v('model-select'), color: v('color-select'), codepage: v('codepage-select'),
    muteClick: els['mute-click'] ? els['mute-click'].checked : undefined,
    clickStyle: v('click-style') }));
} catch (e) { console.log('RUNTIME ERROR:', e.message); process.exit(1); }
