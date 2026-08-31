#!/usr/bin/env bash
# Contract needles are literal grep patterns, not expansions.
# shellcheck disable=SC2016
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
workflow="$root/.github/workflows/ci.yml"
depot_ci="$root/.depot/workflows/ci.yml"
depot_e2e="$root/.depot/workflows/e2e.yml"
depot_floor="$root/.depot/workflows/floor-e2e.yml"
harness="$root/scripts/ci/linux-kvm-e2e.sh"
storage_harness="$root/scripts/ci/linux-kvm-storage-e2e.sh"
systemd_harness="$root/scripts/qa/linux-systemd-packaging-e2e.sh"
lib="$root/scripts/ci/kvm-e2e-lib.sh"

require() {
  local needle=$1
  local file=$2
  if ! grep -Fq -- "$needle" "$file"; then
    printf 'missing required CI contract %q in %s\n' "$needle" "$file" >&2
    exit 1
  fi
}

# GitHub Actions keeps only the lanes Depot CI cannot serve.
require 'macos-15' "$workflow"
require 'ubuntu-24.04-arm' "$workflow"

# Fast lanes live in the Depot CI workflow.
require 'depot-ubuntu-24.04-8' "$depot_ci"
require 'depot-ubuntu-24.04-4' "$depot_ci"
require 'nix flake check' "$depot_ci"
require 'depot/cache-mount' "$depot_ci"

# KVM e2e lanes: merge-gating substrate + storage on PRs and main.
require 'kvm-substrate' "$depot_e2e"
require 'kvm-storage' "$depot_e2e"
require 'scripts/ci/linux-kvm-e2e.sh' "$depot_e2e"
require 'scripts/ci/linux-kvm-storage-e2e.sh' "$depot_e2e"
require 'qemu-system-x86' "$depot_e2e"
require 'socat nfs-ganesha nfs-ganesha-vfs rpcbind' "$depot_e2e"

# Floor lane is release-gating: version tags and manual dispatch only.
require 'tags: ["v*"]' "$depot_floor"
require 'workflow_dispatch' "$depot_floor"
require 'TBX_E2E_TALOS_VERSION: v1.12.0' "$depot_floor"
require 'scripts/ci/linux-kvm-e2e.sh' "$depot_floor"

for kvm_workflow in "$depot_e2e" "$depot_floor"; do
  require 'test -w /dev/kvm' "$kvm_workflow"
  require "sudo iptables -C FORWARD -i 'br-tbx+' -j ACCEPT" "$kvm_workflow"
  require "sudo iptables -I FORWARD 1 -i 'br-tbx+' -j ACCEPT" "$kvm_workflow"
  require "sudo iptables -C FORWARD -o 'br-tbx+' -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "$kvm_workflow"
  require "sudo iptables -I FORWARD 1 -o 'br-tbx+' -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "$kvm_workflow"
done

# Shared plumbing lives in the lib; both harnesses must source it.
require 'retry() {' "$lib"
require 'wait_for_process_socket() {' "$lib"
require 'select_scratch() {' "$lib"
require 'usable_scratch() {' "$lib"
require 'prepare_workdir() {' "$lib"
require 'refusing to reuse existing TBX_E2E_WORKDIR' "$lib"
require 'socket_ready() {' "$lib"
require 'cat "$log_file" >&2' "$lib"
require 'source "$root/scripts/ci/kvm-e2e-lib.sh"' "$harness"
require 'source "$root/scripts/ci/kvm-e2e-lib.sh"' "$storage_harness"

