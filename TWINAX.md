<!-- Twinax station engine spec, reverse-engineered from the 3477 ROM.
     Implemented in internal/board/twinax.go and ioga.go. -->
# IBM 3477 twinax station hardware: behavioural spec

Derived from the 3477 ROMs (C0000-FFFFF). Confidence tags:
**[C]** confirmed by code (the cited instructions do exactly this),
**[L]** likely (several independent pieces of code agree),
**[G]** guess (consistent with the code but not forced by it).

The model is implemented in `internal/board/twinax.go` (engine) and
`internal/board/ioga.go` (status/mask registers, interrupt priority).
With it **POST test 3 passes** on both channels and POST continues to
test 5, the video test ("VDO").

The interrupt-vector handlers are disassembled by seeding the listing
tool with them, e.g.:

```sh
printf 'C000:6C1F\nC000:6D22\nC000:5E02\nC000:5E60\nD000:5102\nD000:5902\nD000:103D\nC000:5702\nC000:5806\n' > seeds.txt
python3 tools/disasm3486.py --model 3477 --listing 3477.listing \
        --seeds seeds.txt ibm3477_part2.bin ibm3477_part1.bin
```

## 0. Big picture

The twinax engine is part of the I/O gate array. It has **two channels**,
which are two station addresses (sessions). The engine runs **byte-coded
microprograms** that the firmware copies into low RAM. The command byte of
each received message selects one of 4 programs through a 32-entry class
table at 0x60.

Programs move bytes between a 16-byte **control block in RAM** (0x180 for
channel 0, 0x190 for channel 1) and internal DMA registers. They also start
DMA receive or transmit, send the poll response, and set status bits in a
per-channel interrupt-status byte.

Poll and Ack are answered by the engine itself (microprogram 2) using
status bytes that the firmware keeps in the control block (+C/+D). The
firmware only ever sees:

- command bytes, which are DMA'd into a 16-byte receive buffer;
- data streamed directly into or out of display RAM (block +0..+4);
- the status/interrupt bits.

| | Channel 0 | Channel 1 |
|---|---|---|
| Control block (RAM) | 0x180-0x18F | 0x190-0x19F |
| Channel/start register | 0x20000 | 0x20001 |
| Interrupt status (W1C) | 0x20002 | 0x20003 |
| Interrupt mask bits | 0x20004 bits 0-6 | 0x20005 bits 0-6 (= mask bits 8-14) |
| Station address | 0x20008 bits 0-2 | 0x20008 bits 3-5 |
| Enable bits in 0x20009 | bit1 (+ bit3, bit5) | bit2 (+ bit4, bit6) |
| Programs (4 x 32 bytes) | 0x080,0x0A0,0x0C0,0x0E0 | 0x100,0x120,0x140,0x160 |
| Firmware data segment | DS=0x00F0 | DS=0x0290 (or 0x0430 in printer mode) |
| Receive buffer | DS:1010 = phys 0x1F10, 16 bytes | phys 0x3910 (0x4440 for DS=0x430) |
| "Command" vector (status bits 1-6) | INT 9 = C000:6C1F | INT 10 = C000:6D22 (D000:5102) |
| "Poll done" vector (status bit 0) | INT 7 = C000:5E02 | INT 8 = C000:5E60 (D000:5902) |

The firmware uses far pointers in its data segment: `[0]` points to the
control block (0000:0180 or 0000:0190) and `[4]` to the channel register
(2000:0000 or 2000:0001). So `[4]+2` is the status register. Evidence:
C000:7511 and C000:75BE (session init), D000:7AD5 (printer session).

## 1. Registers

### 1.1 I/O gate array

