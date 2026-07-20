#!/bin/sh
# PROTOTYPE — build, sign, run one boot mode: ./run.sh efi|kernel
set -eu
MODE="$1"
ASSETS="${ASSETS:-./assets}"
go build -o talos-vz-boot .
codesign --force -s - --entitlements entitlements.plist talos-vz-boot
rm -f efi-vars.fd
case "$MODE" in
  efi)    exec ./talos-vz-boot efi "$ASSETS/metal-arm64.iso" ;;
  kernel) exec ./talos-vz-boot kernel "$ASSETS/kernel-arm64-raw" "$ASSETS/initramfs-arm64.xz" ;;
  disk)   exec ./talos-vz-boot disk "$ASSETS/talos-install.img" ;;
esac
