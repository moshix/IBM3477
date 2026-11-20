# IBM 3477: where the Setup configuration lives, and how Setup is entered

Confidence levels: **confirmed** means seen in the code and reproduced in
the emulator. **likely** means strong code evidence. **guess** means
plausible but not verified.

## 1. The result: battery-backed RAM 0x001A0-0x00A9F, not the I2C bus

The lead (the I2C routine at F000:0EE8) is real, but the 3477 does not
use it. The I2C bus is described in section 6. The Setup configuration
is stored in **ordinary RAM that survives power-off**: physical
`0x001A0`-`0x00A9F`, which is `001A:0000` plus 0x900 bytes. Confirmed by
the following evidence:

- **The RAM test skips it.** POST test 1 (`F000:1C1C`, table `F000:1C24`)
  tests `0000:0000` for 0xD0 words (`0x000`-`0x19F`), then `0000:0B00`,
  `0000:8000` and `1000:0000`. The gap `0x1A0`-`0xAFF` is never
  destructively tested. `0xAA0`-`0xAFF` (POST state) is tested at reset,
  before anything is stored there.
- **POST test 6 (`F000:2DFE`) tests it non-destructively.** It walks
  words `0x1A0`-`0xA9F`. For each word it saves the address at `0xA20`
  and the value at `0xA22`, writes `A55A` and `5AA5`, then restores the
  value. It writes the marker `ABCD` to `0xA20` when it finishes. If
  power fails in the middle, the next power-on restores the word
  (`F000:2E29`-`2E3B`). This only makes sense for memory that keeps its
  contents across power-off.
- **The firmware treats the data as persistent.**
  - `F000:01D1`-`01EB` increments a 32-bit power-on counter at
    `0xA90`/`0xA92`. In the emulator it counts 1, 2, … across saved and
    reloaded images.
  - The POST error log at `0xA20` (`00A2:0000`) is also in this range.
- **No other storage is read.** During boot no unmapped or unmodelled
  address is accessed (probe trace). The run time never reads
  `0x2012F` bits 0-1. The only `0x2F` accesses in C000/D000/E000 test
  bit 3 (vsync) or write the video control byte (`C000:0C11`).

## 2. Validation: POST test 7 (`F000:2EFE`), confirmed

Pointers `cs:[0x2FD6]` = `001A:0000` (config) and `cs:[0x2FDE]` =
`00AA:0000` (result flags at `0xAA0`).

| Check | Failure bit in `0xAA0` |
|---|---|
| `sum(0x1A6..0x1DD)` (`F000:14B2`, 0x38 bytes) `+ [0x1DE] == 0xEA`; on failure `[0x1DC] = 1` as well | `0x80` |
| `sum(0x1F6..0x1FE)` (`F000:147E`, 9 bytes, serial number) `+ [0x1FF] == 0xEA` | `0x40` |
| `[0x1A0]+[0x1A1] == 0xEA` and `[0x1A0]&0x7F <= 6` (display address 1) | `0x04` |
| `[0x1A2]+[0x1A3] == 0xEA` and `<= 6` (display address 2) | `0x02` |
| `[0x1A4]+[0x1A5] == 0xEA` and `<= 6` (printer address) | `0x01` |

All sums are mod 256.

**How the run time decides to enter Setup (`C000:08D4`), confirmed.**
`es:bx` = `00AA:0000`. The firmware enters **Offline Setup** if any of
the following holds:

- `[0xAA0] & 0xE4` is non-zero;
- `ss:[0x389] == 1` and `[0xAA0] & 2`;
- `ss:[0x389] == 2` and `[0xAA0] & 1`;
- `[0xAA2] & 0x40` is set (the power-on key; see section 4).

Otherwise it skips Setup and goes to `C000:097D`.

When it enters Setup it calls `es:[si+0x1E0]` → `D000:267A` (UI state
`ss:[0x28E]` = 0x14, then 0x15). State 0x15 calls `es:[bx+0x3C4]`
(`D000:17F1`), which writes the default selections (FF = "none" for
unset address pairs) and prints message 6, "Selections have been reset
to the IBM default values." (written to the screen at `F000:8C43`).
With all-zero RAM, all five checks fail.

