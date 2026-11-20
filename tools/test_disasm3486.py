"""Tests for the ROM images and disassembly listings (3486 and 3477).

Usage:
  test_disasm3486.py --model 3486 --listing 3486.listing \
                     --image 3486_partial_ABEF.bin 3486.bin
  test_disasm3486.py --model 3477 --listing 3477.listing \
                     ibm3477_part2.bin ibm3477_part1.bin
"""
import argparse
import re
import struct
import sys

import capstone

BLOCK = 0x10000
LINE = re.compile(r"^([0-9A-F]{4}):([0-9A-F]{4})  (\S+)\s+(.*)$")
SEGS = re.compile(r"^; image : 256K, segments ((?:[0-9A-F]{4} ?){4})")
# 80186+ opcodes; the firmware is pure 8086 (see disasm3486.OPCODES_186)
OPCODES_186 = {0x60, 0x61, 0x62, 0x68, 0x69, 0x6A, 0x6B, 0x6C, 0x6D, 0x6E,
               0x6F, 0xC0, 0xC1, 0xC8, 0xC9}
PREFIXES = {0x26, 0x2E, 0x36, 0x3E, 0xF0, 0xF2, 0xF3}


def check(cond, msg):
    if not cond:
        sys.exit(f"FAIL: {msg}")
    print(f"ok  : {msg}")


def word_sum(data):
    return sum(struct.unpack(f"<{len(data) // 2}H", data)) & 0xFFFF


def image_3486(args):
    """Rebuild the 3486 image from the dump and check the A17-high layout."""
    rom = open(args.roms[0], "rb").read()
    check(len(rom) == 8 * BLOCK, "3486 dump is 512K")
    blk = [rom[i * BLOCK:(i + 1) * BLOCK] for i in range(8)]
    # A17 stuck high: block i reads the same chip data as block i^2
    check(all(blk[i] == blk[i ^ 2] for i in range(8)),
          "3486 dump repeats with A17 stuck high")
    check(word_sum(rom) != 0x619A,
          "3486 dump fails ROM self-check 0x619A (known partial dump)")
    img = blk[0] + blk[1] + blk[4] + blk[5]
    if args.image:
        check(open(args.image, "rb").read() == img,
              "image file equals dump blocks 0,1,4,5 (A000 B000 E000 F000)")
    return img


def image_3477(args):
    """Concatenate part2 (C000/D000) + part1 (E000/F000), verify sums."""
    img = open(args.roms[0], "rb").read() + open(args.roms[1], "rb").read()
    check(len(img) == 4 * BLOCK, "3477 ROM pair is 256K")
    sums = [word_sum(img[i * BLOCK:(i + 1) * BLOCK]) for i in range(4)]
    check(sums == [0x1234, 0x5678, 0x9ABC, 0xDEF0],
          "3477 segment word sums are 1234 5678 9ABC DEF0")
    return img


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True, choices=["3486", "3477"])
    ap.add_argument("--listing", required=True)
    ap.add_argument("--image")
    ap.add_argument("roms", nargs="+")
    args = ap.parse_args()
    img = image_3486(args) if args.model == "3486" else image_3477(args)
    lst = open(args.listing).read().splitlines()

    # Segment layout comes from the listing header
    segs = None
    for ln in lst:
        m = SEGS.match(ln)
        if m:
            segs = [int(x, 16) for x in m[1].split()]
            break
    want = {"3486": [0xA000, 0xB000, 0xE000, 0xF000],
            "3477": [0xC000, 0xD000, 0xE000, 0xF000]}[args.model]
    check(segs == want, "listing segment layout " +
          " ".join(f"{s:04X}" for s in want))
    seg_to_block = {s: i for i, s in enumerate(segs)}

    # Listing lines are contiguous, cover every byte exactly once, match
    # the image, and every code line re-decodes to the same instruction
    md = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_16)
    pos = 0
    n_code = 0
    n_186 = 0
    for ln in lst:
        m = LINE.match(ln)
        if not m:
            continue
        seg, off, raw, text = (int(m[1], 16), int(m[2], 16), m[3], m[4])
        addr = seg_to_block[seg] * BLOCK + off
        if addr != pos:
            sys.exit(f"FAIL: gap/overlap at {ln!r} (expected {pos:05X})")
        if raw.startswith("CC*"):
            data = b"\xCC" * int(raw[3:], 16)
        else:
            data = bytes.fromhex(raw)
        if img[addr:addr + len(data)] != data:
            sys.exit(f"FAIL: bytes differ from image at {ln!r}")
        if not text.startswith("db "):
            ins = list(md.disasm(data, off))
            # Mnemonic may carry a prefix ("rep movsb"), so match the
            # full mnemonic as the leading text
            if len(ins) != 1 or ins[0].size != len(data) \
                    or not text.startswith(ins[0].mnemonic):
                sys.exit(f"FAIL: code line does not re-decode: {ln!r}")
            op = next((b for b in data if b not in PREFIXES), data[-1])
            n_186 += op in OPCODES_186
            n_code += 1
        pos += len(data)
    check(pos == len(img), f"listing covers all {len(img)} bytes once")
    check(n_code > 0, f"{n_code} code lines re-decode identically")
    check(n_186 == 0, "no 80186+ opcodes in code (firmware is pure 8086)")

    reset = [ln for ln in lst if ln.startswith("F000:FFF0")]
    check(len(reset) == 1 and "ljmp 0xf000:0" in reset[0],
          "reset vector F000:FFF0 is 'ljmp 0xf000:0'")


if __name__ == "__main__":
    main()
