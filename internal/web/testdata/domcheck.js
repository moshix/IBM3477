// Run terminal.js against a minimal fake DOM to surface runtime errors.
const fs = require('fs');
// fillStyles: every fill colour the page used for text (fillText), so a
// test can see which colour the screen was drawn in
const fillStyles = [];
// selRects: every rectangle filled in the copy-selection colour
const selRects = [];
const el = () => ({ value: '', options: [], style: {}, classList: { contains: () => false },
  // listeners are kept so a test can fire 'change' as a user would
  listeners: {}, addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); },
  fire(t) { (this.listeners[t] || []).forEach((f) => f({})); },
  appendChild() {}, querySelectorAll: () => [], getBoundingClientRect: () => ({ left: 0, top: 0, width: 60, height: 30 }),
  getContext: () => ({ fillRect(x, y, w, h) {
    if (this.fillStyle === 'rgba(119,255,153,0.35)') selRects.push([x, y, w, h]);
  }, drawImage() {},
  fillText() { fillStyles.push(this.fillStyle); },
  createImageData: (w, h) => ({ data: new Uint8ClampedArray(w * h * 4) }), putImageData() {} }),
  set innerHTML(v) {}, focus() {}, clientWidth: 1000, clientHeight: 700, offsetHeight: 30, width: 300, height: 150 });
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || el()), createElement: () => el(),
  addEventListener() {}, querySelector: () => null, querySelectorAll: () => [],
  body: { appendChild() {}, removeChild() {} }, activeElement: null, hasFocus: () => true };
global.window = { addEventListener() {}, innerHeight: 800, isSecureContext: true };
global.navigator = { platform: 'MacIntel', userAgent: 'test' };
// argv[3] (optional): JSON object preloaded into localStorage
const store = process.argv[3] ? JSON.parse(process.argv[3]) : {};
global.localStorage = { getItem: (k) => (k in store ? store[k] : null), setItem(k, v) { store[k] = v; } };
global.location = { protocol: 'http:', host: 'x' };
let lastWS = null;
global.WebSocket = function () { this.readyState = 0; lastWS = this; };
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
  // tube (#crt) and picture (canvas) CSS sizes and the zoom label as
  // loaded, then after zoom in / out
  const zoomState = () => JSON.stringify({ crt: els.crt.style.width + 'x' + els.crt.style.height,
    w: els.screen.style.width, h: els.screen.style.height, label: els['btn-zoom-reset'].textContent });
  console.log('ZOOM load ' + zoomState());
  els['btn-zoom-in'].onclick(); console.log('ZOOM in ' + zoomState());
  for (let i = 0; i < 30; i++) els['btn-zoom-out'].onclick();
  console.log('ZOOM min ' + zoomState());
  els['btn-zoom-reset'].onclick(); console.log('ZOOM reset ' + zoomState());
  console.log('ZOOM saved ' + JSON.parse(store['webibm3477-settings']).zoom);
  // Settings take effect on 'change', with the dialog still open (Close
  // is never pressed here): colour redraws the screen, sound and mute
  // are saved and read live
  els['color-select'].value = 'amber'; els['color-select'].fire('change');
  els['sound-select'].value = 'off'; els['sound-select'].fire('change');
  els['mute-click'].checked = true; els['mute-click'].fire('change');
  const st = JSON.parse(store['webibm3477-settings']);
  console.log('LIVE ' + JSON.stringify({ fill: fillStyles[fillStyles.length - 1],
    color: st.color, sound: st.sound, muteClick: st.muteClick }));
  // Copy selection: a 6x3 screen (row 2 is the status row) with text,
  // blanks (0x40 and 0x00) and two blank non-display (password) cells.
  // Dragging over rows 0-1 must mark only the text and password cells.
  const ext = (r, c) => (r === 1 && (c === 1 || c === 2) ? 0x80 : 0);
  const codes = [0xC1, 0xC2, 0x40, 0x40, 0x40, 0xC3,
    0x40, 0x40, 0x40, 0x00, 0xC4, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40];
  const cells = [];
  codes.forEach((code, n) => cells.push(code, 0x20, ext(Math.floor(n / 6), n % 6)));
  const b64s = (a) => Buffer.from(a).toString('base64');
  const msg = (m) => lastWS.onmessage({ data: JSON.stringify(m) });
  msg({ type: 'font', font: b64s(new Uint8Array(0x8000)) });
  msg({ type: 'screen', rows: 3, cols: 6, cellw: 10, cellh: 10, cells: b64s(cells),
    currow: 0, curcol: 0, curon: false, led: 0, lines: [] });
  els.screen.listeners.mousedown[0]({ button: 0, clientX: 1, clientY: 1 });
  selRects.length = 0;
  els.screen.listeners.mousemove[0]({ clientX: 59, clientY: 19 });
  console.log('SEL ' + JSON.stringify(selRects));
} catch (e) { console.log('RUNTIME ERROR:', e.message); process.exit(1); }