require "\"\$root/bin/tbx\" up -f" "$harness"
require 'controlPlanes: 1' "$harness"
require 'workers: 2' "$harness"
require 'memory: 2GiB' "$harness"
require 'memory: 1GiB' "$harness"
require 'diskSize: 10GiB' "$harness"
require 'disabled: true' "$harness"
require 'name: none' "$harness"
require '--config-patch' "$harness"
require 'talosctl gen config' "$harness"
require 'apply-config' "$harness"
require "talosctl version --nodes \"\$1\" --insecure" "$harness"
require 'maintenance_api_reply_ready "$output"' "$harness"
require 'maintenance_api_reply_ready() {' "$lib"
require "talosctl apply-config --nodes \"\$cp_ip\" --insecure" "$harness"
require "talosctl apply-config --nodes \"\$node_ip\" --insecure" "$harness"
require "talosctl version --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require "talosctl bootstrap --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require "talosctl kubeconfig \"\$kubeconfig\" --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require ' bootstrap' "$harness"
require 'flannel/releases/download/v0.27.4/kube-flannel.yml' "$harness"
require 'wait --for=condition=Ready node --all' "$harness"
require 'go test -C "$root" -tags=e2e -c -o "$doctor_e2e_test" ./cmd/tbx' "$harness"
require 'TBX_E2E_CLUSTER=e2e "$doctor_e2e_test" -test.v' "$harness"
require "-test.run '^TestLinuxDoctorExitCodeWithRunningClusterRoutes\$'" "$harness"
require "allocated -lt \$apparent" "$harness"
require 'ready_nodes" -eq 3' "$harness"
require 'retry "Talos bootstrap"' "$harness"
require 'retry "kubeconfig retrieval"' "$harness"
require 'retry "Kubernetes API"' "$harness"
require 'retry "Flannel apply"' "$harness"
require 'retry "node registration"' "$harness"
require "kubectl --kubeconfig \"\$kubeconfig\" get nodes -o name" "$harness"
require "[[ \"\$node_count\" -eq 3 ]]" "$harness"
require 'retry "substrate cluster creation"' "$harness"

# Each curated extension is proved functionally, and the same harness boots
# either end of the supported version window.
require 'extensions: [gvisor, nfs-utils, qemu-guest-agent]' "$harness"
require 'guest-ping' "$harness"
require 'UNIX-CONNECT:$qga_socket' "$harness"
require 'retry "qemu-guest-agent guest-ping"' "$harness"
require 'runtimeClassName: runsc' "$harness"
require 'handler: runsc' "$harness"
require 'retry "runsc pod completion"' "$harness"
require 'nfsvers=3' "$harness"
require 'flock /data/probe' "$harness"
require 'retry "NFSv3 locked write"' "$harness"
# Ganesha serves the export from userspace; the harness owns the daemon and
# must never lean on systemd, which CI sandboxes lack.
require 'ganesha.nfsd -f' "$harness"
require 'Enable_NLM = true;' "$harness"
require 'retry "Ganesha NFSv3 registration"' "$harness"
require 'talos_version=${TBX_E2E_TALOS_VERSION:-v1.13.9}' "$harness"
require '  v1.12.0)' "$harness"

# The storage harness exercises the curated provisioning path end to end:
# longhorn on a multinode cluster, then local-path on a single node, and both
# crossings of the zero-worker boundary on the worker-less cluster.
require "\"\$root/bin/tbx\" up -f" "$storage_harness"
require 'cni: flannel' "$storage_harness"
require 'csi: longhorn' "$storage_harness"
require 'csi: local-path' "$storage_harness"
require 'controlPlanes: 1' "$storage_harness"
require 'workers: 2' "$storage_harness"
require 'workers: 0' "$storage_harness"
require 'diskSize: 10GiB' "$storage_harness"
require 'retry "longhorn cluster provisioning"' "$storage_harness"
require 'retry "local-path cluster provisioning"' "$storage_harness"
require 'storagePhase' "$storage_harness"
require 'is-default-class' "$storage_harness"
require 'numberOfReplicas' "$storage_harness"
require 'replicas.longhorn.io' "$storage_harness"
require 'cat /data/probe' "$storage_harness"
require 'control_plane_reserved() {' "$storage_harness"
require 'node-role.kubernetes.io/control-plane' "$storage_harness"
require 'node.kubernetes.io/exclude-from-external-load-balancers' "$storage_harness"
require 'node add e2es --role worker --force' "$storage_harness"
require 'node remove e2es "$added_worker" --force' "$storage_harness"
require 'retry "control plane reserved after node add" 120 5 control_plane_reserved true' "$storage_harness"
require 'retry "control plane schedulable after node remove" 120 5 control_plane_reserved false' "$storage_harness"
require 'cluster destroy e2es --force' "$storage_harness"
require 'sha256sum --check --strict' "$storage_harness"

for checked_harness in "$harness" "$storage_harness"; do
  require 'cluster_cleanup_needed=true' "$checked_harness"
  require 'daemon_pid=$!' "$checked_harness"
  require 'wait_for_process_socket "tbxd"' "$checked_harness"
  require 'dump_failure_diagnostics() (' "$checked_harness"
  require 'tbx-helper.log' "$checked_harness"
  require "kill -0 \"\$daemon_pid\"" "$checked_harness"
  require 'ip -details address show' "$checked_harness"
  require 'nft list ruleset' "$checked_harness"
  require 'sha256sum --check --strict' "$checked_harness"
  kvm_gate_line=$(grep -nF 'test -w /dev/kvm' "$checked_harness" | head -n 1 | cut -d: -f1)
  diagnostic_trap_line=$(grep -nF 'trap dump_failure_diagnostics ERR' "$checked_harness" | cut -d: -f1)
  if [[ -z "$kvm_gate_line" || -z "$diagnostic_trap_line" || $kvm_gate_line -ge $diagnostic_trap_line ]]; then
    printf 'hard KVM gate must run before diagnostics and setup in %s\n' "$checked_harness" >&2
    exit 1
  fi
  if [[ ! -x "$checked_harness" ]]; then
    printf 'expected executable KVM e2e harness at %s\n' "$checked_harness" >&2
    exit 1
  fi