| Addr | R/W | Meaning | Evidence |
|---|---|---|---|
| 0x20000 / 0x20001 | W | **Channel start ("arm")**: writing 0x80 starts or re-arms the channel after the control block has been set up. Written after block init (C000:788C-7890), after every command queue (C000:726D, 7297 inside 71F8), in POST before tests B and D (F000:225C, 24D7), and in printer init (D000:7BEC). **[C]** that it is written; **[L]** for the meaning. | |
| 0x20000 / 0x20001 | R | bit3 = busy: 71F8 loops `test [si],8` until it is clear (C000:7254). bit7 read right after writing 0x80: if set, the start "did not take" and 71F8 retries (C000:7270, 729A). **Emulator: return 0x00.** **[C]** that it is read this way; **[G]** for the meaning. | |
| 0x20002 / 0x20003 | R | **Interrupt status**, channel 0 / channel 1, bits 0-6 (table below). 0x20003 bit7 is the **timer** (mask bit 15). **[C]** | INT9 reads `[4]+2` (C000:6C46-6C4E); INT10 reads it (6D49); timer acks 0x20003 with 0x80 (C000:5827) |
| 0x20002 / 0x20003 | W | **Write 1 to clear.** The firmware writes the single bit it handled: 4 (6F5D), 0x40 (6F99), 0x20 (6FC1), 0x10 (6FD6), 8 (6FEB), 2 (725B), 1 (5E4C, 5EAA). POST writes word 0xFFFF to clear everything (F000:1FC8, 24CE). **[C]** | |
| 0x20004 (word) | R/W | **Interrupt mask**: 1 = enabled. Bit n matches status bit n, where bits 0-7 are 0x20002 and bits 8-15 are 0x20003. Runtime value: 0x807F with one session, 0xFF7F with two (C000:09A3, 09AB). Handlers save it, write their own nesting mask, and restore it on exit. **[C]** for the values; **[L]** for the bit mapping. | |
| 0x20007 | W | Second enable byte, not twinax (keyboard and the C000:0908 loop). Out of scope. | |
| 0x20008 | W | **Station addresses**: bits 0-2 = channel 0 address (`ss:[0x386]`), bits 3-5 = channel 1 address (`ss:[0x387]` in mode 1, `[0x388]` in mode 2). **[C]** C000:74B7-7504. POST writes 0x2A (addresses 2 and 5, F000:1FFC) and 0x3F (both 7, F000:23A7). | |
| 0x20009 | W | **Line control.** bit7 = global run/enable; it is pulsed 0 then 1 around POST wrap set-up. bit0 is global (runtime `ss:[7] \|= 0x81`, C000:0988). Per channel, from the templates at C000:79B4 / 79BC copied to DS:1310: **ch0 enable = bit1** (`[0x1314]`=02), **ch1 enable = bit2** (04), set at C000:7877-7888. After the first Set Mode, **ch0 bit3 / ch1 bit4** are set (`[0x1312]`, C000:8E6A-8E7B). "Offline" (Reset command, C000:70DE) ANDs with `[0x1315]` = 0xD5 (ch0: clears bits 1,3,5) or 0xAB (ch1: clears bits 2,4,6). POST selects the channel under test with bit1 (ch0) or bit2 (ch1); the other bit is 0 (`[bp+2a]` = 0xFB/0xFD). **[C]** for the writes; **[L]** for the meanings. | |
| 0x20015 | W | Line and wrap control, shared with the beeper (low bits). Runtime init writes 0x0C (C000:0800). POST writes 0x48 (test 3, internal wrap?) or 0x44 (test 4, F000:1FB2, not run at power-on) **after** each wrap set-up. bit6 = wrap enable/go, bit4 = force a parity error (F000:2548), bit7 = memory-DMA wrap (test C, F000:2359). **[C]** for the values; **[G]** for the meanings. | |
| 0x20020 (word) | W | POST test C: DMA address = 0x3FFC (16-bit physical). Only POST uses it. **[C]** F000:2333 | |
| 0x20022 (word) | W | POST test C: count = 8. **[C]** F000:2338 | |
| 0x20024 | W | Wrap frame: bits 0-2 = station address of the injected command (2/5/7). bit5 is set in test B (0x22/0x25) and its meaning is unknown. **[L]** | F000:2665 |
| 0x20025 | W | Wrap frame control: 06/0B (tests A, B), 02/03 (test D), 0x20 (test C). Meaning unknown. bits 0-1 look like the channel tag (2 = ch0, 3 = ch1). **[G]** | |
| 0x20026 | W | Wrap **command byte**. It selects the program class through the table at 0x60 (proven: the per-channel command values in tests A, B and D always map to the same class). **[L]** | F000:266B |
| 0x20027 | W | Wrap **data byte** (one data frame). **[L]** | |
| 0x20028 | W | Copy of 0x20027 (EE/AA/CC/00). Meaning unknown; ignore it. **[G]** | F000:2672 |
| 0x2002B | R | Wrap result: the poll-response status byte (test D) or the last memory byte (test C). Read with 0x20009 bit7 = 0 (F000:2693). **[C]** that it is read; **[L]** for the meaning. | |

**Interrupt status bits** (0x20002 for ch0, 0x20003 for ch1):

| bit | Set by | Firmware handling | Tag |
|---|---|---|---|
| 0 (0x01) | Poll response sent (end of program 2) | INT 7 / INT 8 handler (C000:5E02 / 5E60): delivers the next keystroke to block +D (sub 5EBF → 6B30, 6153 → 6128/6174 → 6B12), then acks 1 | [L] |
| 1 (0x02) | Command received: program 0 stored a command byte | INT9 → 6FF2 reads the first command, then runs the command loop (svc 0x20 = C000:7A02) until EOQ. **Not acked until** 71F8 re-arms (C000:725B writes 2) | [C] handling, [L] source |
| 2 (0x04) | Data DMA finished (op 0x90 at the end of programs 1 and 3) | 6E1E: reads block +0/+1 back and updates the address counter; acks 4 (6F59) | [C] handling, [L] source |
| 3 (0x08) | Line error | 6F68: sets the abort flag `[0x11c1]\|=0x40`, acks 8 | [C] handling |
| 4 (0x10) | Line error | exception 0x0A (`7052(0x0A)`), acks 0x10 | [C] handling |
| 5 (0x20) | Error | exception 4, block+4 = 0 (abort DMA), acks 0x20 | [C] handling |
| 6 (0x40) | Error / line reset | block+4 = 0, abort, counts error `ss:[0x9b]++`, acks 0x40 | [C] handling |

