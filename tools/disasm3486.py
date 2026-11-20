"""Disassemble IBM InfoWindow terminal ROMs (3486 or 3477).

Both ROM sets are laid out as four 64K segments (a 256K image):

3486  3486.bin is a 512K dump read as a 27C040 with the chip's A17 input
      stuck HIGH, so every 64K block appears twice (A B A B C D C D) and
      what we hold is really segments A000/B000/E000/F000.  Segments
      8000/9000/C000/D000 are MISSING; far references into them are not
      followed.  The ROM's own check (word-sum of 80000-FFFFF == 0x619A,
      table at F000:2B97) therefore cannot pass on this dump.
3477  ibm3477_part2.bin = C0000-DFFFF, ibm3477_part1.bin = E0000-FFFFF.
      Complete: each 64K segment word-sums to 1234/5678/9ABC/DEF0.

Usage:
  disasm3486.py --model 3486 --listing 3486.listing \
                --image-out 3486_partial_ABEF.bin 3486.bin
  disasm3486.py --model 3477 --listing 3477.listing \
                ibm3477_part2.bin ibm3477_part1.bin
"""
import argparse
import struct
import sys

import capstone
from capstone import x86 as cx

BLOCK = 0x10000
# Segment held in each of the four image blocks; set by configure().
BLOCK_SEG = []
# Expected per-segment word sums (3477 only), image block -> sum
SEG_SUMS = {"3477": [0x1234, 0x5678, 0x9ABC, 0xDEF0]}


def configure(model):
    """Select the segment layout of the 256K image for `model`."""
    global BLOCK_SEG
    BLOCK_SEG = {"3486": [0xA000, 0xB000, 0xE000, 0xF000],
                 "3477": [0xC000, 0xD000, 0xE000, 0xF000]}[model]


def word_sum(block):
    """16-bit little-endian word sum, as the ROM's own check computes it."""
    return sum(struct.unpack(f"<{len(block) // 2}H", block)) & 0xFFFF


def load_image(model, paths):
    """Build the 256K image (four 64K segments in BLOCK_SEG order)."""
    if model == "3486":
        rom = open(paths[0], "rb").read()
        if len(rom) != 8 * BLOCK:
            sys.exit("3486: expected the 512K dump")
        blk = [rom[i * BLOCK:(i + 1) * BLOCK] for i in range(8)]
        # A17 stuck high: block i and block i^2 must read identically
        for a in (0, 1, 4, 5):
            if blk[a] != blk[a ^ 2]:
                sys.exit(f"block {a} != block {a ^ 2}: not the known dump")
        return b"".join(blk[i] for i in (0, 1, 4, 5))
    lo = open(paths[0], "rb").read()        # part2: C000/D000
    hi = open(paths[1], "rb").read()        # part1: E000/F000
    if len(lo) != 2 * BLOCK or len(hi) != 2 * BLOCK:
        sys.exit("3477: expected two 128K ROM halves (part2, part1)")
    img = lo + hi
    for i, want in enumerate(SEG_SUMS["3477"]):
        got = word_sum(img[i * BLOCK:(i + 1) * BLOCK])
        if got != want:
            sys.exit(f"3477 segment {BLOCK_SEG[i]:04X}: word sum {got:04X}"
                     f" != {want:04X} (bad dump or wrong part order)")
    return img


def phys_to_image(phys):
    """Map a physical address to an image offset; None if the segment is
    outside the image (RAM, I/O, or a missing 3486 ROM segment)."""
    seg = (phys >> 16) << 12
    if seg not in BLOCK_SEG:
        return None
    return BLOCK_SEG.index(seg) * BLOCK + (phys & 0xFFFF)


# Instructions after which execution does not fall through.  HLT is not
# one: this firmware halts waiting for an interrupt and then continues.
STOP_MNEMONICS = {"ret", "retf", "iret", "jmp", "ljmp", "int3"}

# Mnemonics that are implausible in this firmware; a heuristic code
# candidate containing any of them is rejected as data.
SUSPECT = {"arpl", "lar", "lsl", "clts", "lgdt", "lidt", "sgdt", "sidt",
           "lmsw", "smsw", "ltr", "str", "lldt", "sldt", "verr", "verw",
           "bound", "into", "int1", "salc", "aaa", "aas", "daa", "das",
           "aam", "aad", "lock", "wait", "esc"}

