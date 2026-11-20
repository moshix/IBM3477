// webtwinax browser side: draws the emulated terminal's screen with the
// terminal's own font (sent from its character-generator RAM) and passes
// physical key presses to the emulated keyboard. All terminal logic runs
// in the emulated firmware on the server.
'use strict';

const GLYPH_W = 16;    // font RAM glyphs are up to 16 dots wide
const FONT_ROWS = 32;  // font RAM holds up to 32 pixel rows per glyph
const FONT_RIGHT = 0x2000; // right glyph halves start 8K after the left
const FONT2 = 0x4000;  // font 2 (attribute bit 7) follows font 1
let cellW = 15;        // dots per cell, from the server (15 at 80 columns)
let cellH = 20;        // scan lines per character row, from the CRTC
// Phosphor colours the 3477 shipped with (Settings > Display): normal
// and high intensity. White is the slightly warm white of a real 3477.
// High intensity was raised on request (+15%, then +10%; channels capped
// at 255).
// - Amber high keeps the hue of normal amber: (G-B)/(R-B) = 2/3, as in
//   E0 96 00. Raising green alone had made it yellow.
// - White high is at full brightness, so normal white was lowered by
//   1/1.2 instead (C8C8BC -> A7A79D). The highlight now stands out 20%
//   more.
const PHOSPHORS = {
  green: { normal: [0x2a, 0xc8, 0x55], high: [0x97, 0xff, 0xc2] },
  amber: { normal: [0xe0, 0x96, 0x00], high: [0xff, 0xbe, 0x3c] },
  white: { normal: [0xa7, 0xa7, 0x9d], high: [0xff, 0xff, 0xff] },
};
const COLORS = {       // the colours in use, normal and high intensity
  normal: PHOSPHORS.green.normal,
  high: PHOSPHORS.green.high,
  black: [0, 0, 0],
};

// User settings kept by the browser across restarts: last host and port,
// terminal model and size, codepage and text colour (the key mapping has
// its own entry). Server-side defaults apply only to what was never set.
const SETTINGS_STORE = 'webibm3477-settings';
let settings = {};
try {
  settings = JSON.parse(localStorage.getItem(SETTINGS_STORE) || '{}') || {};
} catch (e) { settings = {}; /* private mode or bad JSON */ }
function saveSetting(name, value) {
  settings[name] = value;
  try { localStorage.setItem(SETTINGS_STORE, JSON.stringify(settings)); } catch (e) { /* private mode */ }
}

const defaults = window.WEBTWINAX_DEFAULTS || { model: '3477', host: '', port: 23 };
const canvas = document.getElementById('screen');
const ctx = canvas.getContext('2d');
const statusText = document.getElementById('status-text');
const statusLED = document.getElementById('status-led');

const hostInput = document.getElementById('host-input');
const portInput = document.getElementById('port-input');
hostInput.value = settings.host !== undefined ? settings.host : defaults.host;
portInput.value = settings.port !== undefined ? settings.port : String(defaults.port);
if (defaults.locked) {
  // the server connects only to its own host: show it, not editable
  hostInput.value = defaults.host;
  portInput.value = String(defaults.port);
  hostInput.disabled = portInput.disabled = true;
  hostInput.title = portInput.title = 'This server is locked to this host';
}
hostInput.addEventListener('change', () => saveSetting('host', hostInput.value.trim()));
portInput.addEventListener('change', () => saveSetting('port', portInput.value.trim()));

// Screen sizes each model supports. The choice is the terminal type the
// TN5250 session announces to the host (as in web5250); the emulated
// firmware then displays whatever format the host sends. The 3486 only
// supports 24x80 (27x132 is a 3487/3488 feature), so it is locked.
const SIZES = {
  '3477': [['24x80', '24×80 (IBM-3179-2)'], ['27x132', '27×132 (IBM-3477-FC)']],
  '3486': [['24x80', '24×80 (IBM-3179-2)']],
};
const modelSelect = document.getElementById('model-select');
const sizeSelect = document.getElementById('size-select');

function fillSizes() {
  const opts = SIZES[modelSelect.value] || SIZES['3486'];
  const keep = sizeSelect.value;
  sizeSelect.innerHTML = '';
  for (const [value, label] of opts) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = label;
    sizeSelect.appendChild(o);
  }
  if (opts.some(([v]) => v === keep)) sizeSelect.value = keep;
  sizeSelect.disabled = opts.length === 1; // locked when there is no choice
}

modelSelect.value = defaults.model;
if (settings.model && SIZES[settings.model]) modelSelect.value = settings.model;
modelSelect.addEventListener('change', () => {
  fillSizes();
  saveSetting('model', modelSelect.value);
  saveSetting('size', sizeSelect.value);
});
fillSizes();
if (settings.size && [...sizeSelect.options].some((o) => o.value === settings.size)) {
  sizeSelect.value = settings.size;
}
sizeSelect.addEventListener('change', () => saveSetting('size', sizeSelect.value));

// atlases[font][color] is a canvas with all 256 glyphs side by side
let atlases = null;
let ws = null;

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// buildAtlas renders one font of the font RAM in one colour: glyph c
// occupies x = c*16 .. c*16+15. Pixel row r of glyph c is the left byte at
// base+c*32+r and the right byte at base+0x2000+c*32+r, bit 7 leftmost.
function buildAtlas(font, base, rgb) {
  const a = document.createElement('canvas');
  a.width = 256 * GLYPH_W;
  a.height = FONT_ROWS;
  const g = a.getContext('2d');
  const img = g.createImageData(a.width, a.height);
  for (let c = 0; c < 256; c++) {
    for (let r = 0; r < FONT_ROWS; r++) {
      const bits = (font[base + c * 32 + r] << 8) | font[base + FONT_RIGHT + c * 32 + r];
      for (let x = 0; x < GLYPH_W; x++) {
        if (bits & (0x8000 >> x)) {
          const p = (r * a.width + c * GLYPH_W + x) * 4;
          img.data[p] = rgb[0];
          img.data[p + 1] = rgb[1];
          img.data[p + 2] = rgb[2];
          img.data[p + 3] = 255;
        }
      }
    }
  }
  g.putImageData(img, 0, 0);
  return a;
}

