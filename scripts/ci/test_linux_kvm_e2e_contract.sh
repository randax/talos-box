#!/usr/bin/env bash
# Contract needles are literal grep patterns, not expansions.
# shellcheck disable=SC2016
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
workflow="$root/.github/workflows/ci.yml"
harness="$root/scripts/ci/linux-kvm-e2e.sh"
storage_harness="$root/scripts/ci/linux-kvm-storage-e2e.sh"
lib="$root/scripts/ci/kvm-e2e-lib.sh"

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
require 'linux-kvm-storage-e2e' "$workflow"
require 'scripts/ci/linux-kvm-storage-e2e.sh' "$workflow"
require "if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository" "$workflow"
require 'qemu-system-x86' "$workflow"
require '/etc/udev/rules.d/99-kvm4all.rules' "$workflow"
require 'MODE="0666"' "$workflow"
require 'OPTIONS+="static_node=kvm"' "$workflow"
require 'test -w /dev/kvm' "$workflow"
require "sudo iptables -C FORWARD -i 'br-tbx+' -j ACCEPT" "$workflow"
require "sudo iptables -I FORWARD 1 -i 'br-tbx+' -j ACCEPT" "$workflow"
require "sudo iptables -C FORWARD -o 'br-tbx+' -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "$workflow"
require "sudo iptables -I FORWARD 1 -o 'br-tbx+' -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" "$workflow"

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
require 'retry "node registration"' "$harness"
require "kubectl --kubeconfig \"\$kubeconfig\" get nodes -o name" "$harness"
require "[[ \"\$node_count\" -eq 3 ]]" "$harness"
require 'retry "substrate cluster creation"' "$harness"

# The storage harness exercises the curated provisioning path end to end:
# longhorn on a multinode cluster, then local-path on a single node.
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
require 'robustness' "$storage_harness"
require 'cat /data/probe' "$storage_harness"
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

if grep -Fq -- 'setfacl' "$workflow" || grep -Fq -- ' acl' "$workflow"; then
  printf 'KVM access must come from the udev rule, not ACL setup\n' >&2
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
if grep -Eq 'uses: [^[:space:]]+@v[0-9]' "$workflow"; then
  printf 'scheduled CI jobs must pin actions to immutable commit SHAs\n' >&2
  exit 1
fi

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