### 1.2 Interrupt vectors and priority [L]

C000:0058 copies 15 far pointers to IVT entries 0-14. Which table it uses
depends on `ss:[0x389]` (0 = one session, 1 = two display sessions,
2 = display + printer). The tables are at C000:051C / 0558 / 0594 and
can be read with `tables.txt`:

```
INT6  C000:5806  timer (acks 0x20003 bit7, mask 0 while running)
INT7  C000:5E02  ch0 status bit0          (mask 0x8000 while running)
INT8  C000:5E60 / D000:5902 ch1 bit0      (mask 0x8001)
INT9  C000:6C1F  ch0 status bits 1-6      (mask 0x8101)
INT10 C000:6D22 / D000:5102 ch1 bits 1-6  (mask 0x817F)
INT12 C000:5702  video   INT14 D000:103D keyboard (also "int 0Eh")
INT0-5, 11, 13 = IRET stubs
```

In one-session mode INT8 and INT10 are IRET stubs. The nesting masks
imply this priority, highest first: timer(bit15) > ch0.bit0 > ch1.bit0 >
ch0.bits1-6 > ch1.bits1-6. Suggested INTA logic: let
`p = status & mask` (16 bits); the vector is 6 if bit15 is set, else 7
if bit0, else 8 if bit8, else 9 if bits 1-6, else 10 if bits 9-14. This
is a **level** request (INTR = p != 0). Handlers clear the source by
writing 1s to the status register (W1C).

### 1.3 Control block (RAM 0x180 / 0x190), firmware view

| Off | Written by firmware | Written by engine | Meaning | Evidence |
|---|---|---|---|---|
| +0/+1 | data DMA address (16-bit physical) = DS*16+DX (708B) | updated as data moves (prog 1 and 3) | display-RAM pointer for Write/Read Activate | C000:70A9-70B0, read back 6E62/6E9A |
| +2/+3 | data DMA count (CX) | — (read by the engine) | | 70BC/70C4 |
| +4 | mode `[0x118d]`: 1 = receive into RAM (Write Activate), 2 = transmit from RAM (Read Activate), 0 = none | cleared to +F (0) when prog 1/3 starts | | 7092, 9751 (1), 8349... (2) |
| +5 | receive buffer length 0x10 | decremented per stored byte | | 782D, 7239 |
| +6/+7 | receive buffer pointer (phys) | **advanced live** per stored byte | firmware polls +6 for new command bytes (72E9, 733C) | 7835/783D, 7259 |
| +8 | station address (low 3 bits) and Set Mode bits 3-7 | read by prog 2 and 3 | | 7825, 8DDE |
| +9 | config `[0x1339]` \| 0x20 (\| 0x10 after Set Mode != 0) | read by prog 3 | | 7873, 8DEA |
| +A | config `[0x133a]`: bit1 set, bit0 clear, +0x10 after Set Mode, +[0x1310] (04 ch0 / 08 ch1) when the station is initialised, cleared on offline | read by prog 2 | | 7867, 8E8D, 7102 |
| +B | firmware sets 0xFF | **prog 2 sets it to +F (0) on every poll** | "controller is polling us": the timer checks for 0 (C000:5B6B, 5C1E) | |
| +C | **poll status byte**: exception code (0x0E at power-on = exception 7, 0x02, 0x04, 0x08, 0x0A; 0 = OK) | read by prog 2 | | 7841, 7052, AC2B |
| +D | **poll data byte** = keyboard scan code (0 = none) | read by prog 2 | | 6B12 |
| +E | — | scratch (old +5 / +4) | | |
| +F | always 0 | read as the constant 0 | | 7850, 710B; POST 1FDD |

### 1.4 Program area 0x60-0x17F

- 0x60-0x7F: **class table**. Entry `[0x60 + (cmd & 0x1F)]`; its top 2 bits
  give the class (00→prog0, 40→prog1, 80→prog2, C0→prog3). The runtime
  template is C000:7993, installed by C000:74A2: entry 0x00 (Read
  Activate) = C0, entry 0x01 (Write Activate) = 40, entry 0x10 (Poll 0x10,
  Ack 0x30, and the parity variants 0x50/0x70) = 80, everything else = 00.
  **[L]**
- Programs (installed by C000:7424 from C000:7930/7951/78EE/790F/7972):

```
slot+0x00 prog0 (ordinary commands)  56 20 57 21 55 22 3E 5F 23 82 A2 42 35 40 36 41 37 84
slot+0x20 prog1 (Write Activate)     54 3E 88 5F 34 52 22 53 23 50 20 51 21 A2 40 30 41 31 90 84
slot+0x40 prog2 (Poll/Ack)           5A 25 5D 27 5C 26 58 24 C0 5F 3B 84   (ch1: 5D 28 instead of 5D 27)
slot+0x60 prog3 (Read Activate)      54 3E 88 5F 34 59 25 58 24 52 22 C0 53 23 50 20 51 21 A1 40 30 41 31 90 84
```