let lastFont = null; // the font RAM, kept to rebuild atlases on colour change

function buildAtlases(font) {
  lastFont = font;
  atlases = [0, FONT2].map((base) => ({
    normal: buildAtlas(font, base, COLORS.normal),
    high: buildAtlas(font, base, COLORS.high),
    black: buildAtlas(font, base, COLORS.black),
  }));
}

function rgbCSS(c) { return 'rgb(' + c[0] + ',' + c[1] + ',' + c[2] + ')'; }

// Attribute byte (cell byte 2). The firmware stores the 5250 attribute
// (0x20-0x3F) itself: its run-time table at SS:0B40 maps 5250 attribute
// 0x2n to cell bits n. The bits are not independent: like IBM's 5250
// attribute table (and tn5250), only these combinations blink or show
// column separators, and the colour attributes 0x38-0x3E (pink/blue)
// show neither. Index = attr & 0x1F:
//   h = high intensity, r = reverse, u = underline, b = blink,
//   c = column separators, n = non-display.
const MONO_ATTR = [
  '', 'r', 'h', 'hr', 'u', 'ur', 'hu', 'n',      // 20-27 green/white
  '', 'r', 'b', 'br', 'u', 'ur', 'ub', 'n',      // 28-2F red
  'c', 'cr', 'hc', 'hcr', 'cu', 'cur', 'hcu', 'n', // 30-37 turquoise/yellow
  '', 'r', '', 'r', 'u', 'ur', 'u', 'n',         // 38-3F pink/blue
];
// attrStyle returns the style of a cell. For host screens the firmware
// stores the 5250 attribute without bit 5 (0x2n -> n), for its own
// screens with it, so only the low 5 bits count. Column separators are
// not drawn: photos of a real 3477 show input fields underlined and no
// separator dots anywhere (e.g. on 0x30 output fields). The status row
// (last row) is never highlighted; there bit 0 draws the separator line
// along its top, as on the real terminal.
// The firmware stores the 5250 non-display attributes (0x27/2F/37/3F,
// password fields) not in the attribute byte but as bit 7 of the third
// cell byte (extra); characters typed into the field inherit it.
function attrStyle(attr, statusRow, extra) {
  if (statusRow) return '';
  if (extra & 0x80) return 'n';
  return MONO_ATTR[attr & 0x1F].replace('c', '');
}

let lastScreen = null;
let blinkOn = true;
let cursor = { row: 0, col: 0, on: false };

function drawScreen(rows, cols, cells) {
  lastScreen = { rows: rows, cols: cols, cells: cells };
  setCanvasSize(cols, rows);
  ctx.fillStyle = '#000';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  if (!atlases) return;
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      const i = (r * cols + c) * 3;
      const code = cells[i];
      const attr = cells[i + 1];
      const x = c * cellW;
      const y = r * cellH;
      const set = atlases[attr & 0x80 ? 1 : 0];
      const statusRow = r === rows - 1;
      const st = attrStyle(attr, statusRow, cells[i + 2]);
      if (st === 'n') continue; // non-display (e.g. password fields)
      const high = st.includes('h');
      const fg = high ? COLORS.high : COLORS.normal;
      const reverse = st.includes('r');
      if (reverse) {
        ctx.fillStyle = rgbCSS(fg);
        ctx.fillRect(x, y, cellW, cellH);
      }
      const hidden = st.includes('b') && !blinkOn;
      if (!hidden && code !== 0x00 && code !== 0x40) {
        const atlas = reverse ? set.black : (high ? set.high : set.normal);
        ctx.drawImage(atlas, code * GLYPH_W, 0, cellW, cellH, x, y, cellW, cellH);
      }
      ctx.fillStyle = rgbCSS(reverse ? COLORS.black : fg);
      if (st.includes('u')) ctx.fillRect(x, y + cellH - 2, cellW, 1); // underline

    }
  }
  // The 3477's video hardware draws a fixed line separating the status
  // row from the screen (it is not in video RAM: the status-row cells are
  // ordinary 0x20 cells in normal operation; photos of a real terminal
  // show the line on every screen).
  ctx.fillStyle = rgbCSS(COLORS.normal);
  ctx.fillRect(0, (rows - 1) * cellH, cols * cellW, 1);
  // mouse selection (copy): translucent highlight over the rectangle
  if (sel) {
    const b = selBounds();
    ctx.fillStyle = 'rgba(119,255,153,0.35)';
    ctx.fillRect(b.c0 * cellW, b.r0 * cellH, (b.c1 - b.c0 + 1) * cellW, (b.r1 - b.r0 + 1) * cellH);
  }
  // cursor: an underscore on the cursor cell, blinking
  if (cursor.on && blinkOn && cursor.row < rows && cursor.col < cols) {
    ctx.fillStyle = rgbCSS(COLORS.high);
    ctx.fillRect(cursor.col * cellW, cursor.row * cellH + cellH - 3, cellW, 2);
  }
}

// Blink phase for blinking attributes and the cursor.
setInterval(() => {
  blinkOn = !blinkOn;
  if (lastScreen) drawScreen(lastScreen.rows, lastScreen.cols, lastScreen.cells);
}, 500);

