#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
workflow="$root/.github/workflows/ci.yml"
harness="$root/scripts/ci/linux-kvm-e2e.sh"

require() {
  local needle=$1
  local file=$2
  if ! grep -Fq -- "$needle" "$file"; then
    printf 'missing required CI contract %q in %s\n' "$needle" "$file" >&2
    exit 1
  fi
}

require 'schedule:' "$workflow"
require 'ubuntu-latest' "$workflow"
require 'ubuntu-24.04-arm' "$workflow"
require 'nix flake check' "$workflow"
require 'linux-kvm-e2e' "$workflow"
require "if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository" "$workflow"
require 'qemu-system-x86' "$workflow"
require '/etc/udev/rules.d/99-kvm4all.rules' "$workflow"
require 'MODE="0666"' "$workflow"
require 'OPTIONS+="static_node=kvm"' "$workflow"
require 'test -w /dev/kvm' "$workflow"
require "--physdev-in 'br-tbx+' -j ACCEPT" "$workflow"
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
require "talosctl apply-config --nodes \"\$cp_ip\" --insecure" "$harness"
require "talosctl apply-config --nodes \"\$node_ip\" --insecure" "$harness"
require "talosctl version --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require "talosctl bootstrap --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require "talosctl kubeconfig \"\$kubeconfig\" --talosconfig \"\$talos_config/talosconfig\" --nodes \"\$cp_ip\" --endpoints \"\$cp_ip\"" "$harness"
require ' bootstrap' "$harness"
require 'flannel/releases/download/v0.27.4/kube-flannel.yml' "$harness"
require 'wait --for=condition=Ready node --all' "$harness"
require "allocated -lt \$apparent" "$harness"
require 'ready_nodes" -eq 3' "$harness"
require 'retry "Talos bootstrap"' "$harness"
require 'retry "kubeconfig retrieval"' "$harness"
require 'retry "Kubernetes API"' "$harness"
require 'retry "Flannel apply"' "$harness"
require 'retry "substrate cluster creation"' "$harness"
require 'cluster_cleanup_needed=true' "$harness"
require 'daemon_pid=$!' "$harness"
require 'wait_for_process_socket "tbxd"' "$harness"
require "cat \"\$log_file\" >&2" "$harness"
require 'dump_failure_diagnostics() (' "$harness"
require 'tbx-helper.log' "$harness"
require "kill -0 \"\$daemon_pid\"" "$harness"
require 'ip -details address show' "$harness"
require 'nft list ruleset' "$harness"
require 'console e2e' "$harness"
require 'sha256sum --check --strict' "$harness"
kvm_gate_line=$(grep -nF 'test -w /dev/kvm' "$harness" | head -n 1 | cut -d: -f1)
diagnostic_trap_line=$(grep -nF 'trap dump_failure_diagnostics ERR' "$harness" | cut -d: -f1)
if [[ -z "$kvm_gate_line" || -z "$diagnostic_trap_line" || $kvm_gate_line -ge $diagnostic_trap_line ]]; then
  printf 'hard KVM gate must run before diagnostics and setup\n' >&2
  exit 1
fi
if grep -Fq -- 'setfacl' "$workflow" || grep -Fq -- ' acl' "$workflow"; then
  printf 'KVM access must come from the udev rule, not ACL setup\n' >&2
  exit 1
fi
if grep -Eq -- '--(cni|lb|bgp|hubble)([ =]|$)' "$harness"; then
  printf 'substrate-only e2e must not request talosbox provisioning flags\n' >&2
  exit 1
fi
if grep -Eq 'uses: [^[:space:]]+@v[0-9]' "$workflow"; then
  printf 'scheduled CI jobs must pin actions to immutable commit SHAs\n' >&2
  exit 1
fi

if [[ ! -x "$harness" ]]; then
  printf 'expected executable KVM e2e harness at %s\n' "$harness" >&2
  exit 1
fi

expect_kvm_gate_failure() {
  local device=$1 workdir
  workdir=$(mktemp -d)
  rmdir "$workdir"
  if TBX_E2E_KVM_DEVICE="$device" TBX_E2E_WORKDIR="$workdir" "$harness"; then
    printf 'KVM e2e harness unexpectedly continued with KVM device %s\n' "$device" >&2
    exit 1
  fi
  if [[ -e "$workdir" ]]; then
    printf 'KVM e2e harness performed work after a failed KVM gate\n' >&2
    exit 1
  fi
}

missing_kvm=$(mktemp -u)
expect_kvm_gate_failure "$missing_kvm"

unwritable_kvm=$(mktemp)
chmod 000 "$unwritable_kvm"
expect_kvm_gate_failure "$unwritable_kvm"
chmod 600 "$unwritable_kvm"
[[ -w "$unwritable_kvm" ]]

existing_workdir=$(mktemp -d)
touch "$existing_workdir/user-data"
if TBX_E2E_KVM_DEVICE="$unwritable_kvm" TBX_E2E_WORKDIR="$existing_workdir" "$harness"; then
  printf 'KVM e2e harness unexpectedly reused an existing workdir\n' >&2
  exit 1
fi
if [[ ! -f "$existing_workdir/user-data" ]]; then
  printf 'KVM e2e harness removed data from an existing workdir\n' >&2
  exit 1
fi
rm -rf "$existing_workdir"
rm -f "$unwritable_kvm"