**Microcode ISA [L].** It reproduces every POST expectation and every
firmware use.

| Op | Meaning |
|---|---|
| 0x20+n | h[n] = acc (16 hardware registers: h0/h1 = DMA address lo/hi, h2/h3 = DMA count lo/hi, h4-h8 = response parameters) |
| 0x30+n | block[n] = acc |
| 0x40+n | acc = h[n] |
| 0x50+n | acc = block[n] |
| 0x84 | end. Writing 0x84 at a slot start **disables** that program: POST does this for prog3 (F000:26CA, 26CF), and 71F8 does it for prog0 while re-arming (C000:7249, then restores 0x56 at 7275) |
| 0x88 | end if acc == 0 (no data transfer was set up, +4 == 0) [G] |
| 0x82 | store the command byte through the DMA (h1:h0, count h3:h2) and set status bit1 [L] |
| 0xA2 | store the message's data frames through the DMA [L] |
| 0xA1 | transmit h3:h2 bytes from h1:h0 as response frames [L] |
| 0x90 | set status bit2 (data transfer complete) [L] |
| 0xC0 | start the response. In prog2 this sends the poll status (h6 = +C, h7 = +D, h4 = +8, h5 = +A) and then sets status bit0. In prog3 it is the read-response start (h4 = +8, h5 = +9) [L/G] |

Decoded, for example, prog0 is: h0..h2 = +6, +7, +5; +E = +5;
h3 = +F; store cmd; store data; then +5, +6, +7 = h2, h0, h1.

## 2. POST test 3 (F000:1FA4, variant `[bp+28]` = 0x48) exact sequence

DS = 0x05D0, so the far pointer `[0]` sits at 0x5D00 and points at 0x180
or 0x190. A failure sets byte 0xAA6 bit0 and the error code word at
0xAAA, and every later step is skipped. On failure the test calls
F000:08A2 with "COM" (C3 D6 D4); the 0x44 variant uses "AWP". The channel
number (1/2) goes to 0xAB4. "Select ch0" means 0x20009 = 0xFB (strobed
0x7B → 0xFB); "select ch1" means 0xFD.

A **wrap transfer** (sub 2645, called with hi word H and lo word L) is:
0x20009 = sel & 0x7F; 0x20024 word = H; 0x20026 word = L;
0x20028 = L >> 8; 0x20009 = sel | 0x80; **0x20015 = 0x48**; delay
(F000:0917). Reading the result (sub 2693) is: 0x20009 = sel & 0x7F;
read 0x2002B; 0x20009 = sel | 0x80.

**Set-up (1FBC-200B):** CLI; mask 0x20004 = 0; 0x20002 word = 0xFFFF;
0x18F = 0x19F = 0; **0x20008 = 0x2A**; 0x60-0x7F filled with C0 except
0x65 = 40, 0x66 = 80, 0x69 = 00, 0x77 = 40, 0x7A = 80, 0x7B = 00
(sub 2091); C000:7402 installs the programs; **0x0E0 = 0x160 = 0x84**
(prog3 disabled).

| Sub | Channel | Inputs | Wrap regs (24,25,26,27,28) | cmd → class | Required result (else code) |
|---|---|---|---|---|---|
| A (20D2/2139) | ch0, blk 0x180 | +5 = 0x80, +6 word = 0x3FFF | 02 06 1B EE EE | 0x1B → prog0 | **0x185 = 0x7E, word 0x186 = 0x4001** (else 0x10, with 0xAAC = +5 and 0xAAE = +6) |
| A | ch1, blk 0x190 | same | 05 0B 09 EE EE | 0x09 → prog0 | 0x195 = 0x7E, word 0x196 = 0x4001 |
| B (21AD/223C) | ch0 | mask 0, status cleared; +0 word = 0x3FFF, +2 word = 0x2000, +4 = 1; **0x20000 = 0x80** | 22 06 05 AA AA | 0x05 → prog1 | **word 0x180 = 0x4000** (else 0x20, 0xAB0 = value) |
| B | ch1 | same, **0x20001 = 0x80** | 25 0B 17 CC CC | 0x17 → prog1 | word 0x190 = 0x4000 |
| C (22A7/2319) | ch0 | RAM 0x3FFC..0x4003 = 7D 7E 7F 80 81 82 83 84; 0x20020 = 0x3FFC, 0x20022 = 8, 0x20024 word = 0x2002; **0x20015 = 0xC8** | — | — | **0x2002B = 0x84** (else 0x30, 0xAB2 = value) |
| C | ch1 | same, 0x20024 word = 0x2005 | | | 0x2002B = 0x84 |
| D set-up (23A2) | both | **0x20008 = 0x3F**; blk+8 = 7, +A = 2 (ch0) / 3 (ch1), +C = +D = 0 | | | |
| D1 (24BB) | ch0 | mask 0, 0x20002 = 0xFFFF, **0x20000 = 0x80**, poll ×2 | 07 02 1A 00 00 | 0x1A → prog2 | 1st: 0x2002B bit7 = **0**; 2nd: bit7 = **1** (else 0x41) |
| D2 (2518) | ch0 | poll(0x5A) | 07 02 5A 00 00 | prog2 | bit6 = 0 |
| | | poll(0x1A) with 0x20015 = **0x58**, then 0x20015 = 0x48 again, delay | 07 02 1A | | bit6 = **1** |
| | | poll(0x5A) | | | bit6 = 0 (else 0x42) |
| D cleanup (25A1) | both | poll(0x5A) on ch0, poll(0x46) on ch1 | | | not checked |
| D3 (25E0) | ch0 | poll(0x1A) ×3 | | | bit5 **toggles** 1→2 and toggles back 2→3 (else 0x43) |
| D1-D3 | ch1 | same, with 0x20001, frames 07 03 06/46 00 00 | | 0x06/0x46 → prog2 | same |

