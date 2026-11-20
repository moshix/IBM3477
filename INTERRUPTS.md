# Interrupts on the IBM 3477 / 3486 InfoWindow boards

Everything known so far about the interrupt system, worked out from the
ROMs and from running them in the emulator (`cmd/probe`). Confidence:
**confirmed** = seen in code and/or observed running; **likely** = strong
evidence from the code; **guess** = plausible, not yet verified.

Addresses are `segment:offset` in the ROM unless written as a physical
address (`0x20007`). For the 3486, addresses use the corrected segment
map: the dump holds segments A000, B000, E000 and F000 (see README). An
earlier analysis labelled A000/B000 as 8000/9000; those addresses are
converted here.

## CPU side

- **Processor (confirmed):** 8088-class. The firmware uses only 8086
  instructions and no I/O ports, and there is no 80186 peripheral control
  block. Interrupts therefore follow the plain 8088 model:
  - one maskable INTR line, with the vector number supplied by external
    logic during the interrupt-acknowledge (INTA) cycle;
  - NMI is vector 2;
  - CPU exceptions use vectors 0 (divide error), 1 (single step), 3 (INT3)
    and 4 (INTO).
- **Interrupt flag use (confirmed):** the 3477 ROM has 63 `STI` and 71
  `CLI` sites. Reset code starts with `CLI` (3477 `F000:0054`).
- **Vector table (confirmed):** the standard 8088 table at `0000:0000`,
  4 bytes per vector.
  - On the 3477, physical `0x60`–`0x7F` (vectors `18h`–`1Fh`) is reused
    as the twinax engine's descriptor area: POST `F000:2091` fills it with
    `C0` and terminators. So only vectors `00h`–`17h` can be real vectors
    on that board.
  - POST's RAM tests overwrite the whole table with test patterns before
    any handler is installed.

## Interrupt controller: the I/O gate array (0x20000)

There is no 8259. Interrupt control lives in the I/O gate array at
`0x20000` (reached through ROM far-pointer tables such as 3477
`F000:086C` / `F000:169E` / `F000:26D6` → `2000:0002`).

| Address | Use | Model | Conf. |
|---|---|---|---|
| `0x20000`–`0x20003` | Interrupt/line status, write-1-to-clear. POST writes `FFFF` to `0x20002` before the twinax test to clear everything (3477 `F000:1FC8`, `F000:2080`) | both | likely |
| `0x20004` (word) | Interrupt mask. Cleared to 0 at reset (3477 `F000:005A`). 3486 handlers save it, write `817F`/`8001`/`8000`/`0`, then restore it (3486 `B000:1029`, `E000:00B9`) | both | likely |
| `0x20005` | Interrupt-enable byte; RAM shadow on the 3486 at `SS:3` | both | likely |
| `0x20007` | Interrupt enables. **Bit 0 = keyboard**, set by 3477 `F000:0C25` / `F000:0D48` before waiting for a keyboard interrupt and cleared afterwards (`F000:0C98`, `F000:0D92`). Bit 1 = a second source (3486 `F000:16C6`, `F000:1954`). RAM shadow on the 3486 at `SS:5` | both | confirmed (bit 0) |
| `0x20009` | Twinax control: bit 7 strobe, bits 1/2 select station (3477 `F000:2645`). Per-station interrupt enables on the 3486 (`E000:2605`). RAM shadow on the 3486 at `SS:7` | both | likely |
| `0x2000B` / `0x2000D` / `0x2000F` | Older board revision: acknowledge/status and an 8-bit mask, chosen when `SS:[0x91]` bit `0x40` is clear | 3486 | likely |

The vector is supplied by the gate array during INTA; the CPU never
computes it. How the gate array chooses between pending sources (fixed
priority?) is not known yet.

## Interrupt sources and vectors

### 3477 runtime (from the twinax analysis, see TWINAX.md)