done
require 'console e2e' "$harness"
require 'console e2es' "$storage_harness"

# The packaged-systemd lane is deliberately separate from the two init-agnostic
# CI harnesses. It must install the checkout package and exercise the live unit,
# forwarding, DHCP persistence/recovery, and supervised restart contracts.
if [[ ! -x "$systemd_harness" ]]; then
  printf 'expected executable packaged-systemd QA harness at %s\n' "$systemd_harness" >&2
  exit 1
fi
require '--i-know-this-installs-system-files' "$systemd_harness"
require 'case "$(uname -m)" in' "$systemd_harness"
require 'x86_64) qemu_system_bin=qemu-system-x86_64 ;;' "$systemd_harness"
require 'aarch64) qemu_system_bin=qemu-system-aarch64 ;;' "$systemd_harness"
require 'readlink /proc/1/exe' "$systemd_harness"
require 'systemctl is-system-running' "$systemd_harness"
require 'sudo cp -a "$root/packaging/linux/." /' "$systemd_harness"
require 'systemd-sysusers' "$systemd_harness"
require 'sudo systemctl daemon-reload' "$systemd_harness"
require 'systemctl --user daemon-reload' "$systemd_harness"
require 'enable --now tbx-helper.socket' "$systemd_harness"
require 'enable --now tbxd.socket' "$systemd_harness"
require 'AmbientCapabilities' "$systemd_harness"
require 'CapabilityBoundingSet' "$systemd_harness"
require '/proc/sys/net/ipv4/ip_forward' "$systemd_harness"
require '/proc/sys/net/ipv4/conf/$bridge/forwarding' "$systemd_harness"
require '/var/lib/tbx/reservations.json' "$systemd_harness"
require 'sudo test -e "$state_file"' "$systemd_harness"
require 'already exists; restore or remove it before running this harness' "$systemd_harness"
require "ss -H -ulpn 'sport = :67'" "$systemd_harness"
require 'nft list table inet tbx' "$systemd_harness"
require '--data-urlencode "host=$target"' "$systemd_harness"
require 'assert_routed_dial "$vip_a" "$vip_b"' "$systemd_harness"
require 'assert_routed_dial "$vip_b" "$vip_a"' "$systemd_harness"
require 'sudo systemctl restart tbx-helper.service' "$systemd_harness"
require 'prepare_workdir "tbx-systemd-packaging-e2e.XXXXXX"' "$systemd_harness"
require 'state_backup="$workdir/reservations.json.backup"' "$systemd_harness"
require 'state_backup_moved_flag="$workdir/reservations-backup.moved"' "$systemd_harness"
require 'sudo mv "$state_file" "$state_backup"' "$systemd_harness"
require 'touch "$state_backup_moved_flag"' "$systemd_harness"
require "retry 'periodic net.sync reservation and DHCP recovery' 19 5" "$systemd_harness"
require 'fresh DHCP exchange and maintenance reachability' "$systemd_harness"
require 'sudo test -e "$state_file" || return 1' "$systemd_harness"
require 'sudo test -e "$state_backup"' "$systemd_harness"
require 'delete_state_backup' "$systemd_harness"
require 'sudo rm -f -- "$state_backup"' "$systemd_harness"
require 'rm -f -- "$state_backup_moved_flag"' "$systemd_harness"
require 'FAILED to restore %s from %s' "$systemd_harness"
require 'if ! restore_state_backup; then' "$systemd_harness"
require 'return 1' "$systemd_harness"
require 'sudo rm -f -- "$state_file"' "$systemd_harness"
require 'sudo mv "$state_backup" "$state_file"' "$systemd_harness"
state_backup_move_line=$(grep -nF 'sudo mv "$state_file" "$state_backup"' "$systemd_harness" | head -n 1 | cut -d: -f1)
state_backup_flag_line=$(grep -nF 'touch "$state_backup_moved_flag"' "$systemd_harness" | head -n 1 | cut -d: -f1)
if [[ -z "$state_backup_move_line" || -z "$state_backup_flag_line" || $state_backup_move_line -ge $state_backup_flag_line ]]; then
  printf 'packaged-systemd recovery must mark the move only after the sudo move succeeds in %s\n' "$systemd_harness" >&2
  exit 1
