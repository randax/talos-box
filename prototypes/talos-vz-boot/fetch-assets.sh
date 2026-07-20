#!/bin/sh
# PROTOTYPE — fetch Talos v1.13.6 boot assets (vanilla schematic) into $ASSETS (default: ./assets)
set -eu
SCHEMATIC=376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba
VERSION=v1.13.6
BASE="https://factory.talos.dev/image/$SCHEMATIC/$VERSION"
ASSETS="${ASSETS:-./assets}"
mkdir -p "$ASSETS"
for f in metal-arm64.iso kernel-arm64 initramfs-arm64.xz; do
  [ -s "$ASSETS/$f" ] || curl -fL --progress-bar -o "$ASSETS/$f" "$BASE/$f"
done
# Factory's kernel-arm64 is an EFI zboot wrapper (MZ + "zimg", zstd payload).
# VZLinuxBootLoader needs the raw arm64 Image inside: payload offset/size are LE uint32 at bytes 8-15.
if [ ! -s "$ASSETS/kernel-arm64-raw" ]; then
  OFF=$(python3 -c "import struct;print(struct.unpack('<II',open('$ASSETS/kernel-arm64','rb').read(16)[8:16])[0])")
  SIZE=$(python3 -c "import struct;print(struct.unpack('<II',open('$ASSETS/kernel-arm64','rb').read(16)[8:16])[1])")
  tail -c +$((OFF+1)) "$ASSETS/kernel-arm64" | head -c "$SIZE" | zstd -d -o "$ASSETS/kernel-arm64-raw"
fi
ls -lh "$ASSETS"