## 3. Data format, confirmed by diffing images written by the firmware

Choices are stored as **index + 1** (1 = the first choice on the
screen). FF means "nothing selected".

| Addr | Meaning | Default image |
|---|---|---|
| 1A0/1A1 | Display Address 1, check byte (pair sum EA) | 00 EA |
| 1A2/1A3 | Display Address 2, check byte | 00 EA |
| 1A4/1A5 | Printer Address, check byte | 00 EA |
| 1A6 | Terminal Mode (1 = One-Display, 2 = Two-Display, 3 = Display-Printer) | 01 |
| 1A7 / 1A8 | Display Address 1 / 2, index form | 01 01 |
| 1A9 | Character Set (1 = Country Specific) | 01 |
| 1AA | Printer Address, index form | 01 |
| 1AB | Printer character set (1 = PC; the IBM default is 03 = Multilingual1) | 01 |
| 1AD | Printer Emulation (1 = 5256; the IBM default is 02 = 4214) | 01 |
| 1B6-1BE | Serial number in index form (0-9 = 01-0A, A-Z = 0B-24) | 08 09 02 … 08 |
| other bytes of 1A6-1DD | Customization page 2 (keyboard type/ID) and Online Setup choices (alarm, clicker, cursor, row/column indicator, rule line, …), all IBM defaults | see image |
| 1DE | Check byte for block 1A6-1DD | 6C |
| 1F6-1FE | Machine serial number in EBCDIC: plant (2) + serial (7) | F7 F8 F1 F2 F3 F4 F5 F6 F7 |
| 1FF | Check byte for 1F6-1FE | 4F |

The individual bytes inside 1AE-1DD are not mapped beyond this table
(likely).

**The serial number is "78-1234567"**: plant of manufacture "78", serial
"1234567". The Update serial number screen accepts 0-9 and A-Z in each
of the 9 positions. It is stored twice, in index form inside the checked
block and in EBCDIC with its own check byte (`F000:147E`).

### Default image, 0x1A0-0x1FF (the rest of the 0x900-byte range is zero)

```
1A0: 00 EA 00 EA 00 EA 01 01 01 01 01 01 FF 01 01 FF
1B0: FF FF FF FF FF FF 08 09 02 03 04 05 06 07 08 04
1C0: 01 04 01 02 01 01 02 01 01 02 01 FF 02 03 04 FF
1D0: 01 01 01 01 02 01 01 01 01 01 01 01 01 20 6C 00
1E0: 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
1F0: 00 00 00 00 00 00 F7 F8 F1 F2 F3 F4 F5 F6 F7 4F
```

The rest of the 2304-byte image is zero. The emulator builds it from
`board.Default3477Setup` and writes it to `~/.config/ibm3477/nvram.bin`
on first start.

**How it was made.** The firmware wrote it itself, from all-zero RAM,
with these keystrokes in `cmd/nvdrive` syntax. `g=R,C` presses Down
until the Setup cursor is at that screen position; `t=` taps keys (make,
then `F0` break).

```
w=150 t=58 w=10                          Enter: Customize workstation
g=5,62 t=29   Display-Printer            (unlocks printer fields)
g=7,38 t=29   Display Address 1 = 1      (temporarily: addresses must differ)
g=13,30 t=29  Printer Address = 0
g=15,30 t=29  Printer character set = PC
g=18,30 t=29  Printer Emulation = 5256
g=5,46 t=29   Two-Display
g=9,30 t=29   Display Address 2 = 0
g=5,30 t=29   One-Display
g=7,30 t=29   Display Address 1 = 0
t=17 w=20                                F3 = Save and Exit
t=60 t=58 w=10                           Update serial number
(then Right/Left x n + Space on each of the 9 rows: 7,8,1,2,3,4,5,6,7)
t=17 w=20                                F3 = Save and Exit
```

Page 2 of Customization was left alone, so it keeps the IBM defaults. In
One-Display mode the UI skips Display Address 2, Printer Address and
Printer Emulation. Two sessions may not share an address, so the
"all addresses 0" combination can only be stored by switching modes as
above. The firmware still stored it, and POST accepts it.