fi
require '/usr/bin/tbx system restart' "$systemd_harness"
require 'systemctl --user restart tbxd.service' "$systemd_harness"
require "grep -Fq -- '--force'" "$systemd_harness"
require 'cluster destroy "$cluster_b" --force' "$systemd_harness"
require 'cluster destroy "$cluster_a" --force' "$systemd_harness"
require 'dump_failure_diagnostics() (' "$systemd_harness"
require 'journalctl -u tbx-helper.socket -u tbx-helper.service' "$systemd_harness"
require 'journalctl --user -u tbxd.socket -u tbxd.service' "$systemd_harness"

for kvm_workflow in "$depot_e2e" "$depot_floor"; do
  if grep -Eq -- 'setfacl| acl|udev' "$kvm_workflow"; then
    printf '/dev/kvm is writable out of the box in CI sandboxes; no ACL or udev setup allowed in %s\n' "$kvm_workflow" >&2
    exit 1
  fi
done
if grep -Fq -- 'systemctl' "$harness" || grep -Fq -- 'systemctl' "$storage_harness"; then
  printf 'e2e harnesses must stay init-agnostic: CI sandboxes have no systemd\n' >&2
  exit 1
fi
if grep -Eq -- '--(cni|csi|lb|bgp|hubble)([ =]|$)' "$harness"; then
  printf 'substrate-only e2e must not request talosbox provisioning flags\n' >&2
  exit 1
fi
if grep -Fq -- 'csi:' "$harness"; then
  printf 'substrate-only e2e must not declare curated storage\n' >&2
  exit 1
fi
if grep -Fq -- 'talosctl' "$storage_harness"; then
  printf 'provisioning-path e2e must not manage Talos by hand\n' >&2
  exit 1
fi
release_workflow="$root/.github/workflows/release.yml"
for pinned_workflow in "$workflow" "$release_workflow" "$depot_ci" "$depot_e2e" "$depot_floor"; do
  if grep -Eq 'uses: [^[:space:]]+@v[0-9]' "$pinned_workflow"; then
    printf 'CI workflows must pin actions to immutable commit SHAs (%s)\n' "$pinned_workflow" >&2
    exit 1
  fi
  if grep -Fq -- 'schedule:' "$pinned_workflow"; then
    printf 'CI runs on PRs, pushes to main, and release tags only — no cron (%s)\n' "$pinned_workflow" >&2
    exit 1
  fi
done

expect_kvm_gate_failure() {
  local gated_harness=$1 device=$2 workdir
  workdir=$(mktemp -d)
  rmdir "$workdir"
  if TBX_E2E_KVM_DEVICE="$device" TBX_E2E_WORKDIR="$workdir" "$gated_harness"; then
    printf 'KVM e2e harness %s unexpectedly continued with KVM device %s\n' "$gated_harness" "$device" >&2
    exit 1
  fi
  if [[ -e "$workdir" ]]; then
    printf 'KVM e2e harness %s performed work after a failed KVM gate\n' "$gated_harness" >&2
    exit 1
  fi
}

for checked_harness in "$harness" "$storage_harness"; do
  missing_kvm=$(mktemp -u)
  expect_kvm_gate_failure "$checked_harness" "$missing_kvm"

  unwritable_kvm=$(mktemp)
  chmod 000 "$unwritable_kvm"
  expect_kvm_gate_failure "$checked_harness" "$unwritable_kvm"
  chmod 600 "$unwritable_kvm"
  [[ -w "$unwritable_kvm" ]]

  existing_workdir=$(mktemp -d)
  touch "$existing_workdir/user-data"
  if TBX_E2E_KVM_DEVICE="$unwritable_kvm" TBX_E2E_WORKDIR="$existing_workdir" "$checked_harness"; then
    printf 'KVM e2e harness %s unexpectedly reused an existing workdir\n' "$checked_harness" >&2
    exit 1
  fi
  if [[ ! -f "$existing_workdir/user-data" ]]; then
    printf 'KVM e2e harness %s removed data from an existing workdir\n' "$checked_harness" >&2
    exit 1
  fi
  rm -rf "$existing_workdir"
  rm -f "$unwritable_kvm"
done