// fitCanvas shows the terminal with the proportions of its real screen:
// the InfoWindow has a 4:3 CRT, whatever its raster (80 or 132 columns,
// 24/27/43 rows), so the canvas is drawn into the largest 4:3 box that
// fits below the toolbar, growing and shrinking with the window. Pixels
// become non-square, as they were on the CRT.
const SCREEN_ASPECT = 4 / 3;
const wrap = document.getElementById('screen-wrap');
function fitCanvas() {
  const pad = 16;
  // measure the space left between toolbar and status bar, not the
  // canvas itself (which would grow the area it is measured in)
  const toolbarH = document.getElementById('toolbar').offsetHeight;
  const statusH = document.getElementById('statusbar').offsetHeight;
  const w = Math.max(0, wrap.clientWidth - pad);
  const h = Math.max(0, Math.min(wrap.clientHeight, window.innerHeight - toolbarH - statusH) - pad);
  const boxW = Math.min(w, h * SCREEN_ASPECT);
  canvas.style.width = Math.floor(boxW) + 'px';
  canvas.style.height = Math.floor(boxW / SCREEN_ASPECT) + 'px';
}
window.addEventListener('resize', fitCanvas);

function setCanvasSize(cols, rows) {
  if (canvas.width !== cols * cellW || canvas.height !== rows * cellH) {
    canvas.width = cols * cellW;
    canvas.height = rows * cellH;
    fitCanvas();
  }
}

// Before power-on show a blank 80x25 screen (24 rows + status row) at the
// terminal's real proportions, with a hint.
function drawPoweredOff() {
  setCanvasSize(80, 25);
  ctx.fillStyle = '#000';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.fillStyle = '#1a6b2a';
  ctx.font = '28px monospace';
  ctx.textAlign = 'center';
  ctx.fillText(poweredOn ? '' : 'Powered off', canvas.width / 2, canvas.height / 2);
}

function open() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(proto + '//' + location.host + '/ws');
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'font') buildAtlases(b64(m.font));
    else if (m.type === 'screen') {
      if (m.cellh) cellH = m.cellh;
      if (m.cellw) cellW = m.cellw;
      cursor = { row: m.currow, col: m.curcol, on: m.curon };
      if (m.lines) screenLines = m.lines;
      drawScreen(m.rows, m.cols, b64(m.cells));
      statusLED.textContent = 'LED ' + m.led.toString(16).toUpperCase().padStart(2, '0');
    } else if (m.type === 'sound') {
      playSound(m);
    } else if (m.type === 'traffic') {
      // bytes to / from the host, shown as nnnnKB TX / NNNNNKB RX
      const kb = (n) => Math.floor(n / 1024) + 'KB';
      document.getElementById('status-traffic').textContent =
        kb(m.tx) + ' TX / ' + kb(m.rx) + ' RX';
    } else if (m.type === 'status') {
      statusText.textContent = m.text;
      if (m.text.endsWith('powered on')) setPowered(true);
      if (m.text.startsWith('Connecting to')) {
        document.getElementById('status-traffic').textContent = '0KB TX / 0KB RX';
      }
      if (m.text.startsWith('Connected to')) setHostConnected(true);
      if (m.text.startsWith('Disconnected') || m.text.startsWith('Host session ended') ||
          m.text.startsWith('Cannot connect') || m.text === 'Powered off') {
        setHostConnected(false);
      }
    }
  };
  // The terminal starts powered off; the user switches it on.
  ws.onclose = () => { statusText.textContent = 'Disconnected from server'; };
}

function send(m) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m));
}

// Power switch. The terminal starts powered off; the button switches it
// on and off. The state follows the server's status messages.
const powerBtn = document.getElementById('btn-power');
let poweredOn = false;

function setPowered(on) {
  poweredOn = on;
  if (!on) toneOff();
  // SETUP (status bar) is only offered while the terminal is on
  document.getElementById('setup-link').style.display = on ? '' : 'none';
  powerBtn.innerHTML = on ? '&#x23FB; Power off' : '&#x23FB; Power on';
  powerBtn.className = on ? 'power-on' : 'power-off';
  if (!on) {
    lastScreen = null; // stop blink redraws of the old screen
    drawPoweredOff();
  }
}

function powerOn() {
  send({ type: 'power', model: modelSelect.value, codepage: codepageSelect.value });
  canvas.focus();
}

powerBtn.onclick = () => {
  if (poweredOn) {
    send({ type: 'poweroff' });
    setPowered(false);
  } else {
    powerOn();
  }
};
// Changing the model while on restarts the terminal as the new model.
modelSelect.addEventListener('change', () => { if (poweredOn) powerOn(); });

// Connect / Disconnect: the button follows the host session state, which
// the server reports in status messages.
const connectBtn = document.getElementById('btn-connect');
let hostConnected = false;
function setHostConnected(on) {
  hostConnected = on;
  connectBtn.textContent = on ? 'Disconnect' : 'Connect';
  connectBtn.className = on ? 'disconnect' : ''; // red while connected, as in web5250
}
connectBtn.onclick = () => {
  if (hostConnected) {
    send({ type: 'disconnect' });
    return;
  }
  if (!poweredOn) powerOn();
  // remember the last host used, even if typed without leaving the field
  // (not the host a locked server imposes)
  if (!defaults.locked) {
    saveSetting('host', hostInput.value.trim());
    saveSetting('port', portInput.value.trim());
  }
  send({
    type: 'connect',
    host: hostInput.value,
    port: portInput.value,
    size: sizeSelect.value,
    codepage: codepageSelect.value,
  });
};

// ── Copy and paste ──────────────────────────────────────────────────
//
// Drag with the mouse over the screen to select a rectangle; Cmd+C
// (macOS) or Ctrl+C copies its text. Cmd/Ctrl+V types the clipboard text
// through the emulated keyboard (the terminal and the 5250 controller
// apply their usual field rules); a plain click clears the selection.

