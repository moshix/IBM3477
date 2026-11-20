# Memory map of the IBM 3477 / 3486 InfoWindow terminals

All RAM, ROM and memory-mapped hardware addresses found so far, and what
they contain. This comes from the ROM disassembly (`tools/disasm3486.py`)
and from running the firmware in the emulator (`cmd/probe`).

- **Confidence:** **confirmed** = seen in code and/or observed in the
  emulator; **likely** = strong evidence from the code; **guess** =
  plausible, not verified.
- **Address format:** physical addresses are 5 hex digits (`0x0AA0`).
  ROM code addresses are `segment:offset`.
- **3486 addresses:** they use the corrected segment map (the dump holds
  A000, B000, E000 and F000). Older notes that said 8000/9000 mean
  A000/B000.

See also [INTERRUPTS.md](INTERRUPTS.md) for the interrupt system.

---

## IBM 3477

### Overview

| Range | Size | What | Conf. |
|---|---|---|---|
| `0x00000`–`0x1FFFF` | 128K | RAM (POST test 1 checks all of it) | confirmed |
| `0x20000`–`0x200FF` | — | I/O gate array registers | confirmed |
| `0x20100`–`0x201FF` | — | Video gate array (CRTC) registers | confirmed |
| `0xC0000`–`0xDFFFF` | 128K | ROM `ibm3477_part2.bin` (segments C000, D000) | confirmed |
| `0xE0000`–`0xFFFFF` | 128K | ROM `ibm3477_part1.bin` (segments E000, F000) | confirmed |
| everything else | — | unmapped; reads return `FF` in the emulator | — |

POST probes two addresses outside this map to detect options: `0xBFFFC`
(`BFFF:000C`) and `0x45678` (`4567:0008`). See below.

### RAM in detail

POST test 1 (`F000:1C1C`) tests RAM in four pieces, from the table at
`F000:1C24`: `0000:0000` (`0xD0` words), `0000:0B00` (`0x3500` words),
`0000:8000` (`0x4000` words) and `1000:0000` (`0x8000` words). The
earliest code (`F000:1A34`) tests `0x0AA0`–`0x0AFF` and `0x7500`–`0x7FFF`
before a stack exists.

| Address | Contents | Conf. |
|---|---|---|
| `0x00000`–`0x0005F` | Interrupt vector table, vectors `00h`–`17h`. Keyboard = `0Eh` at `0x0038` | confirmed |
| `0x00060`–`0x0007F` | Twinax command class table: class = `table[cmd & 1F] >> 6` (installed from `C000:7993` by `C000:74A2`) | likely |
| `0x00080`–`0x000FF` | Twinax channel 0 microprograms: 4 × 32 bytes, one per command class (copied from `C000:7930`/`7951`/`78EE`/`790F`/`7972` by `C000:7424`); `0x84` = end / disabled | likely |
| `0x00100`–`0x0017F` | Twinax channel 1 microprograms | likely |
| `0x00180`–`0x0018F` | Twinax channel 0 control block: `+0/+1` data DMA address, `+2/+3` count, `+4` mode (1 receive, 2 transmit), `+5` receive count, `+6/+7` receive pointer (advanced by the hardware), `+8` station address, `+B` polled flag, `+C` poll status (`0E` = power on), `+D` scan code for the next poll response (see TWINAX.md) | likely |
| `0x00190`–`0x0019F` | Twinax channel 1 control block (same layout) | likely |
| `0x001A0`–`0x00A9F` | **Non-volatile setup memory** (battery-backed RAM; POST never destroys it). `0x1A0`–`0x1FF` is the configuration: check-summed choices at `0x1A6`–`0x1DE`, display/printer address pairs at `0x1A0`–`0x1A5`, serial number in EBCDIC at `0x1F6`–`0x1FF`. Also holds the POST error log and a power-on counter at `0xA90`. Saved by the emulator in `~/.config/ibm3477/nvram.bin`. See NVRAM.md | confirmed |
| `0x00A20`– | POST error log `00A2:0000` (`F000:087C`): `+4` bit set per display type (`F000:04DA`); `+6` entry index, `1`–`0x15`, wrapping; entries of 3 bytes at `+4+3n` (word and byte) | likely |
| `0x00AA0`–`0x00AFF` | POST state `00AA:0000` (`F000:0880`); see the table below | confirmed |
| `0x05D00`–`0x07FFF` | Main data and stack segment `05D0`: `DS=SS=05D0`, `SP=1700` at reset, `2300` later (stack top `0x8000`). Holds `SS:05A4`/`05A5`/`05A8` (display state) | confirmed |
| `0x07500`–`0x07FFF` | Part of the above, tested at reset before the stack is used | confirmed |
| `0x08000`–`0x17FFF` | Video RAM, segment `0800` | confirmed |
| `0x0C200`–`0x0EE4F` | Screen cells, 3 bytes each: EBCDIC character, attribute (`0x20` = normal), extra byte. After POST: 3695 cells of `40 20 xx` (27×132 + one 131/132-cell status row). Two non-blank bytes at `0xEE36`/`0xEE37` (`D3 62`) are in the status row | confirmed (layout), likely (meaning) |
| `0x18000`–`0x19FFF` | Character generator, font 1, **left halves**: glyph for code *c* at `0x18000 + c·32`, one byte per pixel row (bit 7 = leftmost), up to 32 rows. Indexed by EBCDIC (`C1` = A) | confirmed |
| `0x1A000`–`0x1BFFF` | Font 1 **right halves** (`+0x2000`). Glyphs are 16 pixels wide | confirmed |
| `0x1C000`–`0x1FFFF` | Second font (alternate set or 132-column font), same layout | likely |