The interrupt status is 16 bits: `0x20002` (bits 0–7, twinax channel 0)
and `0x20003` (bits 8–15, channel 1, plus the timer in bit 15). Writing 1
clears a bit; handlers write exactly the bit they handled. The mask is the
word at `0x20004`, where 1 = enabled. Its run-time value is `0x807F` with
one session and `0xFF7F` with two (`C000:0988`). A request is the level
`(status & mask) != 0`.

| Vector | Handler | Source | Nesting mask | Conf. |
|---|---|---|---|---|
| `06h` | `C000:5806` | timer, `0x20003` bit 7 (acknowledged at `C000:5827`) | `0000` | confirmed (handler), guess (rate) |
| `07h` | `C000:5E02` | channel 0 bit 0: poll response sent | `8000` | likely |
| `08h` | `C000:5E60` / `D000:5902` | channel 1 bit 0 | `8001` | likely |
| `09h` | `C000:6C1F` | channel 0 bits 1–6: command received (bit 1), data DMA done (bit 2), line errors (bits 3–6) | `8101` | likely |
| `0Ah` | `C000:6D22` / `D000:5102` | channel 1 bits 1–6 | `817F` | likely |
| `0Ch` | `C000:5702` | video range engine done: raised at the vertical sync after a range operation is started with `0x20138`; cleared by writing `0x20136` | — | likely |
| `0Eh` | `D000:103D` | keyboard (also called by the timer handler with `INT 0Eh` at `C000:5845`) | — | likely |

The nesting masks give the priority, highest first: timer > channel 0
bit 0 > channel 1 bit 0 > channel 0 bits 1–6 > channel 1 bits 1–6. The
emulator's INTA logic follows it (`IOGA.Ack`).

- **Timer rate:** never programmed in the ROM. It is modelled as a 60 Hz
  interrupt raised at each vertical sync (guess).
- **Vector table copy:** `C000:0058` copies 15 vectors from one of three
  templates, chosen by `SS:[0x389]`:

  | `SS:[0x389]` | Configuration | Template |
  |---|---|---|
  | 0 | one session | `C000:051C` |
  | 1 | two display sessions | `C000:0558` |
  | 2 | display + printer | `C000:0594` |

  In one-session mode vectors 8 and 10 are IRET stubs.

### Other sources

| Source | Vector | Evidence | Conf. |
|---|---|---|---|
| Keyboard (3477 POST) | **`0Eh`** | POST installs handler `F000:08F1` at `0000:0038` (`F000:0C13`–`0C1C`, `F000:0D3B`–`0D44`) and restores `F000:0B63` afterwards | confirmed |
| Keyboard (3486) | `0Fh` on the newer board, `0Eh` on the older one | 3486 `F000:16A3` installs `F000:12DF` at `0Eh` or `0Fh` | likely |
| Display (3486) | unknown | 3486 `E000:0000` signals "operation done" with `0x20136 = FF`; `E000:0094` acknowledges with `0x20003 = 80` | likely |

### 3477 vector templates (confirmed layout)

`C000:0058` copies 15 far pointers from one of these tables into vectors
`00h`–`0Eh`:

| Vector | `C000:051C` (one session) | `C000:0558` (two sessions) | `C000:0594` (display + printer) |
|---|---|---|---|
| 00h | `C000:00AF` | `C000:00AF` | `C000:00AF` |
| 01h | `C000:00B1` | `C000:00B1` | `C000:00B1` |
| 02h | `C000:00B3` | `C000:00B3` | `C000:00B3` |
| 03h | `C000:00B5` | `C000:00B5` | `C000:00B5` |
| 04h | `C000:00B7` | `C000:00B7` | `C000:00B7` |
| 05h | `C000:00AD` | `C000:00AD` | `C000:00AD` |
| 06h | `C000:5806` | `C000:5806` | `C000:5806` |
| 07h | `C000:5E02` | `C000:5E02` | `C000:5E02` |
| 08h | `C000:00AD` | `C000:5E60` | `D000:5902` |
| 09h | `C000:6C1F` | `C000:6C1F` | `C000:6C1F` |
| 0Ah | `C000:00AD` | `C000:6D22` | `D000:5102` |
| 0Bh | `C000:00AD` | `C000:00AD` | `C000:00AD` |
| 0Ch | `C000:5702` | `C000:5702` | `C000:5702` |
| 0Dh | `C000:00AD` | `C000:00AD` | `C000:00AD` |
| 0Eh | `D000:103D` | `D000:103D` | `D000:103D` |