const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent || '');
let sel = null;          // {a: {r, c}, b: {r, c}} in screen cells
let selecting = false;
let screenLines = [];    // displayed text per row, from the server

function selBounds() {
  return {
    r0: Math.min(sel.a.r, sel.b.r), r1: Math.max(sel.a.r, sel.b.r),
    c0: Math.min(sel.a.c, sel.b.c), c1: Math.max(sel.a.c, sel.b.c),
  };
}

// cellAt maps a mouse position to a screen cell (the canvas is scaled by
// CSS, so positions are converted through its on-screen size).
function cellAt(ev) {
  if (!lastScreen) return null;
  const rect = canvas.getBoundingClientRect();
  const c = Math.floor((ev.clientX - rect.left) / rect.width * lastScreen.cols);
  const r = Math.floor((ev.clientY - rect.top) / rect.height * lastScreen.rows);
  return {
    r: Math.max(0, Math.min(lastScreen.rows - 1, r)),
    c: Math.max(0, Math.min(lastScreen.cols - 1, c)),
  };
}

function redraw() {
  if (lastScreen) drawScreen(lastScreen.rows, lastScreen.cols, lastScreen.cells);
}

// downAt is where the mouse button went down, to tell a click (moves the
// cursor) from a drag (selects for copying): a hand that drifts a few
// pixels over a cell edge still clicks.
let downAt = null;
const CLICK_SLOP_PX = 5;

// The click that brings the window (or the screen) into focus only
// focuses: it must not move the cursor, or the cursor would land wherever
// the mouse happened to be (typing then fails with 0005). focusClick is
// set when the window loses the focus and cleared shortly after it comes
// back, so a window focused from the keyboard (Cmd+Tab) does not swallow
// a later, deliberate click.
let focusClick = !document.hasFocus();
let focusTimer = null;
window.addEventListener('blur', () => {
  clearTimeout(focusTimer); // a pending clear belongs to the old focus
  focusClick = true;
});
window.addEventListener('focus', () => {
  clearTimeout(focusTimer);
  focusTimer = setTimeout(() => { focusClick = false; }, 300);
});
let clickFocuses = false; // the current mousedown was a focus click

canvas.addEventListener('mousedown', (ev) => {
  if (ev.button !== 0) return;
  clickFocuses = focusClick || document.activeElement !== canvas;
  focusClick = false;
  const cell = cellAt(ev);
  if (!cell) return;
  downAt = { x: ev.clientX, y: ev.clientY };
  sel = { a: cell, b: cell };
  selecting = true;
  canvas.focus();
  redraw();
});
canvas.addEventListener('mousemove', (ev) => {
  if (!selecting) return;
  const cell = cellAt(ev);
  if (cell) {
    sel.b = cell;
    redraw();
  }
});
window.addEventListener('mouseup', (ev) => {
  if (!selecting) return;
  selecting = false;
  const still = downAt && Math.hypot(ev.clientX - downAt.x, ev.clientY - downAt.y) < CLICK_SLOP_PX;
  if (sel && (still || (sel.a.r === sel.b.r && sel.a.c === sel.b.c))) {
    // a click (no drag): move the cursor there, except on the status
    // row and for the click that only focused the screen
    if (hostConnected && lastScreen && sel.a.r < lastScreen.rows - 1 && !clickFocuses) {
      send({ type: 'cursor', row: sel.a.r, col: sel.a.c });
    }
    sel = null;
  }
  redraw();
});

function selectionText() {
  const b = selBounds();
  const out = [];
  for (let r = b.r0; r <= b.r1; r++) {
    const line = screenLines[r] || '';
    out.push(line.slice(b.c0, b.c1 + 1).replace(/\s+$/, ''));
  }
  return out.join('\n');
}

function copySelection() {
  const text = selectionText();
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).catch(() => copyFallback(text));
  } else {
    copyFallback(text);
  }
  statusText.textContent = 'Copied ' + text.length + ' characters';
}

// copyFallback copies through a temporary text area, for pages served
// over plain HTTP where the clipboard API is not available.
function copyFallback(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (e) { /* nothing more to try */ }
  document.body.removeChild(ta);
  canvas.focus();
}

function pasteText(text) {
  if (!text || !poweredOn) return;
  send({ type: 'paste', text: text });
  statusText.textContent = 'Pasting ' + text.length + ' characters';
}

// Paste without asking for clipboard permission: reading the clipboard
// through navigator.clipboard makes Chrome/Brave show a "Share
// clipboard?" prompt. Instead, Cmd/Ctrl+V moves the focus to an invisible
// text box; the browser pastes into it as for any text field (no
// prompt), the paste event hands over the text, and the focus returns to
// the screen.
const pasteSink = document.createElement('textarea');
pasteSink.style.position = 'fixed';
pasteSink.style.left = '-10000px';
pasteSink.style.top = '0';
pasteSink.style.opacity = '0';
pasteSink.tabIndex = -1;
document.body.appendChild(pasteSink);

function armPaste() {
  pasteSink.value = '';
  pasteSink.focus();
}

pasteSink.addEventListener('paste', (ev) => {
  ev.preventDefault();
  // the focus moves back to the screen below, so the document handler
  // would see the screen focused and paste the text a second time
  ev.stopPropagation();
  pasteText(ev.clipboardData ? ev.clipboardData.getData('text/plain') : '');
  canvas.focus();
});
// back to the screen also when nothing was pasted (empty clipboard)
pasteSink.addEventListener('keyup', () => canvas.focus());

// a browser that delivers the paste event to the focused screen itself
document.addEventListener('paste', (ev) => {
  if (document.activeElement !== canvas) return; // e.g. the host field
  ev.preventDefault();
  pasteText(ev.clipboardData ? ev.clipboardData.getData('text/plain') : '');
});