**End (2075):** mask = 0, 0x20002 = 0xFFFF, 0x20009 = 0x80.

### A minimal model that passes (implemented in `internal/board/twinax.go`)

- **Arming:** a rising edge on 0x20009 bit7 arms the wrap. The next write
  to 0x20015 with bit6 set performs **one** transfer, using that written
  value as the mode, and disarms. Triggering on the 0x20015 write is
  required: D2 writes 0x58 *after* the strobe. The second 0x20015 write
  in D2 must **not** cause another transfer.
- **Channel:** ch0 if 0x20009 bit1 is set, else ch1.
- **Mode bit7 (test C):** 0x2002B = mem[(0x20020 + 0x20022 - 1) & 0xFFFF].
- **Otherwise:** build the message {addr = 0x20024 & 7, cmd = 0x20026,
  data = [0x20027], parityError = mode bit4}. Drop it if addr does not
  equal the channel's 0x20008 field. Run the channel's program for the
  class (§1.4 ISA). For prog2 set 0x2002B = the response status byte.
- **Status byte for prog2:**
  - Clear the parity latch first if the command has bit 0x40.
  - Clear "unacked" first if the command has bit 0x20.
  - status = +C | (unacked or status.bit1 ? 0x80 : 0) | (parity latch ? 0x40 : 0) | (toggle ? 0x20 : 0).
  - Toggle the toggle flip-flop on every poll response; then set
    unacked = 1 if the command had no 0x20 bit.
- **Resets:** writing 0x80 to 0x2000c clears unacked and toggle for that
  channel. A frame received with a parity error sets the parity latch.

Bits 7 and 5 are [G]: POST only proves "0 then 1 after a start" and
"toggles on every poll". The parity behaviour matches 5250 practice (the
controller acknowledges a parity error by sending Poll|0x40; the
reference controller does exactly this).

## 3. Runtime path

### 3.1 Initialisation (per session)

svc 0x1D → C000:7511 (ch0, DS = 0xF0); svc 0x1E → 75BE (ch1, DS = 0x290).
In order:

1. Set the far pointers `[0]` and `[4]`, and `[0x12e0]` = prog0 slot
   (0x80/0x100).
2. Copy the channel masks to DS:1310.
3. 7655: receive buffer DS:1010, length `[0x10d6]` = 0x10, filled with
   AF.
4. 781B:
   - +8 = station address, +5 = 0x10, +6/+7 = buffer, +C = 0x0E
     (exception 7, "power on"), +4 = 0, +F = 0, +B = 0xFF, +A, +9;
   - 0x20009 |= channel enable;
   - **0x2000c = 0x80**.

Elsewhere at start-up: svc 0x1A (C000:7424) installs the programs, svc
0x19 (74A2) installs the class table, and svc 0x1B (74B7) writes 0x20008.
C000:0988 sets 0x20009 |= 0x81 and the mask to 0x807F/0xFF7F. svc 0x1C
(C000:7509) is `cli; call [0x1204]; sti`, where `[0x1204]` = the session
init routine (7511/75BE). That is the 3477 counterpart of the 3486's
E000:2216 `call [0x1204]`.

### 3.2 Controller → terminal

A message is a command frame plus N data frames. The first frame carries
the station address; the last frame of a multi-frame message carries
address 7. The engine matches the first frame's address against 0x20008
and requires the channel's 0x20009 enable bit. It then runs the class
program:

- **Ordinary command (class 0)**:
  - The command byte and every data byte are appended at block+6/+7 and
    +5 is decremented. The pointer persists, so later messages in the
    same queue **append**. Status bit1 is set, which raises INT9.
  - The firmware (INT9 → 6FF2 → svc 0x20 command loop C000:7A06) reads
    bytes with 72E7 and 733A from its read pointer `[0x10d2]`. It compares
    against block+6 and **waits for more bytes** until EOQ (0x62:
    `cmd&0x1F` = 2, high bits 0x60 → `[0x11c1]|=0x40`).
  - It then re-arms in **71F8**:
    1. prog0 = 0x84;
    2. wait until chreg bit3 = 0;
    3. ack status bit1;
    4. +6 = buffer start low byte, +5 = 0x10;
    5. chreg = 0x80 (retry while it reads back bit7);
    6. prog0 = 0x56.
  - The buffer holds only 16 bytes. If the read pointer reaches the end,
    the firmware raises exception 8. Controllers must send queues of at
    most 16 bytes; the reference sends ≤ 10 characters per WDLC and an
    EOQ after each.
- **Write Activate (0x01, class 1)**: if +4 != 0, set +4 = 0 and DMA the
  data frames to +0/+1 (count +2/+3), write the pointer back, and set
  status bit2. The firmware arms this with mode 1, DX = DS:0010, CX =
  0x800 (C000:9751, 982E) for "write to display buffer" commands; 6E9A
  then computes the last offset.
- **Read Activate (0x00, class 3)**: if +4 != 0, set +4 = 0 and
  **transmit** +2/+3 bytes starting at +0/+1 (display RAM), write back
  +0/+1, and set bit2. Firmware handler 6E62: address counter `[0x1170]`
  = +0/+1 − DS*16 − 0x10. Read commands arm this with +4 = 2 (C000:8349,
  8423, 849D, ... 9A7C, ABE9).
- **Poll / Ack (class 2)**: respond with [status = +C | hardware bits,
  data = +D] and set +B = 0. Then **set status bit0**, which raises INT7.
  The INT7 handler delivers the next keystroke into +D (or 0) for the
  next poll. This is why the reference project recommends a slow poll
  (≥ 650 µs) for the 3477.

### 3.3 Command dispatcher

`[0x11c7]` = command byte, and `[0x11c7] & 0x1F` indexes a 32-word table
through the far pointer `[0x11ca]` (C000:7A38-7A4D). There are two
tables:

- **default** C000:A1CC, selected by svc 0x21 = 9CBC;
- **alternate** C000:A20C, selected by 9C42 when a command has the high
  bits 0x60/E0 or for command 0x09.

The high 3 bits are modifiers that each handler checks: 0x60 = EOQ form,
0x40 = "indicators" form (e.g. 0x51 = WDLC + indicators → 91D9).

| idx | 5250 command | A1CC (default) | A20C (alternate) |
|---|---|---|---|
| 00 | Read Activate (done by the engine) | 7B56 (invalid) | 7B56 |
| 01 | Write Activate (done by the engine) | 7B56 | 7B56 |
| 02 | Reset / **EOQ (0x62)** | 8D75 | 8D75 |
| 03 | Insert Character | 9DEB | 7C22 |
| 04 | Search Next Attribute | 9F88 | 8EE0 |
| 05 | Write Control Data (control byte → `[0x1207]`; bit 0x40: a second byte follows → `[0x1209]`, whose bit 0x08 selects 27x132, clear 24x80, C000:94A4-954A; see below) | 932F | 932F |
| 06 | Move Data | 9F65 | 8228 |
| 07 | Load Reference Counter | 7ECF | 7ECF |
| 08 | Read Data | 9901 | 8361 |
| 09 | (Read/Write display, switches table) | 9859 | 9859 |
| 0A | Read to End of Line | 99A3 | 8779 |
| 0B | ? | 8982 | 8982 |
| 0C | (Read Registers-like) | 82D7 | 82D7 |
| 0D | ? | 9FE8 | 8F7D |
| 0F | ? | 7EA8 | 7EA8 |
| 10 | Poll/Ack (done by the engine) | 7B56 | 7B56 |
| 11 | **Write Data & Load Cursor** [0x11][n][n chars] (n in 1..14; otherwise the single byte is one character) | 99E9 | 7EEF |
| 12 | Clear | 9D48 | 7A65 |
| 13 | **Set Mode** (data: bits 3-7 → +8; clears exception 7, sets 0x20009 bit3/4) | 8DA6 | 8DA6 |
| 14 | ? | A140 | 9160 |
| 15 | **Load Address Counter** (2 bytes) | 7E52 | 7E52 |
| 16 | ? | 9DBC | 7B61 |
| 17 | **Load Cursor** (2 bytes) | 7E72 | 7E72 |
| 18 | ? | 98F2 | 7A78 |
| 19 | Read Immediate | 9AE5 | 8591 |
| 1A | ? | 9B53 | 8660 |
| 1C | Read Registers | 7A8B | 7A8B |
| 1D | Write Immediate | 9BAC | 9777 |
| 1E | ? | 9BF1 | 96A4 |
| 0E, 1B, 1F | invalid | 7B56 | 7B56 |

Firmware services (far pointers at 0x0B00 + (n−1)*4, from the list at
C000:00EE, called via C000:71D2 with BP = n):