The font is built in RAM by the firmware; there is no character-generator
ROM in either dump.

#### POST state block, `0x0AA0` (`00AA:0000`)

| Offset | Contents | Conf. |
|---|---|---|
| `+01` | Display type from the straps: 3, 4 or 5 (`F000:044D`) | confirmed |
| `+02` | Flags. Bit 7 is set when `0xBFFFC` holds `EAEA` or RAM answers at `0x45678` (`F000:0830`–`086B`) | confirmed |
| `+04` | Initialised to `BF` (`F000:0144`) | confirmed |
| `+05` | Start mode from straps bits 4–5: 0 = normal, 2/3/4 = special modes (`F000:0793`). Selects the POST script | confirmed |
| `+16` | Step index into the POST script | confirmed |
| `+17` | Current POST test number (1–9) | confirmed |
| `+18` | Word: pointer to the POST script, from the table at `F000:0253` indexed by `+05` | confirmed |
| `+1A`/`+1B` | Error counter (`F000:0609`) | likely |
| `+1C` | Bit 7 = "keyboard interrupt happened" (set by `F000:08F1`) | confirmed |
| `+23` | 1 = known display type, `FF` = unknown | confirmed |
| `+24` | Initialised to 1 | confirmed |
| `+0A`–`+14` | Twinax test result: `+06` bit 0 = failed; `+0A`, `+0C`, `+0E` = failure details; `+14` = failing station (1 or 2) (`F000:216C`) | likely |

### Memory-mapped hardware, `0x20000`–`0x201FF`

**I/O gate array:**

| Address | Read | Write | Conf. |
|---|---|---|---|
| `0x20000`/`0x20001` | 0 | twinax channel 0/1 start: `80` = (re)start | likely |
| `0x20002`/`0x20003` | interrupt status: ch0 bits 0–6, ch1 bits 8–14, timer bit 15 | write 1 to clear | confirmed |
| `0x20004`/`0x20005` | mask | interrupt mask word, 1 = enabled; `807F` (one session) / `FF7F` (two) at run time | confirmed |
| `0x20007` | — | interrupt enables; bit 0 = keyboard | confirmed |
| `0x20008` | — | twinax station addresses: ch0 bits 0–2, ch1 bits 3–5 (`C000:74B7`) | confirmed |
| `0x20009` | — | twinax control: bit 7 = start strobe, bits 1/2 = station select; 0 at reset, `80` after the twinax test | likely |
| `0x2000A` | straps: bits 1–3 = display type (4/C/E valid), bits 4–5 = start mode | — | confirmed |
| `0x20010` | LED/error code | `9B` at reset and on POST error, 0 = POST passed | confirmed |
| `0x20012` | keyboard byte received | keyboard byte to send | confirmed |
| `0x20013` | status: bit 0 = parity, bit 1 = busy/pending, bit 2 = received | 2 = release line, 3 = inhibit | confirmed |
| `0x20014` | — | keyboard acknowledge (`FF`) | confirmed |
| `0x20015` | — | 0 at reset. Twinax test writes `48`/`44` (test mode or wrap select). On the 3486 bit 1 = beeper | likely |
| `0x20020`–`0x2002B` | `0x2002B` bit `20` (3486) | twinax wrap test: `0x20024` word, `0x20026` word, `0x20028` byte (`F000:2660`) | likely |