# Opcodes that only exist on the 80186 and later.  Code reached from the
# reset vector never uses them (it is pure 8086), so a heuristic candidate
# containing one is data: pusha/popa/bound (60-62), push imm (68/6A),
# imul imm (69/6B), ins/outs (6C-6F), shift imm (C0/C1), enter/leave (C8/C9).
OPCODES_186 = {0x60, 0x61, 0x62, 0x68, 0x69, 0x6A, 0x6B, 0x6C, 0x6D, 0x6E,
               0x6F, 0xC0, 0xC1, 0xC8, 0xC9}
PREFIXES = {0x26, 0x2E, 0x36, 0x3E, 0xF0, 0xF2, 0xF3}


def opcode_byte(raw):
    """First byte of an instruction after any segment/rep/lock prefixes."""
    for b in raw:
        if b not in PREFIXES:
            return b
    return raw[-1]


class Disassembler:
    def __init__(self, img):
        self.img = img
        self.md = capstone.Cs(capstone.CS_ARCH_X86, capstone.CS_MODE_16)
        self.md.detail = True
        self.insn = {}          # image offset -> capstone instruction
        self.owner = {}         # image offset of every code byte -> insn start
        self.labels = {}        # image offset -> "sub" | "loc"
        self.xref_note = {}     # insn offset -> comment text

    def decode_at(self, off):
        """Decode one instruction at image offset `off` (within its block)."""
        blk_end = (off // BLOCK + 1) * BLOCK
        # Pass the in-segment offset as the address so capstone computes
        # near branch targets relative to CS.
        for ins in self.md.disasm(self.img[off:blk_end], off % BLOCK, 1):
            return ins
        return None

    def branch_target(self, off, ins):
        """Return (image_target, kind) for direct branches/calls, else None."""
        blk = off // BLOCK
        raw = bytes(ins.bytes)
        # Far direct call/jmp: 9A/EA off16 seg16 (no prefixes expected)
        if raw[0] in (0x9A, 0xEA) and len(raw) == 5:
            o, s = struct.unpack_from("<HH", raw, 1)
            tgt = phys_to_image(s * 16 + o)
            kind = "sub" if raw[0] == 0x9A else "loc"
            return (tgt, kind) if tgt is not None else None
        is_call = capstone.CS_GRP_CALL in ins.groups
        is_jump = capstone.CS_GRP_JUMP in ins.groups
        if not (is_call or is_jump):
            return None
        ops = ins.operands
        if len(ops) == 1 and ops[0].type == cx.X86_OP_IMM:
            # Near target wraps within the 64K segment
            tgt = blk * BLOCK + (ops[0].imm & 0xFFFF)
            return tgt, ("sub" if is_call else "loc")
        return None

    def trace(self, seeds, kind="sub"):
        """Recursive-descent from `seeds`; returns number of new insns."""
        before = len(self.insn)
        work = [(s, kind) for s in seeds]
        while work:
            off, k = work.pop()
            # Never let a weaker label kind overwrite "sub"
            # kind None = seed without a label (live-execution seeds)
            if k and self.labels.get(off) != "sub":
                self.labels[off] = k
            while True:
                if off in self.insn:
                    break                       # already traced from here
                if off in self.owner:
                    break                       # lands mid-instruction
                ins = self.decode_at(off)
                if ins is None:
                    break
                span = range(off, off + ins.size)
                if any(b in self.owner for b in span):
                    break                       # overlaps existing code
                self.insn[off] = ins
                for b in span:
                    self.owner[b] = off
                bt = self.branch_target(off, ins)
                if bt:
                    work.append(bt)
                    self.xref_note[off] = self.label_name(bt[0], bt[1])
                if ins.mnemonic in STOP_MNEMONICS:
                    break
                nxt = off + ins.size
                if nxt % BLOCK == 0:
                    break                       # don't run off a segment
                off = nxt
        return len(self.insn) - before

    def trial(self, seed, max_insns=4000):
        """Speculatively trace from `seed` WITHOUT committing anything.

        Returns the dict {offset: insn} of the newly reachable code if it
        looks like genuine code, else None.  Genuine means: every path
        decodes, never lands inside an existing or trial instruction,
        contains no SUSPECT mnemonic and no '00 00' (add [bx+si],al --
        the classic sign of decoding zero-filled data), and every path
        ends in a terminator or joins already-known code."""
        new = {}
        owner = {}
        work = [seed]
        while work:
            off = work.pop()
            while True:
                if off in self.insn or off in new:
                    break                       # joined known/trial code
                if off in self.owner or off in owner:
                    return None                 # mid-instruction landing
                if self.img[off] == 0xCC:
                    return None                 # ran into int3 fill
                ins = self.decode_at(off)
                if ins is None or ins.mnemonic in SUSPECT \
                        or ins.mnemonic.startswith("f"):
                    return None
                raw = bytes(ins.bytes)
                if raw[:2] == b"\x00\x00" or opcode_byte(raw) in OPCODES_186:
                    return None
                span = range(off, off + ins.size)
                if any(b in self.owner or b in owner for b in span):
                    return None
                new[off] = ins
                for b in span:
                    owner[b] = off
                if len(new) > max_insns:
                    return None
                bt = self.branch_target(off, ins)
                if bt:
                    work.append(bt[0])
                if ins.mnemonic in STOP_MNEMONICS:
                    break
                off += ins.size
                if off % BLOCK == 0:
                    return None                 # fell off the segment
        return new

    def gap_seeds(self):
        """Candidate entry points: the first byte after a traced
        unconditional terminator, or after an int3 fill run, that is not
        itself code or fill."""
        seeds = []
        for off, ins in self.insn.items():
            if ins.mnemonic in STOP_MNEMONICS:
                nxt = off + ins.size
                if nxt % BLOCK and nxt not in self.owner \
                        and self.img[nxt] != 0xCC:
                    seeds.append(nxt)
        img = self.img
        for i in range(1, len(img)):
            if img[i - 1] == 0xCC and img[i] != 0xCC and i % BLOCK \
                    and i not in self.owner:
                seeds.append(i)
        return sorted(set(seeds))

    def fill_gaps(self):
        """Repeatedly accept validated gap candidates until none remain.
        Accepted entries get the label kind 'ent' (heuristic entry)."""
        total = 0
        while True:
            got = 0
            for seed in self.gap_seeds():
                if seed in self.owner:
                    continue
                cand = self.trial(seed)
                if cand and len(cand) >= 3:
                    got += self.trace([seed], "ent")
            total += got
            if not got:
                return total

    def label_name(self, off, kind):
        seg = BLOCK_SEG[off // BLOCK]
        return f"{kind}_{seg:04X}_{off % BLOCK:04X}"

    def entry_idiom_seeds(self):
        """Routines in this ROM start with EB 00 (jmp $+2).  Seed every
        EB 00 that follows a terminator byte (ret/retf/iret/int3 fill)
        and is not already part of traced code."""
        term = {0xC3, 0xCB, 0xCF, 0xCC}
        seeds = []
        for i in range(1, len(self.img) - 1):
            if (self.img[i] == 0xEB and self.img[i + 1] == 0x00
                    and self.img[i - 1] in term and i not in self.owner):
                seeds.append(i)
        return seeds


def fmt_line(off, raw, text):
    seg = BLOCK_SEG[off // BLOCK]
    return f"{seg:04X}:{off % BLOCK:04X}  {raw:<20}  {text}"


def emit_data(lines, img, start, end):
    """Emit non-code bytes [start, end) as db lines.  Runs of 16+ 0xCC
    fill bytes collapse into one dup line; printable ASCII runs of 6+
    are shown as quoted strings; everything else 8 bytes per line."""
    i = start
    while i < end:
        j = i
        while j < end and img[j] == 0xCC:
            j += 1
        if j - i >= 16:
            lines.append(fmt_line(i, f"CC*{j - i:04X}",
                                  f"db {j - i:#x} dup (0CCh)"))
            i = j
            continue
        j = i
        while j < end and j - i < 8 and 0x20 <= img[j] < 0x7F \
                and img[j] != 0x27:
            j += 1
        if j - i >= 6:
            s = img[i:j].decode("ascii")
            lines.append(fmt_line(i, img[i:j].hex().upper(), f"db '{s}'"))
            i = j
            continue
        j = min(end, i + 8)
        # Stop a hex row early if a fill run or string begins inside it
        for k in range(i + 1, j):
            if img[k:k + 16] == b"\xCC" * 16:
                j = k
                break
        chunk = img[i:j]
        lines.append(fmt_line(i, chunk.hex().upper(),
                              "db " + ",".join(f"{b:02X}h" for b in chunk)))
        i = j


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--model", required=True, choices=["3486", "3477"])
    ap.add_argument("--listing", required=True)
    ap.add_argument("--image-out", help="also write the 256K image here")
    ap.add_argument("--seeds", help="file of SSSS:OOOO addresses known to "
                    "be executed (e.g. from cmd/probe -seeds); traced first")
    ap.add_argument("roms", nargs="+")
    args = ap.parse_args()
    configure(args.model)
    rom_path = " + ".join(args.roms)
    img = load_image(args.model, args.roms)
    if args.image_out:
        open(args.image_out, "wb").write(img)
    lst_path = args.listing

    d = Disassembler(img)
    reset = phys_to_image(0xFFFF0)
    n1 = d.trace([reset])
    # Addresses observed executing in the emulator are trusted code
    if args.seeds:
        live = []
        for ln in open(args.seeds):
            sg, _, of = ln.strip().partition(":")
            if of:
                t = phys_to_image((int(sg, 16) << 4) + int(of, 16))
                if t is not None:
                    live.append(t)
        n1 += d.trace(sorted(set(live)), None)
    # Iterate the entry-idiom seeding until no new code is found
    n2 = 0
    while True:
        seeds = d.entry_idiom_seeds()
        got = d.trace(seeds)
        n2 += got
        if not got:
            break
    n3 = d.fill_gaps()
    code_bytes = len(d.owner)

    lines = [
        f"; IBM {args.model} ROM -- 16-bit x86 real-mode disassembly",
        f"; source: {rom_path}",
        f"; image : 256K, segments "
        + " ".join(f"{sg:04X}" for sg in BLOCK_SEG)
        + " (image 0x00000/10000/20000/30000)",
        "; reset : F000:FFF0",
        *(["; NOTE  : partial dump (A17 stuck high); 8000/9000/C000/D000",
           ";   are missing, far refs into them are not followed."]
          if args.model == "3486" else []),
        "; method: recursive descent from the reset vector, then from",
        ";   'EB 00' routine entries, then validated heuristic entries",
        ";   (labelled ent_*) after ret/jmp or int3 fill.",
        ";   Untraced bytes are shown as db.",
        ";   Code reached only via indirect jumps/calls may appear as db.",
        f"; stats : {n1} insns from reset, {n2} from entry idiom, "
        f"{n3} heuristic,",
        f";   {code_bytes} code bytes of {len(img)}",
        ";",
        "; addr       bytes                 instruction",
    ]
    off = 0
    data_start = None
    while off < len(img):
        if off % BLOCK == 0:
            if data_start is not None:
                emit_data(lines, img, data_start, off)
                data_start = None
            lines.append("")
            lines.append(f"; ===== segment {BLOCK_SEG[off // BLOCK]:04X} "
                         f"(image {off:05X}) =====")
        if off in d.insn:
            if data_start is not None:
                emit_data(lines, img, data_start, off)
                data_start = None
            ins = d.insn[off]
            if off in d.labels:
                lines.append("")
                lines.append(d.label_name(off, d.labels[off]) + ":")
            text = f"{ins.mnemonic} {ins.op_str}".rstrip()
            if off in d.xref_note:
                text = f"{text:<40} ; {d.xref_note[off]}"
            lines.append(fmt_line(off, bytes(ins.bytes).hex().upper(),
                                  text))
            off += ins.size
        else:
            if data_start is None:
                data_start = off
            off += 1
    if data_start is not None:
        emit_data(lines, img, data_start, off)
    open(lst_path, "w").write("\n".join(lines) + "\n")
    print(f"reset insns={n1} idiom insns={n2} heur insns={n3} "
          f"code={code_bytes}/{len(img)} ({100 * code_bytes / len(img):.1f}%)"
          f" lines={len(lines)}")


if __name__ == "__main__":
    main()