| svc | Target | Meaning |
|---|---|---|
| 0x11 | 70CE | get data word |
| 0x12 | 708B | set up data DMA: +0..+4 |
| 0x13 | 7052 | set exception → +C |
| 0x14 | 70DE | go offline |
| 0x17 | 72E7 | get next byte; exception 8 on overrun |
| 0x18 | 733A | get next command byte |
| 0x19 | 74A2 | class table |
| 0x1A | 7424 | programs |
| 0x1B | 74B7 | 0x20008 |
| 0x1C | 7509 | session init |
| 0x1D / 0x1E | 7511 / 75BE | session init per channel |
| 0x20 | 7A02 | command loop |
| 0x21 | 9C3C | default table |

## 4. Proposed emulator device

```
type twinaxChan struct {
    unacked, toggle, parity bool   // poll-response flip-flops
}
type Twinax struct {
    ch [2]twinaxChan
    armed bool; last09 uint8       // POST wrap trigger
    rxq  []Message                 // controller->station, queued by the bridge
    tx   func(ch int, frames []byte) // station->controller sink
}
```

**Register writes (IOGA):**

| Register | Action |
|---|---|
| 0x20000/1 | bit7 → reset unacked and toggle for that channel; mark the channel started |
| 0x20002/3 | `status &^= v` |
| 0x20004/5 | mask |
| 0x20008 | addresses |
| 0x20009 | store; a 0→1 edge on bit7 arms the wrap |
| 0x20015 | store; if bit6 is set and armed → wrap transfer (§2) |
| 0x20020-0x20028 | plain storage |

**Reads:**

| Register | Value |
|---|---|
| 0x20000/1 | 0 |
| 0x20002/3 | status (0x20003 bit7 = timer) |
| 0x2002B | wrap result |

**Interrupt:** INTR = `(status16 & mask16) != 0`. Ack() returns the vector
by the priority in §1.2 (6/7/8/9/10).

**Message injection** (bridge → device, between CPU steps):

1. Drop the message if 0x20009 bit7 or the channel enable bit is 0, or
   the address does not match.
2. cls = mem[0x60 + (cmd & 0x1F)] >> 6; prog = 0x80 + 0x80*ch + 0x20*cls.
3. Run the micro-interpreter (§1.4). This automatically honours 0x84
   (disabled) slots.

| Class | Data path | Status bit | Output to the bridge |
|---|---|---|---|
| 0 | +6/+7 receive buffer | bit1 | none |
| 1 | +0/+1 | bit2 | none |
| 2 | — | bit0 | `[status, +D]` |
| 3 | +0/+1 | bit2 | the data bytes |

Pace the injection like real time: about 20 µs per frame at 1 Mbit/s;
poll no faster than about 1 ms of emulated time. A message that arrives
while status bit1 is pending is appended (it belongs to the same queue).
The controller should not start a new queue while the station reports
busy.

**Recommended runtime poll status** (our own wire format):

| Bit | Value |
|---|---|
| 0x80 busy | status bit1 pending (or "unacked" for POST) |
| 0x40 | parity latch |
| 0x10 outstanding | +D holds new data (firmware wrote 0x10 to 0x20000) [L] |
| 0x0E | +C |
| 0x01 response level | toggles whenever a response carries +D != 0 [G] |

The bridge (controller) should send Ack (0x30) after each response, like
the reference does. A bring-up sequence that works with this firmware:
Poll → status has exception 7 → `[13 00]`, `[62]`, `[05 ctl]`, `[62]` →
then WDLC as `[11 n c1..cn]`, `[62]`.

**Open points:**

- the real meaning of 0x2002B bits 7 and 5, and of 0x20024 bit5 and
  0x20025;
- the ch1 prog2 quirk (writes h8 instead of h7);
- the line error bits 3-6 (never raise them);
- the 0x20000 read bits;
- the exact on-wire format of the read and poll responses (only our own
  bridge consumes it, so it does not matter).

## 5. The workstation controller (internal/controller)

The controller is the other end of the twinax cable. It plays the role of
a 5494 or an AS/400 workstation controller between a TN5250 host session
and the emulated station:

- **Polling:** it polls about every 1 ms of emulated time and acknowledges
  every response (`Poll 0x10`, then `Ack 0x30`).
- **Finding the station:** with no address configured it polls addresses
  0–6 in turn until one answers. After 500 missed polls in a row (the
  address may have been changed in Setup) it searches again and repaints.
- **Bring-up:** on exception 7 (power on) it sends `[13 00][62]` (Set
  Mode, zero fill) and `[05 04][62]` (Write Control Data, reset
  exception). Before every full repaint it sets the screen size with
  `[05 44 08][62]` (27x132) or `[05 44 00][62]` (24x80), then clears the
  whole screen: `[15 0000][07 last][12][62]`. Without the size command a
  132-column address sent to a station in 24x80 mode raises exception 6
  (status 0x0C).
