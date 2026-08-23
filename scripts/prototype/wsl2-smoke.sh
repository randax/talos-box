#!/usr/bin/env bash
# PROTOTYPE — THROWAWAY. Wayfinder ticket #461 (map #456).
# Smoke-tests the existing Linux tbx stack inside WSL2 and produces a failure
# list. Not production tooling: no error handling beyond what keeps it
# runnable, delete the branch when the ticket closes.
#
# Run inside the Ubuntu 24.04 WSL distro, from a talos-box checkout:
#   bash scripts/prototype/wsl2-smoke.sh <phase>
# Phases, in order: env deps install doctor cluster probes windows keepalive
# Each phase appends to ~/tbx-wsl2-smoke-report.txt. Rerunning a phase is fine.

set -u
REPORT="$HOME/tbx-wsl2-smoke-report.txt"
PHASE="${1:-env}"

section() {
  echo | tee -a "$REPORT"
  echo "==================== [$PHASE] $* — $(date -Is) ====================" | tee -a "$REPORT"
}
run() {
  echo "\$ $*" | tee -a "$REPORT"
  "$@" 2>&1 | tee -a "$REPORT"
  echo "(exit $?)" | tee -a "$REPORT"
}

case "$PHASE" in
env)
  section "WSL environment capture"
  run uname -a
  run wslinfo --networking-mode
  run systemctl is-system-running
  run ls -l /dev/kvm
  run grep MemTotal /proc/meminfo
  run cat /proc/sys/fs/binfmt_misc/WSLInterop 2>/dev/null || true
  run ip -4 addr show eth0
  run cat /etc/resolv.conf
  run resolvectl status --no-pager || true
  run loginctl show-user "$USER" -p Linger
  MEM_KB=$(awk '/MemTotal/{print $2}' /proc/meminfo)
  if [ "$MEM_KB" -lt 10000000 ]; then
    echo "!! MemTotal < ~10 GiB. Before continuing, on WINDOWS create" | tee -a "$REPORT"
    echo "!! %UserProfile%\\.wslconfig with:" | tee -a "$REPORT"
    printf '[wsl2]\nmemory=12GB\n\n[experimental]\nautoMemoryReclaim=disabled\n' | tee -a "$REPORT"
    echo "!! then run 'wsl --shutdown' from PowerShell, reopen this shell, rerun phase env." | tee -a "$REPORT"
    exit 1
  fi
  echo "env OK — next: bash scripts/prototype/wsl2-smoke.sh deps"
  ;;
deps)
  section "Host dependencies (docs/linux.md, Ubuntu amd64)"
  run sudo apt update
  run sudo apt install -y ca-certificates curl git make openssl policykit-1 \
    qemu-system-x86 ovmf iproute2 iptables
  run test -r /dev/kvm
  run test -w /dev/kvm
  run qemu-system-x86_64 --version
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q go1.26; then
    echo ">> installing Go 1.26.5 per docs/linux.md" | tee -a "$REPORT"
    GO_ARCHIVE=go1.26.5.linux-amd64.tar.gz
    run curl -fLO "https://go.dev/dl/$GO_ARCHIVE"
    run sudo rm -rf /usr/local/go
    run sudo tar -C /usr/local -xzf "$GO_ARCHIVE"
    rm -f "$GO_ARCHIVE"
  fi
  run /usr/local/go/bin/go version
  echo "deps OK — next: bash scripts/prototype/wsl2-smoke.sh install"
  ;;
install)
  section "Build and install source preview (docs/linux.md)"
  export PATH="/usr/local/go/bin:$PATH"
  run make binaries
  run sudo install -Dm0755 bin/tbx /usr/bin/tbx
  run sudo install -Dm0755 bin/tbxd /usr/bin/tbxd
  run sudo install -Dm0755 bin/tbx-helper /usr/bin/tbx-helper
  for u in system/tbx-helper.socket system/tbx-helper.service user/tbxd.socket user/tbxd.service; do
    run sudo install -Dm0644 "packaging/linux/usr/lib/systemd/$u" "/usr/lib/systemd/$u"
  done
  run sudo install -Dm0644 packaging/linux/usr/lib/sysusers.d/talos-box.conf /usr/lib/sysusers.d/talos-box.conf
  run sudo install -Dm0644 packaging/linux/usr/share/polkit-1/rules.d/90-talos-box-resolved.rules \
    /usr/share/polkit-1/rules.d/90-talos-box-resolved.rules
  run sudo systemd-sysusers /usr/lib/sysusers.d/talos-box.conf
  run sudo usermod -aG tbx "$USER"
  run sudo usermod -aG kvm "$USER"
  run sudo systemctl daemon-reload
  run sudo systemctl enable --now tbx-helper.socket
  run systemctl --user daemon-reload
  run systemctl --user enable --now tbxd.socket
  echo "install OK — now CLOSE this shell, reopen (group membership), rerun phase doctor" | tee -a "$REPORT"
  ;;
doctor)
  section "tbx doctor (pre-cluster)"
  run id
  run tbx doctor
  echo "doctor captured (non-zero exit is a finding, not a script failure)"
  echo "next: bash scripts/prototype/wsl2-smoke.sh cluster"
  ;;
cluster)
  section "Cluster create smoke (small topology for the WSL memory cap)"
  run tbx cluster create wslproto --cp 1 --workers 1
  run tbx cluster list
  run tbx doctor
  echo "next: bash scripts/prototype/wsl2-smoke.sh probes"
  ;;
probes)
  section "In-WSL data-path probes"
  GW=$(ip -4 addr show | awk '/br-tbx/{print; exit}')
  run ip -4 addr show
  run ip route
  run sudo nft list ruleset
  run resolvectl status --no-pager || true
  run dig +short @172.30.0.1 wslproto-cp-1.wslproto.k8s.test || true
  run dig +short wslproto-cp-1.wslproto.k8s.test || true
  run cat /etc/resolv.conf
  run tbx bgp status wslproto || true
  echo ">> If an ingress VIP exists, curl it and note the result manually." | tee -a "$REPORT"
  echo "next: bash scripts/prototype/wsl2-smoke.sh windows (prints PowerShell steps)"
  ;;
windows)
  section "Windows-side probes (run these in PowerShell as admin, paste results into the report)"
  ETH0=$(ip -4 addr show eth0 | awk '/inet /{sub(/\/.*/,"",$2); print $2}')
  cat <<EOW | tee -a "$REPORT"
# WSL eth0 IP right now: $ETH0
route add 172.30.0.0 mask 255.255.0.0 $ETH0
Test-NetConnection 172.30.0.1 -Port 53
nslookup wslproto-cp-1.wslproto.k8s.test $ETH0
Test-NetConnection <ingress-VIP-if-any> -Port 443
# then open https://<something>.k8s.test in the Windows browser and note what happens
# also note: does the Hyper-V firewall need an allow rule first?
EOW
  echo "next: bash scripts/prototype/wsl2-smoke.sh keepalive"
  ;;
keepalive)
  section "VM-idle keepalive experiment"
  run tbx cluster list
  run pgrep -a qemu
  cat <<'EOK' | tee -a "$REPORT"
>> Now: close EVERY WSL terminal window. Wait 3+ minutes.
>> Reopen a WSL shell and run:
>>   bash scripts/prototype/wsl2-smoke.sh keepalive
>> again. Compare: did the distro restart (uptime reset)? Are the QEMU
>> processes gone? What does 'tbx cluster list' say?
EOK
  run uptime
  ;;
*)
  echo "unknown phase: $PHASE (env deps install doctor cluster probes windows keepalive)" >&2
  exit 2
  ;;
esac