**Video gate array (CRTC):**

| Address | Read | Write | Conf. |
|---|---|---|---|
| `0x20127` | — | 0 at reset (`F000:0088`) | — |
| `0x20128` | — | `D0` (`F000:1DBE`) | — |
| `0x20129` | — | `F0` at reset (`F000:0083`) | — |
| `0x2012F` | status: bit 3 = vertical sync; bits 0–1 = monitor ID (`F000:07A2`) | `18` at reset | confirmed (bit 3) |

### ROM in detail

| Address | Contents | Conf. |
|---|---|---|
| `F000:FFF0` | Reset vector: `JMP F000:0000` | confirmed |
| `F000:0000` | `JMP F000:0054`: CLI, gate array init, `SS=05D0`, then `JMP F000:1A32` | confirmed |
| `F000:0002`–`F000:0037` | POST test table, 6-byte entries (flag, flag, far pointer). Table below | confirmed |
| `F000:0054` | Reset entry | confirmed |
| `F000:009C` | Main init after the RAM and ROM checks: straps, display type, start-mode dispatch | confirmed |
| `F000:0173`–`019E` | POST script interpreter: high nibble of each script byte = handler (table at `F000:0257`), low nibble = argument | confirmed |
| `F000:0253` | Table of POST script pointers, indexed by start mode | confirmed |
| `F000:0279` | POST test loop | confirmed |
| `F000:03BA`–`03E5` | POST failure handling: LED `9B` if flag bit `80`, `HLT` unless flag bit `40` | confirmed |
| `F000:03E6` | Wait for a key after a POST error (calls `F000:0B80`) | confirmed |
| `F000:044D` | Read straps → display type | confirmed |
| `F000:0793` | Read straps → start mode | confirmed |
| `F000:0830` | Option probes at `0xBFFFC` / `0x45678` | confirmed |
| `F000:086C` | Pointer table: `2000:0002`, `2002:0000`, `2010:0000`, `001A:0000`, `00A2:0000`, `00AA:0000`, `BFFF:000C`, `4567:0008` | confirmed |
| `F000:08A2` | Show a 3-character EBCDIC POST code (e.g. `C1 D9 D4`) | likely |
| `F000:08F1` | POST keyboard interrupt handler | confirmed |
| `F000:090E`, `0917`, `092A`, `0964` | Delay loops (`0964` is about 1 ms) | confirmed |
| `F000:0AF5`, `0B2A`, `0B4F` | Set the LED/error code at `0x20010` | confirmed |
| `F000:0B63` | Default keyboard vector restored after POST waits | confirmed |
| `F000:0B80` / `0BB9` / `0BF2` | Receive a keyboard byte, with timeout | confirmed |
| `F000:0CC3` | Send a byte to the keyboard | confirmed |
| `F000:169E` | Pointer table: `2000:0002`, `2002:0000`, `2010:0000`, `001A:0000`, `00A2:0000`, `00AA:0000`, `0000:0000` (vector table) | confirmed |
| `F000:1A32` | Reset RAM tests (`0x0AA0`, `0x7500`–`0x7FFF`) | confirmed |
| `F000:1B18` | ROM checksum table: (segment, word sum) for `C000`=1234, `D000`=5678, `E000`=9ABC, `F000`=DEF0 | confirmed |
| `F000:1C24` | RAM test table (see RAM above) | confirmed |
| `F000:26D6` | Pointer table: … `2002:0004` (`0x20024`), `0006:0000` (`0x60`), `0008:0000` (`0x80`) | confirmed |
| `C000:00AD`–`00B8` | IRET stubs (default interrupt handlers) | confirmed |
| `C000:051C`–`05CF` | Three 15-entry interrupt vector templates (one session / two sessions / display + printer), copied to vectors 0–14 by `C000:0058` (see INTERRUPTS.md) | confirmed |
| `C000:5702`, `5806`, `5E02`, `5E60`, `6C1F`, `6D22`; `D000:103D`, `5102`, `5902` | Interrupt handlers | likely |
| last 2 bytes of each 64K segment | Checksum padding (`9C25` and `961B` at the ends of the two files) | confirmed |

#### POST tests (`F000:0002`)

