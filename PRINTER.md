# IBM 3477: the printer port and printing

Confidence levels as in the other notes: **confirmed** means seen in the
code and reproduced in the emulator, **likely** means strong code
evidence, **guess** means plausible but not verified.

## 1. The printer port (gate array 0x20030-0x20031), confirmed

The printer code lives in D000 (the second-session code, used in
Display-Printer mode). It reaches the port through two far pointers:
`cs:[0xDE04]` = `2003:0000` and `cs:[0xDE08]` = `2003:0001`.

| Address | Dir | Use | Code |
|---|---|---|---|
| 0x20030 | W | data byte for the printer | `D000:DA96` |
| 0x20031 | W | control: bit 0 = strobe. For each byte the firmware writes the control value with bit 0 cleared, then with bit 0 set; the other bits keep their value (`[0x186]`) | `D000:DAA6`-`DAB5` |
| 0x20031 | R | status: read twice until both reads agree; the low 4 bits select one of 16 handlers (table `D000:DE0E`) | `D000:D723` |

- **Status 3 = printer on line and ready (confirmed).** With status 3 the
  firmware initialises the printer at every start and sends it data. With
  any other value it sends one byte and stops.
- **The emulator** (`board/printer.go`) reports status 3, so a printer is
  always attached and ready, and it collects every strobed byte.

## 2. What the firmware sends: IBM Proprinter / PPDS, confirmed

At every start (One-Display mode too) the firmware sends 624 set-up bytes
and no text:

| Bytes | Meaning |
|---|---|
| `ESC [ T 05 00 00 00 03 52 00` | select code page 850 (0x0352) |
| `ESC A 0C`, `ESC 2` | line spacing 12/72" = 6 lines per inch |
| `ESC [ @ ...` | double height off |
| `ESC W 00`, `DC2`, `ESC I 00` | single width, 10 characters per inch, draft quality |
| `ESC C 01`, `ESC 6` | page length set-up, character set 2 |
| `ESC = n1 n2 data` | download of 11-byte character matrices (national characters) |

`internal/printer` interprets this command set and renders print jobs as
PDF and text. A stream without visible text, such as this set-up, is not
a print job.

## 3. When the 3477 prints

- **Terminal Mode = Display-Printer** (Setup; setup memory `0x1A6` = 3,
  printer address at `0x1A4`/`0x1A5` and `0x1AA`; see NVRAM.md). The
  printer is a second twinax station at the Printer Address.
- **The terminal never makes a local copy by itself (confirmed).** Its
  Print key goes to the controller as the 5250 Print key. In tests with
  every setup byte `0x1C0`-`0x1DD` set to 1 and to 2 (48 boots), with
  emulation 5256 and 4214, it always went to the host. Setup Printer's
  Print key and Trim border lines are shown dimmed with emulation 5256.
- **Print data reaches the printer through the printer station.** The
  controller (or the host) sends SCS print data to the station at the
  Printer Address; the firmware formats it with the Setup Printer choices
  (lines and characters per inch, quality, character height) and drives
  the port.

Setup Printer choices found in the setup memory so far (likely):
`0x1CE` = lines per inch, `0x1D2` = character height, `0x1CD` and
`0x1D0` change with the choices on the first lines.

### 3.1 The twinax printer protocol as the 3477 implements it, confirmed

From IBM's *5250 Information Display System to S/36, S/38 and AS/400
System Units Product Attachment Information* (1989), chapter 3 (printed
pages 85-153), checked against the firmware (`controller/printer.go`,
`TestFirmwarePrinterStationLongJob`):

1. **Bring-up.** The first Poll reports exception 7 (power-on
   transition, status `0E`). Send Set Mode (`0x13`, data `00`), **then
   End of Queue (`0x62`)**: the 3477 processes a station's command queue
   only at its end, like the display's. Other exceptions: WCD (`0x05`,
   data `04`) + End of Queue.
2. **Print a block.** When a Poll shows not busy, no exception and frame 2
   bit `0x20` (receive buffers full) off: Write Data (`0x1E`, ends its
   queue). When not busy again: Activate Write (`0x01`) with up to 256
   SCS data bytes.
3. **Busy.** After Write Data and after Activate Write the station is busy
   until the firmware re-arms its receiver (as after End of Queue): about
   50 ms per 256-byte block while it formats the data. Without that, the
   next block overwrote the previous one and print data was lost.
4. **Frame 2** (receive buffers full `0x20`, print complete `0x10`,
   cancel request `0x08`, ...) is the control block's `+D` (`0x19D` for
   channel 1). Cancel request is answered with Clear (`0x12`).
5. **Data.** SCS: EBCDIC text, New Line `15`, Form Feed `0C`, Carriage
   Return `0D`, Line Feed `25`.

### 3.2 The Print key in the emulator

In Display-Printer mode the machine (`emu/print.go`) attaches the display
controller at the setup's display address and a printer station driver
at its printer address. Settings › Print key chooses:
- **Local copy** (default): the controller turns the host screen into SCS
  (one line per row, then a form feed; attributes and non-display fields
  such as passwords print as blanks) and sends it to the printer station.
  The 3477 prints it as it would any print job.
- **Send to host:** the Print key goes to the host, which prints the screen
  on its own printer device.

### 3.3 Planned: host printing (stage 4)

The host sends spooled files to a printer device. The plan is a second
TN5250 session as a printer device (TN5250E printer session) whose SCS
data goes to the printer station through the same driver, so AS/400
output prints through the 3477 with its Setup Printer choices.

## 4. Output in the emulator

Finished jobs (the port quiet for 2 s, `emu/print.go`) go to the browser,
which offers:
- **Save as PDF / text:** the browser's Save dialog where available
  (Chrome, Brave), else a download.
- **Print on:** one of this computer's CUPS printers (macOS, Linux), with
  `lp`. The name must be one `lpstat` lists. Not offered by a server
  started with `-lock`.
- **Open PDF to print:** a new tab with the PDF, for the browser's own
  print dialog.

Settings › Print jobs (or "Do this every time") makes one of these
automatic.