// ── Keyboard mapping (ported from web5250) ─────────────────────────
//
// A PC key combination can be mapped to a function of the terminal's IBM
// 122-key keyboard (Reset, PF13, Field Exit, ...); pressing it presses
// that terminal key. Every other key types as the key in the same
// position. Ctrl, Alt and Meta are only shortcut modifiers here: on the
// 122-key keyboard the key in the PC's left-Ctrl position is Reset.

const DEFAULT_KEYMAP = {
  'Enter': 'Enter',
  'F1': 'PF1', 'F2': 'PF2', 'F3': 'PF3', 'F4': 'PF4', 'F5': 'PF5',
  'F6': 'PF6', 'F7': 'PF7', 'F8': 'PF8', 'F9': 'PF9', 'F10': 'PF10',
  'F11': 'PF11', 'F12': 'PF12',
  'Shift+F1': 'PF13', 'Shift+F2': 'PF14', 'Shift+F3': 'PF15',
  'Shift+F4': 'PF16', 'Shift+F5': 'PF17', 'Shift+F6': 'PF18',
  'Shift+F7': 'PF19', 'Shift+F8': 'PF20', 'Shift+F9': 'PF21',
  'Shift+F10': 'PF22', 'Shift+F11': 'PF23', 'Shift+F12': 'PF24',
  'PageUp': 'RollDown',
  'PageDown': 'RollUp',
  'Escape': 'Reset',
  'Pause': 'Attn',
  'Shift+Escape': 'Clear',
  'Ctrl+p': 'Print',
  'Ctrl+s': 'SysReq',
  'Ctrl+h': 'Help',
  'Ctrl+d': 'Dup',
  'Ctrl+e': 'EraseEOF',
  'End': 'FieldExit',
  'Shift+End': 'FieldPlus',
  'Ctrl+End': 'FieldMinus',
  'RightAlt': 'Enter',
  'Home': 'Home',
  'Insert': 'Insert',
  'Delete': 'Delete',
  'Shift+Tab': 'BackTab',
  'Ctrl+Backspace': 'ErInp',
};

// Every key of the 5250 keyboard that can be mapped: web5250's list plus
// the 122-key keys a 3477 keyboard has that web5250 does not map.
const ALL_FUNCTIONS = [
  'Enter', 'NewLine',
  'PF1', 'PF2', 'PF3', 'PF4', 'PF5', 'PF6', 'PF7', 'PF8', 'PF9', 'PF10', 'PF11', 'PF12',
  'PF13', 'PF14', 'PF15', 'PF16', 'PF17', 'PF18', 'PF19', 'PF20', 'PF21', 'PF22', 'PF23', 'PF24',
  'Help', 'RollUp', 'RollDown', 'Print', 'SysReq', 'TestReq', 'Attn',
  'Clear', 'FieldExit', 'FieldPlus', 'FieldMinus', 'Dup',
  'Reset', 'EraseEOF', 'ErInp', 'Home', 'Insert', 'Delete', 'BackTab',
  'CrSel', 'Hex', 'FieldMark', 'Rule', 'Jump', 'Setup', 'Record', 'Play',
];

// Keys whose position on the terminal keyboard is not confirmed yet; the
// emulator ignores them until the firmware's keyboard tables are decoded.
const NOT_YET_KNOWN = new Set(['Hex', 'FieldMark', 'Rule', 'Jump',
  'Record', 'Play', 'CrSel']);

const FUNCTION_DESCRIPTIONS = {
  'Enter': 'Enter / send the screen',
  'NewLine': 'New Line - first field on the next line',
  'Help': 'Help',
  'RollUp': 'Page Down - show more (the "More..." prompt)',
  'RollDown': 'Page Up - show the previous page',
  'Print': 'Print the screen',
  'SysReq': 'System Request',
  'TestReq': 'Test Request',
  'Attn': 'Attention',
  'Clear': 'Clear the screen',
  'FieldExit': 'Field Exit - leave field, blank to its end',
  'FieldPlus': 'Field+ - exit a numeric field',
  'FieldMinus': 'Field- - sign a numeric field negative',
  'Dup': 'Duplicate field',
  'Reset': 'Error Reset - unlock the keyboard (clears X II)',
  'EraseEOF': 'Erase to end of field',
  'ErInp': 'Erase Input - clear all input fields',
  'Home': 'Home - first input field on the screen',
  'Insert': 'Toggle Insert mode',
  'Delete': 'Delete character',
  'BackTab': 'Back Tab - previous input field',
  'CrSel': 'Cursor Select (light-pen style selection)',
  'Hex': 'Hex - enter a character by its hex code',
  'FieldMark': 'Field Mark',
  'Rule': 'Rule - show the ruler line (terminal local)',
  'Jump': 'Jump - switch display session (terminal local)',
  'Setup': 'Setup - the terminal setup menu (also: click SETUP below the screen)',
  'Record': 'Record keystrokes (terminal local)',
  'Play': 'Play back recorded keystrokes (terminal local)',
};

const KEYMAP_STORE = 'webibm3477-keymap';
let keymap = {};

function loadKeymap() {
  try {
    const saved = localStorage.getItem(KEYMAP_STORE);
    keymap = saved ? JSON.parse(saved) : Object.assign({}, DEFAULT_KEYMAP);
  } catch (e) {
    keymap = Object.assign({}, DEFAULT_KEYMAP);
  }
}

function saveKeymap() {
  try { localStorage.setItem(KEYMAP_STORE, JSON.stringify(keymap)); } catch (e) { /* private mode */ }
}