| # | Flags | Routine | What |
|---|---|---|---|
| 1 | `03 C2` | `F000:1C1C` | RAM test (passes in the emulator) |
| 2 | `03 50` | `F000:1E22` | passes in the emulator; not analysed |
| 3 | `03 C2` | `F000:1FA4` | Twinax wrap test (fails until the twinax engine is modelled) |
| 4 | `02 C2` | `F000:1FB2` | Twinax test, variant |
| 5 | `03 C2` | `F000:26FA` | not analysed |
| 6 | `03 C2` | `F000:2DFE` | not analysed |
| 7 | `03 00` | `F000:2EFE` | not analysed |
| 8 | `03 43` | `F000:3116` | not analysed |
| 9 | `03 01` | `F000:329E` | not analysed |

Flag byte 2: bit `80` = show LED `9B` on failure, bit `40` = continue
instead of halting.

---

## IBM 3486

The 3486 dump is incomplete: segments 8000, 9000, C000 and D000 are
missing (see README). The map below comes from static analysis of the
segments we do have.

### Overview

| Range | Size | What | Conf. |
|---|---|---|---|
| `0x00000`–`0x1FFFF` | 128K | RAM: system, configuration, video | likely |
| `0x20000`–`0x201FF` | — | I/O and video gate arrays (same as the 3477, with extra registers) | confirmed |
| `0x30000`–`0x37FFF` | 32K | Expansion RAM, tested when the expansion card is present (`F000:2FEB`) | likely |
| `0x38000`–`0x4FFFF` | 96K | System RAM (stack `SS=3800`) | likely |
| `0x60000`–`0x7FFFF` | 128K | Expansion ROM (3487 feature card). ID word `0x3487` at `6000:0000` (`F000:2FBF`); checksum `0x619A` (`F000:3043`); "EXP" error if missing | likely |
| `0x80000`–`0xFFFFF` | 512K | ROM `3486.bin`. Word sum must be `0x619A` (`F000:2B97`). The dump holds only A000, B000, E000, F000 | confirmed |

### RAM in detail

POST clears RAM from a table at `F000:2EBC` (segment, offset, word count)
using the loop at `F000:2DCA`.

| Address | Contents | Conf. |
|---|---|---|
| `0x00000`–`0x0003F` | Interrupt vector table (16 vectors); `F000:2E73` fills 1–16 with a RAM stub | likely |
| `0x00060`–`0x0027F` | Twinax descriptor blocks (32 bytes, ending in `0x84`), loaded from `E000:2716`–`2805` | likely |
| `0x00180` | Twinax block, display session 1 (data `DS=00D8`, ack via `0x20000`, address `SS:[0x3DC]&7`) | likely |
| `0x00190` | Twinax block, display session 2 (`DS=0292`, ack via `0x20001`, address `SS:[0x3DD]`) | likely |
| `0x00190`/`0x001A0` | Twinax block, printer station (`DS=044C`, ack via `0x20001` or `0x2000B`, address `SS:[0x3DE]`) | likely |
| `0x00280`–`0x004FF` | Non-volatile setup (segment `0028`): signature `ABCD`, checksum, ROM-patch record (`F000:0285`) | likely |
| `0x00500`–`0x0055F` | POST status (segment `0050`): the only low RAM tested before the stack exists | confirmed |
| `0x00560`–`0x00D5F` | 2K configuration (segment `0056`), read at `+7A0`–`+7FC` (`A000:DB54`) | likely |
| `0x00D80`– | Display session 1 data (`DS=00D8`) | likely |
| `0x02920`– | Display session 2 data (`DS=0292`) | likely |
| `0x044C0`– | Printer session data (`DS=044C`) | likely |
| `0x08000`–`0x1FFFF` | Video RAM, segment `0800`; cleared only during vertical sync | confirmed |
| `0x0C204`– | Presentation space: 3-byte cells, 27×132 = 3564 cells (`0800:4204`, `F000:138D`–`13DA`) | likely |
| `0x0EE04`– | 132-cell status row (`0800:6E04`); the CRTC register `0x20134 = 6E04` points here | likely |
| refresh buffer | character and attribute planes `0xFA0` apart; blank = `0x20` (`B000:4846`–`486E`) | likely |
| `0x38000`–`0x39E7F` | Stack and data (`SS=3800`, `SP=1E80`) | confirmed |
| `0x3AF80`–`0x3BFFF` | `3800:2F80`–`3FFF`: far-call table indexed by function number (`SS:2F80+4(n-1)`), used by all cross-module calls (`lcall ss:[0x3718]` 196×, `ss:[0x3000]` 95×) | likely |
| `0x3C080`–`0x3F07F` | RAM cleared by POST | confirmed |
| `0x43084`–`0x469DF` | RAM cleared by POST | confirmed |
| `0x469E0`– | ROM patch area; patches the far-call table (`F000:DB06`, `F000:DC44`) | likely |