**Result (confirmed).** POST leaves `0xAA0 = 00`. Power-on goes straight
to the normal screen: no Setup, no "Selections have been reset". The
image is unchanged after boot. On the next power-on the counter at
`0xA90` is 2.

## 4. Entering Setup

- **At run time: set-3 key `01`, alone.** Make `01`, break `F0 01`.
  - It opens **Online Setup**: "Setup display / Browse display
    information / Setup printer / Browse printer information". The
    status row shows `SETUP`. F3 exits.
  - Holding Shift (12/59), Ctrl (11) or Caps (14) makes no difference.
    With Alt (19 or 39) held, it does not open Setup.
  - A brute-force scan of keys 01-8F, with no modifier and with each of
    those modifiers, found only `01`. **Confirmed** in the emulator.
  - Which physical key sends `01` is a **guess**: on the 122-key
    terminal keyboard it is a left-block key (bottom-left). The
    project's `kbdlayout` (enhanced 5250 layout) has no key for `01`, so
    the app needs a binding for it.
- **At power-on: hold any key during POST**, until about 24M cycles
  (≈3.2 s). Confirmed.
  - `F000:01A0` calls `F000:0BB9`, a keyboard receive with a short
    timeout. If a byte arrives, it sets `[0xAA2] |= 0x40`
    (`F000:01BB`); otherwise it clears the bit (`F000:01C7`).
  - `C000:0901` then opens **Offline Setup** (Customize workstation /
    Update serial number / Test workstation), keeping the stored
    values.

## 5. Video range engine (needed for the normal screen), likely

After a valid configuration, the run time hung at `C000:9D1A`, waiting
for `ss:[0x563]` bit 1 to clear.

- `C000:5717` writes a range to `0x2013B/3A` (end) and `0x20139/38`
  (start, in 4-byte units of video RAM). Writing `0x20138` starts the
  engine. It then sets the busy bit.
- The video interrupt, **vector 0Ch** (`C000:5702`), clears the busy bit
  and acknowledges by writing `0x20136 = FF`.

It is now modelled as follows: completion at the next vsync raises
vector 0Ch, and a write to `0x20136` clears it. What the engine does to
the data is unknown, so only the completion is modelled (guess). The
3486 equivalent is `E000:0000`.

## 6. The I2C bus on 0x2012F (the original lead), confirmed protocol

- **Lines:** bit 0 = SDA, bit 1 = SCL, both inverted (1 = pull low;
  reading 1 = line low).
  - `0x18` = idle.
  - `0x19` = START (`F000:0F4A`).
  - `0x1B` = both low.
  - `0x1A` = SCL low, SDA released.
- **Primitives:**
  - `120C` sends a byte, MSB first, and samples the ACK: `0xAA` = ACK,
    0 = NAK, 1 = arbitration lost, 2 = clock stretch timeout.
  - `139E` receives a byte, then NAKs.
  - `0EE8` is a write transfer: device, word address, data, then STOP.
  - `1031` is a random read: device&FE, address, repeated START,
    device|1, read, then STOP.
- **Retrying wrappers:** `0DB3` (write) and `0E4C` (read) retry up to 7
  times with a 1 ms delay.
- **Devices:**
  - `A0`: 24C02-style EEPROM. Addresses 01-2B hold monitor adjustments
    (table `F000:5D07`, 3 bytes per entry; `F000:5A0C`/`5A89`), with a
    copy at 1B/29/2A.
  - `8C`: monitor control chip, written with register 2 (`F000:30CC`,
    `4E49`).
- **Use:** only for display type 6 (`[0xAA1]=6`, `ss:[0xEC4]=2`,
  `[0xAC3]=2`). The 3477 straps (4/C/E) give types 3-5, so the bus is
  never used. Modelled in `i2c.go`, which is optional.

## 7. Normal screen after a power-on with the default image

All 24 rows are blank. The status row shows only the row/column
indicator `1/80` at the right; the "Row/column indicator" Online Setup
option defaults to On. The cursor is on, at row 0, column 79. This is
the 5250 display with no host polling. Typing works; for example, `1C`
leaves the screen otherwise unchanged with no error.