// getKeyId names a key combination like web5250 does ("Ctrl+Shift+F3").
function getKeyId(e) {
  if (e.code === 'AltRight') return 'RightAlt';
  if (e.code === 'MetaRight') return 'RightMeta';
  if (['Control', 'Shift', 'Alt', 'Meta'].includes(e.key)) return null;
  const parts = [];
  if (e.ctrlKey) parts.push('Ctrl');
  if (e.shiftKey) parts.push('Shift');
  if (e.altKey) parts.push('Alt');
  if (e.metaKey) parts.push('Meta');
  let keyName = e.key;
  if (/^F([1-9]|1[0-9]|2[0-4])$/.test(e.code)) keyName = e.code;
  parts.push(keyName);
  return parts.join('+');
}

// Physical Shift keys that are currently down on the terminal keyboard;
// a mapped function is sent without them (Shift+F1 is PF13, not
// Shift+PF1), and they are pressed again afterwards.
const heldShift = new Set();
// Function currently held down for each PC key (e.code), for the release.
const activeFn = {};

function sendKey(code, down) { send({ type: 'key', code: code, down: down }); }

function onKey(ev, down) {
  if (ev.code === 'F5' && ev.ctrlKey) return; // keep a browser reload key
  // Copy and paste use the platform's usual shortcut: Cmd on macOS,
  // Ctrl on Linux and Windows.
  const mod = IS_MAC ? ev.metaKey : ev.ctrlKey;
  const k = (ev.key || '').toLowerCase();
  if (mod && k === 'c' && sel) {
    ev.preventDefault();
    if (down) copySelection();
    return;
  }
  if (mod && k === 'v') {
    // no preventDefault: the browser's own paste goes to the paste box
    if (down) armPaste();
    return;
  }
  ev.preventDefault();
  if (down) {
    const fn = keymap[getKeyId(ev)];
    if (fn) {
      for (const s of heldShift) sendKey(s, false);
      activeFn[ev.code] = fn;
      // Auto-repeat arrives as repeated make codes, as from a typematic
      // keyboard
      sendKey('fn:' + fn, true);
      return;
    }
  } else if (activeFn[ev.code]) {
    sendKey('fn:' + activeFn[ev.code], false);
    delete activeFn[ev.code];
    for (const s of heldShift) sendKey(s, true);
    return;
  }
  if (ev.ctrlKey || ev.altKey || ev.metaKey) return; // unmapped shortcut
  if (ev.code === 'CapsLock' && IS_MAC) {
    // macOS reports Caps Lock as keydown when it turns on and keyup when
    // it turns off; the terminal toggles on each press, so every event
    // is one press and release of the key
    sendKey('CapsLock', true);
    setTimeout(() => sendKey('CapsLock', false), 50);
    return;
  }
  if (ev.code === 'ShiftLeft' || ev.code === 'ShiftRight') {
    if (down) heldShift.add(ev.code); else heldShift.delete(ev.code);
  }
  if (down) heldKeys.add(ev.code); else heldKeys.delete(ev.code);
  sendKey(ev.code, down);
}

// Keys still down when the screen loses the focus (switching windows)
// never get their keyup: release them, or a non-repeating key (Enter,
// Field Exit, Reset) would stay "held" and ignore every later press.
const heldKeys = new Set();
function releaseAll() {
  for (const code of Object.keys(activeFn)) {
    sendKey('fn:' + activeFn[code], false);
    delete activeFn[code];
  }
  for (const code of heldKeys) sendKey(code, false);
  heldKeys.clear();
  heldShift.clear();
}
canvas.addEventListener('blur', releaseAll);
window.addEventListener('blur', releaseAll);
canvas.addEventListener('keydown', (ev) => { if (!listeningFor) onKey(ev, true); });
canvas.addEventListener('keyup', (ev) => { if (!listeningFor) onKey(ev, false); });
canvas.addEventListener('click', () => canvas.focus());

// ── Settings dialog ────────────────────────────────────────────────

let listeningFor = null; // function whose key is being captured
const overlay = document.getElementById('modal-overlay');

function keyFor(fn) {
  for (const [k, v] of Object.entries(keymap)) if (v === fn) return k;
  return '(unassigned)';
}

function openSettings() {
  const list = document.getElementById('keymap-list');
  list.innerHTML = '';
  for (const fn of ALL_FUNCTIONS) {
    const row = document.createElement('div');
    row.className = 'keymap-row';
    const label = document.createElement('span');
    label.className = 'keymap-fn';
    const nameEl = document.createElement('span');
    nameEl.textContent = fn;
    label.appendChild(nameEl);
    let desc = FUNCTION_DESCRIPTIONS[fn] || '';
    if (NOT_YET_KNOWN.has(fn)) desc += ' (terminal key not identified yet)';
    if (desc) {
      const d = document.createElement('span');
      d.className = 'keymap-fn-desc';
      d.textContent = desc;
      label.appendChild(d);
    }
    const keyBtn = document.createElement('span');
    keyBtn.className = 'keymap-key';
    keyBtn.textContent = keyFor(fn);
    keyBtn.dataset.fn = fn;
    const cancelBtn = document.createElement('button');
    cancelBtn.className = 'keymap-cancel';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.style.display = 'none';
    cancelBtn.onclick = (evt) => {
      evt.stopPropagation();
      keyBtn.className = 'keymap-key';
      keyBtn.textContent = keyFor(fn);
      cancelBtn.style.display = 'none';
      listeningFor = null;
    };
    keyBtn.onclick = () => {
      list.querySelectorAll('.listening').forEach((p) => {
        p.className = 'keymap-key';
        p.textContent = keyFor(p.dataset.fn);
      });
      list.querySelectorAll('.keymap-cancel').forEach((b) => { b.style.display = 'none'; });
      listeningFor = fn;
      keyBtn.className = 'keymap-key listening';
      keyBtn.textContent = 'Press a key...';
      cancelBtn.style.display = 'inline-block';
    };
    row.appendChild(label);
    row.appendChild(cancelBtn);
    row.appendChild(keyBtn);
    list.appendChild(row);
  }
  overlay.className = 'visible';
}

