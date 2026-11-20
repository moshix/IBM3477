#!/bin/sh
# Download the SingleStepTests 8088 v2 suite (hardware-captured, ~760 MB)
# into internal/cpu/testdata/8088/ for the CPU core tests.  Re-running
# skips files that are already present.
set -eu
BRANCH=${BRANCH:-main}
BASE="https://raw.githubusercontent.com/SingleStepTests/8088/$BRANCH/v2"
DEST="$(cd "$(dirname "$0")/.." && pwd)/internal/cpu/testdata/8088"
mkdir -p "$DEST"
[ -s "$DEST/metadata.json" ] || curl -fsSL -o "$DEST/metadata.json" "$BASE/metadata.json"
# Test file names come from metadata.json: plain opcodes ("00") map to
# 00.json.gz; group opcodes carry a "reg" table and map to one file per
# ModRM reg value ("80.0" .. "80.7").  Prefix bytes have no test file.
KEYS=$(python3 - "$DEST/metadata.json" <<'PY'
import json, sys
ops = json.load(open(sys.argv[1]))["opcodes"]
out = []
for k, v in ops.items():
    if "reg" in v:
        out += [f"{k}.{r}" for r in v["reg"]]
    elif v.get("status") != "prefix":
        out.append(k)
print(" ".join(out))
PY
)
for k in $KEYS; do
  f="$k.json.gz"
  [ -s "$DEST/$f" ] && continue
  curl -fsSL -o "$DEST/$f.part" "$BASE/$f" && mv "$DEST/$f.part" "$DEST/$f" \
    || echo "missing: $f" >&2
done
echo "done: $(ls "$DEST" | grep -c 'json.gz$') test files in $DEST"