`C000:00AD`, `00AF`, `00B1`, `00B3`, `00B5` and `00B7` are bare `IRET`
(`CF`) stubs: the default "ignore" handler.

### 3486 handlers (likely, from static analysis)

- **Entries:** handlers are entered through `EB 00` stubs.
- **Locations:** `B000:1000` (printer/twinax), `B000:1730`, `E000:0000`
  (display operation done), `E000:0094` (display), `E000:0850`,
  `E000:08BF`, `E000:1784`, `E000:1891`, `E000:9F80` (keyboard) and
  `E000:CDA4`.
- **Default vectors:** `F000:2E73` fills vectors 1–16 with `DS:3D84`,
  probably a RAM stub (guess).
- **Installation:** vectors are installed by computed index as
  `es:[4n-4]` (`F000:16A3`, and `F000:4CBE` from RAM table
  `SS:[0x3D54]`).

## Keyboard interrupt in detail (3477, confirmed)

**Receive** (POST routine `F000:0BF2`):
1. Write `0x20013 = 2` to release the keyboard line.
2. Clear the "byte arrived" flag (bit 7 of `00AA:001C`) and install
   `F000:08F1` at vector `0Eh`.
3. Set `0x20007` bit 0 and execute `STI`.
4. Poll the flag, with a timeout of `CX = 91C0h` delay loops.
5. On arrival, read the data from `0x20012` and the status from
   `0x20013`, then write `0x20013 = 3` (inhibit) and `0x20014 = FF`
   (acknowledge).
6. Check odd parity: status bit 0 must equal the parity bit, so data plus
   bit 0 has an odd number of ones (`F000:0C72`–`0C80`).

**Handler `F000:08F1`:**
- sets bit 7 of `00AA:001C`;
- clears IF in the FLAGS image on the stack (`and byte [bp+9], 0FDh`), so
  the code it returns to runs with interrupts disabled;
- ends with `IRET`.

It does **not** touch the hardware; acknowledgement is done by the
waiting code.

**Send** (POST routine `F000:0CC3`):
1. Write `0x20013 = 2`.
2. Wait until status bit 1 (`0x20013`) is clear, writing `0x20014 = FF`
   while waiting.
3. Write `0x20013 = 3`, `0x20014 = FF`, the data to `0x20012`, then
   `0x20013 = 2` to start sending.
4. Install `F000:08F1`, enable `0x20007` bit 0 and `STI`.
5. Wait for the interrupt that signals the end of transmission
   (timeout `CX = 64h`), then inhibit and acknowledge.

The keyboard's reply arrives later as a normal receive.

## How the emulator models it today

`internal/board/ioga.go`, `twinax.go` and `keyboard.go`:

- **INTR:** asserted while `(status & mask) != 0`, or while the keyboard
  port has a pending event and `0x20007` bit 0 is set.
- **INTA:** returns the vector for the highest-priority source (`06h`–`0Ah`
  as above); otherwise the keyboard's `0Eh`.
- **Status:** `0x20002`/`0x20003` are write-1-to-clear. The twinax engine
  sets bits 0–2; the 60 Hz frame timer sets `0x20003` bit 7.
- **Keyboard:** each byte transmitted to or received from the keyboard
  takes 1 ms and then raises the keyboard event, cleared by a write to
  `0x20014`.

## Open questions

1. **Twinax line errors:** what sets status bits 3–6 on real hardware.
2. **Display interrupt:** the vector, and what raises it (vertical sync,
   or completion of the address engine).
3. **Timer rate:** the real frequency of the `06h` timer.
4. **3477 run-time vector table:** where it is installed, and how the
   three templates map to vectors.
5. **3486 vector assignments:** how the newer board's numbering differs
   (keyboard `0Fh` versus `0Eh`).
