#!/bin/sh
# PROTOTYPE — wayfinder ticket #10: does vmnet shared mode pass ARP for an IP it never assigned?
# Phase 1: boot ISO + blank disk, apply a config with a static secondary IP (the fake "VIP"),
#          wait for install (installer image is pulled from ghcr.io — takes minutes) + reboot.
# Phase 2: boot from disk, then from the host: ping/ARP the VIP.
set -eu
ASSETS="${ASSETS:-./assets}"
VIP=192.168.64.250

go build -o talos-vz-boot .
codesign --force -s - --entitlements entitlements.plist talos-vz-boot

rm -f "$ASSETS/talos-install.img"
mkfile -n 6g "$ASSETS/talos-install.img" 2>/dev/null || truncate -s 6g "$ASSETS/talos-install.img"

cat > "$ASSETS/patch.yaml" <<EOF
machine:
  install:
    disk: /dev/vdb
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: "52:54:00:aa:bb:05"
        dhcp: true
        addresses:
          - $VIP/32
EOF
rm -rf "$ASSETS/cfg"
talosctl gen config vmnet-arp-test https://192.168.64.3:6443 \
  --config-patch @"$ASSETS/patch.yaml" --output-dir "$ASSETS/cfg" --force

echo "=== phase 1: boot ISO, apply config, wait for install ==="
rm -f efi-vars.fd
KEEP_ALIVE=1200 ./talos-vz-boot efi "$ASSETS/metal-arm64.iso" "$ASSETS/talos-install.img" 2>phase1.log &
P1=$!
for i in $(seq 1 60); do grep -q "RESULT=SUCCESS" phase1.log 2>/dev/null && break; sleep 5; done
grep "RESULT" phase1.log
IP=$(sed -n 's/.*reachable at \([0-9.]*\):50000.*/\1/p' phase1.log | head -1)
talosctl apply-config --insecure --nodes "$IP" --file "$ASSETS/cfg/controlplane.yaml"

# wait until the disk has a GPT (install actually wrote something), tailing dmesg for progress
echo "waiting for install to write the disk..."
INSTALLED=no
for i in $(seq 1 60); do
  sleep 15
  talosctl dmesg --insecure --nodes "$IP" 2>/dev/null | tail -3 || true
  if xxd -s 512 -l 8 "$ASSETS/talos-install.img" | grep -q "EFI PART"; then INSTALLED=yes; break; fi
done
echo "disk GPT present: $INSTALLED"
# give the installer time to finish writing + node time to reboot, then stop phase 1
sleep 90
kill $P1 2>/dev/null; wait $P1 2>/dev/null || true

echo "=== phase 2: boot from installed disk ==="
rm -f efi-vars.fd
KEEP_ALIVE=900 ./talos-vz-boot disk "$ASSETS/talos-install.img" 2>phase2.log &
P2=$!
for i in $(seq 1 90); do grep -q "RESULT=" phase2.log 2>/dev/null && break; sleep 5; done
grep "RESULT" phase2.log
IP=$(sed -n 's/.*reachable at \([0-9.]*\):50000.*/\1/p' phase2.log | head -1)
# a configured node rejects insecure connections — confirms we're no longer in maintenance mode
talosctl version --insecure --nodes "$IP" >/dev/null 2>&1 && MODE=maintenance || MODE=configured
echo "node up at $IP, mode=$MODE; interrogating the VIP $VIP from the host"

echo "=== host -> VIP ping ==="
ping -c 3 -t 5 "$VIP" && PING=pass || PING=fail
echo "=== host ARP table entry for VIP ==="
arp -an | grep "$VIP" || true
echo "=== host -> VIP tcp/50000 (apid listens on all interfaces) ==="
nc -z -G 3 "$VIP" 50000 && TCP=pass || TCP=fail

echo ""
echo "RESULT: ping=$PING tcp50000=$TCP (node dhcp ip=$IP mode=$MODE, vip=$VIP)"
kill $P2 2>/dev/null; wait $P2 2>/dev/null || true