function closeSettings() {
  overlay.className = '';
  listeningFor = null;
  canvas.focus();
}

document.getElementById('btn-settings').onclick = openSettings;
document.getElementById('btn-settings-close').onclick = closeSettings;
document.getElementById('btn-settings-done').onclick = closeSettings;
document.getElementById('btn-keymap-reset').onclick = () => {
  keymap = Object.assign({}, DEFAULT_KEYMAP);
  saveKeymap();
  openSettings();
};

// While the dialog is open: capture a key for the selected function, or
// close the dialog with Escape when nothing is being captured.
document.addEventListener('keydown', (e) => {
  if (!overlay.classList.contains('visible')) return;
  if (e.key === 'Escape' && !listeningFor) {
    e.preventDefault();
    closeSettings();
    return;
  }
  if (!listeningFor) return;
  e.preventDefault();
  e.stopPropagation();
  const keyId = getKeyId(e);
  if (!keyId) return;
  for (const [k, v] of Object.entries(keymap)) if (v === listeningFor) delete keymap[k];
  keymap[keyId] = listeningFor;
  saveKeymap();
  const btn = document.querySelector('[data-fn="' + listeningFor + '"]');
  if (btn) {
    btn.className = 'keymap-key';
    btn.textContent = keyId;
  }
  document.querySelectorAll('.keymap-cancel').forEach((b) => { b.style.display = 'none'; });
  listeningFor = null;
}, true);

loadKeymap();

// SETUP in the status bar presses the terminal's Setup key, so users do
// not need to know the key combination; the firmware then opens Setup.
document.getElementById('setup-link').onclick = (ev) => {
  ev.preventDefault();
  if (!poweredOn) return; // hidden while off; ignore a stray click
  // Hold the key about as long as a finger would: the firmware does not
  // take a press and release that arrive at the same instant as the
  // Setup key (its own test holds the key ~130 ms).
  sendKey('fn:Setup', true);
  setTimeout(() => sendKey('fn:Setup', false), 150);
  canvas.focus();
};

// Host codepage (EBCDIC <-> characters), kept in the browser like web5250.
const codepageSelect = document.getElementById('codepage-select');
try {
  const cp = localStorage.getItem('webibm3477-codepage');
  if (cp) codepageSelect.value = cp;
} catch (e) { /* private mode */ }
codepageSelect.addEventListener('change', () => {
  try { localStorage.setItem('webibm3477-codepage', codepageSelect.value); } catch (e) { /* private mode */ }
});

// Text colour (Settings > Display): IBM green, amber or white.
const colorSelect = document.getElementById('color-select');
function applyColor(name) {
  const p = PHOSPHORS[name] || PHOSPHORS.green;
  COLORS.normal = p.normal;
  COLORS.high = p.high;
  if (lastFont) buildAtlases(lastFont);
  if (lastScreen) drawScreen(lastScreen.rows, lastScreen.cols, lastScreen.cells);
  else if (!poweredOn) drawPoweredOff();
}
if (settings.color && PHOSPHORS[settings.color]) colorSelect.value = settings.color;
applyColor(colorSelect.value);
colorSelect.addEventListener('change', () => {
  saveSetting('color', colorSelect.value);
  applyColor(colorSelect.value);
});

// ── Sound ───────────────────────────────────────────────────────────
//
// The terminal's speaker, as the firmware drives it (server "sound"
// messages): a tone switched on and off (the alarm: ~0.8 s for the host,
// ~0.2 s for local errors, a short beep at power-on) and a click per key.
// Volume 0-31 comes from the terminal's own Setup choices. The pitch is
// not known from the hardware; 1 kHz is a guess. Browsers allow audio
// only after a user gesture, so the AudioContext starts on the first key
// press or click.
let audio = null;
let tone = null;          // {osc, gain} while a tone sounds
let lastClicks = 0;
const soundSelect = document.getElementById('sound-select');
if (settings.sound === 'off' || settings.sound === 'on') soundSelect.value = settings.sound;
soundSelect.addEventListener('change', () => {
  saveSetting('sound', soundSelect.value);
  if (soundSelect.value === 'off') toneOff();
});
function soundOn() { return soundSelect.value !== 'off'; }
// "Mute keyboard click" silences only the clicks; the alarm still sounds
const muteClick = document.getElementById('mute-click');
muteClick.checked = settings.muteClick === true;
muteClick.addEventListener('change', () => saveSetting('muteClick', muteClick.checked));

function audioReady() {
  if (!audio) {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return null;
    audio = new AC();
  }
  if (audio.state === 'suspended') audio.resume();
  return audio;
}
for (const ev of ['keydown', 'mousedown']) {
  window.addEventListener(ev, () => {
    if (!soundOn()) return;
    audioReady();
    if (clickStyle.value === 'realistic') loadRecordedClicks();
  }, true);
}

function gainFor(volume) { return Math.max(0, Math.min(31, volume)) / 31 * 0.25; }

function toneOn(volume) {
  const a = audioReady();
  if (!a || tone) return;
  const osc = a.createOscillator();
  const gain = a.createGain();
  osc.type = 'square';
  osc.frequency.value = 1000;
  gain.gain.value = gainFor(volume) * 0.5; // square waves sound loud
  osc.connect(gain).connect(a.destination);
  osc.start();
  tone = { osc: osc, gain: gain };
}