### Memory-mapped hardware (beyond the 3477 set)

| Address | Use | Conf. |
|---|---|---|
| `0x20000`/`0x20001` | Station acknowledge, session 1 / session 2 | likely |
| `0x20004` (word) | Interrupt mask, saved and rewritten by handlers (`817F`/`8001`/`8000`/`0`) | likely |
| `0x2000A` | Straps: bits 1–3 = model, bits 4–5, bit 6 (`F000:09DD`, `0EA5`, `0F6D`) | confirmed |
| `0x2000B`/`0x2000D`/`0x2000F` | Older board revision: acknowledge/status and mask | likely |
| `0x20010` | LED/error code (`9B` at reset, 0 when POST passes) | confirmed |
| `0x20012`–`0x20014` | Keyboard (as on the 3477) | confirmed |
| `0x20015` | Beeper, bit 1 | confirmed |
| `0x20017`, `0x2001A`/`0x2001B` | Miscellaneous control | — |
| `0x20020`–`0x2002B` | Twinax wrap/line test (`F000:3922`–`39DC`) | likely |
| `0x20030`/`0x20031` | Small control port (`B000:CED7`, `B000:3EF2`) | — |
| `0x20100`–`0x20117` | CRTC timing: 12 words copied from the mode record (`F000:2495`–`278E`) | confirmed |
| `0x20121`/`0x20123` | Cursor or pointer address (`E000:BF5B`) | likely |
| `0x20125`–`0x2012F` | 11 bytes from the mode record | confirmed |
| `0x2012F` | Status: bit 3 = vertical sync, bits 0–1 = monitor ID | confirmed |
| `0x20134` | Status-row start address (`6E04`) | likely |
| `0x20136` | Written `FF` by the display interrupt (`E000:0000`) | likely |
| `0x20138`–`0x2013B` | Address-range engine (fill/move), completion raises an interrupt | likely |
| `0x20140`/`0x20141`/`0x20149` | Mode record bytes `23`–`25` (colour/extended) | confirmed |
| `0x20145` | Hardware revision bit `0x80` | likely |

### ROM in detail

| Address | Contents | Conf. |
|---|---|---|
| `F000:FFF0` | Reset vector `JMP F000:0000` → `F000:01DF` | confirmed |
| `F000:01DF` | Reset entry: `DS=SS=3800`, `SP=1E80` | confirmed |
| `F000:0192` | POST status characters `CLDI12KM01` | confirmed |
| `F000:01A2`/`01B0` | POST script selection per model | confirmed |
| `F000:04DA` | POST byte-code interpreter (dispatch at `F000:05AB`) | confirmed |
| `F000:0374`, `F000:05A6` | Far call / jump into `D000` (**missing** from the dump) | confirmed |
| `F000:1244`, `F000:2792`, `F000:2C52` | Hardware pointer tables (`2000:0002`, `2002:0000`, `2010:0000`, `0056:0000`, `0028:0000`, `0050:0000`, `6000:0000`) | confirmed |
| `F000:2495`–`278E` | Display mode set | confirmed |
| `F000:27B2`–`2A10` | Mode records, 38 bytes each: 24/25, 32/33, 43/44, 27/28 and 49/50 rows at 80 or 132 columns; pointers at `F000:2A21` | confirmed |
| `F000:2B97` | ROM checksum record: `8000`–`F000` must sum to `0x619A` | confirmed |
| `F000:2EBC` | RAM clear table | confirmed |
| `F000:B876` | 3487 monitor adjustment names in 5 languages | confirmed |
| `F000:DCBC` | "NO PATCH" | confirmed |
| `A000:3000`–`AD00` | EBCDIC message tables in English, French (~`4F00`), German (~`6200`), Italian (~`7E00`) and Spanish (~`9800`); pointer tables at `A000:4137` and nearby | confirmed |
| `A000:3284` / `A000:32B4` | "3486 Microcode … 1989 1992" / "3487 Microcode …" | confirmed |
| `E000:2216` | Twinax per-session command decode (`call [0x1204]`) | likely |
| `E000:9F82` | Keyboard interrupt handler (state machine `E000:A1C8`–`A4CF`) | likely |
| `FFFF:000E` | Last word of the ROM `0xDCBA` (bytes `BA DC`): checksum padding | confirmed |