- **Other exceptions** (status 0x02-0x0C): while one is pending the
  firmware drops every command except Write Control Data with
  reset-exception (C000:935A-9374). The controller sends `[05 04][62]`
  and repaints; if the exception keeps coming back before a clean
  repaint, it waits 1, 2, 4, … 2048 polls before the next reset.
- **Screen mirror:** the host's presentation space (EBCDIC plus inline
  attribute bytes 0x20–0x3F) is diffed against what the station already
  shows. Changed runs go out as `[15 addr][11 n data...][62]`, with
  n ≤ 10 so a queue fits the 16-byte receive buffer. The cursor goes out
  as `[17 addr][62]`. An address is `row × cols + col`, big-endian
  (inmbolmie `getEncodedPosition`).
- **Flow control:** a queue is sent only when the poll status is not busy
  (bit 0x80).
  - After End of Queue the station stays busy until the firmware re-arms
    its receiver. It does that by writing 0x80 to the channel register in
    its receive set-up (`C000:71F8`, write at `C000:726D`), once the queue
    is executed.
  - The status bit alone is not enough: a Load Cursor queue is still being
    executed after status bit 1 is cleared. A queue sent in that window
    was lost, and the next Write Data then landed at a stale address
    (typed letters showed up in protected text, e.g. "Generwl").
  - It also reports busy for one more poll after the re-arm. The firmware
    is still finishing its set-up (`C000:7275` on), and a queue arriving
    in the same instant was dropped with the station busy for good.
  - **Confirmed** by `TestFirmwareFastTypingKeepsScreen`: fast typing and
    host rewrites against the real firmware, compared cell by cell.
- **Typeahead:** keys the terminal queued while no controller polled (up
  to 7) are dropped until the first repaint of the host screen. They are
  not typed into the new host session. Shift and Caps Lock state is
  still tracked.
- **Keys: new data only once.** The INT7 handler stores the next key in
  +D through `C000:6B12`, which then writes 0x10 to the channel
  register (0x20000/1). With an empty key queue it writes nothing and
  the old key stays in +D. So +D is new data only after that write, and
  it is reported in one poll response, until the controller's Ack. The
  real station signals this with the response-level bit, which inmbolmie's
  converter checks. **Confirmed**: reading +D on every poll repeated a key
  hundreds of times.
- **Keys: the code set.** The firmware does not pass the keyboard's set-3
  codes on. It translates each key into the **5250 twinax keyboard code**
  of the 122-key keyboard: `D000:01F5` stores `ss:[0x305]` into the send
  buffer `0x160A`, and the translation is dispatch function 0x7C
  (`D000:0002`). The full table, recorded by pressing every key on the
  emulated 3477, is `kbdlayout.ToTwinax`. It matches IBM's table, *5250
  Information Display System to S/36, S/38, AS/400 Product Attachment
  Information* (1989), figures 103 and 105, and inmbolmie's `122KEY_EN`.
  Examples:
  - a = 11, 1 = 31, Space = 0F, Field Exit = 2D, Enter (right Ctrl) = 68,
    Reset (left Ctrl) = 7E.
  - Arrows: Up 71, Left 72, Right 73, Down 70.
  - Shift: 57/56 make, D7/D6 break. Caps Lock: 54 make, D4 break.
  - F1-F12 = Cmd prefix 6F + 31..3C.
  - F13-F24 = 6F 57 31..3C: the terminal sends Shift make itself, then
    Shift break (D7) before the next key.
  - With Shift held, F1 sends D7 6F 31: Shift+F1 is PF1, not PF13.
  - The key at set-3 6D sends 57 6C (Shift + Erase EOF = Erase Input).
  - With Shift held, set-3 65 sends D7 6C.
- **Keyboard modes.** After reset the firmware sends `FF F6`, then
  `FD 5A 58 11 7C` (make only), `FB` + the keypad keys (typematic), then
  `FA 61 6A F4` (ROM table `D000:28BE`). `F6` must give the set-3
  defaults: typematic make-only for every key, make/break only for
  Ctrl, Shift, Alt and Caps Lock. Any other break is forwarded to the
  controller as the same code again, a second keystroke. `FA` + keys is
  treated as typematic, as IBM's table lists Left/Right as "T".
- **Keyboard command replies.** The firmware sends the keyboard an Echo
  (EE) about every 5 s. The reply must come before key codes that are
  already waiting, as a real keyboard answers a command first. With a key
  code in its place the firmware took the keyboard for broken: it reset
  and reprogrammed it (FF ...), and the key was lost. **Confirmed**,
  `TestKeyboardEchoDuringTyping`.
- The controller (`internal/controller`, `key`) decodes these codes with
  `kbdlayout.DecodeTwinax` and passes them to the host session as typing
  (`tn5250.Client.TypeChar`) or as a function (`Key`). Field rules, insert
  mode and AID records are applied there, as the real controller does.

`internal/controller/keys_firmware_test.go` runs the controller against
the real 3477 firmware. It checks every key's twinax code and that every
5250 function and character reaches the host once.