function toneOff() {
  if (!tone) return;
  // a 5 ms fade avoids a pop at the end of the tone
  const t = audio.currentTime;
  tone.gain.gain.setTargetAtTime(0, t, 0.005);
  tone.osc.stop(t + 0.03);
  tone = null;
}

// Realistic key click (Settings > Keyboard click): four keystrokes of a
// real IBM 3477 with its 122-key keyboard, cut from a video
// (leela.pocnet.net/~poc/3477.mov, 0:34.99, 0:35.56, 0:36.15, 0:44.42).
// Each is the buckling-spring clack alone. A narrow 3,520 Hz tone heard
// on only a third of the presses in the video (not part of the key
// press), and the room's steady tones at 2.5 and 5.6 kHz, were notched
// out. Also high-passed at 80 Hz, low-passed at 6 kHz, gated, faded,
// peak -3 dBFS. Each key plays one of them at random, as real
// keystrokes never sound alike.
const clickStyle = document.getElementById('click-style');
if (settings.clickStyle === 'synthetic' || settings.clickStyle === 'realistic') {
  clickStyle.value = settings.clickStyle;
}
clickStyle.addEventListener('change', () => {
  saveSetting('clickStyle', clickStyle.value);
  if (clickStyle.value === 'realistic') loadRecordedClicks();
});
let recordedClicks = null; // AudioBuffers, once loaded and decoded
let recordedLoading = false;
function loadRecordedClicks() {
  const a = audioReady();
  if (!a || recordedClicks || recordedLoading) return;
  recordedLoading = true;
  const files = [1, 2, 3, 4].map((i) => '/static/sounds/key-click-' + i + '.wav');
  Promise.all(files.map((f) => fetch(f)
    .then((r) => r.arrayBuffer())
    .then((b) => new Promise((ok, bad) => a.decodeAudioData(b, ok, bad)))))
    .then((bufs) => { recordedClicks = bufs; })
    .catch(() => { recordedLoading = false; }); // try again next time
}

function recordedClick(volume) {
  const a = audioReady();
  if (!a) return false;
  if (!recordedClicks) { loadRecordedClicks(); return false; }
  const src = a.createBufferSource();
  src.buffer = recordedClicks[Math.floor(Math.random() * recordedClicks.length)];
  const gain = a.createGain();
  // the recordings peak at -3 dBFS; 1.25 is half the first level (2.5),
  // lowered on request
  gain.gain.value = gainFor(volume) * 1.25;
  src.connect(gain).connect(a.destination);
  src.start();
  return true;
}

// click plays one key click: the recorded one when chosen (and loaded),
// else the synthetic one. A short 3 kHz square-wave burst (CLICK_HZ:
// ~6 kHz first, halved on request) excites a "hollow" body: two resonant
// band-pass filters (an octave below the click, and at the click pitch)
// that ring for a few milliseconds, and a low-pass that removes the
// harsh square-wave overtones. The burst alone sounded thin and bright.
const CLICK_HZ = 3000;      // pitch of the exciting burst
const CLICK_BODY_HZ = 1500; // main resonance (the "hollow" body)
const CLICK_BODY_Q = 9;     // higher Q = longer, more hollow ring
const CLICK_LOWPASS_HZ = 3500;
function click(volume) {
  if (clickStyle.value === 'realistic' && recordedClick(volume)) return;
  const a = audioReady();
  if (!a) return;
  // 15 ms buffer: 4 ms of burst, then silence for the resonances to ring
  const n = Math.floor(a.sampleRate * 0.015);
  const burst = Math.floor(a.sampleRate * 0.004);
  const buf = a.createBuffer(1, n, a.sampleRate);
  const d = buf.getChannelData(0);
  const period = a.sampleRate / CLICK_HZ; // samples per square-wave period
  for (let i = 0; i < burst; i++) {
    d[i] = ((i % period) < period / 2 ? 1 : -1) * Math.exp(-i / (burst / 4));
  }
  const src = a.createBufferSource();
  src.buffer = buf;
  const body = a.createBiquadFilter();  // the hollow resonance
  body.type = 'bandpass';
  body.frequency.value = CLICK_BODY_HZ;
  body.Q.value = CLICK_BODY_Q;
  const top = a.createBiquadFilter();   // a little of the click pitch
  top.type = 'bandpass';
  top.frequency.value = CLICK_HZ;
  top.Q.value = 5;
  const topGain = a.createGain();
  topGain.gain.value = 0.35;
  const lp = a.createBiquadFilter();
  lp.type = 'lowpass';
  lp.frequency.value = CLICK_LOWPASS_HZ;
  const out = a.createGain();
  // band-pass filters lose level; make up for it
  out.gain.value = gainFor(volume) * 3;
  src.connect(body).connect(lp);
  src.connect(top).connect(topGain).connect(lp);
  lp.connect(out).connect(a.destination);
  src.start();
}

function playSound(m) {
  const clicks = m.clicks || 0;
  // a new terminal (power cycle) counts from 0 again
  const newClicks = clicks >= lastClicks ? clicks - lastClicks : clicks;
  lastClicks = clicks;
  if (!soundOn()) { toneOff(); return; }
  if (!muteClick.checked) {
    for (let i = 0; i < Math.min(newClicks, 3); i++) click(m.volume || 0);
  }
  if (m.tone) toneOn(m.volume || 0); else toneOff();
}

// Toolbar buttons must not keep the keyboard focus: Space or Enter typed
// for the terminal would press them again (e.g. Disconnect).
for (const b of document.querySelectorAll('#toolbar button')) {
  b.addEventListener('click', () => canvas.focus());
}

// Version label at the top right of the screen
const versionLabel = document.getElementById('version-label');
if (versionLabel && defaults.version) versionLabel.textContent = 'webibm3477 v' + defaults.version;

drawPoweredOff();
open();

